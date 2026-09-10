package util

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
)

// testDrainIdle keeps the timing tests quick. The drain's logic does not
// depend on the size of the window — only on whether the survivor moved
// bytes during one — so exercising it at 400ms proves the same mechanism
// the production window runs. What the short window cannot check is that
// the production value is big enough to be useful; that is pinned
// separately by TestRelayDrainIdleLeavesRoomForARealUpstream.
const testDrainIdle = 400 * time.Millisecond

func tcpPair(t *testing.T, ln net.Listener) (dialed, accepted net.Conn) {
	t.Helper()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() { c, err := ln.Accept(); ch <- res{c, err} }()
	d, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	got := <-ch
	if got.err != nil {
		t.Fatalf("accept: %v", got.err)
	}
	return d, got.c
}

// splitHalfConn models the property that makes the drain necessary: a
// connection whose READ half has failed while its WRITE half still works.
//
// That is not a plain-TCP shape — resetting a socket kills both halves —
// but it is exactly what Go's tls.Conn does, keeping c.in.err and
// c.out.err separate. Production reaches these functions with SafeConns
// over *tls.Conn on every TLS-intercepted path, so this is the real case,
// not a contrivance. Writes still go to the real socket, so the assertions
// below are about bytes that genuinely crossed the wire.
type splitHalfConn struct {
	net.Conn
	readErr error
}

func (c *splitHalfConn) Read(p []byte) (int, error) {
	if c.readErr != nil {
		return 0, c.readErr
	}
	return c.Conn.Read(p)
}

// Delegated for the same reason as deadWriteConn.CloseWrite — interface
// embedding does not promote it. Not load-bearing for these tests (the
// broken direction never forwards a FIN), but a fake that silently
// swallows CloseWrite is a trap for the next person to reuse it.
func (c *splitHalfConn) CloseWrite() error { return CloseWriteIfPossible(c.Conn) }

// relayFixture wires up two real TCP pairs with one side's READ half
// broken, and runs the relay over them.
//
// breakDest selects WHICH direction fails, and running every drain test
// both ways is the point. The drain has to watch the byte counter of the
// direction that did NOT report, and code that assumes the first result
// came from the first goroutine is right in one orientation and silently
// wrong in the other — it watches the dead direction's counter, which
// never moves again, so the very first tick reads "stalled" and the drain
// collapses back into a single fixed wait. Tests that only ever break the
// app side certify half a function.
type relayFixture struct {
	appSide, appPeer   net.Conn
	destSide, destPeer net.Conn
	// sender writes into the relay; receiver reads what the surviving
	// direction delivered.
	sender, receiver net.Conn
	relayDone        chan struct{}
}

func newRelayFixture(t *testing.T, breakDest bool, idle time.Duration) *relayFixture {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	f := &relayFixture{relayDone: make(chan struct{})}
	f.appSide, f.appPeer = tcpPair(t, ln)
	f.destSide, f.destPeer = tcpPair(t, ln)
	t.Cleanup(func() {
		_ = f.appSide.Close()
		_ = f.appPeer.Close()
		_ = f.destSide.Close()
		_ = f.destPeer.Close()
	})

	clientConn, destConn := net.Conn(f.appSide), net.Conn(f.destSide)
	if breakDest {
		// The upstream read fails: the surviving direction is the REQUEST
		// still being uploaded to that upstream. This is the orientation
		// that shows up first in production — a reset or a TLS alert from
		// the server surfaces on the dest read while the client is still
		// mid-body.
		destConn = &splitHalfConn{Conn: f.destSide, readErr: io.ErrUnexpectedEOF}
		f.sender, f.receiver = f.appPeer, f.destPeer
	} else {
		// The client read fails: the surviving direction is the RESPONSE
		// still being delivered to the app.
		clientConn = &splitHalfConn{Conn: f.appSide, readErr: io.ErrUnexpectedEOF}
		f.sender, f.receiver = f.destPeer, f.appPeer
	}

	go func() {
		relayRawPassthrough(NewSafeConn(clientConn, zap.NewNop()), NewSafeConn(destConn, zap.NewNop()), idle)
		close(f.relayDone)
	}()
	return f
}

