package mapdb

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
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
// leaves it, and every other test that lists it too, for the scope at the
// place given, with its entry; one the scope lists already takes the place
// given; one neither lists is left alone. The scope stays in recorded order,
// and the other tests' entries are kept. A set with no mapping file moves
// nothing.
func TestMoveToWorkerBakesRecordingsIntoTheWorkerScope(t *testing.T) {
	ctx := context.Background()
	db := New(zap.NewNop(), t.TempDir(), "mappings")
	at := func(s int) string { return models.FormatMockTimestamp(time.Unix(1000+int64(s), 0)) }
	if err := db.UpsertBatch(ctx, "test-set-0", map[string][]models.MockEntry{
		"test-1": {{Name: "own-1", Kind: "Http", ReqTimestampMock: at(1)}, {Name: "delete-1", Kind: "Http", ReqTimestampMock: at(3)}},
		"test-2": {{Name: "own-2", Kind: "Http", ReqTimestampMock: at(5)}, {Name: "delete-1", Kind: "Http", ReqTimestampMock: at(3)}},
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

// AddToWorker adds entries to the worker scope in recorded order and leaves
// every test's list, the set's other sections, and a worker entry it already
// holds as they were; a set with no mapping file gets none, and no file.
func TestAddToWorkerAddsWithoutTakingFromTheTests(t *testing.T) {
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(format.FileExtension(), func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			db := NewWithFormat(zap.NewNop(), dir, "mappings", format)
			before := goldenMapping()
			if err := db.Insert(ctx, before); err != nil {
				t.Fatal(err)
			}
			listed := before.TestCases[1].Mocks[0] // mock-6, listed by post-url-1
			listed.Reason = models.WorkerRuleReason("jwks")
			held := before.Worker[0] // mock-2, in the worker scope already
			held.Reason = models.WorkerRuleReason("jwks")
			late := listed
			late.Name = "mock-9"
			late.ReqTimestampMock = models.FormatMockTimestamp(listed.RecordedAt().Add(time.Hour))

			added, err := db.AddToWorker(ctx, "test-set-0", []models.MockEntry{late, held, listed, {Name: ""}})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(added, []string{"mock-9", "mock-6"}) {
				t.Fatalf("added %v; want [mock-9 mock-6]", added)
			}
			after, err := db.GetMapping(ctx, "test-set-0")
			if err != nil {
				t.Fatal(err)
			}
			var worker []string
			for _, e := range after.Worker {
				worker = append(worker, e.Name)
				if e.Name == "mock-2" && e.Reason != "" {
					t.Fatalf("the entry held already was rewritten: %+v", e)
				}
			}
			// mock-2 (10ms), mock-6 (2010ms), mock-5 (2050ms), mock-9 (an hour on).
			if !reflect.DeepEqual(worker, []string{"mock-2", "mock-6", "mock-5", "mock-9"}) {
				t.Fatalf("worker scope %v; want [mock-2 mock-6 mock-5 mock-9]", worker)
			}
			if !reflect.DeepEqual(names(after.TestCases), names(before.TestCases)) || !reflect.DeepEqual(entryNames(after.Startup), entryNames(before.Startup)) {
				t.Fatalf("tests %v startup %v; want them as they were", names(after.TestCases), entryNames(after.Startup))
			}

			// A scope out of recorded order keeps its entries' order: an added
			// entry goes before the first entry recorded after it.
			t0 := listed.RecordedAt()
			ent := func(name string, sec int) models.MockEntry {
				e := listed
				e.Name, e.ReqTimestampMock = name, models.FormatMockTimestamp(t0.Add(time.Duration(sec)*time.Second))
				return e
			}
			if err := db.UpsertWorker(ctx, "test-set-0", []models.MockEntry{ent("w-late", 10), ent("w-early", 1)}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.AddToWorker(ctx, "test-set-0", []models.MockEntry{ent("w-mid", 5)}); err != nil {
				t.Fatal(err)
			}
			if m, _ := db.GetMapping(ctx, "test-set-0"); !reflect.DeepEqual(entryNames(m.Worker), []string{"w-mid", "w-late", "w-early"}) {
				t.Fatalf("worker scope %v; want [w-mid w-late w-early]", entryNames(m.Worker))
			}

			if added, err := db.AddToWorker(ctx, "test-set-1", []models.MockEntry{listed}); err != nil || added != nil {
				t.Fatalf("a set with no mapping file: added %v, %v; want none", added, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "test-set-1")); !os.IsNotExist(err) {
				t.Fatalf("a set with no mapping file got one (stat: %v)", err)
			}
		})
	}
}

func names(tcs []models.MappedTestCase) map[string][]string {
	out := map[string][]string{}
	for _, tc := range tcs {
		out[tc.ID] = entryNames(tc.Mocks)
	}
	return out
}

func entryNames(es []models.MockEntry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Name)
	}
	return out
}
