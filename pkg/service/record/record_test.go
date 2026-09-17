package record

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// fakeInstr is a minimal Instrumentation whose mapping stream is driven by the
// test. GetIncoming/GetOutgoing return live-but-silent channels so
// GetTestAndMockChans wires up exactly as it does in production.
type fakeInstr struct {
	mappings chan models.TestMockMapping
	incoming chan *models.TestCase
	outgoing chan *models.Mock
}

func (f *fakeInstr) Setup(context.Context, string, models.SetupOptions) error { return nil }
func (f *fakeInstr) GetIncoming(context.Context, models.IncomingOptions) (<-chan *models.TestCase, error) {
	return f.incoming, nil
}
func (f *fakeInstr) GetOutgoing(context.Context, models.OutgoingOptions) (<-chan *models.Mock, error) {
	return f.outgoing, nil
}
func (f *fakeInstr) GetMappings(context.Context, models.IncomingOptions) (<-chan models.TestMockMapping, error) {
	return f.mappings, nil
}
func (f *fakeInstr) Run(context.Context, models.RunOptions) models.AppError { return models.AppError{} }
func (f *fakeInstr) MakeAgentReadyForDockerCompose(context.Context) error   { return nil }
func (f *fakeInstr) NotifyGracefulShutdown(context.Context) error           { return nil }
func (f *fakeInstr) StreamPcapArtifacts(context.Context, string) error      { return nil }

// TestGetTestAndMockChans_DrainsMappingTailOnShutdown is the other half of the
// go-memory-load-mongo reproduction: persisting the tail is worthless if the tail
// never arrives.
//
// Recording stops and the agent still has resolved mappings queued. The pre-fix
// code cancelled the mapping stream the instant ctx was done, so those never
// reached the consumer at all — no write was attempted, so no error was logged.
// Measured in CI even WITH the write fixed: 27 of 342 tests lost, all tails.
//
// It is tempting to assume teardown stops the app first and the agent has already
// flushed. That holds only for the native path: under docker-compose keploy never
// runs the app, so reqCtx is cancelled within milliseconds of SIGINT with the
// agent's queue still full. That is the configuration this bug was measured in.
func TestGetTestAndMockChans_DrainsMappingTailOnShutdown(t *testing.T) {
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	r := &Recorder{logger: zap.NewNop(), instrumentation: f, config: &config.Config{}}

	g, gctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(gctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)

	frames, err := r.GetTestAndMockChans(ctx)
	require.NoError(t, err)

	var got []string
	collected := make(chan struct{})
	go func() {
		defer close(collected)
		for m := range frames.Mappings {
			got = append(got, m.TestName)
		}
	}()

	// Recording stops FIRST — the real ordering. The agent has already resolved
	// these tests and flushes them on the still-open stream immediately after.
	cancel()

	tail := []string{"post-orders-63", "post-orders-64", "post-orders-65"}
	for _, tn := range tail {
		select {
		case f.mappings <- models.TestMockMapping{TestName: tn, MockIDs: []string{"mock-" + tn}}:
		case <-time.After(5 * time.Second):
			t.Fatalf("mapping for %q was never accepted after shutdown began: the stream was "+
				"torn down while the agent still had it queued, so this test is dropped from "+
				"mappings.yaml and replay reports no_mocks for it", tn)
		}
	}
	close(f.mappings)

	select {
	case <-collected:
	case <-time.After(5 * time.Second):
		t.Fatal("mapping consumer did not finish after the stream closed")
	}

	assert.Equal(t, tail, got,
		"every mapping the agent flushes after shutdown begins must reach the consumer; "+
			"a missing tail is the go-memory-load-mongo no_mocks flake")
}

// TestGetTestAndMockChans_MappingDrainStopsWhenIdle guards the drain itself: the
// agent holds the stream open for the whole session and never closes it, so the
// drain cannot wait for EOF — it must end once the stream falls idle, or recording
// would hang on exit. This one does not fail pre-fix (there was no drain to hang);
// it pins the bound that makes the drain safe.
func TestGetTestAndMockChans_MappingDrainStopsWhenIdle(t *testing.T) {
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	r := &Recorder{logger: zap.NewNop(), instrumentation: f, config: &config.Config{}}

	g, gctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(gctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)

	frames, err := r.GetTestAndMockChans(ctx)
	require.NoError(t, err)

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for range frames.Mappings { //nolint:revive // draining
		}
	}()

	// Shut down and send nothing more: the stream stays open (as the real agent's
	// does), so the drain must exit on the idle bound rather than hang.
	cancel()

	select {
	case <-closed:
	case <-time.After(mappingIdleGrace + 10*time.Second):
		t.Fatal("mapping drain did not stop after the stream fell idle — shutdown would hang")
	}
}

// These tests pin the contract behind the go-memory-load-mongo "no_mocks" flake:
// a record session that is shutting down must still persist everything the agent
// has already streamed to it.
//
// Why the tail is special: recording stops on SIGINT, which cancels the root
// context immediately, but the agent's streams run on reqCtx — deliberately
// WithoutCancel'd — and keep delivering right through teardown (a graceful-
// shutdown notify of up to 10s, then an app drain of up to 30s). Every store
// call in that window used to run on the cancelled context and refuse to write.
// Mappings are emitted last (the agent resolves a test's mock range only once
// that test is done), so the tail of every endpoint landed exactly there: in CI,
// 21 of 327 tests were absent from mappings.yaml, and replay reported
// no_mocks/candidates:0 for precisely those tests.

// TestConsumeMappings_PersistsTailAfterShutdown asserts on mappings.yaml itself
// — the artifact replay actually reads. Accepting a mapping off the channel is
// worthless if the write then discards it.
func TestConsumeMappings_PersistsTailAfterShutdown(t *testing.T) {
	const testSetID = "test-set-0"
	dir := t.TempDir()

	r := &Recorder{
		logger:    zap.NewNop(),
		config:    &config.Config{},
		mappingDb: mapdb.New(zap.NewNop(), dir, "mappings"),
	}

	// The mock loop has already correlated these tempIDs.
	var correlationMap, asyncMockIDs sync.Map
	tail := []string{"post-orders-58", "post-orders-59", "post-orders-60"}
	for _, tn := range tail {
		correlationMap.Store("temp-"+tn, models.MockEntry{
			Name: "mock-" + tn,
			Kind: string(models.Mongo),
		})
	}

	mappings := make(chan models.TestMockMapping, len(tail))
	for _, tn := range tail {
		mappings <- models.TestMockMapping{TestName: tn, MockIDs: []string{"temp-" + tn}}
	}
	close(mappings)

	// Recording has been cancelled: the state every tail mapping is written in.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, r.consumeMappings(ctx, testSetID, mappings, &correlationMap, &asyncMockIDs, &droppedMockSet{}, nil, &sync.Map{}))

	require.FileExists(t, filepath.Join(dir, testSetID, "mappings.yaml"),
		"no mappings.yaml on disk: every mapping written during shutdown was discarded "+
			"by the cancelled context, and replay reports no_mocks for the tail")

	// Read back through the same store replay uses, on a live context.
	saved, meaningful, err := mapdb.New(zap.NewNop(), dir, "mappings").Get(context.Background(), testSetID)
	require.NoError(t, err)
	require.True(t, meaningful, "mappings.yaml exists but holds no mock entries")

	for _, tn := range tail {
		assert.Len(t, saved[tn], 1,
			"test %q was streamed by the agent during shutdown and must be mapped in "+
				"mappings.yaml; dropping it is the go-memory-load-mongo no_mocks flake", tn)
	}
}

// TestConsumeMappings_UpsertsIntoExistingFile covers the path production actually
// takes. The sibling test starts from an empty dir, so it only exercises the
// create-file gate; by the time the tail arrives in a real run, mappings.yaml
// already holds hundreds of tests and the write goes through the read-modify-write
// path instead. Both must survive cancellation.
func TestConsumeMappings_UpsertsIntoExistingFile(t *testing.T) {
	const testSetID = "test-set-0"
	dir := t.TempDir()
	db := mapdb.New(zap.NewNop(), dir, "mappings")

	// An earlier, healthy part of the session — written while ctx was live.
	require.NoError(t, db.Upsert(context.Background(), testSetID, "post-orders-1",
		[]models.MockEntry{{Name: "mock-1", Kind: string(models.Mongo)}}))

	r := &Recorder{logger: zap.NewNop(), config: &config.Config{}, mappingDb: db}

	var correlationMap, asyncMockIDs sync.Map
	correlationMap.Store("temp-tail", models.MockEntry{Name: "mock-tail", Kind: string(models.Mongo)})

	mappings := make(chan models.TestMockMapping, 1)
	mappings <- models.TestMockMapping{TestName: "post-orders-60", MockIDs: []string{"temp-tail"}}
	close(mappings)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, r.consumeMappings(ctx, testSetID, mappings, &correlationMap, &asyncMockIDs, &droppedMockSet{}, nil, &sync.Map{}))

	saved, _, err := mapdb.New(zap.NewNop(), dir, "mappings").Get(context.Background(), testSetID)
	require.NoError(t, err)
	assert.Len(t, saved["post-orders-60"], 1,
		"the tail mapping must be merged into the existing mappings.yaml during shutdown")
	assert.Len(t, saved["post-orders-1"], 1,
		"upserting the tail must not lose mappings written earlier in the session")
}

// countingMapDb records how many file rewrites the consumer asks for.
type countingMapDb struct {
	mu      sync.Mutex
	writes  int
	byTest  map[string][]models.MockEntry
	perCall []int
}

