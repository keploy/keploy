package relay

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.uber.org/zap"
)

// newTestTee constructs a tee with reasonable defaults for tests.
// cap is expressed in bytes; buf is the channel capacity.
const testStallGrace = 150 * time.Millisecond

func newTestTee(t *testing.T, capBytes int64, buf int, memCheck func() bool) (*tee, func(reason string), *dropRecorder) {
	t.Helper()
	rec := &dropRecorder{}
	t2 := newTee(fakeconn.FromClient, capBytes, buf, testStallGrace, memCheck, rec.record, nil)
	// A consumer that never goes away: these tests exercise queueing and
	// delivery, so drain must never take the abandon branch. Tests that
	// simulate a departed parser build their own signal (see
	// newTestTeeWithConsumer).
	t2.start(make(chan struct{}))
	t.Cleanup(func() {
		t2.close()
		t2.waitDone()
	})
	return t2, rec.record, rec
}

// newTestTeeWithConsumer is newTestTee plus control over the consumer's
// liveness: closing the returned channel is what a parser abandoning its
// FakeConn looks like to the tee.
func newTestTeeWithConsumer(t *testing.T, capBytes int64, buf int) (*tee, *dropRecorder, chan struct{}) {
	t.Helper()
	rec := &dropRecorder{}
	gone := make(chan struct{})
	t2 := newTee(fakeconn.FromClient, capBytes, buf, testStallGrace, nil, rec.record, nil)
	t2.start(gone)
	t.Cleanup(func() {
		t2.close()
		t2.waitDone()
	})
	return t2, rec, gone
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
		tt := newTee(fakeconn.FromClient, 1<<30, 1, testStallGrace, nil, rec.record, nil) // buf=1 forces spill
		tt.start(make(chan struct{}))                                                     // consumer stays alive for this test

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
	tt := newTee(fakeconn.FromClient, 1<<20, 4, testStallGrace, nil, nil, nil)
	tt.start(make(chan struct{})) // consumer stays alive for this test

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
	tt := newTee(fakeconn.FromClient, 1<<20, 4, testStallGrace, nil, nil, nil)
	tt.start(make(chan struct{})) // consumer stays alive for this test
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
		tt := newTee(fakeconn.FromClient, 1<<30, 4, testStallGrace, nil, rec.record, nil)
		tt.start(make(chan struct{})) // consumer stays alive for this test

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

// TestTee_SlowConsumerLosesNothing is the stress property.
//
// A parser that is merely behind — a boot burst, a big decode, a loaded
// node — must receive every chunk no matter how long the whole drain takes.
// The delivery wait is therefore bounded by STALLED time, not elapsed time:
// each time the consumer takes anything the wait restarts. A consumer that
// crawls for many multiples of the grace still loses nothing.
func TestTee_SlowConsumerLosesNothing(t *testing.T) {
	t.Parallel()
	tt, rec, _ := newTestTeeWithConsumer(t, 1<<30, 1) // buf=1 → every send waits

	const n = 40
	for i := 0; i < n; i++ {
		if !tt.push(mkChunk(fmt.Sprintf("c%03d", i))) {
			t.Fatalf("push %d refused", i)
		}
	}
	tt.close()

	// Drain slower than the grace on purpose: total time far exceeds one
	// window, but no single gap does.
	var got []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for c := range tt.readCh() {
			got = append(got, string(c.Bytes))
			time.Sleep(testStallGrace / 3)
		}
	}()
	<-done

	if len(got) != n {
		t.Fatalf("slow consumer lost data: got %d chunks, want %d (drops=%v)", len(got), n, rec.snapshot())
	}
	for i, s := range got {
		if want := fmt.Sprintf("c%03d", i); s != want {
			t.Fatalf("out of order at %d: got %q want %q", i, s, want)
		}
	}
	if d := tt.dropCount(); d != 0 {
		t.Errorf("dropCount = %d, want 0 for a consumer that kept progressing", d)
	}
}