// awaitReturnAndClose waits for the relay to return, then models
// handleConnection: it closes the REAL sockets the moment the relay hands
// control back. Without that close these tests prove nothing — returning
// early is harmless on its own, since the copier goroutines keep
// delivering; it is the caller's close that truncates them.
func (f *relayFixture) awaitReturnAndClose(t *testing.T) {
	t.Helper()
	select {
	case <-f.relayDone:
	case <-time.After(30 * time.Second):
		t.Fatal("relayRawPassthrough never returned; a reintroduced unconditional wait shows " +
			"up here rather than as an opaque package timeout")
	}
	_ = f.appSide.Close()
	_ = f.destSide.Close()
}

// pacedSend writes payload in chunks with a gap between them, so the
// transfer is still ARRIVING when the other direction breaks. Blasting it
// in one write finishes over loopback before the break lands, leaving
// nothing in flight to truncate — which is how the first version of these
// tests passed against a build that truncated.
func pacedSend(c net.Conn, payload []byte, chunkSize int, gap time.Duration) {
	go func() {
		for off := 0; off < len(payload); off += chunkSize {
			end := off + chunkSize
			if end > len(payload) {
				end = len(payload)
			}
			if _, err := c.Write(payload[off:end]); err != nil {
				return
			}
			time.Sleep(gap)
		}
	}()
}

func readAllInto(c net.Conn, n int) <-chan int {
	got := make(chan int, 1)
	go func() {
		buf := make([]byte, n)
		read, _ := io.ReadFull(c, buf)
		got <- read
	}()
	return got
}

// TestRelayRawPassthrough_LetsTheSurvivorFinishAfterABreak is the pin that
// makes the early exit safe, and the one whose absence made the first
// version of this function a data-loss regression.
//
// The two directions do NOT necessarily fail together. Production passes
// SafeConns over *tls.Conn, and Go's tls.Conn keeps its read and write
// halves' errors separate — so a read-side break leaves a healthy write
// half with already-decrypted bytes still to deliver. Returning the
// instant either direction errors lets the caller's deferred close land on
// that healthy direction and truncate it.
func TestRelayRawPassthrough_LetsTheSurvivorFinishAfterABreak(t *testing.T) {
	for _, tc := range []struct {
		name      string
		breakDest bool
	}{
		{"client read half fails, response survives", false},
		{"upstream read half fails, request survives", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const chunks, chunkSize = 20, 32 * 1024
			payload := bytes.Repeat([]byte("R"), chunks*chunkSize)

			f := newRelayFixture(t, tc.breakDest, testDrainIdle)
			pacedSend(f.sender, payload, chunkSize, 5*time.Millisecond)
			got := readAllInto(f.receiver, len(payload))

			f.awaitReturnAndClose(t)

			select {
			case n := <-got:
				if n < len(payload) {
					t.Fatalf("the surviving direction delivered %d of %d bytes. Returning the "+
						"moment either direction errors lets the caller's deferred close "+
						"truncate a healthy direction — the two halves of a tls.Conn fail "+
						"independently.", n, len(payload))
				}
			case <-time.After(5 * time.Second):
				t.Fatal("reader never completed")
			}
		})
	}
}

// TestRelayRawPassthrough_DrainIsIdleNotAWallClock pins the difference
// between "stop a stalled survivor" and "cut every survivor off after a
// fixed interval".
//
// A fixed grace is only a slower way of abandoning the survivor: a
// response that legitimately takes longer than the grace gets truncated,
// and this proxy sits in front of real upstreams where that is entirely
// ordinary. Measured on the wall-clock version: 33% of a 960 KiB response
// delivered before the cut.
//
// Here the survivor keeps delivering, slowly, for well beyond one idle
// window — every byte must still arrive, in BOTH orientations. Watching
// the wrong direction's counter reproduces the wall clock exactly, and
// only in one of them.
func TestRelayRawPassthrough_DrainIsIdleNotAWallClock(t *testing.T) {
	for _, tc := range []struct {
		name      string
		breakDest bool
	}{
		{"client read half fails, response survives", false},
		{"upstream read half fails, request survives", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Total transfer time well over one window, but never idle for a
			// whole window: a wall clock truncates this, an idle timer does not.
			const chunks, chunkSize = 12, 16 * 1024
			gap := testDrainIdle / 3
			payload := bytes.Repeat([]byte("S"), chunks*chunkSize)

			f := newRelayFixture(t, tc.breakDest, testDrainIdle)
			pacedSend(f.sender, payload, chunkSize, gap)
			got := readAllInto(f.receiver, len(payload))

			f.awaitReturnAndClose(t)

			select {
			case n := <-got:
				if n < len(payload) {
					t.Fatalf("the surviving direction delivered %d of %d bytes over %v. A fixed "+
						"grace is a deadline on the whole transfer, not a flush — it truncates "+
						"any response slower than the grace, which for a real upstream is normal.",
						n, len(payload), time.Duration(chunks)*gap)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("reader never completed")
			}
		})
	}
}

