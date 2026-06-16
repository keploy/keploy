package manager

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestSetMemoryPressureClearsBufferedMocksAndDropsNewOnes(t *testing.T) {
	t.Parallel()

	mgr := &SyncMockManager{
		buffer: []*models.Mock{
			{
				Spec: models.MockSpec{ReqTimestampMock: time.Now()},
			},
			{
				Spec: models.MockSpec{ReqTimestampMock: time.Now()},
			},
		},
	}
	oldBuffer := mgr.buffer

	mgr.SetMemoryPressure(true)
	if len(mgr.buffer) != 0 {
		t.Fatalf("expected memory pressure to clear buffered mocks, got %d items", len(mgr.buffer))
	}
	if cap(mgr.buffer) != defaultMockBufferCapacity {
		t.Fatalf("expected buffer capacity to reset to %d, got %d", defaultMockBufferCapacity, cap(mgr.buffer))
	}
	for i, mock := range oldBuffer {
		if mock != nil {
			t.Fatalf("expected cleared buffer entry %d to be nil", i)
		}
	}

	mgr.AddMock(&models.Mock{Spec: models.MockSpec{ReqTimestampMock: time.Now()}})
	if len(mgr.buffer) != 0 {
		t.Fatalf("expected memory pressure to drop new mocks, got %d buffered items", len(mgr.buffer))
	}

	mgr.SetMemoryPressure(false)
	mgr.AddMock(&models.Mock{Spec: models.MockSpec{ReqTimestampMock: time.Now()}})
	if len(mgr.buffer) != 1 {
		t.Fatalf("expected buffer to accept mocks after recovery, got %d buffered items", len(mgr.buffer))
	}
}

// TestAddMockAfterCloseDropsSilently exercises the shutdown path:
// once CloseOutChan has fired, AddMock must silently drop (no panic,
// no send) instead of racing a closed channel. Previously this was
// guarded by recover(); the refactor relies on outChanClosed being
// read under the same lock as the send so there is nothing to
// recover from.
func TestAddMockAfterCloseDropsSilently(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 1)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)
	mgr.CloseOutChan()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("AddMock must not panic after CloseOutChan; got %v", r)
		}
	}()

	mgr.AddMock(&models.Mock{Spec: models.MockSpec{ReqTimestampMock: time.Now()}})

	// Mock must not have been buffered either — we aren't after
	// firstReqSeen in this test, but outChan is closed so the
	// send path bails early. ResolveRange-style reemission of the
	// dropped mock is out of scope here.
	if len(mgr.buffer) != 0 {
		t.Fatalf("post-close AddMock should not buffer; buffer=%d", len(mgr.buffer))
	}
}

// TestCloseOutChanRacesAddMock is the direct regression for the
// -race report seen on keploy#4045 CI: 8 goroutines calling AddMock
// concurrently with a midstream CloseOutChan. With sendToOutChan's
// RLock, no send can interleave the close. Must stay clean under
// `go test -race`.
func TestCloseOutChanRacesAddMock(t *testing.T) {
	t.Parallel()

	const senders = 8
	const sendsPerSender = 500

	ch := make(chan *models.Mock, 1024)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)

	// Each goroutine owns its mock slice to avoid racing on the same
	// *models.Mock's TestModeInfo — AddMock calls DeriveLifetime on
	// entry and mutates the struct, so sharing a pointer across
	// senders would race even though the manager's internal state
	// stays serialized.
	var wg sync.WaitGroup
	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < sendsPerSender; j++ {
				mgr.AddMock(&models.Mock{Spec: models.MockSpec{ReqTimestampMock: time.Now()}})
			}
		}()
	}

	time.Sleep(time.Millisecond)
	mgr.CloseOutChan()
	// Second close must be a no-op, not a panic.
	mgr.CloseOutChan()

	wg.Wait()
	drainChan(t, ch, senders*sendsPerSender)
}

// TestCloseOutChanRacesResolveRange is the ResolveRange equivalent.
// ResolveRange's send loop previously ran outside any lock after
// releasing m.mu, so a proxy shutdown calling CloseOutChan during
// dedup processing would reproduce the same data race. The refactor
// routes ResolveRange's sends through sendToOutChan; that must stay
// -race clean under the same concurrent Close pattern.
func TestCloseOutChanRacesResolveRange(t *testing.T) {
	t.Parallel()

	const resolvers = 4
	const mocksPerResolver = 200

	ch := make(chan *models.Mock, 1024)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)

	// Seed the buffer across a known time range so every ResolveRange
	// call finds at least one mock to emit.
	now := time.Now()
	for i := 0; i < resolvers*mocksPerResolver; i++ {
		mgr.mu.Lock()
		mgr.buffer = append(mgr.buffer, &models.Mock{
			Spec: models.MockSpec{ReqTimestampMock: now.Add(time.Duration(i) * time.Microsecond)},
		})
		mgr.mu.Unlock()
	}

	start := now
	end := now.Add(time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < resolvers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < mocksPerResolver; j++ {
				mgr.ResolveRange(start, end, "t", true, false)
			}
		}()
	}

	time.Sleep(time.Millisecond)
	mgr.CloseOutChan()

	wg.Wait()
	drainChan(t, ch, resolvers*mocksPerResolver)
}

// TestSetOutputChannelAfterCloseReopens covers the TOCTOU sequence
// surfaced in code review: CloseOutChan closes ch1, SetOutputChannel
// binds a fresh ch2 (clearing outChanClosed), and a subsequent
// AddMock should send on ch2 — not on the stale closed ch1. The
// refactor pulls outChan into sendToOutChan under the same RLock
// that checks outChanClosed, so the local-variable staleness bug
// cannot recur.
func TestSetOutputChannelAfterCloseReopens(t *testing.T) {
	t.Parallel()

	ch1 := make(chan *models.Mock, 1)
	ch2 := make(chan *models.Mock, 1)

	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch1)
	mgr.CloseOutChan()

	mgr.SetOutputChannel(ch2)
	mock := &models.Mock{Spec: models.MockSpec{ReqTimestampMock: time.Now()}}
	mgr.AddMock(mock)

	select {
	case got := <-ch2:
		if got != mock {
			t.Fatalf("expected mock on ch2, got something else")
		}
	default:
		t.Fatalf("expected AddMock to forward to ch2 after SetOutputChannel")
	}

	// ch1 must remain empty — no stale send should have landed on it.
	select {
	case got, ok := <-ch1:
		if ok {
			t.Fatalf("unexpected mock on old (closed) ch1: %v", got)
		}
	default:
	}
}

// TestSendConfigMockDropsAfterClose locks in the DNS-path contract:
// after CloseOutChan, SendConfigMock is a no-op (no panic, no send).
// DNS is the only caller today and its dedupe map guarantees each
// (name, qtype) is sent at most once per run, so a silent drop
// during shutdown is the correct outcome.
func TestSendConfigMockDropsAfterClose(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 1)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)
	mgr.CloseOutChan()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("SendConfigMock must not panic after close; got %v", r)
		}
	}()

	mgr.SendConfigMock(&models.Mock{Kind: models.DNS})
}

// TestSendConfigMockIgnoresFirstReqSeen pins the semantic split
// from AddMock: DNS records must always forward immediately,
// including after SetFirstRequestSignaled. AddMock would buffer
// here; SendConfigMock must not.
func TestSendConfigMockIgnoresFirstReqSeen(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 1)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)
	mgr.SetFirstRequestSignaled()

	mock := &models.Mock{Kind: models.DNS}
	mgr.SendConfigMock(mock)

	select {
	case got := <-ch:
		if got != mock {
			t.Fatalf("expected SendConfigMock to forward even after firstReqSeen")
		}
	default:
		t.Fatalf("SendConfigMock dropped the mock despite outChan being open")
	}
}