// TestTee_GoneConsumerIsAbandonedAtOnce pins the two halves of the "gone"
// verdict: it is reached without waiting out the grace, and it applies to
// the whole connection rather than being re-decided per chunk.
func TestTee_GoneConsumerIsAbandonedAtOnce(t *testing.T) {
	t.Parallel()
	tt, rec, gone := newTestTeeWithConsumer(t, 1<<30, 1)

	for i := 0; i < 200; i++ {
		tt.push(mkChunk(fmt.Sprintf("c%03d", i)))
	}

	start := time.Now()
	close(gone) // the parser closed its FakeConn
	tt.close()
	tt.waitDone()
	elapsed := time.Since(start)

	if elapsed > testStallGrace {
		t.Errorf("took %v to release a departed consumer; the close signal should not wait out the grace (%v)",
			elapsed, testStallGrace)
	}
	// One report for the connection, not one per abandoned chunk.
	if n := rec.count(DropConsumerGone); n != 1 {
		t.Errorf("consumer_gone reported %d times, want exactly 1 for the whole remainder", n)
	}
	if d := tt.dropCount(); d == 0 {
		t.Error("abandoned chunks were not counted; loss must be visible, not silent")
	}
}

// TestTee_StalledConsumerCostsOneGraceNotOnePerChunk is the regression guard
// for the teardown-latency trap.
//
// Arming the wait per chunk is what keeps a slow-but-progressing consumer
// whole, but if the verdict is re-decided per chunk then a consumer that has
// actually stopped costs grace × queue-depth — hundreds of queued chunks turn
// teardown into minutes, which is exactly why bounding the whole flush was
// tempting. Deciding once and abandoning the remainder gets both: this must
// finish in about ONE grace regardless of how much is queued.
func TestTee_StalledConsumerCostsOneGraceNotOnePerChunk(t *testing.T) {
	t.Parallel()
	const queued = 200
	tt, rec, _ := newTestTeeWithConsumer(t, 1<<30, 1) // consumer never reads

	for i := 0; i < queued; i++ {
		tt.push(mkChunk(fmt.Sprintf("c%03d", i)))
	}

	start := time.Now()
	tt.close()
	tt.waitDone()
	elapsed := time.Since(start)

	// Per-chunk re-decision would be ~queued × grace; allow generous slack
	// for scheduling while still failing that shape by orders of magnitude.
	if limit := 4 * testStallGrace; elapsed > limit {
		t.Errorf("teardown took %v with %d chunks queued (limit %v): the stall verdict is being "+
			"re-taken per chunk instead of once for the connection", elapsed, queued, limit)
	}
	if n := rec.count(DropConsumerGone); n != 1 {
		t.Errorf("consumer_gone reported %d times, want exactly 1", n)
	}
	// Conservation: every chunk is either delivered or counted as lost, and
	// the tally is the ONLY way that loss becomes visible. os/exec makes the
	// same point the hard way — when WaitDelay fires it returns ErrWaitDelay
	// even for a process that exited 0, because a capture that may be
	// incomplete must never be reported as clean. A tally of 1 when 199 were
	// abandoned is a silent truncation, and a silently truncated recording
	// yields an unreplayable mock.
	delivered := 0
	for range tt.readCh() { // out is closed by now; this drains its buffer
		delivered++
	}
	if got := delivered + int(tt.dropCount()); got != queued {
		t.Errorf("chunk accounting does not balance: %d delivered + %d dropped = %d, want %d — "+
			"undelivered chunks are going unreported", delivered, tt.dropCount(), got, queued)
	}
}

// TestTee_PushRefusedAfterAbandon is the C1 regression guard.
//
// abandon() ends the drain, so from that moment nothing will ever deliver a
// queued chunk. If push kept succeeding it would append to a queue with no
// consumer and report TRUE for a chunk that is already lost — worse than the
// leak this path exists to fix, because the caller uses that return to decide
// the chunk was teed and to arm the supervisor's pending-work watchdog, whose
// eventual hang verdict abandons the connection's recording wholesale.
func TestTee_PushRefusedAfterAbandon(t *testing.T) {
	t.Parallel()
	tt, _, gone := newTestTeeWithConsumer(t, 1<<30, 1)

	for i := 0; i < 20; i++ {
		tt.push(mkChunk("x"))
	}
	close(gone) // parser departed → drain abandons and returns
	tt.waitDone()

	before := tt.dropCount()
	accepted := 0
	for i := 0; i < 10; i++ {
		if tt.push(mkChunk("after")) {
			accepted++
		}
	}
	if accepted != 0 {
		t.Errorf("push accepted %d chunks after the connection was abandoned; "+
			"they are queued behind a drain that has already returned, so each one is "+
			"silently lost while the caller is told it was recorded", accepted)
	}
	if got := tt.dropCount(); got != before {
		t.Errorf("dropCount moved %d -> %d: refused pushes should not be counted twice, "+
			"the abandonment was already reported once with its total", before, got)
	}
}

