package relay

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.uber.org/zap"
)

// awaitState polls cond until it holds: it waits only for a goroutine the test
// started to reach a state (a parser blocked in a read), and no assertion
// depends on how long that takes. The bound only ends a test that is broken.
func awaitState(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// readChunk reads one chunk off a parser-facing stream, as a parser would.
func readChunk(t *testing.T, fc *fakeconn.FakeConn) fakeconn.Chunk {
	t.Helper()
	c, err := fc.ReadChunk()
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	return c
}

// blockInRead starts a reader on fc that blocks for the next chunk (a parser
// that has taken everything and waits for more), and waits until it does.
func blockInRead(t *testing.T, fc *fakeconn.FakeConn) {
	t.Helper()
	go func() { _, _ = fc.ReadChunk() }()
	awaitState(t, "the parser to block for more", func() bool { w, _ := fc.Waiting(); return w })
}

// A connection carried a request its parser has not got to (it is behind the
// traffic): the watermark holds every test case over it and says a stop may
// still get something from before it. Once the parser has taken the bytes and
// waits for more, it is done with them.
func TestWatermark_HoldsWhatAParserHasNotGotTo(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	untrack := w.Track(h.r, "conn-1")
	defer untrack()

	before := time.Now()
	h.writeClient([]byte("GET / HTTP/1.1\r\n\r\n"))
	h.readDest(18) // forwarded: the app's traffic does not wait on the parser
	after := time.Now()

	if !w.PendingBefore(after) {
		t.Fatal("PendingBefore: false while the parser had not read a request the connection carried before the time asked about")
	}
	if w.Settled("", "", before, after) {
		t.Fatal("Settled: true for a test case over a request its connection's parser had not got to")
	}
	if b := w.HeldBy("", "", before, after, 0); b == nil {
		t.Fatal("HeldBy: nil while the watermark held the test case")
	}
	// A time before the connection carried anything is behind no parser.
	if w.PendingBefore(before.Add(-time.Second)) {
		t.Fatal("PendingBefore: true for a time before the connection carried anything")
	}

	readChunk(t, h.r.ClientStream())
	blockInRead(t, h.r.ClientStream())
	if w.PendingBefore(after) {
		t.Fatalf("PendingBefore: true once the parser had taken everything and waited for more: %v", w.PendingOn(after))
	}
	if !w.Settled("", "", before, after) {
		t.Fatalf("Settled: false once the parser was done: %v", w.HeldBy("", "", before, after, 0))
	}
}

// A parser that has taken a chunk from after a time is done with what came
// before it, though newer bytes wait for it.
func TestWatermark_AParserPastATimeIsDoneWithIt(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	h.writeClient([]byte("one"))
	h.readDest(3)
	mid := time.Now()
	time.Sleep(2 * time.Millisecond) // the next chunk is stamped after mid
	h.writeClient([]byte("two"))
	h.readDest(3)
	h.writeClient([]byte("three"))
	h.readDest(5)

	if !w.PendingBefore(mid) {
		t.Fatal("PendingBefore(mid): false with the chunk from before mid unread")
	}
	readChunk(t, h.r.ClientStream()) // "one"
	// "one" is consumed, but the parser has neither taken "two" nor waits:
	// it is still working on "one", from before mid.
	if !w.PendingBefore(mid) {
		t.Fatal("PendingBefore(mid): false while the parser was still working on the chunk from before mid")
	}
	c := readChunk(t, h.r.ClientStream()) // "two", stamped after mid
	if !c.ReadAt.After(mid) {
		t.Fatalf("test setup: the second chunk was stamped %v, not after %v", c.ReadAt, mid)
	}
	if w.PendingBefore(mid) {
		t.Fatalf("PendingBefore(mid): true once the parser took a chunk from after it: %v", w.PendingOn(mid))
	}
	if !w.PendingBefore(time.Now()) {
		t.Fatal("PendingBefore(now): false with \"three\" unread")
	}
}

// A connection that carried nothing within a test case's window does not hold
// it back, whatever its parser is doing.
func TestWatermark_AConnectionIdleOverAWindowDoesNotHoldIt(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	h.writeClient([]byte("early"))
	h.readDest(5) // never read by the parser
	// The forwarder marked "early" held before its Write, so once the mark
	// is clear it has let the chunk go to the parser's queue: this row is
	// about an idle connection, not a chunk still in the forwarder's hand
	// (TestWatermark_AChunkInTheForwardersHandDoesNotHoldALaterWindow's).
	awaitState(t, "the forwarder to let go of the chunk", func() bool {
		return h.r.teeC2D.progress.held.Load() == 0
	})
	// The window starts after the chunk's stamp, which the forwarder took
	// before its Write.
	start := time.Now()
	for start.UnixNano() <= h.r.teeC2D.progress.last.Load() {
		start = time.Now()
	}
	end := start.Add(time.Millisecond)
	if !w.Settled("", "", start, end) {
		t.Fatalf("Settled: false for a window the connection carried nothing in: %v", w.HeldBy("", "", start, end, 0))
	}
}

// A chunk still in the forwarder's hand (its Write to a slow reader blocks,
// or the forwarder is descheduled after it) does not hold a window that
// started after the chunk's stamp: the connection carried nothing in that
// window. A window the stamp falls in is still held (the row after this one).
func TestWatermark_AChunkInTheForwardersHandDoesNotHoldALaterWindow(t *testing.T) {
	release := make(chan struct{})
	var holding atomic.Bool
	cfg := Config{beforeTee: func(dir fakeconn.Direction) {
		if dir == fakeconn.FromDest {
			holding.Store(true)
			<-release
		}
	}}
	h := newHarness(t, cfg)
	defer close(release)
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	h.writeDest([]byte("answer"))
	h.readClient(6)
	awaitState(t, "the forwarder to hold the chunk", holding.Load)
	start := time.Now()
	for start.UnixNano() <= h.r.teeD2C.progress.last.Load() {
		start = time.Now()
	}
	end := start.Add(time.Millisecond)
	if !w.Settled("", "", start, end) {
		t.Fatalf("Settled: false for a window after the stamp of the chunk in the forwarder's hand: %v", w.HeldBy("", "", start, end, 0))
	}
	if w.Settled("", "", start.Add(-time.Second), end) {
		t.Fatal("Settled: true for a window the chunk in the forwarder's hand was stamped in")
	}
}

// The TLS twin of the row above: the destination's TLS layer handed a reply
// on, and the forwarder's Write of it to a slow client blocks. The layer keeps
// the mark of the read under the reply all that while (it reads again only on
// the forwarder's next Read), with the same stamp as the forwarder's hand. That
// mark counts by its stamp too: it holds a window the stamp falls in, and no
// window that started after it.
func TestWatermark_AReplyATLSForwardersWriteBlocksOnDoesNotHoldALaterWindow(t *testing.T) {
	h := upgradedSideUndrained(t, true, func(c net.Conn) net.Conn { return &layerConn{Conn: c} })
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	go func() { _, _ = h.destSvc.Write([]byte("answer")) }()
	d2c := &h.r.teeD2C.progress
	awaitState(t, "the forwarder to hold the reply its layer handed on", func() bool {
		return d2c.held.Load() > 0 && d2c.layer.Load() == d2c.held.Load()
	})
	start := time.Now()
	for start.UnixNano() <= d2c.last.Load() {
		start = time.Now()
	}
	end := start.Add(time.Millisecond)
	if !w.Settled("", "", start, end) {
		t.Errorf("Settled: false for a window after the stamp of a reply a TLS forwarder's Write blocks on: %v", w.HeldBy("", "", start, end, 0))
	}
	if w.Settled("", "", start.Add(-time.Second), end) {
		t.Error("Settled: true for a window a reply a TLS forwarder's Write blocks on was stamped in")
	}
	if d2c.held.Load() == 0 || d2c.layer.Load() == 0 {
		t.Fatal("the forwarder's Write did not block for the whole look: the row did not test what it says")
	}
	if got := string(h.readClient(6)); got != "answer" {
		t.Fatalf("client read %q, want answer", got)
	}
}

// unrunRelay is a relay made and never run: no forwarder exists, so progress a
// test sets by hand stays as it was set.
func unrunRelay(t *testing.T) *Relay {
	t.Helper()
	c1, c2 := net.Pipe()
	d1, d2 := net.Pipe()
	r := New(Config{Logger: zap.NewNop()}, c1, connseq.NewUpstream(d1))
	t.Cleanup(func() {
		r.teeC2D.close()
		r.teeD2C.close()
		r.teeC2D.waitDone()
		r.teeD2C.waitDone()
		for _, c := range []net.Conn{c1, c2, d1, d2} {
			_ = c.Close()
		}
	})
	return r
}

// A chunk the forwarder holds counts by its stamp, and one it is still
// stamping (heldUnstamped) can be stamped within any window from now on, so it
// holds them all, whatever the other direction carried. A held chunk's stamp
// counts before its direction's first stamp is published (noteCarried stores
// held, then first): a direction's first chunk, stamped in the window and
// still in its forwarder's hand, holds the window even when the other
// direction's first chunk came after it. HeldProgress's view (stampsOnly)
// passes over a chunk not stamped yet and answers by the stamps alone. The
// progress is set by hand on a relay that is never run, so no forwarder
// touches it.
func TestWatermark_AChunkBeingStampedHoldsEveryWindow(t *testing.T) {
	t.Parallel()
	start := time.Now().UnixNano()
	end := start + int64(time.Second)
	old, late := start-int64(time.Hour), end+int64(time.Second)
	type dirProgress struct{ first, last, held int64 }
	// carriedNothingIn is the verdict's answer (unstampedInAnyWindow);
	// stampsOnly is HeldProgress's, which passes over a chunk not stamped yet.
	for _, row := range []struct {
		name                         string
		client, dest                 dirProgress
		carriedNothingIn, stampsOnly bool
	}{
		{"idle since before the window", dirProgress{}, dirProgress{old, old, 0}, true, true},
		{"a held chunk stamped before the window", dirProgress{}, dirProgress{old, old, old + 1}, true, true},
		{"a held chunk not stamped yet", dirProgress{}, dirProgress{old, old, heldUnstamped}, false, true},
		{"a held chunk stamped at the window's start", dirProgress{}, dirProgress{old, old, start}, false, false},
		{"a direction's first chunk held, stamped in the window, the other's first after it",
			dirProgress{late, late, 0}, dirProgress{0, 0, start}, false, false},
		{"a direction's first chunk held, stamped after the window, the other's first after it",
			dirProgress{late, late, 0}, dirProgress{0, 0, late + 1}, true, true},
		{"the connection's first chunk held, stamped in the window", dirProgress{}, dirProgress{0, 0, start}, false, false},
		{"the connection's first chunk held, stamped before the window", dirProgress{}, dirProgress{0, 0, old}, true, true},
		{"the connection's first chunk not stamped yet", dirProgress{}, dirProgress{0, 0, heldUnstamped}, false, true},
		{"a direction's first chunk not stamped yet, the other's first, held, after the window",
			dirProgress{0, 0, late}, dirProgress{0, 0, heldUnstamped}, false, true},
		{"a direction's first chunk not stamped yet, the other's first after the window",
			dirProgress{late, late, 0}, dirProgress{0, 0, heldUnstamped}, false, true},
		{"a direction's chunk not stamped yet, the other's held chunk stamped in the window",
			dirProgress{old, old, start}, dirProgress{old, old, heldUnstamped}, false, false},
	} {
		r := unrunRelay(t)
		for _, d := range []struct {
			p *teeProgress
			v dirProgress
		}{{&r.teeC2D.progress, row.client}, {&r.teeD2C.progress, row.dest}} {
			d.p.first.Store(d.v.first)
			d.p.last.Store(d.v.last)
			d.p.held.Store(d.v.held)
		}
		if got := r.carriedNothingIn(start, end, unstampedInAnyWindow); got != row.carriedNothingIn {
			t.Errorf("%s: carriedNothingIn = %v, want %v", row.name, got, row.carriedNothingIn)
		}
		if got := r.carriedNothingIn(start, end, stampsOnly); got != row.stampsOnly {
			t.Errorf("%s: carriedNothingIn, stamps only = %v, want %v", row.name, got, row.stampsOnly)
		}
	}
}

// Bytes in the hand of the TLS layer a forwarder reads through count as a
// chunk in the forwarder's hand does: by the stamp of the read that brought
// them (layer), which the record the layer hands on takes, toward the
// connection's first stamp as well as its newest; a read the layer is still
// stamping (heldUnstamped) is taken as view says. A TLS forwarder blocked in
// its Write holds the record and the layer the read under it, both with one
// stamp, and so holds no window after it. Progress set by hand on a relay
// that is never run.
func TestWatermark_BytesInATLSLayersHandCountByTheirStamp(t *testing.T) {
	t.Parallel()
	start := time.Now().UnixNano()
	end := start + int64(time.Second)
	old, late := start-int64(time.Hour), end+int64(time.Second)
	type dirProgress struct{ first, last, held, layer int64 }
	for _, row := range []struct {
		name                         string
		client, dest                 dirProgress
		carriedNothingIn, stampsOnly bool
	}{
		{"the layer holds bytes read before the window", dirProgress{}, dirProgress{old, old, 0, old + 1}, true, true},
		{"a TLS forwarder blocked in its Write since before the window", dirProgress{}, dirProgress{old, old + 1, old + 1, old + 1}, true, true},
		{"the layer holds bytes read at the window's start", dirProgress{}, dirProgress{old, old, 0, start}, false, false},
		{"a TLS forwarder blocked in its Write on a reply read in the window", dirProgress{}, dirProgress{old, start, start, start}, false, false},
		{"the layer is stamping a read", dirProgress{}, dirProgress{old, old, 0, heldUnstamped}, false, true},
		{"the layer holds a direction's first bytes, read in the window, the other's first after it",
			dirProgress{late, late, 0, 0}, dirProgress{0, 0, 0, start}, false, false},
		{"the layer holds the connection's first bytes, read after the window", dirProgress{}, dirProgress{0, 0, 0, late}, true, true},
		{"the layer is stamping a read, the other's held chunk stamped in the window",
			dirProgress{old, old, start, 0}, dirProgress{old, old, 0, heldUnstamped}, false, false},
	} {
		r := unrunRelay(t)
		for _, d := range []struct {
			p *teeProgress
			v dirProgress
		}{{&r.teeC2D.progress, row.client}, {&r.teeD2C.progress, row.dest}} {
			d.p.first.Store(d.v.first)
			d.p.last.Store(d.v.last)
			d.p.held.Store(d.v.held)
			d.p.layer.Store(d.v.layer)
		}
		if got := r.carriedNothingIn(start, end, unstampedInAnyWindow); got != row.carriedNothingIn {
			t.Errorf("%s: carriedNothingIn = %v, want %v", row.name, got, row.carriedNothingIn)
		}
		if got := r.carriedNothingIn(start, end, stampsOnly); got != row.stampsOnly {
			t.Errorf("%s: carriedNothingIn, stamps only = %v, want %v", row.name, got, row.stampsOnly)
		}
	}
}

// A chunk the forwarder has read and forwarded, and not yet queued for its
// parser, holds the watermark: a test case that used the bytes completed after
// the forwarder wrote them on, and is held until the parser has them.
func TestWatermark_HoldsAChunkInTheForwardersHand(t *testing.T) {
	release := make(chan struct{})
	var holding atomic.Bool
	cfg := Config{beforeTee: func(dir fakeconn.Direction) {
		if dir == fakeconn.FromDest {
			holding.Store(true)
			<-release
		}
	}}
	h := newHarness(t, cfg)
	defer close(release)
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	h.writeDest([]byte("answer"))
	h.readClient(6) // written on: the app has it
	awaitState(t, "the forwarder to hold the chunk", holding.Load)
	got := time.Now()
	if !w.PendingBefore(got) {
		t.Fatal("PendingBefore: false while the forwarder still held a chunk it had written on")
	}
	if w.Settled("", "", got.Add(-time.Second), got) {
		t.Fatal("Settled: true for a test case over a chunk still in the forwarder's hand")
	}
}

// A chunk the forwarder has read and stamped stays in its hand until it is
// queued. The forwarder lets go of it (noteReleased) at the top of its next
// loop, after push has queued it. Here the forwarder is held just before the
// push. Nothing is queued. The parser waits on its stream. Only the
// forwarder's hand says the parser has not been given the chunk. A window the
// chunk's stamp falls in is not settled. Were the hand let go of before the
// push, the chunk would be in neither the hand nor the queue for a moment. A
// look then would take the waiting parser as caught up. It would pass the
// window with the chunk still to come (#4900's missing mock).
func TestWatermark_AChunkJustBeforeItsQueueHoldsItsWindowWhileTheParserWaits(t *testing.T) {
	release := make(chan struct{})
	var holding atomic.Bool
	h := newHarness(t, Config{beforeTee: func(dir fakeconn.Direction) {
		if dir == fakeconn.FromClient {
			holding.Store(true)
			<-release
		}
	}})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	start := time.Now()
	h.writeClient([]byte("req"))
	h.readDest(3)
	awaitState(t, "the forwarder to hold the chunk before its queue", holding.Load)
	got := make(chan string, 1)
	go func() {
		c, err := h.r.ClientStream().ReadChunk()
		if err != nil {
			got <- "error: " + err.Error()
			return
		}
		got <- string(c.Bytes)
	}()
	awaitState(t, "the parser to wait on its stream", func() bool { ok, _ := h.r.ClientStream().Waiting(); return ok })
	end := time.Now()
	if w.Settled("", "", start, end) {
		t.Error("Settled: true for a window holding the stamp of a chunk not yet queued, the parser waiting")
	}
	close(release)
	released = true
	select {
	case s := <-got:
		if s != "req" {
			t.Fatalf("the parser got %q, want req", s)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the parser never got the chunk")
	}
}

// A forwarder marks a chunk it read as in its hand, being stamped
// (heldUnstamped), before it reads the clock for the chunk's stamp, so a look
// that found its hand empty is older than the stamp. Were the clock read
// first, a look in between would find nothing in hand and pass a window that
// ends then, and the chunk would come after it, stamped inside it (#4900).
// Both directions' forwarders stamp a chunk the same way, and a socket that
// reports no read time of its own leaves the chunk the clock's time.
func TestWatermark_AForwarderMarksAChunkBeforeItReadsTheClock(t *testing.T) {
	for _, dir := range []fakeconn.Direction{fakeconn.FromClient, fakeconn.FromDest} {
		t.Run(dir.String(), func(t *testing.T) {
			var reading atomic.Pointer[tee] // the forwarder's tee, once the relay exists
			stamp := time.Unix(0, 1_700_000_000_000_000_000)
			var mu sync.Mutex
			var atClock []int64
			h := newHarness(t, Config{readClock: func() time.Time {
				mu.Lock()
				defer mu.Unlock()
				atClock = append(atClock, reading.Load().progress.held.Load())
				return stamp
			}})
			var stream *fakeconn.FakeConn
			if dir == fakeconn.FromClient {
				reading.Store(h.r.teeC2D)
				stream = h.r.ClientStream()
				h.writeClient([]byte("ping"))
				h.readDest(4)
			} else {
				reading.Store(h.r.teeD2C)
				stream = h.r.DestStream()
				h.writeDest([]byte("pong"))
				h.readClient(4)
			}

			// Written on, so stamped: the clock was read once, for this chunk.
			mu.Lock()
			got := append([]int64(nil), atClock...)
			mu.Unlock()
			if len(got) != 1 || got[0] != heldUnstamped {
				t.Errorf("the forwarder's hand when the chunk's stamp was taken from the clock = %v, want [%d] (heldUnstamped)", got, heldUnstamped)
			}
			if c := readChunk(t, stream); !c.ReadAt.Equal(stamp) {
				t.Errorf("the chunk's stamp = %v, want the clock's %v", c.ReadAt, stamp)
			}
		})
	}
}

// A parser that returned is no longer followed, and its progress is counted.
func TestWatermark_UntrackAndProgress(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	untrack := w.Track(h.r, "conn-1")
	h.writeClient([]byte("abc"))
	h.readDest(3)
	p0 := w.Progress()
	readChunk(t, h.r.ClientStream())
	p1 := w.Progress()
	if p1 == p0 {
		t.Fatal("Progress did not move as the parser consumed bytes")
	}
	h.writeClient([]byte("unread"))
	h.readDest(6)
	if !w.PendingBefore(time.Now()) {
		t.Fatal("PendingBefore: false with bytes unread")
	}
	untrack()
	untrack() // idempotent
	if w.Following() != 0 {
		t.Fatalf("Following: %d after untrack", w.Following())
	}
	if w.PendingBefore(time.Now()) {
		t.Fatal("PendingBefore: true for a connection whose parser returned")
	}
	if p2 := w.Progress(); p2 <= p1 {
		t.Fatalf("Progress went from %d to %d as the parser returned; it must move", p1, p2)
	}
}

// ReaderLag delivers each chunk to the parser no earlier than the lag after it
// was read, and the watermark holds what is lagging.
func TestWatermark_ReaderLagHoldsTheParserBehind(t *testing.T) {
	const lag = 300 * time.Millisecond
	h := newHarness(t, Config{ReaderLag: lag})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	h.writeClient([]byte("late"))
	h.readDest(4) // the application is not delayed
	if !w.PendingBefore(time.Now()) {
		t.Fatal("PendingBefore: false while the chunk lagged on its way to the parser")
	}
	c := readChunk(t, h.r.ClientStream())
	if d := time.Since(c.ReadAt); d < lag {
		t.Fatalf("the chunk reached the parser %v after it was read, want at least %v", d, lag)
	}
}

// A response chunk from before mid that the drain has taken off the queue
// (delivered) but the parser has not consumed, while the parser has already
// taken a later client chunk: only the delivered list sees the response, and
// without it the watermark says the parser is past mid.
func TestWatermark_ADeliveredChunkOnTheOtherDirectionHoldsIt(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	h.writeDest([]byte("resp"))
	h.readClient(4)
	mid := time.Now()
	time.Sleep(2 * time.Millisecond) // the next chunk is stamped after mid
	h.writeClient([]byte("next"))
	h.readDest(4)
	// The drain hands the response to the parser's stream at once (it is
	// delivered, not queued); the parser reads only the client direction.
	awaitState(t, "the response to leave the tee's queue", func() bool {
		h.r.teeD2C.mu.Lock()
		defer h.r.teeD2C.mu.Unlock()
		return len(h.r.teeD2C.q) == 0
	})
	c := readChunk(t, h.r.ClientStream())
	if !c.ReadAt.After(mid) {
		t.Fatalf("setup: client chunk stamped %v, not after %v", c.ReadAt, mid)
	}
	if !w.PendingBefore(mid) {
		t.Fatal("PendingBefore(mid) = false with a response from before mid delivered but never consumed")
	}
	if w.Settled("", "", mid.Add(-time.Second), mid) {
		t.Fatal("Settled = true for a window whose response the parser never consumed")
	}
}

// A connection's newest stamp is the newer of its two directions'. Here the
// client's request is stamped before the window. The parser has taken it and
// waits for more. The destination's reply is stamped inside the window. It
// waits for a parser that has not read it. Only the reply holds the window,
// and it is not settled. Taking the newest stamp from the client's side alone
// would put all the connection's traffic before the window. The window would
// then pass.
func TestWatermark_AReplyInsideAWindowAfterATakenRequestHoldsIt(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	h.writeClient([]byte("req"))
	h.readDest(3)
	if c := readChunk(t, h.r.ClientStream()); string(c.Bytes) != "req" {
		t.Fatalf("setup: the parser took %q, want req", c.Bytes)
	}
	blockInRead(t, h.r.ClientStream())
	start := time.Now()
	for start.UnixNano() <= h.r.teeC2D.progress.last.Load() {
		start = time.Now() // the window opens after the request's stamp
	}
	h.writeDest([]byte("ans"))
	h.readClient(3)
	awaitState(t, "the reply to leave the forwarder's hand", func() bool { return h.r.teeD2C.progress.held.Load() == 0 })
	end := time.Now()
	if h.r.teeD2C.progress.last.Load() < start.UnixNano() {
		t.Fatal("setup: the reply was stamped before the window")
	}
	if w.Settled("", "", start, end) {
		t.Error("Settled: true for a window holding a reply the parser has not taken")
	}
}

// A parser that took a request from before mid and now waits on the client
// while the response, from after mid, waits for it on the other direction has
// not consumed everything the connection carried: it may still be building
// the exchange it took before mid, so it is not done with mid.
func TestWatermark_AParserWaitingWithTheOtherDirectionUnreadIsNotDone(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	h.writeClient([]byte("req"))
	h.readDest(3)
	readChunk(t, h.r.ClientStream())
	mid := time.Now()
	time.Sleep(2 * time.Millisecond) // the response is stamped after mid
	h.writeDest([]byte("resp"))
	h.readClient(4)
	awaitState(t, "the response to reach the parser's stream", func() bool {
		return h.r.teeD2C.acceptedBytes() == 4
	})
	blockInRead(t, h.r.ClientStream())
	if !w.PendingBefore(mid) {
		t.Fatal("PendingBefore(mid) = false while the parser waits on the client with the response to the request it took before mid unread")
	}
}