// TestCloseOutChanRacesSendConfigMock mirrors the AddMock race test
// for the DNS path so the guard is exercised end-to-end.
func TestCloseOutChanRacesSendConfigMock(t *testing.T) {
	t.Parallel()

	const senders = 4
	const sendsPerSender = 500

	ch := make(chan *models.Mock, 1024)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)

	mock := &models.Mock{Kind: models.DNS}
	var wg sync.WaitGroup
	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < sendsPerSender; j++ {
				mgr.SendConfigMock(mock)
			}
		}()
	}

	time.Sleep(time.Millisecond)
	mgr.CloseOutChan()

	wg.Wait()
	drainChan(t, ch, senders*sendsPerSender)
}

// TestSetOutputChannelSamePointerAfterCloseKeepsClosed locks in
// the idempotence rule: re-binding the same channel pointer after
// CloseOutChan must NOT reset outChanClosed. DNS recordDNSMock
// calls SetOutputChannel(session.MC) on every mock; if that reset
// the flag, a post-shutdown DNS mock would send on the closed
// session.MC and panic. Reviewed on keploy#4045 / ee0332e.
func TestSetOutputChannelSamePointerAfterCloseKeepsClosed(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 1)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)
	mgr.CloseOutChan()

	// Re-bind the SAME channel. Must be a no-op — not a reopen.
	mgr.SetOutputChannel(ch)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("SendConfigMock after same-pointer re-bind post-close must not panic; got %v", r)
		}
	}()
	mgr.SendConfigMock(&models.Mock{Kind: models.DNS})
	// Nothing should have been written to ch (it's closed).
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("unexpected send on closed channel after same-pointer re-bind")
		}
	default:
	}
}

// TestResolveRangePostCloseRetainsBuffer documents the
// post-revert contract: after CloseOutChan fires, ResolveRange
// treats outChan as "unbound" and retains matching mocks in the
// buffer (they will never drain, but we accept the small leak in
// exchange for dropping the pre-drop branch that was suspected
// of regressing #4045 Mongo Fuzzer record_build_replay_latest).
// In-window mocks are retained regardless of age (the long-running-
// test contract — see TestResolveRangeLongRunningWindowKeepsOldMocks);
// out-of-window stale mocks are still time-capped by the 7-second
// cutoff in the unmatched branch.
func TestResolveRangePostCloseRetainsBuffer(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 4)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)

	now := time.Now()
	match := &models.Mock{Spec: models.MockSpec{ReqTimestampMock: now}}
	mgr.mu.Lock()
	mgr.buffer = append(mgr.buffer, match)
	mgr.mu.Unlock()

	mgr.CloseOutChan()
	mgr.ResolveRange(now.Add(-time.Second), now.Add(time.Second), "t", true, false)

	mgr.mu.Lock()
	remaining := len(mgr.buffer)
	mgr.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("expected 1 mock retained in buffer after post-close ResolveRange; got %d", remaining)
	}
}

// TestResolveRangeLongRunningWindowKeepsOldMocks is the
// regression test for the Mongo Fuzzer record_build_replay_build
// failure caused by routing V2 EmitMock through syncMock (#4122).
//
// The fuzzer's curl POST /run takes ~56 s to complete 10 000 mongo
// ops, so by the time ResolveRange fires at request resolution the
// per-test mongo mocks captured in the first ~49 s are all older
// than 7 s. The cutoff used to be checked BEFORE the window match,
// which dropped them despite their being in-window — leaving replay
// without the mongo handshake mocks and producing a connection-pool
// error at driver init. The fix re-orders the loop to evaluate the
// window match first, so an in-window mock is kept regardless of
// age; only un-matched mocks are subject to the stale cutoff.
func TestResolveRangeLongRunningWindowKeepsOldMocks(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 4)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)

	// Simulate a 60-second test window that starts 60 s ago.
	now := time.Now()
	windowStart := now.Add(-60 * time.Second)
	windowEnd := now

	// Mock at +5 s into the window — older than the 7-second cutoff
	// (~55 s old by now) but legitimately in-window.
	oldInWindow := &models.Mock{
		Spec: models.MockSpec{ReqTimestampMock: windowStart.Add(5 * time.Second)},
	}
	// Mock at -10 s before the window — out of window AND old. Must
	// be dropped by the stale cutoff.
	staleOutWindow := &models.Mock{
		Spec: models.MockSpec{ReqTimestampMock: windowStart.Add(-10 * time.Second)},
	}
	// Mock 3 s in the future relative to now — out of window but
	// recent. Must be retained for a possible future window match.
	freshOutWindow := &models.Mock{
		Spec: models.MockSpec{ReqTimestampMock: now.Add(3 * time.Second)},
	}

	mgr.mu.Lock()
	mgr.buffer = append(mgr.buffer, oldInWindow, staleOutWindow, freshOutWindow)
	mgr.mu.Unlock()

	mgr.ResolveRange(windowStart, windowEnd, "test-1", true, false)

	// Drain anything sent to the outChan.
	var sent []*models.Mock
loop:
	for {
		select {
		case m := <-ch:
			sent = append(sent, m)
		default:
			break loop
		}
	}

	if len(sent) != 1 || sent[0] != oldInWindow {
		t.Fatalf("expected the in-window mock to be flushed despite being older than 7s; sent=%d", len(sent))
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.buffer) != 1 || mgr.buffer[0] != freshOutWindow {
		t.Fatalf("expected only freshOutWindow retained in buffer; got len=%d", len(mgr.buffer))
	}
}

// TestAddMockSessionLifetimeBuffersAfterFirstReqSeen pins the
// post-firstReqSeen routing for session-lifetime mocks. AddMock
// must NOT hoist them straight to outChan: that would interleave
// handshake mocks ahead of the per-test mocks emitted at end-of-
// test from the buffer, and replay's connection-keyed FIFO matcher
// stalls on the resulting out-of-order stream. The expected path
// is: buffer the session mock now, and let ResolveRange's lifetime
// carve-out drain it to outChan in arrival order, exempt from the
// per-test window match and the 7 s stale cutoff. Regression coverage
// for the run_fuzzer_linux / Mongo Fuzzer (record_build_replay_build)
// hang on the bypass attempt of PR #4130.
func TestAddMockSessionLifetimeBuffersAfterFirstReqSeen(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 4)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)
	// Past the gate: per-test mocks now buffer; session mocks must too.
	mgr.SetFirstRequestSignaled()

	sessionMock := &models.Mock{
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "config"},
			ReqTimestampMock: time.Now(),
		},
	}
	mgr.AddMock(sessionMock)

	select {
	case got := <-ch:
		t.Fatalf("session mock unexpectedly forwarded to outChan: %p", got)
	default:
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.buffer) != 1 || mgr.buffer[0] != sessionMock {
		t.Fatalf("expected session mock buffered post-firstReqSeen; buffer=%d", len(mgr.buffer))
	}
}

