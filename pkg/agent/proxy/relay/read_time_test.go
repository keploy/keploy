package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// layerConn stands in for a TLS layer over the conn the relay's upgrade hands
// it (the read-tracking conn): it reads from that conn into a buffer of its own
// and hands what it holds on in pieces of at most piece bytes, as tls.Conn
// hands on one record at a time of what one read under it brought in. With
// readAfter, a Read that empties the buffer reads once more under it before it
// returns, as tls.Conn does when an alert follows the record it hands on.
type layerConn struct {
	net.Conn
	piece     int
	readAfter bool
	// beforeHand is called before the nth Read with bytes in hand returns
	// them (n from 1); beforeRaw before the nth read under the layer.
	beforeHand func(n int)
	beforeRaw  func(n int)
	// failHand, when not 0, is the Read with bytes in hand that returns
	// failWith instead and keeps them, as tls.Conn does when what it holds
	// after a record is the peer's close_notify (io.EOF).
	failHand int
	failWith error

	buf          []byte
	hands, raws  int
	afterReadErr error
}

func (c *layerConn) rawRead() error {
	c.raws++
	if c.beforeRaw != nil {
		c.beforeRaw(c.raws)
	}
	raw := make([]byte, 4096)
	n, err := c.Conn.Read(raw)
	c.buf = append(c.buf, raw[:n]...)
	return err
}

