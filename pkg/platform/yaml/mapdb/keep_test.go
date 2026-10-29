package mapdb

import (
	"context"
	"fmt"
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

// A key keploy writes with a comment above it and one after its value
// keeps both: the comment after the value is not lost because the one
// above the key was kept. A comment after a flow list keploy writes as a
// block stays after its key, on its own test.
func TestACommentAboveAKeyAndOneAfterItsValueAreBothKept(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := writeSet(t, "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\n# the payments set\ntest_set_id: s # renamed from pay-old\ntests:\n    - id: o::TestA\n      mock_entries:\n        - name: mock-0\n      cases: [test-1] # the login case\n")
	core, logs := observer.New(zapcore.WarnLevel)
	must(t, NewWithFormat(zap.New(core), dir, "mappings", yaml.FormatYAML).UpsertBatch(context.Background(), "s", map[string][]models.MockEntry{"o::TestB": {{Name: "mock-1"}}}))
	got := readSet(t, dir)
	for _, want := range []string{"# the payments set\ntest_set_id: s # renamed from pay-old\n", "      cases: # the login case\n        - test-1\n    - id: o::TestB\n"} {
		if strings.Count(got, want) != 1 {
			t.Errorf("the rewrite did not keep %q once:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "# the login case"); n != 1 {
		t.Errorf("the rewrite wrote the flow list's comment %d times:\n%s", n, got)
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

// A mock the old file lists twice and the new file once keeps the first
// listing's hand edit on its one listing. The second's is reported dropped,
// named as the second listing, and is never moved onto the first; nothing
// panics.
func TestAMockListedOnceNowKeepsTheFirstListingsEdit(t *testing.T) {
	old := []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: s\ntests:\n    - id: o::TestA\n      mock_entries:\n        - name: mock-0\n          note: first call\n        - name: mock-0\n          note: second call\n")
	m := &models.Mapping{Version: "api.keploy.io/v1beta1", Kind: models.MappingKind, TestSetID: "s", TestCases: []models.MappedTestCase{{ID: "o::TestA", Mocks: []models.MockEntry{{Name: "mock-0"}}}}}
	data, unkept, err := EncodeOver(m, old, nil)
	must(t, err)
	var doc struct {
		Tests []struct {
			Entries []map[string]any `yaml:"mock_entries"`
		} `yaml:"tests"`
	}
	must(t, yamlLib.Unmarshal(data, &doc))
	if len(doc.Tests) != 1 || len(doc.Tests[0].Entries) != 1 || doc.Tests[0].Entries[0]["note"] != "first call" {
		t.Fatalf("the one listing did not keep the first listing's note alone:\n%s", data)
	}
	if want := []string{"tests[o::TestA].mock_entries[mock-0 #2].note"}; fmt.Sprint(unkept.Dropped) != fmt.Sprint(want) || len(unkept.Fields) != 0 {
		t.Fatalf("Dropped = %q, Fields = %q, want Dropped %q", unkept.Dropped, unkept.Fields, want)
	}
}

// A test entry in the older shape (mocks:) keeps the comments on its list
// when the rewrite writes the list as mock_entries:, a block list's and a
// flow list's, each on its own test, and keploy, having dropped none, says
// nothing.
func TestAnOlderShapedListKeepsItsComments(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := writeSet(t, "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: s\ntests:\n    - id: o::TestA\n      # the calls of the login\n      mocks: # recorded before the upgrade\n        - mock-0\n    - id: o::TestC\n      mocks: [mock-2] # the logout\n")
	core, logs := observer.New(zapcore.WarnLevel)
	must(t, NewWithFormat(zap.New(core), dir, "mappings", yaml.FormatYAML).UpsertBatch(context.Background(), "s", map[string][]models.MockEntry{"o::TestB": {{Name: "mock-1"}}}))
	got := readSet(t, dir)
	for _, want := range []string{"# the calls of the login\n      mock_entries: # recorded before the upgrade\n", "- id: o::TestC\n      mock_entries: # the logout\n"} {
		if strings.Count(got, want) != 1 {
			t.Errorf("the rewrite did not keep %q once:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "# the logout"); n != 1 {
		t.Errorf("the rewrite wrote the flow list's comment %d times:\n%s", n, got)
	}
	if n := len(logs.All()); n != 0 {
		t.Errorf("keploy kept every comment, yet logged %v", logs.All())
	}
}

// The comment a file ends with, after a blank line, stays at its end, and
// keploy, having dropped none, says nothing.
func TestTheCommentAFileEndsWithIsKept(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := writeSet(t, "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: s\ntests:\n    - id: o::TestA\n      mock_entries:\n        - name: mock-0\n\n# re-record after the API change\n")
	core, logs := observer.New(zapcore.WarnLevel)
	must(t, NewWithFormat(zap.New(core), dir, "mappings", yaml.FormatYAML).UpsertBatch(context.Background(), "s", map[string][]models.MockEntry{"o::TestB": {{Name: "mock-1"}}}))
	if got := readSet(t, dir); !strings.HasSuffix(got, "\n\n# re-record after the API change\n") || strings.Count(got, "# re-record") != 1 || !strings.Contains(got, "- name: mock-1") {
		t.Errorf("the rewrite did not end the file with its comment, once:\n%s", got)
	}
	if n := len(logs.All()); n != 0 {
		t.Errorf("keploy kept every comment, yet logged %v", logs.All())
	}
}

// A pair's comment after it stays on its pair, never on a later pair (the
// next key, another test). A key's comment over a scalar (dir:) is written
// after the scalar, and of a scalar pair with one comment after the key
// and one after the value, the value's is kept and the key's is counted
// dropped.
func TestAPairsCommentStaysOnItsPair(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := writeSet(t, "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: # renamed\n    s # was pay-old\ntests:\n    - id: o::TestA\n      mock_entries:\n        - name: mock-0\n      dir: # the service folder\n        o\n")
	core, logs := observer.New(zapcore.WarnLevel)
	must(t, NewWithFormat(zap.New(core), dir, "mappings", yaml.FormatYAML).UpsertBatch(context.Background(), "s", map[string][]models.MockEntry{"o::TestB": {{Name: "mock-1"}}}))
	got := readSet(t, dir)
	for _, want := range []string{"test_set_id: s # was pay-old\ntests:\n", "      dir: o # the service folder\n    - id: o::TestB\n"} {
		if strings.Count(got, want) != 1 {
			t.Errorf("the rewrite did not keep %q once:\n%s", want, got)
		}
	}
	if strings.Contains(got, "# renamed") || strings.Count(got, "# the service folder") != 1 {
		t.Errorf("a comment moved off its pair:\n%s", got)
	}
	if warned := logs.FilterMessageSnippet("dropped 1 comment line(s)").All(); len(warned) != 1 || len(logs.All()) != 1 {
		t.Errorf("keploy dropped one comment line and said %v", logs.All())
	}
}

// A comment after "mocks:" over a boot's list, when the rewrite writes the
// boot's list empty (mocks: [], a flow list), stays on that boot, after its
// list, never on the boot after it.
func TestACommentOverABootsEmptiedListStaysOnItsBoot(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := writeSet(t, "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: s\ntests: []\nboots:\n    - dir: o\n      key: k1\n      program: p1\n      mocks: # the warm-up calls\n        - name: mock-0\n    - dir: o\n      key: k2\n      program: p2\n      mocks: []\n")
	core, logs := observer.New(zapcore.WarnLevel)
	must(t, NewWithFormat(zap.New(core), dir, "mappings", yaml.FormatYAML).UpsertBoots(context.Background(), "s", []models.BootSpec{{Dir: "o", Key: "k1", Program: "p1"}, {Dir: "o", Key: "k2", Program: "p2"}}))
	got := readSet(t, dir)
	if want := "      mocks: [] # the warm-up calls\n    - dir: o\n      key: k2\n"; strings.Count(got, want) != 1 || strings.Count(got, "# the warm-up calls") != 1 {
		t.Errorf("the rewrite did not keep %q on its boot, once:\n%s", want, got)
	}
	if n := len(logs.All()); n != 0 {
		t.Errorf("keploy kept every comment, yet logged %v", logs.All())
	}
}

// A comment after a boot's empty list (mocks: [], which keploy writes as a
// flow list) stays on that boot, never on the boot after it. (This pins
// that a flow list keeps the comment after it on itself.)
func TestACommentAfterABootsEmptyListStaysOnItsBoot(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := writeSet(t, "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: s\ntests: []\nboots:\n    - dir: o\n      key: k1\n      program: p1\n      mocks: [] # no calls at boot\n    - dir: o\n      key: k2\n      program: p2\n      mocks: []\n")
	core, logs := observer.New(zapcore.WarnLevel)
	must(t, NewWithFormat(zap.New(core), dir, "mappings", yaml.FormatYAML).UpsertBoots(context.Background(), "s", []models.BootSpec{{Dir: "o", Key: "k1", Program: "p1"}, {Dir: "o", Key: "k2", Program: "p2"}}))
	got := readSet(t, dir)
	if want := "      mocks: [] # no calls at boot\n    - dir: o\n      key: k2\n"; strings.Count(got, want) != 1 || strings.Count(got, "# no calls at boot") != 1 {
		t.Errorf("the rewrite did not keep %q on its boot, once:\n%s", want, got)
	}
	if n := len(logs.All()); n != 0 {
		t.Errorf("keploy kept every comment, yet logged %v", logs.All())
	}
}

// A test the rewrite removes, which lists a mock twice with a note on
// each listing, reports each note dropped by its own listing.
func TestARemovedTestsTwoListingsAreReportedApart(t *testing.T) {
	old := []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: s\ntests:\n    - id: o::TestA\n      mock_entries:\n        - name: mock-0\n    - id: o::TestX\n      mock_entries:\n        - name: mock-5\n          note: first call\n        - name: mock-5\n          note: second call\n")
	m := &models.Mapping{Version: "api.keploy.io/v1beta1", Kind: models.MappingKind, TestSetID: "s", TestCases: []models.MappedTestCase{{ID: "o::TestA", Mocks: []models.MockEntry{{Name: "mock-0"}}}}}
	_, unkept, err := EncodeOver(m, old, nil)
	must(t, err)
	if want := []string{"tests[o::TestX].mock_entries[mock-5].note", "tests[o::TestX].mock_entries[mock-5 #2].note"}; fmt.Sprint(unkept.Dropped) != fmt.Sprint(want) {
		t.Fatalf("Dropped = %q, want %q", unkept.Dropped, want)
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