// TestAddMockConnectionLifetimeBuffersAfterFirstReqSeen is the
// LifetimeConnection counterpart. Postgres v3 prepared-statement
// PREPARE mocks land here: they're tagged "type: connection" with
// a connID and must follow the same buffer-then-carve-out path so
// arrival order is preserved relative to per-test mocks emitted on
// the same connection.
func TestAddMockConnectionLifetimeBuffersAfterFirstReqSeen(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 4)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)
	mgr.SetFirstRequestSignaled()

	connMock := &models.Mock{
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"type":   "connection",
				"connID": "c-42",
			},
			ReqTimestampMock: time.Now(),
		},
	}
	mgr.AddMock(connMock)

	select {
	case got := <-ch:
		t.Fatalf("connection mock unexpectedly forwarded to outChan: %p", got)
	default:
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.buffer) != 1 || mgr.buffer[0] != connMock {
		t.Fatalf("expected connection mock buffered post-firstReqSeen; buffer=%d", len(mgr.buffer))
	}
}

// TestResolveRangeFlushesBufferedSessionMocksInArrivalOrder is the
// pure regression test for the Mongo Fuzzer hang on the bypass
// attempt. Buffer interleaves session and per-test mocks in the
// order [session1, perTest1, session2, perTest2, perTest3] and
// invokes a per-test ResolveRange whose window covers the per-test
// mocks. The expectation: outChan receives mocks in the SAME
// interleaved order, NOT all-sessions-then-all-per-tests. Replay's
// connection-keyed FIFO matcher requires this ordering — with the
// pre-fix bypass, session mocks landed on outChan ahead of any
// per-test mocks queued in the buffer, the connection's mock stream
// went out of order, and replay stalled on the last batch of ops.
func TestResolveRangeFlushesBufferedSessionMocksInArrivalOrder(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 8)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)
	mgr.SetFirstRequestSignaled()

	now := time.Now()
	mkSession := func(id string, ts time.Time) *models.Mock {
		m := &models.Mock{
			Kind: models.Mongo,
			Name: id,
			Spec: models.MockSpec{
				Metadata:         map[string]string{"type": "config"},
				ReqTimestampMock: ts,
			},
		}
		m.DeriveLifetime()
		return m
	}
	mkPerTest := func(id string, ts time.Time) *models.Mock {
		return &models.Mock{
			Kind: models.Mongo,
			Name: id,
			Spec: models.MockSpec{ReqTimestampMock: ts},
		}
	}

	// Capture order: s1, p1, s2, p2, p3 — all timestamped inside
	// the test window so the per-test path matches them. The per-
	// test ResolveRange window must include every per-test mock's
	// timestamp; session mocks are exempt regardless.
	mocks := []*models.Mock{
		mkSession("s1", now.Add(-50*time.Millisecond)),
		mkPerTest("p1", now.Add(-40*time.Millisecond)),
		mkSession("s2", now.Add(-30*time.Millisecond)),
		mkPerTest("p2", now.Add(-20*time.Millisecond)),
		mkPerTest("p3", now.Add(-10*time.Millisecond)),
	}
	for _, m := range mocks {
		mgr.AddMock(m)
	}

	mgr.ResolveRange(now.Add(-100*time.Millisecond), now, "test-1", true, false)

	for _, want := range mocks {
		select {
		case got := <-ch:
			if got != want {
				t.Fatalf("outChan order broken: expected %s, got %s", want.Name, got.Name)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for mock %s on outChan", want.Name)
		}
	}
	select {
	case extra := <-ch:
		t.Fatalf("unexpected extra mock on outChan: %s", extra.Name)
	default:
	}
}

// TestAddMockPerTestStillBuffersAfterFirstReqSeen is the negative
// control for the carve-out: per-test mocks (zero Lifetime / no
// metadata tag) MUST still buffer once firstReqSeen has flipped, so
// ResolveRange can match them against the active test window. If
// the bypass leaks here the dedup/windowing contract collapses.
func TestAddMockPerTestStillBuffersAfterFirstReqSeen(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 4)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)
	mgr.SetFirstRequestSignaled()

	perTest := &models.Mock{
		Spec: models.MockSpec{ReqTimestampMock: time.Now()},
	}
	mgr.AddMock(perTest)

	select {
	case got := <-ch:
		t.Fatalf("per-test mock unexpectedly forwarded to outChan: %p", got)
	default:
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.buffer) != 1 || mgr.buffer[0] != perTest {
		t.Fatalf("expected per-test mock buffered post-firstReqSeen; buffer=%d", len(mgr.buffer))
	}
}

// TestResolveRangeKeepsSessionLifetimeMocksPastCutoff covers the
// ResolveRange safety net paired with the AddMock bypass. AddMock
// now forwards session-lifetime mocks directly to outChan when the
// channel is bound, so the buffer should generally not see them.
// The exception is the brief startup window where outChan is still
// nil at AddMock time — those session mocks land in the buffer.
// Without the lifetime carve-out in ResolveRange the post-#4130
// out-of-window cutoff would silently drop them once they aged
// past 7 s and outChan became bound. This test mirrors that
// scenario: pre-buffer a session mock with a >7 s ReqTimestampMock,
// invoke a per-test ResolveRange whose window doesn't include the
// mock's timestamp, and assert the mock is FLUSHED to outChan
// rather than dropped.
func TestResolveRangeKeepsSessionLifetimeMocksPastCutoff(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 4)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)

	// Mongo handshake captured ~10 s ago — well past the 7 s cutoff.
	// "type: config" is the on-disk tag DeriveLifetime maps to
	// LifetimeSession. Pre-derive on the test mock so the buffered
	// state matches what AddMock would have written.
	handshake := &models.Mock{
		Kind: models.Mongo,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "config"},
			ReqTimestampMock: time.Now().Add(-10 * time.Second),
		},
	}
	handshake.DeriveLifetime()
	if handshake.TestModeInfo.Lifetime != models.LifetimeSession {
		t.Fatalf("test fixture: expected LifetimeSession after DeriveLifetime, got %v", handshake.TestModeInfo.Lifetime)
	}

	// Pre-buffer it (simulates the unbound-at-AddMock startup path).
	mgr.mu.Lock()
	mgr.buffer = append(mgr.buffer, handshake)
	mgr.mu.Unlock()

	// Per-test ResolveRange with a window 1 s wide centred on now —
	// the handshake timestamp (-10 s) is well outside.
	now := time.Now()
	mgr.ResolveRange(now.Add(-500*time.Millisecond), now.Add(500*time.Millisecond), "test-1", true, false)

	// Session mock must have been flushed to outChan despite being
	// out-of-window AND past the 7 s cutoff.
	select {
	case got := <-ch:
		if got != handshake {
			t.Fatalf("expected session mock flushed to outChan; got different pointer")
		}
	default:
		t.Fatalf("expected session-lifetime mock to be flushed to outChan past cutoff; got nothing")
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.buffer) != 0 {
		t.Fatalf("expected empty buffer after flushing session mock; got %d", len(mgr.buffer))
	}
}

// TestResolveRangeRetainsSessionMocksWhenOutChanUnbound complements
// the cutoff exemption: when outChan is unbound at ResolveRange time
// (post-CloseOutChan, or before a fresh SetOutputChannel re-binds),
// session mocks must be RETAINED in the buffer rather than flushed
// (sendToOutChan would no-op silently and we'd lose them) or
// dropped. A later ResolveRange with the channel re-bound is
// expected to drain them.
func TestResolveRangeRetainsSessionMocksWhenOutChanUnbound(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 4)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)
	mgr.CloseOutChan() // outChanBound goes false, channel closed

	handshake := &models.Mock{
		Kind: models.Mongo,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "config"},
			ReqTimestampMock: time.Now().Add(-10 * time.Second),
		},
	}
	handshake.DeriveLifetime()

	mgr.mu.Lock()
	mgr.buffer = append(mgr.buffer, handshake)
	mgr.mu.Unlock()

	now := time.Now()
	mgr.ResolveRange(now.Add(-500*time.Millisecond), now.Add(500*time.Millisecond), "t", true, false)

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.buffer) != 1 || mgr.buffer[0] != handshake {
		t.Fatalf("expected session mock retained in buffer when outChan unbound; got len=%d", len(mgr.buffer))
	}
}

