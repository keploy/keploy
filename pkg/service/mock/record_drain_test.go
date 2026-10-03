package mock

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// drainInstrumentation is a wrapped runner that emits its last mock AFTER Run
// has returned — the shape of every real recording, where the agent is still
// parsing the final dependency response as the runner exits. The measured gap
// on a real reproduction was a median of 6.5ms.
type drainInstrumentation struct {
	Instrumentation // embedded: nil, so any unexpected call panics loudly

	out        chan *models.Mock
	captureCtx context.Context
	emitAfter  time.Duration
	wg         sync.WaitGroup
}

func (d *drainInstrumentation) Setup(context.Context, string, models.SetupOptions) error { return nil }

// GetOutgoing models the real stream's lifetime, which is the whole point of
// this test. pkg/platform/http/agent.go builds the mock stream with
// http.NewRequestWithContext(ctx, ...) and forwards each decoded mock with
//
//	select {
//	case <-ctx.Done():
//	        return nil          // decoded mock discarded
//	case mockChan <- &mock:
//	}
//
// so cancelling this context aborts the request, ends the forwarding loop and
// closes the channel. A fake that merely returns a plain channel would keep
// delivering after cancellation and would pass against the very bug being
// guarded — it would test nothing.
func (d *drainInstrumentation) GetOutgoing(ctx context.Context, _ models.OutgoingOptions) (<-chan *models.Mock, error) {
	d.captureCtx = ctx
	return d.out, nil
}

// send forwards a mock unless the capture context has already been cancelled,
// mirroring agent.go's select. Reports whether the mock made it.
func (d *drainInstrumentation) send(m *models.Mock) bool {
	select {
	case <-d.captureCtx.Done():
		return false
	case d.out <- m:
		return true
	}
}

func (d *drainInstrumentation) Run(_ context.Context, _ models.RunOptions) models.AppError {
	// One mock captured while the runner was alive.
	d.send(&models.Mock{Name: "mock-0", Kind: models.HTTP})

	// ...and one whose agent-side parse completes just after the runner exits.
	// Emitted from a goroutine so Run returns first, exactly like the real race.
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		select {
		case <-time.After(d.emitAfter):
		case <-d.captureCtx.Done():
		}
		d.send(&models.Mock{Name: "mock-1", Kind: models.HTTP})
		close(d.out)
	}()
	return models.AppError{}
}

func (d *drainInstrumentation) NotifyGracefulShutdown(context.Context) error { return nil }

// recordingMockDB records what actually reached persistence.
type recordingMockDB struct {
	MockDB // embedded: nil, so an unexpected call panics loudly

	mu    sync.Mutex
	names []string
}

func (r *recordingMockDB) InsertMock(_ context.Context, m *models.Mock, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names = append(r.names, m.Name)
	return nil
}
func (r *recordingMockDB) DeleteMocksForSet(context.Context, string) error { return nil }
func (r *recordingMockDB) ResetCounterID()                                 {}
func (r *recordingMockDB) inserted() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.names...)
}

type recordingMappingDB struct {
	mu            sync.Mutex
	deletedSets   []string
	upsertBatches map[string][]models.MockEntry
}

func (r *recordingMappingDB) UpsertBatch(_ context.Context, testSetID string, byTest map[string][]models.MockEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.upsertBatches == nil {
		r.upsertBatches = make(map[string][]models.MockEntry)
	}
	for _, entries := range byTest {
		r.upsertBatches[testSetID] = append(r.upsertBatches[testSetID], entries...)
	}
	return nil
}

func (r *recordingMappingDB) Get(_ context.Context, _ string) (map[string][]models.MockEntry, bool, error) {
	return make(map[string][]models.MockEntry), false, nil
}

func (r *recordingMappingDB) DeleteMappingsForSet(_ context.Context, testSetID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deletedSets = append(r.deletedSets, testSetID)
	return nil
}

