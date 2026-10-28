package record

import (
	"context"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A testcase's mapping must include its ordinary (sync) egress mocks but MUST
// EXCLUDE async-egress mocks — even when the async mock's tempID is present in
// the mapping because its timestamp overlapped the testcase's request window.
// Async mocks are served at replay by the async engine from the full corpus, so
// per-test mapping them would wrongly bind a background delivery to a testcase.
func TestResolveMappingEntriesExcludesAsyncMocks(t *testing.T) {
	r := &Recorder{logger: zap.NewNop()}

	var correlationMap, asyncMockIDs sync.Map
	correlationMap.Store("sync-1", models.MockEntry{Name: "sync-1", Kind: "Http"})
	correlationMap.Store("async-1", models.MockEntry{Name: "async-1", Kind: "Http"})
	correlationMap.Store("async-2", models.MockEntry{Name: "async-2", Kind: "Http"})
	// async-1 and async-2 were stamped async by the AsyncRecorder hook.
	asyncMockIDs.Store("async-1", struct{}{})
	asyncMockIDs.Store("async-2", struct{}{})

	// The agent binned all three into this test's window (overlap), so all three
	// tempIDs appear in the mapping.
	mapping := models.TestMockMapping{
		TestName: "get-step-1",
		MockIDs:  []string{"sync-1", "async-1", "async-2"},
	}

	got, _, _ := r.resolveMappingEntries(mapping, &correlationMap, &asyncMockIDs, &droppedMockSet{}, nil)

	if len(got) != 1 {
		t.Fatalf("want exactly the 1 sync mock in the mapping, got %d: %+v", len(got), got)
	}
	if got[0].Name != "sync-1" {
		t.Fatalf("want sync-1 mapped, got %q", got[0].Name)
	}
	// All resolved tempIDs (async included) are consumed from the correlation map.
	for _, id := range []string{"sync-1", "async-1", "async-2"} {
		if _, ok := correlationMap.Load(id); ok {
			t.Fatalf("tempID %q should have been consumed from correlationMap", id)
		}
	}
}

// With no async mocks, every correlated mock is mapped (baseline: the exclusion
// doesn't drop ordinary mocks).
func TestResolveMappingEntriesKeepsAllSyncMocks(t *testing.T) {
	r := &Recorder{logger: zap.NewNop()}
	var correlationMap, asyncMockIDs sync.Map
	correlationMap.Store("m1", models.MockEntry{Name: "m1"})
	correlationMap.Store("m2", models.MockEntry{Name: "m2"})

	got, _, _ := r.resolveMappingEntries(
		models.TestMockMapping{TestName: "t", MockIDs: []string{"m1", "m2"}},
		&correlationMap, &asyncMockIDs, &droppedMockSet{}, nil,
	)
	if len(got) != 2 {
		t.Fatalf("want both sync mocks mapped, got %d: %+v", len(got), got)
	}
}

// TestResolveMappingEntries_DroppedMockRevokesOwner guards the consequence of
// letting a recording SURVIVE an unencodable mock: dropping a mock means its
// correlationMap entry never arrives, and the test that owned it must be
// revoked, or it replays with a short pool and fails for reasons that look like
// a product bug.
func TestResolveMappingEntries_DroppedMockRevokesOwner(t *testing.T) {
	r := &Recorder{logger: zap.NewNop()}
	var correlationMap, asyncMockIDs sync.Map
	var droppedMockIDs droppedMockSet

	correlationMap.Store("good", models.MockEntry{Name: "good"})
	droppedMockIDs.add("dropped-1")
	droppedMockIDs.add("dropped-2")

	got, lostMock, _ := r.resolveMappingEntries(
		models.TestMockMapping{TestName: "t", MockIDs: []string{"dropped-1", "good", "dropped-2"}},
		&correlationMap, &asyncMockIDs, &droppedMockIDs, nil,
	)

	if !lostMock {
		t.Error("the owning test was not flagged as having lost a mock, so it will not be revoked " +
			"and will replay with a short mock pool")
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("want only the resolvable mock, got %+v", got)
	}
	// Consuming the IDs must free them: mocks and drops accumulate for the whole
	// recording, and a set that only ever grows is a leak on a multi-day session.
	if live := droppedMockIDs.live.Load(); live != 0 {
		t.Errorf("droppedMockSet still holds %d consumed entries; it grows unbounded over a long recording", live)
	}
}

// TestDroppedMockSet_BoundedWhenNothingConsumes covers the entries that have no
// consumer at all: a background egress mock whose owning test never completes is
// never referenced by any mapping, so nothing ever take()s it. Unbounded, that
// is a slow leak for the life of the recording.
func TestDroppedMockSet_BoundedWhenNothingConsumes(t *testing.T) {
	var d droppedMockSet
	for i := 0; i < maxLiveDroppedMockIDs+500; i++ {
		d.add("orphan-" + strconv.Itoa(i))
	}
	if live := d.live.Load(); live > maxLiveDroppedMockIDs {
		t.Fatalf("droppedMockSet grew to %d entries, past the %d cap", live, maxLiveDroppedMockIDs)
	}
	// Past the cap add() must say so, so the caller can report that further
	// owning tests will not be revoked instead of silently believing they were.
	if d.add("one-more") {
		t.Error("add reported success past the cap; callers would log ownerRevocable=true for a test that is never revoked")
	}
}

// TestConsumeMappings_CountsShortPoolTests pins the accounting for the one
// data-loss path this change deliberately does NOT revoke.
//
// A mock the mock stream never delivered is not the same fact as a mock we
// know was dropped: this recorder does not know it was lost. Revoking on that
// inference would delete a customer's recorded test, so the test is still
// persisted with a short pool — exactly as before this change.
//
// What was missing is the total. The loss used to surface only as scattered
// per-occurrence log lines, so nobody could say how often it fires; that is the
// stated reason the semantics were left alone. Counting it makes the frequency
// measurable before anyone argues about changing them.
func TestConsumeMappings_CountsShortPoolTests(t *testing.T) {
	r := &Recorder{logger: zap.NewNop(), config: &config.Config{}, mappingDb: &countingMappingDB{}}
	var correlationMap, asyncMockIDs sync.Map
	var droppedMockIDs droppedMockSet
	var shortPoolByTest sync.Map

	// One mock per test: resolveMappingEntries consumes a correlation entry as
	// it resolves it, so sharing an ID would make the second test look short too.
	correlationMap.Store("m-short", models.MockEntry{Name: "m-short"})
	correlationMap.Store("m-healthy", models.MockEntry{Name: "m-healthy"})

	mappings := make(chan models.TestMockMapping, 2)
	// "ghost" never correlates and was never recorded as dropped.
	mappings <- models.TestMockMapping{TestName: "t-short", MockIDs: []string{"m-short", "ghost"}}
	mappings <- models.TestMockMapping{TestName: "t-healthy", MockIDs: []string{"m-healthy"}}
	close(mappings)

	var revoked []string
	require.NoError(t, r.consumeMappings(context.Background(), "set-0", mappings,
		&correlationMap, &asyncMockIDs, &droppedMockIDs,
		func(n string) { revoked = append(revoked, n) },
		&shortPoolByTest))

	assert.Empty(t, revoked,
		"a correlation timeout must NOT revoke: it is an inference, not the known-drop fact, and "+
			"deleting a recorded test on it turns a latency event into data destruction")
	tests, mocks := totalShortPool(&shortPoolByTest)
	assert.Equal(t, 1, tests,
		"the test persisted with an incomplete mock set was not counted, so the loss stays invisible in aggregate")
	assert.Equal(t, 1, mocks)
}

// countingMappingDB is a no-op MappingDb; these tests only care about the
// correlation accounting, not what reaches mappings.yaml.
type countingMappingDB struct{}

func (countingMappingDB) Insert(context.Context, *models.Mapping) error { return nil }
func (countingMappingDB) Upsert(context.Context, string, string, []models.MockEntry) error {
	return nil
}
func (countingMappingDB) UpsertBatch(context.Context, string, map[string][]models.MockEntry) error {
	return nil
}
func (countingMappingDB) UpsertWorker(context.Context, string, []models.MockEntry) error {
	return nil
}

// TestResolveMappingEntries_DroppedAsyncMockDoesNotRevoke guards the blast
// radius of the revoke path.
//
// Async-egress mocks are deliberately excluded from per-test mappings — replay
// serves them from the complete corpus — so a missing async mock is not a short
// pool for any test. If a dropped async mock set droppedOwner, one unencodable
// poll response would DELETE the healthy test whose request window that
// background egress happened to fall in, while the recording finished green.
// (Windows are non-overlapping FIFO and the entry is consumed on match, so it
// is one test per dropped async mock — still silent destruction of recorded
// data, which is the class this whole change exists to remove.)
func TestResolveMappingEntries_DroppedAsyncMockDoesNotRevoke(t *testing.T) {
	r := &Recorder{logger: zap.NewNop()}
	var correlationMap, asyncMockIDs sync.Map
	var droppedMockIDs droppedMockSet

	correlationMap.Store("good", models.MockEntry{Name: "good"})

	// The mock loop's real ordering, after the mapping is already held: the
	// marker is stored, then the insert is attempted, then the drop is
	// recorded and told. Pre-populating both maps before the call would miss
	// the ordering entirely, which is how an earlier revision passed its own
	// test while still deleting healthy tests.
	waits := newMappingWaits()
	got, lostMock, uncorrelated := r.resolveMappingEntries(
		models.TestMockMapping{TestName: "t", MockIDs: []string{"async-bad", "good"}},
		&correlationMap, &asyncMockIDs, &droppedMockIDs, waits,
	)
	require.Equal(t, 1, waits.heldCount(), "the async mock has no outcome yet: its entry must wait")
	asyncMockIDs.Store("async-bad", struct{}{})
	droppedMockIDs.add("async-bad") // the async mock failed to encode
	waits.arrived("async-bad")
	var late mappingResolution
	for _, e := range waits.takeReady() {
		entry, fate := settleMock(e.tempID, &correlationMap, &asyncMockIDs, &droppedMockIDs)
		r.addSettled(&late, e.test, e.tempID, entry, fate, &asyncMockIDs)
	}
	lostMock = lostMock || late.lost
	uncorrelated += late.uncorrelated
	got = append(got, late.entries...)

	if lostMock {
		t.Error("a dropped ASYNC mock revoked the test whose window it fell in. Async mocks are " +
			"excluded from per-test mappings by design, so their absence is never a short pool — " +
			"this silently deletes a healthy recorded test and finishes green")
	}
	if uncorrelated != 0 {
		t.Errorf("a dropped async mock was counted as a short pool (%d); its absence is by design", uncorrelated)
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("want only the sync mock, got %+v", got)
	}
}

// TestResolveMappingEntries_AsyncMockNeverCountsAsShortPool covers the
// never-arrived settle point. An async mock that never correlates is not an incomplete mock
// set for anyone — its absence is the design — so it must not inflate the
// short-pool counters whose credibility is the justification for leaving the
// never-arrived path unrevoked.
func TestResolveMappingEntries_AsyncMockNeverCountsAsShortPool(t *testing.T) {
	r := &Recorder{logger: zap.NewNop()}
	var correlationMap, asyncMockIDs sync.Map
	var droppedMockIDs droppedMockSet

	correlationMap.Store("good", models.MockEntry{Name: "good"})
	asyncMockIDs.Store("async-never", struct{}{})

	got, lostMock, uncorrelated := r.resolveMappingEntries(
		models.TestMockMapping{TestName: "t", MockIDs: []string{"async-never", "good"}},
		&correlationMap, &asyncMockIDs, &droppedMockIDs, nil,
	)

	if lostMock {
		t.Error("an uncorrelated async mock revoked its test")
	}
	if uncorrelated != 0 {
		t.Errorf("an uncorrelated async mock was counted as a short pool (%d); its absence is by design", uncorrelated)
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("want only the sync mock, got %+v", got)
	}
}

// TestResolveMappingEntries_BenignSkipDoesNotRevokeOrCount covers the mocks a
// RecordHook intentionally drops (a collapsed async poll no-change cycle). The
// agent does not know about the skip, so the tempID still arrives in a mapping.
// The skip settles its entry at once; revoking the test or counting it as
// an incomplete mock set is not — poll lanes collapse most cycles by design, so
// that would fire a data-loss alarm on every async-poll recording.
func TestResolveMappingEntries_BenignSkipDoesNotRevokeOrCount(t *testing.T) {
	r := &Recorder{logger: zap.NewNop()}
	var correlationMap, asyncMockIDs sync.Map
	var droppedMockIDs droppedMockSet

	correlationMap.Store("good", models.MockEntry{Name: "good"})
	markSkippedMock(&asyncMockIDs, "hook-skipped")

	// A live mock stream: the skip must settle the entry, not leave it waiting.
	waits := newMappingWaits()
	got, lostMock, uncorrelated := r.resolveMappingEntries(
		models.TestMockMapping{TestName: "t", MockIDs: []string{"hook-skipped", "good"}},
		&correlationMap, &asyncMockIDs, &droppedMockIDs, waits,
	)

	if lostMock {
		t.Error("a hook-skipped mock revoked its test; the skip is normal operation, not data loss")
	}
	if uncorrelated != 0 {
		t.Errorf("a hook-skipped mock was counted as a short pool (%d)", uncorrelated)
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("want only the real mock, got %+v", got)
	}
	if n := waits.heldCount(); n != 0 {
		t.Fatalf("a hook-skipped mock left %d entries waiting for a save that never comes", n)
	}
}

// TestConsumeMappings_ShortPoolCountsDistinctTests pins the unit of the
// counter. The agent emits a test's mocks as several DELTA mappings, so a
// per-mapping counter reports more "tests with a short pool" than the set even
// contains — and a test that gets revoked by a later delta must drop out of the
// count entirely, because it is deleted rather than shipped incomplete.
func TestConsumeMappings_ShortPoolCountsDistinctTests(t *testing.T) {
	r := &Recorder{logger: zap.NewNop(), config: &config.Config{}, mappingDb: &countingMappingDB{}}
	var correlationMap, asyncMockIDs sync.Map
	var droppedMockIDs droppedMockSet
	var shortPoolByTest sync.Map

	correlationMap.Store("m1", models.MockEntry{Name: "m1"})
	correlationMap.Store("m2", models.MockEntry{Name: "m2"})

	mappings := make(chan models.TestMockMapping, 2)
	// Two DELTA mappings for the SAME test, each carrying an uncorrelated mock.
	mappings <- models.TestMockMapping{TestName: "t-1", MockIDs: []string{"m1", "ghost-a"}}
	mappings <- models.TestMockMapping{TestName: "t-1", MockIDs: []string{"m2", "ghost-b"}}
	close(mappings)

	require.NoError(t, r.consumeMappings(context.Background(), "set-0", mappings,
		&correlationMap, &asyncMockIDs, &droppedMockIDs, nil, &shortPoolByTest))

	tests, mocks := totalShortPool(&shortPoolByTest)
	assert.Equal(t, 1, tests,
		"two delta mappings for one test counted as two tests; the reported figure can then exceed the test count")
	assert.Equal(t, 2, mocks,
		"the mock count is per uncorrelated mock and both deltas lost one")
}

// TestConsumeMappings_RevokeRollsBackShortPoolCounts covers the interaction
// between the two paths: a test can look merely short-pooled on one delta
// mapping and be revoked by a later one.
//
// The revoke wins — the test is deleted, so it never reaches replay with an
// incomplete mock set. Leaving the earlier increment standing inflates
// tests-short-pool and mocks-uncorrelated, and those numbers are the entire
// stated justification for NOT revoking on a correlation timeout. A metric
// that double-counts the cases it excludes cannot support that argument.
func TestConsumeMappings_RevokeRollsBackShortPoolCounts(t *testing.T) {
	r := &Recorder{logger: zap.NewNop(), config: &config.Config{}, mappingDb: &countingMappingDB{}}
	var correlationMap, asyncMockIDs sync.Map
	var droppedMockIDs droppedMockSet
	var shortPoolByTest sync.Map

	correlationMap.Store("m1", models.MockEntry{Name: "m1"})
	correlationMap.Store("m2", models.MockEntry{Name: "m2"})
	droppedMockIDs.add("really-dropped")

	mappings := make(chan models.TestMockMapping, 2)
	// Delta 1: only a correlation timeout → looks like a short pool.
	mappings <- models.TestMockMapping{TestName: "t-1", MockIDs: []string{"m1", "ghost"}}
	// Delta 2: a known drop → the test is revoked and deleted.
	mappings <- models.TestMockMapping{TestName: "t-1", MockIDs: []string{"m2", "really-dropped"}}
	close(mappings)

	var revoked []string
	require.NoError(t, r.consumeMappings(context.Background(), "set-0", mappings,
		&correlationMap, &asyncMockIDs, &droppedMockIDs,
		func(n string) { revoked = append(revoked, n) },
		&shortPoolByTest))

	assert.Equal(t, []string{"t-1"}, revoked, "the known drop must revoke the test")
	tests, mocks := totalShortPool(&shortPoolByTest)
	assert.Equal(t, 0, tests,
		"a REVOKED test is still counted as shipping with a short mock pool; it is deleted, not shipped")
	assert.Equal(t, 0, mocks,
		"the uncorrelated mock of a revoked test still inflates mocks-uncorrelated")
}

// totalShortPool mirrors the aggregation Start's teardown does over
// shortPoolByTest, so these tests assert on the numbers an operator actually
// sees rather than on the intermediate map.
func totalShortPool(m *sync.Map) (tests int, mocks int) {
	m.Range(func(_, v any) bool {
		tests++
		n, _ := v.(int64)
		mocks += int(n)
		return true
	})
	return tests, mocks
}

// TestResolveMappingEntries_SkippedMockEntryIsFreed pins the retention half of
// the hook-skip guard.
//
// asyncMockIDs is never pruned, and a hook-skipped mock is the highest-frequency
// event in a poll-heavy recording — a poll lane collapses most of its cycles by
// design. At roughly 105 bytes per entry a single 100ms lane retains hundreds of
// megabytes for the life of the session, which is an OOM risk in the DaemonSet
// embedding where Start is re-entered per session. So a SKIP-marked entry is
// released once the mapping that references it has been resolved.
//
// An ordinary async entry must NOT be released: it is in correlationMap, so a
// later mapping referencing a pruned entry would resolve it and append it,
// leaking a background egress into a test's mock pool.
func TestResolveMappingEntries_SkippedMockEntryIsFreed(t *testing.T) {
	r := &Recorder{logger: zap.NewNop()}
	var correlationMap, asyncMockIDs sync.Map
	var droppedMockIDs droppedMockSet

	correlationMap.Store("good", models.MockEntry{Name: "good"})
	// A hook-skipped mock (never persisted) and a real async mock (persisted).
	asyncMockIDs.Store("hook-skipped", skippedMockMarker)
	asyncMockIDs.Store("real-async", struct{}{})
	correlationMap.Store("real-async", models.MockEntry{Name: "real-async"})

	got, lostMock, uncorrelated := r.resolveMappingEntries(
		models.TestMockMapping{TestName: "t", MockIDs: []string{"hook-skipped", "real-async", "good"}},
		&correlationMap, &asyncMockIDs, &droppedMockIDs, nil,
	)
	if lostMock || uncorrelated != 0 {
		t.Errorf("unexpected loss: lostMock=%v uncorrelated=%d", lostMock, uncorrelated)
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("want only the sync mock, got %+v", got)
	}

	if _, still := asyncMockIDs.Load("hook-skipped"); still {
		t.Error("a consumed hook-skip entry was retained; asyncMockIDs is never pruned, so on a " +
			"poll-heavy recording this grows for the whole session and is an OOM risk under the DaemonSet")
	}
	if _, still := asyncMockIDs.Load("real-async"); !still {
		t.Error("an ORDINARY async entry was pruned. It is in correlationMap, so a later mapping " +
			"would now resolve and append it — leaking a background egress into a test's mock pool")
	}
}

// TestMarkSkippedMock_TagsTheEntryAsReleasable pins the one line that decides
// whether a hook-skipped entry can ever be freed.
//
// Every behavioural test still passes if this stores a plain struct{}{}: the
// mock is still excluded from mappings, still not revoked, still not counted.
// The only difference is that releaseSkippedMock no longer recognises it and
// the entry is retained for the life of the session — a silent return of the
// OOM risk. So assert the stored value directly.
func TestMarkSkippedMock_TagsTheEntryAsReleasable(t *testing.T) {
	var asyncMockIDs sync.Map

	markSkippedMock(&asyncMockIDs, "hook-skipped")

	v, ok := asyncMockIDs.Load("hook-skipped")
	if !ok {
		t.Fatal("a hook-skipped mock was not marked at all; it would be per-test mapped and could " +
			"revoke a healthy test")
	}
	if _, releasable := v.(skippedMock); !releasable {
		t.Errorf("the entry is marked but NOT as releasable (%T). releaseSkippedMock will skip it, "+
			"so it is retained for the whole session — the OOM risk returns with every test green", v)
	}
}

// Each test's mocks, as its mapping resolves them, are told to the capture
// watch (consumeMappingsFor's mapped): a delta mapping too, so a mock stamped
// late is charged to the test its mapping names.
func TestConsumeMappingsTellsEachTestsResolvedMocks(t *testing.T) {
	r := &Recorder{logger: zap.NewNop(), config: &config.Config{}, mappingDb: &countingMappingDB{}}
	var correlationMap, asyncMockIDs sync.Map
	var droppedMockIDs droppedMockSet
	var shortPoolByTest sync.Map
	correlationMap.Store("m1", models.MockEntry{Name: "mock-1"})
	correlationMap.Store("m2", models.MockEntry{Name: "mock-2"})
	mappings := make(chan models.TestMockMapping, 2)
	mappings <- models.TestMockMapping{TestName: "test-1", MockIDs: []string{"m1"}}
	mappings <- models.TestMockMapping{TestName: "test-1", MockIDs: []string{"m2"}} // a delta
	close(mappings)
	var told []string
	require.NoError(t, r.consumeMappingsFor(context.Background(), "set-0", mappings,
		&correlationMap, &asyncMockIDs, &droppedMockIDs, nil, nil, &shortPoolByTest,
		func(test string, entries []models.MockEntry) {
			for _, e := range entries {
				told = append(told, test+"/"+e.Name)
			}
		}))
	assert.Equal(t, []string{"test-1/mock-1", "test-1/mock-2"}, told)
}
