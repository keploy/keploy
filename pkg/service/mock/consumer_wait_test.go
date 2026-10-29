package mock

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// untilSaid waits until logs hold a message with snippet, or until the run
// returns first (early): what it returned is err.
func untilSaid(t *testing.T, logs *observer.ObservedLogs, snippet string, returned <-chan error) (err error, early bool) {
	t.Helper()
	guard := time.After(time.Minute) // only turns a hang into a failure
	for logs.FilterMessageSnippet(snippet).Len() == 0 {
		select {
		case err := <-returned:
			return err, true
		case <-guard:
			t.Fatalf("never said %q: %v", snippet, logs.All())
		case <-time.After(time.Millisecond):
		}
	}
	return nil, false
}

func returnedWithin(t *testing.T, returned <-chan error) error {
	t.Helper()
	select {
	case err := <-returned:
		return err
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the run never returned")
		return nil
	}
}

// afterExitInstrumentation is a runner that made one call while it ran
// (mock-0), and left something running that makes one more once it has
// exited, as the drain looks at the agent (late): the agent then holds
// nothing more from before the exit, and the drain does not wait for late.
type afterExitInstrumentation struct {
	trickleInstrumentation
	once sync.Once
}

func (a *afterExitInstrumentation) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	a.once.Do(func() {
		a.out <- &models.Mock{Name: "late", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: time.Now().Add(time.Hour)}}
	})
	return models.AgentBacklog{Verdict: models.BacklogClear}, nil
}

func (a *afterExitInstrumentation) Run(context.Context, models.RunOptions) models.AppError {
	a.out <- &models.Mock{Name: "mock-0", Kind: models.HTTP}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		<-a.captureCtx.Done() // a live agent keeps its stream until the client ends it
		close(a.out)
	}()
	return models.AppError{}
}

// heldWriteDB is a disk that does not return the write of one mock (held)
// until it is let go, or after holdFor of its own (0: never), or until the
// write's context ends. That write touches nothing Record reads, so a Record
// that reads what its consumer counted before the consumer has ended races
// with it under -race.
type heldWriteDB struct {
	recordingMockDB
	held    string
	holdFor time.Duration

	release   chan struct{}
	releaseMu sync.Once
	wrote     chan struct{} // closed once the held write returned
	gaveUp    atomic.Bool   // the held write's context ended first
	unstick   chan struct{} // the test's end
	// heldSaved: the held mock was written when the set was promoted.
	heldSaved bool
}

func newHeldWriteDB(t *testing.T, held string, holdFor time.Duration) *heldWriteDB {
	d := &heldWriteDB{held: held, holdFor: holdFor, release: make(chan struct{}), wrote: make(chan struct{}), unstick: make(chan struct{})}
	t.Cleanup(func() { close(d.unstick) })
	return d
}

func (d *heldWriteDB) letGo() { d.releaseMu.Do(func() { close(d.release) }) }

func (d *heldWriteDB) InsertMock(ctx context.Context, m *models.Mock, set string) error {
	if m.Name != d.held {
		return d.recordingMockDB.InsertMock(ctx, m, set)
	}
	if d.holdFor > 0 {
		time.AfterFunc(d.holdFor, d.letGo)
	}
	defer close(d.wrote)
	select {
	case <-d.release:
		return nil
	case <-ctx.Done():
		d.gaveUp.Store(true)
		return ctx.Err()
	case <-d.unstick:
		return errors.New("the test ended")
	}
}

func (d *heldWriteDB) PromoteStagedSet(ctx context.Context, staging, name string) error {
	select {
	case <-d.wrote:
		d.heldSaved = !d.gaveUp.Load()
	default:
	}
	return d.recordingMockDB.PromoteStagedSet(ctx, staging, name)
}

