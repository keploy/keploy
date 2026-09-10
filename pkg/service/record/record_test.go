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

func (c *countingMapDb) Insert(context.Context, *models.Mapping) error { return nil }
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

type recTestDB struct {
	mu       sync.Mutex
	inserted []string
	deleted  []string
}

func (d *recTestDB) GetAllTestSetIDs(context.Context) ([]string, error) { return nil, nil }
func (d *recTestDB) InsertTestCase(_ context.Context, tc *models.TestCase, _ string, _ bool) error {
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

func (d *recMappingDB) Insert(context.Context, *models.Mapping) error                    { return nil }
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
}

func (tl *recTelemetry) RecordedTestSuite(_ string, _ int, _ map[string]int, metadata map[string]interface{}) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tl.suite = metadata
}
func (tl *recTelemetry) RecordedTestCaseMock(string)                                {}
func (tl *recTelemetry) RecordedMocks(map[string]int)                               {}
func (tl *recTelemetry) RecordedTestAndMocks()                                      {}
func (tl *recTelemetry) RecordSessionCompleted(int64, int64, int64, string, string) {}

type recTestSetConf struct{}

func (recTestSetConf) Read(context.Context, string) (*models.TestSet, error) { return nil, nil }
func (recTestSetConf) Write(context.Context, string, *models.TestSet) error  { return nil }

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
	tele.mu.Unlock()
	require.NotNil(t, suite)
	assert.NotContains(t, suite, "tests-short-pool",
		"a test the AGENT revoked is still counted as shipping with a short mock pool; it was "+
			"deleted, so the metric over-counts exactly the population it claims to exclude: %v", suite)
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
// This drives that exact window — take an item, cancel, never consume — for
// both the test-case and the mock forwarder.
func TestGetTestAndMockChans_ShutdownHandoffDoesNotWedgeWithoutAConsumer(t *testing.T) {
	for _, tc := range []struct {
		name string
		send func(f *fakeInstr)
	}{
		{"incoming", func(f *fakeInstr) { f.incoming <- &models.TestCase{Name: "tail", Kind: models.HTTP} }},
		{"outgoing", func(f *fakeInstr) { f.outgoing <- &models.Mock{Name: "tail", Kind: models.HTTP} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeInstr{
				mappings: make(chan models.TestMockMapping),
				incoming: make(chan *models.TestCase),
				outgoing: make(chan *models.Mock),
			}
			r := &Recorder{logger: zap.NewNop(), instrumentation: f, config: &config.Config{}}

			g, gctx := errgroup.WithContext(context.Background())
			ctx, cancel := context.WithCancel(gctx)
			ctx = context.WithValue(ctx, models.ErrGroupKey, g)

			_, err := r.GetTestAndMockChans(ctx)
			require.NoError(t, err)

			// The agent hands an item over; the forwarder now holds it. Nothing
			// is reading the frame channels — this is the "Start returned early"
			// shape.
			tc.send(f)
			cancel()

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
