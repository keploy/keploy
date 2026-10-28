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
	"go.uber.org/zap"
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
	c := &consumerRun{
		db:       &recMappingDB{},
		waits:    newMappingWaits(),
		mappings: make(chan models.TestMockMapping),
		mapped:   make(chan string, 64),
		revoked:  make(chan string, 64),
		done:     make(chan error, 1),
	}
	c.r = &Recorder{logger: zap.NewNop(), config: &config.Config{}, mappingDb: c.db}
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
		case <-c.done:
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
	case <-c.done:
		t.Fatal("the consumer returned with an entry still waiting for its mock")
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
// waiting at the end would be, so the waits never hold more than the cap.
func TestMappingWaits_HeldEntriesAreBounded(t *testing.T) {
	prev := maxHeldMappingEntries
	maxHeldMappingEntries = 2
	t.Cleanup(func() { maxHeldMappingEntries = prev })

	r := &Recorder{logger: zap.NewNop()}
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
	_, fate, held := waits.claim("t", "z", func() (models.MockEntry, mockFate) { return models.MockEntry{}, fateUnknown })
	assert.False(t, held)
	assert.Equal(t, fateUnknown, fate)
}

// TestStart_MockSavedAfterItsMappingJoinsItsTest drives the real mock loop:
// mock-1's save is held at the store until test-2's mapping (whose mock was
// saved first) is persisted; mock-1 must still land in test-1's mapping.
func TestStart_MockSavedAfterItsMappingJoinsItsTest(t *testing.T) {
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	testDB := &recTestDB{}
	tele := &recTelemetry{}
	release := make(chan struct{})
	mapDB := &recMappingDB{upserted: make(chan struct{}, 1)}
	r := &Recorder{
		logger:          zap.NewNop(),
		testDB:          testDB,
		mockDB:          &recMockDB{unencodable: map[string]bool{}, gate: map[string]chan struct{}{"mock-1": release}},
		mappingDb:       mapDB,
		telemetry:       tele,
		instrumentation: &blockingInstr{f},
		testSetConf:     recTestSetConf{},
		hooks:           BaseRecordHooks{},
		config:          &config.Config{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	ts := time.Unix(1700000000, 0)
	for _, name := range []string{"mock-0", "mock-1"} {
		select {
		case f.outgoing <- &models.Mock{
			Version: models.GetVersion(), Name: name, Kind: models.HTTP,
			Spec: models.MockSpec{ReqTimestampMock: ts, ResTimestampMock: ts},
		}:
		case <-time.After(hangGuard):
			t.Fatalf("recorder never consumed %s", name)
		}
	}
	for _, name := range []string{"test-1", "test-2"} {
		select {
		case f.incoming <- &models.TestCase{Name: name, Kind: models.HTTP}:
		case <-time.After(hangGuard):
			t.Fatalf("recorder never consumed %s", name)
		}
	}
	waitForPersistedTestCase(t, testDB, "test-2")
	for _, m := range []models.TestMockMapping{
		{TestName: "test-1", MockIDs: []string{"mock-1"}},
		{TestName: "test-2", MockIDs: []string{"mock-0"}},
	} {
		select {
		case f.mappings <- m:
		case <-time.After(hangGuard):
			t.Fatalf("recorder never consumed the mapping of %s", m.TestName)
		}
	}
	// test-2's mapping is persisted while mock-1 is still unsaved.
	for len(persistedFor(mapDB, "test-2")) == 0 {
		select {
		case <-mapDB.upserted:
		case <-time.After(hangGuard):
			t.Fatal("test-2's mapping was never persisted")
		}
	}
	require.Empty(t, persistedFor(mapDB, "test-1"), "test-1 was persisted before its mock was saved")

	close(release) // mock-1's save goes through
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
	assert.Equal(t, []string{"mock-1"}, persistedFor(mapDB, "test-1"),
		"a mock saved after its test's mapping arrived was left out of that mapping")
	tele.mu.Lock()
	suite := tele.suite
	tele.mu.Unlock()
	require.NotNil(t, suite)
	assert.NotContains(t, suite, "tests-short-pool", "a mock saved late was counted as an incomplete mock set: %v", suite)
}
