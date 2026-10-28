package record

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// hangGuard only fails a row that would otherwise hang; no row waits on it.
const hangGuard = 30 * time.Second

// consumerRun runs the mapping consumer beside a mock loop the test plays,
// so the order in which mocks and mappings arrive is the test's to choose.
type consumerRun struct {
	r               *Recorder
	db              *recMappingDB
	waits           *mappingWaits
	correlationMap  sync.Map
	asyncMockIDs    sync.Map
	shortPool       sync.Map
	dropped         droppedMockSet
	mappings        chan models.TestMockMapping
	mapped, revoked chan string
	done            chan error
	mappingsClosed  bool
}

func startConsumer(t *testing.T) *consumerRun {
	t.Helper()
	return startConsumerWith(t, zap.NewNop())
}

func startConsumerWith(t *testing.T, logger *zap.Logger) *consumerRun {
	t.Helper()
	c := &consumerRun{
		db:       &recMappingDB{},
		waits:    newMappingWaits(),
		mappings: make(chan models.TestMockMapping),
		mapped:   make(chan string, 64),
		revoked:  make(chan string, 64),
		done:     make(chan error, 1),
	}
	c.r = &Recorder{logger: logger, config: &config.Config{}, mappingDb: c.db}
	go func() {
		c.done <- c.r.consumeMappingsFor(context.Background(), "set-0", c.mappings,
			&c.correlationMap, &c.asyncMockIDs, &c.dropped, c.waits,
			func(tn string) { c.revoked <- tn }, &c.shortPool,
			func(tn string, entries []models.MockEntry) {
				for _, e := range entries {
					c.mapped <- tn + "/" + e.Name
				}
			})
	}()
	t.Cleanup(func() {
		c.closeMappings()
		c.waits.end()
		select {
		case err := <-c.done:
			assert.NoError(t, err)
		case <-time.After(hangGuard):
			t.Error("the mapping consumer did not return once its streams were over")
		}
	})
	return c
}

// save plays the mock loop saving a mock: publish it, then tell the waits.
func (c *consumerRun) save(tempID string) {
	c.correlationMap.Store(tempID, models.MockEntry{Name: tempID, Kind: string(models.MySQL)})
	c.waits.arrived(tempID)
}

// drop plays the mock loop failing to save a mock.
func (c *consumerRun) drop(tempID string) {
	c.dropped.add(tempID)
	c.waits.arrived(tempID)
}

func (c *consumerRun) send(t *testing.T, m models.TestMockMapping) {
	t.Helper()
	select {
	case c.mappings <- m:
	case <-time.After(hangGuard):
		t.Fatalf("the mapping consumer did not take the mapping of %s", m.TestName)
	}
}

func (c *consumerRun) closeMappings() {
	if !c.mappingsClosed {
		c.mappingsClosed = true
		close(c.mappings)
	}
}

func (c *consumerRun) finish(t *testing.T) {
	t.Helper()
	select {
	case err := <-c.done:
		require.NoError(t, err)
		c.done <- nil // for the cleanup
	case <-time.After(hangGuard):
		t.Fatal("the mapping consumer did not return")
	}
}

func recvName(t *testing.T, ch chan string, what string) string {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(hangGuard):
		t.Fatalf("never told: %s", what)
		return ""
	}
}

func persistedFor(db *recMappingDB, test string) []string {
	db.mu.Lock()
	defer db.mu.Unlock()
	var names []string
	for _, b := range db.batches {
		for _, e := range b[test] {
			names = append(names, e.Name)
		}
	}
	return names
}

// TestMappingWaits_MockSavedAfterItsMappingJoinsItsTest is the loss this
// change removes. test-1's mapping names m1, whose save the test holds back
// (on a loaded host the mock loop had 15 other mocks to write first); test-2's
// mapping comes next, its mock already saved. The consumer must hold m1 and
// go on to test-2 — the agent drops mappings it cannot hand over — and m1,
// saved only after test-2's mapping is through, must still join test-1.
//
// The order is what kills a restored clock: the old ~500 ms spin blocked on m1,
// gave it up, and only then reached test-2, so by the time this test saves m1
// test-1's mapping had already been persisted without it.
func TestMappingWaits_MockSavedAfterItsMappingJoinsItsTest(t *testing.T) {
	c := startConsumer(t)
	c.save("m2")

	c.send(t, models.TestMockMapping{TestName: "test-1", MockIDs: []string{"m1"}})
	c.send(t, models.TestMockMapping{TestName: "test-2", MockIDs: []string{"m2"}})
	assert.Equal(t, "test-2/m2", recvName(t, c.mapped, "test-2's mock"))
	require.Equal(t, 1, c.waits.heldCount(),
		"test-1's entry for m1, whose mock is not saved yet, must wait for it, not be given up")

	c.save("m1") // the gate: the save the test held back
	assert.Equal(t, "test-1/m1", recvName(t, c.mapped, "test-1's late mock"))

	c.closeMappings()
	c.waits.end()
	c.finish(t)
	assert.Equal(t, []string{"m1"}, persistedFor(c.db, "test-1"))
	assert.Equal(t, []string{"m2"}, persistedFor(c.db, "test-2"))
	tests, mocks := totalShortPool(&c.shortPool)
	assert.Zero(t, tests, "no test may be counted short: every mock was saved (%d mocks)", mocks)
}

