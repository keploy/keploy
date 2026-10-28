package mapdb

import (
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	yamlLib "gopkg.in/yaml.v3"
)

// A comment on a key keploy writes (beside kind:, above tests:) is kept,
// and keploy, having dropped none, says nothing.
func TestACommentOnAKeyKeployWritesIsKept(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := writeSet(t, "version: api.keploy.io/v1beta1\nkind: TestMocksMapping # keploy's kind\ntest_set_id: s\n# reviewed by the payments team\ntests:\n    - id: o::TestA\n      mock_entries:\n        - name: mock-0\n")
	core, logs := observer.New(zapcore.WarnLevel)
	must(t, NewWithFormat(zap.New(core), dir, "mappings", yaml.FormatYAML).UpsertBatch(context.Background(), "s", map[string][]models.MockEntry{"o::TestB": {{Name: "mock-1"}}}))
	got := readSet(t, dir)
	for _, want := range []string{"kind: TestMocksMapping # keploy's kind\n", "# reviewed by the payments team\ntests:\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("the rewrite lost %q:\n%s", want, got)
		}
	}
	if n := len(logs.All()); n != 0 {
		t.Errorf("keploy kept every comment, yet logged %v", logs.All())
	}
}

// A comment keploy cannot place again (on a worker entry a record replaced)
// is never lost without a word: keploy says how many lines it dropped.
func TestADroppedCommentIsCounted(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := writeSet(t, "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: s\ntests:\n    - id: o::TestA\n      mock_entries:\n        - name: mock-0\nworker:\n    # the poll the old client made\n    - name: mock-9\n")
	core, logs := observer.New(zapcore.WarnLevel)
	must(t, NewWithFormat(zap.New(core), dir, "mappings", yaml.FormatYAML).UpsertWorker(context.Background(), "s", []models.MockEntry{{Name: "mock-10"}}))
	if strings.Contains(readSet(t, dir), "the poll the old client made") {
		t.Fatalf("the comment of the replaced entry is still there:\n%s", readSet(t, dir))
	}
	warned := logs.FilterMessageSnippet("dropped 1 comment line(s)").All()
	if len(warned) != 1 {
		t.Fatalf("keploy dropped a comment line and said %v", logs.All())
	}
}

// A field a hand edit brings into a test entry through a YAML merge key
// (<<) is kept on the entry: the rewrite reads the entry as YAML resolves
// it.
func TestAFieldAMergeKeyBringsIsKept(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := writeSet(t, "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: s\nx-review: &r\n    ticket: JIRA-9\ntests:\n    - <<: *r\n      id: o::TestA\n      mock_entries:\n        - name: mock-0\n")
	must(t, NewWithFormat(zap.NewNop(), dir, "mappings", yaml.FormatYAML).UpsertBatch(context.Background(), "s", map[string][]models.MockEntry{"o::TestB": {{Name: "mock-1"}}}))
	var doc struct {
		Tests []map[string]any `yaml:"tests"`
	}
	got := readSet(t, dir)
	must(t, yamlLib.Unmarshal([]byte(got), &doc))
	for _, e := range doc.Tests {
		if e["id"] == "o::TestA" {
			if e["ticket"] != "JIRA-9" {
				t.Fatalf("TestA lost the field its merge key brought:\n%s", got)
			}
			return
		}
	}
	t.Fatalf("TestA is gone:\n%s", got)
}

// A test entry in the older shape (mocks:) is rewritten in the current one
// (mock_entries:) without its old list copied back beside it, which the
// next read would refuse as an entry with both lists.
func TestAnOlderEntryIsRewrittenWithOneMockList(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := writeSet(t, "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: s\ntests:\n    - id: o::TestA\n      ticket: JIRA-1\n      mocks:\n        - name: mock-0\n          kind: Http\n")
	db := NewWithFormat(zap.NewNop(), dir, "mappings", yaml.FormatYAML)
	must(t, db.UpsertBatch(context.Background(), "s", map[string][]models.MockEntry{"o::TestB": {{Name: "mock-1"}}}))
	got := readSet(t, dir)
	if strings.Contains(got, "\n      mocks:") || !strings.Contains(got, "ticket: JIRA-1") {
		t.Fatalf("the rewrite kept the older list beside the new one, or lost the hand edit:\n%s", got)
	}
	back, err := db.GetMapping(context.Background(), "s")
	must(t, err)
	if len(back.TestCases) != 2 || back.TestCases[0].ID != "o::TestA" || len(back.TestCases[0].Mocks) != 1 || back.TestCases[0].Mocks[0].Name != "mock-0" {
		t.Fatalf("read back %+v", back.TestCases)
	}
}

// A mock a test lists twice keeps each listing's hand edit on its own
// listing: the n-th old item of a name is the n-th new one, never the last.
func TestAMockListedTwiceKeepsEachListingsEdit(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := writeSet(t, "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: s\ntests:\n    - id: o::TestA\n      mock_entries:\n        - name: mock-0\n          note: first call\n        - name: mock-0\n          note: second call\n")
	must(t, NewWithFormat(zap.NewNop(), dir, "mappings", yaml.FormatYAML).UpsertBatch(context.Background(), "s", map[string][]models.MockEntry{"o::TestB": {{Name: "mock-1"}}}))
	var doc struct {
		Tests []struct {
			ID      string           `yaml:"id"`
			Entries []map[string]any `yaml:"mock_entries"`
		} `yaml:"tests"`
	}
	got := readSet(t, dir)
	must(t, yamlLib.Unmarshal([]byte(got), &doc))
	if len(doc.Tests) == 0 || doc.Tests[0].ID != "o::TestA" || len(doc.Tests[0].Entries) != 2 ||
		doc.Tests[0].Entries[0]["note"] != "first call" || doc.Tests[0].Entries[1]["note"] != "second call" {
		t.Fatalf("a listing lost its note, or took the other's:\n%s", got)
	}
}

// A boot is matched by its dir and key: two binaries of one directory keep
// each their own hand edit, and a rewrite that drops one never moves its
// edit onto the other.
func TestABootKeepsItsOwnEdit(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := writeSet(t, "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: s\ntests: []\nboots:\n    - dir: o\n      key: k1\n      program: p1\n      note: the first binary\n      mocks: []\n    - dir: o\n      key: k2\n      program: p2\n      note: the second binary\n      mocks: []\n")
	must(t, NewWithFormat(zap.NewNop(), dir, "mappings", yaml.FormatYAML).UpsertBoots(context.Background(), "s", []models.BootSpec{{Dir: "o", Key: "k2", Program: "p2"}}))
	var doc struct {
		Boots []map[string]any `yaml:"boots"`
	}
	got := readSet(t, dir)
	must(t, yamlLib.Unmarshal([]byte(got), &doc))
	if len(doc.Boots) != 1 || doc.Boots[0]["key"] != "k2" || doc.Boots[0]["note"] != "the second binary" {
		t.Fatalf("the kept boot lost its note, or took the dropped one's:\n%s", got)
	}
}
