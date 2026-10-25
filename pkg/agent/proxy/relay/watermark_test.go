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

// parkedDestForwarder is a Config whose destination forwarder parks with the
// first chunk it has written on, before it queues the chunk for the parser
// (beforeTee): holding is closed as it parks there, and it goes on, with that
// chunk and every later one, once the test closes release.
func parkedDestForwarder() (cfg Config, holding <-chan struct{}, release chan struct{}) {
	parked := make(chan struct{})
	release = make(chan struct{})
	var once sync.Once
	cfg = Config{beforeTee: func(dir fakeconn.Direction) {
		if dir == fakeconn.FromDest {
			once.Do(func() { close(parked) })
			<-release
		}
	}}
	return cfg, parked, release
}

// A chunk the forwarder has read and forwarded, and not yet queued for its
// parser, holds the watermark: a test case that used the bytes completed after
// the forwarder wrote them on, and is held until the parser has them.
func TestWatermark_HoldsAChunkInTheForwardersHand(t *testing.T) {
	cfg, holding, release := parkedDestForwarder()
	h := newHarness(t, cfg)
	defer close(release)
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	h.writeDest([]byte("answer"))
	h.readClient(6) // written on: the app has it
	<-holding
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

// forwarderParked reports whether dir's forwarder has signalled that it parks
// on the relay's pause barrier in the current pause window
// (markForwarderParked): it is then past its stash and its release.
func forwarderParked(r *Relay, dir fakeconn.Direction) bool {
	r.pauseMu.Lock()
	parked := r.parkedC2D
	if dir == fakeconn.FromDest {
		parked = r.parkedD2C
	}
	r.pauseMu.Unlock()
	if parked == nil {
		return false
	}
	select {
	case <-parked:
		return true
	default:
		return false
	}
}

// A forwarder that reads a chunk while the relay is paused stashes it (and
// queues a client chunk for the parser too, in a pre-dispatch pause or under a
// client write hold), lets go of it, and only then parks until the pause ends.
// The chunk is then in neither forwarder's hand: the parser has taken what it
// was given and waits for more. A window over the chunk's stamp is settled while
// the forwarder stays parked. Were the hand let go of only at the top of the
// next loop, after the pause, the parked forwarder would keep the chunk's
// stamp in its hand. It would hold every window the stamp falls in for as long
// as the pause lasts: a late settle, or a stop that waits out its bound.
func TestWatermark_AForwarderParkedInAPauseHoldsNoWindowWithTheChunkItStashed(t *testing.T) {
	for _, dir := range []fakeconn.Direction{fakeconn.FromClient, fakeconn.FromDest} {
		t.Run(dir.String(), func(t *testing.T) {
			h := newHarness(t, Config{PreDispatchPause: true})
			w := NewWatermark()
			defer w.Track(h.r, "conn-1")()

			start := time.Now()
			if dir == fakeconn.FromClient {
				h.writeClient([]byte("first"))
				// Queued for the parser as well as stashed: it takes it.
				if c := readChunk(t, h.r.ClientStream()); string(c.Bytes) != "first" {
					t.Fatalf("the parser got %q, want first", c.Bytes)
				}
				blockInRead(t, h.r.ClientStream())
			} else {
				h.writeDest([]byte("greet"))
				// Stashed only: the parser is given nothing and waits, nor will
				// it be given it: ResumePreDispatch writes it to the client and
				// UpgradeTLS takes it for the handshake, and neither tees it.
				blockInRead(t, h.r.DestStream())
			}
			awaitState(t, "the forwarder to park on the pause", func() bool { return forwarderParked(h.r, dir) })
			end := time.Now()
			if by := w.HeldBy("", "", start, end, 0); by != nil {
				t.Errorf("a window over the chunk the parked forwarder stashed is held by %v, want settled", by)
			}
			if w.PendingBefore(end) {
				t.Error("PendingBefore: true while the only chunk is stashed and the forwarder that read it is parked")
			}
		})
	}
}

// A connection that has carried nothing is behind no time and holds no test
// case, even one whose window has no start, though no parser waits on it.
func TestWatermark_AConnectionThatCarriedNothingHoldsNothing(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	now := time.Now()
	if w.PendingBefore(now) {
		t.Fatalf("PendingBefore: true for a connection that has carried nothing: %v", w.PendingOn(now))
	}
	if !w.Settled("", "", time.Time{}, now) {
		t.Fatalf("Settled: false for a connection that has carried nothing: %v", w.HeldBy("", "", time.Time{}, now, 0))
	}
}

// A connection's newest chunk counts on either direction: a window that starts
// after every chunk the client sent and covers an answer the parser has not got
// to is held by that answer.
func TestWatermark_AnAnswerNewerThanEveryRequestHoldsAWindowOverIt(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	h.writeClient([]byte("req"))
	h.readDest(3)
	h.writeDest([]byte("resp"))
	h.readClient(4)
	h.forwardersBackInRead() // neither forwarder holds a chunk: the answer is queued
	sent, answered := h.r.teeC2D.progress.last.Load(), h.r.teeD2C.progress.last.Load()
	if answered <= sent {
		t.Fatalf("test setup: the answer was stamped %d, not after the request's %d", answered, sent)
	}
	start, end := time.Unix(0, sent+1), time.Unix(0, answered)
	if w.Settled("", "", start, end) {
		t.Fatal("Settled: true for a window over an answer its connection's parser had not got to")
	}
}

// A chunk in the forwarder's hand counts by its stamp. A forwarder that keeps
// one from before a test case's window (its Write blocked on a full peer) does
// not hold the test case, though the parser has not got the chunk; a window
// that covers the stamp is held by it.
func TestWatermark_AChunkInTheForwardersHandFromBeforeAWindowDoesNotHoldIt(t *testing.T) {
	cfg, holding, release := parkedDestForwarder()
	h := newHarness(t, cfg)
	defer close(release)
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	h.writeDest([]byte("answer"))
	h.readClient(6) // written on: the app has it
	<-holding
	stamp := h.r.teeD2C.progress.held.Load()
	if stamp <= 0 {
		t.Fatalf("test setup: the forwarder holds a chunk with stamp %d", stamp)
	}
	start := time.Unix(0, stamp+1)
	end := start.Add(time.Millisecond)
	if !w.Settled("", "", start, end) {
		t.Fatalf("Settled: false for a window after the only chunk the connection carried, still in the forwarder's hand: %v",
			w.HeldBy("", "", start, end, 0))
	}
	if w.Settled("", "", time.Unix(0, stamp), end) {
		t.Fatal("Settled: true for a window that covers the stamp of a chunk still in the forwarder's hand")
	}
}

// parkedStampConn parks the forwarder that reads it as it stamps a chunk: the
// relay asks the conn it read from for the read's time (observedReadAt) after
// it took the chunk in hand and before it stamps it (heldUnstamped). stamping
// gets a value as the forwarder parks there; it goes on, the chunk stamped
// now, once the test closes release.
type parkedStampConn struct {
	net.Conn
	stamping chan struct{}
	release  chan struct{}
}

func (c parkedStampConn) LastReadTime() time.Time {
	c.stamping <- struct{}{}
	<-c.release
	return time.Time{}
}

// A chunk in a forwarder's hand that it has not stamped yet holds every window,
// even one from before anything the connection carried: its stamp is not known
// yet. The client forwarder is parked there through the conn it read the chunk
// from.
func TestWatermark_AnUnstampedChunkInTheForwardersHandHoldsEveryWindow(t *testing.T) {
	src := parkedStampConn{stamping: make(chan struct{}, 1), release: make(chan struct{})}
	h := newHarnessOver(t, Config{}, func(c net.Conn) net.Conn { src.Conn = c; return src })
	t.Cleanup(func() { close(src.release) }) // before the harness's: Run waits for the forwarder
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	start := time.Now().Add(-time.Hour)
	end := start.Add(time.Millisecond)
	go func() { _, _ = h.clientApp.Write([]byte("REQ1")) }()
	<-src.stamping
	if got := h.r.teeC2D.progress.held.Load(); got != heldUnstamped {
		t.Fatalf("test setup: held = %d with the forwarder parked as it stamps, want heldUnstamped", got)
	}
	if w.Settled("", "", start, end) {
		t.Fatal("Settled: true for a window while a chunk not yet stamped is in the forwarder's hand")
	}
}

// forwardersBackInRead returns once both forwarders are back in a Read, so
// neither still holds a chunk it queued: an empty write on a net.Pipe returns
// only once the other end's Read took it, a forwarder reads again only after
// it let go of its last chunk, and an empty read moves nothing.
func (h *relayHarness) forwardersBackInRead() {
	h.t.Helper()
	for _, c := range []net.Conn{h.clientApp, h.destSvc} {
		if _, err := c.Write(nil); err != nil {
			h.t.Fatalf("empty write: %v", err)
		}
	}
}

// A parser that reads ahead of itself (fakeconn.FakeConn.ReadAhead) is done
// with what its reader took only once its loop has marked it processed: a
// reader that took everything and waits for more says nothing of the loop.
// The loop working through what was read ahead is progress. The forwarder
// lets go of the chunk once it has queued it, so a reader that took it is all
// that holds it, even while the forwarder is still busy with it after that.
// That holds on each of the forwarder's three ways to queue a client chunk:
// forwarded, held from the destination, and stashed in a pre-dispatch pause.
func TestWatermark_AParserReadingAheadIsDoneOnlyWithWhatItProcessed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cfg       Config
		forwarded bool // the chunk reaches the destination before it is queued
	}{
		{"forwarded", Config{}, true},
		{"held", Config{HoldClientWrites: true}, false},
		{"pre-dispatch", Config{PreDispatchPause: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var readerDone chan struct{}
			t.Cleanup(func() { // after the harness's: the streams' close ends the read
				if readerDone != nil {
					<-readerDone
				}
			})
			teed, resume := make(chan struct{}, 1), make(chan struct{})
			cfg := tc.cfg
			cfg.OnClientChunkTeed = func() {
				teed <- struct{}{}
				<-resume
			}
			h := newHarness(t, cfg)
			t.Cleanup(func() { close(resume) }) // before the harness's: its Run waits for the forwarder
			w := NewWatermark()
			defer w.Track(h.r, "conn-1")()
			client := h.r.ClientStream()

			h.writeClient([]byte("GET / HTTP/1.1\r\n\r\n"))
			if tc.forwarded {
				h.readDest(18)
			}
			<-teed // the forwarder queued the chunk, and stays busy after it
			after := time.Now()
			c, err := client.ReadAhead()
			if err != nil {
				t.Fatalf("ReadAhead: %v", err)
			}
			// Told of the reader's next wait, not of one the read above made.
			blocked := make(chan struct{}, 1)
			defer fakeconn.BeforeWaitForTest(func(f *fakeconn.FakeConn) {
				if f == client {
					select {
					case blocked <- struct{}{}:
					default:
					}
				}
			})()
			readerDone = make(chan struct{})
			go func() {
				defer close(readerDone)
				_, _ = client.ReadAhead()
			}()
			<-blocked // the reader took everything and waits for more

			if !w.PendingBefore(after) {
				t.Fatal("PendingBefore: false while the parser's loop had not processed the request its reader took")
			}
			p0 := w.Progress()
			client.MarkProcessed(c)
			if p1 := w.Progress(); p1 <= p0 {
				t.Fatalf("Progress went from %d to %d as the loop processed what its reader took; it must move", p0, p1)
			}
			if w.PendingBefore(after) {
				t.Fatalf("PendingBefore: true once the loop processed all its reader took and the reader waited for more: %v",
					w.PendingOn(after))
			}
		})
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

// A parser that reads ahead takes its two streams' chunks in no order of their
// stamps: past a time on one stream, with a chunk from before it read ahead
// and not processed on the other, it is not done with that time.
func TestWatermark_AParserReadingAheadPastATimeOnOneStreamIsNotDoneWithTheOther(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	client, dest := h.r.ClientStream(), h.r.DestStream()
	readAhead := func(s *fakeconn.FakeConn) fakeconn.Chunk {
		t.Helper()
		c, err := s.ReadAhead()
		if err != nil {
			t.Fatalf("ReadAhead: %v", err)
		}
		return c
	}

	h.writeClient([]byte("REQ1"))
	h.readDest(4)
	client.MarkProcessed(readAhead(client))
	h.writeDest([]byte("RES1"))
	h.readClient(4)
	res := readAhead(dest) // queued behind the next request, not processed
	h.writeClient([]byte("REQ2"))
	h.readDest(4)
	req := readAhead(client)
	h.forwardersBackInRead()
	if !req.ReadAt.After(res.ReadAt) {
		t.Fatalf("test setup: the second request was stamped %v, not after the answer's %v", req.ReadAt, res.ReadAt)
	}
	client.MarkProcessed(req)

	if !w.PendingBefore(res.ReadAt) {
		t.Fatal("PendingBefore: false while the answer from then was read ahead and not processed")
	}
	dest.MarkProcessed(res)
	if w.PendingBefore(res.ReadAt) {
		t.Fatalf("PendingBefore: true once the answer was processed: %v", w.PendingOn(res.ReadAt))
	}
}

// waitingReader starts a reader that reads ahead on s and returns once it
// waits for more with until true (nil: at its first wait); its read ends as the
// test ends, which closes s. It fails if the read returns without waiting, and
// waits for good if until never holds at a wait (the tee never delivers).
func waitingReader(t *testing.T, s *fakeconn.FakeConn, until func() bool) {
	t.Helper()
	blocked := make(chan struct{}, 1)
	defer fakeconn.BeforeWaitForTest(func(f *fakeconn.FakeConn) {
		if f == s && (until == nil || until()) {
			select {
			case blocked <- struct{}{}:
			default:
			}
		}
	})()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.ReadAhead()
	}()
	t.Cleanup(func() { // before the harness's, which waits for nothing of it
		_ = s.Close()
		<-done
	})
	select {
	case <-blocked:
	case <-done:
		t.Fatal("test setup: the reader returned without waiting for more")
	}
}

