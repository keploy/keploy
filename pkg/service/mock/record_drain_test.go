package mock

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/neterr"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
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
	// onRun, when set, runs as the test command starts (a user stopping the
	// recording, say); runErr is how the test command ends.
	onRun  func()
	runErr models.AppError
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
	if d.onRun != nil {
		d.onRun()
	}

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
	return d.runErr
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

func (r *recordingMappingDB) Delete(_ context.Context, testSetID string) error {
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
		t.Fatalf("expected Delete to be called for drain-test, got %v", deleted)
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
	// sent is how many it has handed over: on its unbuffered stream, taken
	// by the client.
	sent atomic.Int32
	// holdOpen keeps the stream open after the last mock until the client
	// ends it, as a live agent does.
	holdOpen bool
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
				d.sent.Add(1)
			}
		}
		if d.holdOpen {
			<-d.captureCtx.Done()
		}
	}()
	return models.AppError{}
}

func (d *trickleInstrumentation) NotifyGracefulShutdown(context.Context) error { return nil }

// The drain's quiet resets on every arrival, and while the consumer is
// still persisting one: with an agent that cannot say what it still holds,
// quiet is all the drain has to go by, and one that ended at its first quiet
// look, or did not see what came between looks, would cut off a backlog
// still coming. Driven by what the consumer has taken at each look, not by
// when mocks come, so no load on the host decides it: the drain ends at the
// first look that finds nothing new and the consumer free, and that end is
// unconfirmed (tailQuiet), never silent.
func TestDrainTrailingMocks_QuietResetsOnEveryArrival(t *testing.T) {
	// What the consumer has taken, and whether it is persisting one, at the
	// drain's start and at each look after: the last holds from then on.
	steps := []struct {
		taken uint64
		busy  bool
	}{{0, false}, {1, false}, {1, true}, {2, false}, {2, false}}
	var looked int // read and written on the drain's goroutine only
	held := func() (uint64, bool) {
		s := steps[min(looked, len(steps)-1)]
		looked++
		return s.taken, s.busy
	}
	m := &mockService{logger: zap.NewNop(), instrumentation: struct{ Instrumentation }{}}
	lost, tail := m.drainTrailingMocks(make(chan struct{}), time.Now(), true, held, func() uint64 { return 0 }, recordDrainWords, nil)
	if looked != len(steps) {
		t.Fatalf("the drain read what the consumer had taken %d time(s), want %d: it ended while mocks were still coming or being persisted, or waited past the first quiet look", looked, len(steps))
	}
	if lost || tail != tailQuiet {
		t.Fatalf("a drain ended on quiet: lost %v, unconfirmed %v, want not lost and tailQuiet", lost, tail)
	}
}

// tellingTrickle is a trickle from an agent that can tell: until it has handed
// over its last mock it says it may still hand one over from before the run
// ended, its progress moving with each (AgentClient.BacklogBefore).
type tellingTrickle struct {
	trickleInstrumentation
	asked atomic.Int32
}

func (d *tellingTrickle) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	d.asked.Add(1)
	if n := d.sent.Load(); int(n) < d.count {
		return models.AgentBacklog{Verdict: models.BacklogPending, Progress: uint64(n)}, nil
	}
	return models.AgentBacklog{Verdict: models.BacklogClear}, nil
}

// A backlog the agent is still handing over when the runner exits is drained
// for as long as the agent says it may still hand over a mock from before the
// run ended, however far apart they come: further apart than the drain's
// quiet bound, and for longer in all than the fixed 5 s it was once cut off
// at. The end is the agent's word, not the stream's quiet: a load that slows
// the agent down cannot cut it short.
func TestRecord_DrainIsNotCutOffWhileMocksStillArrive(t *testing.T) {
	const want = 6
	inst := &tellingTrickle{trickleInstrumentation: trickleInstrumentation{
		out:      make(chan *models.Mock),
		spacing:  2 * mockDrainQuiet, // 6 of them: ~6 s
		count:    want,
		holdOpen: true,
	}}
	db := &recordingMockDB{}
	cfg := &config.Config{}
	cfg.Mock.Name = "backlog-test"
	if err := New(zap.NewNop(), inst, db, nil, FileStore{}, nil, cfg).Record(context.Background()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	inst.wg.Wait()
	if got := db.inserted(); len(got) != want || inst.asked.Load() == 0 {
		t.Fatalf("persisted %d of %d mocks (the agent was asked %d times): the drain was cut off while the agent was still handing them over", len(got), want, inst.asked.Load())
	}
}

// openStreamAgent hands over one mock while the runner runs, then holds its
// stream open until the client ends it, as a live agent does. It cannot be
// asked what it still holds.
type openStreamAgent struct{ trickleInstrumentation }

func (a *openStreamAgent) Run(context.Context, models.RunOptions) models.AppError {
	// Unbuffered: taken by the client before the run ends.
	a.out <- &models.Mock{Name: "mock-0", Kind: models.HTTP}
	ctx := a.captureCtx
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer close(a.out)
		<-ctx.Done()
	}()
	return models.AppError{}
}

