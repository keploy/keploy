package relay

import (
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
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

// A connection that carried nothing within a test case's window holds it back
// whatever its parser is doing.
func TestWatermark_AConnectionIdleOverAWindowDoesNotHoldIt(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	h.writeClient([]byte("early"))
	h.readDest(5) // never read by the parser
	time.Sleep(2 * time.Millisecond)
	start := time.Now()
	end := start.Add(time.Millisecond)
	if !w.Settled("", "", start, end) {
		t.Fatalf("Settled: false for a window the connection carried nothing in: %v", w.HeldBy("", "", start, end, 0))
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
