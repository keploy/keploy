package mapdb

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
)

func mappingFor(testID string, names ...string) *models.Mapping {
	entries := make([]models.MockEntry, 0, len(names))
	for _, n := range names {
		entries = append(entries, models.MockEntry{Name: n, Kind: "Mongo"})
	}
	return &models.Mapping{
		Version:   string(models.GetVersion()),
		Kind:      models.MappingKind,
		TestSetID: "test-set-0",
		TestCases: []models.MappedTestCase{{ID: testID, Mocks: entries}},
	}
}

func namesFor(t *testing.T, db *MappingDb, testID string) []string {
	t.Helper()
	got, present, err := db.Get(context.Background(), "test-set-0")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !present {
		t.Fatal("mapping file not present")
	}
	var names []string
	for _, m := range got[testID] {
		names = append(names, m.Name)
	}
	return names
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A replay run reports the mocks it happened to consume, which is not
// necessarily everything the test needs: a subset run, a short-circuited run,
// or one degraded by an earlier mock miss all observe less. Writing such a run
// as authoritative truncates the pool, and because every later run did the
// same the pool could only ever shrink — the test then replays against a short
// pool or an empty one, which surfaces as no_mocks.
//
// Regression guard: Insert assigned `finalMappings[t.ID] = t.Mocks`
// unconditionally.
func TestInsertUnionsWhenNotRefreshing(t *testing.T) {
	db := New(zap.NewNop(), t.TempDir(), "mappings")
	ctx := context.Background()

	if err := db.Insert(ctx, mappingFor("post-query-29", "mock-517", "mock-518", "mock-519"), false); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	if err := db.Insert(ctx, mappingFor("post-query-29", "mock-520"), false); err != nil {
		t.Fatalf("second Insert: %v", err)
	}

	want := []string{"mock-517", "mock-518", "mock-519", "mock-520"}
	if got := namesFor(t, db, "post-query-29"); !equal(got, want) {
		t.Fatalf("a later run replaced the pool instead of widening it: got %v, want %v", got, want)
	}
}

// Union alone would make a wrong mapping permanent: nothing could ever remove
// a stale entry, and MappingDb has no Delete. --update-test-mapping is the
// operator saying "this list is wrong, take mine" — that has to replace.
func TestInsertReplacesWhenRefreshRequested(t *testing.T) {
	db := New(zap.NewNop(), t.TempDir(), "mappings")
	ctx := context.Background()

	if err := db.Insert(ctx, mappingFor("test-1", "stale-1", "stale-2", "wrong-3"), false); err != nil {
		t.Fatalf("seed Insert: %v", err)
	}
	if err := db.Insert(ctx, mappingFor("test-1", "good-1", "good-2"), true); err != nil {
		t.Fatalf("refresh Insert: %v", err)
	}

	want := []string{"good-1", "good-2"}
	if got := namesFor(t, db, "test-1"); !equal(got, want) {
		t.Fatalf("refresh did not replace the stale list: got %v, want %v", got, want)
	}
}

// A refresh must not disturb tests it does not mention — a subset refresh
// (`--tests test-A --update-test-mapping`) carries only that test.
func TestInsertRefreshLeavesOtherTestsAlone(t *testing.T) {
	db := New(zap.NewNop(), t.TempDir(), "mappings")
	ctx := context.Background()

	if err := db.Insert(ctx, mappingFor("test-A", "a-1", "a-2"), false); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if err := db.Insert(ctx, mappingFor("test-B", "b-1"), false); err != nil {
		t.Fatalf("seed B: %v", err)
	}
	if err := db.Insert(ctx, mappingFor("test-A", "a-9"), true); err != nil {
		t.Fatalf("refresh A: %v", err)
	}

	if got := namesFor(t, db, "test-A"); !equal(got, []string{"a-9"}) {
		t.Fatalf("test-A not refreshed: got %v", got)
	}
	if got := namesFor(t, db, "test-B"); !equal(got, []string{"b-1"}) {
		t.Fatalf("refresh of test-A disturbed test-B: got %v", got)
	}
}

// mergeMockEntries keys on name, so a mock reported by two runs stays once.
// Characterization of the helper rather than a guard for this change — it
// passes either way.
func TestInsertDoesNotDuplicateRepeatedMockEntries(t *testing.T) {
	db := New(zap.NewNop(), t.TempDir(), "mappings")
	ctx := context.Background()

	if err := db.Insert(ctx, mappingFor("test-1", "mock-1"), false); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	if err := db.Insert(ctx, mappingFor("test-1", "mock-1", "mock-2"), false); err != nil {
		t.Fatalf("second Insert: %v", err)
	}

	if got := namesFor(t, db, "test-1"); !equal(got, []string{"mock-1", "mock-2"}) {
		t.Fatalf("expected 2 unique entries: got %v", got)
	}
}