// TestSendToOutChanNoDropWhenDrainedWithinBudget exercises the
// fast-then-bounded-block path: once the fast-path slot is taken,
// a consumer that drains within sendBudget must let the blocked
// sender complete without bumping dropCount. This is the "normal
// scheduling jitter" case the bounded block was introduced for —
// the pre-fix silent-drop shipped a recording-loss flake in
// exactly this scenario, so the test lives to prevent regression.
func TestSendToOutChanNoDropWhenDrainedWithinBudget(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 1)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)

	// Prime the buffer slot so the next send goes down the
	// bounded-block branch rather than the fast path.
	ch <- &models.Mock{}

	// Drain one slot well inside sendBudget so the blocked send
	// succeeds. 50ms is comfortably under the 200ms budget while
	// still giving the goroutine time to park on the send.
	done := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		<-ch // drop the primer
		close(done)
	}()

	mock := &models.Mock{Spec: models.MockSpec{ReqTimestampMock: time.Now()}}
	mgr.sendToOutChan(mock)
	<-done

	if got := mgr.DropCount(); got != 0 {
		t.Fatalf("expected zero drops when consumer drains within budget, got %d", got)
	}

	// Real mock should have landed on the channel.
	select {
	case got := <-ch:
		if got != mock {
			t.Fatalf("expected bounded-block send to deliver mock; got different pointer")
		}
	default:
		t.Fatalf("bounded-block send should have delivered the mock after the primer was drained")
	}
}

// TestSendToOutChanDropsAfterBudget is the inverse: a consumer that
// never drains must cause exactly one drop per send after the
// budget, and dropCount must increment accordingly. Also asserts
// the first drop emits an Error log (the sampled, immediately-
// visible signal the pre-fix code lacked).
func TestSendToOutChanDropsAfterBudget(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 1)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)

	// Capture log output so we can verify the first-drop Error
	// actually fires — the whole point of promoting Warn->Error.
	core, logs := observer.New(zap.ErrorLevel)
	mgr.SetLogger(zap.New(core))

	// Fill the buffered slot; subsequent sends must exhaust the
	// budget and drop.
	ch <- &models.Mock{}

	// Use a tight send budget override via measuring elapsed time —
	// we can't stub sendBudget directly without adding a knob, so
	// just accept the 200ms wall-clock cost on a single send and
	// validate the semantics. 200ms * 1 send is fine for unit tests.
	start := time.Now()
	mgr.sendToOutChan(&models.Mock{Spec: models.MockSpec{ReqTimestampMock: time.Now()}})
	elapsed := time.Since(start)

	if elapsed < sendBudget {
		t.Fatalf("bounded-block send returned in %v, expected >= sendBudget (%v)", elapsed, sendBudget)
	}
	if got := mgr.DropCount(); got != 1 {
		t.Fatalf("expected dropCount=1 after budget exhaustion, got %d", got)
	}
	if logs.Len() != 1 {
		t.Fatalf("expected exactly one Error log on first drop, got %d", logs.Len())
	}
	entry := logs.All()[0]
	if entry.Level != zap.ErrorLevel {
		t.Fatalf("expected Error level on drop log, got %v", entry.Level)
	}
}

// TestSendToOutChanLoudOnFirstDrop validates the two-message-shape
// behaviour: the very first drop's Error carries the expanded
// "recording is now LOSSY" wording, and subsequent sampled
// emissions (n%1024 == 0) carry the TERSE default. Per Copilot
// review on PR #4176 the loud signal rides on the existing Error
// log on the n==1 branch rather than as a separate Warn — one
// clear actionable line at the moment capture goes lossy, no
// log-level duplication.
func TestSendToOutChanLoudOnFirstDrop(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 1)
	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	mgr.SetOutputChannel(ch)

	core, logs := observer.New(zap.ErrorLevel)
	mgr.SetLogger(zap.New(core))

	// Fill the slot so the next send hits the drop branch.
	ch <- &models.Mock{}

	mgr.sendToOutChan(&models.Mock{Spec: models.MockSpec{ReqTimestampMock: time.Now()}})

	if got := mgr.DropCount(); got != 1 {
		t.Fatalf("expected dropCount=1, got %d", got)
	}
	if logs.Len() != 1 {
		t.Fatalf("expected exactly one Error log on first drop, got %d", logs.Len())
	}
	first := logs.All()[0]
	if first.Level != zap.ErrorLevel {
		t.Fatalf("expected Error level, got %v", first.Level)
	}
	// The first-drop message must carry the operator-actionable
	// "LOSSY" wording — the whole point of the expanded n==1
	// message is to surface the actionable cliff signal *the moment*
	// capture stops being whole, so the operator can react before
	// the per-1024 sampler hides hundreds of subsequent drops.
	if !strings.Contains(first.Message, "LOSSY") {
		t.Errorf("first-drop Error missing LOSSY wording: %q", first.Message)
	}

	// Now drive dropCount up to the next sampler milestone (1024)
	// without paying the per-drop wall-clock cost, then walk
	// sendToOutChan once more to fire the n==1024 branch. The
	// emitted message must be the TERSE default — without "LOSSY"
	// — so the sampled telemetry doesn't drown a stuck consumer's
	// goroutine in the long actionable string.
	mgr.dropCount.Store(sendDropSampleRate - 1) // next Add brings n to 1024
	mgr.sendToOutChan(&models.Mock{Spec: models.MockSpec{ReqTimestampMock: time.Now()}})
	if logs.Len() != 2 {
		t.Fatalf("expected a second Error log at n==%d, got total=%d",
			sendDropSampleRate, logs.Len())
	}
	second := logs.All()[1]
	if second.Level != zap.ErrorLevel {
		t.Fatalf("expected Error level on second sampled emission, got %v", second.Level)
	}
	if strings.Contains(second.Message, "LOSSY") {
		t.Errorf("subsequent sampled Error must use terse default; got %q", second.Message)
	}
}

// TestSendToOutChanDropSampling validates the sampling rule:
// the first drop logs, and subsequent drops only log every
// sendDropSampleRate entries. Without the sampling, a stuck
// consumer would flood the log and further starve the producer
// — the specific anti-pattern the windows-redirector work
// surfaced. Uses direct atomic writes to avoid 200ms * N real
// timeouts in the test.
func TestSendToOutChanDropSampling(t *testing.T) {
	t.Parallel()

	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	core, logs := observer.New(zap.ErrorLevel)
	mgr.SetLogger(zap.New(core))

	// Simulate the drop counter hitting values around the
	// sampling boundary without paying the per-drop wall-clock
	// cost. We reach directly into dropCount because the whole
	// point of this test is the logging cadence, not the send
	// path's timing.
	for i := uint64(1); i <= 2050; i++ {
		n := mgr.dropCount.Add(1)
		if n == 1 || n%sendDropSampleRate == 0 {
			mgr.dropLogger().Error("test-sampled-drop",
				zap.Uint64("dropsSoFar", n),
			)
		}
	}

	// Expected emits: n=1, n=1024, n=2048 → 3 entries.
	if got := logs.Len(); got != 3 {
		t.Fatalf("expected 3 sampled drop logs (n=1, 1024, 2048), got %d", got)
	}
}