// TestMappingWaits_DroppedMockResolvesAtOnce: a mock the recorder fails to save
// revokes its test the moment the drop is told, whether the drop comes before
// the mapping or after it — not at the end of the recording.
func TestMappingWaits_DroppedMockResolvesAtOnce(t *testing.T) {
	c := startConsumer(t)

	// The drop lands first: the mapping settles on it.
	c.drop("d0")
	c.send(t, models.TestMockMapping{TestName: "test-0", MockIDs: []string{"d0"}})
	assert.Equal(t, "test-0", recvName(t, c.revoked, "test-0's revoke"))

	// The mapping lands first: its entry waits, and the drop settles it.
	c.send(t, models.TestMockMapping{TestName: "test-1", MockIDs: []string{"d1"}})
	c.drop("d1")
	assert.Equal(t, "test-1", recvName(t, c.revoked, "test-1's revoke, told while the streams are still open"))
	assert.Zero(t, c.waits.heldCount())

	c.closeMappings()
	c.waits.end()
	c.finish(t)
	tests, _ := totalShortPool(&c.shortPool)
	assert.Zero(t, tests, "a revoked test is deleted, never counted short")
}

// TestMappingWaits_NeverSavedMockIsLostAtTheRecordingsEnd: a mock the stream
// never delivers keeps its entry waiting until the mock stream ends — the
// consumer does not return while it waits — and only then is its test counted
// as recorded with an incomplete mock set.
func TestMappingWaits_NeverSavedMockIsLostAtTheRecordingsEnd(t *testing.T) {
	c := startConsumer(t)
	c.save("ok")
	c.send(t, models.TestMockMapping{TestName: "test-1", MockIDs: []string{"never", "ok"}})
	assert.Equal(t, "test-1/ok", recvName(t, c.mapped, "test-1's saved mock"))
	c.closeMappings()

	tests, _ := totalShortPool(&c.shortPool)
	assert.Zero(t, tests, "counted short before the mock stream ended")
	select {
	case err := <-c.done:
		t.Fatalf("the consumer returned (%v) with an entry still waiting for its mock", err)
	default:
	}
	assert.Equal(t, 1, c.waits.heldCount())

	c.waits.end() // the mock stream is over
	c.finish(t)
	tests, mocks := totalShortPool(&c.shortPool)
	assert.Equal(t, 1, tests)
	assert.Equal(t, 1, mocks)
	assert.Equal(t, []string{"ok"}, persistedFor(c.db, "test-1"))
}

// TestMappingWaits_HeldEntriesAreBounded pins the memory bound: past
// maxHeldMappingEntries an entry is not held but settled at once, as one still
// waiting at the end would be, so the waits never hold more than the cap. The
// first entry the cap turns away is said once, in a Warn naming the cap, and
// each such entry's "Failed to correlate" line says the cap, not the stream's
// end, is why.
func TestMappingWaits_HeldEntriesAreBounded(t *testing.T) {
	prev := maxHeldMappingEntries
	maxHeldMappingEntries = 2
	t.Cleanup(func() { maxHeldMappingEntries = prev })

	core, logs := observer.New(zap.DebugLevel)
	r := &Recorder{logger: zap.New(core)}
	var correlationMap, asyncMockIDs sync.Map
	var dropped droppedMockSet
	waits := newMappingWaits()
	got, lost, uncorrelated := r.resolveMappingEntries(
		models.TestMockMapping{TestName: "t", MockIDs: []string{"a", "b", "c"}},
		&correlationMap, &asyncMockIDs, &dropped, waits)
	assert.Empty(t, got)
	assert.False(t, lost)
	assert.Equal(t, 2, waits.heldCount(), "held past the cap")
	assert.Equal(t, 1, uncorrelated, "the entry past the cap must be settled at once")
	_, _, more := r.resolveMappingEntries(
		models.TestMockMapping{TestName: "u", MockIDs: []string{"d"}},
		&correlationMap, &asyncMockIDs, &dropped, waits)
	assert.Equal(t, 1, more)
	capWarns := logs.FilterMessageSnippet("too many mapping entries are waiting").All()
	require.Len(t, capWarns, 1, "the cap must be said once per recording")
	assert.EqualValues(t, 2, capWarns[0].ContextMap()["maxHeldMappingEntries"])
	for _, e := range logs.FilterMessageSnippet("Failed to correlate").All() {
		assert.Equal(t, lossNextStep(reasonCapFull), e.ContextMap()["next_step"],
			"an entry the cap turned away was logged as if the stream had ended")
	}

	// Held ones still settle as their mocks arrive.
	correlationMap.Store("a", models.MockEntry{Name: "a"})
	waits.arrived("a")
	ready := waits.takeReady()
	require.Len(t, ready, 1)
	entry, fate := settleMock(ready[0].tempID, &correlationMap, &asyncMockIDs, &dropped)
	assert.Equal(t, fateMapped, fate)
	assert.Equal(t, "a", entry.Name)
	assert.Equal(t, 1, waits.heldCount())

	// After the end nothing is held: a late claim settles at once.
	waits.end()
	assert.Equal(t, 0, waits.heldCount())
	c := waits.claim("t", "z", func() (models.MockEntry, mockFate) { return models.MockEntry{}, fateUnknown })
	assert.False(t, c.held)
	assert.Equal(t, fateUnknown, c.fate)
	assert.Equal(t, reasonEnded, c.why)
}

