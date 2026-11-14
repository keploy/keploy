//go:build darwin

package proxy

import (
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// closeBegunNotDoneOnDarwin begins the proxy's close of a pair held by a new
// Proxy, after it read the 10 bytes the application wrote, and leaves it
// short of the kernel, as closeBegunNotDone does on Linux: the close is read
// (atClose, and the application socket's count, noteWroteAtClose), the
// descriptor is marked closing, so the wait reads the proxy's side as closed,
// but the close(2) has not run. A reference a stalled goroutine holds is what
// holds it here, as one more step of Close's own on a loaded host does. let
// lets the close finish.
func closeBegunNotDoneOnDarwin(t *testing.T) (p *Proxy, pc *pairedConn, app *net.TCPConn, at proxySide, let func()) {
	t.Helper()
	p, app, proxySide, reader := darwinPair(t)
	if _, err := app.Write([]byte("publish 1\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	pc = pairedConnOf(t, p, 1)
	rc, err := proxySide.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	held, release := make(chan struct{}), make(chan struct{})
	refDone := make(chan struct{})
	go func() {
		defer close(refDone)
		_ = rc.Read(func(uintptr) bool {
			close(held)
			<-release
			return true
		})
	}()
	<-held
	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		_ = reader.Close()
	}()
	// Registered before anything here can fail, so a failure still lets the
	// close finish: its cleanup, and the pair's, wait for it.
	var once sync.Once
	let = func() {
		once.Do(func() {
			close(release)
			<-refDone
			<-closeDone
		})
	}
	t.Cleanup(let)
	// The close marks the descriptor closing once it has read the
	// application's socket: at once, with nothing of the application's on
	// its way. A close that finds bytes on their way reads the socket for
	// them first (plainOnItsWay), until data, a FIN or a reset comes, and
	// none comes here: fail, and close the application's socket so its FIN
	// ends that read and the close (let) can finish.
	deadline := time.Now().Add(10 * time.Second)
	for !errors.Is(readProxySide(proxySide).err, net.ErrClosed) {
		if time.Now().After(deadline) {
			_ = app.Close()
			t.Fatalf("the proxy's close did not mark its descriptor closing in 10s: it waited for bytes on their way, and the application wrote none after the 10 it read (the close's read of the application's socket: %+v)", pc.wroteAtClose.Load())
		}
		runtime.Gosched()
	}
	at, closed := pc.side()
	if !closed || at.state != xnuTCPEstablished || !at.counted || at.received != 10 {
		t.Fatalf("the proxy's side as its close began: %+v closed %v, want the close's own count of an established socket, 10 bytes", at, closed)
	}
	if w := pc.wroteAtClose.Load(); w == nil || w.want != 10 || w.fin {
		t.Fatalf("the close's read of the application's socket: %+v, want the 10 bytes written before it", w)
	}
	if d := proxyPCBIn(t, pc, xnuTCPEstablished); d.irs != pc.wroteAtClose.Load().isn {
		t.Fatalf("the proxy's socket began from %d, want the application's first sequence number %d", d.irs, pc.wroteAtClose.Load().isn)
	}
	return p, pc, app, at, let
}

// wroteBeforeTheCloseOnDarwin makes pc's record of what the application had
// written as the proxy began closing it n past what its side had received then
// (at), a FIN the last of them with fin: what that read finds when they were
// written before the close and were still on their way, which a fixture on
// loopback cannot hold back on demand. Without it the record is the read's
// own, and what the application writes after closeBegunNotDoneOnDarwin came
// after the close began.
func wroteBeforeTheCloseOnDarwin(pc *pairedConn, at proxySide, n uint64, fin bool) {
	pc.wroteAtClose.Store(&appWrote{want: at.received + n, fin: fin, appWroteOS: appWroteOS{isn: pc.wroteAtClose.Load().isn}})
}

// closedTarget is a bytes wait's target on pc for want written, counted from
// the application socket's first sequence number, the last of it a FIN with
// fin.
func closedTarget(pc *pairedConn, want uint64, fin bool) *pairTarget {
	return &pairTarget{pc: pc, base: targetBase{isn: pc.wroteAtClose.Load().isn}, want: want, finCounted: fin}
}

// proxyPCBIn returns the proxy's socket of pc from macOS's TCP table once it
// shows state: its state moves on loopback's input thread.
func proxyPCBIn(t *testing.T, pc *pairedConn, state uint8) xnuPCB {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		table, err := readXNUTable()
		if err != nil {
			t.Fatal(err)
		}
		d, ok := findXNUPCB(table, pc.proxy, pc.app)
		if ok && d.state == state {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("the proxy's socket %s: %+v (listed %v), want TCP state %d", pc.proxy, d, ok, state)
		}
		time.Sleep(time.Millisecond)
	}
}

// The proxy answered, began closing the connection, and the application's FIN
// came in before the close reached the kernel. What was written counts that
// FIN, which the close's own read of the proxy's side, made before it, does
// not, so the wait is one FIN short, and the socket it finds is no orphan: it
// took the FIN in sequence (CLOSE_WAIT), so nothing was refused. On macOS this
// was charged as a loss, a test failed for bytes nobody lost.
func TestAFINTakenBeforeTheProxysCloseReachedTheKernelIsNoLossOnDarwin(t *testing.T) {
	p, pc, app, at, let := closeBegunNotDoneOnDarwin(t)
	wroteBeforeTheCloseOnDarwin(pc, at, 1, true)
	_ = app.Close()
	proxyPCBIn(t, pc, xnuTCPCloseWait)
	reached, err := p.pairReached(closedTarget(pc, at.received+1, true))
	if err != nil || !reached {
		t.Fatalf("pairReached = %v, %v, want settled: the proxy's socket took the application's FIN in sequence", reached, err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost charged, want none: every byte the application wrote, and its FIN, reached the proxy", l)
	}
	let()
}

// The same close with the application's FIN still on its way: the socket, not
// yet an orphan, still ESTABLISHED, will take it or refuse a byte in its turn.
// The wait waits, as it does for an orphan one FIN short, and settles when the
// FIN comes in. It charges nothing on the way; on macOS it charged a loss.
func TestAFINOnItsWayToACloseNotYetInTheKernelIsWaitedForOnDarwin(t *testing.T) {
	p, pc, app, at, let := closeBegunNotDoneOnDarwin(t)
	wroteBeforeTheCloseOnDarwin(pc, at, 1, true)
	target := closedTarget(pc, at.received+1, true)
	reached, err := p.pairReached(target)
	if err != nil || reached {
		t.Fatalf("pairReached = %v, %v, want a wait: the FIN is on its way", reached, err)
	}
	if target.orphan != xnuTCPEstablished {
		t.Fatalf("the wait looks for state %d, want the socket's own, ESTABLISHED", target.orphan)
	}
	if got := orphansUnmoved([]*pairTarget{target}).Error(); !strings.Contains(got, "ESTABLISHED") {
		t.Fatalf("a wait that ends on it says %q, want the socket's state named", got)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost charged while the FIN was on its way, want none", l)
	}
	_ = app.Close()
	proxyPCBIn(t, pc, xnuTCPCloseWait)
	if reached, err := p.pairReached(target); err != nil || !reached {
		t.Fatalf("pairReached after the FIN = %v, %v, want settled", reached, err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost charged, want none", l)
	}
	let()
}

// A close that has not reached the kernel loses bytes all the same: nothing
// reads the descriptor again, so bytes written before the close began that
// come in now go with the close. The FIN taken after them says nothing for
// them, one byte short with no FIN counted (a byte of data) as two short with
// one: each is a loss. So is a byte short of what the close's read counted
// (no FIN in it), whatever FIN the wait reads later, and bytes short with the
// socket still ESTABLISHED, which are no FIN on its way.
func TestBytesThatMissedTheCloseAreALossBeforeItReachesTheKernelOnDarwin(t *testing.T) {
	for _, tc := range []struct {
		name string
		// atClose is what the close's read counts past what its side had
		// received (fin: its last a FIN); want and finCounted what the wait
		// reads.
		atClose     uint64
		fin         bool
		want        uint64
		finCounted  bool
		writeClose  bool
		socketState uint8
	}{
		{name: "two short with the FIN", atClose: 2, fin: true, want: 2, finCounted: true, writeClose: true, socketState: xnuTCPCloseWait},
		{name: "one short with no FIN counted", atClose: 1, want: 1, writeClose: true, socketState: xnuTCPCloseWait},
		{name: "a byte short of the close's count, the FIN after it", atClose: 1, want: 2, finCounted: true, socketState: xnuTCPCloseWait},
		{name: "two short before anything came in", atClose: 2, fin: true, want: 2, finCounted: true, socketState: xnuTCPEstablished},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, pc, app, at, let := closeBegunNotDoneOnDarwin(t)
			wroteBeforeTheCloseOnDarwin(pc, at, tc.atClose, tc.fin)
			if tc.writeClose {
				if _, err := app.Write([]byte("x")); err != nil {
					t.Fatal(err)
				}
			}
			if tc.socketState == xnuTCPCloseWait {
				_ = app.Close()
			}
			proxyPCBIn(t, pc, tc.socketState)
			if reached, err := p.pairReached(closedTarget(pc, at.received+tc.want, tc.finCounted)); err != nil || !reached {
				t.Fatalf("pairReached = %v, %v, want settled as a loss", reached, err)
			}
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
				t.Fatalf("%d app_bytes_lost charged, want 1: bytes written before the close never reached the proxy", l)
			}
			let()
		})
	}
	// Two bytes on their way at the close, an end cut short waiting for both
	// or for the first, read by a later wait that also counts a byte and
	// the FIN the application wrote after the close: the loss runs to what
	// the close counted, its last byte data, so it is the cut end's alone, or
	// the cut end's and the open test's; never the open test's for bytes
	// written after the close.
	for _, tc := range []struct {
		cut  uint64
		open uint64
	}{{cut: 2}, {cut: 1, open: 1}} {
		t.Run(fmt.Sprintf("a cut end waiting for %d of 2, a later wait past the close", tc.cut), func(t *testing.T) {
			p, pc, app, at, let := closeBegunNotDoneOnDarwin(t)
			pc.cutShort("test-1", at.received+tc.cut)
			wroteBeforeTheCloseOnDarwin(pc, at, 2, false)
			if _, err := app.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			_ = app.Close()
			proxyPCBIn(t, pc, xnuTCPCloseWait)
			if reached, err := p.pairReached(closedTarget(pc, at.received+4, true)); err != nil || !reached {
				t.Fatalf("pairReached = %v, %v, want settled as a loss", reached, err)
			}
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != tc.open {
				t.Fatalf("%d app_bytes_lost charged to the open test, want %d", l, tc.open)
			}
			var owners []string
			for _, o := range p.TakeOwnedLosses() {
				owners = append(owners, o.Test)
			}
			if len(owners) != 1 || owners[0] != "test-1" {
				t.Fatalf("the loss is owned by %v, want test-1's", owners)
			}
			let()
		})
	}
}

