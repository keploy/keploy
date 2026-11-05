package mapdb

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A set keploy mock record wrote under the proven switch carries, on each
// test entry, data a replay acts on under the switch only: the test's claim
// key, begin, mock.real destinations, given values and browser seed, beside
// its dir, cases, case mocks, case steps and starts, and the set's suites,
// boots and runner. mapdb keeps every field keploy writes on a set whatever
// the switch: every writer of mappings.yaml run with the switch off keeps
// each of them, in YAML and JSON, but for what it writes itself
// (UpsertBoots' boots). A rewrite
// that dropped one would replay the set wrong with the switch on, and say
// nothing (a lost begin runs the test at the start time; a lost key leaves a
// later move out of the test no key to give the moved call). A call moved
// out of a keyed test with the switch off joins worker: under that test's
// key, as with the switch on.
func TestEachWriterKeepsEachTestsFieldsWithoutTheSwitch(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	writers := fileWriters()
	names := make([]string, 0, len(writers))
	for name := range writers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		for _, name := range names {
			t.Run(format.FileExtension()+"/"+name, func(t *testing.T) {
				db := NewWithFormat(zap.NewNop(), t.TempDir(), "mappings", format)
				t.Setenv(provenswitch.Env, "1")
				must(t, db.Insert(ctx, &models.Mapping{TestSetID: "s",
					TestCases: []models.MappedTestCase{
						{ID: "o::TestA", Key: "pkg/a", At: at, Real: []string{"db:5432"}, Given: []models.GivenValue{{Name: "given:a", Value: "v"}}, Seed: "0123456789abcdef",
							Dir: "o", Cases: []string{"test-1"}, CaseMocks: map[string][]string{"test-1": {"mock-0"}}, CaseSteps: map[string]string{"test-1": "step"}, Starts: 2,
							Mocks: []models.MockEntry{{Name: "mock-0", Kind: "Http"}}},
						{ID: "o::TestB", Key: "pkg/b", Mocks: []models.MockEntry{{Name: "mock-1", Kind: "Http"}}},
					},
					Suites: []models.SuiteSpan{{Dir: "o", Start: at, End: at.Add(time.Minute)}},
					Boots:  []models.BootSpec{{Dir: "o", Key: "k0", Program: "p0"}},
					Runner: []models.MockEntry{{Name: "mock-9", Kind: "Http"}},
				}))
				before, err := db.GetMapping(ctx, "s")
				must(t, err)
				t.Setenv(provenswitch.Env, "")
				must(t, writers[name](db))
				t.Setenv(provenswitch.Env, "1")
				got, err := db.GetMapping(ctx, "s")
				must(t, err)
				b, a := testEntry(before, "o::TestA"), testEntry(got, "o::TestA")
				fields := []struct {
					field      string
					was, is    any
					writerSets bool
				}{
					{field: "key", was: b.Key, is: a.Key},
					{field: "at", was: b.At, is: a.At},
					{field: "real", was: b.Real, is: a.Real},
					{field: "given", was: b.Given, is: a.Given},
					{field: "seed", was: b.Seed, is: a.Seed},
					{field: "dir", was: b.Dir, is: a.Dir},
					{field: "cases", was: b.Cases, is: a.Cases},
					{field: "case_mocks", was: b.CaseMocks, is: a.CaseMocks},
					{field: "case_steps", was: b.CaseSteps, is: a.CaseSteps},
					{field: "starts", was: b.Starts, is: a.Starts},
					{field: "suites", was: before.Suites, is: got.Suites},
					{field: "boots", was: before.Boots, is: got.Boots, writerSets: name == "UpsertBoots"},
					{field: "runner", was: before.Runner, is: got.Runner},
					{field: "o::TestB's key", was: testEntry(before, "o::TestB").Key, is: testEntry(got, "o::TestB").Key},
				}
				// Each field the set holds before the writer runs, so a writer
				// that drops one cannot pass by finding it already empty.
				for _, f := range fields {
					if empty(f.was) {
						t.Fatalf("the set was not written whole under the switch: its %s is empty: %+v", f.field, before)
					}
				}
				for _, f := range fields {
					t.Run(f.field, func(t *testing.T) {
						if f.writerSets {
							t.Skipf("%s writes the set's %s", name, f.field)
						}
						if !reflect.DeepEqual(f.was, f.is) {
							t.Errorf("after %s with the switch off, %s is %v; it was %v", name, f.field, f.is, f.was)
						}
					})
				}
				if name != "MoveToWorker" {
					return
				}
				var moved *models.MockEntry
				for i := range got.Worker {
					if got.Worker[i].Name == "mock-0" {
						moved = &got.Worker[i]
					}
				}
				switch {
				case moved == nil:
					t.Errorf("mock-0 did not join worker: %+v", got.Worker)
				case moved.Key != "pkg/a":
					t.Errorf("mock-0, moved out of o::TestA with the switch off, joined worker: under the key %q; want the test's, %q", moved.Key, "pkg/a")
				}
			})
		}
	}
}