// TestMappingWaits_LossReasonsAreDistinct: an entry that finds no mock says
// why. Two entries held for one mock: the second finds it taken. A mock
// dropped past the dropped-mock cap: the drop, not the stream's end.
func TestMappingWaits_LossReasonsAreDistinct(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	c := startConsumerWith(t, zap.New(core))
	c.send(t, models.TestMockMapping{TestName: "test-1", MockIDs: []string{"m1"}})
	c.send(t, models.TestMockMapping{TestName: "test-2", MockIDs: []string{"m1", "u1"}})
	c.save("m1")
	got := recvName(t, c.mapped, "m1's one taker")
	assert.Contains(t, []string{"test-1/m1", "test-2/m1"}, got)
	c.waits.droppedUntracked("u1")
	c.closeMappings()
	c.waits.end()
	c.finish(t)

	reasons := map[string]bool{}
	for _, e := range logs.FilterMessageSnippet("Failed to correlate").All() {
		reasons[e.ContextMap()["tempMockID"].(string)+": "+e.ContextMap()["next_step"].(string)] = true
	}
	assert.Equal(t, map[string]bool{
		"m1: " + lossNextStep(reasonTaken):         true,
		"u1: " + lossNextStep(reasonUntrackedDrop): true,
	}, reasons)
}

// TestMappingWaits_AsyncMarkerSetWhileHeld: the mock loop marks a mock async
// and only then saves it, so a mapping routinely names it while it is
// neither. The entry is held, the marker is set, then the mock is saved: it
// must stay out of the test's mapping (replay serves async egress from the
// whole corpus) and must not count as loss.
func TestMappingWaits_AsyncMarkerSetWhileHeld(t *testing.T) {
	c := startConsumer(t)
	c.save("good")
	c.send(t, models.TestMockMapping{TestName: "t", MockIDs: []string{"async-late", "good"}})
	assert.Equal(t, "t/good", recvName(t, c.mapped, "t's sync mock"))
	require.Equal(t, 1, c.waits.heldCount())

	c.asyncMockIDs.Store("async-late", struct{}{}) // the mock loop's order: marker first,
	c.save("async-late")                           // then the save
	c.closeMappings()
	c.finish(t) // returns only once the held entry is settled
	assert.Equal(t, []string{"good"}, persistedFor(c.db, "t"),
		"an ASYNC mock leaked into the per-test mapping: replay would serve it twice")
	tests, _ := totalShortPool(&c.shortPool)
	assert.Zero(t, tests, "a resolved async mock was counted as loss")
}

// recRun is a recording Start runs on fake streams the test feeds, with its
// mapping waits handed to the test (mappingWaitsMade) so a row can see an
// entry held, and settled, while the streams are still open.
type recRun struct {
	f      *fakeInstr
	r      *Recorder
	testDB *recTestDB
	tele   *recTelemetry
	mapDB  *recMappingDB
	waits  *mappingWaits
	cancel context.CancelFunc
	done   chan error
	closed bool
}

