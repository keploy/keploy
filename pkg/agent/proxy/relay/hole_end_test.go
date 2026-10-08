package relay

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// holeEnds records each call of a tee's endAtHole, or of Config.EndAtHole.
type holeEnds struct {
	mu   sync.Mutex
	dirs []fakeconn.Direction
	why  []string
}

func (h *holeEnds) end(dir fakeconn.Direction, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dirs = append(h.dirs, dir)
	h.why = append(h.why, reason)
}

func (h *holeEnds) calls() ([]fakeconn.Direction, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]fakeconn.Direction(nil), h.dirs...), append([]string(nil), h.why...)
}

// newHoleEndTee is a tee of the client direction for a parser that cannot
// re-align and asks to have its stream ended at its hole, with capBytes and an
// out channel of one chunk, so what drain cannot hand over stays queued.
func newHoleEndTee(t *testing.T, capBytes int64, memCheck func() bool) (*tee, *dropRecorder, *holeEnds) {
	t.Helper()
	rec := &dropRecorder{}
	ends := &holeEnds{}
	tt := newTee(fakeconn.FromClient, capBytes, 1, testStallGrace, memCheck, rec.record, nil)
	tt.endAtHole = func(reason string) { ends.end(fakeconn.FromClient, reason) }
	tt.start(make(chan struct{}))
	t.Cleanup(func() {
		tt.close()
		tt.waitDone()
	})
	return tt, rec, ends
}

// recvOrEnd takes the next chunk off out, or reports that out has ended.
func recvOrEnd(t *testing.T, tt *tee) (fakeconn.Chunk, bool) {
	t.Helper()
	select {
	case c, ok := <-tt.readCh():
		return c, ok
	case <-time.After(2 * time.Second):
		t.Fatal("out neither delivered a chunk nor ended")
	}
	return fakeconn.Chunk{}, false
}

