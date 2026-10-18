package mapdb

import (
	"context"
	"reflect"
	"testing"
	"time"

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

// MoveToWorker bakes recordings into the worker scope: a mock a test lists
// leaves it for the scope at the place given, with its entry; one the scope
// lists already takes the place given; one neither lists is left alone. The
// scope stays in recorded order, and the other tests' entries are kept. A set
// with no mapping file moves nothing.
func TestMoveToWorkerBakesRecordingsIntoTheWorkerScope(t *testing.T) {
	ctx := context.Background()
	db := New(zap.NewNop(), t.TempDir(), "mappings")
	at := func(s int) string { return models.FormatMockTimestamp(time.Unix(1000+int64(s), 0)) }
	if err := db.UpsertBatch(ctx, "test-set-0", map[string][]models.MockEntry{
		"test-1": {{Name: "own-1", Kind: "Http", ReqTimestampMock: at(1)}, {Name: "delete-1", Kind: "Http", ReqTimestampMock: at(3)}},
		"test-2": {{Name: "own-2", Kind: "Http", ReqTimestampMock: at(5)}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertWorker(ctx, "test-set-0", []models.MockEntry{
		{Name: "w-boot", Kind: "Http", ReqTimestampMock: at(0)},
		{Name: "w-gap", Kind: "Http", Place: "test-1", ReqTimestampMock: at(4)},
	}); err != nil {
		t.Fatal(err)
	}
	moved, err := db.MoveToWorker(ctx, "test-set-0", []models.MockEntry{
		{Name: "delete-1"}, {Name: "w-gap", Place: ""}, {Name: "unknown", Place: "test-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(moved, []string{"delete-1", "w-gap"}) {
		t.Fatalf("moved %v; want [delete-1 w-gap]", moved)
	}
	m, err := db.GetMapping(ctx, "test-set-0")
	if err != nil || m == nil {
		t.Fatalf("GetMapping = %v, %v", m, err)
	}
	var worker []string
	for _, e := range m.Worker {
		worker = append(worker, e.Name+"@"+e.Place)
	}
	if want := []string{"w-boot@", "delete-1@", "w-gap@"}; !reflect.DeepEqual(worker, want) {
		t.Fatalf("worker scope %v; want %v, in recorded order", worker, want)
	}
	if e := m.Worker[1]; e.Kind != "Http" || e.ReqTimestampMock != at(3) {
		t.Fatalf("a moved mock lost its entry: %+v", e)
	}
	tests := map[string][]string{}
	for _, tc := range m.TestCases {
		for _, e := range tc.Mocks {
			tests[tc.ID] = append(tests[tc.ID], e.Name)
		}
	}
	if want := map[string][]string{"test-1": {"own-1"}, "test-2": {"own-2"}}; !reflect.DeepEqual(tests, want) {
		t.Fatalf("tests %v; want %v", tests, want)
	}
	if moved, err := db.MoveToWorker(ctx, "test-set-none", []models.MockEntry{{Name: "own-1"}}); err != nil || moved != nil {
		t.Fatalf("a set with no mapping moved %v, %v", moved, err)
	}
}