// unsupportedAgent is an openStreamAgent that answers the question, but
// cannot tell, and never will: it predates the route, or its capture has no
// watermark (models.BacklogUnsupported).
type unsupportedAgent struct{ *openStreamAgent }

func (unsupportedAgent) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	return models.AgentBacklog{Verdict: models.BacklogUnsupported}, nil
}

// An agent that cannot say what it still holds leaves the drain only the
// stream's quiet to go by: a mock it hands over later than that is not in the
// recording, and nothing says whether there was one. keploy mock record saves
// what it has, and says at WARN, naming the set and its mocks, that it cannot
// confirm the recording holds them all. It used to save it as whole, in
// silence: under load, short of the mocks a slow agent was still handing over.
func TestRecord_ADrainEndedOnQuietIsSaid(t *testing.T) {
	for _, tc := range []struct {
		name string
		inst func(*openStreamAgent) Instrumentation
	}{
		{"an agent that cannot be asked", func(a *openStreamAgent) Instrumentation { return a }},
		{"an agent that cannot tell", func(a *openStreamAgent) Instrumentation { return unsupportedAgent{a} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &openStreamAgent{trickleInstrumentation{out: make(chan *models.Mock)}}
			db := &recordingMockDB{}
			cfg := &config.Config{}
			cfg.Mock.Name = "quiet-test"
			core, logs := observer.New(zapcore.WarnLevel)
			require.NoError(t, New(zap.New(core), tc.inst(a), db, nil, FileStore{}, nil, cfg).Record(context.Background()))
			a.wg.Wait()
			require.Equal(t, []string{"mock-0"}, db.inserted())
			require.True(t, db.promoted.Load())
			warned := logs.FilterLevelExact(zapcore.WarnLevel).FilterMessage(quietTailUnconfirmed).All()
			require.Len(t, warned, 1, "the saved recording was not said to be unconfirmed: %v", logs.All())
			ctx := warned[0].ContextMap()
			require.Equal(t, "quiet-test", ctx["mock-set"])
			require.EqualValues(t, 1, ctx["mocks"])
			require.Equal(t, quietTailUnconfirmedNext, ctx["next_step"])
		})
	}
}

// How the drain ends once the stream has gone quiet, by what the agent says
// and whether the round can end on its markers: only an agent that said it
// holds nothing more, to a round it can end by its markers, confirms the
// tail. Every other end came on quiet alone.
func TestDrainTrailingMocks_OnlyTheAgentsWordConfirmsTheTail(t *testing.T) {
	ender := kindEnder{}
	for _, tc := range []struct {
		name  string
		inst  Instrumentation
		round *roundEnd
		want  unconfirmedTail
	}{
		{"cannot be asked", struct{ Instrumentation }{}, &roundEnd{ender: ender, token: "t"}, tailQuiet},
		{"cannot tell", backlogSays{verdict: models.BacklogUnsupported}, &roundEnd{ender: ender, token: "t"}, tailQuiet},
		{"clear, without markers", backlogSays{verdict: models.BacklogClear}, &roundEnd{ender: ender, token: "t"}, tailQuiet},
		{"clear with markers, no round to end", backlogSays{verdict: models.BacklogClear, markers: true}, nil, tailQuiet},
		{"clear with markers", backlogSays{verdict: models.BacklogClear, markers: true}, &roundEnd{ender: ender, token: "t"}, tailConfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &mockService{logger: zap.NewNop(), instrumentation: tc.inst}
			lost, tail := m.drainTrailingMocks(make(chan struct{}), time.Now(), true, func() (uint64, bool) { return 0, false }, func() uint64 { return 0 }, recordDrainWords, tc.round)
			if lost || tail != tc.want {
				t.Fatalf("lost %v, unconfirmed %v, want not lost and %v", lost, tail, tc.want)
			}
		})
	}
}