// A parser that reads ahead is done with a time once it has processed every
// chunk from up to then, though it has a later chunk read ahead and queued and
// its loop has not got past the time on that stream: a loop that processed a
// request and waits on the destination's answer, with the next request queued
// behind it, is done with a time between the two requests.
func TestWatermark_AParserReadingAheadIsDoneWithATimeBeforeWhatItHasQueued(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	client, dest := h.r.ClientStream(), h.r.DestStream()

	h.writeClient([]byte("REQ1"))
	h.readDest(4)
	first, err := client.ReadAhead()
	if err != nil {
		t.Fatalf("ReadAhead: %v", err)
	}
	client.MarkProcessed(first)
	mid := time.Now()
	h.writeClient([]byte("REQ2"))
	h.readDest(4)
	queued, err := client.ReadAhead() // queued for the loop, not processed
	if err != nil {
		t.Fatalf("ReadAhead: %v", err)
	}
	if first.ReadAt.After(mid) || !queued.ReadAt.After(mid) {
		t.Fatalf("test setup: the requests were stamped %v and %v, not either side of %v", first.ReadAt, queued.ReadAt, mid)
	}
	waitingReader(t, client, nil)
	waitingReader(t, dest, nil) // the destination's answer has not come
	h.forwardersBackInRead()

	if w.PendingBefore(mid) {
		t.Fatalf("PendingBefore: true with every chunk from then processed: %v", w.PendingOn(mid))
	}
	if !w.PendingBefore(queued.ReadAt) {
		t.Fatal("PendingBefore: false while the request from then was read ahead and not processed")
	}
	client.MarkProcessed(queued)
	if w.PendingBefore(queued.ReadAt) {
		t.Fatalf("PendingBefore: true once the request was processed: %v", w.PendingOn(queued.ReadAt))
	}
}