// TestRelayRawPassthrough_WaitsThroughUpstreamThinkTime covers the window
// BEFORE the survivor's first byte.
//
// The idle clock starts at the break, so a survivor that has not begun
// delivering yet is indistinguishable from one that has stalled — it has
// moved zero bytes either way. An upstream that is still computing when
// the other direction fails therefore loses its entire response if the
// window is shorter than its think time, which is why the window is sized
// for a real upstream rather than for a loopback test.
func TestRelayRawPassthrough_WaitsThroughUpstreamThinkTime(t *testing.T) {
	for _, tc := range []struct {
		name      string
		breakDest bool
	}{
		{"client read half fails, response survives", false},
		{"upstream read half fails, request survives", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte("the answer, eventually")
			think := testDrainIdle * 3 / 4 // silent for most of a window, then answers

			f := newRelayFixture(t, tc.breakDest, testDrainIdle)
			go func() {
				time.Sleep(think)
				_, _ = f.sender.Write(payload)
			}()
			got := readAllInto(f.receiver, len(payload))

			f.awaitReturnAndClose(t)

			select {
			case n := <-got:
				if n < len(payload) {
					t.Fatalf("delivered %d of %d bytes after a %v think time. The drain treats "+
						"'has not started yet' as 'stalled', so a window shorter than the "+
						"upstream's first-byte latency drops the whole response.",
						n, len(payload), think)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("reader never completed")
			}
		})
	}
}

// TestRelayDrainIdleLeavesRoomForARealUpstream is the one assertion the
// short-window tests cannot make.
//
// Everything above runs the drain at testDrainIdle, which proves the
// mechanism but says nothing about whether the production window is long
// enough to be worth having. It is not an arbitrary number: the window is
// the maximum time a survivor may be SILENT before keploy decides it is
// dead, and ordinary upstreams are silent for hundreds of milliseconds
// before their first byte and between streamed chunks. A previous 500ms
// window lost entire responses to an upstream that thought for 800ms.
func TestRelayDrainIdleLeavesRoomForARealUpstream(t *testing.T) {
	if RelayDrainIdle < 5*time.Second {
		t.Fatalf("RelayDrainIdle is %v. This is how long a survivor may go quiet before its "+
			"bytes are abandoned, and sub-second silence is normal for a database mid-query, "+
			"an upstream computing its first byte, or an event stream between heartbeats. "+
			"Shrinking it back turns the idle drain into the fixed wall clock it replaced.",
			RelayDrainIdle)
	}
}

// TestRelayRawPassthrough_ReturnsWhenOneDirectionBreaks is the liveness
// half: a broken pair must not wedge the caller indefinitely.
//
// The bound is real but not "forever" — the surviving peer's own close or
// timeout releases it. What it costs meanwhile is the CALLER: the
// abandoned copier itself exits as soon as handleConnection's defer closes
// the real sockets, so nothing lingers past the return.
func TestRelayRawPassthrough_ReturnsWhenOneDirectionBreaks(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	appSide, appPeer := tcpPair(t, ln)
	destSide, destPeer := tcpPair(t, ln)
	defer func() { _ = appSide.Close(); _ = appPeer.Close(); _ = destSide.Close() }()

	// The app peer stays open and silent, so app->dest can only end by a FIN.
	go func() { _, _ = appPeer.Write([]byte("hello")) }()
	go func() {
		time.Sleep(50 * time.Millisecond)
		if tc, ok := destPeer.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		_ = destPeer.Close()
	}()

	returned := make(chan struct{})
	go func() {
		relayRawPassthrough(NewSafeConn(appSide, zap.NewNop()), NewSafeConn(destSide, zap.NewNop()), testDrainIdle)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("relayRawPassthrough did not return after one direction was reset. keploy " +
			"cannot wake the surviving io.Copy before session teardown — SafeConn's Close and " +
			"deadline setters are no-ops — so this holds a goroutine and both fds until the " +
			"surviving peer gives up, on the one path that runs only under memory pressure.")
	}
}