func (c *countingMapDb) Insert(context.Context, *models.Mapping, bool) error { return nil }
func (c *countingMapDb) Upsert(ctx context.Context, testSetID, testID string, e []models.MockEntry) error {
	return c.UpsertBatch(ctx, testSetID, map[string][]models.MockEntry{testID: e})
}
func (c *countingMapDb) UpsertBatch(_ context.Context, _ string, byTest map[string][]models.MockEntry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes++
	c.perCall = append(c.perCall, len(byTest))
	for k, v := range byTest {
		c.byTest[k] = v
	}
	return nil
}

// TestConsumeMappings_BatchesWrites pins the fix for the slow-consumer half of
// the go-memory-load-mongo flake.
//
// mappings.yaml is one document, so each write re-encodes the whole file. Writing
// per mapping is quadratic — measured at 368 tests, 164us for the first write and
// 19.45ms for the last, ~2.7s of pure rewriting. That is slow enough to
// back-pressure the agent's mapping stream, and the agent discards what it cannot
// hand over, so the tests behind the backlog replay as no_mocks.
//
// The consumer must therefore drain the stream far faster than it writes: batch
// the mappings and rewrite once per batch.
func TestConsumeMappings_BatchesWrites(t *testing.T) {
	const total = 320
	db := &countingMapDb{byTest: map[string][]models.MockEntry{}}
	r := &Recorder{logger: zap.NewNop(), config: &config.Config{}, mappingDb: db}

	var correlationMap, asyncMockIDs sync.Map
	mappings := make(chan models.TestMockMapping, total)
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("post-orders-%d", i)
		correlationMap.Store("temp-"+name, models.MockEntry{Name: "mock-" + name, Kind: string(models.Mongo)})
		mappings <- models.TestMockMapping{TestName: name, MockIDs: []string{"temp-" + name}}
	}
	close(mappings)

	require.NoError(t, r.consumeMappings(context.Background(), "test-set-0", mappings, &correlationMap, &asyncMockIDs, &droppedMockSet{}, nil, &sync.Map{}))

	db.mu.Lock()
	writes, saved := db.writes, len(db.byTest)
	db.mu.Unlock()

	assert.Equal(t, total, saved, "every mapping must still be persisted")
	assert.LessOrEqual(t, writes, total/mappingFlushBatch+2,
		"mappings must be batched, not written one file-rewrite per test: %d writes for %d "+
			"mappings means the consumer is quadratic again and will back-pressure the agent "+
			"into dropping the tail", writes, total)
	assert.Greater(t, writes, 0, "mappings must actually reach the store")
}

// TestConsumeMappings_LateMockDoesNotWipeEarlierOnes pins the delta semantics of
// a test's mapping.
//
// The agent emits a test's mocks when its window resolves, then emits MORE later
// for mocks it retroactively bins into that already-resolved window. The second
// emission carries only the late mock — it is a delta. Replacing on it deletes
// the mocks the first emission recorded, and the test then replays against a short
// pool (or an empty one), which is the same no_mocks failure by another route.
func TestConsumeMappings_LateMockDoesNotWipeEarlierOnes(t *testing.T) {
	const testSetID, test = "test-set-0", "post-orders-1"
	dir := t.TempDir()
	r := &Recorder{
		logger:    zap.NewNop(),
		config:    &config.Config{},
		mappingDb: mapdb.New(zap.NewNop(), dir, "mappings"),
	}

	var correlationMap, asyncMockIDs sync.Map
	for _, id := range []string{"a", "b", "c"} {
		correlationMap.Store("temp-"+id, models.MockEntry{Name: "mock-" + id, Kind: string(models.Mongo)})
	}

	// Resolution emits a and b; a later retroactive bin emits only c.
	mappings := make(chan models.TestMockMapping, 2)
	mappings <- models.TestMockMapping{TestName: test, MockIDs: []string{"temp-a", "temp-b"}}
	mappings <- models.TestMockMapping{TestName: test, MockIDs: []string{"temp-c"}}
	close(mappings)

	require.NoError(t, r.consumeMappings(context.Background(), testSetID, mappings, &correlationMap, &asyncMockIDs, &droppedMockSet{}, nil, &sync.Map{}))

	saved, _, err := mapdb.New(zap.NewNop(), dir, "mappings").Get(context.Background(), testSetID)
	require.NoError(t, err)

	names := make([]string, 0, 3)
	for _, e := range saved[test] {
		names = append(names, e.Name)
	}
	assert.ElementsMatch(t, []string{"mock-a", "mock-b", "mock-c"}, names,
		"a late-binned mock is a DELTA: it must be unioned into the test's mapping, not "+
			"replace it — dropping mock-a/mock-b here means this test replays with a short "+
			"mock pool and reports a mismatch")
}

// TestConsumeMappings_FlushesPartialBatchOnTicker covers the path a batching
// consumer must not get wrong: a recording that produces fewer than a full batch
// (or trails off) must still persist, rather than holding mappings in memory until
// the stream closes. Exercises the ticker concurrently with the feed.
func TestConsumeMappings_FlushesPartialBatchOnTicker(t *testing.T) {
	db := &countingMapDb{byTest: map[string][]models.MockEntry{}}
	r := &Recorder{logger: zap.NewNop(), config: &config.Config{}, mappingDb: db}

	var correlationMap, asyncMockIDs sync.Map
	mappings := make(chan models.TestMockMapping)

	// Feed slowly and never fill a batch, keeping the stream open throughout.
	go func() {
		defer close(mappings)
		for i := 0; i < 3; i++ {
			name := fmt.Sprintf("post-orders-%d", i)
			correlationMap.Store("temp-"+name, models.MockEntry{Name: "mock-" + name, Kind: string(models.Mongo)})
			mappings <- models.TestMockMapping{TestName: name, MockIDs: []string{"temp-" + name}}
			time.Sleep(mappingFlushInterval + 200*time.Millisecond)
		}
	}()

	require.NoError(t, r.consumeMappings(context.Background(), "test-set-0", mappings, &correlationMap, &asyncMockIDs, &droppedMockSet{}, nil, &sync.Map{}))

	db.mu.Lock()
	writes, saved := db.writes, len(db.byTest)
	db.mu.Unlock()

	assert.Equal(t, 3, saved, "every mapping must be persisted")
	assert.Greater(t, writes, 1,
		"a partial batch must be flushed by the ticker as recording proceeds, not held in "+
			"memory until the stream closes: got %d write(s)", writes)
}

// TestMockStore_RefusesCancelledContext is the reason persistCtx exists, pinned
// at the store boundary.
//
// It documents why threading the recording context into a record-time write is a
// data-loss bug rather than a style preference — and it guards the second half of
// the failure: the mock consumer skips correlationMap.Store when its insert
// fails, so a dropped tail mock also strands the mapping that references it. The
// mapping is then uncorrelatable and no row is written for that test at all, which
// is the same no_mocks symptom by a different route.
func TestMockStore_RefusesCancelledContext(t *testing.T) {
	dir := t.TempDir()
	db := mockdb.New(zap.NewNop(), dir, "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	mock := &models.Mock{
		Version: models.V1Beta1,
		Kind:    models.Mongo,
		Name:    "mock-tail",
		Spec:    models.MockSpec{Metadata: map[string]string{}},
	}

	err := db.InsertMock(ctx, mock, "test-set-0")
	require.Error(t, err,
		"if the mock store ever starts accepting a cancelled context this test is "+
			"obsolete; until then, record-time writes must not run on the recording context")
	require.ErrorIs(t, err, context.Canceled)

	// The same insert on a detached context — what Start now passes — persists.
	require.NoError(t, db.InsertMock(context.WithoutCancel(ctx), mock, "test-set-0"),
		"detaching the write from cancellation is what keeps the tail of a recording")
}

// ── End-to-end fakes for the skip-and-revoke chain ───────────────────────────

/*
WAIT UNTIL THE RECORDING IS DEMONSTRABLY UNDER WAY, rather than inferring it
from a send that completed.

A send to f.incoming / f.outgoing / f.mappings completes at the FORWARDER,
which is one hop before the consumer and is spawned by GetTestAndMockChans --
several hundred lines before Start spawns anything that reads it. Start then
has a `ctx.Err()` gate to pass. So a test that sends and immediately cancels is
racing that gate, and when it loses, Start returns context.Canceled and the
test's own require.NoError fails. MEASURED on four tests in this file with a
200ms sleep in front of the gate: 5/5 red, every time at the same assertion.

A persisted test case is the first thing that can only happen AFTER the gate --
the consumer that writes it is spawned past it -- so it is exactly the signal
these tests need. Polling rather than sleeping: a sleep long enough to be safe
on a loaded machine is a sleep paid on every green run, and one short enough
not to be is the flake all over again.

The deadline is generous because it only matters when something is genuinely
broken; the loop exits as soon as the write lands, which is microseconds.
*/
func waitForPersistedTestCase(t *testing.T, db *recTestDB, name string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		db.mu.Lock()
		var found bool
		for _, got := range db.inserted {
			if got == name {
				found = true
				break
			}
		}
		db.mu.Unlock()
		if found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("test case %q was never persisted, so the recording never got "+
				"past Start's post-setup gate; cancelling now would assert on a race "+
				"rather than on the behaviour under test", name)
		}
		time.Sleep(time.Millisecond)
	}
}

