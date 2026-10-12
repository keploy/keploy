package mock

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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

	mu       sync.Mutex
	names    []string
	promoted atomic.Bool
}

func (r *recordingMockDB) InsertMock(_ context.Context, m *models.Mock, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names = append(r.names, m.Name)
	return nil
}
func (r *recordingMockDB) DeleteMocksForSet(context.Context, string) error { return nil }
func (r *recordingMockDB) PromoteStagedSet(context.Context, string, string) error {
	r.promoted.Store(true)
	return nil
}
func (r *recordingMockDB) DiscardStagedSet(context.Context, string) error { return nil }
func (r *recordingMockDB) ResetCounterID()                                {}
func (r *recordingMockDB) inserted() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.names...)
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

	cfg := &config.Config{}
	cfg.Mock.Name = "drain-test"

	svc := New(zap.NewNop(), inst, db, nil, FileStore{}, nil, cfg)

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
// then (AgentClient.BacklogBefore).
type pendingInstrumentation struct {
	trickleInstrumentation
	quietFor time.Duration
	pending  atomic.Bool
	asked    atomic.Int32
}

func (p *pendingInstrumentation) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	p.asked.Add(1)
	if p.pending.Load() {
		return models.AgentBacklog{Verdict: models.BacklogPending}, nil
	}
	return models.AgentBacklog{Verdict: models.BacklogClear}, nil
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
		// A live agent keeps the stream open until the client ends it.
		<-p.captureCtx.Done()
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

// unansweredInstrumentation is pendingInstrumentation whose agent does not
// answer the drain's looks while its parser is behind (a /record/pending that
// times out), and says it holds nothing more once it has handed the mock over.
type unansweredInstrumentation struct {
	pendingInstrumentation
}

func (p *unansweredInstrumentation) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	p.asked.Add(1)
	if p.pending.Load() {
		return models.AgentBacklog{}, context.DeadlineExceeded
	}
	return models.AgentBacklog{Verdict: models.BacklogClear}, nil
}

// A look the agent does not answer is never read as "nothing pending": the
// drain waits, and the mock the parser hands over later is saved (P7).
func TestRecord_ALookTheAgentDoesNotAnswerDoesNotLoseAMock(t *testing.T) {
	inst := &unansweredInstrumentation{pendingInstrumentation{
		trickleInstrumentation: trickleInstrumentation{out: make(chan *models.Mock)},
		quietFor:               4 * mockDrainQuiet,
	}}
	db := &recordingMockDB{}
	cfg := &config.Config{}
	cfg.Mock.Name = "unanswered-test"
	if err := New(zap.NewNop(), inst, db, nil, FileStore{}, nil, cfg).Record(context.Background()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	inst.wg.Wait()
	if got := db.inserted(); len(got) != 1 || inst.asked.Load() == 0 {
		t.Fatalf("persisted %v (the agent was asked %d times): a look it did not answer ended the drain", got, inst.asked.Load())
	}
}

// goneInstrumentation is an agent that says, at the drain's look, that it
// still holds a mock from before the runner exited, and then goes away
// (killed, or crashed) without handing it over: its stream ends.
type goneInstrumentation struct {
	trickleInstrumentation
	looked chan struct{}
	once   sync.Once
	asked  atomic.Int32
}

func (g *goneInstrumentation) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	n := g.asked.Add(1)
	if g.looked == nil {
		// Gone before the drain asks: a look that comes anyway (a slow
		// machine) finds it unreachable, and says nothing of what it held.
		return models.AgentBacklog{}, refusedErr()
	}
	g.once.Do(func() { close(g.looked) })
	// Its capture moving on every look: the drain never gives up on it, so
	// only the stream's end ends the drain.
	return models.AgentBacklog{Verdict: models.BacklogPending, Progress: uint64(n)}, nil
}

func (g *goneInstrumentation) Run(context.Context, models.RunOptions) models.AppError {
	captured := time.Now()
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if g.looked == nil {
			// Gone before the drain asks, and before it handed anything
			// over: nothing says it held anything.
			close(g.out)
			return
		}
		// One mock it hands over: the run has something to save.
		select {
		case g.out <- &models.Mock{Name: "handed", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: captured}}:
		case <-g.captureCtx.Done():
			return
		}
		select {
		case <-g.looked:
		case <-g.captureCtx.Done():
		}
		close(g.out) // the agent goes away
	}()
	return models.AppError{}
}

// recordGone records with an agent that goes away during the drain, after
// the drain's first look or (lookFirst false) before any, and returns what
// was logged and the exit code the run set.
func recordGone(t *testing.T, lookFirst bool) (*goneInstrumentation, *observer.ObservedLogs, int, error, *recordingMockDB) {
	t.Helper()
	defer func(code int) { utils.ErrCode = code }(utils.ErrCode)
	utils.ErrCode = 0
	inst := &goneInstrumentation{trickleInstrumentation: trickleInstrumentation{out: make(chan *models.Mock)}}
	if lookFirst {
		inst.looked = make(chan struct{})
	}
	core, logs := observer.New(zapcore.WarnLevel)
	cfg := &config.Config{}
	cfg.Mock.Name = "gone-test"
	db := &recordingMockDB{}
	err := New(zap.New(core), inst, db, nil, FileStore{}, nil, cfg).Record(context.Background())
	inst.wg.Wait()
	return inst, logs, utils.ErrCode, err, db
}

