package mapdb

import (
	"context"
	"sort"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A set keploy mock record wrote under the proven switch carries each test's
// claim key (MappedTestCase.Key). Every writer of mappings.yaml run with the
// switch off keeps those keys, in YAML and JSON: a rewrite that dropped them
// would leave a later move out of the test no key to give the moved call,
// and keploy mock replay would serve that call to no test process. A call
// moved out of a keyed test with the switch off joins worker: under that
// test's key, as with the switch on.
func TestEachWriterKeepsEachTestsKeyWithoutTheSwitch(t *testing.T) {
	ctx := context.Background()
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
				must(t, db.Insert(ctx, &models.Mapping{TestSetID: "s", TestCases: []models.MappedTestCase{
					{ID: "o::TestA", Key: "pkg/a", Mocks: []models.MockEntry{{Name: "mock-0", Kind: "Http"}}},
					{ID: "o::TestB", Key: "pkg/b", Mocks: []models.MockEntry{{Name: "mock-1", Kind: "Http"}}},
				}}))
				t.Setenv(provenswitch.Env, "")
				must(t, writers[name](db))
				t.Setenv(provenswitch.Env, "1")
				got, err := db.GetMapping(ctx, "s")
				must(t, err)
				keys := map[string]string{}
				for _, tc := range got.TestCases {
					keys[tc.ID] = tc.Key
				}
				for id, want := range map[string]string{"o::TestA": "pkg/a", "o::TestB": "pkg/b"} {
					if keys[id] != want {
						t.Errorf("after %s with the switch off, test %s has the key %q; want %q (tests: %v)", name, id, keys[id], want, keys)
					}
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

// Insert with the switch off gives each test the key the caller wrote for
// it, else the one the file held: a test written again with no key keeps
// the file's, one written with a key takes it, and a new test takes its
// own.
func TestInsertWithoutTheSwitchKeepsATestsKeyUnlessItWritesOne(t *testing.T) {
	ctx := context.Background()
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(format.FileExtension(), func(t *testing.T) {
			db := NewWithFormat(zap.NewNop(), t.TempDir(), "mappings", format)
			t.Setenv(provenswitch.Env, "1")
			must(t, db.Insert(ctx, &models.Mapping{TestSetID: "s", TestCases: []models.MappedTestCase{
				{ID: "o::TestA", Key: "pkg/a", Mocks: []models.MockEntry{{Name: "mock-0", Kind: "Http"}}},
				{ID: "o::TestB", Key: "pkg/b", Mocks: []models.MockEntry{{Name: "mock-1", Kind: "Http"}}},
			}}))
			t.Setenv(provenswitch.Env, "")
			must(t, db.Insert(ctx, &models.Mapping{TestSetID: "s", TestCases: []models.MappedTestCase{
				{ID: "o::TestA", Mocks: []models.MockEntry{{Name: "mock-2", Kind: "Http"}}},
				{ID: "o::TestB", Key: "pkg/b2", Mocks: []models.MockEntry{{Name: "mock-3", Kind: "Http"}}},
				{ID: "o::TestC", Key: "pkg/c", Mocks: []models.MockEntry{{Name: "mock-4", Kind: "Http"}}},
			}}))
			got, err := db.GetMapping(ctx, "s")
			must(t, err)
			keys := map[string]string{}
			for _, tc := range got.TestCases {
				keys[tc.ID] = tc.Key
			}
			want := map[string]string{"o::TestA": "pkg/a", "o::TestB": "pkg/b2", "o::TestC": "pkg/c"}
			for id, w := range want {
				if keys[id] != w {
					t.Errorf("test %s has the key %q; want %q (tests: %v)", id, keys[id], w, keys)
				}
			}
		})
	}
}