type recTestDB struct {
	mu       sync.Mutex
	inserted []string
	deleted  []string
	// insertErr, when set, fails EVERY insert -- the disk-full shape.
	// Set at construction and never mutated, so reading it without the
	// mutex is race-free.
	insertErr error
	// failNames fails only the named test cases, so a recording can
	// persist some and lose others. Same construction-time rule.
	failNames map[string]bool
}

func (d *recTestDB) GetAllTestSetIDs(context.Context) ([]string, error) { return nil, nil }
func (d *recTestDB) InsertTestCase(_ context.Context, tc *models.TestCase, _ string, _ bool) error {
	if d.insertErr != nil {
		return d.insertErr
	}
	if d.failNames[tc.Name] {
		return errors.New("no space left on device")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.inserted = append(d.inserted, tc.Name)
	return nil
}
func (d *recTestDB) DeleteTests(_ context.Context, _ string, ids []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deleted = append(d.deleted, ids...)
	return nil
}

// recMockDB fails exactly the mocks named in unencodable, with the sentinel the
// recorder classifies as "skip this one and keep going".
type recMockDB struct {
	mu          sync.Mutex
	unencodable map[string]bool
	inserted    []string
}

func (d *recMockDB) InsertMock(_ context.Context, m *models.Mock, _ string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.unencodable[m.Name] {
		return fmt.Errorf("%w (yaml): cannot marshal invalid UTF-8 data as !!str", models.ErrMockEncode)
	}
	d.inserted = append(d.inserted, m.Name)
	return nil
}
func (d *recMockDB) DeleteMocksForSet(context.Context, string) error { return nil }
func (d *recMockDB) GetCurrMockID() int64                            { return 0 }
func (d *recMockDB) ResetCounterID()                                 {}

type recMappingDB struct {
	mu      sync.Mutex
	batches []map[string][]models.MockEntry
}

func (d *recMappingDB) Insert(context.Context, *models.Mapping, bool) error              { return nil }
func (d *recMappingDB) Upsert(context.Context, string, string, []models.MockEntry) error { return nil }
func (d *recMappingDB) UpsertBatch(_ context.Context, _ string, byTest map[string][]models.MockEntry) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	cp := make(map[string][]models.MockEntry, len(byTest))
	for k, v := range byTest {
		cp[k] = append([]models.MockEntry(nil), v...)
	}
	d.batches = append(d.batches, cp)
	return nil
}

type recTelemetry struct {
	mu    sync.Mutex
	suite map[string]interface{}
	/*
	 * THE TEST COUNT, KEPT, so the package can observe it at all.
	 *
	 * Both telemetry calls take the recorded test count. A fake that
	 * discards it (`_ int`, `int64`) leaves NOTHING in the package able to
	 * see the number, which is how the ordering defect below survives. MEASURED: moving the testCountSnapshot read above
	 * the finalize revoke block -- which makes both numbers count tests that
	 * were revoked and deleted, i.e. overstates what was recorded -- left
	 * the whole package green 10 runs out of 10.
	 *
	 * A discarded argument in a fake is a silent hole in every test that
	 * uses it: the call is "asserted" only in the sense that it did not
	 * panic.
	 */
	suiteTestCount   int
	suiteRecorded    bool
	sessionTestCount int64
	sessionRecorded  bool
}

func (tl *recTelemetry) RecordedTestSuite(_ string, testCount int, _ map[string]int, metadata map[string]interface{}) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tl.suite = metadata
	tl.suiteTestCount = testCount
	tl.suiteRecorded = true
}
func (tl *recTelemetry) RecordedTestCaseMock(string)  {}
func (tl *recTelemetry) RecordedMocks(map[string]int) {}
func (tl *recTelemetry) RecordedTestAndMocks()        {}
func (tl *recTelemetry) RecordSessionCompleted(testCount int64, _ int64, _ int64, _ string, _ string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tl.sessionTestCount = testCount
	tl.sessionRecorded = true
}

type recTestSetConf struct{}

func (recTestSetConf) Read(context.Context, string) (*models.TestSet, error) { return nil, nil }
func (recTestSetConf) ReadForUpdate(context.Context, string) (*models.TestSet, error) {
	return nil, nil
}
func (recTestSetConf) Write(context.Context, string, *models.TestSet) error { return nil }

// blockingInstr is fakeInstr whose Run blocks until ctx is done, so the test
// controls when the session ends.
type blockingInstr struct{ *fakeInstr }

func (b *blockingInstr) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	<-ctx.Done()
	return models.AppError{AppErrorType: models.ErrCtxCanceled}
}