// backlogSays answers every look with verdict, and says it ends its streams
// on request when markers is set.
type backlogSays struct {
	Instrumentation
	verdict models.BacklogVerdict
	markers bool
}

func (b backlogSays) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	return models.AgentBacklog{Verdict: b.verdict, EndMarkers: b.markers}, nil
}

// A producer that never finishes ends the drain in a named loss, never a
// silent cut: an agent that says look after look that it still holds a mock
// from before the run ended, its capture making no progress and nothing
// coming, is given up after mockDrainStallLooks looks. It is said at ERROR,
// the run fails with keploy's exit code, and the drain is lost: its caller
// saves nothing.
func TestDrainTrailingMocks_AProducerThatHangsIsANamedLoss(t *testing.T) {
	defer func(n int) { mockDrainStallLooks = n }(mockDrainStallLooks)
	mockDrainStallLooks = 2
	defer func(code int) { utils.ErrCode = code }(utils.ErrCode)
	utils.ErrCode = 0
	core, logs := observer.New(zapcore.ErrorLevel)
	m := &mockService{logger: zap.New(core), instrumentation: alwaysPending{}}
	lost, tail := m.drainTrailingMocks(make(chan struct{}), time.Now(), true, func() (uint64, bool) { return 0, false }, func() uint64 { return 0 }, recordDrainWords, nil)
	if !lost || tail != tailConfirmed {
		t.Fatalf("a drain on an agent that never finishes: lost %v, unconfirmed %v, want lost", lost, tail)
	}
	require.Equal(t, 1, logs.FilterMessage(recordDrainWords.stalled).Len(), "%v", logs.All())
	require.Equal(t, utils.ExitKeployError, utils.ErrCode)
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
	defer cancel()
	// The user interrupts as the test command runs, once the recording is
	// set up. A stop before Record raced the setup gate (NewSetupGate's
	// AfterFunc against its Up), and a Ctrl+C during setup rightly ends the
	// run before anything is captured: the row failed 2 runs in 300.
	inst.onRun = cancel
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

// A stop delivered twice is one stop: the copy of a signal that sudo hands on
// (`sudo pkill -INT -f keploy` reaches keploy directly and from sudo) comes
// before the drain says another interrupt stops it now (utils.OfferStopNow),
// and ends nothing. Taken for a second Ctrl+C, it gave the drain up at once,
// and the mock the agent still held was lost. The drain offers only a whole
// notice period after the stop's signal came: on the test's clock, which
// stands still, never -- the copy comes after its notices have begun.
func TestRecord_AStopDeliveredTwiceStillSavesWhatTheAgentHolds(t *testing.T) {
	defer func(d time.Duration) { mockDrainNotice = d }(mockDrainNotice)
	mockDrainNotice = 20 * time.Millisecond
	utils.ResetInterruptedAgain()
	utils.ClearInterrupted()
	t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted() })
	utils.MarkInterrupted() // the stop
	at := time.Now()
	defer utils.SetNoticeClockForTest(func() time.Time { return at })()
	inst := &pendingInstrumentation{
		trickleInstrumentation: trickleInstrumentation{out: make(chan *models.Mock)},
		quietFor:               4 * mockDrainQuiet,
	}
	db := &recordingMockDB{}
	cfg := &config.Config{}
	cfg.Mock.Name = "duplicate-stop"
	core, logs := observer.New(zapcore.InfoLevel)
	returned := make(chan error, 1)
	go func() {
		returned <- New(zap.New(core), inst, db, nil, FileStore{}, nil, cfg).Record(context.Background())
	}()
	deadline := time.After(time.Minute) // only turns a hang into a failure
	for logs.FilterMessageSnippet(recordDrainWords.saving).Len() == 0 {
		select {
		case err := <-returned:
			t.Fatalf("Record returned (%v) before its drain gave a notice: %v", err, logs.All())
		case <-deadline:
			t.Fatalf("the drain gave no notice: %v", logs.All())
		case <-time.After(time.Millisecond):
		}
	}
	utils.MarkInterruptedAgain() // the copy, after the drain's notices began
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("Record: %v", err)
		}
	case <-deadline:
		t.Fatal("Record never returned")
	}
	inst.wg.Wait()
	if got := db.inserted(); len(got) != 1 {
		t.Fatalf("persisted %v: the copy of the stop gave the drain up before the agent handed its mock over", got)
	}
	if logs.FilterMessage(recordDrainWords.gaveUp).Len() != 0 {
		t.Fatalf("the copy of the stop was taken for a second interrupt: %v", logs.All())
	}
}