// A mock the consumer is still writing once the capture has stopped is part
// of the recording, however long the disk takes with it: Record waits for the
// consumer to end, says what it still holds while it does, and only then reads
// what the consumer counted. Before, a consumer that took nothing for 5 s was
// given up: the set was saved without the mock it was writing, as a whole
// recording, and what it counted (mockCount, recorded) was read as it wrote
// them (-race). The held write here lands on its own 8 s after it began, past
// that 5 s, unordered with anything Record does.
func TestRecord_WaitsForAMockStillBeingWrittenAfterTheCaptureStops(t *testing.T) {
	defer func(d time.Duration) { mockDrainNotice = d }(mockDrainNotice)
	mockDrainNotice = 20 * time.Millisecond
	inst := &afterExitInstrumentation{trickleInstrumentation: trickleInstrumentation{out: make(chan *models.Mock, 4)}}
	db := newHeldWriteDB(t, "late", 8*time.Second)
	cfg := &config.Config{}
	cfg.Mock.Name = "held-write"
	core, logs := observer.New(zapcore.InfoLevel)
	returned := make(chan error, 1)
	go func() {
		returned <- New(zap.New(core), inst, db, nil, FileStore{}, nil, cfg).Record(context.Background())
	}()
	if err, early := untilSaid(t, logs, recordDrainWords.writing, returned); early {
		<-db.wrote                         // the held write lands on its own
		time.Sleep(100 * time.Millisecond) // and its count after it, for -race to name here
		t.Fatalf("Record returned (%v) with a mock still being written: the set was saved with %v and the held mock in it: %v", err, db.inserted(), db.heldSaved)
	}
	said := logs.FilterMessageSnippet(recordDrainWords.writing).All()[0].ContextMap()
	if said["mocksLeft"] != uint64(1) || said["mocksDone"] != uint64(1) {
		t.Fatalf("the notice did not say what the consumer still holds: %v", said)
	}
	db.letGo()
	require.NoError(t, returnedWithin(t, returned))
	inst.wg.Wait()
	require.True(t, db.heldSaved, "the set was saved before the held mock was written")
	recorded := logs.FilterMessage("recorded mocks").All()
	require.Len(t, recorded, 1)
	require.Equal(t, int64(2), recorded[0].ContextMap()["mocks"])
}

// A write that never returns (a stuck disk) is not given up on by a clock:
// the run waits, says so, and offers the user to stop it now. The user's
// second interrupt ends it as a recording that is NOT saved, said at ERROR
// with what was not written, and the existing set is kept; the held write's
// context ends, so a write waiting for room gives up. Before, the consumer was
// given up after 5 s without progress and the set was saved without the mock,
// as a whole recording.
func TestRecord_AStuckWriteEndsAtTheUsersInterruptAsARecordingNotSaved(t *testing.T) {
	defer func(d time.Duration) { mockDrainNotice = d }(mockDrainNotice)
	mockDrainNotice = 20 * time.Millisecond
	utils.ResetInterruptedAgain()
	utils.ClearInterrupted()
	t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted() })
	utils.MarkInterrupted() // the stop
	inst := &afterExitInstrumentation{trickleInstrumentation: trickleInstrumentation{out: make(chan *models.Mock, 4)}}
	db := newHeldWriteDB(t, "late", 0)
	cfg := &config.Config{}
	cfg.Mock.Name = "stuck-write"
	core, logs := observer.New(zapcore.InfoLevel)
	returned := make(chan error, 1)
	go func() {
		returned <- New(zap.New(core), inst, db, nil, FileStore{}, nil, cfg).Record(context.Background())
	}()
	if err, early := untilSaid(t, logs, recordDrainWords.writing+"; "+recordDrainWords.stopNow, returned); early {
		t.Fatalf("Record returned (%v) while a write had not returned: the set was saved (%v) with %v", err, db.promoted.Load(), db.inserted())
	}
	if !utils.MarkInterruptedAgain() { // the user, as told
		t.Fatal("setup: no offer standing")
	}
	require.Error(t, returnedWithin(t, returned), "a recording short of a mock it was handed ended as a success")
	require.False(t, db.promoted.Load(), "a recording short of a mock it was handed was saved over the set")
	select {
	case <-db.wrote:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the stuck write's context did not end when the user gave it up")
	}
	require.True(t, db.gaveUp.Load(), "the stuck write ended, but not at its context")
	failed := logs.FilterLevelExact(zapcore.ErrorLevel).FilterMessage(recordDrainWords.notWritten).All()
	require.Len(t, failed, 1, "the mocks not written were not said: %v", logs.All())
	fields := failed[0].ContextMap()
	require.Equal(t, uint64(1), fields["mocksLeft"])
	require.Equal(t, uint64(1), fields["mocksDone"])
	require.Equal(t, recordDrainWords.nextStep, fields["next_step"])
}

