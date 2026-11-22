package mapdb

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A mock taken out of a test's list joins the worker scope under that test's
// claim key (MappedTestCase.Key): a replay serves a worker entry only while
// a test process of its key holds the run, so a moved call that lost its key
// sat in the slice of no test process and was never served. The test's key
// wins over a key the entry carries; of several tests whose lists held the
// mock, the first gives it, even when the first has none. A test with no key
// leaves the entry's own key, and an entry the scope lists already keeps its
// own when it is placed again. All of it holds with or without the proven
// switch: a test's key is read and kept whatever it says. A move that names
// a key gives the entry that key, moved or placed again, as a move that
// names an owner does.
func TestMoveToWorkerGivesAMovedCallItsTestsKey(t *testing.T) {
	for _, on := range []bool{true, false} {
		sw, name := "", "switch=off"
		if on {
			sw, name = "1", "switch=on"
		}
		for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
			t.Run(name+"/"+format.FileExtension(), func(t *testing.T) {
				t.Setenv(provenswitch.Env, sw)
				ctx := context.Background()
				db := NewWithFormat(zap.NewNop(), t.TempDir(), "mappings", format)
				m := goldenMapping()
				m.TestCases[1].Key = "pkg/b" // post-url-1 lists mock-6 and mock-7
				entry := func(name, key string) models.MockEntry {
					e := m.TestCases[0].Mocks[0]
					e.Name, e.Key = name, key
					return e
				}
				test := func(id, key string, mocks ...models.MockEntry) models.MappedTestCase {
					return models.MappedTestCase{ID: id, Key: key, Mocks: mocks}
				}
				m.TestCases = append(m.TestCases,
					test("post-url-9", "", entry("mock-9", "pkg/own")),          // no key; the entry has its own
					test("post-url-10", "pkg/x", entry("mock-10", "")),          // mock-10: listed by two keyed tests
					test("post-url-11", "pkg/y", entry("mock-10", "")),          // the second test listing mock-10
					test("post-url-12", "", entry("mock-11", "pkg/own11")),      // mock-11: the first test has no key,
					test("post-url-13", "pkg/z", entry("mock-11", "")),          // a later one has
					test("post-url-14", "pkg/v", entry("mock-12", "pkg/own12")), // a keyed test whose entry has its own key
				)
				m.Worker[0].Key = "pkg/w" // mock-2, placed again below
				if err := db.Insert(ctx, m); err != nil {
					t.Fatal(err)
				}
				if _, err := db.MoveToWorker(ctx, "test-set-0", []models.MockEntry{
					{Name: "mock-7"},               // out of post-url-1, keyed
					{Name: "mock-9"},               // out of post-url-9, which has no key
					{Name: "mock-10"},              // out of post-url-10 and post-url-11
					{Name: "mock-11"},              // out of post-url-12 and post-url-13
					{Name: "mock-12"},              // out of post-url-14
					{Name: "mock-2"},               // placed again
					{Name: "mock-6", Key: "pkg/a"}, // out of post-url-1, the move names a key
					{Name: "mock-5", Key: "pkg/c"}, // placed again, the move names a key
				}); err != nil {
					t.Fatal(err)
				}
				got, err := db.GetMapping(ctx, "test-set-0")
				if err != nil {
					t.Fatal(err)
				}
				keys := map[string]string{}
				for _, e := range got.Worker {
					keys[e.Name] = e.Key
				}
				want := map[string]string{
					"mock-9": "pkg/own", "mock-11": "pkg/own11", "mock-2": "pkg/w",
					"mock-6": "pkg/a", "mock-5": "pkg/c",
					"mock-7": "pkg/b", "mock-10": "pkg/x", "mock-12": "pkg/v",
				}
				for name, w := range want {
					if v, ok := keys[name]; !ok || v != w {
						t.Errorf("worker entry %s has the key %q (listed %v); want %q", name, v, ok, w)
					}
				}
			})
		}
	}
}
