package relay

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// newTestTee constructs a tee with reasonable defaults for tests.
// cap is expressed in bytes; buf is the channel capacity.
func newTestTee(t *testing.T, capBytes int64, buf int, memCheck func() bool) (*tee, func(reason string), *dropRecorder) {
	t.Helper()
	rec := &dropRecorder{}
	t2 := newTee(fakeconn.FromClient, capBytes, buf, memCheck, rec.record, nil)
	t.Cleanup(func() {
		t2.close()
		t2.waitDone()
	})
	return t2, rec.record, rec
}

// dropRecorder collects drop reasons for assertion.
type dropRecorder struct {
	mu      sync.Mutex
	reasons []string
}

func (d *dropRecorder) record(reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reasons = append(d.reasons, reason)
}

func (d *dropRecorder) snapshot() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.reasons))
	copy(out, d.reasons)
	return out
}

func (d *dropRecorder) count(reason string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, r := range d.reasons {
		if r == reason {
			n++
		}
	}
	return n
}

func mkChunk(payload string) fakeconn.Chunk {
	return fakeconn.Chunk{
		Dir:       fakeconn.FromClient,
		Bytes:     []byte(payload),
		ReadAt:    time.Now(),
		WrittenAt: time.Now(),
	}
}

func TestTee_PushAndDrain(t *testing.T) {
	t.Parallel()
	tt, _, rec := newTestTee(t, 1<<20, 4, nil)

	if !tt.push(mkChunk("hello")) {
		t.Fatalf("push returned false unexpectedly")
	}
	got := <-tt.readCh()
	if string(got.Bytes) != "hello" {
		t.Fatalf("got bytes %q, want %q", got.Bytes, "hello")
	}
	if rec.count(DropMemoryPressure)+rec.count(DropPerConnCap)+rec.count(DropChannelFull) != 0 {
		t.Fatalf("unexpected drops: %v", rec.snapshot())
	}
}

func TestTee_DropOnMemoryPressure(t *testing.T) {
	t.Parallel()
	var paused atomic.Bool
	paused.Store(true)
	tt, _, rec := newTestTee(t, 1<<20, 4, paused.Load)

	if tt.push(mkChunk("x")) {
		t.Fatalf("push should have dropped under memory pressure")
	}
	if rec.count(DropMemoryPressure) != 1 {
		t.Fatalf("want 1 memory_pressure drop, got reasons %v", rec.snapshot())
	}
}

func TestTee_DropOnPerConnCap(t *testing.T) {
	t.Parallel()
	// Cap at 4 bytes; first 3-byte push fits, second also fits (6 > 4) → drop.
	tt, _, rec := newTestTee(t, 4, 16, nil)

	if !tt.push(mkChunk("abc")) {
		t.Fatalf("first push should succeed")
	}
	if tt.push(mkChunk("def")) {
		t.Fatalf("second push should be dropped (per_conn_cap)")
	}
	if rec.count(DropPerConnCap) < 1 {
		t.Fatalf("want at least 1 per_conn_cap drop, got %v", rec.snapshot())
	}
}

// TestTee_SpillsInsteadOfDroppingOnChannelFull pins the core of the
// boot-capture fix: a full staging channel no longer forces a drop. buf=1
// with no receiver means every push after the first would previously drop
// on channel_full — losing response chunks and stranding their mock. With
// the overflow spill each push instead succeeds (buffered up to the
// per-conn cap), so no chunk is dropped.
func TestTee_SpillsInsteadOfDroppingOnChannelFull(t *testing.T) {
	t.Parallel()
	// Large cap so the spill has headroom; buf=1 so staging fills at once.
	tt, _, rec := newTestTee(t, 1<<30, 1, nil)

	for i := 0; i < 64; i++ {
		if !tt.push(mkChunk(fmt.Sprintf("c%03d", i))) {
			t.Fatalf("push %d dropped despite spill headroom (drops=%v)", i, rec.snapshot())
		}
	}
	if rec.count(DropChannelFull) != 0 {
		t.Fatalf("channel_full drop must not happen with the spill: %v", rec.snapshot())
	}
}