// A drain the user gives up at its offer (a second Ctrl+C) leaves out the mock
// the agent still holds: the recording is not saved over the set, and the run
// fails. keploy mock record's command returns no error for a run the user
// stopped (an interrupt is not a failure), so the exit code is what says it:
// 128+N for the signal that stopped the run, as keploy mock replay exits for a
// run the user stopped before it finished, or keploy's 1 where no signal's
// number is known (a build with its own handler). Before, it was 0, a pass.
func TestRecord_ADrainTheUserGaveUpIsNotSavedAndExitsNonZero(t *testing.T) {
	defer func(d time.Duration) { mockDrainNotice = d }(mockDrainNotice)
	mockDrainNotice = 20 * time.Millisecond
	for _, tc := range []struct {
		name         string
		signal, code int
	}{{"stopped by SIGINT", 2, 130}, {"no signal number known", 0, utils.ExitKeployError}} {
		t.Run(tc.name, func(t *testing.T) {
			utils.ResetInterruptedAgain()
			utils.ClearInterrupted()
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted(); utils.ErrCode = 0 })
			t.Cleanup(utils.SetInterruptSignalForTest(tc.signal))
			utils.MarkInterrupted() // the stop
			inst := &pendingInstrumentation{
				trickleInstrumentation: trickleInstrumentation{out: make(chan *models.Mock)},
				quietFor:               time.Hour, // the agent still holds its mock when the user gives up
			}
			db := &recordingMockDB{}
			cfg := &config.Config{}
			cfg.Mock.Name = "given-up-drain"
			core, logs := observer.New(zapcore.InfoLevel)
			returned := make(chan error, 1)
			go func() {
				returned <- New(zap.New(core), inst, db, nil, FileStore{}, nil, cfg).Record(context.Background())
			}()
			if err, early := untilSaid(t, logs, recordDrainWords.saving+"; "+recordDrainWords.stopNow, returned); early {
				t.Fatalf("Record returned (%v) before its drain offered to stop now", err)
			}
			if !utils.MarkInterruptedAgain() { // the user, as told
				t.Fatal("setup: no offer standing")
			}
			err := returnedWithin(t, returned)
			inst.wg.Wait()
			require.Error(t, err, "a recording short of the mock the agent held ended as a success")
			require.False(t, db.promoted.Load(), "a recording short of the mock the agent held was saved over the set")
			require.Equal(t, 1, logs.FilterLevelExact(zapcore.WarnLevel).FilterMessage(recordDrainWords.gaveUp).Len(), "the give-up was not said: %v", logs.All())
			require.Equal(t, tc.code, utils.ErrCode, "a recording the user gave up, not saved, left exit code %d", utils.ErrCode)
		})
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
	refused error // what a look gets once it has gone (refusedErr)
	looked  chan struct{}
	once    sync.Once
	asked   atomic.Int32
}

func (g *goneInstrumentation) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	n := g.asked.Add(1)
	if g.looked == nil {
		// Gone before the drain asks: a look that comes anyway (a slow
		// machine) finds it unreachable, and says nothing of what it held.
		return models.AgentBacklog{}, g.refused
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
	inst := &goneInstrumentation{trickleInstrumentation: trickleInstrumentation{out: make(chan *models.Mock)}, refused: refusedErr(t)}
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
	// agent unreachable (refused): it ends the stream there.
	goneAtSecond func()
	refused      error
}