func (c *layerConn) Read(p []byte) (int, error) {
	if c.afterReadErr != nil {
		return 0, c.afterReadErr
	}
	for len(c.buf) == 0 {
		if err := c.rawRead(); err != nil && len(c.buf) == 0 {
			return 0, err
		}
	}
	c.hands++
	if c.hands == c.failHand {
		return 0, c.failWith
	}
	if c.beforeHand != nil {
		c.beforeHand(c.hands)
	}
	if c.piece > 0 && len(p) > c.piece {
		p = p[:c.piece]
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	if c.readAfter && len(c.buf) == 0 {
		if err := c.rawRead(); err != nil {
			c.afterReadErr = err
			return n, err
		}
	}
	return n, nil
}

// upgradedThrough starts a relay whose client side the parser has upgraded
// to TLS, served by the layer layer builds over the conn the upgrade hands it.
func upgradedThrough(t *testing.T, layer func(net.Conn) net.Conn) *relayHarness {
	t.Helper()
	return upgradedSideThrough(t, false, layer)
}

// upgradedDestThrough is upgradedThrough for the destination side: the layer
// carries the server's replies, what most mocks are built from.
func upgradedDestThrough(t *testing.T, layer func(net.Conn) net.Conn) *relayHarness {
	t.Helper()
	return upgradedSideThrough(t, true, layer)
}

func upgradedSideThrough(t *testing.T, dest bool, layer func(net.Conn) net.Conn) *relayHarness {
	t.Helper()
	h := newHarness(t, Config{TLSUpgradeFn: func(_ context.Context, conn net.Conn, isClient bool, _ *tls.Config) (net.Conn, error) {
		// isClient: the relay is the TLS client, on the destination side.
		if isClient != dest {
			return conn, nil
		}
		return layer(conn), nil
	}})
	d := directive.UpgradeTLS(nil, &tls.Config{}, "client-side upgrade")
	if dest {
		d = directive.UpgradeTLS(&tls.Config{}, nil, "dest-side upgrade")
	}
	h.r.Directives() <- d
	select {
	case ack := <-h.r.Acks():
		if !ack.OK {
			t.Fatalf("TLS upgrade failed: %+v", ack)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no ack for the TLS upgrade")
	}
	// The forwarder writes each chunk on before it tees it, and a pipe's
	// Write waits for its reader: the far end takes whatever comes.
	far := h.destSvc
	if dest {
		far = h.clientApp
	}
	go func() { _, _ = io.Copy(io.Discard, far) }()
	return h
}

// chunksOf reads every chunk off fc as a parser would, and hands each on.
func chunksOf(fc *fakeconn.FakeConn) <-chan fakeconn.Chunk {
	ch := make(chan fakeconn.Chunk, 8)
	go func() {
		defer close(ch)
		for {
			c, err := fc.ReadChunk()
			if err != nil {
				return
			}
			ch <- c
		}
	}()
	return ch
}

func nextChunk(t *testing.T, ch <-chan fakeconn.Chunk) fakeconn.Chunk {
	t.Helper()
	select {
	case c, ok := <-ch:
		if !ok {
			t.Fatal("the parser's stream ended before the chunk came")
		}
		return c
	case <-time.After(30 * time.Second):
		t.Fatal("no chunk reached the parser")
	}
	return fakeconn.Chunk{}
}

// await waits for a goroutine the test started to reach a point; the bound
// only ends a test that is broken.
func await(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func awaitParserWaiting(t *testing.T, fc *fakeconn.FakeConn) {
	t.Helper()
	awaitState(t, "the parser to wait for more", func() bool { w, _ := fc.Waiting(); return w })
}

// A TLS layer read the bytes under a record (the chunk's stamp is that read's
// time) and has not handed the record to the forwarder yet: decryption, or a
// starved goroutine. The watermark holds a window over that stamp until the
// parser has the chunk, and lets it pass once it has.
func TestWatermark_HoldsBytesATLSLayerReadAndHasNotHandedOn(t *testing.T) {
	inHand, release := make(chan struct{}), make(chan struct{})
	h := upgradedThrough(t, func(c net.Conn) net.Conn {
		return &layerConn{Conn: c, beforeHand: func(n int) {
			if n == 1 {
				close(inHand)
				<-release
			}
		}}
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	chunks := chunksOf(h.r.ClientStream())
	awaitParserWaiting(t, h.r.ClientStream())

	go func() { _, _ = h.clientApp.Write([]byte("query")) }()
	await(t, inHand, "the TLS layer to hold the bytes")
	end := time.Now()
	start := end.Add(-time.Second)
	if w.Settled("", "", start, end) {
		t.Error("Settled: true for a window over bytes the TLS layer read and still holds")
	}
	if !w.PendingBefore(end) {
		t.Error("PendingBefore: false while the TLS layer still holds bytes read before then")
	}
	close(release)

	c := nextChunk(t, chunks)
	if string(c.Bytes) != "query" {
		t.Fatalf("chunk = %q, want query", c.Bytes)
	}
	if c.ReadAt.After(end) {
		t.Fatalf("chunk stamped %v, after the window's end %v: the row did not hold it in the layer's hand", c.ReadAt, end)
	}
	awaitParserWaiting(t, h.r.ClientStream())
	awaitState(t, "the window to pass once the parser has the chunk", func() bool {
		return w.Settled("", "", start, end)
	})
}

// The destination's twin: the server's reply, read by the destination side's
// TLS layer and not handed to the forwarder yet, holds a window over its stamp
// until the parser has it.
func TestWatermark_HoldsAReplyTheDestTLSLayerReadAndHasNotHandedOn(t *testing.T) {
	inHand, release := make(chan struct{}), make(chan struct{})
	h := upgradedDestThrough(t, func(c net.Conn) net.Conn {
		return &layerConn{Conn: c, beforeHand: func(n int) {
			if n == 1 {
				close(inHand)
				<-release
			}
		}}
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	chunks := chunksOf(h.r.DestStream())
	awaitParserWaiting(t, h.r.DestStream())

	go func() { _, _ = h.destSvc.Write([]byte("reply")) }()
	await(t, inHand, "the destination's TLS layer to hold the reply")
	end := time.Now()
	start := end.Add(-time.Second)
	if w.Settled("", "", start, end) {
		t.Error("Settled: true for a window over a reply the destination's TLS layer read and still holds")
	}
	if !w.PendingBefore(end) {
		t.Error("PendingBefore: false while the destination's TLS layer still holds a reply read before then")
	}
	close(release)

	c := nextChunk(t, chunks)
	if string(c.Bytes) != "reply" {
		t.Fatalf("chunk = %q, want reply", c.Bytes)
	}
	if c.ReadAt.After(end) {
		t.Fatalf("chunk stamped %v, after the window's end %v: the row did not hold it in the layer's hand", c.ReadAt, end)
	}
	awaitParserWaiting(t, h.r.DestStream())
	awaitState(t, "the window to pass once the parser has the reply", func() bool {
		return w.Settled("", "", start, end)
	})
}

// One read under a TLS layer brought in two records. The layer handed the
// first on and the parser took it; the second, with the same stamp, is still
// in the layer's hand, and holds the window until the parser has it.
func TestWatermark_HoldsARecordATLSLayerKeepsAfterHandingOnOne(t *testing.T) {
	inHand, release := make(chan struct{}), make(chan struct{})
	h := upgradedThrough(t, func(c net.Conn) net.Conn {
		return &layerConn{Conn: c, piece: 4, beforeHand: func(n int) {
			if n == 2 {
				close(inHand)
				<-release
			}
		}}
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	chunks := chunksOf(h.r.ClientStream())
	awaitParserWaiting(t, h.r.ClientStream())

	go func() { _, _ = h.clientApp.Write([]byte("AAAABBBB")) }()
	first := nextChunk(t, chunks)
	if string(first.Bytes) != "AAAA" {
		t.Fatalf("first chunk = %q, want AAAA", first.Bytes)
	}
	await(t, inHand, "the TLS layer to hold the bytes")
	awaitParserWaiting(t, h.r.ClientStream())
	end := time.Now()
	start := end.Add(-time.Second)
	if w.Settled("", "", start, end) {
		t.Error("Settled: true for a window over a record the TLS layer still holds")
	}
	if !w.PendingBefore(end) {
		t.Error("PendingBefore: false while the TLS layer still holds a record read before then")
	}
	close(release)

	second := nextChunk(t, chunks)
	if string(second.Bytes) != "BBBB" {
		t.Fatalf("second chunk = %q, want BBBB", second.Bytes)
	}
	if second.ReadAt.After(end) {
		t.Fatalf("second chunk stamped %v, after the window's end %v: the row did not hold it in the layer's hand", second.ReadAt, end)
	}
	awaitParserWaiting(t, h.r.ClientStream())
	awaitState(t, "the window to pass once the parser has the record", func() bool {
		return w.Settled("", "", start, end)
	})
}

// A TLS layer that handed a record out reads once more under it before it
// returns it (an alert may follow), and that read waits. Nothing is in the
// relay's hand meanwhile, so the watermark may pass a window; the record then
// reaches the parser stamped after any window it passed, never inside one.
func TestWatermark_ARecordHeldOverAReadUnderTheTLSLayerIsStampedAfterIt(t *testing.T) {
	waiting := make(chan struct{})
	h := upgradedThrough(t, func(c net.Conn) net.Conn {
		return &layerConn{Conn: c, readAfter: true, beforeRaw: func(n int) {
			if n == 2 {
				close(waiting)
			}
		}}
	})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	chunks := chunksOf(h.r.ClientStream())
	awaitParserWaiting(t, h.r.ClientStream())

	go func() { _, _ = h.clientApp.Write([]byte("last")) }()
	await(t, waiting, "the TLS layer to read again")
	var end time.Time
	awaitState(t, "the watermark to pass a window over the record", func() bool {
		end = time.Now()
		return w.Settled("", "", end.Add(-time.Second), end)
	})
	_ = h.clientApp.Close() // the read under the layer ends with EOF

	c := nextChunk(t, chunks)
	if string(c.Bytes) != "last" {
		t.Fatalf("chunk = %q, want last", c.Bytes)
	}
	if !c.ReadAt.After(end) {
		t.Fatalf("chunk stamped %v, inside a window ending %v that the watermark passed while the record was in hand", c.ReadAt, end)
	}
}

// The connection carried a chunk before the window, and the bytes of the next
// one, inside it, are still in the TLS layer's hand: the connection did carry
// bytes within the window, and the watermark holds it.
func TestWatermark_ATLSLayerHoldingBytesInAWindowAfterAnEarlierChunkHoldsIt(t *testing.T) {
	inHand, release := make(chan struct{}), make(chan struct{})
	h := upgradedThrough(t, func(c net.Conn) net.Conn {
		return &layerConn{Conn: c, beforeHand: func(n int) {
			if n == 2 {
				close(inHand)
				<-release
			}
		}}
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	chunks := chunksOf(h.r.ClientStream())

	go func() { _, _ = h.clientApp.Write([]byte("early")) }()
	early := nextChunk(t, chunks)
	awaitParserWaiting(t, h.r.ClientStream())
	start := time.Now()
	time.Sleep(time.Millisecond) // the next read is stamped after start
	go func() { _, _ = h.clientApp.Write([]byte("query")) }()
	await(t, inHand, "the TLS layer to hold the bytes")
	end := time.Now()
	if early.ReadAt.After(start) {
		t.Fatalf("the early chunk is stamped %v, not before the window's start %v", early.ReadAt, start)
	}
	if w.Settled("", "", start, end) {
		t.Error("Settled: true for a window over bytes the TLS layer read and still holds, after a chunk from before it")
	}
	close(release)
	c := nextChunk(t, chunks)
	if c.ReadAt.Before(start) || c.ReadAt.After(end) {
		t.Fatalf("chunk stamped %v, outside the window [%v, %v]", c.ReadAt, start, end)
	}
}

// The TLS layer holds the bytes of a read from after a window: the chunk the
// connection carried inside the window was consumed, and the layer's mark is
// that later read's stamp, so the window passes.
func TestWatermark_ATLSLayerHoldingALaterReadLetsAnEarlierWindowPass(t *testing.T) {
	inHand, release := make(chan struct{}), make(chan struct{})
	h := upgradedThrough(t, func(c net.Conn) net.Conn {
		return &layerConn{Conn: c, beforeHand: func(n int) {
			if n == 2 {
				close(inHand)
				<-release
			}
		}}
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	chunks := chunksOf(h.r.ClientStream())
	awaitParserWaiting(t, h.r.ClientStream())

	go func() { _, _ = h.clientApp.Write([]byte("early")) }()
	early := nextChunk(t, chunks)
	awaitParserWaiting(t, h.r.ClientStream())
	end := time.Now()
	start := early.ReadAt.Add(-time.Second)
	time.Sleep(time.Millisecond) // the next read is stamped after end
	go func() { _, _ = h.clientApp.Write([]byte("query")) }()
	await(t, inHand, "the TLS layer to hold the later bytes")
	if !w.Settled("", "", start, end) {
		t.Errorf("Settled: false for a window before the read the TLS layer holds: %v", w.HeldBy("", "", start, end, 0))
	}
	close(release)
	c := nextChunk(t, chunks)
	if !c.ReadAt.After(end) {
		t.Fatalf("chunk stamped %v, not after the window's end %v", c.ReadAt, end)
	}
}

// A client-side upgrade whose handshake read bytes and then failed drops the
// layer it started, and what that layer read: nothing it read will reach the
// parser, so it holds no window.
func TestWatermark_AFailedHandshakeLeavesNothingHeld(t *testing.T) {
	handshook := make(chan struct{})
	var h *relayHarness
	h = newHarness(t, Config{TLSUpgradeFn: func(_ context.Context, conn net.Conn, isClient bool, _ *tls.Config) (net.Conn, error) {
		if isClient {
			return conn, nil
		}
		// The forwarders are parked by now, so the handshake reads these.
		go func() { _, _ = h.clientApp.Write([]byte("hello")) }()
		buf := make([]byte, 16)
		_, err := conn.Read(buf)
		close(handshook)
		if err != nil {
			return nil, err
		}
		return nil, errors.New("bad handshake")
	}})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	start := time.Now()
	h.r.Directives() <- directive.UpgradeTLS(nil, &tls.Config{}, "client-side upgrade")
	await(t, handshook, "the handshake to read")
	select {
	case ack := <-h.r.Acks():
		if ack.OK {
			t.Fatal("the upgrade succeeded; the row needs it to fail")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no ack for the TLS upgrade")
	}
	end := time.Now()
	if !w.Settled("", "", start, end) {
		t.Errorf("Settled: false over a failed handshake's read: %v", w.HeldBy("", "", start, end, 0))
	}
}

// noCloseConn is a layer whose Close leaves the conn under it open.
type noCloseConn struct{ net.Conn }

func (noCloseConn) Close() error { return nil }

// An upgrade of both sides: the destination's handshake read the server's
// bytes and succeeded, then the client's read the client's and failed. The
// upgrade drops both layers it started, so neither holds a window over what
// its handshake read. (The destination's layer is closed without closing the
// conn under it, so its forwarder goes on reading the plain conn and does not
// stop: the failed upgrade alone clears the marks.)
func TestWatermark_AFailedUpgradeLeavesNeitherSidesLayerHeld(t *testing.T) {
	var h *relayHarness
	h = newHarness(t, Config{TLSUpgradeFn: func(_ context.Context, conn net.Conn, isClient bool, _ *tls.Config) (net.Conn, error) {
		// The forwarders are parked by now, so each handshake reads these.
		peer, hello := h.clientApp, "hello"
		if isClient {
			peer, hello = h.destSvc, "server-hello"
		}
		go func() { _, _ = peer.Write([]byte(hello)) }()
		buf := make([]byte, 16)
		if _, err := conn.Read(buf); err != nil {
			return nil, err
		}
		if isClient {
			return noCloseConn{conn}, nil
		}
		return nil, errors.New("bad handshake")
	}})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	start := time.Now()
	h.r.Directives() <- directive.UpgradeTLS(&tls.Config{}, &tls.Config{}, "upgrade of both sides")
	select {
	case ack := <-h.r.Acks():
		if ack.OK {
			t.Fatal("the upgrade succeeded; the row needs it to fail")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no ack for the TLS upgrade")
	}
	end := time.Now()
	if !w.Settled("", "", start, end) {
		t.Errorf("Settled: false over a failed upgrade's handshake reads: %v", w.HeldBy("", "", start, end, 0))
	}
}

// The client's TLS layer handed one record of a read on, and what it still
// holds from that read ends the stream (the client's close_notify after the
// record), so the forwarder stops at EOF while the relay goes on carrying the
// server's side. Nothing the layer still holds will reach the parser, so once
// the forwarder has stopped it holds no window.
func TestWatermark_AStoppedForwarderLeavesNothingItsTLSLayerHeld(t *testing.T) {
	h := newHalfCloseHarness(t, Config{
		HalfCloseGrace: time.Minute, // the relay stays up while the row looks
		TLSUpgradeFn: func(_ context.Context, conn net.Conn, isClient bool, _ *tls.Config) (net.Conn, error) {
			if isClient {
				return conn, nil
			}
			return &layerConn{Conn: conn, piece: 4, failHand: 2, failWith: io.EOF}, nil
		},
	})
	h.r.Directives() <- directive.UpgradeTLS(nil, &tls.Config{}, "client-side upgrade")
	select {
	case ack := <-h.r.Acks():
		if !ack.OK {
			t.Fatalf("TLS upgrade failed: %+v", ack)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no ack for the TLS upgrade")
	}
	finPassed := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, h.destSvc); close(finPassed) }()
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	chunks := chunksOf(h.r.ClientStream())
	_ = chunksOf(h.r.DestStream())
	awaitParserWaiting(t, h.r.ClientStream())

	start := time.Now()
	go func() { _, _ = h.clientApp.Write([]byte("AAAABBBB")) }()
	if c := nextChunk(t, chunks); string(c.Bytes) != "AAAA" {
		t.Fatalf("chunk = %q, want AAAA", c.Bytes)
	}
	// The relay passes the client's end on to the server once the
	// forwarder has stopped.
	await(t, finPassed, "the client's end to reach the server")
	awaitParserWaiting(t, h.r.DestStream())
	end := time.Now()
	if !w.Settled("", "", start, end) {
		t.Errorf("Settled: false once the forwarder stopped, over bytes its TLS layer held: %v", w.HeldBy("", "", start, end, 0))
	}
}
