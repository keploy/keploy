//go:build windows

package proxy

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
)

// winTracked is winPair with the proxy's side paired (number 1) and kept as
// the accept keeps it under the proven switch: its reads are seen, and its
// close is read before it happens (pairClosing). The shim's counts say the
// application completed sends of done bytes.
func winTracked(t *testing.T, done uint64) (p *Proxy, app, side *net.TCPConn, reader *trackedConn) {
	t.Helper()
	p = &Proxy{logger: zap.NewNop(), settleReads: true}
	f := &fakeSends{}
	f.set(appSends{done: done, started: done}, nil)
	p.listenerCounter = f
	app, side = winPair(t)
	p.paired.add(side, 1, 0)
	reader = p.inflight.track(side)
	reader.beforeClose = func() { p.pairClosing(context.Background(), side, reader) }
	t.Cleanup(func() { p.inflight.forget(reader) })
	return p, app, side, reader
}

// resetApp ends app's connection with a reset (SO_LINGER 0) and waits until
// the proxy's side reads it: CLOSED, no FIN taken.
func resetApp(t *testing.T, app, side *net.TCPConn) {
	t.Helper()
	if err := app.SetLinger(0); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if ps := untilEnded(t, side); ps.err != nil || ps.state != winTCPClosed {
		t.Fatalf("after the reset: %+v (%s); want CLOSED, no FIN taken", ps, tcpStateName(ps.state))
	}
}

// TestOnWindowsASettleWaitsForBytesTheProxysSocketHoldsUnread: a test's end
// (the settle, inflight.go) cuts each kept connection at what its socket has
// received, and waits until its handler's reads have taken that much. On
// Windows that count is the socket's own BytesIn: bytes that arrived and that
// no read has taken are untaken, so the end waits for the handler to read
// the request they carry, and a read that takes them leaves none.
func TestOnWindowsASettleWaitsForBytesTheProxysSocketHoldsUnread(t *testing.T) {
	_, app, side, reader := winTracked(t, 4)
	if _, err := app.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	until(t, "the bytes to arrive", func() bool { return readProxySide(side).received == 4 })
	if u := reader.untaken(reader.view()); u != 4 {
		t.Fatalf("4 bytes arrived and none was read: %d untaken; want 4 (a test's end would not wait for the request they carry)", u)
	}
	cut, _ := reader.cutNow()
	if cut != 4 {
		t.Fatalf("a settle that begins now is cut at %d; want 4", cut)
	}
	if _, err := io.ReadFull(reader, make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	if u := reader.untaken(reader.view()); u != 0 {
		t.Fatalf("every byte read: %d untaken; want 0", u)
	}
	release := reader.HoldHangUp()
	defer release()
	if reader.hungUp(reader.view()) {
		t.Fatal("the client is connected (ESTABLISHED), and its hang-up is read as come")
	}
	if err := app.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	until(t, "the FIN", func() bool { return peerEnded(readProxySide(side).state) })
	if u := reader.untaken(reader.view()); u != 0 {
		t.Fatalf("every byte read, then the FIN: %d untaken; want 0 (the FIN is no byte)", u)
	}
	if !reader.hungUp(reader.view()) {
		t.Fatal("the client's FIN (CLOSE_WAIT), no read has returned it: its hang-up is not read as come")
	}
}

// TestOnWindowsACloseOfAConnectionNoLongerPairedChargesNothing: a close of a
// connection the pairing no longer holds (paired.closing gives none) reads
// nothing of it, after a reset as before one.
func TestOnWindowsACloseOfAConnectionNoLongerPairedChargesNothing(t *testing.T) {
	p, app, side, reader := winTracked(t, 4)
	if _, err := app.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	until(t, "the bytes to arrive", func() bool { return readProxySide(side).received == 4 })
	resetApp(t, app, side)
	p.paired.remove(side)
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("the close of a connection no longer paired panicked: %v", r)
			}
		}()
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("a connection no longer paired: %d losses; want none (its pairing settled it)", l)
	}
}

// TestOnWindowsACloseOfALiveConnectionDoesNotWaitForAReadInProgress: the
// close waits for reads in progress only on a socket the application reset
// (resetDropped), where every read ends at once. On a live one a read waits
// for the application's next bytes, which may never come: the close goes on,
// and that read ends with it.
func TestOnWindowsACloseOfALiveConnectionDoesNotWaitForAReadInProgress(t *testing.T) {
	p, _, _, reader := winTracked(t, 0)
	reader.beginRead()
	ended := make(chan struct{})
	go func() {
		n, err := reader.Conn.Read(make([]byte, 16))
		reader.returned(n, false)
		reader.moved()
		t.Logf("the read in progress ended: %d, %v", n, err)
		close(ended)
	}()
	closed := make(chan error, 1)
	go func() { closed <- reader.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy's close of a live connection waited 5 s for a read in progress")
	}
	<-ended
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("nothing written: %d losses; want none", l)
	}
	release := reader.HoldHangUp()
	defer release()
	if reader.hungUp(reader.view()) {
		t.Fatal("a connection its handler closed is read as one whose client hung up (a socket that cannot be asked shows nothing)")
	}
}

