package mapdb

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
)

// A test's mapping arrives in more than one emission: the agent reports a
// test's mocks when its window resolves and reports more later for mocks it
// retroactively bins into that already-resolved window. The second emission is
// a DELTA, so Insert must union it onto what is already on disk.
//
// The regression this guards: Insert assigned `finalMappings[t.ID] = t.Mocks`,
// so the second emission deleted everything the first recorded. A test that
// really consumed several mocks ended up owning whichever subset arrived last
// — replaying afterwards against a short pool, or an empty one, which surfaces
// as a no_mocks failure. UpsertBatch already unions via mergeMockEntries for
// exactly this reason; Insert did not.
func TestInsertUnionsMockEntriesAcrossEmissions(t *testing.T) {
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "mappings")
	ctx := context.Background()

	mapping := func(entries ...models.MockEntry) *models.Mapping {
		return &models.Mapping{
			Version:   string(models.GetVersion()),
			Kind:      models.MappingKind,
			TestSetID: "test-set-0",
			TestCases: []models.MappedTestCase{{ID: "post-query-29", Mocks: entries}},
		}
	}

	// First emission: the authz reads resolved with the test's window.
	if err := db.Insert(ctx, mapping(
		models.MockEntry{Name: "mock-517", Kind: "Mongo"},
		models.MockEntry{Name: "mock-518", Kind: "Mongo"},
		models.MockEntry{Name: "mock-519", Kind: "Mongo"},
	)); err != nil {
		t.Fatalf("first Insert: %v", err)
	}

	// Second emission: one more mock binned into the same window afterwards.
	if err := db.Insert(ctx, mapping(
		models.MockEntry{Name: "mock-520", Kind: "Mongo"},
	)); err != nil {
		t.Fatalf("second Insert: %v", err)
	}

	got, present, err := db.Get(ctx, "test-set-0")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !present {
		t.Fatal("mapping file not present after Insert")
	}

	var names []string
	for _, m := range got["post-query-29"] {
		names = append(names, m.Name)
	}

	want := []string{"mock-517", "mock-518", "mock-519", "mock-520"}
	if len(names) != len(want) {
		t.Fatalf("second emission replaced the first instead of unioning: got %v, want %v", names, want)
	}
	for i, n := range want {
		if names[i] != n {
			t.Fatalf("recorded order not preserved: got %v, want %v", names, want)
		}
	}
}

// A mock reported twice must not be duplicated — mergeMockEntries keys on name.
func TestInsertDoesNotDuplicateRepeatedMockEntries(t *testing.T) {
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "mappings")
	ctx := context.Background()

	mapping := func(entries ...models.MockEntry) *models.Mapping {
		return &models.Mapping{
			Version:   string(models.GetVersion()),
			Kind:      models.MappingKind,
			TestSetID: "test-set-0",
			TestCases: []models.MappedTestCase{{ID: "test-1", Mocks: entries}},
		}
	}

	entry := models.MockEntry{Name: "mock-1", Kind: "Mongo"}
	if err := db.Insert(ctx, mapping(entry)); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	if err := db.Insert(ctx, mapping(entry, models.MockEntry{Name: "mock-2", Kind: "Mongo"})); err != nil {
		t.Fatalf("second Insert: %v", err)
	}

	got, _, err := db.Get(ctx, "test-set-0")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if n := len(got["test-1"]); n != 2 {
		t.Fatalf("expected 2 unique entries, got %d: %+v", n, got["test-1"])
	}
}
