package relay

import (
	"testing"
	"time"
)

// HeldProgress counts the bytes the parsers behind a test case's window have
// consumed: it moves as such a parser takes a chunk, and not for a connection
// that carried nothing within the window.
func TestWatermark_HeldProgressMovesAsTheParserBehindTheWindowReads(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	before := time.Now()
	h.writeClient([]byte("one"))
	h.readDest(3)
	time.Sleep(2 * time.Millisecond) // the next chunk is a chunk of its own
	h.writeClient([]byte("two"))
	h.readDest(3)
	after := time.Now()

	start := w.HeldProgress("", "", before, after)
	if w.Settled("", "", before, after) {
		t.Fatal("setup: the window is settled with its parser behind")
	}
	readChunk(t, h.r.ClientStream()) // the parser takes "one": still behind "two"
	moved := w.HeldProgress("", "", before, after)
	if moved == start {
		t.Fatal("HeldProgress did not move as the parser behind the window read a chunk")
	}
	if got := w.HeldProgress("", "", before.Add(-time.Hour), before.Add(-time.Minute)); got != 0 {
		t.Fatalf("HeldProgress counted %d for a window the connection carried nothing in, want 0", got)
	}
}

// What another connection does does not move a test case's held progress: one
// that consumes bytes outside its window, or closes, says nothing of its
// verdict. Counted, a busy capture would keep a test case whose own parser
// has stopped held for as long as anything else in it moves.
func TestWatermark_HeldProgressIsNotMovedByOtherConnections(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	before := time.Now()
	h.writeClient([]byte("one"))
	h.readDest(3)
	after := time.Now()
	if w.Settled("", "", before, after) {
		t.Fatal("setup: the window is settled with its parser behind")
	}
	held := w.HeldProgress("", "", before, after)

	other := newHarness(t, Config{})
	untrack := w.Track(other.r, "conn-2")
	other.writeClient([]byte("two"))
	other.readDest(3)
	readChunk(t, other.r.ClientStream()) // another parser moves, after the window
	if got := w.HeldProgress("", "", before, after); got != held {
		t.Fatalf("HeldProgress moved from %d to %d as a connection outside the window was parsed", held, got)
	}
	untrack() // and its parser returns
	if got := w.HeldProgress("", "", before, after); got != held {
		t.Fatalf("HeldProgress moved from %d to %d as a connection outside the window retired", held, got)
	}
}