// Bytes swallowed for a parser that reads ahead (DiscardBefore), behind a
// chunk it read ahead and has not processed, count processed before that
// chunk does, and ProcessedThrough cannot say exactly how far it is: the
// parser is not done with the chunk's time for them.
func TestWatermark_AParserReadingAheadIsNotDoneWithAChunkSwallowedBytesFollow(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	client := h.r.ClientStream()

	h.writeClient([]byte("REQ1"))
	h.readDest(4)
	h.writeClient([]byte("SKIP"))
	h.readDest(4)
	first, err := client.ReadAhead()
	if err != nil {
		t.Fatalf("ReadAhead: %v", err)
	}
	client.DiscardBefore(8)
	// Swallows "SKIP" once the tee has delivered it, then waits for more.
	waitingReader(t, client, func() bool { return client.Consumed() == 8 })
	h.forwardersBackInRead()
	if through, exact := client.ProcessedThrough(); exact {
		t.Fatalf("test setup: ProcessedThrough = %d, exact, past a discard behind a chunk read ahead", through)
	}

	if !w.PendingBefore(first.ReadAt) {
		t.Fatal("PendingBefore: false while the chunk from then was read ahead and not processed, behind swallowed bytes")
	}
	client.MarkProcessed(first)
	if w.PendingBefore(first.ReadAt) {
		t.Fatalf("PendingBefore: true once the chunk was processed: %v", w.PendingOn(first.ReadAt))
	}
}