// TestRelayRawPassthrough_ForwardsFINOnCleanEOF: a clean end of stream must
// still propagate, or an EOF-delimited peer waits forever for a reply that
// was already complete.
//
// This one calls the exported entry point, so the wiring from
// RelayRawPassthrough down to the copy loop is covered by a test too, not
// only the parameterised form the timing tests use.
func TestRelayRawPassthrough_ForwardsFINOnCleanEOF(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	appSide, appPeer := tcpPair(t, ln)
	destSide, destPeer := tcpPair(t, ln)
	defer func() { _ = appSide.Close(); _ = appPeer.Close(); _ = destSide.Close(); _ = destPeer.Close() }()

	gotAll := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(destPeer) // returns only once the FIN arrives
		gotAll <- b
	}()

	go RelayRawPassthrough(NewSafeConn(appSide, zap.NewNop()), NewSafeConn(destSide, zap.NewNop()))

	if _, err := appPeer.Write([]byte("request")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if tc, ok := appPeer.(*net.TCPConn); ok {
		if err := tc.CloseWrite(); err != nil {
			t.Fatalf("half-close: %v", err)
		}
	}

	select {
	case b := <-gotAll:
		if string(b) != "request" {
			t.Fatalf("destination got %q, want %q", b, "request")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the destination never saw EOF: the client's FIN was not forwarded, so an " +
			"EOF-delimited peer would wait forever for a request that was already complete")
	}
}

// deadWriteConn fails on Write while Read keeps working — the other half
// of tls.Conn's split error state, and the case where forwarding a FIN is
// most wrong: dst may be alive after a short write, so claiming the
// message is complete is a lie about data that never arrived.
type deadWriteConn struct {
	net.Conn
	writeErr error
}

func (c *deadWriteConn) Write(p []byte) (int, error) {
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return c.Conn.Write(p)
}

// CloseWrite must be delegated explicitly. Embedding net.Conn as an
// INTERFACE promotes only net.Conn's method set, so CloseWrite is NOT
// inherited — a fake without this silently swallows every FIN and any test
// built on it certifies nothing. That exact omission has shipped four
// times in this tree — readTimeReportingConn, readTrackingConn, SafeConn
// and util.Conn each shipped without it and each had to be fixed, so they
// all carry an explicit CloseWrite today — and it made the first version
// of the write-error test below pass against a build that forwarded the
// FIN unconditionally.
func (c *deadWriteConn) CloseWrite() error { return CloseWriteIfPossible(c.Conn) }

// TestRelayRawPassthrough_NoFINAfterAWriteError covers the direction the
// other tests miss: io.Copy also exits when the WRITE to dst fails, and a
// FIN there tells the peer a truncated exchange finished cleanly.
func TestRelayRawPassthrough_NoFINAfterAWriteError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	appSide, appPeer := tcpPair(t, ln)
	destSide, destPeer := tcpPair(t, ln)
	defer func() { _ = appSide.Close(); _ = appPeer.Close(); _ = destSide.Close(); _ = destPeer.Close() }()

	sawEOF := make(chan struct{})
	go func() {
		_, _ = io.ReadAll(destPeer) // returns when destSide's write half closes
		close(sawEOF)
	}()

	go func() { _, _ = appPeer.Write([]byte("request")); _ = appPeer.Close() }()

	// Writing to the destination fails, so the app->dest copy exits with an
	// error even though the app side ended cleanly.
	dead := &deadWriteConn{Conn: destSide, writeErr: io.ErrClosedPipe}
	returned := make(chan struct{})
	go func() {
		relayRawPassthrough(NewSafeConn(appSide, zap.NewNop()), NewSafeConn(dead, zap.NewNop()), testDrainIdle)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(30 * time.Second):
		// Run it in a goroutine so a liveness regression fails HERE with a
		// name, instead of hanging until the package-level timeout kills
		// every test in the package with a stack dump.
		t.Fatal("relayRawPassthrough never returned after a write error")
	}

	select {
	case <-sawEOF:
		t.Fatal("a FIN was forwarded to the destination after the write to it FAILED. That " +
			"tells the peer the request is complete when its bytes never arrived.")
	case <-time.After(300 * time.Millisecond):
		// No FIN — correct.
	}
}