// A run the agent holds its streams for (any but compose) opens them marked to
// say so. An agent that ends its stream after it said it still held a mock,
// with nothing handed over since, has lost that mock: said at ERROR, and the
// run fails.
func TestRecord_AnAgentThatGoesAwayWhileItHoldsAMockFailsTheRun(t *testing.T) {
	inst, logs, code, err, db := recordGone(t, true)
	if !models.DrainsAfterStop(inst.captureCtx) {
		t.Fatal("a native run opened its mock stream without asking the agent to hold for it")
	}
	// Certain or possible (a look between the mock and the end, on a slow
	// machine, counts the mock), the run fails: gatedDrain tells the two apart.
	if logs.FilterMessageSnippet("ended its mock stream").FilterLevelExact(zapcore.ErrorLevel).Len() != 1 || code != utils.ExitKeployError || err == nil {
		t.Fatalf("the lost mock was not said at ERROR, with the run failed (exit code %d, error %v): %v", code, err, logs.All())
	}
	// Short of a mock, the set must not replace the one it was recorded to
	// update.
	if db.promoted.Load() {
		t.Fatal("a mock set the recording said is incomplete was promoted over the existing one")
	}
}

// One that ends its stream before the drain asked it is said at WARN: what it
// held, if anything, is lost, but nothing says it held anything.
func TestRecord_AnAgentThatGoesAwayBeforeTheDrainAsksIsSaid(t *testing.T) {
	_, logs, code, err, _ := recordGone(t, false)
	if logs.FilterMessageSnippet("ended its mock stream before it said").FilterLevelExact(zapcore.WarnLevel).Len() != 1 || code != 0 || err != nil {
		t.Fatalf("an agent going away before the drain asked was not said once at WARN, or failed the run (exit code %d, error %v): %v", code, err, logs.All())
	}
}

// gatedBacklog answers the drain's first look that it still holds a mock (or,
// with err set, does not answer it), and holds that look open until the test
// releases it.
type gatedBacklog struct {
	Instrumentation // embedded: nil, so an unexpected call panics loudly
	entered, gate   chan struct{}
	once            sync.Once
	asked           atomic.Int32
	err             error
	// goneAtSecond, when set, runs at the second look, which then finds the
	// agent unreachable: it ends the stream there.
	goneAtSecond func()
}

// refusedErr is the error of a real refused dial: the platform's own, as an
// agent that is gone gives it.
func refusedErr() error {
	refusedOnce.Do(func() {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			refused = err
			return
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		_, refused = net.Dial("tcp4", addr)
	})
	return refused
}

var (
	refusedOnce sync.Once
	refused     error
)

func (g *gatedBacklog) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	n := g.asked.Add(1)
	g.once.Do(func() {
		close(g.entered)
		<-g.gate
	})
	if n >= 2 && g.goneAtSecond != nil {
		g.goneAtSecond()
		return models.AgentBacklog{}, refusedErr()
	}
	if g.err != nil {
		return models.AgentBacklog{}, g.err
	}
	return models.AgentBacklog{Verdict: models.BacklogPending, Progress: uint64(n)}, nil
}

// gatedDrain runs drainTrailingMocks against gatedBacklog: while the drain's
// first look is held open, whileLooking runs, and the agent's stream ends;
// then the look returns. held() reports *frames. It returns the drain's
// verdict (lost), its logs and the exit code it set.
func gatedDrain(t *testing.T, lookErr error, frames *atomic.Uint64, whileLooking func(), endAtSecondLook ...bool) (bool, *observer.ObservedLogs, int) {
	t.Helper()
	defer func(code int) { utils.ErrCode = code }(utils.ErrCode)
	utils.ErrCode = 0
	inst := &gatedBacklog{entered: make(chan struct{}), gate: make(chan struct{}), err: lookErr}
	atSecond := len(endAtSecondLook) > 0 && endAtSecondLook[0]
	core, logs := observer.New(zapcore.WarnLevel)
	m := &mockService{logger: zap.New(core), instrumentation: inst}
	consumerDone := make(chan struct{})
	var lost atomic.Bool
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		lost.Store(m.drainTrailingMocks(consumerDone, time.Now(), true, func() (uint64, bool) { return frames.Load(), false }, frames.Load))
	}()
	wait := func(c <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-c:
		case <-time.After(30 * time.Second):
			t.Fatalf("%s did not happen", what)
		}
	}
	if atSecond {
		var end sync.Once                                                     // a later look, on a slow machine, finds it ended
		inst.goneAtSecond = func() { end.Do(func() { close(consumerDone) }) } // the agent's stream ends
	}
	wait(inst.entered, "the drain's look")
	whileLooking()
	if !atSecond {
		close(consumerDone) // the agent's stream ended
	}
	close(inst.gate)
	wait(drained, "the drain ending")
	return lost.Load(), logs, utils.ErrCode
}

