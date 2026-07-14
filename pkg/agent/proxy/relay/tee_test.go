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
	t2 := newTee(fakeconn.FromClient, capBytes, buf, memCheck, rec.record, rec.recordAt, nil)
	t.Cleanup(func() {
		t2.close()
		t2.waitDone()
	})
	return t2, rec.record, rec
}

// dropRecorder collects drop reasons for assertion. It also records the
// (reason, ts) pairs delivered via the onDropAt callback so tests can
// assert the accurately-attributed orphan-window path.
type dropRecorder struct {
	mu      sync.Mutex
	reasons []string
	atTs    []time.Time
	atReas  []string
}

func (d *dropRecorder) record(reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reasons = append(d.reasons, reason)
}

func (d *dropRecorder) recordAt(reason string, ts time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.atReas = append(d.atReas, reason)
	d.atTs = append(d.atTs, ts)
}

// atSnapshot returns the (reason, ts) pairs seen via onDropAt.
func (d *dropRecorder) atSnapshot() ([]string, []time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rs := make([]string, len(d.atReas))
	copy(rs, d.atReas)
	ts := make([]time.Time, len(d.atTs))
	copy(ts, d.atTs)
	return rs, ts
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

// TestTee_PressureSelfManagedSkipsMemoryPressureDrop locks in the deep fix
// for the go-memory-load-mongo tail dead zone: a tee whose parser self-manages
// memory pressure (mongo/v2) must NOT drop chunks under memoryguard pressure,
// so the length-prefix reassembler is never desynced by a mid-message drop and
// stays byte-synced across the pause.
func TestTee_PressureSelfManagedSkipsMemoryPressureDrop(t *testing.T) {
	t.Parallel()
	var memPressure atomic.Bool // the memCheck (memoryguard) signal, not the paused flag
	memPressure.Store(true)
	tt, _, rec := newTestTee(t, 1<<20, 4, memPressure.Load)
	tt.setPressureSelfManaged(true)

	if !tt.push(mkChunk("hello")) {
		t.Fatalf("pressureSelfManaged tee must deliver (not drop) under memory pressure")
	}
	got := <-tt.readCh()
	if string(got.Bytes) != "hello" {
		t.Fatalf("got bytes %q, want %q", got.Bytes, "hello")
	}
	if rec.count(DropMemoryPressure) != 0 {
		t.Fatalf("self-managed tee must record no memory_pressure drop, got %v", rec.snapshot())
	}
}

// TestTee_PressureSelfManagedStillDropsCapacityAndPaused guards the boundary
// of the opt-out: only the memory_pressure path is suppressed. Genuine
// mid-message byte loss (per_conn_cap AND channel_full) and the abort/finalize
// pause MUST still drop — the capacity paths so the parser resyncs on real byte
// loss (and memory stays bounded), the pause because an aborted/finalized
// session has no live mock.
func TestTee_PressureSelfManagedStillDropsCapacityAndPaused(t *testing.T) {
	t.Parallel()

	// per_conn_cap still fires even while pressure is active and self-managed.
	var pressure atomic.Bool
	pressure.Store(true)
	ttCap, _, recCap := newTestTee(t, 4, 16, pressure.Load)
	ttCap.setPressureSelfManaged(true)
	if ttCap.push(mkChunk("abcde")) { // 5 bytes > cap 4
		t.Fatalf("oversized push must still trip per_conn_cap under self-managed pressure")
	}
	if recCap.count(DropPerConnCap) != 1 {
		t.Fatalf("want 1 per_conn_cap drop, got %v", recCap.snapshot())
	}
	if recCap.count(DropMemoryPressure) != 0 {
		t.Fatalf("memory_pressure must be opted out, got %v", recCap.snapshot())
	}

	// channel_full still fires: with a large cap but chanBuf=1 and no reader,
	// staging saturates and the self-managed tee must drop (bounding memory)
	// rather than silently backlog forever.
	var pressure2 atomic.Bool
	pressure2.Store(true)
	ttChan, _, recChan := newTestTee(t, 1<<30, 1, pressure2.Load)
	ttChan.setPressureSelfManaged(true)
	for i := 0; i < 10; i++ {
		ttChan.push(mkChunk("x"))
	}
	time.Sleep(20 * time.Millisecond) // let the drain goroutine settle its buffer
	if recChan.count(DropChannelFull) == 0 {
		t.Fatalf("self-managed tee must still drop on channel_full, got %v", recChan.snapshot())
	}
	if recChan.count(DropMemoryPressure) != 0 {
		t.Fatalf("memory_pressure must be opted out, got %v", recChan.snapshot())
	}

	// paused (PauseTees abort / KindPauseDir finalize) still drops.
	ttPause, _, recPause := newTestTee(t, 1<<20, 4, nil)
	ttPause.setPressureSelfManaged(true)
	ttPause.setPaused(true)
	if ttPause.push(mkChunk("x")) {
		t.Fatalf("paused tee must still drop under self-managed pressure")
	}
	if recPause.count(DropPaused) != 1 {
		t.Fatalf("want 1 paused drop, got %v", recPause.snapshot())
	}
}

// TestTee_PressureSelfManagedSurvivesPauseResume is the load-bearing invariant
// for the abort-recovery interaction: the mongo opt-in is set ONCE per
// connection, before the parser-generation loop, and the relay/tees outlive
// every generation. A SessionOnAbort → PauseTees → ResumeTees respawn cycle
// must NOT clobber pressureSelfManaged — otherwise the recovered generation
// would resume dropping mongo chunks under pressure and re-open the very dead
// zone this fixes. setPaused mirrors what Pause/ResumeTees drive.
func TestTee_PressureSelfManagedSurvivesPauseResume(t *testing.T) {
	t.Parallel()
	var memPressure atomic.Bool
	memPressure.Store(true)
	tt, _, rec := newTestTee(t, 1<<20, 4, memPressure.Load)
	tt.setPressureSelfManaged(true)

	// Delivered under pressure before the abort.
	if !tt.push(mkChunk("before")) {
		t.Fatalf("pre-abort push must deliver under self-managed pressure")
	}
	<-tt.readCh()

	// Abort: PauseTees drops; then ResumeTees re-arms delivery.
	tt.setPaused(true)
	if tt.push(mkChunk("during-abort")) {
		t.Fatalf("push while paused (aborted) must drop")
	}
	tt.setPaused(false)

	// After resume, the self-managed opt-out MUST still hold under pressure.
	if !tt.push(mkChunk("after")) {
		t.Fatalf("post-resume push must still deliver under self-managed pressure — pressureSelfManaged was clobbered by the pause/resume cycle")
	}
	if string((<-tt.readCh()).Bytes) != "after" {
		t.Fatalf("post-resume chunk mismatch")
	}
	if rec.count(DropMemoryPressure) != 0 {
		t.Fatalf("no memory_pressure drop expected across the whole cycle, got %v", rec.snapshot())
	}
}

func TestTee_DropOnPerConnCap(t *testing.T) {
	t.Parallel()
	// A single chunk LARGER than the cap always trips per_conn_cap on the
	// calling goroutine (bytes.Load()+n > cap with n > cap), independent of
	// the drain goroutine. The old two-small-push form ("abc" then "def",
	// cap 4) relied on the drain NOT yet having decremented the byte counter
	// when the second push ran — a ~1/12000 race. Oversized is deterministic.
	tt, _, rec := newTestTee(t, 4, 16, nil)

	if tt.push(mkChunk("abcde")) { // 5 bytes > cap 4
		t.Fatalf("oversized push must trip per_conn_cap")
	}
	if rec.count(DropPerConnCap) != 1 {
		t.Fatalf("want 1 per_conn_cap drop, got %v", rec.snapshot())
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

// TestTee_DropRecordsWindowAt is the guard for the orphan-window fix and
// its whitelist: a genuine per-op byte-loss drop (per_conn_cap, channel_full)
// reports the dropped chunk's OWN wire timestamp via onDropAt, while the
// sustained-interval reasons (memory_pressure, paused) report NOTHING — they
// would flood the bounded ring and mass-suppress healthy TCs. A successful
// push reports nothing. channel_full is covered by the coalesce test below.
func TestTee_DropRecordsWindowAt(t *testing.T) {
	t.Parallel()
	stamp := time.Unix(1700000000, 12345)
	chunkAt := func(payload string) fakeconn.Chunk {
		return fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte(payload), ReadAt: stamp}
	}

	// memory_pressure: drops, but records NO orphan window (pressureRanges
	// already cover it; per-chunk recording over a pause floods the ring).
	var paused atomic.Bool
	paused.Store(true)
	tt, _, rec := newTestTee(t, 1<<20, 4, paused.Load)
	if tt.push(chunkAt("x")) {
		t.Fatalf("push should have dropped under memory pressure")
	}
	if rec.count(DropMemoryPressure) != 1 {
		t.Fatalf("want 1 memory_pressure drop via onDrop, got %v", rec.snapshot())
	}
	if reasons, _ := rec.atSnapshot(); len(reasons) != 0 {
		t.Fatalf("memory_pressure must record NO orphan window, got %v", reasons)
	}

	// paused: drops, but records NO orphan window (an aborted connection
	// stays paused raw-forwarding until close — recording every chunk would
	// mass-suppress healthy TCs for the connection's whole remaining life).
	tt2, _, rec2 := newTestTee(t, 1<<20, 4, nil)
	tt2.setPaused(true)
	if tt2.push(chunkAt("y")) {
		t.Fatalf("push while paused should drop")
	}
	if rec2.count(DropPaused) != 1 {
		t.Fatalf("want 1 paused drop via onDrop, got %v", rec2.snapshot())
	}
	if rs2, _ := rec2.atSnapshot(); len(rs2) != 0 {
		t.Fatalf("paused must record NO orphan window, got %v", rs2)
	}

	// per_conn_cap: genuine per-op loss → records a window at the chunk's ts.
	// Oversized chunk (len > cap) for determinism: it never enters staging,
	// so t.bytes stays 0 and the cap check trips independent of the drain
	// goroutine — unlike a two-small-push form which races the drain.
	tt3, _, rec3 := newTestTee(t, 4, 16, nil)
	if tt3.push(chunkAt("abcde")) { // 5 bytes > cap 4
		t.Fatalf("oversized push must trip per_conn_cap")
	}
	rs3, ts3 := rec3.atSnapshot()
	if len(rs3) != 1 || rs3[0] != DropPerConnCap || !ts3[0].Equal(stamp) {
		t.Fatalf("per_conn_cap: want one per_conn_cap window at %v, got %v / %v", stamp, rs3, ts3)
	}

	// A successful push records NO window.
	tt4, _, rec4 := newTestTee(t, 1<<20, 4, nil)
	if !tt4.push(chunkAt("ok")) {
		t.Fatalf("healthy push should succeed")
	}
	if rs, _ := rec4.atSnapshot(); len(rs) != 0 {
		t.Fatalf("healthy push must record no orphan window, got %v", rs)
	}
}

// TestTee_DropWindowChannelFullCoalesceFallback covers the paths
// TestTee_DropRecordsWindowAt does not: the channel_full drop (the
// primary target of the fix, and the only path where drop() runs after
// closeMu.RUnlock), per-operation coalescing of a chunk burst, the
// WrittenAt fallback when ReadAt is unset, and the zero-timestamp guard.
func TestTee_DropWindowChannelFullCoalesceFallback(t *testing.T) {
	t.Parallel()

	// --- channel_full + coalescing: many same-instant chunk drops -> ONE window ---
	stamp := time.Unix(1700000000, 500000)
	tt, _, rec := newTestTee(t, 1<<30, 1, nil) // buf=1, never drained -> staging fills
	for i := 0; i < 20; i++ {
		tt.push(fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("x"), ReadAt: stamp})
	}
	time.Sleep(20 * time.Millisecond)
	if rec.count(DropChannelFull) == 0 {
		t.Fatalf("expected channel_full drops, got %v", rec.snapshot())
	}
	rs, tss := rec.atSnapshot()
	if len(rs) != 1 {
		t.Fatalf("coalescing failed: want exactly 1 window for the same-instant burst, got %d (%v)", len(rs), rs)
	}
	if rs[0] != DropChannelFull || !tss[0].Equal(stamp) {
		t.Fatalf("want one channel_full window at %v, got %v / %v", stamp, rs, tss)
	}

	// --- distinct operations (>1ms apart) each keep their own window ---
	// Use per_conn_cap with OVERSIZED chunks (len > cap) for deterministic
	// drops that DO record windows: a chunk bigger than the cap always trips
	// per_conn_cap on the calling goroutine (bytes.Load()+n > cap, n > cap),
	// independent of the drain goroutine. memory_pressure/paused no longer
	// record windows, and channel_full races the drain.
	tt2, _, rec2 := newTestTee(t, 4, 16, nil) // cap=4; 5-byte chunks always trip
	base := time.Unix(1700000000, 0)
	for i := 0; i < 6; i++ {
		ts := base.Add(time.Duration(i) * 2 * time.Millisecond) // 2ms apart
		if tt2.push(fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("yyyyy"), ReadAt: ts}) {
			t.Fatalf("oversized push must trip per_conn_cap")
		}
	}
	rs2, _ := rec2.atSnapshot()
	if len(rs2) != 6 {
		t.Fatalf("distinct 2ms-apart drops must each record (no coalescing): want 6 windows, got %d", len(rs2))
	}
	for _, r := range rs2 {
		if r != DropPerConnCap {
			t.Fatalf("want per_conn_cap windows, got %v", rs2)
		}
	}

	// --- WrittenAt fallback when ReadAt is zero (dest-side pre-forward stamp) ---
	tt3, _, rec3 := newTestTee(t, 4, 16, nil)
	wStamp := time.Unix(1700000001, 777)
	if tt3.push(fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("zzzzz"), WrittenAt: wStamp}) {
		t.Fatalf("oversized push must trip per_conn_cap")
	}
	if rs3, ts3 := rec3.atSnapshot(); len(rs3) != 1 || rs3[0] != DropPerConnCap || !ts3[0].Equal(wStamp) {
		t.Fatalf("WrittenAt fallback: want one per_conn_cap window at %v, got %v / %v", wStamp, rs3, ts3)
	}

	// --- zero-timestamp chunk records NO window (even for a recorded reason) ---
	tt4, _, rec4 := newTestTee(t, 4, 16, nil)
	if tt4.push(fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("qqqqq")}) {
		t.Fatalf("oversized push must trip per_conn_cap")
	}
	if rs4, _ := rec4.atSnapshot(); len(rs4) != 0 {
		t.Fatalf("zero-ts chunk must record no window, got %v", rs4)
	}
}

func TestTee_ClosePreventsSendPanic(t *testing.T) {
	t.Parallel()
	tt := newTee(fakeconn.FromClient, 1<<20, 4, nil, nil, nil, nil)

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
	tt := newTee(fakeconn.FromClient, 1<<20, 4, nil, nil, nil, nil)
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
		tt := newTee(fakeconn.FromClient, 1<<30, 4, nil, rec.record, rec.recordAt, nil)

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