// refusedErr is the error of a real refused dial: the platform's own, as an
// agent that is gone gives it. The port dialled is held for the test's life
// with nothing listening there (testnet.RefusedAddr), so no other socket can
// take it and answer the dial.
func refusedErr(t *testing.T) error {
	t.Helper()
	c, err := net.Dial("tcp", testnet.RefusedAddr(t, "tcp"))
	if err == nil {
		_ = c.Close()
	}
	require.True(t, neterr.IsConnRefused(err), "a dial to a held port nothing listens at was not refused: %v", err)
	return err
}

func (g *gatedBacklog) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	n := g.asked.Add(1)
	g.once.Do(func() {
		close(g.entered)
		<-g.gate
	})
	if n >= 2 && g.goneAtSecond != nil {
		g.goneAtSecond()
		return models.AgentBacklog{}, g.refused
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
		l, _ := m.drainTrailingMocks(consumerDone, time.Now(), true, func() (uint64, bool) { return frames.Load(), false }, frames.Load, recordDrainWords, nil)
		lost.Store(l)
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
		inst.refused = refusedErr(t)
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
	lost, logs, code := gatedDrain(t, refusedErr(t), &frames, func() {})
	if lost || code != 0 || logs.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet("ended its mock stream before it said").Len() != 1 {
		t.Fatalf("want one WARN and the run not failed: lost=%v exit code %d: %v", lost, code, logs.All())
	}
}

