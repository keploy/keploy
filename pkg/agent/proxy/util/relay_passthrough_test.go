package util

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/zap"
)

// testDrainIdle keeps the real-TCP liveness/FIN tests quick. The multi-window
// survivor drain and prompt-return tests below run inside testing/synctest on
// RelayDrainIdle directly over in-memory half-closable conns, so none of them
// race wall-clock sleeps against this value.
const testDrainIdle = 400 * time.Millisecond

var errMemConnReset = errors.New("read: connection reset by peer")

// memStream is one unidirectional byte pipe of an in-memory net.Conn pair.
// Every blocking wait uses bubble-local channels so testing/synctest treats
// blocked Reads and Writes as durably blocked.
type memStream struct {
	data   chan []byte
	closed chan struct{}

	mu           sync.Mutex
	isClosed     bool
	readerClosed bool
	reset        bool
	closeErr     error
	rem          []byte
}

func newMemStream() *memStream {
	return &memStream{
		data:   make(chan []byte, 64),
		closed: make(chan struct{}),
	}
}

func (s *memStream) write(p []byte) (int, error) {
	select {
	case <-s.closed:
		return 0, net.ErrClosed
	default:
	}
	if len(p) == 0 {
		return 0, nil
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	select {
	case <-s.closed:
		return 0, net.ErrClosed
	case s.data <- cp:
		return len(p), nil
	}
}

func (s *memStream) read(p []byte) (int, error) {
	if err := s.abortErr(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	if len(s.rem) > 0 {
		n := copy(p, s.rem)
		s.rem = s.rem[n:]
		s.mu.Unlock()
		return n, nil
	}
	s.mu.Unlock()

	// Drain already-buffered bytes before reporting a clean EOF so data
	// written prior to CloseWrite is always delivered first.
	select {
	case chunk := <-s.data:
		if err := s.abortErr(); err != nil {
			return 0, err
		}
		return s.consume(p, chunk), nil
	default:
	}

	select {
	case chunk := <-s.data:
		if err := s.abortErr(); err != nil {
			return 0, err
		}
		return s.consume(p, chunk), nil
	case <-s.closed:
		if err := s.abortErr(); err != nil {
			return 0, err
		}
		select {
		case chunk := <-s.data:
			return s.consume(p, chunk), nil
		default:
			s.mu.Lock()
			err := s.closeErr
			s.mu.Unlock()
			if err == nil {
				err = io.EOF
			}
			return 0, err
		}
	}
}

func (s *memStream) consume(p, chunk []byte) int {
	n := copy(p, chunk)
	if n < len(chunk) {
		s.mu.Lock()
		s.rem = append(s.rem[:0], chunk[n:]...)
		s.mu.Unlock()
	}
	return n
}

func (s *memStream) abortErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readerClosed {
		return net.ErrClosed
	}
	if s.reset {
		return s.closeErr
	}
	return nil
}

func (s *memStream) closeWrite() {
	s.mu.Lock()
	if !s.isClosed {
		s.isClosed = true
		s.closeErr = io.EOF
		close(s.closed)
	}
	s.mu.Unlock()
}

func (s *memStream) resetWrite(err error) {
	s.mu.Lock()
	s.reset = true
	if !s.isClosed {
		s.isClosed = true
		s.closeErr = err
		close(s.closed)
	} else if !s.readerClosed {
		s.closeErr = err
	}
	s.mu.Unlock()
}

func (s *memStream) closeRead() {
	s.mu.Lock()
	s.readerClosed = true
	if !s.isClosed {
		s.isClosed = true
		s.closeErr = net.ErrClosed
		close(s.closed)
	}
	s.mu.Unlock()
}

// memConn is an in-memory net.Conn that supports independent half-close
// (CloseWrite) and peer reset (Reset) without OS sockets, so RelayRawPassthrough
// can be driven on real time.Timer and RelayDrainIdle inside testing/synctest.
type memConn struct {
	rd *memStream
	wr *memStream
}

func newMemConnPair() (*memConn, *memConn) {
	aToB := newMemStream()
	bToA := newMemStream()
	return &memConn{rd: bToA, wr: aToB}, &memConn{rd: aToB, wr: bToA}
}

func (c *memConn) Read(p []byte) (int, error)       { return c.rd.read(p) }
func (c *memConn) Write(p []byte) (int, error)      { return c.wr.write(p) }
func (c *memConn) CloseWrite() error                { c.wr.closeWrite(); return nil }
func (c *memConn) LocalAddr() net.Addr              { return memAddr("mem") }
func (c *memConn) RemoteAddr() net.Addr             { return memAddr("mem") }
func (c *memConn) SetDeadline(time.Time) error      { return nil }
func (c *memConn) SetReadDeadline(time.Time) error  { return nil }
func (c *memConn) SetWriteDeadline(time.Time) error { return nil }

func (c *memConn) Close() error {
	c.rd.closeRead()
	c.wr.closeWrite()
	return nil
}

func (c *memConn) Reset() {
	c.rd.closeRead()
	c.wr.resetWrite(errMemConnReset)
}

type memAddr string

func (a memAddr) Network() string { return string(a) }
func (a memAddr) String() string  { return string(a) }

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
// not a contrivance.
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

// memRelayFixture wires up two in-memory half-closable conn pairs inside a
// testing/synctest bubble with one side's READ half broken, and runs the
// production RelayRawPassthrough entry point over them.
//
// breakDest selects WHICH direction fails, and running every drain test
// both ways is the point. The drain has to watch the byte counter of the
// direction that did NOT report, and code that assumes the first result
// came from the first goroutine is right in one orientation and silently
// wrong in the other — it watches the dead direction's counter, which
// never moves again, so the very first tick reads "stalled" and the drain
// collapses back into a single fixed wait.
type memRelayFixture struct {
	appSide, appPeer   *memConn
	destSide, destPeer *memConn
	sender, receiver   *memConn
	relayDone          chan struct{}
}

func newMemRelayFixture(breakDest bool) *memRelayFixture {
	f := &memRelayFixture{relayDone: make(chan struct{})}
	f.appSide, f.appPeer = newMemConnPair()
	f.destSide, f.destPeer = newMemConnPair()

	clientConn, destConn := net.Conn(f.appSide), net.Conn(f.destSide)
	if breakDest {
		// The upstream read fails: the surviving direction is the REQUEST
		// still being uploaded to that upstream.
		destConn = &splitHalfConn{Conn: f.destSide, readErr: io.ErrUnexpectedEOF}
		f.sender, f.receiver = f.appPeer, f.destPeer
	} else {
		// The client read fails: the surviving direction is the RESPONSE
		// still being delivered to the app.
		clientConn = &splitHalfConn{Conn: f.appSide, readErr: io.ErrUnexpectedEOF}
		f.sender, f.receiver = f.destPeer, f.appPeer
	}

	go func() {
		RelayRawPassthrough(NewSafeConn(clientConn, zap.NewNop()), NewSafeConn(destConn, zap.NewNop()))
		close(f.relayDone)
	}()
	return f
}

func (f *memRelayFixture) closeAll() {
	_ = f.appSide.Close()
	_ = f.appPeer.Close()
	_ = f.destSide.Close()
	_ = f.destPeer.Close()
}

// closeProxySides models handleConnection: it closes the real proxy-side
// sockets the moment RelayRawPassthrough hands control back. Without that
// close these tests prove nothing — returning early is harmless on its own,
// since the copier goroutines keep delivering; it is the caller's close that
// truncates them.
func (f *memRelayFixture) closeProxySides() {
	_ = f.appSide.Close()
	_ = f.destSide.Close()
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
			synctest.Test(t, func(t *testing.T) {
				const chunks, chunkSize = 20, 32 * 1024
				payload := bytes.Repeat([]byte("R"), chunks*chunkSize)
				got := make([]byte, len(payload))

				f := newMemRelayFixture(tc.breakDest)
				defer f.closeAll()

				// Wait for the broken direction to fail and WaitForSurvivor to
				// arm its real RelayDrainIdle timer before the survivor starts.
				synctest.Wait()
				select {
				case <-f.relayDone:
					f.closeProxySides()
					t.Fatal("RelayRawPassthrough returned immediately on the first copy error without draining the survivor")
				default:
				}

				gap := RelayDrainIdle / 4
				for i := 0; i < chunks; i++ {
					time.Sleep(gap)
					synctest.Wait()
					select {
					case <-f.relayDone:
						f.closeProxySides()
						t.Fatalf("the surviving direction delivered %d of %d bytes. Returning the "+
							"moment either direction errors lets the caller's deferred close "+
							"truncate a healthy direction — the two halves of a tls.Conn fail "+
							"independently.", i*chunkSize, len(payload))
					default:
					}
					off := i * chunkSize
					if _, err := f.sender.Write(payload[off : off+chunkSize]); err != nil {
						t.Fatalf("chunk %d write: %v", i+1, err)
					}
					if _, err := io.ReadFull(f.receiver, got[off:off+chunkSize]); err != nil {
						t.Fatalf("chunk %d read: %v", i+1, err)
					}
					synctest.Wait()
				}
				if !bytes.Equal(got, payload) {
					t.Fatal("surviving direction delivered corrupted bytes")
				}

				// Go silent without closing: RelayRawPassthrough must stop
				// within two idle windows (one to observe the final chunk's
				// byte count, one with no further progress).
				silenceStart := time.Now()
				select {
				case <-f.relayDone:
				case <-time.After(3 * RelayDrainIdle):
					t.Fatal("RelayRawPassthrough never returned after the survivor stalled")
				}
				if wait := time.Since(silenceStart); wait > 2*RelayDrainIdle {
					t.Fatalf("RelayRawPassthrough stalled after %v of silence, want <= %v", wait, 2*RelayDrainIdle)
				}
				f.closeProxySides()
				synctest.Wait()
			})
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
// Here the survivor keeps delivering, slowly, across six full RelayDrainIdle
// windows — every byte must still arrive, in BOTH orientations. Watching
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
			synctest.Test(t, func(t *testing.T) {
				const windows, chunksPerWindow, chunkSize = 6, 2, 16 * 1024
				payload := bytes.Repeat([]byte("S"), windows*chunksPerWindow*chunkSize)
				got := make([]byte, len(payload))

				f := newMemRelayFixture(tc.breakDest)
				defer f.closeAll()

				// Wait until the broken direction has failed and WaitForSurvivor
				// has armed the real RelayDrainIdle timer.
				synctest.Wait()
				select {
				case <-f.relayDone:
					f.closeProxySides()
					t.Fatal("RelayRawPassthrough returned without arming the survivor drain")
				default:
				}

				step := RelayDrainIdle * 2 / 3
				rem := RelayDrainIdle - step
				for w := 0; w < windows; w++ {
					// Stay silent for 2/3 of the production idle window before
					// sending: this catches both a shortened initial window and
					// a shortened Reset duration (e.g. idle/2).
					time.Sleep(step)
					synctest.Wait()
					select {
					case <-f.relayDone:
						f.closeProxySides()
						t.Fatalf("window %d of %d: surviving direction was abandoned after %d of %d bytes "+
							"before its idle window expired", w+1, windows, w*chunksPerWindow*chunkSize, len(payload))
					default:
					}

					for c := 0; c < chunksPerWindow; c++ {
						off := (w*chunksPerWindow + c) * chunkSize
						if _, err := f.sender.Write(payload[off : off+chunkSize]); err != nil {
							t.Fatalf("window %d chunk %d write: %v", w+1, c+1, err)
						}
						if _, err := io.ReadFull(f.receiver, got[off:off+chunkSize]); err != nil {
							t.Fatalf("window %d chunk %d read: %v", w+1, c+1, err)
						}
					}
					synctest.Wait()

					// Advance across the idle timer tick; WaitForSurvivor must
					// observe the delivered bytes and reset for another window.
					time.Sleep(rem)
					synctest.Wait()
					select {
					case <-f.relayDone:
						f.closeProxySides()
						t.Fatalf("window %d of %d: surviving direction was abandoned after %d of %d bytes. "+
							"A fixed grace or watching the dead direction's counter truncates an active survivor.",
							w+1, windows, (w+1)*chunksPerWindow*chunkSize, len(payload))
					default:
					}
				}

				if !bytes.Equal(got, payload) {
					t.Fatal("surviving direction delivered corrupted bytes")
				}

				// Go silent without closing: the drain must stall after one
				// full RelayDrainIdle window with no new bytes.
				silenceStart := time.Now()
				select {
				case <-f.relayDone:
				case <-time.After(2 * RelayDrainIdle):
					t.Fatal("drain kept resetting after the survivor went silent")
				}
				if wait := time.Since(silenceStart); wait != RelayDrainIdle {
					t.Fatalf("drain stalled after %v of silence, want %v", wait, RelayDrainIdle)
				}
				f.closeProxySides()
				synctest.Wait()
			})
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
			synctest.Test(t, func(t *testing.T) {
				part1 := []byte("the answer, ")
				part2 := []byte("eventually")
				want := append(append([]byte(nil), part1...), part2...)
				got := make([]byte, len(want))

				f := newMemRelayFixture(tc.breakDest)
				defer f.closeAll()

				// Wait until the break has armed the first RelayDrainIdle
				// window, then keep the survivor silent for 2/3 of the window.
				synctest.Wait()
				think := RelayDrainIdle * 2 / 3
				time.Sleep(think)
				synctest.Wait()
				select {
				case <-f.relayDone:
					f.closeProxySides()
					t.Fatalf("RelayRawPassthrough abandoned the survivor during %v of think time (< %v)",
						think, RelayDrainIdle)
				default:
				}

				if _, err := f.sender.Write(part1); err != nil {
					t.Fatalf("write part1: %v", err)
				}
				if _, err := io.ReadFull(f.receiver, got[:len(part1)]); err != nil {
					t.Fatalf("read part1: %v", err)
				}
				if _, err := f.sender.Write(part2); err != nil {
					t.Fatalf("write part2: %v", err)
				}
				if _, err := io.ReadFull(f.receiver, got[len(part1):]); err != nil {
					t.Fatalf("read part2: %v", err)
				}
				synctest.Wait()

				// Cross the first tick: the answer arrived within the window,
				// so the drain must reset rather than stop.
				time.Sleep(RelayDrainIdle - think)
				synctest.Wait()
				select {
				case <-f.relayDone:
					f.closeProxySides()
					t.Fatal("drain abandoned the survivor on the first tick even though it answered within the window")
				default:
				}

				if !bytes.Equal(got, want) {
					t.Fatalf("got %q, want %q", got, want)
				}

				silenceStart := time.Now()
				select {
				case <-f.relayDone:
				case <-time.After(2 * RelayDrainIdle):
					t.Fatal("drain kept resetting after survivor went silent")
				}
				if wait := time.Since(silenceStart); wait != RelayDrainIdle {
					t.Fatalf("drain stalled after %v of silence, want %v", wait, RelayDrainIdle)
				}
				f.closeProxySides()
				synctest.Wait()
			})
		})
	}
}