// TestSetLoggerNilFallsBackToNop ensures the drop path never
// panics even if the host process never calls SetLogger or
// clears it back to nil. zap.L() was the previous fallback and
// produced silent drops on any binary that skipped
// zap.ReplaceGlobals; the Nop fallback here is deliberately
// allocation-free on the drop path.
func TestSetLoggerNilFallsBackToNop(t *testing.T) {
	t.Parallel()

	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}
	// Explicit nil to exercise the clear-back path.
	mgr.SetLogger(nil)

	// dropLogger must return something non-nil and safe to call.
	l := mgr.dropLogger()
	if l == nil {
		t.Fatalf("dropLogger returned nil with no logger installed")
	}
	l.Error("must not panic")
}

// TestDropCountAtomicUnderLoad pins the atomic.Uint64 contract:
// concurrent increments from many goroutines must be observable
// as a single monotonic counter. Bare uint64 + sync/atomic would
// work too, but this test also guards against a future refactor
// accidentally dropping the atomic access.
func TestDropCountAtomicUnderLoad(t *testing.T) {
	t.Parallel()

	mgr := &SyncMockManager{
		buffer: make([]*models.Mock, 0, defaultMockBufferCapacity),
	}

	const goroutines = 16
	const incsPer = 1000
	var wg sync.WaitGroup
	var started atomic.Int32
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started.Add(1)
			for j := 0; j < incsPer; j++ {
				mgr.dropCount.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := mgr.DropCount(); got != uint64(goroutines*incsPer) {
		t.Fatalf("expected %d total increments, got %d", goroutines*incsPer, got)
	}
}

// drainChan empties ch up to max elements; fails if more arrive than
// the sender configuration could have possibly produced.
func drainChan(t *testing.T, ch chan *models.Mock, max int) {
	t.Helper()
	count := 0
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				if count > max {
					t.Fatalf("drained %d mocks, exceeds max %d", count, max)
				}
				return
			}
			count++
			if count > max {
				t.Fatalf("drained %d mocks, exceeds max %d", count, max)
			}
		default:
			return
		}
	}
}

// TestResolveRangeRetroactivelyBinsLateMock is the regression for the
// async-emit vs window-close race (the Mongo cursor getMore case): a
// mock whose presaved ReqTimestampMock falls inside a window whose
// ResolveRange already fired — because the parser decoded/emitted it a
// few ms after the response was captured and the window closed — must
// be retroactively binned into the window that owns it, not stale-
// dropped. Before the recentWindows ring, the late mock matched no
// current-or-future window (every future window starts later) and was
// reaped, so replay's timestamp filter never saw it.
func TestResolveRangeRetroactivelyBinsLateMock(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 8)
	mgr := &SyncMockManager{}
	mgr.SetOutputChannel(ch)
	mgr.SetFirstRequestSignaled() // so AddMock buffers (windowed path)

	// Anchor recently enough that nothing trips the 7 s stale cutoff.
	base := time.Now().Add(-2 * time.Second)

	// test-1 resolves its window FIRST, while its own getMore mock has
	// not been decoded/emitted yet — the buffer is empty for it.
	w1Start, w1End := base, base.Add(5*time.Millisecond)
	mgr.ResolveRange(w1Start, w1End, "test-1", true, false)

	// The late getMore arrives now, after test-1's window closed. Its
	// presaved request timestamp sits INSIDE test-1's window. Untagged
	// Mongo mock => LifetimePerTest, so the session/connection carve-out
	// does not apply and the retroactive-bin path is what rescues it.
	lateTs := base.Add(2 * time.Millisecond)
	mgr.AddMock(&models.Mock{
		Kind: models.Mongo,
		Spec: models.MockSpec{ReqTimestampMock: lateTs},
	})
	if len(mgr.buffer) != 1 {
		t.Fatalf("expected the late mock to be buffered, got %d", len(mgr.buffer))
	}

	// A later request resolves test-2's window. This pass must flush the
	// late mock by attributing it back to test-1's (still-recent) window.
	w2Start, w2End := base.Add(100*time.Millisecond), base.Add(105*time.Millisecond)
	mgr.ResolveRange(w2Start, w2End, "test-2", true, false)

	if len(mgr.buffer) != 0 {
		t.Fatalf("expected the late mock to be flushed (retroactively binned), buffer=%d", len(mgr.buffer))
	}

	select {
	case got := <-ch:
		if !got.Spec.ReqTimestampMock.Equal(lateTs) {
			t.Fatalf("flushed mock has wrong timestamp: got %v want %v", got.Spec.ReqTimestampMock, lateTs)
		}
		if got.Name == "" {
			t.Fatalf("retroactively binned mock must be named for replay correlation")
		}
	default:
		t.Fatalf("expected the late mock to be flushed to outChan, got nothing")
	}
}

// TestResolveRangeRecordsLateMockInOldWindowButDropsOrphan documents the
// deliberate design choice: recentWindows is intentionally NOT age-pruned,
// because a mock whose ReqTimestampMock falls inside a recorded window
// belongs to that test and must be recorded even if its decode landed
// late and the window is now old — losing it would be the very data-loss
// bug this change exists to fix. The buffer-growth bound is preserved
// instead by (a) the stale-cutoff, which still reaps a mock that falls
// inside NO recorded window, and (b) the maxRecentWindows cap on the ring.
func TestResolveRangeRecordsLateMockInOldWindowButDropsOrphan(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 8)
	mgr := &SyncMockManager{}
	mgr.SetOutputChannel(ch)
	mgr.SetFirstRequestSignaled()
	// Past the startup window, so the orphan below is a genuine non-startup
	// orphan and the stale-cutoff drop (this test's invariant) still applies —
	// inside the window every ingest is tagged IsStartup and rescued.
	mgr.resolvedTestCount = models.StartupMockTestCaseWindow

	// An old window, well past the 7 s cutoff.
	old := time.Now().Add(-30 * time.Second)
	mgr.ResolveRange(old, old.Add(5*time.Millisecond), "old-test", true, false)

	inWindowTs := old.Add(2 * time.Millisecond)   // inside old-test's window → recorded
	orphanTs := time.Now().Add(-20 * time.Second) // inside no window, ancient → dropped

	mgr.AddMock(&models.Mock{
		Kind: models.Mongo,
		Spec: models.MockSpec{ReqTimestampMock: inWindowTs},
	})
	mgr.AddMock(&models.Mock{
		Kind: models.Mongo,
		Spec: models.MockSpec{ReqTimestampMock: orphanTs},
	})

	// A fresh resolve drains the buffer: the in-window mock is retroactively
	// binned to old-test (and flushed) even though old-test is ancient; the
	// orphan, owning no window, is reaped by the stale cutoff.
	now := time.Now()
	mgr.ResolveRange(now, now.Add(5*time.Millisecond), "new-test", true, false)

	if len(mgr.buffer) != 0 {
		t.Fatalf("expected buffer drained, buffer=%d", len(mgr.buffer))
	}

	var sent []*models.Mock
	for drained := false; !drained; {
		select {
		case m := <-ch:
			sent = append(sent, m)
		default:
			drained = true
		}
	}
	if len(sent) != 1 {
		t.Fatalf("expected exactly the in-window mock flushed, got %d", len(sent))
	}
	if !sent[0].Spec.ReqTimestampMock.Equal(inWindowTs) {
		t.Fatalf("expected the in-window mock flushed; got ts %v (orphan must be dropped)", sent[0].Spec.ReqTimestampMock)
	}
}