func startRecording(t *testing.T, mockDB MockDB, hooks RecordHooks, logger *zap.Logger, opts ...func(*Recorder)) *recRun {
	t.Helper()
	run := &recRun{
		f: &fakeInstr{
			mappings: make(chan models.TestMockMapping),
			incoming: make(chan *models.TestCase),
			outgoing: make(chan *models.Mock),
		},
		testDB: &recTestDB{},
		tele:   &recTelemetry{},
		mapDB:  &recMappingDB{upserted: make(chan struct{}, 1)},
		done:   make(chan error, 1),
	}
	made := make(chan *mappingWaits, 1)
	run.r = &Recorder{
		logger:          logger,
		testDB:          run.testDB,
		mockDB:          mockDB,
		mappingDb:       run.mapDB,
		telemetry:       run.tele,
		instrumentation: &blockingInstr{run.f},
		testSetConf:     recTestSetConf{},
		hooks:           hooks,
		config:          &config.Config{},
		mappingWaitsMade: func(w *mappingWaits) {
			w.changed = make(chan struct{}, 1)
			made <- w
		},
	}
	for _, o := range opts {
		o(run.r)
	}
	var ctx context.Context
	ctx, run.cancel = context.WithCancel(context.Background())
	t.Cleanup(run.cancel)
	go func() { run.done <- run.r.Start(ctx) }()
	select {
	case run.waits = <-made:
	case <-time.After(hangGuard):
		t.Fatal("Start never made its mapping waits")
	}
	return run
}

func (run *recRun) mock(t *testing.T, name string) {
	t.Helper()
	ts := time.Unix(1700000000, 0)
	select {
	case run.f.outgoing <- &models.Mock{Version: models.GetVersion(), Name: name, Kind: models.HTTP,
		Spec: models.MockSpec{ReqTimestampMock: ts, ResTimestampMock: ts}}:
	case <-time.After(hangGuard):
		t.Fatalf("recorder never consumed %s", name)
	}
}

func (run *recRun) frame(t *testing.T, m *models.Mock) {
	t.Helper()
	select {
	case run.f.outgoing <- m:
	case <-time.After(hangGuard):
		t.Fatalf("recorder never consumed %s", m.Name)
	}
}

func (run *recRun) test(t *testing.T, name string) {
	t.Helper()
	select {
	case run.f.incoming <- &models.TestCase{Name: name, Kind: models.HTTP}:
	case <-time.After(hangGuard):
		t.Fatalf("recorder never consumed %s", name)
	}
	waitForPersistedTestCase(t, run.testDB, name)
}

func (run *recRun) mapping(t *testing.T, test string, ids ...string) {
	t.Helper()
	select {
	case run.f.mappings <- models.TestMockMapping{TestName: test, MockIDs: ids}:
	case <-time.After(hangGuard):
		t.Fatalf("recorder never consumed the mapping of %s", test)
	}
}

// until returns once cond holds of the waits, looked at on each change.
func (run *recRun) until(t *testing.T, what string, cond func(*mappingWaits) bool) {
	t.Helper()
	for !cond(run.waits) {
		select {
		case <-run.waits.changed:
		case <-time.After(hangGuard):
			t.Fatalf("while the streams are open, never: %s", what)
		}
	}
}

// persisted returns once test's mapping is persisted with a mock.
func (run *recRun) persisted(t *testing.T, test string) {
	t.Helper()
	for len(persistedFor(run.mapDB, test)) == 0 {
		select {
		case <-run.mapDB.upserted:
		case <-time.After(hangGuard):
			t.Fatalf("%s's mapping was never persisted", test)
		}
	}
}

func (run *recRun) closeStreams() {
	if !run.closed {
		run.closed = true
		run.cancel()
		close(run.f.outgoing)
		close(run.f.incoming)
		close(run.f.mappings)
	}
}

// stop stops the recording and returns its suite telemetry.
func (run *recRun) stop(t *testing.T) map[string]interface{} {
	t.Helper()
	run.closeStreams()
	select {
	case err := <-run.done:
		require.NoError(t, err)
	case <-time.After(90 * time.Second):
		t.Fatal("Start did not return")
	}
	run.tele.mu.Lock()
	defer run.tele.mu.Unlock()
	require.NotNil(t, run.tele.suite)
	return run.tele.suite
}

func (run *recRun) deleted() []string {
	run.testDB.mu.Lock()
	defer run.testDB.mu.Unlock()
	return append([]string(nil), run.testDB.deleted...)
}

func heldIs(n int) func(*mappingWaits) bool {
	return func(w *mappingWaits) bool { return w.heldCount() == n }
}

func settledNow(w *mappingWaits) bool { return w.settled() }

// holds reports that test's entry for tempID is held.
func holds(test, tempID string) func(*mappingWaits) bool {
	return func(w *mappingWaits) bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, tn := range w.held[tempID] {
			if tn == test {
				return true
			}
		}
		return false
	}
}