// TestTee_SpillPreservesOrderNoLoss proves the spill is lossless AND
// order-preserving end to end: push a burst that overruns the drain, then
// drain everything (including the teardown-flushed overflow tail) and
// assert the exact sequence is delivered.
func TestTee_SpillPreservesOrderNoLoss(t *testing.T) {
	t.Parallel()
	tt, _, rec := newTestTee(t, 1<<30, 1, nil)

	recvd := make(chan []string, 1)
	go func() {
		var got []string
		for c := range tt.readCh() {
			got = append(got, string(c.Bytes))
		}
		recvd <- got
	}()

	const N = 300
	for i := 0; i < N; i++ {
		if !tt.push(mkChunk(fmt.Sprintf("c%03d", i))) {
			t.Fatalf("push %d dropped unexpectedly (drops=%v)", i, rec.snapshot())
		}
	}
	tt.close()
	tt.waitDone()
	got := <-recvd

	if len(got) != N {
		t.Fatalf("LOST chunks: got %d, want %d (drops=%v)", len(got), N, rec.snapshot())
	}
	for i, s := range got {
		if want := fmt.Sprintf("c%03d", i); s != want {
			t.Fatalf("out of wire order at index %d: got %q want %q", i, s, want)
		}
	}
	if d := rec.count(DropChannelFull) + rec.count(DropPerConnCap); d != 0 {
		t.Fatalf("unexpected drops despite spill + cap headroom: %v", rec.snapshot())
	}
}

// TestTee_CloseRacingSpillBacklog_Conserves pins the subtle teardown
// guarantee flagged by review: when close() races an overflow backlog, every
// accepted chunk is either delivered exactly once or counted as a drop —
// never silently lost, duplicated, or reordered. Correctness depends on
// push's len(overflow)==0 fast-path re-checking `closed`; a refactor that
// broke that ordering would reintroduce stranding, and this test would catch
// it. Run under -race, it also stresses the push/close/drain interleaving.
func TestTee_CloseRacingSpillBacklog_Conserves(t *testing.T) {
	t.Parallel()
	for iter := 0; iter < 100; iter++ {
		rec := &dropRecorder{}
		tt := newTee(fakeconn.FromClient, 1<<30, 1, nil, rec.record, nil) // buf=1 forces spill

		recvd := make(chan []int, 1)
		go func() {
			var got []int
			for c := range tt.readCh() {
				var n int
				_, _ = fmt.Sscanf(string(c.Bytes), "%d", &n)
				got = append(got, n)
			}
			recvd <- got
		}()

		const N = 50
		accepted := 0
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < N; i++ {
				if tt.push(mkChunk(fmt.Sprintf("%d", i))) {
					accepted++ // single writer; read after wg.Wait()
				}
			}
		}()
		wg.Wait()
		tt.close()
		tt.waitDone()
		got := <-recvd

		seen := make(map[int]bool, len(got))
		for i, n := range got {
			if seen[n] {
				t.Fatalf("iter %d: duplicate delivery of %d: %v", iter, n, got)
			}
			seen[n] = true
			if i > 0 && n <= got[i-1] {
				t.Fatalf("iter %d: out of wire order: %v", iter, got)
			}
		}
		if delivered, drops := len(got), int(tt.dropCount()); delivered+drops != accepted {
			t.Fatalf("iter %d: conservation violated: delivered=%d + drops=%d != accepted=%d",
				iter, delivered, drops, accepted)
		}
	}
}