// TestRecord_KeepsMockEmittedAfterRunnerExit is the regression guard for the
// silent mock loss reproduced from CI runs 98492767425 / 97406735496 /
// 98512662485, where a two-call recording persisted only the FIRST call.
//
// The cause was ordering, not timing: Record cancelled the capture context and
// only then waited out a 5s "drain grace". Cancelling tears down the outgoing
// stream the grace was meant to drain, so the wait returned in microseconds on
// an already-closed channel and the trailing mock — already written to the wire
// by the agent — was discarded with no warning and no drop counter.
//
// Emitting mock-1 after Run returns fails against that ordering and passes once
// the drain happens before the cancel.
func TestRecord_KeepsMockEmittedAfterRunnerExit(t *testing.T) {
	inst := &drainInstrumentation{
		out: make(chan *models.Mock, 4),
		// Comfortably inside mockDrainQuiet, comfortably outside the
		// microseconds a cancel-first teardown leaves.
		emitAfter: 50 * time.Millisecond,
	}
	db := &recordingMockDB{}
	mappingDB := &recordingMappingDB{}

	cfg := &config.Config{}
	cfg.Mock.Name = "drain-test"

	svc := New(zap.NewNop(), inst, db, mappingDB, FileStore{}, nil, cfg)

	if err := svc.Record(context.Background()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	inst.wg.Wait()

	got := db.inserted()
	if len(got) != 2 {
		t.Fatalf("persisted %d mock(s) %v, want 2 — the mock emitted after the runner exited was dropped, which is the CI failure this test exists for", len(got), got)
	}
	if got[0] != "mock-0" || got[1] != "mock-1" {
		t.Fatalf("persisted %v, want [mock-0 mock-1]", got)
	}

	mappingDB.mu.Lock()
	deleted := mappingDB.deletedSets
	mappingDB.mu.Unlock()
	if len(deleted) == 0 || deleted[0] != "drain-test" {
		t.Fatalf("expected DeleteMappingsForSet to be called for drain-test, got %v", deleted)
	}
}

// trickleInstrumentation emits several trailing mocks spaced further apart than
// a single quiet period, so the drain only sees them all if it RESETS its timer
// on each arrival rather than returning at the first tick.
type trickleInstrumentation struct {
	Instrumentation

	out        chan *models.Mock
	captureCtx context.Context
	spacing    time.Duration
	count      int
	wg         sync.WaitGroup
}

func (d *trickleInstrumentation) Setup(context.Context, string, models.SetupOptions) error {
	return nil
}

func (d *trickleInstrumentation) GetOutgoing(ctx context.Context, _ models.OutgoingOptions) (<-chan *models.Mock, error) {
	d.captureCtx = ctx
	return d.out, nil
}

func (d *trickleInstrumentation) Run(_ context.Context, _ models.RunOptions) models.AppError {
	// The calls behind these mocks were made while the runner ran: the agent
	// is still handing them over after it exits.
	captured := time.Now()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer close(d.out)
		for i := 0; i < d.count; i++ {
			select {
			case <-time.After(d.spacing):
			case <-d.captureCtx.Done():
				return
			}
			select {
			case <-d.captureCtx.Done():
				return
			case d.out <- &models.Mock{Name: "trickle-" + string(rune('a'+i)), Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: captured}}:
			}
		}
	}()
	return models.AppError{}
}

func (d *trickleInstrumentation) NotifyGracefulShutdown(context.Context) error { return nil }