// A tee whose parser cannot re-align and asks for it (Config.EndAtHole) ends
// its stream at its hole: it delivers every chunk it queued before the hole,
// in order, then calls endAtHole with why the chunk was lost, then ends out,
// so the FakeConn returns io.EOF exactly where the hole is. It marks no mock
// incomplete, for the lost chunk or for those it refuses after it: the parser
// runs behind its capture, and a mark voids whichever mock it emits next, one
// from before the hole. The drops are still counted, and onDesync still
// fires once.
//
// Before, the stream was fed nothing more and never ended while the
// connection lived, and each of those chunks marked a mock incomplete.
func TestTee_EndsAStreamAtItsHoleWhenItsParserAsks(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		cause    string
		pressure bool
	}{
		{name: "lost to the cap", cause: DropPerConnCap},
		{name: "lost to memory pressure", cause: DropMemoryPressure, pressure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var pressure struct {
				sync.Mutex
				on bool
			}
			memCheck := func() bool { pressure.Lock(); defer pressure.Unlock(); return pressure.on }
			tt, rec, ends := newHoleEndTee(t, 8, memCheck)
			desyncs := 0
			tt.onDesync = func(string) bool { desyncs++; return true }

			// Three chunks before the hole: one in out, one in drain's hand,
			// one queued.
			for _, p := range []string{"a1", "a2", "a3"} {
				if !tt.push(mkChunk(p)) {
					t.Fatalf("push %q before the hole was refused", p)
				}
			}
			if tc.pressure {
				pressure.Lock()
				pressure.on = true
				pressure.Unlock()
			}
			if tt.push(mkChunk("0123456789")) {
				t.Fatal("the chunk at the hole was admitted")
			}
			if tt.push(mkChunk("x")) {
				t.Fatal("a chunk after the hole was admitted")
			}
			if _, why := ends.calls(); len(why) != 0 {
				t.Fatalf("endAtHole was called %v with chunks before the hole still to deliver", why)
			}

			for _, want := range []string{"a1", "a2", "a3"} {
				c, ok := recvOrEnd(t, tt)
				if !ok {
					t.Fatalf("out ended before %q, a chunk teed before the hole", want)
				}
				if string(c.Bytes) != want {
					t.Fatalf("out delivered %q, want %q", c.Bytes, want)
				}
			}
			if c, ok := recvOrEnd(t, tt); ok {
				t.Fatalf("out delivered %q after the hole", c.Bytes)
			}
			dirs, why := ends.calls()
			if len(why) != 1 || why[0] != tc.cause || dirs[0] != fakeconn.FromClient {
				t.Fatalf("endAtHole was called %v %v by the time out ended, want once, for %q", dirs, why, tc.cause)
			}
			if got := rec.snapshot(); len(got) != 0 {
				t.Fatalf("the tee marked %v: a mark for the hole voids the next mock the parser emits, one from before the hole", got)
			}
			if n := tt.dropCount(); n != 2 {
				t.Fatalf("dropCount = %d, want 2: the chunk at the hole and the one after it", n)
			}
			if desyncs != 1 {
				t.Fatalf("onDesync fired %d times, want once", desyncs)
			}
			if tt.push(mkChunk("y")) {
				t.Fatal("a chunk was admitted after the stream ended")
			}
		})
	}

	t.Run("with nothing queued before the hole it ends at once", func(t *testing.T) {
		t.Parallel()
		tt, rec, ends := newHoleEndTee(t, 4, nil)
		if !tt.push(mkChunk("ab")) {
			t.Fatal("push before the hole was refused")
		}
		if c, ok := recvOrEnd(t, tt); !ok || string(c.Bytes) != "ab" {
			t.Fatalf("out delivered %q (ok %v), want ab", c.Bytes, ok)
		}
		// drain is parked on an empty queue now: the hole must wake it.
		time.Sleep(20 * time.Millisecond)
		if tt.push(mkChunk("0123456789")) {
			t.Fatal("the chunk at the hole was admitted")
		}
		if c, ok := recvOrEnd(t, tt); ok {
			t.Fatalf("out delivered %q after the hole", c.Bytes)
		}
		if _, why := ends.calls(); len(why) != 1 || why[0] != DropPerConnCap {
			t.Fatalf("endAtHole was called %v, want once, for %q", why, DropPerConnCap)
		}
		if got := rec.snapshot(); len(got) != 0 {
			t.Fatalf("the tee marked %v for the hole", got)
		}
	})

	t.Run("a connection that closes after the hole still ends at it", func(t *testing.T) {
		t.Parallel()
		tt, _, ends := newHoleEndTee(t, 8, nil)
		for _, p := range []string{"a1", "a2", "a3"} {
			tt.push(mkChunk(p))
		}
		tt.push(mkChunk("0123456789"))
		tt.close()
		for _, want := range []string{"a1", "a2", "a3"} {
			if c, ok := recvOrEnd(t, tt); !ok || string(c.Bytes) != want {
				t.Fatalf("out delivered %q (ok %v), want %q", c.Bytes, ok, want)
			}
		}
		if c, ok := recvOrEnd(t, tt); ok {
			t.Fatalf("out delivered %q after the hole", c.Bytes)
		}
		if _, why := ends.calls(); len(why) != 1 || why[0] != DropPerConnCap {
			t.Fatalf("endAtHole was called %v, want once, for %q: the stream is short, whether or not the connection ended after", why, DropPerConnCap)
		}
	})

	t.Run("a connection that closes with no hole does not end at one", func(t *testing.T) {
		t.Parallel()
		tt, rec, ends := newHoleEndTee(t, 8, nil)
		tt.push(mkChunk("a1"))
		tt.close()
		if c, ok := recvOrEnd(t, tt); !ok || string(c.Bytes) != "a1" {
			t.Fatalf("out delivered %q (ok %v), want a1", c.Bytes, ok)
		}
		if c, ok := recvOrEnd(t, tt); ok {
			t.Fatalf("out delivered %q after close", c.Bytes)
		}
		if _, why := ends.calls(); len(why) != 0 {
			t.Fatalf("endAtHole was called %v for a stream with no hole", why)
		}
		if got := rec.snapshot(); len(got) != 0 {
			t.Fatalf("the tee marked %v with nothing lost", got)
		}
	})

	t.Run("a pause is not a hole: it marks, and the stream goes on", func(t *testing.T) {
		t.Parallel()
		tt, rec, ends := newHoleEndTee(t, 8, nil)
		tt.setPaused(true)
		if tt.push(mkChunk("p")) {
			t.Fatal("push while paused was admitted")
		}
		tt.setPaused(false)
		if !tt.push(mkChunk("q")) {
			t.Fatal("push after the pause was refused")
		}
		if c, ok := recvOrEnd(t, tt); !ok || string(c.Bytes) != "q" {
			t.Fatalf("out delivered %q (ok %v), want q", c.Bytes, ok)
		}
		if got := rec.count(DropPaused); got != 1 {
			t.Fatalf("%s marks = %d, want 1: a pause is not a hole the stream ends at, so it marks as before", DropPaused, got)
		}
		if _, why := ends.calls(); len(why) != 0 {
			t.Fatalf("endAtHole was called %v for a pause", why)
		}
	})

	t.Run("a parser that re-aligns is fed after the hole, and marked as before", func(t *testing.T) {
		t.Parallel()
		rec := &dropRecorder{}
		ends := &holeEnds{}
		tt := newTee(fakeconn.FromClient, 4, 4, testStallGrace, nil, rec.record, nil)
		tt.parserCanResync = true
		tt.endAtHole = func(reason string) { ends.end(fakeconn.FromClient, reason) }
		tt.start(make(chan struct{}))
		t.Cleanup(func() { tt.close(); tt.waitDone() })

		tt.push(mkChunk("0123456789"))
		if !tt.push(mkChunk("ok")) {
			t.Fatal("a parser that re-aligns must be fed the bytes after the hole")
		}
		if c, ok := recvOrEnd(t, tt); !ok || string(c.Bytes) != "ok" {
			t.Fatalf("out delivered %q (ok %v), want ok", c.Bytes, ok)
		}
		if got := rec.count(DropPerConnCap); got != 1 {
			t.Fatalf("%s marks = %d, want 1", DropPerConnCap, got)
		}
		if _, why := ends.calls(); len(why) != 0 {
			t.Fatalf("endAtHole was called %v for a parser that re-aligns", why)
		}
	})

	t.Run("a parser that does not ask is marked as before, and its stream does not end", func(t *testing.T) {
		t.Parallel()
		tt, _, rec := newTestTee(t, 4, 4, nil)
		tt.push(mkChunk("0123456789"))
		tt.push(mkChunk("ok"))
		if rec.count(DropPerConnCap) != 1 || rec.count(DropDesynced) != 1 {
			t.Fatalf("marks %v, want one %s and one %s", rec.snapshot(), DropPerConnCap, DropDesynced)
		}
		select {
		case c, ok := <-tt.readCh():
			t.Fatalf("out delivered %q (ok %v) for a desynced stream of a parser that did not ask to have it ended", c.Bytes, ok)
		case <-time.After(100 * time.Millisecond):
		}
	})
}

