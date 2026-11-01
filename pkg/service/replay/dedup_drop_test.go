package replay

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.uber.org/zap"
)

func perTestMockState(name string) models.MockState {
	return models.MockState{Name: name, Kind: models.HTTP, Type: "mocks", Lifetime: models.LifetimePerTest}
}

func mockEntries(names ...string) []models.MockEntry {
	out := make([]models.MockEntry, 0, len(names))
	for _, n := range names {
		out = append(out, models.MockEntry{Name: n})
	}
	return out
}

// planDuplicateDrops deletes a mock only duplicates consumed whose lines go,
// and a duplicate's line goes only with its mocks; every other mock stays.
func TestPlanDuplicateDrops(t *testing.T) {
	consumed := map[string]models.MockState{}
	for _, n := range []string{"m1", "m2", "m3", "m5", "m6", "m10", "m11"} {
		consumed[n] = perTestMockState(n)
	}
	consumed["m4"] = models.MockState{Name: "m4", Kind: models.HTTP, Type: "mocks", Lifetime: models.LifetimeSession}
	consumed["m7"] = models.MockState{Name: "m7", Kind: models.HTTP, Type: "config", Lifetime: models.LifetimePerTest}
	in := duplicateDropIn{
		dups: []string{"d1", "d2", "d3", "d4"},
		consumers: map[string][]string{
			"m1":  {"d1"},       // d1's own: deleted
			"m2":  {"d1", "k1"}, // a kept test's too: stays
			"m3":  {"d2"},       // d2 keeps its line (m9), so its mocks stay
			"m4":  {"d4"},       // reusable: stays
			"m5":  {"d4"},       // in the startup section: stays
			"m6":  {"d4"},       // the setup drain consumed it: stays
			"m7":  {"d4"},       // a config mock: stays
			"m10": {"d4"},       // a kept test's line on disk names it: stays
			"m11": {"d3"},       // d3 is in a concurrency group: stays
		},
		consumed: consumed,
		setup:    map[string]bool{"m6": true},
		run: &models.Mapping{
			TestCases: []models.MappedTestCase{
				{ID: "d1", Mocks: mockEntries("m1", "m2")},
				{ID: "k1", Mocks: mockEntries("m2")},
				{ID: "d2", Mocks: mockEntries("m3", "m9")},
				{ID: "d3", Mocks: mockEntries("m11")},
				{ID: "d4", Mocks: mockEntries("m4", "m5", "m6", "m7", "m10")},
			},
			Startup: mockEntries("m5"),
		},
		disk: &models.Mapping{
			TestCases: []models.MappedTestCase{{ID: "k2", Mocks: mockEntries("m10")}},
			Groups:    []models.ConcurrencyGroup{{ID: "g", Tests: []string{"d3", "k3"}}},
		},
	}
	p := planDuplicateDrops(in)
	if !reflect.DeepEqual(p.mocks, []string{"m1"}) {
		t.Errorf("mocks to delete = %v, want [m1]", p.mocks)
	}
	// d4's line names a reusable, a startup, a setup and a config mock no kept
	// line names: it stays with them. d2's names m9, which nothing deletes.
	if !reflect.DeepEqual(p.tests, []string{"d1"}) {
		t.Errorf("lines that may go = %v, want [d1]", p.tests)
	}
	if got := p.linesToDrop(nil); len(got) != 0 {
		t.Errorf("with m1 kept by the store (a kept mock needs it), lines dropped = %v, want none", got)
	}
	if got := p.linesToDrop([]string{"m1"}); !reflect.DeepEqual(got, []string{"d1"}) {
		t.Errorf("with m1 deleted, lines dropped = %v, want [d1]", got)
	}

	// A duplicate that keeps its line keeps every mock it names, and so the
	// line of another duplicate that names one of them.
	chain := duplicateDropIn{
		dups:      []string{"a", "b"},
		consumers: map[string][]string{"x": {"a", "b"}, "y": {"b"}},
		consumed:  map[string]models.MockState{"x": perTestMockState("x"), "y": perTestMockState("y")},
		run: &models.Mapping{TestCases: []models.MappedTestCase{
			{ID: "a", Mocks: mockEntries("x", "z")}, // z: nothing deletes it
			{ID: "b", Mocks: mockEntries("x", "y")},
		}},
	}
	if p := planDuplicateDrops(chain); len(p.mocks) != 0 || len(p.tests) != 0 {
		t.Errorf("chain: mocks %v, lines %v; want none: a keeps x, so b keeps its line", p.mocks, p.tests)
	}
	if p := planDuplicateDrops(duplicateDropIn{consumers: in.consumers, consumed: consumed, run: in.run}); len(p.mocks) != 0 || len(p.tests) != 0 {
		t.Errorf("no duplicates: mocks %v, lines %v; want none", p.mocks, p.tests)
	}
}

// duplicateNamerHooks names duplicates; any other hook panics.
type duplicateNamerHooks struct {
	TestHooks
	dups []string
}

func (h duplicateNamerHooks) GetDuplicateTestCaseNames(string) []string { return h.dups }

// noNamerHooks names none: hooks without DuplicateNamer.
type noNamerHooks struct{ TestHooks }