// TestDeadWriteConnActuallyForwardsCloseWrite is the positive control for
// the fake above.
//
// TestRelayRawPassthrough_NoFINAfterAWriteError passes when NO FIN
// arrives, so it also passes when the fake silently swallows every FIN —
// at which point it asserts nothing at all. Interface embedding does not
// promote CloseWrite, and that omission has shipped four times in this
// tree, so the fake's delegation needs its own proof. Here the write half
// is healthy and the copy ends cleanly, so a FIN MUST come through.
func TestDeadWriteConnActuallyForwardsCloseWrite(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	appSide, appPeer := tcpPair(t, ln)
	destSide, destPeer := tcpPair(t, ln)
	defer func() { _ = appSide.Close(); _ = appPeer.Close(); _ = destSide.Close(); _ = destPeer.Close() }()

	gotAll := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(destPeer) // returns only once the FIN arrives
		gotAll <- b
	}()

	// Same fake, healthy write half: whatever the FIN test proves about the
	// error case, this proves the fake can carry a FIN at all.
	healthy := &deadWriteConn{Conn: destSide}
	go relayRawPassthrough(NewSafeConn(appSide, zap.NewNop()), NewSafeConn(healthy, zap.NewNop()), testDrainIdle)

	if _, err := appPeer.Write([]byte("request")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if tc, ok := appPeer.(*net.TCPConn); ok {
		if err := tc.CloseWrite(); err != nil {
			t.Fatalf("half-close: %v", err)
		}
	}

	select {
	case b := <-gotAll:
		if string(b) != "request" {
			t.Fatalf("destination got %q, want %q", b, "request")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deadWriteConn swallowed CloseWrite, so TestRelayRawPassthrough_NoFINAfterAWriteError " +
			"is a tautology: it would pass against a build that forwards the FIN unconditionally")
	}
}