// TestStart_MockSavedAfterItsMappingJoinsItsTest drives the real mock loop:
// mock-1's save is held at the store until test-2's mapping (whose mock was
// saved first) is persisted; mock-1 must still land in test-1's mapping, and
// when it is saved, not when the mock stream ends: test-1's mapping is
// persisted while every stream is still open.
func TestStart_MockSavedAfterItsMappingJoinsItsTest(t *testing.T) {
	release := make(chan struct{})
	run := startRecording(t, &recMockDB{unencodable: map[string]bool{}, gate: map[string]chan struct{}{"mock-1": release}},
		BaseRecordHooks{}, zap.NewNop())
	run.mock(t, "mock-0")
	run.mock(t, "mock-1")
	run.test(t, "test-1")
	run.test(t, "test-2")
	run.mapping(t, "test-1", "mock-1")
	run.mapping(t, "test-2", "mock-0")
	// test-2's mapping is persisted while mock-1 is still unsaved.
	run.persisted(t, "test-2")
	require.Empty(t, persistedFor(run.mapDB, "test-1"), "test-1 was persisted before its mock was saved")

	close(release) // mock-1's save goes through
	run.persisted(t, "test-1")
	assert.Equal(t, []string{"mock-1"}, persistedFor(run.mapDB, "test-1"),
		"a mock saved after its test's mapping arrived was left out of that mapping")
	suite := run.stop(t)
	assert.NotContains(t, suite, "tests-short-pool", "a mock saved late was counted as an incomplete mock set: %v", suite)
}

// TestStart_MockDroppedAfterItsMappingSettlesAtOnce: the real save fails on
// an unencodable mock whose mapping is already held. The drop settles the
// entry (its test is revoked) while the streams are still open, not at the
// mock stream's end.
func TestStart_MockDroppedAfterItsMappingSettlesAtOnce(t *testing.T) {
	release := make(chan struct{})
	run := startRecording(t, &recMockDB{unencodable: map[string]bool{"mock-1": true}, gate: map[string]chan struct{}{"mock-1": release}},
		BaseRecordHooks{}, zap.NewNop())
	run.test(t, "test-1")
	run.mock(t, "mock-1")
	run.mapping(t, "test-1", "mock-1")
	run.until(t, "test-1's entry held", heldIs(1))

	close(release) // the save fails: the mock cannot be encoded
	run.until(t, "the drop settled test-1's entry", settledNow)
	suite := run.stop(t)
	assert.Contains(t, run.deleted(), "test-1", "a test that owned a dropped mock was not revoked")
	assert.NotContains(t, suite, "tests-short-pool", "a revoked test was counted short: %v", suite)
}

// gatedSkipHooks skips the mocks it names, as AsyncRecorder does a collapsed
// poll cycle (Skip set, Spec.Async not), once the test releases each.
type gatedSkipHooks struct {
	BaseRecordHooks
	gate map[string]chan struct{}
}

func (h gatedSkipHooks) BeforeMockInsert(_ context.Context, info *MockContext) error {
	if ch := h.gate[info.Mock.Name]; ch != nil {
		<-ch
		info.Skip = true
	}
	return nil
}

// TestStart_MockSkippedAfterItsMappingSettlesAtOnce: a hook skips a mock whose
// mapping is already held. The skip is the mock's outcome: the entry settles
// while the streams are still open, as no loss.
func TestStart_MockSkippedAfterItsMappingSettlesAtOnce(t *testing.T) {
	release := make(chan struct{})
	run := startRecording(t, &recMockDB{unencodable: map[string]bool{}},
		gatedSkipHooks{gate: map[string]chan struct{}{"mock-1": release}}, zap.NewNop())
	run.test(t, "test-1")
	run.mock(t, "mock-1")
	run.mapping(t, "test-1", "mock-1")
	run.until(t, "test-1's entry held", heldIs(1))

	close(release) // the hook skips it
	run.until(t, "the skip settled test-1's entry", settledNow)
	suite := run.stop(t)
	assert.Empty(t, persistedFor(run.mapDB, "test-1"))
	assert.NotContains(t, run.deleted(), "test-1")
	assert.NotContains(t, suite, "tests-short-pool", "a skipped mock was counted as loss: %v", suite)
}

