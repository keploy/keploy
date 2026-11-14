package mapdb

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
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

// MoveToWorkerFromStartup takes a mock the start-up list holds and no test
// lists (a timer's first call, recorded before the first test began) into
// the worker scope at the move's place, with every mark the move names,
// its recorded times and its own claim key, and no owner (no test's window
// recorded it); the start-up list keeps its other entries. A mock a test
// lists still moves as MoveToWorker moves it. MoveToWorker leaves the same
// start-up mock where it is, as every other caller expects. Both hold with
// the proven switch on and off.
func TestMoveToWorkerFromStartupTakesAStartupCallARuleNames(t *testing.T) {
	at := func(s int) string { return models.FormatMockTimestamp(time.Unix(1000+int64(s), 0)) }
	mapping := func() *models.Mapping {
		return &models.Mapping{
			Version:   string(models.V1Beta1),
			Kind:      models.MappingKind,
			TestSetID: "test-set-0",
			TestCases: []models.MappedTestCase{
				{ID: "test-1", Key: "k-test", Mocks: []models.MockEntry{{Name: "own-1", Kind: "Mongo", ReqTimestampMock: at(3)}, {Name: "touch-1", Kind: "Http", ReqTimestampMock: at(4)}}},
			},
			Startup: []models.MockEntry{
				{Name: "boot-read", Kind: "Mongo", ReqTimestampMock: at(0)},
				{Name: "lease", Kind: "Mongo", Key: "k-boot", ReqTimestampMock: at(1)},
				{Name: "boot-ping", Kind: "Mongo", ReqTimestampMock: at(2)},
			},
			Worker: []models.MockEntry{{Name: "w-gap", Kind: "Http", Place: "test-1", ReqTimestampMock: at(5)}},
		}
	}
	moves := []models.MockEntry{
		{Name: "lease", Rule: "lease@5e6f7a8b", Series: "lease/-/1", Once: true, Due: "10s", Unchanged: true, Reason: "background by rule lease"},
		{Name: "touch-1", Place: "test-1", Rule: "touch@01020304", Series: "touch/-/1", Once: true},
	}
	for _, sw := range []string{"1", ""} {
		t.Run("switch="+sw, func(t *testing.T) {
			t.Setenv(provenswitch.Env, sw)
			ctx := context.Background()
			db := New(zap.NewNop(), t.TempDir(), "mappings")
			if err := db.Insert(ctx, mapping()); err != nil {
				t.Fatal(err)
			}
			moved, err := db.MoveToWorkerFromStartup(ctx, "test-set-0", moves)
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{"lease", "touch-1"}; !reflect.DeepEqual(moved, want) {
				t.Fatalf("moved %v; want %v", moved, want)
			}
			m, err := db.GetMapping(ctx, "test-set-0")
			if err != nil || m == nil {
				t.Fatalf("GetMapping = %v, %v", m, err)
			}
			if got, want := entryNames(m.Startup), []string{"boot-read", "boot-ping"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("start-up %v; want %v: the moved call leaves it, the others stay", got, want)
			}
			if got, want := entryNames(m.Worker), []string{"lease", "touch-1", "w-gap"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("worker %v; want %v, in recorded order", got, want)
			}
			l := m.Worker[0]
			if l.Place != "" || l.Rule != "lease@5e6f7a8b" || l.Series != "lease/-/1" || !l.Once || l.Due != "10s" || !l.Unchanged || l.Reason != "background by rule lease" {
				t.Fatalf("the start-up call's entry %+v; want it at the set's start with the move's marks", l)
			}
			if l.Kind != "Mongo" || l.ReqTimestampMock != at(1) || l.Key != "k-boot" || l.Owner != "" {
				t.Fatalf("the start-up call's entry %+v; want its kind, recorded time and own key k-boot, and no owner", l)
			}
			tc := m.Worker[1]
			if tc.Key != "k-test" || tc.Place != "test-1" {
				t.Fatalf("the test's call %+v; want it moved as MoveToWorker moves it: key k-test, place test-1", tc)
			}
			if got := entryNames(m.TestCases[0].Mocks); !reflect.DeepEqual(got, []string{"own-1"}) {
				t.Fatalf("test-1 lists %v; want [own-1]", got)
			}

			// MoveToWorker leaves a start-up call alone.
			db2 := New(zap.NewNop(), t.TempDir(), "mappings")
			if err := db2.Insert(ctx, mapping()); err != nil {
				t.Fatal(err)
			}
			moved, err = db2.MoveToWorker(ctx, "test-set-0", moves)
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{"touch-1"}; !reflect.DeepEqual(moved, want) {
				t.Fatalf("MoveToWorker moved %v; want %v: it leaves a start-up call alone", moved, want)
			}
			m2, err := db2.GetMapping(ctx, "test-set-0")
			if err != nil || m2 == nil {
				t.Fatalf("GetMapping = %v, %v", m2, err)
			}
			if got, want := entryNames(m2.Startup), []string{"boot-read", "lease", "boot-ping"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("MoveToWorker left start-up %v; want %v", got, want)
			}

			// A start-up call with a key of the move's takes the move's key.
			db3 := New(zap.NewNop(), t.TempDir(), "mappings")
			if err := db3.Insert(ctx, mapping()); err != nil {
				t.Fatal(err)
			}
			if _, err := db3.MoveToWorkerFromStartup(ctx, "test-set-0", []models.MockEntry{{Name: "lease", Key: "k-move", Owner: "test-1"}}); err != nil {
				t.Fatal(err)
			}
			m3, err := db3.GetMapping(ctx, "test-set-0")
			if err != nil || m3 == nil {
				t.Fatalf("GetMapping = %v, %v", m3, err)
			}
			if i := slices.IndexFunc(m3.Worker, func(e models.MockEntry) bool { return e.Name == "lease" }); i < 0 || m3.Worker[i].Key != "k-move" || m3.Worker[i].Owner != "test-1" {
				t.Fatalf("worker %+v; want lease with the move's key k-move and owner test-1", m3.Worker)
			}

			// A mock a test lists moves from that list even when a hand
			// edit lists it in start-up too, which keeps its entry there.
			both := mapping()
			both.Startup = append(both.Startup, models.MockEntry{Name: "touch-1", Kind: "Http", Key: "k-boot", ReqTimestampMock: at(4)})
			db4 := New(zap.NewNop(), t.TempDir(), "mappings")
			if err := db4.Insert(ctx, both); err != nil {
				t.Fatal(err)
			}
			if _, err := db4.MoveToWorkerFromStartup(ctx, "test-set-0", []models.MockEntry{{Name: "touch-1", Place: "test-1"}}); err != nil {
				t.Fatal(err)
			}
			m4, err := db4.GetMapping(ctx, "test-set-0")
			if err != nil || m4 == nil {
				t.Fatalf("GetMapping = %v, %v", m4, err)
			}
			if got, want := entryNames(m4.Startup), []string{"boot-read", "lease", "boot-ping", "touch-1"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("start-up %v; want %v: a mock a test lists moves from the test's list", got, want)
			}
			if i := slices.IndexFunc(m4.Worker, func(e models.MockEntry) bool { return e.Name == "touch-1" }); i < 0 || m4.Worker[i].Key != "k-test" {
				t.Fatalf("worker %+v; want touch-1 with its test's key k-test", m4.Worker)
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