// TestTee_LiveConnectionIsNotAbandonedOnStall is the H1 regression guard.
//
// The stall bound exists only so relay teardown (which waits on waitDone)
// cannot hang. Applying it MID-connection would throw away a live recording
// and close out under a parser that is still reading, handing it a premature
// io.EOF — a net-new loss path that the fixed-slot design never had. A parser
// that is simply behind for longer than the grace must lose nothing.
func TestTee_LiveConnectionIsNotAbandonedOnStall(t *testing.T) {
	t.Parallel()
	tt, rec, _ := newTestTeeWithConsumer(t, 1<<30, 1) // buf=1: the send blocks

	const n = 6
	for i := 0; i < n; i++ {
		tt.push(mkChunk(fmt.Sprintf("c%03d", i)))
	}

	// Stall well past the grace WITHOUT closing the tee: the parser is alive,
	// just busy.
	time.Sleep(3 * testStallGrace)

	if d := tt.dropCount(); d != 0 {
		t.Fatalf("dropCount = %d after a mid-connection stall: a live connection was "+
			"abandoned (reasons=%v)", d, rec.snapshot())
	}

	// The parser comes back and must still receive everything.
	got := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range tt.readCh() {
			got++
		}
	}()
	tt.close()
	<-done

	if got != n {
		t.Errorf("slow-but-alive parser received %d of %d chunks; the stall bound must not "+
			"apply mid-connection", got, n)
	}
}

// TestTee_DeliversWhenOutHasRoomEvenIfConsumerGone pins drain's non-blocking
// pre-try.
//
// Once consumerGone is closed, a plain select between "send to out" and "give
// up" picks at RANDOM whenever both are ready — so roughly half of the chunks
// out still had room for would be abandoned. The pre-try is what makes
// delivery win whenever it is possible at all; without it this test loses
// about half its data.
func TestTee_DeliversWhenOutHasRoomEvenIfConsumerGone(t *testing.T) {
	t.Parallel()
	const n = 200
	tt, _, gone := newTestTeeWithConsumer(t, 1<<30, n*4) // out has ample room

	close(gone) // consumer "gone", but out can still accept everything
	for i := 0; i < n; i++ {
		tt.push(mkChunk(fmt.Sprintf("c%03d", i)))
	}
	tt.close()
	tt.waitDone()

	delivered := 0
	for range tt.readCh() {
		delivered++
	}
	if delivered != n {
		t.Errorf("delivered %d of %d chunks that out had room for; without the "+
			"non-blocking pre-try the select coin-flips against a closed consumerGone "+
			"and discards deliverable data", delivered, n)
	}
}

// TestDrainingAChunkReturnsCapacity pins the queue's core accounting invariant.
//
// qBytes must fall when a chunk leaves the queue. Without that, PerConnCap
// stops being an INSTANTANEOUS bound and silently becomes a CUMULATIVE one:
// once a connection has carried its cap worth of bytes in total, every further
// push is refused with per_conn_cap and the connection records nothing for the
// rest of its life — however little is actually queued.
func TestDrainingAChunkReturnsCapacity(t *testing.T) {
	t.Parallel()
	const chunk = "0123456789" // 10 bytes
	// Room for ~3 chunks at once, but far less than the total we will push.
	tt, rec, _ := newTestTeeWithConsumer(t, 3*int64(len(chunk)), 64)

	// Drain continuously so the queue never accumulates.
	drained := make(chan int, 1)
	go func() {
		n := 0
		for range tt.readCh() {
			n++
		}
		drained <- n
	}()

	const total = 200 // 2000 bytes through a 30-byte cap
	sent := 0
	for i := 0; i < total; i++ {
		if tt.push(mkChunk(chunk)) {
			sent++
		}
		time.Sleep(time.Millisecond) // let the drain keep up
	}
	tt.close()
	got := <-drained

	if sent < total/2 {
		t.Errorf("only %d of %d pushes were admitted through a %d-byte cap (drops=%v): capacity is "+
			"not being returned when a chunk drains, so the cap has become cumulative rather than "+
			"instantaneous and the connection stops recording", sent, total, 3*len(chunk), rec.snapshot())
	}
	if got != sent {
		t.Errorf("delivered %d but admitted %d", got, sent)
	}
}