// Bytes the application writes, and its FIN, after the proxy began closing the
// connection are no capture loss: keploy's close, not a capture it missed,
// ended the connection under them. The close's own read of the application's
// socket (noteWroteAtClose) bounds what the wait waits for, also when the
// counts wrap at 2^32 between that read and the wait's (the frame of the
// connection's counts moved so the close's read is 2 short of the wrap, as on
// a connection 4 GiB long). On macOS both were charged as a loss.
func TestBytesTheApplicationWritesAfterTheProxysCloseBeganAreNoLossOnDarwin(t *testing.T) {
	t.Run("a byte and the FIN", func(t *testing.T) {
		p, pc, app, at, let := closeBegunNotDoneOnDarwin(t)
		if _, err := app.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		_ = app.Close()
		proxyPCBIn(t, pc, xnuTCPCloseWait)
		// What a wait reads of the application's socket now: the byte and the FIN.
		if reached, err := p.pairReached(closedTarget(pc, at.received+2, true)); err != nil || !reached {
			t.Fatalf("pairReached = %v, %v, want settled: nothing written before the close was missed", reached, err)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
			t.Fatalf("%d app_bytes_lost charged for a byte written after the proxy's close began, want none", l)
		}
		let()
	})
	t.Run("across the wrap", func(t *testing.T) {
		p, pc, _, at, let := closeBegunNotDoneOnDarwin(t)
		isn := at.rcvNxt + 1 // the side received 1<<32 - 2 from it
		pc.wroteAtClose.Store(&appWrote{want: 1<<32 - 2, appWroteOS: appWroteOS{isn: isn}})
		target := &pairTarget{pc: pc, base: targetBase{isn: isn}, want: 1, finCounted: true} // 3 more, past the wrap
		if reached, err := p.pairReached(target); err != nil || !reached {
			t.Fatalf("pairReached = %v, %v, want settled: nothing written before the close was missed", reached, err)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
			t.Fatalf("%d app_bytes_lost charged for bytes written after the proxy's close began, want none", l)
		}
		let()
	})
}