// TestRecord_DrainKeepsWaitingWhileMocksStillArrive pins the quiescence reset.
// A drain that returned at its first quiet tick, or that never noticed
// arrivals, would keep the previous test green — one trailing mock lands well
// inside the first window — while still truncating a suite whose agent is
// handing over a backlog.
//
// The spacing straddles the tick deliberately: each mock arrives inside the
// window opened by the previous one, so only a drain that RESETS on arrival
// sees them all. It is under mockDrainQuiet rather than over it because a first
// mock arriving after a whole quiet period is outside what this drain promises
// — the real agent-to-exit gap measured 6.5ms median, 22.8ms max — and a test
// asserting more than the fix provides would be testing a wish.
func TestRecord_DrainKeepsWaitingWhileMocksStillArrive(t *testing.T) {
	const want = 3
	inst := &trickleInstrumentation{
		out:     make(chan *models.Mock, want),
		spacing: mockDrainQuiet - 150*time.Millisecond,
		count:   want,
	}
	db := &recordingMockDB{}

	cfg := &config.Config{}
	cfg.Mock.Name = "trickle-test"

	if err := New(zap.NewNop(), inst, db, nil, FileStore{}, nil, cfg).Record(context.Background()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	inst.wg.Wait()

	if got := db.inserted(); len(got) != want {
		t.Fatalf("persisted %d mock(s) %v, want %d — the drain stopped waiting while the agent was still handing mocks over", len(got), got, want)
	}
}

// A backlog the agent is still handing over when the runner exits is drained
// for as long as it keeps coming: a sink still draining is not cut off at a
// fixed deadline (it used to be, at 5 s).
func TestRecord_DrainIsNotCutOffWhileMocksStillArrive(t *testing.T) {
	const want = 20
	inst := &trickleInstrumentation{
		out:     make(chan *models.Mock, want),
		spacing: mockDrainQuiet - 150*time.Millisecond, // 20 of them: ~7 s
		count:   want,
	}
	db := &recordingMockDB{}
	cfg := &config.Config{}
	cfg.Mock.Name = "backlog-test"
	if err := New(zap.NewNop(), inst, db, nil, FileStore{}, nil, cfg).Record(context.Background()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	inst.wg.Wait()
	if got := db.inserted(); len(got) != want {
		t.Fatalf("persisted %d of %d mocks: the drain was cut off while the agent was still handing them over", len(got), want)
	}
}

// Ctrl+C stops the recording, not the drain of what the agent already has.
func TestRecord_InterruptStillDrainsWhatTheAgentHas(t *testing.T) {
	inst := &drainInstrumentation{
		out:       make(chan *models.Mock, 4),
		emitAfter: 50 * time.Millisecond,
	}
	db := &recordingMockDB{}
	cfg := &config.Config{}
	cfg.Mock.Name = "interrupt-test"
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the user interrupted; the runner has already exited
	_ = New(zap.NewNop(), inst, db, nil, FileStore{}, nil, cfg).Record(ctx)
	inst.wg.Wait()
	if got := db.inserted(); len(got) != 2 {
		t.Fatalf("persisted %v after an interrupt, want both mocks the agent had", got)
	}
}

// lingerInstrumentation leaves something running after the runner exits that
// keeps making calls: their mocks were requested after the runner exited.
type lingerInstrumentation struct {
	trickleInstrumentation
	stop chan struct{}
}

func (d *lingerInstrumentation) Run(_ context.Context, _ models.RunOptions) models.AppError {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer close(d.out)
		for i := 0; ; i++ {
			select {
			case <-d.stop:
				return
			case <-d.captureCtx.Done():
				return
			case <-time.After(mockDrainQuiet / 4):
			}
			select {
			case <-d.captureCtx.Done():
				return
			case d.out <- &models.Mock{Name: "late", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: time.Now()}}:
			}
		}
	}()
	return models.AppError{}
}