// TestStart_AgentRevokeReleasesTheTestsHeldEntries: the agent revokes a test
// over a mock it dropped and never sends, though the test's mapping names it.
// The revoke releases the entry at once, and a later mapping of the test is
// not held: nothing of a deleted test waits (or counts toward the cap) until
// the recording ends. test-2's mapping, after it, is held: once it is, the
// mapping before it has been claimed (the consumer takes them in order).
func TestStart_AgentRevokeReleasesTheTestsHeldEntries(t *testing.T) {
	run := startRecording(t, &recMockDB{unencodable: map[string]bool{}}, BaseRecordHooks{}, zap.NewNop())
	run.test(t, "test-1")
	run.mapping(t, "test-1", "never-sent")
	run.until(t, "test-1's entry held", heldIs(1))

	run.frame(t, revokeFrame("revoke-frame", "test-1"))
	run.until(t, "the revoke released test-1's entry", settledNow)
	run.mapping(t, "test-1", "never-sent-2")
	run.mapping(t, "test-2", "never-sent-3")
	run.until(t, "test-2's entry held", holds("test-2", "never-sent-3"))
	assert.Equal(t, 1, run.waits.heldCount(), "a revoked test's later entry was held")
	suite := run.stop(t)
	assert.Contains(t, run.deleted(), "test-1")
	assert.EqualValues(t, 1, suite["tests-short-pool"], "only test-2 ships short; the deleted test-1 is not counted: %v", suite)
}

// TestStart_NeverSentMockIsCountedAtTheStreamsEnd: a mock the agent names but
// never sends is counted short when the mock stream ends, by the mock loop
// ending the waits. The stop's drain gives nothing up for it, and its line
// says the stream ended without it.
func TestStart_NeverSentMockIsCountedAtTheStreamsEnd(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	run := startRecording(t, &recMockDB{unencodable: map[string]bool{}}, BaseRecordHooks{}, zap.New(core))
	run.test(t, "test-1")
	run.mapping(t, "test-1", "never-sent")
	run.until(t, "test-1's entry held", heldIs(1))
	suite := run.stop(t)
	assert.False(t, gaveUp(logs, "record")(), "the stop's drain had to give the recording's goroutines up")
	assert.EqualValues(t, 1, suite["tests-short-pool"], "%v", suite)
	lost := logs.FilterMessageSnippet("Failed to correlate").All()
	require.Len(t, lost, 1)
	assert.Equal(t, lossNextStep(reasonEnded), lost[0].ContextMap()["next_step"])
}

// armedClock is a ManualClock that says when a drain arms a look.
type armedClock struct {
	stall.ManualClock
	armed chan struct{}
}

func (c *armedClock) After(d time.Duration) (<-chan time.Time, func() bool) {
	ch, stop := c.ManualClock.After(d)
	select {
	case c.armed <- struct{}{}:
	default:
	}
	return ch, stop
}

// stopGivenUp stops a recording one of whose goroutines is wedged: each look
// a stop's drain arms is advanced by a quarter of its stall bound, so the
// record drain gives the wedged goroutine up on that bound with no sleep. It
// returns the suite telemetry.
func (run *recRun) stopGivenUp(t *testing.T, clk *armedClock) map[string]interface{} {
	t.Helper()
	run.closeStreams()
	ended := make(chan struct{})
	defer close(ended)
	go func() {
		for {
			select {
			case <-ended:
				return
			case <-clk.armed:
				clk.Advance(teardownDrainStall / 4)
			}
		}
	}()
	select {
	case err := <-run.done:
		require.NoError(t, err)
	case <-time.After(2 * time.Minute):
		t.Fatal("Start did not return")
	}
	run.tele.mu.Lock()
	defer run.tele.mu.Unlock()
	require.NotNil(t, run.tele.suite)
	return run.tele.suite
}

// lossLines maps each "Failed to correlate" line's mock to its next_step.
func lossLines(t *testing.T, logs *observer.ObservedLogs) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, e := range logs.FilterMessageSnippet("Failed to correlate").All() {
		id, _ := e.ContextMap()["tempMockID"].(string)
		_, dup := got[id]
		require.False(t, dup, "two loss lines for %s", id)
		got[id], _ = e.ContextMap()["next_step"].(string)
	}
	return got
}

// TestStart_WedgedMockLoopsHeldEntryIsCounted: test-1's mapping names mock-1,
// whose save never returns (a hung disk, a hook that never returns). The
// record drain gives the mock loop up on its bound, and that loop never ends
// the waits; test-1 ships without mock-1 in its mapping, and must be counted
// as recorded with an incomplete mock set, as the clock-bounded wait counted
// it.
func TestStart_WedgedMockLoopsHeldEntryIsCounted(t *testing.T) {
	wedge := make(chan struct{})
	t.Cleanup(func() { close(wedge) })
	core, logs := observer.New(zap.DebugLevel)
	clk := &armedClock{armed: make(chan struct{}, 1)}
	run := startRecording(t, &recMockDB{unencodable: map[string]bool{}, gate: map[string]chan struct{}{"mock-1": wedge}},
		BaseRecordHooks{}, zap.New(core), func(r *Recorder) { r.drainClock = clk })
	run.test(t, "test-1")
	run.mock(t, "mock-1")
	run.mapping(t, "test-1", "mock-1")
	run.until(t, "test-1's entry held", heldIs(1))

	suite := run.stopGivenUp(t, clk)
	require.True(t, gaveUp(logs, "record")(), "the record drain did not give the wedged mock loop up")
	assert.EqualValues(t, 1, suite["tests-short-pool"], "test-1 shipped without mock-1 in its mapping and was not counted short: %v", suite)
	assert.EqualValues(t, 1, suite["mocks-uncorrelated"])
	assert.Len(t, logs.FilterMessageSnippet("INCOMPLETE").All(), 1)
	assert.Equal(t, map[string]string{"mock-1": lossNextStep(reasonAbandoned)}, lossLines(t, logs))
}