// A connection the proxy closed one FIN short, its close in the kernel: the
// orphan waiting for that FIN (FIN_WAIT_2) is waited on, and one that took it
// (TIME_WAIT) refused nothing, so the wait settles with no loss.
func TestAnOrphanThatTookTheFINSettlesTheWaitOnDarwin(t *testing.T) {
	p, pc, app, at, let := closeBegunNotDoneOnDarwin(t)
	wroteBeforeTheCloseOnDarwin(pc, at, 1, true)
	let()
	proxyPCBIn(t, pc, xnuTCPFinWait2)
	target := closedTarget(pc, at.received+1, true)
	if reached, err := p.pairReached(target); err != nil || reached {
		t.Fatalf("pairReached = %v, %v, want a wait: the orphan waits for the FIN", reached, err)
	}
	if target.orphan != xnuTCPFinWait2 {
		t.Fatalf("the wait looks for state %d, want the orphan's, FIN_WAIT_2", target.orphan)
	}
	_ = app.Close()
	proxyPCBIn(t, pc, xnuTCPTimeWait)
	if reached, err := p.pairReached(target); err != nil || !reached {
		t.Fatalf("pairReached after the orphan took the FIN = %v, %v, want settled", reached, err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost charged, want none: the orphan took the FIN in sequence", l)
	}
}