// TestOnWindowsACloseWithBytesUnreadOnALiveConnectionIsACaptureLoss: the
// proxy's close of a live connection tosses what its socket holds unread
// (FIONREAD), bytes the application wrote that no read took: the close
// charges them, whatever the count of what the reads took says, which only a
// reset's drop is read from. None, half, or all of the 4 read.
func TestOnWindowsACloseWithBytesUnreadOnALiveConnectionIsACaptureLoss(t *testing.T) {
	for _, c := range []struct {
		read int
		lost uint64
	}{{0, 1}, {2, 1}, {4, 0}} {
		p, app, side, reader := winTracked(t, 4)
		if _, err := app.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		until(t, "the bytes to arrive", func() bool { return readProxySide(side).received == 4 })
		if c.read > 0 {
			if _, err := io.ReadFull(reader, make([]byte, c.read)); err != nil {
				t.Fatal(err)
			}
		}
		if ps := readProxySide(side); ps.err != nil || ps.state != winTCPEstablished {
			t.Fatalf("before the close: %+v (%s); want ESTABLISHED", ps, tcpStateName(ps.state))
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != c.lost {
			t.Fatalf("%d of 4 bytes read, then the proxy's close of the live connection: %d losses; want %d", c.read, l, c.lost)
		}
	}
}

// TestOnWindowsACloseWaitingForAReadEndsAtKeploysStop: keploy's stop (the
// proxy's connections' context done) ends a close's wait for a read in
// progress on a reset socket, and charges nothing, as nothing more of the
// run is judged.
func TestOnWindowsACloseWaitingForAReadEndsAtKeploysStop(t *testing.T) {
	p, app, side, reader := winTracked(t, 4)
	if _, err := app.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	until(t, "the bytes to arrive", func() bool { return readProxySide(side).received == 4 })
	resetApp(t, app, side)
	reader.beginRead() // a read in progress that never returns
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { p.pairClosing(ctx, side, reader); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("keploy's stop did not end the close's wait for a read in progress (waited 5 s)")
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("keploy's stop: %d losses; want none", l)
	}
}

// TestOnWindowsAResetThatDropsBytesNoReadTookIsACaptureLoss: Windows drops
// what a socket holds unread when the application resets the connection
// (recv answers WSAECONNRESET, FIONREAD 0), while BytesIn still counts it,
// so neither the bytes wait (everything arrived) nor the close's unread count
// sees those bytes go. The close charges them, from the count of what the
// proxy's reads took: none, half, or all of the 4 the application sent.
func TestOnWindowsAResetThatDropsBytesNoReadTookIsACaptureLoss(t *testing.T) {
	for _, c := range []struct {
		read int
		lost uint64
	}{{0, 1}, {2, 1}, {4, 0}} {
		p, app, side, reader := winTracked(t, 4)
		if _, err := app.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		until(t, "the bytes to arrive", func() bool { return readProxySide(side).received == 4 })
		if c.read > 0 {
			if _, err := io.ReadFull(reader, make([]byte, c.read)); err != nil {
				t.Fatal(err)
			}
		}
		resetApp(t, app, side)
		if u, err := readUnread(side); err != nil || u != 0 {
			t.Logf("unread after the reset: %d, %v (Windows 11 drops them: 0)", u, err)
		}
		release := reader.HoldHangUp()
		if !reader.hungUp(reader.view()) {
			t.Fatal("the client's reset (CLOSED): its hang-up is not read as come")
		}
		release()
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != c.lost {
			t.Fatalf("%d of 4 bytes read, then a reset, then the proxy's close: %d losses; want %d", c.read, l, c.lost)
		}
		if err := p.WaitPairedBytes(context.Background(), 1); err != nil {
			t.Fatalf("WaitPairedBytes after the close: %v; want settled (by the close's read, or its loss)", err)
		}
	}
}

// TestOnWindowsACloseWaitsForAReadThatTookTheBytesBeforeAReset: a read whose
// system call took the 4 bytes before the application's reset, and that has
// not returned them to the handler yet (its goroutine not run since), is
// still a read in progress as the proxy's close begins: the close waits for
// it to return (the reset socket ends every read at once), then charges
// nothing, as nothing was dropped. A close that read its count first would
// charge a loss no byte made.
func TestOnWindowsACloseWaitsForAReadThatTookTheBytesBeforeAReset(t *testing.T) {
	p, app, side, reader := winTracked(t, 4)
	took := make(chan int, 1)
	release := make(chan struct{})
	reader.beginRead()
	go func() {
		n, _ := reader.Conn.Read(make([]byte, 16)) // the socket's read, not yet returned
		took <- n
		<-release
		reader.returned(n, false)
		reader.moved()
	}()
	if _, err := app.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if n := <-took; n != 4 {
		t.Fatalf("the read took %d; want 4", n)
	}
	resetApp(t, app, side)
	closed := make(chan error, 1)
	go func() { closed <- reader.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("the close ended (%v) with a read in progress on the reset socket; want it to wait for the read", err)
	case <-time.After(500 * time.Millisecond):
	}
	close(release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("a read had taken every byte before the reset: %d losses; want none", l)
	}
}