// A parser that reads ahead, on a stream where ProcessedThrough cannot say
// exactly how far it is and never can again while it stays a chunk ahead of
// its loop, is done with a time once its loop has processed a chunk from after
// it on that stream, and not before.
func TestWatermark_AParserReadingAheadThatCannotSayExactlyIsDoneWithATimeItProcessedPast(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	client := h.r.ClientStream()
	readAhead := func() fakeconn.Chunk {
		t.Helper()
		c, err := client.ReadAhead()
		if err != nil {
			t.Fatalf("ReadAhead: %v", err)
		}
		return c
	}
	notExact := func(when string) {
		t.Helper()
		if through, exact := client.ProcessedThrough(); exact {
			t.Fatalf("test setup: ProcessedThrough = %d, exact, %s", through, when)
		}
	}

	for _, b := range []string{"REQ1", "SKIP", "REQ3"} {
		h.writeClient([]byte(b))
		h.readDest(4)
	}
	first := readAhead()
	client.DiscardBefore(8)
	third := readAhead() // swallows "SKIP" behind "REQ1", read ahead and not processed
	notExact("past a discard behind a chunk read ahead")
	if !third.ReadAt.After(first.ReadAt) {
		t.Fatalf("test setup: the third request was stamped %v, not after the first's %v", third.ReadAt, first.ReadAt)
	}
	if !w.PendingBefore(first.ReadAt) {
		t.Fatal("PendingBefore: false while the loop had processed nothing past the first request")
	}
	client.MarkProcessed(first)
	h.writeClient([]byte("REQ4"))
	h.readDest(4)
	fourth := readAhead() // the reader stays a chunk ahead of the loop
	client.MarkProcessed(third)
	h.forwardersBackInRead()
	notExact("with a chunk read ahead and not processed since the discard")

	if w.PendingBefore(first.ReadAt) {
		t.Fatalf("PendingBefore: true once the loop processed a chunk from after it: %v", w.PendingOn(first.ReadAt))
	}
	if !w.PendingBefore(fourth.ReadAt) {
		t.Fatal("PendingBefore: false while the request from then was read ahead and not processed")
	}
}