// TestConfigSuppliesTheStallGrace pins the plumbing from Config to the tee.
//
// The production caller (proxy.New) passes the CLAMPED
// config.Record.RecordBuffer.ConsumerStallGrace, and keploy.yml ships an
// explicit 2s, so the common path no longer relies on this clause — but the
// zero path is still reachable and still
// load-bearing: an operator who writes 0s, a programmatically built Config, and
// any enterprise SetDefaultConfig string that omits the key all arrive here as
// zero. A zero grace makes the teardown wait expire instantly: a parser that
// resumes reading a moment later has already had its remaining chunks
// abandoned — the boot "no mocks" loss this change exists to remove,
// reintroduced by deleting one line.
func TestConfigSuppliesTheStallGrace(t *testing.T) {
	t.Parallel()

	t.Run("zero resolves to the default", func(t *testing.T) {
		got := Config{}.withDefaults()
		if got.ConsumerStallGrace != DefaultConsumerStallGrace {
			t.Errorf("ConsumerStallGrace = %v, want %v: without this the tee runs with a zero "+
				"grace and abandons a briefly-stalled parser's chunks immediately",
				got.ConsumerStallGrace, DefaultConsumerStallGrace)
		}
	})

	t.Run("an explicit value is preserved", func(t *testing.T) {
		want := 1234 * time.Millisecond
		if got := (Config{ConsumerStallGrace: want}).withDefaults(); got.ConsumerStallGrace != want {
			t.Errorf("ConsumerStallGrace = %v, want %v (caller's value must win)", got.ConsumerStallGrace, want)
		}
	})

	t.Run("a zero grace loses a briefly-stalled parser's chunks", func(t *testing.T) {
		// Demonstrates WHY the clause above matters, so the guard reads as a
		// consequence rather than a style rule.
		rec := &dropRecorder{}
		tt := newTee(fakeconn.FromClient, 1<<30, 1, 0 /* zero grace */, nil, rec.record, nil)
		tt.start(make(chan struct{}))
		for i := 0; i < 50; i++ {
			tt.push(mkChunk("x"))
		}
		tt.close()
		tt.waitDone()
		if tt.dropCount() == 0 {
			t.Skip("zero grace did not abandon here; timing-dependent, nothing to assert")
		}
		// With a real grace the same shape loses nothing — see
		// TestTee_SlowConsumerLosesNothing.
		t.Logf("zero grace abandoned %d chunks; the withDefaults clause is what prevents this", tt.dropCount())
	})
}

// TestNewWiresTheStallGraceIntoBothTees closes the last link in the chain.
//
// TestConfigSuppliesTheStallGrace proves Config resolves the value; this proves
// New actually hands it to the tees. Both halves are needed: passing 0 here
// while withDefaults is correct still yields a zero grace at the only place it
// is read, and no behavioural test notices because every tee test constructs
// newTee directly with an explicit grace.
func TestNewWiresTheStallGraceIntoBothTees(t *testing.T) {
	t.Parallel()
	const want = 4321 * time.Millisecond

	c1, c2 := net.Pipe()
	d1, d2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close(); _ = d1.Close(); _ = d2.Close() })

	r := New(Config{ConsumerStallGrace: want, Logger: zap.NewNop()}, c1, d1)
	t.Cleanup(func() { r.teeC2D.close(); r.teeD2C.close() })

	if got := r.teeC2D.stallGrace; got != want {
		t.Errorf("client→dest tee stallGrace = %v, want %v: Config's value is not reaching the tee, "+
			"so the teardown wait runs at whatever New passed instead", got, want)
	}
	if got := r.teeD2C.stallGrace; got != want {
		t.Errorf("dest→client tee stallGrace = %v, want %v", got, want)
	}

	// And the zero-config path must land on the default, not on zero.
	r2 := New(Config{Logger: zap.NewNop()}, c2, d2)
	t.Cleanup(func() { r2.teeC2D.close(); r2.teeD2C.close() })
	if got := r2.teeC2D.stallGrace; got != DefaultConsumerStallGrace {
		t.Errorf("zero-config stallGrace = %v, want %v", got, DefaultConsumerStallGrace)
	}
}