// TestTee_DropsWhenOverflowExceedsCap confirms the spill is bounded: the
// per-conn cap still governs total buffered bytes (staging + overflow), so
// a runaway that exceeds it drops (per_conn_cap) rather than growing
// without limit — preserving the OOM-safety contract.
func TestTee_DropsWhenOverflowExceedsCap(t *testing.T) {
	t.Parallel()
	// cap=10 bytes, buf=1, no receiver. Each chunk is 4 bytes.
	tt, _, rec := newTestTee(t, 10, 1, nil)
	// "aaaa" (staging) + "bbbb" (overflow, total 8) fit; "cccc" (total 12
	// > 10) must drop on per_conn_cap.
	if !tt.push(mkChunk("aaaa")) {
		t.Fatal("first push should fit")
	}
	if !tt.push(mkChunk("bbbb")) {
		t.Fatal("second push should spill within cap")
	}
	if tt.push(mkChunk("cccc")) {
		t.Fatal("third push should drop (staging+overflow would exceed cap)")
	}
	if rec.count(DropPerConnCap) == 0 {
		t.Fatalf("want a per_conn_cap drop, got %v", rec.snapshot())
	}
}

func TestTee_PausedDropsWithoutCapUsage(t *testing.T) {
	t.Parallel()
	tt, _, rec := newTestTee(t, 1<<20, 4, nil)

	tt.setPaused(true)
	if tt.push(mkChunk("hello")) {
		t.Fatalf("push while paused should drop")
	}
	if rec.count(DropPaused) != 1 {
		t.Fatalf("want 1 paused drop, got %v", rec.snapshot())
	}

	tt.setPaused(false)
	if !tt.push(mkChunk("world")) {
		t.Fatalf("push after resume should succeed")
	}
}

func TestTee_ClosePreventsSendPanic(t *testing.T) {
	t.Parallel()
	tt := newTee(fakeconn.FromClient, 1<<20, 4, nil, nil, nil)

	// Spawn pushers racing with close; no panic expected.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				tt.push(mkChunk("r"))
			}
		}()
	}
	time.Sleep(time.Millisecond)
	tt.close()
	wg.Wait()
	tt.waitDone()
}

func TestTee_CloseIsIdempotent(t *testing.T) {
	t.Parallel()
	tt := newTee(fakeconn.FromClient, 1<<20, 4, nil, nil, nil)
	tt.close()
	tt.close() // must not panic
	tt.waitDone()
}

// TestTee_StagedChunkSurvivesClose is the regression guard for the
// "server closed before response" startup-mock drop. When the upstream
// sends a full Content-Length response then immediately closes the
// connection (Connection: close), the forwarder pushes the response
// chunk into staging and exits, and the relay then calls close() — which
// fires the shutdown channel. The old drain loop selected between
// delivering the chunk to out and dropping it on shutdown, so a fully
// recorded response chunk was discarded on roughly half the teardowns,
// intermittently dropping the boot-time startup mock from a test set.
//
// A chunk that was successfully admitted to staging before close() MUST
// be delivered to out, never dropped: out shares staging's capacity and
// close() halts further pushes, so the bounded tail always fits. The
// loop runs many close races to make the old coin-flip behaviour fail
// deterministically (it would drop on ~50% of iterations).
func TestTee_StagedChunkSurvivesClose(t *testing.T) {
	t.Parallel()
	const iters = 200
	for i := 0; i < iters; i++ {
		rec := &dropRecorder{}
		tt := newTee(fakeconn.FromClient, 1<<30, 4, nil, rec.record, nil)

		// Admit a chunk into staging, then immediately tear the tee
		// down — mirroring the forwarder pushing the final response
		// chunk and the relay closing the tee right behind it.
		if !tt.push(mkChunk("startup-secret-response")) {
			t.Fatalf("iter %d: push returned false unexpectedly", i)
		}
		tt.close()

		// The consumer (parser) drains out after teardown. Every chunk
		// admitted to staging must come out — none may be dropped.
		var got int
		for c := range tt.readCh() {
			if string(c.Bytes) != "startup-secret-response" {
				t.Fatalf("iter %d: unexpected chunk %q", i, c.Bytes)
			}
			got++
		}
		tt.waitDone()

		if got != 1 {
			t.Fatalf("iter %d: delivered %d chunks, want 1 (chunk dropped on teardown)", i, got)
		}
		if d := rec.count(DropChannelFull) + rec.count(DropMemoryPressure) + rec.count(DropPerConnCap); d != 0 {
			t.Fatalf("iter %d: unexpected push-side drops: %v", i, rec.snapshot())
		}
	}
}
