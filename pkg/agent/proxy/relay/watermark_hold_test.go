package relay

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// heldCallAcrossAWindow puts a connection in the state a parser that holds a
// call reads on in: the call's request, captured before mid, is taken; the
// next chunk, the reply captured after mid, is taken too. By its reads alone
// the parser is past mid.
func heldCallAcrossAWindow(t *testing.T, h *relayHarness) (first, next fakeconn.Chunk, mid time.Time) {
	t.Helper()
	h.writeClient([]byte("req"))
	h.readDest(3)
	first = readChunk(t, h.r.ClientStream())
	mid = time.Now()
	time.Sleep(2 * time.Millisecond) // the reply is stamped after mid
	h.writeDest([]byte("ans"))
	h.readClient(3)
	next = readChunk(t, h.r.DestStream())
	if !first.ReadAt.Before(mid) || !next.ReadAt.After(mid) {
		t.Fatalf("setup: chunks stamped %v and %v, not either side of %v", first.ReadAt, next.ReadAt, mid)
	}
	return first, next, mid
}

// A parser that holds a call it parsed, its mock not handed on, and reads on
// past the call's window is not done with that window: the call's first chunk
// is at or before it. A hold from after the window does not hold it, the
// earliest hold on either stream counts, and once the parser clears it the
// window is done.
func TestWatermark_AParserHoldingACallItParsedIsNotDoneWithItsWindow(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	client, dest := h.r.ClientStream(), h.r.DestStream()

	first, next, mid := heldCallAcrossAWindow(t, h)
	start := first.ReadAt.Add(-time.Second)
	if w.PendingBefore(mid) || !w.Settled("", "", start, mid) {
		t.Fatalf("setup: the parser read past mid and no hold was set, yet the window was held: %v", w.PendingOn(mid))
	}

	client.HoldFrom(first.ReadAt)
	if !w.PendingBefore(mid) {
		t.Error("PendingBefore(mid): false while the parser held a call whose first chunk was captured before mid")
	}
	if w.Settled("", "", start, mid) {
		t.Error("Settled: true for a window holding the first chunk of a call the parser held")
	}
	if b := w.HeldBy("", "", start, mid, 0); b == nil || !strings.Contains(b.String(), "holds a call") {
		t.Errorf("HeldBy = %v, want the parser's hold named", b)
	}
	// A window that ends at the call's first chunk holds it.
	if !w.PendingBefore(first.ReadAt) {
		t.Error("PendingBefore(first chunk's time): false while the parser held the call that chunk opened")
	}

	client.HoldFrom(next.ReadAt) // a call opened after mid
	if w.PendingBefore(mid) {
		t.Errorf("PendingBefore(mid): true for a hold on a call opened after mid: %v", w.PendingOn(mid))
	}

	client.HoldFrom(time.Time{})
	dest.HoldFrom(first.ReadAt)
	if !w.PendingBefore(mid) {
		t.Error("PendingBefore(mid): false with the hold set on the destination stream")
	}
	client.HoldFrom(next.ReadAt)
	if !w.PendingBefore(mid) {
		t.Error("PendingBefore(mid): false with the earliest hold, on the destination stream, before mid")
	}
	client.HoldFrom(first.ReadAt)
	dest.HoldFrom(next.ReadAt)
	if !w.PendingBefore(mid) {
		t.Error("PendingBefore(mid): false with the earliest hold, on the client stream, before mid")
	}

	client.HoldFrom(time.Time{})
	dest.HoldFrom(time.Time{})
	if w.PendingBefore(mid) || !w.Settled("", "", start, mid) {
		t.Errorf("the window was still held once the parser cleared its holds: %v", w.PendingOn(mid))
	}
}

// A parser that has taken everything and waits for more while it holds a call
// is not done with the call's window either.
func TestWatermark_AParserWaitingWithACallHeldIsNotDone(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	client := h.r.ClientStream()

	h.writeClient([]byte("req"))
	h.readDest(3)
	first := readChunk(t, client)
	client.HoldFrom(first.ReadAt)
	blockInRead(t, client)
	after := time.Now()
	if !w.PendingBefore(after) {
		t.Error("PendingBefore: false while the parser waited for more with a call it parsed held")
	}
	client.HoldFrom(time.Time{})
	if w.PendingBefore(after) {
		t.Errorf("PendingBefore: true once the parser, waiting for more, cleared its hold: %v", w.PendingOn(after))
	}
}

// A look reads the parser's reads before its hold. A parser that sets its hold
// and reads past the window just as the look reads the hold leaves a look that
// saw its earlier reads only, and that look does not take it as past the
// window. Read the other way round, the look would see the read past the
// window without the hold set before it.
func TestWatermark_AHoldSetAsALookReadsItIsSeenWithTheReadPastIt(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	client, dest := h.r.ClientStream(), h.r.DestStream()

	h.writeClient([]byte("req"))
	h.readDest(3)
	first := readChunk(t, client)
	mid := time.Now()
	time.Sleep(2 * time.Millisecond) // the reply is stamped after mid
	h.writeDest([]byte("ans"))
	h.readClient(3)
	awaitState(t, "the reply to reach the parser's stream", func() bool {
		return h.r.teeD2C.acceptedBytes() == 3
	})

	fired := false
	var next fakeconn.Chunk
	hook := func() {
		if fired {
			return
		}
		fired = true
		client.HoldFrom(first.ReadAt)
		next = readChunk(t, dest)
	}
	afterHoldLook.Store(&hook)
	t.Cleanup(func() { afterHoldLook.Store(nil) })

	if !w.PendingBefore(mid) {
		t.Error("PendingBefore(mid): false for a look that read the parser past mid but not the hold it set before that read")
	}
	if !fired || !next.ReadAt.After(mid) {
		t.Fatalf("setup: the hook ran %v and the parser took a chunk stamped %v, want one after %v", fired, next.ReadAt, mid)
	}
	if !w.PendingBefore(mid) {
		t.Error("PendingBefore(mid): false on the next look, with the hold set")
	}
}