// TestStart_UnencodableMockIsSkippedAndItsTestRevoked is the end-to-end guard
// for the defect that ended a 46-hour production recording: ONE HTTP response
// body carrying invalid UTF-8 failed to encode, and the recorder treated a
// failed mock insert as fatal, tearing the whole session down.
//
// It pins the full chain, which no narrower test reaches:
//   - the session SURVIVES the unencodable mock, and mocks recorded after it
//     still land (the 46-hour loss was everything after the bad one);
//   - the dropped mock's ID reaches the mapping consumer, so the test that
//     owned it is REVOKED rather than persisted with a silently short mock
//     pool that fails at replay looking like a product regression;
//   - the drop is counted, so an operator and telemetry see the hole.
func TestStart_UnencodableMockIsSkippedAndItsTestRevoked(t *testing.T) {
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	testDB := &recTestDB{}
	mockDB := &recMockDB{unencodable: map[string]bool{"mock-bad": true}}
	mappingDB := &recMappingDB{}
	tele := &recTelemetry{}

	r := &Recorder{
		logger:          zap.NewNop(),
		testDB:          testDB,
		mockDB:          mockDB,
		mappingDb:       mappingDB,
		telemetry:       tele,
		instrumentation: &blockingInstr{f},
		testSetConf:     recTestSetConf{},
		hooks:           BaseRecordHooks{},
		config:          &config.Config{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	ts := time.Unix(1700000000, 0)
	httpMock := func(name string) *models.Mock {
		return &models.Mock{
			Version: models.GetVersion(), Name: name, Kind: models.HTTP,
			Spec: models.MockSpec{ReqTimestampMock: ts, ResTimestampMock: ts},
		}
	}
	// Every send is bounded. If the recorder treats the unencodable mock as
	// fatal it stops consuming, and an unguarded send would simply deadlock —
	// the regression would surface as a CI timeout with no explanation instead
	// of a named failure.
	sendMock := func(name string) {
		t.Helper()
		select {
		case f.outgoing <- httpMock(name):
		case <-time.After(10 * time.Second):
			t.Fatalf("the recorder stopped consuming mocks at %q — it tore the session down instead "+
				"of skipping the one unencodable mock (the 46-hour production failure)", name)
		}
	}
	sendTest := func(name string) {
		t.Helper()
		select {
		case f.incoming <- &models.TestCase{Name: name, Kind: models.HTTP}:
		case <-time.After(10 * time.Second):
			t.Fatalf("the recorder stopped consuming test cases at %q", name)
		}
	}
	sendMapping := func(testName string, mockIDs ...string) {
		t.Helper()
		select {
		case f.mappings <- models.TestMockMapping{TestName: testName, MockIDs: mockIDs}:
		case <-time.After(10 * time.Second):
			t.Fatalf("the recorder stopped consuming mappings at %q", testName)
		}
	}

	sendMock("mock-good-1")
	sendMock("mock-bad")
	// Recorded AFTER the failure: under the old fatal behaviour this one, and
	// everything else for the rest of the session, was lost.
	sendMock("mock-good-2")

	sendTest("test-1")
	sendTest("test-2")

	// test-1 owns the dropped mock; test-2 is healthy and must be untouched.
	sendMapping("test-1", "mock-good-1", "mock-bad")
	sendMapping("test-2", "mock-good-2")

	// Let the consumers drain before teardown.
	require.Eventually(t, func() bool {
		mappingDB.mu.Lock()
		defer mappingDB.mu.Unlock()
		return len(mappingDB.batches) > 0
	}, 5*time.Second, 20*time.Millisecond, "no mapping batch was ever persisted")

	cancel()
	close(f.outgoing)
	close(f.incoming)
	close(f.mappings)

	select {
	case err := <-done:
		require.NoError(t, err, "the session died on an unencodable mock — this is the 46-hour production failure")
	case <-time.After(90 * time.Second):
		t.Fatal("Start did not return")
	}

	mockDB.mu.Lock()
	insertedMocks := append([]string(nil), mockDB.inserted...)
	mockDB.mu.Unlock()
	assert.Contains(t, insertedMocks, "mock-good-2",
		"the mock recorded AFTER the unencodable one is missing: the session was torn down instead of skipping one mock")
	assert.NotContains(t, insertedMocks, "mock-bad")

	testDB.mu.Lock()
	deleted := append([]string(nil), testDB.deleted...)
	testDB.mu.Unlock()
	assert.Contains(t, deleted, "test-1",
		"the test that owned the dropped mock was NOT revoked; it is persisted with a short mock pool "+
			"and fails at replay looking like a product regression")
	assert.NotContains(t, deleted, "test-2", "a healthy test was revoked")

	tele.mu.Lock()
	suite := tele.suite
	tele.mu.Unlock()
	require.NotNil(t, suite, "no test-suite telemetry was recorded")
	assert.EqualValues(t, 1, suite["mocks-dropped"],
		"the dropped mock was not counted, so a recording growing holes looks clean in telemetry: %v", suite)
}

// TestStart_ShortPoolReachesTelemetryWithoutAnyDrops pins the reporting path
// for the one data-loss class this change deliberately does NOT revoke.
//
// A mock that never correlates leaves its test persisted with an incomplete
// mock set. Not revoking it is a judgement call — a timeout is an inference,
// not the known-drop fact — and the ONLY thing that makes that call defensible
// is that the frequency becomes measurable. The motivating case is a
// correlation timeout under parallelism with a perfectly healthy encoder, i.e.
// ZERO dropped mocks, so gating the counters behind the drop counter would
// leave them invisible in exactly the recording they were added to measure.
func TestStart_ShortPoolReachesTelemetryWithoutAnyDrops(t *testing.T) {
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	testDB := &recTestDB{}
	// No unencodable mocks at all: this recording has zero drops.
	mockDB := &recMockDB{unencodable: map[string]bool{}}
	tele := &recTelemetry{}

	r := &Recorder{
		logger:          zap.NewNop(),
		testDB:          testDB,
		mockDB:          mockDB,
		mappingDb:       &recMappingDB{},
		telemetry:       tele,
		instrumentation: &blockingInstr{f},
		testSetConf:     recTestSetConf{},
		hooks:           BaseRecordHooks{},
		config:          &config.Config{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	ts := time.Unix(1700000000, 0)
	select {
	case f.outgoing <- &models.Mock{
		Version: models.GetVersion(), Name: "mock-1", Kind: models.HTTP,
		Spec: models.MockSpec{ReqTimestampMock: ts, ResTimestampMock: ts},
	}:
	case <-time.After(10 * time.Second):
		t.Fatal("recorder never consumed the mock")
	}
	select {
	case f.incoming <- &models.TestCase{Name: "test-1", Kind: models.HTTP}:
	case <-time.After(10 * time.Second):
		t.Fatal("recorder never consumed the test case")
	}
	// "ghost" was never streamed as a mock, so it never correlates — the
	// pre-existing "Failed to correlate mock mapping" path.
	select {
	case f.mappings <- models.TestMockMapping{TestName: "test-1", MockIDs: []string{"mock-1", "ghost"}}:
	case <-time.After(10 * time.Second):
		t.Fatal("recorder never consumed the mapping")
	}

	// The recording must be demonstrably under way before it is stopped;
	// otherwise this cancel races Start's post-setup gate and the
	// require.NoError below asserts on that race. See
	// waitForPersistedTestCase.
	waitForPersistedTestCase(t, testDB, "test-1")

	cancel()
	close(f.outgoing)
	close(f.incoming)
	close(f.mappings)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(90 * time.Second):
		t.Fatal("Start did not return")
	}

	// Not revoked — the timeout is an inference, so the test still ships.
	testDB.mu.Lock()
	deleted := append([]string(nil), testDB.deleted...)
	testDB.mu.Unlock()
	assert.NotContains(t, deleted, "test-1",
		"a correlation timeout revoked a test; that converts a latency event into data destruction")

	tele.mu.Lock()
	suite := tele.suite
	tele.mu.Unlock()
	require.NotNil(t, suite, "no test-suite telemetry was recorded")
	assert.EqualValues(t, 1, suite["tests-short-pool"],
		"a test shipped with an incomplete mock set but nothing reached telemetry, so the frequency "+
			"stays unmeasurable — which is the whole justification for not revoking: %v", suite)
	assert.EqualValues(t, 1, suite["mocks-uncorrelated"], "%v", suite)
	assert.NotContains(t, suite, "mocks-dropped", "this recording dropped nothing")
}

// TestStart_AgentRevokedTestIsNotCountedAsShortPool closes the second revoke
// path. consumeMappings revokes a test whose mock it KNOWS was dropped, but the
// agent independently revokes tests via a RevokedTests control frame when it
// capacity-drops a mock after the test streamed — a revoke consumeMappings
// never sees.
//
// A capacity-dropped test is a prime candidate for also having an uncorrelated
// mock, so counting it would inflate tests-short-pool with exactly the
// population the metric claims to exclude. Since that metric's credibility is
// the entire justification for not revoking on a correlation timeout, both
// revoke paths have to be subtracted, which is why the totals are computed at
// teardown instead of accumulated as mappings arrive.
func TestStart_AgentRevokedTestIsNotCountedAsShortPool(t *testing.T) {
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	testDB := &recTestDB{}
	tele := &recTelemetry{}

	r := &Recorder{
		logger:          zap.NewNop(),
		testDB:          testDB,
		mockDB:          &recMockDB{unencodable: map[string]bool{}},
		mappingDb:       &recMappingDB{},
		telemetry:       tele,
		instrumentation: &blockingInstr{f},
		testSetConf:     recTestSetConf{},
		hooks:           BaseRecordHooks{},
		config:          &config.Config{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	ts := time.Unix(1700000000, 0)
	send := func(m *models.Mock) {
		t.Helper()
		select {
		case f.outgoing <- m:
		case <-time.After(10 * time.Second):
			t.Fatalf("recorder never consumed mock %q", m.Name)
		}
	}
	send(&models.Mock{
		Version: models.GetVersion(), Name: "mock-1", Kind: models.HTTP,
		Spec: models.MockSpec{ReqTimestampMock: ts, ResTimestampMock: ts},
	})
	select {
	case f.incoming <- &models.TestCase{Name: "test-1", Kind: models.HTTP}:
	case <-time.After(10 * time.Second):
		t.Fatal("recorder never consumed the test case")
	}
	// "ghost" never correlates → test-1 looks short-pooled...
	select {
	case f.mappings <- models.TestMockMapping{TestName: "test-1", MockIDs: []string{"mock-1", "ghost"}}:
	case <-time.After(10 * time.Second):
		t.Fatal("recorder never consumed the mapping")
	}
	// ...but the AGENT then revokes it via its own control frame.
	send(&models.Mock{
		Version: models.GetVersion(),
		Name:    "revoke-frame",
		Kind:    models.RevokedTests,
		Spec:    models.MockSpec{Metadata: map[string]string{"revoked_tests": "test-1"}},
	})

	// The recording must be demonstrably under way before it is stopped;
	// otherwise this cancel races Start's post-setup gate and the
	// require.NoError below asserts on that race. See
	// waitForPersistedTestCase.
	waitForPersistedTestCase(t, testDB, "test-1")

	cancel()
	close(f.outgoing)
	close(f.incoming)
	close(f.mappings)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(90 * time.Second):
		t.Fatal("Start did not return")
	}

	testDB.mu.Lock()
	deleted := append([]string(nil), testDB.deleted...)
	testDB.mu.Unlock()
	require.Contains(t, deleted, "test-1", "the agent's RevokedTests frame did not delete the test")

	tele.mu.Lock()
	suite := tele.suite
	suiteCount, suiteRecorded := tele.suiteTestCount, tele.suiteRecorded
	sessionCount, sessionRecorded := tele.sessionTestCount, tele.sessionRecorded
	tele.mu.Unlock()
	require.NotNil(t, suite)
	assert.NotContains(t, suite, "tests-short-pool",
		"a test the AGENT revoked is still counted as shipping with a short mock pool; it was "+
			"deleted, so the metric over-counts exactly the population it claims to exclude: %v", suite)

	/*
	 * AND THE COUNT ITSELF EXCLUDES IT.
	 *
	 * This recording inserted exactly one test case and the agent then
	 * revoked it, so finalize DELETED it -- asserted above. Both telemetry
	 * calls must therefore report ZERO tests recorded: a revoked test is not
	 * a recorded one, and a count that includes it tells us a recording
	 * produced work it does not have.
	 *
	 * This is the discriminating shape, which is why it belongs here rather
	 * than in a test where nothing is revoked: with one insert and one
	 * revoke, reading testCountSnapshot before the finalize revoke block
	 * gives 1 and reading it after gives 0. Nothing distinguished them until
	 * recTelemetry stopped discarding the argument -- MEASURED, that mutant
	 * survived the whole package 10 runs out of 10.
	 *
	 * Both calls, because they take the count from the same snapshot by
	 * different routes and each is a separate place to get it wrong.
	 */
	require.True(t, suiteRecorded, "no test-suite telemetry was emitted at all")
	assert.Equal(t, 0, suiteCount,
		"the suite telemetry counted a test the agent revoked and finalize deleted")
	require.True(t, sessionRecorded, "no session-completed telemetry was emitted at all")
	assert.Equal(t, int64(0), sessionCount,
		"the session telemetry counted a test the agent revoked and finalize deleted")
}

// TestStart_HookSkippedMockIsExcludedFromMappings pins the guard that a naive
// reading of the code makes look unnecessary.
//
// A RecordHook can ask the recorder to drop a mock (AsyncRecorder does this for
// a collapsed poll cycle whose value did not change). The agent does not know
// about the skip, so the tempID still arrives in the mapping it streams. The
// recorder must therefore mark it itself.
//
// The trap: AsyncRecorder sets info.Skip and returns BEFORE assigning
// Spec.Async, so a skipped mock reports IsAsync()==false. Gating the marker on
// mock.IsAsync() — the obvious thing to write — makes the guard a silent no-op,
// and the skipped tempID then burns the full ~500ms correlation spin and is
// reported as an incomplete mock set on a healthy test. This test drives the
// real hook rather than a fake so that trap cannot be re-introduced.
func TestStart_HookSkippedMockIsExcludedFromMappings(t *testing.T) {
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	testDB := &recTestDB{}
	tele := &recTelemetry{}

	// Force the BEST-EFFORT half (droppedMockSet's benign cache) to decline
	// every entry, which is the steady state of a long poll-heavy recording.
	// What remains is the unconditional asyncMockIDs marker — the correctness
	// guarantee — and this test exists to prove it carries the load alone.
	prevCap := maxLiveBenignMockIDs
	maxLiveBenignMockIDs = 0
	t.Cleanup(func() { maxLiveBenignMockIDs = prevCap })

	r := &Recorder{
		logger:          zap.NewNop(),
		testDB:          testDB,
		mockDB:          &recMockDB{unencodable: map[string]bool{}},
		mappingDb:       &recMappingDB{},
		telemetry:       tele,
		instrumentation: &blockingInstr{f},
		testSetConf:     recTestSetConf{},
		hooks:           skipEverythingHooks{},
		config:          &config.Config{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	ts := time.Unix(1700000000, 0)
	select {
	case f.outgoing <- &models.Mock{
		Version: models.GetVersion(), Name: "poll-collapsed", Kind: models.HTTP,
		Spec: models.MockSpec{ReqTimestampMock: ts, ResTimestampMock: ts},
	}:
	case <-time.After(10 * time.Second):
		t.Fatal("recorder never consumed the mock")
	}
	select {
	case f.incoming <- &models.TestCase{Name: "test-1", Kind: models.HTTP}:
	case <-time.After(10 * time.Second):
		t.Fatal("recorder never consumed the test case")
	}

	select {
	case f.mappings <- models.TestMockMapping{TestName: "test-1", MockIDs: []string{"poll-collapsed"}}:
	case <-time.After(10 * time.Second):
		t.Fatal("recorder never consumed the mapping")
	}

	// The recording must be demonstrably under way before it is stopped;
	// otherwise this cancel races Start's post-setup gate and the
	// require.NoError below asserts on that race. See
	// waitForPersistedTestCase.
	waitForPersistedTestCase(t, testDB, "test-1")

	cancel()
	close(f.outgoing)
	close(f.incoming)
	close(f.mappings)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(90 * time.Second):
		t.Fatal("Start did not return")
	}

	testDB.mu.Lock()
	deleted := append([]string(nil), testDB.deleted...)
	testDB.mu.Unlock()
	assert.NotContains(t, deleted, "test-1", "a hook-skipped mock revoked its test; the skip is normal operation")

	tele.mu.Lock()
	suite := tele.suite
	tele.mu.Unlock()
	require.NotNil(t, suite)
	assert.NotContains(t, suite, "tests-short-pool",
		"a hook-skipped mock was reported as an incomplete mock set. Poll lanes collapse most "+
			"cycles by design, so this alarms on every async-poll recording: %v", suite)
}

// skipEverythingHooks reproduces AsyncRecorder's collapsed-cycle contract
// exactly: set Skip and return WITHOUT assigning Spec.Async.
type skipEverythingHooks struct{ BaseRecordHooks }

func (skipEverythingHooks) BeforeMockInsert(_ context.Context, info *MockContext) error {
	info.Skip = true
	return nil
}

// failingDeleteTestDB accepts inserts but cannot delete — the shape of an
// embedding whose TestDB does not really support the revoke.
type failingDeleteTestDB struct{ recTestDB }

func (d *failingDeleteTestDB) DeleteTests(context.Context, string, []string) error {
	return errors.New("delete not supported here")
}

// TestStart_FailedRevokeKeepsTheTestInTheShortPoolCount covers the gap between
// "revoked" and "actually deleted".
//
// The revoke is best-effort: DeleteTests is reached by runtime assertion
// (it is deliberately not on the record TestDB interface so enterprise
// implementations keep compiling), and an individual delete can fail and only
// warn. When that happens the test REMAINS in the set, still carrying an
// incomplete mock set — so subtracting it from tests-short-pool on the strength
// of the revoke alone under-reports exactly the embedding that cannot apply the
// revoke, which is the one an operator most needs to hear about.
func TestStart_FailedRevokeKeepsTheTestInTheShortPoolCount(t *testing.T) {
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	testDB := &failingDeleteTestDB{}
	tele := &recTelemetry{}

	r := &Recorder{
		logger:          zap.NewNop(),
		testDB:          testDB,
		mockDB:          &recMockDB{unencodable: map[string]bool{}},
		mappingDb:       &recMappingDB{},
		telemetry:       tele,
		instrumentation: &blockingInstr{f},
		testSetConf:     recTestSetConf{},
		hooks:           BaseRecordHooks{},
		config:          &config.Config{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	ts := time.Unix(1700000000, 0)
	send := func(m *models.Mock) {
		t.Helper()
		select {
		case f.outgoing <- m:
		case <-time.After(10 * time.Second):
			t.Fatalf("recorder never consumed mock %q", m.Name)
		}
	}
	send(&models.Mock{
		Version: models.GetVersion(), Name: "mock-1", Kind: models.HTTP,
		Spec: models.MockSpec{ReqTimestampMock: ts, ResTimestampMock: ts},
	})
	select {
	case f.incoming <- &models.TestCase{Name: "test-1", Kind: models.HTTP}:
	case <-time.After(10 * time.Second):
		t.Fatal("recorder never consumed the test case")
	}
	select {
	case f.mappings <- models.TestMockMapping{TestName: "test-1", MockIDs: []string{"mock-1", "ghost"}}:
	case <-time.After(10 * time.Second):
		t.Fatal("recorder never consumed the mapping")
	}
	send(&models.Mock{
		Version: models.GetVersion(), Name: "revoke-frame", Kind: models.RevokedTests,
		Spec: models.MockSpec{Metadata: map[string]string{"revoked_tests": "test-1"}},
	})

	// The recording must be demonstrably under way before it is stopped;
	// otherwise this cancel races Start's post-setup gate and the
	// require.NoError below asserts on that race. See
	// waitForPersistedTestCase.
	waitForPersistedTestCase(t, &testDB.recTestDB, "test-1")

	cancel()
	close(f.outgoing)
	close(f.incoming)
	close(f.mappings)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(90 * time.Second):
		t.Fatal("Start did not return")
	}

	tele.mu.Lock()
	suite := tele.suite
	tele.mu.Unlock()
	require.NotNil(t, suite)
	assert.EqualValues(t, 1, suite["tests-short-pool"],
		"the revoke FAILED, so test-1 is still in the set with an incomplete mock set — but it was "+
			"subtracted from the count anyway, hiding the problem in the very embedding that cannot "+
			"apply the revoke: %v", suite)
}

// TestGetTestAndMockChans_ForwardsInFlightTestCaseOnShutdown is the test-case
// half of the tail-loss family this branch is named after.
//
// The mock and mapping forwarders both hand over what they have already taken
// from the agent when ctx is cancelled — the mock one by forwarding then
// returning, the mapping one via a drain grace period. The incoming (test-case)
// forwarder did not: it returned ctx.Err() with a test case already in hand.
//
// The agent counts that test case as delivered and never re-sends it, and its
// MOCKS and its MAPPING are still persisted (the stores write on persistCtx,
// detached from ctx, precisely so the tail survives). So the test case vanished
// with no log and no counter, leaving the set with orphaned mocks and a
// mappings.yaml entry naming a test that does not exist. SIGINT lands while the
// agent is streaming the tail, so this is exactly the window the tail is in.
func TestGetTestAndMockChans_ForwardsInFlightTestCaseOnShutdown(t *testing.T) {
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	r := &Recorder{logger: zap.NewNop(), instrumentation: f, config: &config.Config{}}

	g, gctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(gctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)

	frames, err := r.GetTestAndMockChans(ctx)
	require.NoError(t, err)

	// The agent hands over a test case, then shutdown begins. The send returns,
	// so from the agent's point of view this test case is delivered.
	f.incoming <- &models.TestCase{Name: "tail-test", Kind: models.HTTP}
	cancel()

	select {
	case tc, ok := <-frames.Incoming:
		require.True(t, ok, "the incoming channel closed without delivering the in-flight test case")
		assert.Equal(t, "tail-test", tc.Name)
	case <-time.After(5 * time.Second):
		t.Fatal("a test case already taken from the agent was DROPPED on shutdown. The agent will " +
			"not re-send it, but its mocks and its mapping are still persisted, so the test set " +
			"ships with orphaned mocks and a mappings.yaml entry for a test that does not exist")
	}

	close(f.incoming)
	close(f.outgoing)
	close(f.mappings)
	_ = g.Wait()
}

/*
gateBlockingInstr blocks inside GetOutgoing until the test releases it.

That is the only way to hold GetTestAndMockChans open while the test
arranges the state Start must survive: the INCOMING forwarder is already
spawned by then (GetIncoming runs first), so the test can fill it and
cancel ctx before Start ever reaches its post-setup gate.
*/
type gateBlockingInstr struct {
	*fakeInstr
	// Closed by the test to let GetOutgoing return.
	release chan struct{}
	// Closed by GetOutgoing when it has been entered, so the test knows
	// the incoming forwarder is live and GetTestAndMockChans is parked.
	entered chan struct{}

	once sync.Once
}

func (g *gateBlockingInstr) GetOutgoing(ctx context.Context, o models.OutgoingOptions) (<-chan *models.Mock, error) {
	g.once.Do(func() { close(g.entered) })
	<-g.release
	return g.fakeInstr.GetOutgoing(ctx, o)
}

func (g *gateBlockingInstr) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	<-ctx.Done()
	return models.AppError{AppErrorType: models.ErrCtxCanceled}
}

/*
START MUST TELL THE FORWARDERS WHEN NO CONSUMER IS COMING.

This is the PRODUCTION half of the shutdown-wedge fix, and until this test
existed nothing covered it. GetTestAndMockChans spawns three forwarders on
reqCtx (WithoutCancel); Start spawns their consumers three hundred lines
later, past an `if ctx.Err() != nil` gate that returns. On that path the
forwarders are alive with nobody reading, and each ends by handing over an
item it has already taken from the agent -- a send that parks forever.

`consumersStarted` plus `defer frames.Abandon()` is what closes it.
MEASURED, both halves are load-bearing and both were invisible to the
suite: setting the flag early, or deleting the defer, left
`go test ./...` fully green while Start took 30.0s to return (the
DrainErrGroup timeout) with a forwarder goroutine leaked for the process
lifetime. That is the original SEV-1, restored, reporting green.

THE TIMING IS THE ASSERTION, which is unusual and deliberate. The wedge
has no other observable: the run still ends, the same data is written, and
the error is the same. What changes is that teardown blocks for the full
drain budget. The threshold is far below that budget and far above the
healthy path, so it is not a benchmark -- 30s and ~3s do not overlap under
any load this suite runs at.
*/
func TestStart_ReturningBeforeItsConsumersDoesNotWedgeTheForwarders(t *testing.T) {
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	instr := &gateBlockingInstr{
		fakeInstr: f,
		release:   make(chan struct{}),
		entered:   make(chan struct{}),
	}
	r := &Recorder{
		logger:          zap.NewNop(),
		testDB:          &recTestDB{},
		mockDB:          &recMockDB{unencodable: map[string]bool{}},
		mappingDb:       &recMappingDB{},
		telemetry:       &recTelemetry{},
		instrumentation: instr,
		testSetConf:     recTestSetConf{},
		hooks:           BaseRecordHooks{},
		config:          &config.Config{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	/*
	 * RELEASED ON EVERY EXIT. Any t.Fatal below would otherwise leave
	 * GetOutgoing parked, holding Start and its errgroup for the rest of
	 * the binary's run -- so one failure here would take unrelated tests
	 * with it. The same guard gatedFailTestDB uses, for the same reason.
	 */
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(instr.release) }) }
	defer release()

	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	// GetTestAndMockChans is now parked inside GetOutgoing, which means
	// GetIncoming has already returned and the incoming forwarder is live.
	select {
	case <-instr.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("Start never reached GetOutgoing")
	}

	/*
	 * TWO SENDS, and two is the number. incomingChan carries one slot: the
	 * first lands in it, and the second is taken by the forwarder, which
	 * then parks trying to place it in the full buffer. So after the
	 * second send returns, the forwarder is holding an item it has already
	 * taken from the agent -- exactly the state the hand-over exists for,
	 * and exactly the state that used to park forever.
	 */
	for _, tc := range []*models.TestCase{
		{Name: "t-1", Kind: models.HTTP},
		{Name: "t-2", Kind: models.HTTP},
	} {
		select {
		case f.incoming <- tc:
		case <-time.After(30 * time.Second):
			t.Fatal("the incoming forwarder never took the test case")
		}
	}

	// Ctrl+C lands while the tail is still in flight, THEN setup finishes.
	// Start now finds ctx.Err() != nil at its gate and returns without ever
	// spawning a consumer.
	cancel()
	release()

	started := time.Now()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("Start did not return at all")
	}
	elapsed := time.Since(started)

	if elapsed > 20*time.Second {
		t.Fatalf("Start took %s to return after cancellation. The forwarders were "+
			"never told that no consumer was coming, so one is parked on its "+
			"shutdown hand-over and teardown is waiting out the full "+
			"DrainErrGroup budget -- and that goroutine is leaked for the "+
			"process lifetime, re-leaked per session in the DaemonSet "+
			"embedding. Check that Start still defers frames.Abandon() and "+
			"that consumersStarted is set only after every consumer is "+
			"spawned", elapsed)
	}

	/*
	 * AND IT DOES NOT SIT OUT THE MAPPING DRAIN EITHER.
	 *
	 * A SECOND, MUCH TIGHTER THRESHOLD, because the two failures it
	 * separates look identical from outside: the check above catches a
	 * forwarder parked forever, this one catches a forwarder that
	 * eventually gives up. `abandoned` was wired only into the mapping
	 * forwarder's SEND selects and not its main one, so an abandoned
	 * drain ignored it and waited out the whole mappingIdleGrace --
	 * MEASURED at a fixed 3.00s on every abandoned teardown, and
	 * invisible here because 3s passes a 20s assertion comfortably.
	 *
	 * SAFE AGAINST LOAD, which a timing assertion usually is not:
	 * mappingIdleGrace is a TIMER, not CPU work, so a busy machine does
	 * not move it. The healthy path measures 0.00s against a budget of
	 * half the grace period, so the two do not overlap under any load
	 * this suite runs at.
	 */
	if elapsed > mappingIdleGrace/2 {
		t.Fatalf("Start took %s to return, more than half of mappingIdleGrace "+
			"(%s). Nothing is wedged -- the mapping forwarder is draining a "+
			"stream whose output no consumer can read, and only stopping when "+
			"the stream goes quiet. Abandon has to be in its MAIN select, not "+
			"only in its sends", elapsed, mappingIdleGrace)
	}
}

/*
refusedOutgoingInstr fails GetOutgoing the way a not-yet-up agent does.

`utils.IsShutdownError` matches "connection refused", so this is NOT a
shutdown path: ctx is live and the recording is simply starting before the
agent socket is listening.
*/
type refusedOutgoingInstr struct {
	*fakeInstr
	// Closed by Run, which Start calls immediately AFTER it sets
	// consumersStarted -- an exact signal that the consumers are up, with
	// no sleep.
	running chan struct{}

	once sync.Once
}

func (r *refusedOutgoingInstr) GetOutgoing(context.Context, models.OutgoingOptions) (<-chan *models.Mock, error) {
	return nil, errors.New("dial unix /tmp/agent.sock: connect: connection refused")
}

func (r *refusedOutgoingInstr) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	r.once.Do(func() { close(r.running) })
	<-ctx.Done()
	return models.AppError{AppErrorType: models.ErrCtxCanceled}
}

/*
EVERY FRAME CHANNEL A CALLER RANGES OVER MUST BE CLOSEABLE.

GetTestAndMockChans has two early returns that hand back a usable
FrameChan when the agent is unreachable. The FIRST is before any
forwarder exists; the SECOND comes after the incoming forwarder has been
spawned, so there the forwarder closes incomingChan itself on its way out.
Both closed `incomingChan` and `outgoingChan` and left `mappingChan`
untouched.

An unwritten, unclosed channel is indistinguishable from no channel:
consumeMappings does `for mapping := range mappings` with no ctx escape,
so it parked there forever. Start spawns it unconditionally, so teardown
then waited out the entire DrainErrGroup budget before abandoning it, and
the goroutine plus its flush ticker leaked for the process lifetime.

MEASURED on this exact shape: 30.0s before the fix, ~200µs after. It is
worth a test rather than a comment because the trigger is ordinary --
"connection refused" is a recording started a moment too early, not a
crash -- and because the cost is invisible in the result: Start returns
nil either way, so the recording reports SUCCESS having captured
nothing, only slower.
*/
func TestStart_AnUnreachableAgentDoesNotLeaveTheMappingConsumerParked(t *testing.T) {
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	instr := &refusedOutgoingInstr{fakeInstr: f, running: make(chan struct{})}
	r := &Recorder{
		logger:          zap.NewNop(),
		testDB:          &recTestDB{},
		mockDB:          &recMockDB{unencodable: map[string]bool{}},
		mappingDb:       &recMappingDB{},
		telemetry:       &recTelemetry{},
		instrumentation: instr,
		testSetConf:     recTestSetConf{},
		hooks:           BaseRecordHooks{},
		config:          &config.Config{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	/*
	 * WAIT FOR AN EXACT SIGNAL, not a sleep.
	 *
	 * Start calls instrumentation.Run immediately after setting
	 * consumersStarted, so `running` closing means the consumers are up
	 * and the mapping consumer is ranging. (True for the default empty
	 * CommandType, which this test uses. Under docker-compose Run is
	 * called much earlier, so a config change here would quietly make
	 * this wait meaningless.)
	 *
	 * THIS WAIT IS LOAD-BEARING, and a comment here previously said it
	 * was not. MEASURED with the bug present: a 300ms sleep failed 5/5,
	 * no wait at all passed 0/5 -- because cancel() beat Start's
	 * post-setup gate, consumersStarted stayed false, consumeMappings was
	 * never spawned, and nothing could park. A too-short wait does not
	 * weaken this test, it makes it VACUOUS, which on a loaded machine is
	 * what a sleep eventually is.
	 */
	select {
	case <-instr.running:
	case <-time.After(60 * time.Second):
		t.Fatal("Start never reached instrumentation.Run, so its consumers never started")
	}
	cancel()

	started := time.Now()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("Start never returned")
	}
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Fatalf("Start took %s to return. The mapping consumer is ranging over a "+
			"channel that is never closed and never written, so teardown is "+
			"waiting out the full DrainErrGroup budget and that goroutine is "+
			"leaked for the process lifetime. Every early return in "+
			"GetTestAndMockChans that hands back a usable FrameChan has to "+
			"close mappingChan too", elapsed)
	}
}

/*
refusedIncomingInstr fails the FIRST agent call, GetIncoming.

That return happens BEFORE any forwarder is spawned, so nothing can be
wedged by it -- but it still hands the caller a FrameChan that Start
ranges over, and every channel in it has to be closed or the consumer
parks on it forever. It is the sibling of refusedOutgoingInstr and had no
test at all: `close(mappingChan)`, `Mappings:` and `Abandon:` on this
path were all live mutation survivors.
*/
type refusedIncomingInstr struct {
	*fakeInstr
	running chan struct{}

	once sync.Once
}

func (r *refusedIncomingInstr) GetIncoming(context.Context, models.IncomingOptions) (<-chan *models.TestCase, error) {
	return nil, errors.New("dial unix /tmp/agent.sock: connect: connection refused")
}

func (r *refusedIncomingInstr) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	r.once.Do(func() { close(r.running) })
	<-ctx.Done()
	return models.AppError{AppErrorType: models.ErrCtxCanceled}
}

// The GetIncoming half of the unreachable-agent case. Same property, same
// cost, different early return -- and this one was reached by no test, so
// its three closes were free to disappear.
func TestStart_AnUnreachableAgentOnTheFirstCallDoesNotParkAConsumer(t *testing.T) {
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	instr := &refusedIncomingInstr{fakeInstr: f, running: make(chan struct{})}
	r := &Recorder{
		logger:          zap.NewNop(),
		testDB:          &recTestDB{},
		mockDB:          &recMockDB{unencodable: map[string]bool{}},
		mappingDb:       &recMappingDB{},
		telemetry:       &recTelemetry{},
		instrumentation: instr,
		testSetConf:     recTestSetConf{},
		hooks:           BaseRecordHooks{},
		config:          &config.Config{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	select {
	case <-instr.running:
	case <-time.After(60 * time.Second):
		t.Fatal("Start never reached instrumentation.Run, so its consumers never started")
	}
	cancel()

	started := time.Now()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("Start never returned")
	}
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Fatalf("Start took %s to return. A consumer is ranging over a channel "+
			"this early return never closed, so teardown is waiting out the "+
			"full DrainErrGroup budget and that goroutine is leaked for the "+
			"process lifetime", elapsed)
	}
}

/*
THE TAIL IS PERSISTED, NOT MERELY NOT-HUNG.

Every wedge test in this file asserts TIMING -- that teardown does not
wait out the drain budget. None of them asserts that the test case the
forwarder was holding actually reached the store, and that gap is not
theoretical: MEASURED, deleting `handedOff = true` from the
GetOutgoing shutdown return drops most test cases on that path (23 of 40
delivered against 40 of 40 pristine), and deleting
`consumersStarted = true` drops a scheduler-dependent fraction of
ordinary shutdown hand-overs -- with `go test ./...` fully green both
times.

(The second figure has now been stated as "roughly half", "about 6%" and
"0.14%" by three separate measurements; see the loop-count note below for
why no number belongs here. The first read "EVERY test case" for a
round. Neither was measured; the second is contradicted by the loop-count
measurement ninety lines below, which is the number the loop is sized
from. A rate quoted in a comment is a claim like any other.)

Those are the tail-loss bug this whole change exists to remove, living
inside the fix's own control flow. A timing assertion cannot see them
because both mutants are fast: dropping data is quicker than persisting
it.
*/
func TestStart_AnUnreachableAgentStillPersistsTheTailItTook(t *testing.T) {
	/*
	 * LOOPED, because the loss is a RACE, not a certainty.
	 *
	 * With the flag dropped, `abandon()` has already fired by the time
	 * the consumer exists, so the forwarder's select has BOTH its
	 * ordinary send and its `<-abandoned` arm ready and the runtime picks
	 * between them. MEASURED: a single iteration passed against the
	 * mutant. One green run of a coin flip is not evidence, and shipping
	 * a test that reports one is worse than shipping none.
	 */
	for i := 0; i < 20; i++ {
		runUnreachableAgentTailCase(t, i)
	}
}

func runUnreachableAgentTailCase(t *testing.T, i int) {
	t.Helper()
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	instr := &refusedOutgoingInstr{fakeInstr: f, running: make(chan struct{})}
	testDB := &recTestDB{}
	r := &Recorder{
		logger:          zap.NewNop(),
		testDB:          testDB,
		mockDB:          &recMockDB{unencodable: map[string]bool{}},
		mappingDb:       &recMappingDB{},
		telemetry:       &recTelemetry{},
		instrumentation: instr,
		testSetConf:     recTestSetConf{},
		hooks:           BaseRecordHooks{},
		config:          &config.Config{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	select {
	case <-instr.running:
	case <-time.After(60 * time.Second):
		t.Fatalf("iteration %d: Start never reached instrumentation.Run", i)
	}

	// The agent hands over a test case on the path where GetOutgoing
	// failed. It was taken from the agent, so it is ours to persist.
	select {
	case f.incoming <- &models.TestCase{Name: "t-tail", Kind: models.HTTP}:
	case <-time.After(30 * time.Second):
		t.Fatalf("iteration %d: the incoming forwarder never took the test case", i)
	}

	/*
	 * PERSISTED, not merely not-hung. Every other wedge test here asserts
	 * timing, and timing cannot see this: dropping the item is FASTER
	 * than writing it.
	 */
	deadline := time.Now().Add(30 * time.Second)
	for {
		testDB.mu.Lock()
		var found bool
		for _, got := range testDB.inserted {
			if got == "t-tail" {
				found = true
			}
		}
		testDB.mu.Unlock()
		if found {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("iteration %d: the test case the forwarder took from the "+
				"agent was never persisted. It was already taken, so dropping it "+
				"loses recorded data silently -- and it drops often enough "+
				"that a single run proves nothing, which is why this is "+
				"looped", i)
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatalf("iteration %d: Start did not return", i)
	}
}

/*
AN ORDINARY Ctrl+C MUST NOT DROP THE ITEM IN HAND.

The shutdown hand-over exists so the tail is not silently lost, and
`consumersStarted = true` is what tells the forwarders a reader exists.
Without it every hand-over races Abandon, and the loser drops recorded
data with nothing logged.

LOOPED, AND THE COUNT IS SIZED BY MUTATION rather than chosen.

METHOD, so it can be redone rather than believed: delete
`consumersStarted = true` from record.go, build with `go test -overlay`,
and run this test to a fixed count many times. The loop is large enough
when every run in a sample of that size kills the mutant.

	4000 iterations   killed it in 16 of 16 runs
	 400 iterations   killed it in 26 of 36 (a separate, larger sample
	                  taken under concurrent load)

Healthy cost at 4000, RE-MEASURED on the code as it stands: 0.90-0.99s
over three runs, and 4.57s under -race. That is the price of the
coverage.

The figure quoted here before was 0.51-0.67s, and it predated the
waitForPersistedTestCase call in the loop body below -- each iteration
now waits for the third test case to reach the store, which costs at
least one poll tick. A cost quoted from before the code that dominates
it is exactly the kind of number this comment's next paragraph warns
against, so it is re-measured rather than reasoned about. No figure is
quoted for 400 iterations any more: the loop is 4000, so a cost for a
size nobody runs is a number that can only go stale.

NO PER-ITERATION RATE IS QUOTED HERE, deliberately. Two attempts to state
one were wrong -- first "about half", then "near 6%", each derived from a
sample of ten runs and each contradicted by a larger sample. The drop
depends on the Go scheduler, the machine and the concurrent load, so it
is not a property of the code and writing it down as one is how a loop
gets sized from a number that does not reproduce. Re-measure before
lowering the count; do not reason from a rate.

WHY NOT A DETERMINISTIC TEST INSTEAD. There is no seam. The flag's only
observable effect is which of two goroutines wins at teardown: on a clean
run the deferred abandon() fires after both forwarders have finished, so
it drops nothing and changes nothing measurable. Repetition is the honest
instrument here.
*/
func TestStart_ShutdownDoesNotDropTheTestCaseInHand(t *testing.T) {
	for i := 0; i < 4000; i++ {
		f := &fakeInstr{
			mappings: make(chan models.TestMockMapping),
			incoming: make(chan *models.TestCase),
			outgoing: make(chan *models.Mock),
		}
		testDB := &recTestDB{}
		r := &Recorder{
			logger:          zap.NewNop(),
			testDB:          testDB,
			mockDB:          &recMockDB{unencodable: map[string]bool{}},
			mappingDb:       &recMappingDB{},
			telemetry:       &recTelemetry{},
			instrumentation: &blockingInstr{f},
			testSetConf:     recTestSetConf{},
			hooks:           BaseRecordHooks{},
			config:          &config.Config{},
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- r.Start(ctx) }()

		// Three sends prove a consumer is draining (see the wedge table's
		// note on why two are not enough), so the fourth is the tail.
		for _, n := range []string{"t-1", "t-2", "t-3"} {
			select {
			case f.incoming <- &models.TestCase{Name: n, Kind: models.HTTP}:
			case <-time.After(30 * time.Second):
				cancel()
				t.Fatalf("iteration %d: forwarder never took %s", i, n)
			}
		}
		waitForPersistedTestCase(t, testDB, "t-3")

		select {
		case f.incoming <- &models.TestCase{Name: "t-tail", Kind: models.HTTP}:
		case <-time.After(30 * time.Second):
			cancel()
			t.Fatalf("iteration %d: forwarder never took the tail", i)
		}
		cancel()
		close(f.outgoing)
		close(f.incoming)
		close(f.mappings)

		select {
		case <-done:
		case <-time.After(90 * time.Second):
			t.Fatalf("iteration %d: Start did not return", i)
		}

		testDB.mu.Lock()
		var found bool
		for _, got := range testDB.inserted {
			if got == "t-tail" {
				found = true
			}
		}
		inserted := append([]string(nil), testDB.inserted...)
		testDB.mu.Unlock()
		if !found {
			t.Fatalf("iteration %d: the test case the forwarder was holding at "+
				"shutdown was never persisted (got %v). The hand-over exists so "+
				"the tail is not silently dropped; losing it is the bug, and a "+
				"single green run means nothing -- see this test's header for "+
				"why the loop is the size it is",
				i, inserted)
		}
	}
}

/*
boomOutgoingInstr fails GetOutgoing with an error that is NOT a shutdown,
and only once the test says so.

`utils.IsShutdownError` does not match this, so GetTestAndMockChans takes
its `return FrameChan{}, err` path rather than the graceful one -- with
the incoming forwarder already spawned and already holding an item.
*/
type boomOutgoingInstr struct {
	*fakeInstr
	release chan struct{}
}

func (b *boomOutgoingInstr) GetOutgoing(context.Context, models.OutgoingOptions) (<-chan *models.Mock, error) {
	<-b.release
	return nil, errors.New("boom")
}

func (b *boomOutgoingInstr) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	<-ctx.Done()
	return models.AppError{AppErrorType: models.ErrCtxCanceled}
}

/*
AN ERROR AFTER THE FORWARDER IS SPAWNED MUST STILL RELEASE IT.

GetTestAndMockChans spawns the incoming forwarder and then has two error
returns after it -- an unreadable TLS private key, and any NON-shutdown
failure from GetOutgoing. Both hand back a ZERO FrameChan, so `Abandon`
is nil, and Start bails at `if err != nil` before it can register its own
abandon defer. The forwarder is then unabandonable: it takes an item from
the agent, parks on the hand-over, and nothing in the process can release
it.

MEASURED before the fix: Start returned in 30.028s -- the whole
DrainErrGroup budget -- and the goroutine leaked for the process lifetime.
That is the same SEV-1 the consumersStarted defer closes, reached through
a door that defer cannot see, because Start never gets far enough to arm
it. The guard is inside GetTestAndMockChans for exactly that reason.

TWO SENDS, for the reason the wedge table gives: the first fills
incomingChan's one slot, the second is taken by the forwarder, which then
parks holding it.
*/
func TestGetTestAndMockChans_AnErrorAfterTheSpawnStillReleasesTheForwarder(t *testing.T) {
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	instr := &boomOutgoingInstr{fakeInstr: f, release: make(chan struct{})}
	r := &Recorder{
		logger:          zap.NewNop(),
		testDB:          &recTestDB{},
		mockDB:          &recMockDB{unencodable: map[string]bool{}},
		mappingDb:       &recMappingDB{},
		telemetry:       &recTelemetry{},
		instrumentation: instr,
		testSetConf:     recTestSetConf{},
		hooks:           BaseRecordHooks{},
		config:          &config.Config{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(instr.release) }) }
	defer release()

	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	for i := 0; i < 2; i++ {
		select {
		case f.incoming <- &models.TestCase{Name: "t", Kind: models.HTTP}:
		case <-time.After(30 * time.Second):
			t.Fatal("the incoming forwarder never took the test case")
		}
	}
	// Only now does setup fail, so a live forwarder is holding an item.
	release()

	started := time.Now()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("Start did not return at all")
	}
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Fatalf("Start took %s to return. An error return that comes AFTER the "+
			"incoming forwarder is spawned handed back a zero FrameChan with a "+
			"nil Abandon, so nothing could release that forwarder and teardown "+
			"waited out the whole DrainErrGroup budget -- with the goroutine "+
			"leaked for the process lifetime. Every such return in "+
			"GetTestAndMockChans has to leave handedOff false", elapsed)
	}
}

