package relay

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
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

// A chunk being stamped (heldUnstamped) on another connection does not move a
// test case's held progress, whether that connection's chunks came before the
// window or after it, or within it with its parser past them, waiting for
// more. Counted, it would take in that connection's bytes for the
// moment between its forwarder's Read and the stamp, and drop them again: each
// such move restarts the hold's stall clock for a test case whose own parser
// has stopped, and an app that keeps opening connections would keep it held.
// The verdict still takes such a chunk as one that can land in the window.
// Progress is set by hand on relays that are never run (unrunRelay), whose
// parsers took a chunk through the tee.
func TestWatermark_HeldProgressIsNotMovedByAChunkBeingStamped(t *testing.T) {
	t.Parallel()
	start := time.Now().UnixNano()
	end := start + int64(time.Second)
	// took has r's parser take a chunk stamped at, through its client tee.
	took := func(r *Relay, at int64) {
		t.Helper()
		r.teeC2D.progress.first.Store(at)
		r.teeC2D.progress.last.Store(at)
		if !r.teeC2D.push(fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("one"), ReadAt: time.Unix(0, at)}) {
			t.Fatal("setup: the tee refused the chunk")
		}
		readChunk(t, r.ClientStream())
	}

	s, e := time.Unix(0, start), time.Unix(0, end)
	w := NewWatermark()
	behind := unrunRelay(t) // behind the window: its parser has not got to the answer
	took(behind, start)
	behind.teeD2C.progress.first.Store(start)
	behind.teeD2C.progress.last.Store(start)
	behind.teeD2C.progress.held.Store(start)
	defer w.Track(behind, "conn-1")()

	for _, row := range []struct {
		name string
		at   int64
		// waits: the parser took the chunk and waits for more.
		waits bool
	}{
		{"a connection whose chunks came after the window", end + int64(time.Second), false},
		{"a connection whose chunks came before the window", start - int64(time.Hour), false},
		{"a connection whose chunk came within the window, its parser past it", start, true},
	} {
		other := unrunRelay(t)
		took(other, row.at)
		if row.waits {
			blockInRead(t, other.ClientStream())
		}
		untrack := w.Track(other, "conn-2")
		held := w.HeldProgress("", "", s, e)
		if held == 0 {
			t.Fatalf("%s: setup: HeldProgress is 0 with conn-1's parser behind the window", row.name)
		}
		other.teeD2C.progress.held.Store(heldUnstamped)
		if got := w.HeldProgress("", "", s, e); got != held {
			t.Errorf("%s: HeldProgress moved from %d to %d while its forwarder stamped a chunk", row.name, held, got)
		}

		only := NewWatermark()
		untrackOnly := only.Track(other, "conn-2")
		if only.Settled("", "", s, e) {
			t.Errorf("%s: Settled: true while a chunk that can land in the window is being stamped", row.name)
		}
		other.teeD2C.progress.held.Store(0)
		if !only.Settled("", "", s, e) {
			t.Errorf("%s: setup: Settled: false for a window the connection carried nothing in: %v", row.name, only.HeldBy("", "", s, e, 0))
		}
		untrackOnly()
		untrack()
	}
}

// Bytes a TLS layer read within the window keep the connection behind it while
// its forwarder stamps the record they bring (held is heldUnstamped):
// HeldProgress's view passes over the chunk not stamped yet, not the layer's
// stamp under it. Passed over too, the connection would leave the count for
// that moment and come back once the stamp is stored, and each such move
// restarts the hold's stall clock for a test case whose parser has stopped.
// Progress is set by hand on a relay that is never run, whose parser took a
// chunk read after the window, so its last read alone would take it as past
// the window.
func TestWatermark_HeldProgressCountsATLSLayersReadWhileItsRecordIsStamped(t *testing.T) {
	t.Parallel()
	start := time.Now().UnixNano()
	end := start + int64(time.Second)
	late := end + int64(time.Second)
	s, e := time.Unix(0, start), time.Unix(0, end)

	r := unrunRelay(t)
	r.teeC2D.progress.first.Store(late)
	r.teeC2D.progress.last.Store(late)
	if !r.teeC2D.push(fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("one"), ReadAt: time.Unix(0, late)}) {
		t.Fatal("setup: the tee refused the chunk")
	}
	readChunk(t, r.ClientStream())
	r.teeD2C.progress.layer.Store(start)
	w := NewWatermark()
	defer w.Track(r, "conn-1")()

	held := w.HeldProgress("", "", s, e)
	if held == 0 {
		t.Fatal("setup: HeldProgress is 0 with the layer holding a reply read in the window")
	}
	r.teeD2C.progress.held.Store(heldUnstamped)
	if ok, _ := r.passedThrough(end, stampsOnly); ok {
		t.Error("passedThrough, stamps only: true while the layer holds bytes read in the window and the forwarder stamps their record")
	}
	if got := w.HeldProgress("", "", s, e); got != held {
		t.Errorf("HeldProgress moved from %d to %d while the forwarder stamped the record of a read in the window", held, got)
	}
	r.teeD2C.progress.held.Store(start)
	if got := w.HeldProgress("", "", s, e); got != held {
		t.Errorf("HeldProgress moved from %d to %d as the forwarder stored the record's stamp", held, got)
	}
}