// notExactAhead writes three chunks on one direction of h (write and read
// carry them across), reads the first ahead on s, discards the second behind
// it and reads the third ahead: s then cannot say exactly how far its parser
// is, as in the test above, and its loop has processed nothing.
func notExactAhead(t *testing.T, s *fakeconn.FakeConn, write func([]byte), read func(int) []byte) (first, third fakeconn.Chunk) {
	t.Helper()
	for _, b := range []string{"MSG1", "SKIP", "MSG3"} {
		write([]byte(b))
		read(4)
	}
	var err error
	if first, err = s.ReadAhead(); err != nil {
		t.Fatalf("ReadAhead: %v", err)
	}
	s.DiscardBefore(8)
	if third, err = s.ReadAhead(); err != nil {
		t.Fatalf("ReadAhead: %v", err)
	}
	if through, exact := s.ProcessedThrough(); exact {
		t.Fatalf("test setup: ProcessedThrough = %d, exact, past a discard behind a chunk read ahead", through)
	}
	return first, third
}

// On a stream that cannot say exactly, a chunk stamped at t that is read ahead
// and not processed holds t, though the loop processed one stamped at t too:
// the loop is done with t only once it has processed a chunk from after t. The
// client's conn gives every read one stamp, as a TLS layer can.
func TestWatermark_ANotExactStreamWithAChunkStampedAtTheTimeUnprocessedHoldsIt(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	h := newHarnessOver(t, Config{}, func(c net.Conn) net.Conn { return fixedReadAtConn{Conn: c, readAt: at} })
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	client := h.r.ClientStream()
	first, third := notExactAhead(t, client, h.writeClient, h.readDest)
	if !first.ReadAt.Equal(at) || !third.ReadAt.Equal(at) {
		t.Fatalf("test setup: stamps %v and %v, want both %v", first.ReadAt, third.ReadAt, at)
	}
	client.MarkProcessed(first)
	waitingReader(t, client, nil) // and so caught up, but for the loop
	h.forwardersBackInRead()
	if !w.PendingBefore(at) {
		t.Fatal("PendingBefore: false with a chunk stamped at that time read ahead and not processed")
	}
}