// TestGetTestAndMockChans_ShutdownHandoffDoesNotWedgeWithoutAConsumer is the
// safety net for the in-flight handover above.
//
// Both forwarders deliberately hand over an item already taken from the agent
// when ctx is cancelled, so the tail is not silently dropped. That send must
// complete even when NOTHING is consuming — which is a reachable state, not a
// hypothetical: Start can return at its post-setup ctx.Err() gate before it
// spawns the consumers, while these forwarders run on reqCtx (WithoutCancel)
// and keep pulling from the agent. On an unbuffered channel the handover parks
// forever: a 30s DrainErrGroup timeout on every affected Ctrl+C and a goroutine
// leaked for the process lifetime, which compounds in the DaemonSet embedding
// where Start is re-entered per session.
//
// This drives that exact window — take an item, say no consumer is coming,
// cancel, never consume — for all three forwarders.
//
// THE TWO-ITEM CASES ARE THE POINT of the second half of this table. A single
// item lands in the one-slot buffer on incomingChan/outgoingChan and unwinds
// even with the bug present, which is why the original two subtests passed
// against code that wedged: the buffer can already be full from an ordinary
// send. mappingChan is unbuffered and wedges on the first item, and it had no
// subtest at all.
func TestGetTestAndMockChans_ShutdownHandoffDoesNotWedgeWithoutAConsumer(t *testing.T) {
	for _, tc := range []struct {
		name string
		send func(f *fakeInstr)
	}{
		{"incoming", func(f *fakeInstr) { f.incoming <- &models.TestCase{Name: "tail", Kind: models.HTTP} }},
		{"outgoing", func(f *fakeInstr) { f.outgoing <- &models.Mock{Name: "tail", Kind: models.HTTP} }},
		{"mappings", func(f *fakeInstr) { f.mappings <- models.TestMockMapping{TestName: "tail"} }},
		{"incoming twice", func(f *fakeInstr) {
			f.incoming <- &models.TestCase{Name: "first", Kind: models.HTTP}
			f.incoming <- &models.TestCase{Name: "tail", Kind: models.HTTP}
		}},
		{"outgoing twice", func(f *fakeInstr) {
			f.outgoing <- &models.Mock{Name: "first", Kind: models.HTTP}
			f.outgoing <- &models.Mock{Name: "tail", Kind: models.HTTP}
		}},
	} {
		for _, order := range []struct {
			label        string
			abandonFirst bool
		}{
			{"abandon then cancel", true},
			{"cancel then abandon", false},
		} {
			send, abandonFirst := tc.send, order.abandonFirst
			t.Run(tc.name+", "+order.label, func(t *testing.T) {
				f := &fakeInstr{
					mappings: make(chan models.TestMockMapping),
					incoming: make(chan *models.TestCase),
					outgoing: make(chan *models.Mock),
				}
				r := &Recorder{logger: zap.NewNop(), instrumentation: f, config: &config.Config{}}

				g, gctx := errgroup.WithContext(context.Background())
				ctx, cancel := context.WithCancel(gctx)
				ctx = context.WithValue(ctx, models.ErrGroupKey, g)

				frames, err := r.GetTestAndMockChans(ctx)
				require.NoError(t, err)

				// The agent hands an item over; the forwarder now holds it. Nothing
				// is reading the frame channels — this is the "Start returned early"
				// shape, and Abandon is the half of it that Start's own defer
				// supplies. Without that call the forwarders are entitled to wait
				// forever for a consumer that is, as far as they know, still coming.
				send(f)
				/*
				 * BOTH ORDERS, AND ONLY ONE OF THEM IS PRODUCTION'S.
				 *
				 * Start's abandon defer is registered AFTER the stop defer that
				 * calls reqCtxCancel(), and Go runs defers LIFO, so abandon
				 * always runs FIRST. It also fires only when consumersStarted is
				 * false, whose single reachable return is the ctx.Err() gate. So
				 * `abandon_then_cancel` is the production order, and
				 * `cancel_then_abandon` names no path this binary can take.
				 *
				 * It is kept anyway, as a ROBUSTNESS case: a forwarder that
				 * depends on which of two back-to-back signals it observes first
				 * is fragile whether or not today's defer order happens to
				 * protect it, and the arms under test are selects over exactly
				 * those two signals. What is NOT true is the claim that used to
				 * sit here -- that cancellation ending the run produces the
				 * reverse order. It does not; the ordering is fixed by defer
				 * registration, not by what ended the run.
				 *
				 * NEITHER ORDER IS DETERMINISTIC HERE. It is tempting to read
				 * them as a clean partition -- abandon-then-cancel reaching only
				 * the ordinary send, cancel-then-abandon only the hand-over --
				 * and that is not what happens: the two calls are back to back, so
				 * by the time a forwarder reaches its select both arms are usually
				 * ready and the runtime picks at random. MEASURED, deleting the
				 * outgoing hand-over arm was caught 7 times in 10, and
				 * `abandon_then_cancel` was the catching subtest in half of those.
				 *
				 * So this table detects the hand-over arms PROBABILISTICALLY and
				 * the two ordinary-send arms not at all -- deleting either leaves
				 * the package green.
				 *
				 * AND NO SINGLE ARM HAS A DETERMINISTIC TEST ANYWHERE.
				 * TestStart_ReturningBeforeItsConsumersDoesNotWedgeTheForwarders
				 * kills the incoming pair TOGETHER 10/10, but measured
				 * individually it kills the inner arm 3 times in 10 and the
				 * outer arm 0 in 10. It pins "the fix was deleted", not "each
				 * arm is load-bearing". Said plainly because the gap is easy
				 * to mistake for coverage.
				 */
				if abandonFirst {
					frames.Abandon()
					cancel()
				} else {
					cancel()
					frames.Abandon()
				}

				done := make(chan error, 1)
				go func() { done <- g.Wait() }()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("the shutdown handover WEDGED with no consumer. In production this is a 30s " +
						"DrainErrGroup timeout on Ctrl+C plus a goroutine held for the process lifetime, " +
						"re-leaked on every session in the DaemonSet embedding")
				}
			})
		}
	}
}
