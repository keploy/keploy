package relay

import (
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

func TestTee_DropOnChannelFull(t *testing.T) {
	t.Parallel()
	// Large cap but channel buf=1; drain goroutine starts but we
	// never receive on readCh, so staging fills.
	tt, _, rec := newTestTee(t, 1<<30, 1, nil)

	// Push enough that at least one drop occurs. The drain goroutine
	// buffers one chunk in `out` too, so we need >2 pushes to be sure
	// we see a drop.
	for i := 0; i < 10; i++ {
		tt.push(mkChunk("x"))
	}
	// Give the drain goroutine time to settle its buffer.
	time.Sleep(20 * time.Millisecond)
	if rec.count(DropChannelFull) == 0 {
		t.Fatalf("expected at least one channel_full drop, got %v", rec.snapshot())
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
