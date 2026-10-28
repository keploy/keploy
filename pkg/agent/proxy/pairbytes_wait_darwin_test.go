//go:build darwin

package proxy

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/models"
)

// fill writes to app, without blocking, as much as its socket takes, and
// says how much. It returns once the proxy's side (proxy) advertises a zero
// window with bytes still in app's send buffer: the rest of what the
// application wrote is then held in its own hands, as only a read of the
// proxy's handler opens that window again. (A window that is small but not
// zero is no such hold: the proxy's receive buffer grows on its own, and
// XNU's sender sends what it held back for a small window once its persist
// timer, about 5 s, fires; the zero window answers that segment.) The kernel
// says nothing when the window closes, so its table is looked at again every
// 20 ms until it has.
func fill(t *testing.T, app, proxy *net.TCPConn) uint64 {
	t.Helper()
	if err := proxy.SetReadBuffer(16 << 10); err != nil {
		t.Fatal(err)
	}
	a, _ := addrPortOf(app.LocalAddr())
	r, _ := addrPortOf(app.RemoteAddr())
	rc, err := app.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64<<10)
	var total uint64
	for {
		var n int
		var werr error
		if err := rc.Write(func(fd uintptr) bool { n, werr = unix.Write(int(fd), buf); return true }); err != nil {
			t.Fatal(err)
		}
		switch {
		case errors.Is(werr, unix.EAGAIN):
			table, err := readXNUPCBs()
			if err != nil {
				t.Fatal(err)
			}
			if b, ok := findXNUPCB(table, a, r); ok && b.sndWnd == 0 && b.sndCC > 0 {
				return total
			}
			time.Sleep(20 * time.Millisecond)
		case werr != nil:
			t.Fatal(werr)
		default:
			total += uint64(n)
		}
	}
}

// receivedFrom is how many bytes (and a FIN) the proxy's side has received of
// a connection whose application end began from isn.
func receivedFrom(t *testing.T, conn *net.TCPConn, isn uint32) uint64 {
	t.Helper()
	ps := readProxySide(conn)
	if ps.err != nil {
		t.Fatal(ps.err)
	}
	return uint64(ps.rcvNxt - isn - 1)
}

// This macOS gives the counts the wait reads, in the layout keploy reads
// them: proven once, on keploy's own loopback connection.
func TestTheCountsTheBytesWaitReadsAreProvenOnDarwin(t *testing.T) {
	if err := proveXNUCounts(); err != nil {
		t.Fatalf("proveXNUCounts: %v", err)
	}
	p := &Proxy{}
	if err := p.PairedBytesErr(); err != nil {
		t.Fatalf("PairedBytesErr() = %v, want nil", err)
	}
}

// A pooled connection's call written at a test's end, while the proxy's
// handler is starved: the proxy's receive buffer is full, so the last of what
// the application wrote is still in its own send buffer. The wait counts
// those bytes too, and returns only once all of them are in the proxy's side
// (a wait that returned at once, as the head's nil did, settled on bytes
// still in the application's hands).
func TestAPooledWriteAStarvedHandlerHoldsBackIsWaitedForOnDarwin(t *testing.T) {
	p, app, proxySide, reader := darwinPair(t)
	if err := p.WaitPairedBytes(context.Background(), 1); err != nil {
		t.Fatalf("WaitPairedBytes on an idle connection: %v", err)
	}
	n := fill(t, app, proxySide)
	pending, err := p.pairTargets(1)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pairTargets = %d targets, %v; want 1", len(pending), err)
	}
	if got := pending[0].want; got != n {
		t.Fatalf("the wait's target is %d bytes, want the %d the application wrote", got, n)
	}
	if got := receivedFrom(t, proxySide, pending[0].isn); got >= n {
		t.Fatalf("the proxy's side received %d of %d at once: the fixture holds nothing back", got, n)
	}
	done := make(chan error, 1)
	go func() { done <- p.WaitPairedBytes(context.Background(), 1) }()
	buf := make([]byte, 64<<10)
	var read uint64
	for read < n {
		select {
		case err := <-done:
			if got := receivedFrom(t, proxySide, pending[0].isn); got < n {
				t.Fatalf("WaitPairedBytes returned %v with %d of %d bytes received", err, got, n)
			}
			done <- err
		default:
		}
		m, err := reader.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		read += uint64(m)
	}
	if err := <-done; err != nil {
		t.Fatalf("WaitPairedBytes: %v", err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d losses, want none", l)
	}
}

// Bytes and a FIN the proxy's socket holds unread are in the proxy's hands:
// the wait needs no read of the handler's for them.
func TestBytesAndAFINTheProxysSocketHoldsAreInItsHandsOnDarwin(t *testing.T) {
	p, app, proxySide, _ := darwinPair(t)
	if _, err := app.Write([]byte("publish 1\n")); err != nil {
		t.Fatal(err)
	}
	if err := app.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	untilUnread(t, proxySide, 10)
	if err := p.WaitPairedBytes(context.Background(), 1); err != nil {
		t.Fatalf("WaitPairedBytes: %v", err)
	}
}

// The application resets a connection with bytes still in its send buffer:
// its abort drops what it had not sent, as it would without keploy, and every
// byte it had sent had arrived (a reset is taken only in sequence). The wait
// ends, with no loss.
func TestAnApplicationsResetEndsTheBytesWaitOnDarwin(t *testing.T) {
	p, app, proxySide, _ := darwinPair(t)
	fill(t, app, proxySide)
	if err := app.SetLinger(0); err != nil {
		t.Fatal(err)
	}
	_ = app.Close()
	untilStateLeaves(t, proxySide, xnuTCPEstablished)
	if err := p.WaitPairedBytes(context.Background(), 1); err != nil {
		t.Fatalf("WaitPairedBytes after the application's reset: %v", err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d losses, want none", l)
	}
}

// The proxy closes a paired connection short of what the application wrote,
// while a test's end waits on it: a loss, charged once, and the wait does not
// hold on it (as on Linux, TestBytesThatCanNoLongerArriveAreACaptureLoss).
func TestAProxysCloseShortOfTheBytesIsOneLossAndNoHoldOnDarwin(t *testing.T) {
	p, app, proxySide, reader := darwinPair(t)
	fill(t, app, proxySide)
	targets, err := p.pairTargets(1)
	if err != nil || len(targets) != 1 {
		t.Fatalf("pairTargets = %d, %v; want the connection", len(targets), err)
	}
	done := make(chan error, 1)
	go func() { done <- p.waitPairTargets(context.Background(), targets) }()
	_ = reader.Close()
	p.paired.remove(proxySide)
	if err := <-done; err != nil {
		t.Fatalf("WaitPairedBytes: %v", err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
		t.Fatalf("%d app_bytes_lost, want 1", l)
	}
	if _, err := p.pairReached(targets[0]); err != nil {
		t.Fatal(err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
		t.Fatalf("%d app_bytes_lost after a second look, want still 1", l)
	}
}