// A second interrupt that reaches the agent too ends its stream as the user
// gives the drain up: the user's stop, not the agent going away. What the agent
// still held is not saved, so the drain is lost (the set is not saved over the
// existing one), said at WARN as the user's stop. Both are ready when the
// drain's look returns: a drain that went by the order its select took them in
// failed about half its runs, and 8 runs catch that all but surely.
func TestDrainTrailingMocks_ASecondInterruptAsTheStreamEndsIsTheUsersStop(t *testing.T) {
	defer utils.ResetInterruptedAgain()
	for i := 0; i < 8; i++ {
		var frames atomic.Uint64
		frames.Store(1)
		// The drain has said another interrupt stops it now: this one does.
		lost, logs, code := gatedDrain(t, nil, &frames, func() {
			utils.MarkInterrupted() // the stop
			utils.OfferStopNow()
			utils.MarkInterruptedAgain()
		})
		utils.ClearInterrupted()
		utils.ResetInterruptedAgain()
		if !lost || code != 0 || logs.FilterMessageSnippet("ended its mock stream").Len() != 0 {
			t.Fatalf("run %d: want the user's stop, the drain lost and not the agent going away (lost=%v, exit code %d): %v", i, lost, code, logs.All())
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
	lost, logs, code := gatedDrain(t, refusedErr(t), &frames, func() { frames.Add(1) }, true)
	if lost || code != 0 || logs.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet("ended its mock stream before it said").Len() != 1 {
		t.Fatalf("want one WARN and the run not failed: lost=%v exit code %d: %v", lost, code, logs.All())
	}
}

// lifetimeInstr is an agent whose lifetime is the context its Setup got, as a
// native agent's is: the user presses Ctrl+C while the runner runs, and the
// drain then asks the agent what it still holds.
type lifetimeInstr struct {
	trickleInstrumentation
	setupCtx  context.Context
	interrupt func()
	aliveAtLook,
	looked atomic.Bool
}

func (l *lifetimeInstr) Setup(ctx context.Context, _ string, _ models.SetupOptions) error {
	l.setupCtx = ctx
	return nil
}

func (l *lifetimeInstr) Run(context.Context, models.RunOptions) models.AppError {
	l.interrupt() // Ctrl+C
	// A live agent keeps its stream open until the client ends it.
	captureCtx := l.captureCtx
	go func() {
		<-captureCtx.Done()
		close(l.out)
	}()
	return models.AppError{AppErrorType: models.ErrCtxCanceled}
}

func (l *lifetimeInstr) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	if !l.looked.Swap(true) {
		l.aliveAtLook.Store(l.setupCtx.Err() == nil)
	}
	return models.AgentBacklog{Verdict: models.BacklogClear}, nil
}

// A Ctrl+C stops the runner, not the agent: the agent lives until the drain has
// asked it what it still holds, and the teardown stops it after. Stopped as the
// interrupt landed, it was gone before the drain, and what it held was lost.
func TestRecord_ACtrlCLeavesTheAgentRunningUntilTheDrainIsDone(t *testing.T) {
	parent, interrupt := context.WithCancel(context.Background())
	defer interrupt()
	inst := &lifetimeInstr{trickleInstrumentation: trickleInstrumentation{out: make(chan *models.Mock, 1)}, interrupt: interrupt}
	cfg := &config.Config{}
	cfg.Mock.Name = "lifetime-test"
	if err := New(zap.NewNop(), inst, &recordingMockDB{}, nil, FileStore{}, nil, cfg).Record(parent); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !inst.looked.Load() {
		t.Fatal("the drain never asked the agent")
	}
	if !inst.aliveAtLook.Load() {
		t.Fatal("the agent was stopped by the Ctrl+C, before the drain asked it what it still held")
	}
	if inst.setupCtx.Err() == nil {
		t.Fatal("the agent was never stopped")
	}
}

// ownedGone is goneInstrumentation for an agent this client started
// (AgentClient.StartedAgent): it holds its stream open for this drain.
type ownedGone struct{ *goneInstrumentation }

func (ownedGone) StartedAgent() bool { return true }

// An agent this client started ends its stream during the drain, before the
// drain asked it anything: it went away before the run could be confirmed
// complete. The run fails, and the existing set is kept.
func TestRecord_AnAgentThisClientStartedGoneBeforeTheDrainAsksFailsTheRun(t *testing.T) {
	defer func(code int) { utils.ErrCode = code }(utils.ErrCode)
	utils.ErrCode = 0
	inst := &goneInstrumentation{trickleInstrumentation: trickleInstrumentation{out: make(chan *models.Mock)}, refused: refusedErr(t)}
	core, logs := observer.New(zapcore.WarnLevel)
	cfg := &config.Config{}
	cfg.Mock.Name = "owned-gone-test"
	db := &recordingMockDB{}
	err := New(zap.New(core), ownedGone{inst}, db, nil, FileStore{}, nil, cfg).Record(context.Background())
	inst.wg.Wait()
	if err == nil || utils.ErrCode != utils.ExitKeployError || logs.FilterLevelExact(zapcore.ErrorLevel).FilterMessageSnippet("before it confirmed it had handed over").Len() != 1 {
		t.Fatalf("want the run failed at ERROR (error %v, exit code %d): %v", err, utils.ErrCode, logs.All())
	}
	if db.promoted.Load() {
		t.Fatal("a run that could not be confirmed complete was promoted over the existing set")
	}
}

// interruptedSetup is an agent whose setup the user's Ctrl+C interrupts as it
// finishes: Setup returns only once the interrupt has stopped the agent (its
// context, the agent's lifetime, is done).
type interruptedSetup struct {
	trickleInstrumentation
	interrupt func()
	captured  atomic.Bool
}

func (i *interruptedSetup) Setup(ctx context.Context, _ string, _ models.SetupOptions) error {
	i.interrupt()
	<-ctx.Done()
	return nil
}

func (i *interruptedSetup) GetOutgoing(ctx context.Context, o models.OutgoingOptions) (<-chan *models.Mock, error) {
	i.captured.Store(true)
	return i.trickleInstrumentation.GetOutgoing(ctx, o)
}

// The user's interrupt that stopped the agent as its setup finished ends the
// run as an interrupted setup: no error, and nothing captured with an agent
// that is stopping (utils.SetupGate). Keploy's own failure there is not one
// (TestARecordingKeployFailedIsNotAnInterrupt).
func TestRecord_AnInterruptThatStoppedTheSetupEndsTheRun(t *testing.T) {
	parent, interrupt := context.WithCancel(context.Background())
	defer interrupt()
	inst := &interruptedSetup{trickleInstrumentation: trickleInstrumentation{out: make(chan *models.Mock, 1)}, interrupt: interrupt}
	cfg := &config.Config{}
	cfg.Mock.Name = "interrupted-setup"
	if err := New(zap.NewNop(), inst, &recordingMockDB{}, nil, FileStore{}, nil, cfg).Record(parent); err != nil {
		t.Fatalf("an interrupted setup ended with %v, want nil", err)
	}
	if inst.captured.Load() {
		t.Fatal("the run went on to capture with an agent the interrupt had stopped")
	}
}

// errAgentExited is an agent process's exit, as its wait goroutine hands it to
// keploy's errgroup.
var errAgentExited = errors.New("keploy agent exited: signal: killed")

// diedAgent is an agent this client started that dies under the test command:
// its process's exit fails keploy's errgroup, as the native agent's wait
// goroutine does, and then its mock stream ends, before the drain asks it
// anything.
type diedAgent struct {
	ownedGone
	exited chan struct{}
}

func (d diedAgent) Setup(ctx context.Context, _ string, _ models.SetupOptions) error {
	grp, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("fixture: no errgroup in the setup's context")
	}
	grp.Go(func() error {
		<-d.exited
		return errAgentExited
	})
	return nil
}

