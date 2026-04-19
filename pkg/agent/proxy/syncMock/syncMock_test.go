package manager

import (
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
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

	mock := &models.Mock{Spec: models.MockSpec{ReqTimestampMock: time.Now()}}
	var wg sync.WaitGroup
	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < sendsPerSender; j++ {
				mgr.AddMock(mock)
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

// TestResolveRangeAfterCloseStillDrainsMatches locks in the
// in-window send path: even after CloseOutChan has fired,
// ResolveRange must still attempt to forward matching mocks (via
// sendToOutChan, which silently drops on closed). This matters
// because the record flow's final ResolveRange can fire while
// shutdown is already in progress, and we previously regressed
// the mongo fuzzer (record_build_replay_latest on #4045 CI) by
// pre-dropping those mocks inside ResolveRange instead of letting
// sendToOutChan handle the close check uniformly.
func TestResolveRangeAfterCloseStillDrainsMatches(t *testing.T) {
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

	// Buffer should be cleared for window-matching entries (we
	// attempted the send; sendToOutChan's outChanClosed guard
	// dropped it silently — that's fine, the mock isn't retained).
	mgr.mu.Lock()
	remaining := len(mgr.buffer)
	mgr.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected buffer drained after ResolveRange-post-close; got %d mocks left", remaining)
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