// heldTestDB stores test cases, but does not return the store of one (held)
// until it is let go, or its context ends.
type heldTestDB struct {
	memTestDB
	held    string
	release chan struct{}
}

func (d *heldTestDB) InsertTestCase(ctx context.Context, tc *models.TestCase, set string, b bool) error {
	if tc.Name == d.held {
		select {
		case <-d.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return d.memTestDB.InsertTestCase(ctx, tc, set, b)
}

// Test cases still being stored once the capture has stopped are part of the
// recording too: Record waits for them and maps them to their tests. Before,
// a case store that took nothing for 5 s was given up, and the cases still in
// it were left out of the mapping, with nothing said.
func TestRecord_WaitsForTestCasesStillBeingStoredAfterTheCaptureStops(t *testing.T) {
	defer func(d time.Duration) { mockDrainNotice = d }(mockDrainNotice)
	mockDrainNotice = 20 * time.Millisecond
	instr := newRunnerInstr(t, sequentialMarks()...)
	instr.incoming = []*models.TestCase{
		caseAt("test-1", runnerT0.Add(5*time.Millisecond)),
		caseAt("test-2", runnerT0.Add(15*time.Millisecond)),
	}
	db := &heldTestDB{held: "test-2", release: make(chan struct{})}
	defer func() {
		select {
		case <-db.release:
		default:
			close(db.release)
		}
	}()
	dir := t.TempDir()
	cfg := instrConfig(instr.composeInstr, utils.Native, "./shop.test -test.v")
	cfg.Path = t.TempDir()
	withRequests(cfg)
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	core, logs := observer.New(zapcore.InfoLevel)
	svc := New(zap.New(core), instr, stubMockDB{}, mapdb.New(zap.NewNop(), dir, ""), nil, nil, cfg)
	svc.(TestDBSetter).SetTestDB(db)
	returned := make(chan error, 1)
	go func() { returned <- svc.Record(context.Background()) }()
	if err, early := untilSaid(t, logs, recordDrainWords.writing, returned); early {
		t.Fatalf("Record returned (%v) with a test case still being stored: it mapped %v", err, mappedCases(t, dir))
	}
	if left := logs.FilterMessageSnippet(recordDrainWords.writing).All()[0].ContextMap()["casesLeft"]; left != uint64(1) {
		t.Fatalf("the notice did not say the case still being stored: %v", left)
	}
	close(db.release)
	require.NoError(t, returnedWithin(t, returned))
	require.Equal(t, map[string][]string{
		"orders/e2e.TestA": {"test-1"},
		"orders/e2e.TestC": {"test-2"},
	}, mappedCases(t, dir))
}

// afterStopIncoming is a replay's request stream that hands over its last
// request only once the watch has stopped and the test lets it go.
type afterStopIncoming struct {
	*runnerInstr
	tc      *models.TestCase
	release chan struct{}
}

func (a afterStopIncoming) GetIncoming(ctx context.Context, _ models.IncomingOptions) (<-chan *models.TestCase, error) {
	out := make(chan *models.TestCase)
	go func() {
		defer close(out)
		<-ctx.Done()
		<-a.release
		out <- a.tc
	}()
	return out, nil
}

// A replay compares every request its stream hands over, however long the
// stream takes to end once the watch has stopped. Before, a stream that
// brought nothing for 5 s was given up and the run judged without what it
// still had: a request it had not handed over yet read as one the app never
// made.
func TestReplayWaitsForARequestStreamStillOpenAfterTheWatchStops(t *testing.T) {
	defer func(d time.Duration) { mockDrainNotice = d }(mockDrainNotice)
	mockDrainNotice = 20 * time.Millisecond
	instr := newRunnerInstr(t, sequentialMarks()...)
	late := afterStopIncoming{runnerInstr: instr, release: make(chan struct{}),
		tc: httpCase("", "POST", "http://localhost:8080/orders", 201, `{}`, runnerT0.Add(5*time.Millisecond))}
	t.Cleanup(func() {
		select {
		case <-late.release:
		default:
			close(late.release)
		}
	})
	dir := t.TempDir()
	mapDB := mapdb.New(zap.NewNop(), dir, "")
	require.NoError(t, mapDB.UpsertCases(context.Background(), "set", map[string]models.MappedTestCase{"orders/e2e.TestA": {Cases: []string{"post-orders-1"}}}, nil, nil))
	db := &memTestDB{existing: []*models.TestCase{httpCase("post-orders-1", "POST", "http://localhost:8080/orders", 201, `{}`, runnerT0)}}
	var got ReplayOutcome
	RegisterReplayOutcomeReporter(func(_ context.Context, o ReplayOutcome) { got = o })
	t.Cleanup(func() { RegisterReplayOutcomeReporter(nil) })
	cfg := instrConfig(instr.composeInstr, utils.Native, "./shop.test -test.v")
	cfg.Path = dir
	withRequests(cfg)
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	core, logs := observer.New(zapcore.InfoLevel)
	svc := New(zap.New(core), late, stubMockDB{}, mapDB, nil, nil, cfg)
	svc.(TestDBSetter).SetTestDB(db)
	returned := make(chan error, 1)
	go func() { returned <- svc.Replay(context.Background()) }()
	if err, early := untilSaid(t, logs, replayDrainWords.writing, returned); early {
		t.Fatalf("the replay returned (%v) with its request stream still open: it judged %d case(s), the first with the app's answer %v", err, len(got.Cases), len(got.Cases) > 0 && got.Cases[0].Actual != nil)
	}
	close(late.release)
	require.NoError(t, returnedWithin(t, returned))
	require.Len(t, got.Cases, 1)
	require.NotNil(t, got.Cases[0].Actual, "the request the stream handed over after the watch stopped was not compared")
	require.True(t, got.Cases[0].Passed)
}

// waitConsumers has no bound: a consumer that takes nothing for a long time
// (here, twenty notices) is still waited for, and each notice says what it
// holds. One that ends as the user gives the wait up has ended: the wait is
// not lost.
func TestWaitConsumers_WaitsWithoutABoundAndEndsOnlyAtTheUsersStop(t *testing.T) {
	defer func(d time.Duration) { mockDrainNotice = d }(mockDrainNotice)
	mockDrainNotice = 5 * time.Millisecond
	core, logs := observer.New(zapcore.InfoLevel)
	m := &mockService{logger: zap.New(core)}
	done := make(chan struct{})
	one := func() uint64 { return 1 }
	ended := make(chan bool, 1)
	go func() {
		ended <- m.waitConsumers(recordDrainWords, streamConsumer{what: "mocks", done: done, taken: func() uint64 { return 3 }, handled: one})
	}()
	for logs.FilterMessage(recordDrainWords.writing).Len() < 20 {
		select {
		case e := <-ended:
			t.Fatalf("the wait ended (%v) with its consumer still working", e)
		case <-time.After(time.Millisecond):
		}
	}
	if left := logs.FilterMessage(recordDrainWords.writing).All()[0].ContextMap()["mocksLeft"]; left != uint64(2) {
		t.Fatalf("the notice said %v mocks left, want 2", left)
	}
	close(done)
	require.True(t, <-ended)

	// A press the user gave an earlier wait of the stop (the drain's offer)
	// does not end this one: a consumer that ends as its stream closes is
	// not given up for it.
	utils.ResetInterruptedAgain()
	utils.ClearInterrupted()
	t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted() })
	utils.MarkInterrupted()
	_, withdraw := utils.OfferStopNow()
	require.True(t, utils.MarkInterruptedAgain())
	withdraw()
	ending := make(chan struct{})
	go func() {
		ended <- m.waitConsumers(recordDrainWords, streamConsumer{what: "mocks", done: ending, taken: one, handled: one})
	}()
	select {
	case e := <-ended:
		t.Fatalf("the wait ended (%v) at a press given to an earlier wait", e)
	case <-time.After(20 * mockDrainNotice):
	}
	close(ending)
	require.True(t, <-ended)
}