// The agent ends its stream after a look that said it still held a mock: the
// run fails whether or not a mock came since; what came only decides whether
// the loss is said to be certain or possible.
func TestDrainTrailingMocks_AnAgentGoneAfterSayingItHeldAMockFailsTheRun(t *testing.T) {
	for _, tc := range []struct {
		name    string
		since   bool
		snippet string
	}{
		{"nothing since", false, "those are lost"},
		{"a mock since", true, "some may be lost"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var frames atomic.Uint64
			frames.Store(1)
			lost, logs, code := gatedDrain(t, nil, &frames, func() {
				if tc.since {
					frames.Add(1)
				}
			})
			if !lost || code != utils.ExitKeployError || logs.FilterLevelExact(zapcore.ErrorLevel).FilterMessageSnippet(tc.snippet).Len() != 1 {
				t.Fatalf("want the run failed with %q at ERROR: lost=%v exit code %d: %v", tc.snippet, lost, code, logs.All())
			}
		})
	}
}

// The agent was already gone when the drain first looked (it could not be
// reached): that look says nothing of what it held, and nothing else did. Said
// at WARN; the run is not failed.
func TestDrainTrailingMocks_AnAgentGoneAtTheFirstLookIsAWarning(t *testing.T) {
	var frames atomic.Uint64
	frames.Store(1)
	lost, logs, code := gatedDrain(t, refusedErr(), &frames, func() {})
	if lost || code != 0 || logs.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet("ended its mock stream before it said").Len() != 1 {
		t.Fatalf("want one WARN and the run not failed: lost=%v exit code %d: %v", lost, code, logs.All())
	}
}

// A second interrupt that reaches the agent too ends its stream as the user
// gives the drain up: the user's stop, not the agent going away, and the run is
// not failed for it. Both are ready when the drain's look returns: a drain that
// went by the order its select took them in failed about half its runs, and 8
// runs catch that all but surely.
func TestDrainTrailingMocks_ASecondInterruptAsTheStreamEndsIsTheUsersStop(t *testing.T) {
	defer utils.ResetInterruptedAgain()
	for i := 0; i < 8; i++ {
		var frames atomic.Uint64
		frames.Store(1)
		lost, logs, code := gatedDrain(t, nil, &frames, utils.MarkInterruptedAgain)
		utils.ResetInterruptedAgain()
		if lost || code != 0 || logs.FilterMessageSnippet("ended its mock stream").Len() != 0 {
			t.Fatalf("run %d: a second interrupt was taken for the agent going away (lost=%v, exit code %d): %v", i, lost, code, logs.All())
		}
		if logs.FilterMessageSnippet("at a second interrupt").FilterLevelExact(zapcore.WarnLevel).Len() != 1 {
			t.Fatalf("run %d: the second interrupt was not said once at WARN: %v", i, logs.All())
		}
	}
}

// It said it still held a mock, then was gone at the next look (it could not
// be reached) as its stream ended: that look says nothing, the one before it
// decides, and so does what came since that one.
func TestDrainTrailingMocks_AnAgentGoneAtTheNextLookGoesByTheOneBefore(t *testing.T) {
	for _, tc := range []struct {
		name    string
		since   bool
		snippet string
	}{
		{"nothing between", false, "those are lost"},
		{"a mock between", true, "some may be lost"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var frames atomic.Uint64
			frames.Store(1)
			lost, logs, code := gatedDrain(t, nil, &frames, func() {
				if tc.since {
					frames.Add(1)
				}
			}, true)
			if !lost || code != utils.ExitKeployError || logs.FilterLevelExact(zapcore.ErrorLevel).FilterMessageSnippet(tc.snippet).Len() != 1 {
				t.Fatalf("want the run failed with %q at ERROR: lost=%v exit code %d: %v", tc.snippet, lost, code, logs.All())
			}
		})
	}
}

// Gone before any answer, though mocks from before the runner exited were
// still coming during the drain. That does not tell an agent's backlog from
// this CLI's own lag in taking what the agent had already sent (a slow disk),
// and a complete recording must not fail for that: said at WARN.
func TestDrainTrailingMocks_MocksStillComingBeforeAnyAnswerAreAWarning(t *testing.T) {
	var frames atomic.Uint64
	frames.Store(1)
	lost, logs, code := gatedDrain(t, refusedErr(), &frames, func() { frames.Add(1) }, true)
	if lost || code != 0 || logs.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet("ended its mock stream before it said").Len() != 1 {
		t.Fatalf("want one WARN and the run not failed: lost=%v exit code %d: %v", lost, code, logs.All())
	}
}