// empty says whether v, a field of a set, holds nothing: a zero value, or a
// slice, map or string of length 0.
func empty(v any) bool {
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Slice, reflect.Map, reflect.String:
		return r.Len() == 0
	default:
		return !r.IsValid() || r.IsZero()
	}
}

// testEntry is the entry of test id in m, or none.
func testEntry(m *models.Mapping, id string) models.MappedTestCase {
	for _, tc := range m.TestCases {
		if tc.ID == id {
			return tc
		}
	}
	return models.MappedTestCase{}
}

// Insert gives each test, whatever the switch, each field the caller wrote
// for it, else the file's: a test written again with no key and no givens
// keeps the file's, one written with a key and givens takes them, and a new
// test takes its own.
func TestInsertKeepsATestsFieldsUnlessItWritesThem(t *testing.T) {
	ctx := context.Background()
	g1 := []models.GivenValue{{Name: "given:a", Value: "1"}}
	g2 := []models.GivenValue{{Name: "given:a", Value: "2"}}
	for _, sw := range []string{"1", ""} {
		for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
			t.Run(format.FileExtension()+"/switch="+sw, func(t *testing.T) {
				db := NewWithFormat(zap.NewNop(), t.TempDir(), "mappings", format)
				t.Setenv(provenswitch.Env, "1")
				must(t, db.Insert(ctx, &models.Mapping{TestSetID: "s", TestCases: []models.MappedTestCase{
					{ID: "o::TestA", Key: "pkg/a", Given: g1, Mocks: []models.MockEntry{{Name: "mock-0", Kind: "Http"}}},
					{ID: "o::TestB", Key: "pkg/b", Given: g1, Mocks: []models.MockEntry{{Name: "mock-1", Kind: "Http"}}},
				}}))
				t.Setenv(provenswitch.Env, sw)
				must(t, db.Insert(ctx, &models.Mapping{TestSetID: "s", TestCases: []models.MappedTestCase{
					{ID: "o::TestA", Mocks: []models.MockEntry{{Name: "mock-2", Kind: "Http"}}},
					{ID: "o::TestB", Key: "pkg/b2", Given: g2, Mocks: []models.MockEntry{{Name: "mock-3", Kind: "Http"}}},
					{ID: "o::TestC", Key: "pkg/c", Given: g2, Mocks: []models.MockEntry{{Name: "mock-4", Kind: "Http"}}},
				}}))
				t.Setenv(provenswitch.Env, "1")
				got, err := db.GetMapping(ctx, "s")
				must(t, err)
				for id, w := range map[string]models.MappedTestCase{
					"o::TestA": {Key: "pkg/a", Given: g1},
					"o::TestB": {Key: "pkg/b2", Given: g2},
					"o::TestC": {Key: "pkg/c", Given: g2},
				} {
					e := testEntry(got, id)
					if e.Key != w.Key || !reflect.DeepEqual(e.Given, w.Given) {
						t.Errorf("test %s has the key %q and givens %v; want %q and %v", id, e.Key, e.Given, w.Key, w.Given)
					}
				}
			})
		}
	}
}