// TestDeleteMocksStrictlyBeforeRescuesLateKeptMock is the regression for
// the multi-cycle static-dedup data loss seen in mongo-bigmock: cycle 1
// is kept, but its big-document mocks land in the buffer AFTER their HTTP
// windows resolved; then cycle 2's DUPLICATE requests fire
// DeleteMocksStrictlyBefore, which used to wipe those late kept mocks
// before any retroactive-bin ResolveRange could rescue them. The cleanup
// must now flush mocks that own a recent KEPT window (and session mocks)
// while still dropping the duplicate's own debris.
func TestDeleteMocksStrictlyBeforeRescuesLateKeptMock(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 8)
	mapCh := make(chan models.TestMockMapping, 8)
	mgr := &SyncMockManager{}
	mgr.SetOutputChannel(ch)
	mgr.SetMappingChannel(mapCh)
	mgr.SetFirstRequestSignaled()

	base := time.Now().Add(-1 * time.Second)

	// Cycle-1 kept (non-duplicate) window for test-1, resolved while its
	// own mock was still being decoded — buffer empty, nothing matches yet.
	mgr.ResolveRange(base, base.Add(5*time.Millisecond), "test-1", true, true)

	keptTs := base.Add(2 * time.Millisecond)      // inside test-1's window → rescue
	debrisTs := base.Add(500 * time.Millisecond)  // owns no window → the duplicate's debris → drop
	sessionTs := base.Add(300 * time.Millisecond) // session lifetime → never reaped by per-test cleanup
	dupHorizon := base.Add(1 * time.Second)       // the duplicate request's timestamp
	futureTs := dupHorizon.Add(5 * time.Millisecond)

	mgr.mu.Lock()
	mgr.buffer = append(mgr.buffer,
		&models.Mock{Kind: models.Mongo, Spec: models.MockSpec{ReqTimestampMock: keptTs}},
		&models.Mock{Kind: models.Mongo, Spec: models.MockSpec{ReqTimestampMock: debrisTs}},
		&models.Mock{
			Kind:         models.Mongo,
			Name:         "sess-keep",
			TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeSession},
			Spec:         models.MockSpec{ReqTimestampMock: sessionTs},
		},
		&models.Mock{Kind: models.Mongo, Spec: models.MockSpec{ReqTimestampMock: futureTs}},
	)
	mgr.mu.Unlock()

	// A later duplicate request resolves and cleans up before its timestamp.
	mgr.DeleteMocksStrictlyBefore(dupHorizon)

	// Only the at/after-horizon mock should remain buffered.
	if len(mgr.buffer) != 1 || !mgr.buffer[0].Spec.ReqTimestampMock.Equal(futureTs) {
		t.Fatalf("expected only the future mock retained; buffer=%d", len(mgr.buffer))
	}

	var sent []*models.Mock
	for drained := false; !drained; {
		select {
		case m := <-ch:
			sent = append(sent, m)
		default:
			drained = true
		}
	}
	if len(sent) != 2 {
		t.Fatalf("expected kept + session mocks rescued (2), got %d", len(sent))
	}

	var sawKept, sawSession bool
	for _, m := range sent {
		switch {
		case m.Spec.ReqTimestampMock.Equal(keptTs):
			sawKept = true
			if m.Name == "" {
				t.Fatalf("rescued per-test mock must be named for replay correlation")
			}
		case m.TestModeInfo.Lifetime == models.LifetimeSession:
			sawSession = true
			if m.Name != "sess-keep" {
				t.Fatalf("session mock must be flushed verbatim, not renamed; got %q", m.Name)
			}
		default:
			t.Fatalf("unexpected mock flushed: ts=%v name=%q (debris must be dropped)", m.Spec.ReqTimestampMock, m.Name)
		}
	}
	if !sawKept || !sawSession {
		t.Fatalf("expected both kept and session mocks rescued; kept=%v session=%v", sawKept, sawSession)
	}

	// The rescued kept mock produced a late mapping for test-1; the debris did not.
	select {
	case mp := <-mapCh:
		if mp.TestName != "test-1" || len(mp.MockIDs) != 1 {
			t.Fatalf("expected one late mapping for test-1, got %+v", mp)
		}
	default:
		t.Fatalf("expected a late TestMockMapping for the rescued kept mock")
	}
	select {
	case mp := <-mapCh:
		t.Fatalf("debris/session must not produce mapping entries; got %+v", mp)
	default:
	}
}

// TestFlushOwnedWindowsPersistsLateMocksDuringRecording is the regression
// for the real mongo-bigmock failure: a per-test mock that lands in the
// buffer AFTER its HTTP window resolved (a 6 MB document still decoding)
// must be drained WHILE recording is live — at shutdown the recorder ctx
// is already cancelled and the write path drops everything, so the
// periodic owned-window flush is what actually persists it. Session mocks
// flush too; not-yet-attributable per-test mocks stay buffered for a
// future window.
func TestFlushOwnedWindowsPersistsLateMocksDuringRecording(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 8)
	mapCh := make(chan models.TestMockMapping, 8)
	mgr := &SyncMockManager{}
	mgr.SetOutputChannel(ch)
	mgr.SetMappingChannel(mapCh)
	mgr.SetFirstRequestSignaled()

	base := time.Now().Add(-1 * time.Second)
	// A kept window resolved while its mock was still decoding (buffer empty).
	mgr.ResolveRange(base, base.Add(5*time.Millisecond), "test-1", true, true)

	ownedTs := base.Add(2 * time.Millisecond)     // inside test-1's window → flush
	orphanTs := base.Add(500 * time.Millisecond)  // owns no window yet → keep buffered
	sessionTs := base.Add(300 * time.Millisecond) // session lifetime → flush verbatim
	mgr.mu.Lock()
	mgr.buffer = append(mgr.buffer,
		&models.Mock{Kind: models.Mongo, Spec: models.MockSpec{ReqTimestampMock: ownedTs}},
		&models.Mock{Kind: models.Mongo, Spec: models.MockSpec{ReqTimestampMock: orphanTs}},
		&models.Mock{
			Kind:         models.Mongo,
			Name:         "sess-keep",
			TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeSession},
			Spec:         models.MockSpec{ReqTimestampMock: sessionTs},
		},
	)
	mgr.mu.Unlock()

	// One periodic tick's worth of work (called directly, no real timer).
	mgr.FlushOwnedWindows()

	// Owned + session flushed; the orphan stays for a possible future window.
	if len(mgr.buffer) != 1 || !mgr.buffer[0].Spec.ReqTimestampMock.Equal(orphanTs) {
		t.Fatalf("expected only the unattributable orphan retained; buffer=%d", len(mgr.buffer))
	}

	var sent []*models.Mock
	for drained := false; !drained; {
		select {
		case m := <-ch:
			sent = append(sent, m)
		default:
			drained = true
		}
	}
	if len(sent) != 2 {
		t.Fatalf("expected owned + session flushed (2), got %d", len(sent))
	}
	var sawOwned, sawSession bool
	for _, m := range sent {
		switch {
		case m.Spec.ReqTimestampMock.Equal(ownedTs):
			sawOwned = true
			if m.Name == "" {
				t.Fatalf("flushed per-test mock must be named for replay correlation")
			}
		case m.TestModeInfo.Lifetime == models.LifetimeSession:
			sawSession = true
			if m.Name != "sess-keep" {
				t.Fatalf("session mock must be flushed verbatim; got %q", m.Name)
			}
		default:
			t.Fatalf("unexpected mock flushed: ts=%v", m.Spec.ReqTimestampMock)
		}
	}
	if !sawOwned || !sawSession {
		t.Fatalf("expected owned and session flushed; owned=%v session=%v", sawOwned, sawSession)
	}

	// The owned mock produced a late mapping for test-1.
	select {
	case mp := <-mapCh:
		if mp.TestName != "test-1" || len(mp.MockIDs) != 1 {
			t.Fatalf("expected one late mapping for test-1, got %+v", mp)
		}
	default:
		t.Fatalf("expected a late TestMockMapping for the owned mock")
	}
}

