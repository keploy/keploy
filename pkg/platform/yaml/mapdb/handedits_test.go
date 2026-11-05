package mapdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

// eachSwitch runs row with the proven switch off and on: a rewrite keeps a
// person's hand edits, and says what it could not keep, whatever the
// switch.
func eachSwitch(t *testing.T, row func(t *testing.T)) {
	t.Helper()
	for _, sw := range []string{"0", "1"} {
		t.Run("switch="+sw, func(t *testing.T) {
			t.Setenv(provenswitch.Env, sw)
			row(t)
		})
	}
}

// textDroppedComments is how many of the comment lines old holds written
// does not, each file parsed from its bytes: what keploy's count of the
// comment lines a rewrite dropped (keepEdits, from the nodes it parsed
// once) must say.
func textDroppedComments(old, written []byte) int {
	return uncounted(textComments(old), textComments(written))
}

// textComments is the comment lines the YAML document data holds.
func textComments(data []byte) []string {
	var doc yamlLib.Node
	if yamlLib.Unmarshal(data, &doc) != nil {
		return nil
	}
	return commentLines(&doc, nil)
}

// droppedSaid is the comment lines keploy said a rewrite dropped (0 when
// it said nothing), and fails t when it said it more than once.
func droppedSaid(t *testing.T, logs *observer.ObservedLogs) int {
	t.Helper()
	said := logs.FilterMessageSnippet("comment line(s)").All()
	if len(said) > 1 {
		t.Fatalf("keploy said it dropped comments %d times: %v", len(said), said)
	}
	if len(said) == 0 {
		return 0
	}
	m := regexp.MustCompile(`dropped (\d+) comment line`).FindStringSubmatch(said[0].Message)
	if m == nil {
		t.Fatalf("no count in %q", said[0].Message)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// keploysOwn is a file keploy writes with every field it writes, a boot's
// key holding a '#' as starts.KeyOf writes one (program#n@test).
var keploysOwn = strings.Replace(writerFixtures["full"], "key: k0\n", "key: p0#1@o::TestA\n", 1)

// A file keploy wrote holds no hand edit, so its rewrite keeps nothing of
// it and parses nothing more (holdsEdits): whatever the writer, a '#' in a
// value included. Each kind of hand edit is one.
func TestAFileKeployWroteHoldsNoHandEdit(t *testing.T) {
	holds := func(file string) bool {
		var doc yamlLib.Node
		must(t, yamlLib.Unmarshal([]byte(file), &doc))
		return holdsEdits(&doc, docRoot(&doc))
	}
	eachSwitch(t, func(t *testing.T) {
		for name, write := range fileWriters() {
			for _, fixture := range []string{keploysOwn, writerFixtures["plain"], writerFixtures["nohead"]} {
				dir := writeSet(t, fixture)
				must(t, write(NewWithFormat(zap.NewNop(), dir, "mappings", yaml.FormatYAML)))
				if got := readSet(t, dir); holds(got) {
					t.Fatalf("%s wrote a file holding a hand edit:\n%s", name, got)
				}
			}
		}
	})
	if holds(keploysOwn) || holds("# a person's note\n\n"+keploysOwn) || holds(strings.Replace(keploysOwn, "test_set_id: s\n", "test_set_id: &id s\n", 1)) {
		t.Fatalf("keploy's own file, one whose head holds a note, or one with an anchor no alias names holds a hand edit")
	}
	if !holds("{version: api.keploy.io/v1beta1, kind: TestMocksMapping, test_set_id: s} # set s\n") {
		t.Errorf("a comment after a flow mapping file is not seen as a hand edit")
	}
	for name, edit := range map[string][2]string{
		"a comment on a test":         {"    - id: o::TestA\n", "    # the login test\n    - id: o::TestA\n"},
		"a comment after a value":     {"test_set_id: s\n", "test_set_id: s # renamed\n"},
		"a comment the file ends":     {"    - probe\n", "    - probe\n\n# end\n"},
		"a field at the top":          {"source: keploy-test\n", "source: keploy-test\nteam: payments\n"},
		"a field on a test":           {"    - id: o::TestA\n", "    - id: o::TestA\n      owner_team: payments\n"},
		"a field on a mock entry":     {"        - name: mock-0\n", "        - name: mock-0\n          why: login\n"},
		"a field on a boot":           {"      program: p0\n", "      program: p0\n      note: the api\n"},
		"a field on a given value":    {"        - name: given:a\n", "        - name: given:a\n          note: chosen\n"},
		"a field on a group":          {"      unkept: 1\n", "      unkept: 1\n      why: shared db\n"},
		"a comment in a case list":    {"      cases:\n", "      cases:\n        # the login case\n"},
		"an anchor and its alias":     {"source: keploy-test\n", "source: &s keploy-test\nalso: *s\n"},
		"an alias at a known key":     {"source: keploy-test\n", "source: *id\n"},
		"a comment in a case's mocks": {"            - mock-0\n", "            - mock-0 # the login\n"},
		"a merge key":                 {"    - id: o::TestA\n", "    - <<: {team: payments}\n      id: o::TestA\n"},
		"a comment on the first key":  {"version: api.keploy.io/v1beta1\n", "version: api.keploy.io/v1beta1 # pinned\n"},
	} {
		if !strings.Contains(keploysOwn, edit[0]) {
			t.Fatalf("%s: the fixture has no %q", name, edit[0])
		}
		file := strings.Replace(keploysOwn, edit[0], edit[1], 1)
		if name == "an alias at a known key" {
			file = strings.Replace(file, "test_set_id: s\n", "test_set_id: &id s\n", 1)
		}
		if !holds(file) {
			t.Errorf("%s is not seen as a hand edit", name)
		}
	}
}

// keploy counts the comment lines a rewrite dropped from the nodes it
// parsed once, and the count is what the file lost: each writer, on a file
// whose comments sit at every level, says the number of lines the written
// file lacks, whatever the switch.
func TestTheDroppedCommentCountIsWhatTheFileLost(t *testing.T) {
	commented := strings.NewReplacer(
		"    - id: o::TestA\n", "    # the login test\n    - id: o::TestA\n",
		"        - name: mock-6\n", "        # the second call\n        - name: mock-6\n",
		"      program: p0\n", "      program: p0 # the api\n",
		"    - name: mock-5\n", "    # the poll\n    - name: mock-5\n",
		"    - name: mock-7\n", "    # shared by both\n    - name: mock-7\n",
		"      unkept: 1\n", "      unkept: 1 # one left\n",
		"    - name: mock-4\n", "    # the token\n    - name: mock-4\n",
		"    - probe\n", "    - probe\n\n# the end\n",
	).Replace(keploysOwn)
	// The head holds a line a dropped comment holds too: the head's is
	// kept, the other's is not.
	commented = "# the poll\n" + commented
	eachSwitch(t, func(t *testing.T) {
		total := 0
		for name, write := range fileWriters() {
			dir := writeSet(t, commented)
			core, logs := observer.New(zapcore.WarnLevel)
			must(t, write(NewWithFormat(zap.New(core), dir, "mappings", yaml.FormatYAML)))
			want := textDroppedComments([]byte(commented), []byte(readSet(t, dir)))
			if got := droppedSaid(t, logs); got != want {
				t.Errorf("%s: keploy said it dropped %d comment line(s), the file lost %d:\n%s", name, got, want, readSet(t, dir))
			}
			total += want
		}
		if total == 0 {
			t.Fatalf("no writer dropped a comment: the row checks no count")
		}
	})
}

// A field a person adds to a value a test gave (an item of its given:) is
// kept, with its comment, by every writer that keeps the test, whatever the
// switch; a rewrite that no longer has the value names the field.
func TestAGivenValueKeepsItsHandField(t *testing.T) {
	file := "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: s\ntests:\n    - id: o::TestA\n      mock_entries:\n        - name: mock-0\n      given:\n        - name: given:a\n          value: \"7\"\n          # picked by hand\n          note: chosen by the payments team\n        - name: given:a\n          value: \"8\"\n          note: the second one\n"
	eachSwitch(t, func(t *testing.T) {
		for name, write := range fileWriters() {
			dir := writeSet(t, file)
			core, logs := observer.New(zapcore.WarnLevel)
			must(t, write(NewWithFormat(zap.New(core), dir, "mappings", yaml.FormatYAML)))
			got := readSet(t, dir)
			for _, want := range []string{"value: \"7\"\n          # picked by hand\n          note: chosen by the payments team\n", "value: \"8\"\n          note: the second one\n"} {
				if !strings.Contains(got, want) {
					t.Errorf("%s lost %q:\n%s", name, want, got)
				}
			}
			if len(logs.All()) != 0 {
				t.Errorf("%s kept everything, yet said %v", name, logs.All())
			}
		}
	})
	m := &models.Mapping{Version: "api.keploy.io/v1beta1", Kind: models.MappingKind, TestSetID: "s", TestCases: []models.MappedTestCase{{ID: "o::TestA", Mocks: []models.MockEntry{{Name: "mock-0"}}, Given: []models.GivenValue{{Name: "given:a", Value: "7"}}}}}
	_, unkept, err := EncodeOver(m, []byte(file), nil)
	must(t, err)
	if want := []string{"tests[o::TestA].given[given:a #2].note"}; fmt.Sprint(unkept.Dropped) != fmt.Sprint(want) {
		t.Fatalf("Dropped = %q, want %q", unkept.Dropped, want)
	}
}

// A JSON mappings file keeps only the fields keploy writes: a rewrite
// drops a field a person added, at any level, and names each, whatever the
// switch. A file keploy wrote names none.
func TestAJSONRewriteNamesTheHandFieldsItDrops(t *testing.T) {
	file := `{"version": "api.keploy.io/v1beta1", "kind": "TestMocksMapping", "testSetId": "s", "team": "payments",
 "tests": [{"id": "o::TestA", "owner": "ana", "mock_entries": [{"name": "mock-0", "why": "login"}], "given": [{"name": "given:a", "value": "7", "note": "chosen"}]},
  {"id": "o::TestOld", "mocks": [{"name": "mock-9", "kind": "Http"}]}],
 "boots": [{"dir": "o", "key": "p#1@o::TestA", "program": "p", "mocks": [], "note": "api"}]}`
	eachSwitch(t, func(t *testing.T) {
		dir := t.TempDir()
		must(t, os.MkdirAll(filepath.Join(dir, "s"), 0o755))
		must(t, os.WriteFile(filepath.Join(dir, "s", "mappings.json"), []byte(file), 0o644))
		core, logs := observer.New(zapcore.WarnLevel)
		db := NewWithFormat(zap.New(core), dir, "mappings", yaml.FormatJSON)
		must(t, db.UpsertBatch(context.Background(), "s", map[string][]models.MockEntry{"o::TestB": {{Name: "mock-1"}}}))
		var said []string
		for _, e := range logs.FilterMessageSnippet("a JSON mappings file").All() {
			for _, f := range e.Context {
				if f.Key == "fields" {
					said = append(said, fmt.Sprint(f.Interface))
				}
			}
		}
		want := "[[boots[o p#1@o::TestA].note team tests[o::TestA].given[given:a].note tests[o::TestA].mock_entries[mock-0].why tests[o::TestA].owner]]"
		if fmt.Sprint(said) != want {
			t.Fatalf("keploy named %v, want %s; logs %v", said, want, logs.All())
		}
		core, logs = observer.New(zapcore.WarnLevel)
		db = NewWithFormat(zap.New(core), dir, "mappings", yaml.FormatJSON)
		must(t, db.UpsertBatch(context.Background(), "s", map[string][]models.MockEntry{"o::TestC": {{Name: "mock-2"}}}))
		if len(logs.All()) != 0 {
			t.Fatalf("keploy's own JSON file holds no hand field, yet it said %v", logs.All())
		}
	})
}