// Calls something left running keeps making after the runner exits are not
// what the agent held at the exit: they must not hold the drain open forever.
func TestRecord_LingeringCallsDoNotHoldTheDrain(t *testing.T) {
	inst := &lingerInstrumentation{
		trickleInstrumentation: trickleInstrumentation{out: make(chan *models.Mock, 4)},
		stop:                   make(chan struct{}),
	}
	defer close(inst.stop)
	cfg := &config.Config{}
	cfg.Mock.Name = "linger-test"
	done := make(chan error, 1)
	go func() {
		done <- New(zap.NewNop(), inst, &recordingMockDB{}, nil, FileStore{}, nil, cfg).Record(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Record never returned while something kept making calls after the runner exited")
	}
}

// slowMockDB takes longer than the drain's quiet bound to persist each of the
// first `slow` mocks: a 12 MB mock (RCA #2's 8,209 rows), or a disk stall.
type slowMockDB struct {
	recordingMockDB
	slow  int
	delay time.Duration
}

func (s *slowMockDB) InsertMock(ctx context.Context, m *models.Mock, set string) error {
	s.mu.Lock()
	n := len(s.names)
	s.mu.Unlock()
	if n < s.slow {
		time.Sleep(s.delay)
	}
	return s.recordingMockDB.InsertMock(ctx, m, set)
}

// A consumer persisting one mock for longer than the drain's quiet bound is
// still draining: the mocks the agent holds behind it must not be cut off. The
// drain counted a mock when the consumer took it, so one slow write looked
// like a quiet stream, and the capture was torn down with the agent still
// holding the rest.
func TestRecord_SlowPersistIsNotCutOff(t *testing.T) {
	const want = 6
	inst := &trickleInstrumentation{
		out:     make(chan *models.Mock), // the agent's stream hands over one at a time
		spacing: time.Millisecond,
		count:   want,
	}
	db := &slowMockDB{slow: 2, delay: mockDrainQuiet + 400*time.Millisecond}
	cfg := &config.Config{}
	cfg.Mock.Name = "slow-sink-test"
	if err := New(zap.NewNop(), inst, db, nil, FileStore{}, nil, cfg).Record(context.Background()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	inst.wg.Wait()
	if got := db.inserted(); len(got) != want {
		t.Fatalf("persisted %d of %d mocks the agent held: a slow write ended the drain", len(got), want)
	}
}

// pendingInstrumentation is an agent whose parser is behind: a mock requested
// before the runner exited comes only after a quiet stretch longer than the
// drain's quiet bound, and the agent says it may still hand one over until
// then (AgentClient.PendingBefore).
type pendingInstrumentation struct {
	trickleInstrumentation
	quietFor time.Duration
	pending  atomic.Bool
	asked    atomic.Int32
}

func (p *pendingInstrumentation) PendingBefore(context.Context, time.Time) (bool, bool, error) {
	p.asked.Add(1)
	return p.pending.Load(), true, nil
}

func (p *pendingInstrumentation) Run(_ context.Context, _ models.RunOptions) models.AppError {
	captured := time.Now()
	p.pending.Store(true)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer close(p.out)
		select {
		case <-time.After(p.quietFor):
		case <-p.captureCtx.Done():
			return
		}
		select {
		case p.out <- &models.Mock{Name: "late", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: captured}}:
		case <-p.captureCtx.Done():
			return
		}
		p.pending.Store(false)
	}()
	return models.AppError{}
}

// Quiet is not proof that the agent has handed over what it captured before
// the runner exited: a parser behind the traffic can be quiet for longer than
// any grace. An agent that can tell is asked, and the drain goes on while it
// says it may still hand a mock over.
func TestRecord_DrainWaitsWhileTheAgentSaysItHoldsMore(t *testing.T) {
	inst := &pendingInstrumentation{
		trickleInstrumentation: trickleInstrumentation{out: make(chan *models.Mock)},
		quietFor:               4 * mockDrainQuiet,
	}
	db := &recordingMockDB{}
	cfg := &config.Config{}
	cfg.Mock.Name = "pending-test"
	if err := New(zap.NewNop(), inst, db, nil, FileStore{}, nil, cfg).Record(context.Background()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	inst.wg.Wait()
	if got := db.inserted(); len(got) != 1 || inst.asked.Load() == 0 {
		t.Fatalf("persisted %v (the agent was asked %d times): the drain ended while the agent said it still held a mock", got, inst.asked.Load())
	}
}