// A stream that cannot say exactly is judged by the last chunk its own loop
// processed: the client's loop has processed nothing, and the other stream's
// loop processing an answer from after t says nothing of it.
func TestWatermark_ANotExactStreamIsJudgedByItsOwnLastProcessedChunk(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	client, dest := h.r.ClientStream(), h.r.DestStream()
	first, _ := notExactAhead(t, client, h.writeClient, h.readDest)
	h.writeDest([]byte("RES1"))
	h.readClient(4)
	if c := readChunk(t, dest); !c.ReadAt.After(first.ReadAt) {
		t.Fatalf("test setup: the answer was stamped %v, not after the request's %v", c.ReadAt, first.ReadAt)
	}
	waitingReader(t, client, nil)
	h.forwardersBackInRead()
	if !w.PendingBefore(first.ReadAt) {
		t.Fatal("PendingBefore: false while the client's loop had processed nothing, and the request from then was read ahead")
	}
}

// The destination's stream is judged as the client's is: one that cannot say
// exactly, with nothing from after t processed, holds t, though the client's
// stream is exact and past t.
func TestWatermark_ANotExactDestinationStreamHoldsWhatItsLoopHasNotProcessedPast(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	client, dest := h.r.ClientStream(), h.r.DestStream()
	first, third := notExactAhead(t, dest, h.writeDest, h.readClient)
	h.writeClient([]byte("REQ9"))
	h.readDest(4)
	if c := readChunk(t, client); !c.ReadAt.After(third.ReadAt) {
		t.Fatalf("test setup: the request was stamped %v, not after the answers' %v", c.ReadAt, third.ReadAt)
	}
	h.forwardersBackInRead()
	if !w.PendingBefore(first.ReadAt) {
		t.Fatal("PendingBefore: false while the destination's loop had processed nothing, and the answer from then was read ahead")
	}
	dest.MarkProcessed(first)
	dest.MarkProcessed(third)
	if w.PendingBefore(first.ReadAt) {
		t.Fatalf("PendingBefore: true once the destination's loop processed all it read ahead: %v", w.PendingOn(first.ReadAt))
	}
}