// gatedBatchMapDB is a mapping store whose batch write waits for gate: the
// consumer that flushes into it is wedged until the test lets it go.
type gatedBatchMapDB struct {
	*recMappingDB
	gate    chan struct{}
	entered chan struct{}
}

func (d *gatedBatchMapDB) UpsertBatch(ctx context.Context, id string, byTest map[string][]models.MockEntry) error {
	select {
	case d.entered <- struct{}{}:
	default:
	}
	<-d.gate
	return d.recMappingDB.UpsertBatch(ctx, id, byTest)
}

// revokeFrame is the agent's RevokedTests frame for tests.
func revokeFrame(name string, tests string) *models.Mock {
	return &models.Mock{Version: models.GetVersion(), Name: name, Kind: models.RevokedTests,
		Spec: models.MockSpec{Metadata: map[string]string{"revoked_tests": tests}}}
}

// TestStart_WedgedConsumersReadyEntriesAreCounted: the mapping consumer is
// wedged (its batch write hangs, so its next apply blocks on the flush lock)
// while the mock loop goes on and ends normally, so the entries it hands back
// are never applied. test-1's mock-1 is saved after its entry is held;
// test-2's never-2 is never sent. The record drain gives the consumer up: the
// teardown must count both tests short, as the clock-bounded wait did, and
// each line must say why: mock-1 was saved but never applied, and the mock
// stream ended without never-2. test-3 (revoked while its entry was held) and
// test-4 (revoked after its mock-4 was handed back) are deleted: their entries
// say so in a Debug line, not as a loss, though their mocks were saved.
func TestStart_WedgedConsumersReadyEntriesAreCounted(t *testing.T) {
	wedge := make(chan struct{})
	t.Cleanup(func() { close(wedge) })
	core, logs := observer.New(zap.DebugLevel)
	clk := &armedClock{armed: make(chan struct{}, 1)}
	gdb := &gatedBatchMapDB{gate: wedge, entered: make(chan struct{}, 1)}
	run := startRecording(t, &recMockDB{unencodable: map[string]bool{}},
		BaseRecordHooks{}, zap.New(core), func(r *Recorder) {
			r.drainClock = clk
			gdb.recMappingDB = r.mappingDb.(*recMappingDB)
			r.mappingDb = gdb
		})
	run.mock(t, "mock-0")
	run.mock(t, "mock-2")
	for _, tn := range []string{"test-0", "test-1", "test-2", "test-3", "test-4"} {
		run.test(t, tn)
	}
	run.mapping(t, "test-0", "mock-0")
	select { // the ticker's flush is in the (hung) batch write, holding the flush lock
	case <-gdb.entered:
	case <-time.After(hangGuard):
		t.Fatal("no batch write")
	}
	run.mapping(t, "test-1", "mock-1")
	run.mapping(t, "test-3", "mock-3")
	run.mapping(t, "test-4", "mock-4")
	run.until(t, "the entries of test-1, test-3 and test-4 held", heldIs(3))
	// never-2 held proves the consumer took test-2's mapping (the mapping
	// stream has a forwarder, so the send alone does not): it is now applying
	// test-2's mock-2, which blocks on the flush lock.
	run.mapping(t, "test-2", "mock-2", "never-2")
	run.until(t, "test-2's entry held", heldIs(4))

	// The mock loop takes its frames in order, so each step below is done
	// when the next one's entry moves.
	run.frame(t, revokeFrame("revoke-3", "test-3")) // test-3's held entry is handed back as revoked
	run.until(t, "the revoke handed test-3's entry back", heldIs(3))
	run.mock(t, "mock-3") // saved, for a revoked test
	run.mock(t, "mock-4") // saved: test-4's entry is handed back, never applied
	run.until(t, "mock-4 handed back", heldIs(2))
	run.frame(t, revokeFrame("revoke-4", "test-4")) // test-4's entry is already handed back
	run.mock(t, "mock-1")                           // saved: test-1's entry is handed back, never applied
	run.until(t, "mock-1 handed back", heldIs(1))

	// The mock stream's end hands never-2 back too, before any drain can give
	// anything up (the drain clock moves only in stopGivenUp).
	run.closeStreams()
	run.until(t, "the mock stream's end handed never-2 back", heldIs(0))
	suite := run.stopGivenUp(t, clk)
	require.True(t, gaveUp(logs, "record")(), "the record drain did not give the wedged consumer up")
	assert.Subset(t, run.deleted(), []string{"test-3", "test-4"})
	assert.EqualValues(t, 2, suite["tests-short-pool"], "test-1 (mock-1 saved, never applied) and test-2 (never-2) must both count short: %v", suite)
	assert.EqualValues(t, 2, suite["mocks-uncorrelated"])
	assert.Len(t, logs.FilterMessageSnippet("INCOMPLETE").All(), 1)
	assert.Equal(t, map[string]string{
		"mock-1":  lossNextStep(reasonConsumerGivenUp),
		"never-2": lossNextStep(reasonEnded),
	}, lossLines(t, logs))
	for _, id := range []string{"mock-3", "mock-4"} {
		assert.Len(t, logs.FilterMessageSnippet("not waiting for a revoked test's mock").
			FilterField(zap.String("tempMockID", id)).All(), 1, "%s", id)
	}
}