// TestRelayRawPassthrough_ProductionEntryPointDrainsTheSurvivor closes the
// gap the parameterised window opens.
//
// Every other drain test calls relayRawPassthrough with testDrainIdle, so
// the line that actually wires the production constant into the mechanism
// is executed but never CHECKED. Severing it — passing 0, or any value too
// small to cover a real transfer — abandons the survivor immediately,
// which is this function's original data-loss bug, and the rest of the
// suite stays green because none of it goes through this door.
//
// So drive the exported entry point, on the production constant, through
// the survivor path. It does not cost 30s: a survivor is only ever waited
// ON the window when its source goes silent while still open. Here the
// source closes when it is done, io.Copy returns, and the relay returns
// with it.
func TestRelayRawPassthrough_ProductionEntryPointDrainsTheSurvivor(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	appSide, appPeer := tcpPair(t, ln)
	destSide, destPeer := tcpPair(t, ln)
	defer func() { _ = appSide.Close(); _ = appPeer.Close(); _ = destSide.Close(); _ = destPeer.Close() }()

	const chunks, chunkSize = 10, 16 * 1024
	payload := bytes.Repeat([]byte("P"), chunks*chunkSize)
	go func() {
		for i := 0; i < chunks; i++ {
			if _, err := destPeer.Write(payload[i*chunkSize : (i+1)*chunkSize]); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		// Closing ends the survivor's copy, so the relay returns on its own
		// rather than sitting out the production idle window.
		if tc, ok := destPeer.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	got := readAllInto(appPeer, len(payload))

	broken := &splitHalfConn{Conn: appSide, readErr: io.ErrUnexpectedEOF}
	relayDone := make(chan struct{})
	go func() {
		// The EXPORTED function, on RelayDrainIdle. That is the whole point.
		RelayRawPassthrough(NewSafeConn(broken, zap.NewNop()), NewSafeConn(destSide, zap.NewNop()))
		close(relayDone)
	}()
	select {
	case <-relayDone:
	case <-time.After(60 * time.Second):
		t.Fatal("RelayRawPassthrough never returned")
	}
	_ = appSide.Close()
	_ = destSide.Close()

	select {
	case n := <-got:
		if n < len(payload) {
			t.Fatalf("the surviving direction delivered %d of %d bytes through the EXPORTED "+
				"entry point. The drain is only as good as the window wired into it here; a "+
				"window too small to cover a real transfer abandons the survivor and the "+
				"caller's close truncates it.", n, len(payload))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reader never completed")
	}
}

// TestRelayRawPassthrough_ReplySurvivesTheClientFIN is the shape an
// EOF-delimited exchange actually takes, and nothing else in this tree
// pins it.
//
// The client sends a request and half-closes. That is a CLEAN end of one
// direction, so the copy loop must treat it as "one direction finished"
// and keep waiting for the other. Returning there instead — the natural
// mistake, since a nil error looks like completion — hands control back to
// handleConnection, whose deferred close then destroys the reply the
// upstream was in the middle of sending.
//
// Modelling the caller's close is what gives this test teeth: returning
// early is harmless on its own, because the copier goroutines keep
// running. It is the close that turns it into lost data.
func TestRelayRawPassthrough_ReplySurvivesTheClientFIN(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	appSide, appPeer := tcpPair(t, ln)
	destSide, destPeer := tcpPair(t, ln)
	defer func() { _ = appSide.Close(); _ = appPeer.Close(); _ = destSide.Close(); _ = destPeer.Close() }()

	reply := bytes.Repeat([]byte("A"), 64*1024)

	// A realistic upstream: read the request to EOF, think, then answer.
	go func() {
		if _, err := io.ReadAll(destPeer); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
		_, _ = destPeer.Write(reply)
		if tc, ok := destPeer.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	got := readAllInto(appPeer, len(reply))

	relayDone := make(chan struct{})
	go func() {
		RelayRawPassthrough(NewSafeConn(appSide, zap.NewNop()), NewSafeConn(destSide, zap.NewNop()))
		close(relayDone)
	}()

	if _, err := appPeer.Write([]byte("request")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if tc, ok := appPeer.(*net.TCPConn); ok {
		if err := tc.CloseWrite(); err != nil {
			t.Fatalf("half-close: %v", err)
		}
	}

	select {
	case <-relayDone:
	case <-time.After(60 * time.Second):
		t.Fatal("RelayRawPassthrough never returned")
	}
	// handleConnection closes the real sockets the instant this returns.
	_ = appSide.Close()
	_ = destSide.Close()

	select {
	case n := <-got:
		if n < len(reply) {
			t.Fatalf("the client got %d of %d reply bytes after half-closing. A clean EOF on "+
				"one direction means that direction finished, NOT that the exchange did — "+
				"returning there lets the caller's close destroy a reply that was still "+
				"arriving.", n, len(reply))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("client never received the reply")
	}
}

// TestRelayRawPassthrough_CleanThenResetReturnsPromptly guards the early
// return for the case the code calls "very common": one side ends
// cleanly, the other resets.
//
// Both results are already in hand there, so there is nothing left to
// drain and no reason to wait. Dropping that check costs a full idle
// window of dead time per connection — 30s in production — and no other
// test notices, because they all run on a short window where the
// difference is invisible.
func TestRelayRawPassthrough_CleanThenResetReturnsPromptly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	appSide, appPeer := tcpPair(t, ln)
	destSide, destPeer := tcpPair(t, ln)
	defer func() { _ = appSide.Close(); _ = appPeer.Close(); _ = destSide.Close() }()

	// app->dest ends cleanly; dest->app resets. Both land quickly, so the
	// loop reaches its second iteration with both results in hand.
	go func() {
		_, _ = appPeer.Write([]byte("hello"))
		if tc, ok := appPeer.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	go func() {
		time.Sleep(20 * time.Millisecond)
		if tc, ok := destPeer.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		_ = destPeer.Close()
	}()

	start := time.Now()
	returned := make(chan struct{})
	go func() {
		relayRawPassthrough(NewSafeConn(appSide, zap.NewNop()), NewSafeConn(destSide, zap.NewNop()), testDrainIdle)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("relayRawPassthrough never returned")
	}
	// Comfortably under a window, but not so tight that a loaded runner
	// trips it: the bug this catches costs a WHOLE window, not a slice.
	if elapsed := time.Since(start); elapsed > testDrainIdle*3/4 {
		t.Fatalf("returned after %v, which is most of one %v idle window. Both copy results "+
			"were already available, so there was nothing to drain — waiting anyway burns a "+
			"full window of dead time on every clean-then-reset connection, and in production "+
			"that window is %v.", elapsed, testDrainIdle, RelayDrainIdle)
	}
}