// TestFlushOwnedWindowsLeavesNothingAttributableUnsent is a focused check
// that a second FlushOwnedWindows after everything attributable is gone
// is a no-op (no buffer churn, no spurious sends).
func TestFlushOwnedWindowsNoopWhenNothingOwned(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 4)
	mgr := &SyncMockManager{}
	mgr.SetOutputChannel(ch)
	mgr.SetFirstRequestSignaled()

	// A single buffered per-test mock that owns no recorded window.
	mgr.mu.Lock()
	mgr.buffer = append(mgr.buffer, &models.Mock{Kind: models.Mongo, Spec: models.MockSpec{ReqTimestampMock: time.Now()}})
	mgr.mu.Unlock()

	mgr.FlushOwnedWindows()

	if len(mgr.buffer) != 1 {
		t.Fatalf("unattributable mock must stay buffered; buffer=%d", len(mgr.buffer))
	}
	select {
	case m := <-ch:
		t.Fatalf("nothing should be flushed; got ts=%v", m.Spec.ReqTimestampMock)
	default:
	}
}

// startupHTTPMock builds a once-per-boot outbound HTTP mock (e.g. an AWS
// Secret Manager fetch at process startup): per-test lifetime, as the HTTP
// recorder stamps it, with a request timestamp at app-boot time.
func startupHTTPMock(reqTS time.Time) *models.Mock {
	return &models.Mock{
		Name: "mocks",
		Kind: models.HTTP,
		Spec: models.MockSpec{
			ReqTimestampMock: reqTS,
			ResTimestampMock: reqTS.Add(50 * time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{
			Lifetime:        models.LifetimePerTest,
			LifetimeDerived: true,
		},
	}
}

// TestStartupMockSurvivesDedupDeleteWhenFirstRequestIsDuplicate reproduces
// the flaky per-test-set capture bug (Rank 1). A once-per-boot outbound
// call is captured before the first inbound request while the output
// channel is not yet bound, so it lands in the buffer tagged IsStartup. If
// the FIRST inbound request hashes as a dedup duplicate, ResolveJob calls
// DeleteMocksStrictlyBefore with recentWindows still empty — so the
// ownerWindow rescue cannot save it and the pre-fix code dropped it as
// "duplicate debris". It must be flushed to disk instead.
func TestStartupMockSurvivesDedupDeleteWhenFirstRequestIsDuplicate(t *testing.T) {
	t.Parallel()

	out := make(chan *models.Mock, 4)
	mgr := &SyncMockManager{buffer: make([]*models.Mock, 0, defaultMockBufferCapacity)}

	bootTS := time.Now().Add(-2 * time.Second)
	mgr.AddMock(startupHTTPMock(bootTS)) // outChan nil → buffered, !firstReqSeen → tagged
	if len(mgr.buffer) != 1 {
		t.Fatalf("init call should buffer before outChan binds; buffer=%d", len(mgr.buffer))
	}
	if !mgr.buffer[0].TestModeInfo.IsStartup {
		t.Fatalf("a mock captured before firstReqSeen must be tagged IsStartup")
	}

	// Recorder wires the channel; the first inbound request is a duplicate.
	mgr.SetOutputChannel(out)
	mgr.SetFirstRequestSignaled()
	mgr.DeleteMocksStrictlyBefore(time.Now())

	select {
	case got := <-out:
		if !got.Spec.ReqTimestampMock.Equal(bootTS) {
			t.Fatalf("flushed wrong mock: ts=%v want %v", got.Spec.ReqTimestampMock, bootTS)
		}
	default:
		t.Fatalf("startup init mock was dropped by the dedup cleanup (Rank 1 regression)")
	}
}

// TestStartupMockSurvivesStaleCutoffInResolveRange reproduces Rank 2: a
// slow boot (>7 s between the init call and the first test window) makes
// the buffered, window-less init mock older than the 7 s stale-cutoff, so
// the pre-fix ResolveRange reaped it. It must be rescued and flushed.
func TestStartupMockSurvivesStaleCutoffInResolveRange(t *testing.T) {
	t.Parallel()

	out := make(chan *models.Mock, 4)
	mgr := &SyncMockManager{buffer: make([]*models.Mock, 0, defaultMockBufferCapacity)}

	bootTS := time.Now().Add(-10 * time.Second)
	mgr.AddMock(startupHTTPMock(bootTS)) // buffered (outChan nil), tagged IsStartup
	mgr.SetOutputChannel(out)
	mgr.SetFirstRequestSignaled()

	now := time.Now()
	mgr.ResolveRange(now.Add(-50*time.Millisecond), now, "test-1", true, false)

	select {
	case got := <-out:
		if !got.Spec.ReqTimestampMock.Equal(bootTS) {
			t.Fatalf("flushed wrong mock: ts=%v want %v", got.Spec.ReqTimestampMock, bootTS)
		}
	default:
		t.Fatalf("startup init mock was dropped by the 7s stale-cutoff (Rank 2 regression)")
	}
}

// TestSetMemoryPressurePreservesStartupMocks pins the memory-wipe reaper:
// SetMemoryPressure must drop ordinary buffered mocks to relieve pressure
// but PRESERVE startup mocks, which own no window and rely on the later
// FlushOwnedWindows drain to reach disk. Wiping them would reintroduce the
// once-per-boot loss the IsStartup tag exists to prevent.
func TestSetMemoryPressurePreservesStartupMocks(t *testing.T) {
	t.Parallel()

	startup := &models.Mock{Spec: models.MockSpec{ReqTimestampMock: time.Now()}}
	startup.TestModeInfo.IsStartup = true
	perTest := &models.Mock{Spec: models.MockSpec{ReqTimestampMock: time.Now()}}

	mgr := &SyncMockManager{buffer: []*models.Mock{perTest, startup}}

	mgr.SetMemoryPressure(true)

	if len(mgr.buffer) != 1 || mgr.buffer[0] != startup {
		t.Fatalf("expected only the startup mock preserved after memory wipe; buffer=%d", len(mgr.buffer))
	}

	// New non-startup mocks are still dropped while paused.
	mgr.AddMock(&models.Mock{Spec: models.MockSpec{ReqTimestampMock: time.Now()}})
	if len(mgr.buffer) != 1 {
		t.Fatalf("new mocks must still be dropped under memory pressure; buffer=%d", len(mgr.buffer))
	}
}

// TestStartupMockFlushedByFlushOwnedWindows asserts the proactive drain: the
// per-session ticker flushes startup mocks straight to disk so they cannot
// linger in the buffer where a later reaper (dedup/stale-cutoff/memory wipe)
// would destroy them.
func TestStartupMockFlushedByFlushOwnedWindows(t *testing.T) {
	t.Parallel()

	out := make(chan *models.Mock, 4)
	mgr := &SyncMockManager{buffer: make([]*models.Mock, 0, defaultMockBufferCapacity)}

	bootTS := time.Now().Add(-1 * time.Second)
	mgr.AddMock(startupHTTPMock(bootTS)) // buffered (outChan nil), tagged IsStartup
	mgr.SetOutputChannel(out)

	mgr.FlushOwnedWindows()

	select {
	case got := <-out:
		if !got.Spec.ReqTimestampMock.Equal(bootTS) {
			t.Fatalf("flushed wrong mock")
		}
	default:
		t.Fatalf("startup mock should be flushed proactively by FlushOwnedWindows")
	}
	if len(mgr.buffer) != 0 {
		t.Fatalf("startup mock should leave the buffer after flush; buffer=%d", len(mgr.buffer))
	}
}

// TestDuplicateDebrisStillDroppedAfterStartupWindow is the negative control:
// once we are PAST the startup window (>= N unique recorded test cases), an
// outbound mock captured after firstReqSeen that owns no window is the skipped
// duplicate's own debris — it is NOT a startup mock and the dedup cleanup must
// still drop it, so the widened startup rescue does not over-rescue forever.
func TestDuplicateDebrisStillDroppedAfterStartupWindow(t *testing.T) {
	t.Parallel()

	out := make(chan *models.Mock, 4)
	mgr := &SyncMockManager{buffer: make([]*models.Mock, 0, defaultMockBufferCapacity)}
	mgr.SetOutputChannel(out)
	mgr.SetFirstRequestSignaled() // request-driven traffic, not boot
	// Advance past the startup window so new ingests are no longer tagged.
	mgr.resolvedTestCount = models.StartupMockTestCaseWindow

	debris := startupHTTPMock(time.Now().Add(-1 * time.Second))
	mgr.AddMock(debris)
	if mgr.buffer[0].TestModeInfo.IsStartup {
		t.Fatalf("a mock captured past the startup window must NOT be tagged IsStartup")
	}

	mgr.DeleteMocksStrictlyBefore(time.Now())

	select {
	case got := <-out:
		t.Fatalf("non-startup debris must be dropped, but it was flushed: ts=%v", got.Spec.ReqTimestampMock)
	default:
	}
	if len(mgr.buffer) != 0 {
		t.Fatalf("debris must be dropped from the buffer; buffer=%d", len(mgr.buffer))
	}
}

// TestStartupWindowTagsMocksAfterFirstReqSeen pins the widened tagging: while
// fewer than N unique test cases have been recorded, AddMock tags ingested
// mocks IsStartup EVEN after firstReqSeen — that is what makes the first N
// tests' mocks survive the dedup reapers.
func TestStartupWindowTagsMocksAfterFirstReqSeen(t *testing.T) {
	t.Parallel()

	mgr := &SyncMockManager{buffer: make([]*models.Mock, 0, defaultMockBufferCapacity)}
	mgr.SetFirstRequestSignaled() // past boot, but still inside the startup window

	mgr.AddMock(startupHTTPMock(time.Now()))
	if !mgr.buffer[0].TestModeInfo.IsStartup {
		t.Fatalf("a mock captured within the startup window must be tagged IsStartup")
	}
}

// TestResolvedTestCountClosesStartupWindowOnKeptResolves verifies the boundary:
// only kept resolves (keep==true, one per UNIQUE recorded test) advance the
// counter, and a keep==false (duplicate) resolve does not. After N kept
// resolves the window closes and new ingests are no longer tagged.
func TestResolvedTestCountClosesStartupWindowOnKeptResolves(t *testing.T) {
	t.Parallel()

	out := make(chan *models.Mock, 8)
	mgr := &SyncMockManager{buffer: make([]*models.Mock, 0, defaultMockBufferCapacity)}
	mgr.SetOutputChannel(out)
	mgr.SetFirstRequestSignaled()

	now := time.Now()
	// A duplicate resolve must NOT advance the counter.
	mgr.ResolveRange(now.Add(-time.Second), now, "test-0", false, false)
	if mgr.resolvedTestCount != 0 {
		t.Fatalf("duplicate (keep=false) resolve must not advance the counter; got %d", mgr.resolvedTestCount)
	}

	// N kept resolves close the window.
	for i := range models.StartupMockTestCaseWindow {
		base := now.Add(time.Duration(i+1) * time.Second)
		mgr.ResolveRange(base, base.Add(10*time.Millisecond), "test", true, false)
	}
	if mgr.resolvedTestCount != models.StartupMockTestCaseWindow {
		t.Fatalf("expected counter == %d after kept resolves, got %d", models.StartupMockTestCaseWindow, mgr.resolvedTestCount)
	}

	// Past the window: ingests are no longer startup.
	mgr.AddMock(startupHTTPMock(now.Add(10 * time.Second)))
	last := mgr.buffer[len(mgr.buffer)-1]
	if last.TestModeInfo.IsStartup {
		t.Fatalf("ingest past the startup window must NOT be tagged IsStartup")
	}
}

// TestStartupMockRescuedFromDuplicateWindowResolve is the core static-dedup
// regression: a startup-window mock captured inside the window of a test case
// that static-dedup deems a DUPLICATE (ResolveRange keep=false) must be flushed
// to disk, not pruned with the rest of the duplicate's window. This is the
// in-window keep=false rescue that the OSS path lacked.
func TestStartupMockRescuedFromDuplicateWindowResolve(t *testing.T) {
	t.Parallel()

	out := make(chan *models.Mock, 4)
	mgr := &SyncMockManager{buffer: make([]*models.Mock, 0, defaultMockBufferCapacity)}
	mgr.SetOutputChannel(out)
	mgr.SetFirstRequestSignaled() // inside the startup window (count == 0)

	now := time.Now()
	inWindowTS := now.Add(-20 * time.Millisecond)
	mgr.AddMock(startupHTTPMock(inWindowTS))
	if !mgr.buffer[0].TestModeInfo.IsStartup {
		t.Fatalf("mock captured inside the startup window must be tagged IsStartup")
	}

	// Duplicate resolve (keep=false) over the window that contains the mock.
	mgr.ResolveRange(now.Add(-50*time.Millisecond), now, "test-0", false, false)

	select {
	case got := <-out:
		if !got.Spec.ReqTimestampMock.Equal(inWindowTS) {
			t.Fatalf("rescued wrong mock: ts=%v want %v", got.Spec.ReqTimestampMock, inWindowTS)
		}
	default:
		t.Fatalf("startup mock inside a duplicate window must be rescued, not pruned")
	}
	if len(mgr.buffer) != 0 {
		t.Fatalf("rescued mock should leave the buffer; buffer=%d", len(mgr.buffer))
	}
}

// TestNonStartupMockPrunedFromDuplicateWindowResolve is the matching negative
// control: past the startup window, a non-startup mock inside a duplicate's
// window (ResolveRange keep=false) is pruned as before — the rescue is scoped to
// the startup window only.
func TestNonStartupMockPrunedFromDuplicateWindowResolve(t *testing.T) {
	t.Parallel()

	out := make(chan *models.Mock, 4)
	mgr := &SyncMockManager{buffer: make([]*models.Mock, 0, defaultMockBufferCapacity)}
	mgr.SetOutputChannel(out)
	mgr.SetFirstRequestSignaled()
	mgr.resolvedTestCount = models.StartupMockTestCaseWindow // past the window

	now := time.Now()
	mgr.AddMock(startupHTTPMock(now.Add(-20 * time.Millisecond)))
	if mgr.buffer[0].TestModeInfo.IsStartup {
		t.Fatalf("mock past the startup window must NOT be tagged IsStartup")
	}

	mgr.ResolveRange(now.Add(-50*time.Millisecond), now, "test-0", false, false)

	select {
	case got := <-out:
		t.Fatalf("non-startup mock in a duplicate window must be pruned, but it was flushed: ts=%v", got.Spec.ReqTimestampMock)
	default:
	}
	if len(mgr.buffer) != 0 {
		t.Fatalf("pruned mock should leave the buffer; buffer=%d", len(mgr.buffer))
	}
}