// TestRelayDrainIdleLeavesRoomForARealUpstream pins the floor on the
// production constant itself.
//
// The window is the maximum time a survivor may be SILENT before keploy
// decides it is dead, and ordinary upstreams are silent for hundreds of
// milliseconds before their first byte and between streamed chunks. A
// previous 500ms window lost entire responses to an upstream that thought
// for 800ms.
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

// TestRelayRawPassthrough_ProductionEntryPointDrainsTheSurvivor drives the
// exported RelayRawPassthrough entry point over multi-second chunk gaps
// followed by a clean survivor FIN.
//
// Each gap is RelayDrainIdle/2 (15s), which catches a call site that wires a
// sub-second window instead of RelayDrainIdle, and the ending CloseWrite
// verifies that a survivor finishing cleanly returns immediately without
// sitting out a trailing idle window.
func TestRelayRawPassthrough_ProductionEntryPointDrainsTheSurvivor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newMemRelayFixture(false)
		defer f.closeAll()

		synctest.Wait()
		select {
		case <-f.relayDone:
			f.closeProxySides()
			t.Fatal("RelayRawPassthrough returned immediately without draining the survivor")
		default:
		}

		const chunks, chunkSize = 10, 16 * 1024
		gap := RelayDrainIdle / 2
		payload := bytes.Repeat([]byte("P"), chunks*chunkSize)
		got := make([]byte, len(payload))
		start := time.Now()
		for i := 0; i < chunks; i++ {
			time.Sleep(gap)
			synctest.Wait()
			select {
			case <-f.relayDone:
				f.closeProxySides()
				t.Fatalf("the surviving direction delivered %d of %d bytes through the EXPORTED "+
					"entry point after %v. The drain is only as good as the window wired into it here; "+
					"a window shorter than RelayDrainIdle (%v) abandons the survivor and the caller's "+
					"close truncates it.", i*chunkSize, len(payload), time.Since(start), RelayDrainIdle)
			default:
			}
			off := i * chunkSize
			if _, err := f.sender.Write(payload[off : off+chunkSize]); err != nil {
				t.Fatalf("chunk %d write: %v", i+1, err)
			}
			if _, err := io.ReadFull(f.receiver, got[off:off+chunkSize]); err != nil {
				t.Fatalf("chunk %d read: %v", i+1, err)
			}
			synctest.Wait()
		}
		if !bytes.Equal(got, payload) {
			t.Fatal("surviving direction delivered corrupted bytes")
		}

		// Closing ends the survivor's copy, so the relay must return
		// immediately rather than sitting out the production idle window.
		finAt := time.Now()
		if err := f.sender.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
		select {
		case <-f.relayDone:
		case <-time.After(2 * RelayDrainIdle):
			t.Fatal("RelayRawPassthrough never returned after survivor half-closed")
		}
		if waitAfterFIN := time.Since(finAt); waitAfterFIN != 0 {
			t.Fatalf("RelayRawPassthrough waited %v after survivor FIN, want 0", waitAfterFIN)
		}
		f.closeProxySides()
		synctest.Wait()
	})
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
// drain and no reason to wait. Running inside testing/synctest ensures the
// first clean outcome is consumed before the peer resets and asserts exact
// zero fake elapsed time on return, catching both a missing early return
// and any fixed grace/sleep on that branch without wall-clock flakiness.
func TestRelayRawPassthrough_CleanThenResetReturnsPromptly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		appSide, appPeer := newMemConnPair()
		destSide, destPeer := newMemConnPair()
		defer func() {
			_ = appSide.Close()
			_ = appPeer.Close()
			_ = destSide.Close()
			_ = destPeer.Close()
		}()

		returned := make(chan struct{})
		go func() {
			RelayRawPassthrough(NewSafeConn(appSide, zap.NewNop()), NewSafeConn(destSide, zap.NewNop()))
			close(returned)
		}()

		if _, err := appPeer.Write([]byte("hello")); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := appPeer.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
		got, err := io.ReadAll(destPeer)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if string(got) != "hello" {
			t.Fatalf("dest got %q, want %q", got, "hello")
		}

		// Ensure the clean app->dest outcome has already been consumed by the
		// relay loop (i == 0) before the destination resets, so the reset is
		// guaranteed to land on i == 1.
		synctest.Wait()

		start := time.Now()
		destPeer.Reset()

		select {
		case <-returned:
		case <-time.After(2 * RelayDrainIdle):
			t.Fatalf("RelayRawPassthrough did not return after clean EOF followed by reset")
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("RelayRawPassthrough waited %v after both copy results were in hand, want 0", elapsed)
		}

		_ = appSide.Close()
		_ = destSide.Close()
		synctest.Wait()
	})
}