// TestStart_RecorderRevokeReleasesTheTestsHeldEntries: test-1's mapping names
// mock-1, which the recorder fails to save, and never-1, which the agent
// never sends. The drop revokes test-1 (the recorder's own revoke, not the
// agent's frame), and that revoke must release never-1's entry at once: it
// settles while the streams are open, as a revoked test's entry (a Debug
// line, no "Failed to correlate" Error), and test-1 is deleted, not counted
// short.
func TestStart_RecorderRevokeReleasesTheTestsHeldEntries(t *testing.T) {
	release := make(chan struct{})
	core, logs := observer.New(zap.DebugLevel)
	run := startRecording(t, &recMockDB{unencodable: map[string]bool{"mock-1": true}, gate: map[string]chan struct{}{"mock-1": release}},
		BaseRecordHooks{}, zap.New(core))
	run.test(t, "test-1")
	run.mock(t, "mock-1")
	run.mapping(t, "test-1", "mock-1", "never-1")
	run.until(t, "test-1's entries held", heldIs(2))

	close(release) // the save fails: the mock cannot be encoded, so test-1 is revoked
	run.until(t, "the revoke released never-1's entry", settledNow)
	suite := run.stop(t)
	assert.Contains(t, run.deleted(), "test-1", "a test that owned a dropped mock was not revoked")
	assert.NotContains(t, suite, "tests-short-pool", "a revoked test was counted short: %v", suite)
	assert.Empty(t, lossLines(t, logs), "a revoked test's entry was reported as lost")
	assert.Len(t, logs.FilterMessageSnippet("not waiting for a revoked test's mock").
		FilterField(zap.String("tempMockID", "never-1")).All(), 1)
}

// TestStart_UntrackedDropSettlesAtOnceAndSaysSo: the dropped-mock set is
// full (shrunk to one, filled by mock-0, which no mapping names), so when the
// recorder fails to save mock-1 its test cannot be revoked. test-1's held
// entry must settle on that drop while the streams are open, not wait for
// the mock stream's end, and its line must say the drop is why.
func TestStart_UntrackedDropSettlesAtOnceAndSaysSo(t *testing.T) {
	prev := maxLiveDroppedMockIDs
	maxLiveDroppedMockIDs = 1
	t.Cleanup(func() { maxLiveDroppedMockIDs = prev })

	release := make(chan struct{})
	core, logs := observer.New(zap.DebugLevel)
	run := startRecording(t, &recMockDB{unencodable: map[string]bool{"mock-0": true, "mock-1": true},
		gate: map[string]chan struct{}{"mock-1": release}}, BaseRecordHooks{}, zap.New(core))
	run.mock(t, "mock-0") // dropped and tracked: the set is now full
	run.test(t, "test-1")
	run.mock(t, "mock-1")
	run.mapping(t, "test-1", "mock-1")
	run.until(t, "test-1's entry held", heldIs(1))

	close(release) // the save fails, past the dropped-mock cap
	run.until(t, "the untracked drop settled test-1's entry", settledNow)
	suite := run.stop(t)
	assert.NotContains(t, run.deleted(), "test-1", "an untracked drop cannot revoke its test")
	assert.EqualValues(t, 1, suite["tests-short-pool"], "%v", suite)
	assert.Equal(t, map[string]string{"mock-1": lossNextStep(reasonUntrackedDrop)}, lossLines(t, logs))
}