// A chunk delivered to a parser that reads synchronously, below the discard
// watermark (DiscardBefore, as for a TLS upgrade's handshake bytes), is never
// handed to the parser: it holds no time back while it waits to be swallowed.
func TestWatermark_ASynchronousParserIsDoneWithAChunkBelowTheDiscardWatermark(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	client, dest := h.r.ClientStream(), h.r.DestStream()

	h.writeClient([]byte("HELLO"))
	h.readDest(5)
	hello, err := client.Peek() // delivered to the FakeConn, not read
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	h.writeDest([]byte("X"))
	h.readClient(1)
	answer := readChunk(t, dest)
	if !answer.ReadAt.After(hello.ReadAt) {
		t.Fatalf("test setup: the answer was stamped %v, not after %v", answer.ReadAt, hello.ReadAt)
	}
	h.forwardersBackInRead()
	if !w.PendingBefore(hello.ReadAt) {
		t.Fatal("PendingBefore: false while the parser had not read the chunk from then")
	}
	client.DiscardBefore(5)
	if w.PendingBefore(hello.ReadAt) {
		t.Fatalf("PendingBefore: true for a chunk below the discard watermark: %v", w.PendingOn(hello.ReadAt))
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

// A chunk a parser read ahead and has not processed holds the windows its
// stamp is in, whichever place it waited in before the tee's queue: a
// destination chunk set aside for the client's answer after the client's FIN
// (tee.awaiting), or bytes a TLS layer read and had not handed on (layer). The
// chunk leaves that place before the parser has it, and the parser's reader
// taking it (Consumed) is not the parser being done with it (Processed): were
// the tee to forget a delivered chunk once it is handed on, the look would
// find it in no place and pass its window while the parser's loop has yet to
// emit what it completes.
func TestWatermark_AChunkReadAheadHoldsItsStampWhereverItWaitedBefore(t *testing.T) {
	stamp := time.Unix(100, 0)
	for _, place := range []struct {
		name string
		// enter puts the chunk in the place; leave moves it on, onto the
		// tee's queue, as the relay does.
		enter, leave func(tt *tee, c fakeconn.Chunk)
	}{
		{
			name:  "set aside for the client's answer",
			enter: func(tt *tee, c fakeconn.Chunk) { tt.await(c) },
			// settle pushes a chunk set aside before it lets go of it.
			leave: func(tt *tee, c fakeconn.Chunk) { tt.push(c); tt.doneAwaiting() },
		},
		{
			name:  "in a TLS layer's hand",
			enter: func(tt *tee, c fakeconn.Chunk) { tt.progress.layer.Store(c.ReadAt.UnixNano()) },
			// The forwarder takes the record (held), queues it and lets go
			// of it, and the layer reads again (its mark clears).
			leave: func(tt *tee, c fakeconn.Chunk) {
				tt.noteCarried(c.ReadAt)
				tt.push(c)
				tt.noteReleased()
				tt.noteLayerGone()
			},
		},
	} {
		t.Run(place.name, func(t *testing.T) {
			tt := newTee(fakeconn.FromDest, 1<<20, 16, time.Second, nil, nil, nil)
			fc := fakeconn.New(tt.readCh(), nil, nil)
			tt.consumed, tt.processedThrough = fc.Consumed, fc.ProcessedThrough
			tt.start(fc.Done())
			t.Cleanup(func() {
				_ = fc.Close()
				tt.close()
			})
			held := func(when string) {
				t.Helper()
				at, unstamped, ok := tt.oldestUnprocessed(unstampedInAnyWindow)
				if unstamped || !ok || at != stamp.UnixNano() {
					t.Fatalf("%s: oldest unprocessed (%d, unstamped %v, %v), want the chunk's stamp %d", when, at, unstamped, ok, stamp.UnixNano())
				}
			}
			c := fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("one"), ReadAt: stamp}
			place.enter(tt, c)
			held("while it waits there")
			place.leave(tt, c)
			got, err := fc.ReadAhead()
			if err != nil || string(got.Bytes) != "one" {
				t.Fatalf("ReadAhead = %q, %v; want the chunk", got.Bytes, err)
			}
			if fc.Consumed() != 3 || fc.Processed() != 0 {
				t.Fatalf("test setup: consumed %d, processed %d, want 3, 0", fc.Consumed(), fc.Processed())
			}
			held("once its parser read it ahead and has not processed it")
			fc.MarkProcessed(got)
			if at, _, ok := tt.oldestUnprocessed(unstampedInAnyWindow); ok {
				t.Fatalf("oldest unprocessed %d once the parser processed the chunk, want none", at)
			}
		})
	}
}
