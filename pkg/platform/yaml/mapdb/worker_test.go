package mapdb

import (
	"context"
	"reflect"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
)

// The worker scope (models.Mapping.Worker) is written whole by UpsertWorker,
// read back with its places, and kept by every other write: a later
// UpsertBatch and Insert of the test entries, which cannot see it. An empty
// UpsertWorker clears it.
func TestTheWorkerScopeRoundTripsAndSurvivesTheOtherWrites(t *testing.T) {
	ctx := context.Background()
	db := New(zap.NewNop(), t.TempDir(), "mappings")
	worker := []models.MockEntry{{Name: "w-boot", Kind: "Http"}, {Name: "w-late", Kind: "Http", Place: "test-1"}}
	if err := db.UpsertBatch(ctx, "test-set-0", map[string][]models.MockEntry{"test-1": {{Name: "own", Kind: "Http"}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertWorker(ctx, "test-set-0", worker); err != nil {
		t.Fatal(err)
	}
	read := func(what string) *models.Mapping {
		t.Helper()
		m, err := db.GetMapping(ctx, "test-set-0")
		if err != nil || m == nil {
			t.Fatalf("%s: GetMapping = %v, %v", what, m, err)
		}
		return m
	}
	if m := read("after UpsertWorker"); !reflect.DeepEqual(m.Worker, worker) || len(m.TestCases) != 1 {
		t.Fatalf("after UpsertWorker: worker %+v, %d test(s); want %+v and the test entry kept", m.Worker, len(m.TestCases), worker)
	}
	if err := db.UpsertBatch(ctx, "test-set-0", map[string][]models.MockEntry{"test-2": {{Name: "own-2", Kind: "Http"}}}); err != nil {
		t.Fatal(err)
	}
	if m := read("after UpsertBatch"); !reflect.DeepEqual(m.Worker, worker) {
		t.Fatalf("UpsertBatch dropped the worker scope: %+v", m.Worker)
	}
	if err := db.Insert(ctx, &models.Mapping{Version: string(models.GetVersion()), Kind: models.MappingKind, TestSetID: "test-set-0",
		TestCases: []models.MappedTestCase{{ID: "test-3", Mocks: []models.MockEntry{{Name: "own-3", Kind: "Http"}}}}}); err != nil {
		t.Fatal(err)
	}
	if m := read("after Insert"); !reflect.DeepEqual(m.Worker, worker) {
		t.Fatalf("Insert dropped the worker scope: %+v", m.Worker)
	}
	if err := db.UpsertWorker(ctx, "test-set-0", nil); err != nil {
		t.Fatal(err)
	}
	if m := read("after clearing"); len(m.Worker) != 0 || len(m.TestCases) != 3 {
		t.Fatalf("an empty UpsertWorker left worker %+v and %d test(s); want none and the 3 test entries", m.Worker, len(m.TestCases))
	}
}