// dropFixture writes a set of per-test mocks mock-0..mock-3 and a mapping in
// which d1 owns mock-0, k1 mock-1 and mock-2, and d2 mock-3 and mock-9 (a
// mock no store deletes), and returns the two files' paths.
func duplicateDropFixture(t *testing.T, dir string) (mocksFile, mappingFile string) {
	t.Helper()
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()
	ms := mockdb.New(zap.NewNop(), dir, "mocks")
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		ts := t0.Add(time.Duration(i) * time.Second)
		if err := ms.InsertMock(ctx, &models.Mock{
			Version: models.GetVersion(), Kind: models.HTTP,
			Spec: models.MockSpec{
				Metadata:         map[string]string{"type": "mocks", models.MetadataLifetimeKey: models.MetadataLifetimePerTest},
				HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://dep/x", ProtoMajor: 1, ProtoMinor: 1},
				HTTPResp:         &models.HTTPResp{StatusCode: 200, StatusMessage: "OK", Body: "{}"},
				ReqTimestampMock: ts, ResTimestampMock: ts.Add(time.Millisecond),
			},
		}, "set"); err != nil {
			t.Fatal(err)
		}
	}
	if err := mapdb.New(zap.NewNop(), dir, "").UpsertBatch(ctx, "set", map[string][]models.MockEntry{
		"d1": mockEntries("mock-0"), "k1": mockEntries("mock-1", "mock-2"), "d2": mockEntries("mock-3", "mock-9"),
	}); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "set", "mocks.yaml"), filepath.Join(dir, "set", "mappings.yaml")
}

func duplicateDropRunIn() duplicateDropIn {
	consumed := map[string]models.MockState{}
	for _, n := range []string{"mock-0", "mock-1", "mock-2", "mock-3"} {
		consumed[n] = perTestMockState(n)
	}
	return duplicateDropIn{
		consumers: map[string][]string{"mock-0": {"d1"}, "mock-1": {"k1"}, "mock-2": {"k1"}, "mock-3": {"d2"}},
		consumed:  consumed,
		run:       &models.Mapping{TestCases: []models.MappedTestCase{{ID: "d1", Mocks: mockEntries("mock-0")}, {ID: "k1", Mocks: mockEntries("mock-1", "mock-2")}, {ID: "d2", Mocks: mockEntries("mock-3")}}},
	}
}

// dropDuplicates deletes d1's own mock and line from the files; d2 keeps both,
// its line naming mock-9, which nothing deletes; the kept test keeps all.
func TestDropDuplicatesDeletesTheirOwnMocksAndLines(t *testing.T) {
	dir := t.TempDir()
	duplicateDropFixture(t, dir)
	r := &Replayer{logger: zap.NewNop(), hookImpl: duplicateNamerHooks{dups: []string{"d1", "d2"}},
		mockDB: mockdb.New(zap.NewNop(), dir, "mocks"), mappingDB: mapdb.New(zap.NewNop(), dir, "")}
	held := r.dropDuplicates(context.Background(), "set", duplicateDropRunIn())
	if !reflect.DeepEqual(heldIDs(held), []string{"d1"}) {
		t.Fatalf("held out of the mapping write: %v, want [d1]", heldIDs(held))
	}
	got, _, err := mapdb.New(zap.NewNop(), dir, "").Get(context.Background(), "set")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for id := range got {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if !reflect.DeepEqual(ids, []string{"d2", "k1"}) {
		t.Fatalf("mapping entries %v, want d2 and k1", ids)
	}
	pool, err := mockdb.New(zap.NewNop(), dir, "mocks").GetTestSetMocks(context.Background(), "set", models.BaseTime, time.Now(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ms := range [][]*models.Mock{pool.Filtered, pool.Unfiltered, pool.AllPerTest, pool.AllSession} {
		for _, m := range ms {
			names = append(names, m.Name)
		}
	}
	sort.Strings(names)
	names = compactSortedNames(names)
	if !reflect.DeepEqual(names, []string{"mock-1", "mock-2", "mock-3"}) {
		t.Fatalf("mocks left %v, want mock-1, mock-2 (k1's) and mock-3 (d2's, its line stays)", names)
	}
}

func compactSortedNames(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// No DuplicateNamer, or none named: both files stay as they are, byte for
// byte, and nothing is held out of the mapping write.
func TestDropDuplicatesWithoutDuplicatesChangesNothing(t *testing.T) {
	for name, hooks := range map[string]TestHooks{"no namer": noNamerHooks{}, "no duplicates": duplicateNamerHooks{}} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			mocksFile, mappingFile := duplicateDropFixture(t, dir)
			beforeMocks, _ := os.ReadFile(mocksFile)
			beforeMapping, _ := os.ReadFile(mappingFile)
			r := &Replayer{logger: zap.NewNop(), hookImpl: hooks,
				mockDB: mockdb.New(zap.NewNop(), dir, "mocks"), mappingDB: mapdb.New(zap.NewNop(), dir, "")}
			if held := r.dropDuplicates(context.Background(), "set", duplicateDropRunIn()); held != nil {
				t.Fatalf("held %v, want nothing", held)
			}
			afterMocks, _ := os.ReadFile(mocksFile)
			afterMapping, _ := os.ReadFile(mappingFile)
			if !bytes.Equal(beforeMocks, afterMocks) || !bytes.Equal(beforeMapping, afterMapping) {
				t.Fatal("a run with no duplicates changed the mock or mapping file")
			}
		})
	}
}