func (d diedAgent) Run(ctx context.Context, o models.RunOptions) models.AppError {
	close(d.exited)
	<-ctx.Done() // keploy's errgroup has failed with the agent's exit
	return d.goneInstrumentation.Run(ctx, o)
}

// A run whose agent died under it, not stopped by the user, fails with why:
// the agent's exit, as a run that did not finish says it, on top of the mocks
// it could not confirm. The set is not promoted.
func TestRecord_AnAgentThatDiedUnderTheRunFailsWithItsExit(t *testing.T) {
	defer func(code int) { utils.ErrCode = code }(utils.ErrCode)
	utils.ErrCode = 0
	inst := diedAgent{
		ownedGone: ownedGone{&goneInstrumentation{trickleInstrumentation: trickleInstrumentation{out: make(chan *models.Mock)}, refused: refusedErr(t)}},
		exited:    make(chan struct{}),
	}
	cfg := &config.Config{}
	cfg.Mock.Name = "died-test"
	db := &recordingMockDB{}
	err := New(zap.NewNop(), inst, db, nil, FileStore{}, nil, cfg).Record(context.Background())
	inst.wg.Wait()
	if !errors.Is(err, errAgentExited) {
		t.Fatalf("the run failed with %v, want the agent's exit (%v) in it", err, errAgentExited)
	}
	if db.promoted.Load() {
		t.Fatal("a run whose agent died under it was promoted over the existing set")
	}
}

// alwaysPending is an agent that says it still holds what it captured before
// the run ended, at every look: a drain on it waits until it is stopped.
type alwaysPending struct{ Instrumentation }

func (alwaysPending) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	return models.AgentBacklog{Verdict: models.BacklogPending}, nil
}

// The drain tells each caller's user what ending it at a second interrupt
// costs, in the caller's terms: mock record does not save the recording at
// all (the existing set is kept), not just "the rest"; replay leaves requests
// out of its report, and has no mock set to speak of. Its notice offers once
// a whole notice period after the stop's signal, and the user's interrupt
// then ends it, said in the same terms.
func TestDrainTrailingMocks_SaysWhatStoppingNowCostsItsCaller(t *testing.T) {
	defer func(d time.Duration) { mockDrainNotice = d }(mockDrainNotice)
	mockDrainNotice = 20 * time.Millisecond
	for name, words := range map[string]drainWords{"record": recordDrainWords, "replay": replayDrainWords} {
		t.Run(name, func(t *testing.T) {
			utils.ResetInterruptedAgain()
			utils.ClearInterrupted()
			t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted() })
			utils.MarkInterrupted() // the stop
			core, logs := observer.New(zapcore.InfoLevel)
			m := &mockService{logger: zap.New(core), instrumentation: alwaysPending{}}
			lost := make(chan bool, 1)
			go func() {
				l, _ := m.drainTrailingMocks(make(chan struct{}), time.Now(), false,
					func() (uint64, bool) { return 0, false }, func() uint64 { return 0 }, words, nil)
				lost <- l
			}()
			offered := words.saving + "; " + words.stopNow
			deadline := time.Now().Add(30 * time.Second) // bounds only a drain that never offers
			for logs.FilterMessage(offered).Len() == 0 {
				if time.Now().After(deadline) {
					t.Fatalf("the drain never offered to stop now: %v", logs.All())
				}
				time.Sleep(time.Millisecond)
			}
			utils.MarkInterruptedAgain() // the user, as told
			select {
			case l := <-lost:
				if !l {
					t.Fatal("a drain the user ended was not lost")
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the user's interrupt after the offer did not end the drain")
			}
			warned := logs.FilterLevelExact(zapcore.WarnLevel).FilterMessage(words.gaveUp).All()
			if len(warned) != 1 || warned[0].ContextMap()["next_step"] != words.nextStep {
				t.Fatalf("the user's stop was not said in the caller's terms: %v", logs.All())
			}
		})
	}
}