// Config.EndAtHole reaches each tee with its own direction, and a stream that
// ends at its hole ends its FakeConn (io.EOF) while the other direction, and
// the forward path, go on.
func TestDesyncedRelayEndsAStreamAtItsHoleWhenAsked(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		dir   fakeconn.Direction
		write func(h *relayHarness, p []byte)
		read  func(h *relayHarness, n int) []byte
	}{
		{
			name:  "the client's",
			dir:   fakeconn.FromClient,
			write: func(h *relayHarness, p []byte) { h.writeClient(p) },
			read:  func(h *relayHarness, n int) []byte { return h.readDest(n) },
		},
		{
			name:  "the server's",
			dir:   fakeconn.FromDest,
			write: func(h *relayHarness, p []byte) { h.writeDest(p) },
			read:  func(h *relayHarness, n int) []byte { return h.readClient(n) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			drops := newDropSink()
			ends := &holeEnds{}
			h := newHarness(t, Config{
				PerConnCap:           4,
				TeeChanBuf:           64,
				OnMarkMockIncomplete: drops.record,
				EndAtHole:            ends.end,
			})
			streams := [2]*fakeconn.FakeConn{h.r.ClientStream(), h.r.DestStream()}
			lost, other := streams[tc.dir], streams[1-tc.dir]

			for _, p := range []string{"ab", "0123456789", "cd"} {
				go tc.write(h, []byte(p))
				if got := tc.read(h, len(p)); string(got) != p {
					t.Fatalf("the peer got %q, want %q: the forward path is untouched", got, p)
				}
			}
			c, err := lost.ReadChunk()
			if err != nil || string(c.Bytes) != "ab" {
				t.Fatalf("the stream delivered %q, %v; want ab, the chunk before the hole", c.Bytes, err)
			}
			_ = lost.SetReadDeadline(time.Now().Add(2 * time.Second))
			if c, err := lost.ReadChunk(); !errors.Is(err, io.EOF) {
				t.Fatalf("after the chunk before the hole the stream gave %q, %v; want io.EOF", c.Bytes, err)
			}
			dirs, why := ends.calls()
			if len(dirs) != 1 || dirs[0] != tc.dir || why[0] != DropPerConnCap {
				t.Fatalf("EndAtHole was called %v %v, want once, for %v and %q", dirs, why, tc.dir, DropPerConnCap)
			}
			if got := drops.snapshot(); len(got) != 0 {
				t.Fatalf("the relay marked %v for the hole", got)
			}

			// The other direction goes on.
			if tc.dir == fakeconn.FromClient {
				go h.writeDest([]byte("ef"))
				_ = h.readClient(2)
			} else {
				go h.writeClient([]byte("ef"))
				_ = h.readDest(2)
			}
			_ = other.SetReadDeadline(time.Now().Add(2 * time.Second))
			if c, err := other.ReadChunk(); err != nil || string(c.Bytes) != "ef" {
				t.Fatalf("the other stream delivered %q, %v; want ef", c.Bytes, err)
			}
		})
	}
}