// Under docker compose the agent goes with the project and closes its mock
// stream without a word that it handed over every mock: the drain is not
// lost, nothing says a mock is missing, but it is unconfirmed. The drain says
// nothing of it itself: only its caller knows whether it then saves anything.
// An agent that holds its stream for the drain is never unconfirmed: its
// close is judged by agentEndedDrain.
func TestDrainTrailingMocks_AComposeAgentsClosedStreamIsUnconfirmed(t *testing.T) {
	for _, holds := range []bool{false, true} {
		core, logs := observer.New(zapcore.WarnLevel)
		m := &mockService{logger: zap.New(core), instrumentation: alwaysPending{}}
		closed := make(chan struct{})
		close(closed)
		lost, unconfirmed := m.drainTrailingMocks(closed, time.Now(), holds, func() (uint64, bool) { return 0, false }, func() uint64 { return 0 }, recordDrainWords, nil)
		if (unconfirmed == tailStreamClosed) != !holds || (holds && unconfirmed != tailConfirmed) || (!holds && lost) {
			t.Fatalf("agent holds %v, its stream closed: lost %v, unconfirmed %v", holds, lost, unconfirmed)
		}
		if n := logs.FilterMessage(composeTailUnconfirmed).Len(); n != 0 {
			t.Fatalf("the drain said the tail is unconfirmed %d time(s) before its caller saved anything", n)
		}
	}
}

// composeOneMock is a compose project whose agent hands over one mock, then
// closes its stream as compose stops it with the project.
type composeOneMock struct{ *composeInstr }

func (c composeOneMock) GetOutgoing(ctx context.Context, o models.OutgoingOptions) (<-chan *models.Mock, error) {
	if _, err := c.composeInstr.GetOutgoing(ctx, o); err != nil {
		return nil, err
	}
	out := make(chan *models.Mock, 1)
	out <- &models.Mock{Name: "mock-0", Kind: models.HTTP}
	close(out)
	return out, nil
}

// keploy mock record says at WARN, naming the set, that it cannot confirm a
// compose recording holds every mock, once it has saved it, and only then: a
// recording it does not keep is not said to be saved.
func TestAComposeRecordingSavedUnconfirmedIsSaid(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mocks  bool
		res    models.AppError
		failed func() error
		said   bool
	}{
		{name: "saved", mocks: true, res: models.AppError{AppErrorType: models.ErrAppStopped}, said: true},
		{name: "tests failed: not kept", mocks: true, res: models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: 2}},
		{name: "nothing captured: not kept", res: models.AppError{AppErrorType: models.ErrAppStopped}},
		{name: "the agent died: not kept", mocks: true, res: models.AppError{AppErrorType: models.ErrAppStopped}, failed: func() error { return errors.New("keploy-agent exited (137)") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.WarnLevel)
			instr := newInstr(t, agentUpAfterRun, false, tc.res)
			instr.runUntilReady = true
			instr.agentFailure = tc.failed
			var inst Instrumentation = instr
			if tc.mocks {
				inst = composeOneMock{instr}
			}
			cfg := instrConfig(instr, utils.DockerCompose, "docker compose up")
			cfg.Mock.Name = "e2e"
			cfg.Path = t.TempDir()
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			_ = New(zap.New(core), inst, stubMockDB{}, nil, nil, nil, cfg).Record(context.Background())
			warned := logs.FilterMessage(composeTailUnconfirmed).All()
			if said := len(warned) == 1; said != tc.said {
				t.Fatalf("said the saved recording cannot be confirmed whole: %v, want %v (%v)", said, tc.said, logs.All())
			}
			if tc.said && (warned[0].ContextMap()["mock-set"] != "e2e" || warned[0].ContextMap()["mocks"] != int64(1)) {
				t.Fatalf("the warning does not name the set and its mocks: %v", warned[0].ContextMap())
			}
		})
	}
}
