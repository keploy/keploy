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
	"go.keploy.io/server/v3/utils"
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
		takeFileHead(docRoot(&doc), []byte(file))
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
	if !holds("{\n  # a note\n  version: api.keploy.io/v1beta1,\n  kind: TestMocksMapping,\n  test_set_id: s\n}\n") {
		t.Errorf("a comment on the first key of a flow mapping file is not seen as a hand edit")
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

// A rewrite of a file no person edited parses nothing past the file it
// replaces, which the writer parsed already: keepEdits hands the new
// encoding back as it is, never parsed (holdsEdits), so a record batch
// costs what it cost before keploy kept hand edits.
func TestARewriteOfAnUneditedFileParsesNothingMore(t *testing.T) {
	data := []byte(strings.TrimPrefix(keploysOwn, "# Generated by Keploy (0.0.1)\n"))
	for name, old := range map[string][]byte{"keploy's line": []byte(keploysOwn), "no head": data} {
		var od yamlLib.Node
		must(t, yamlLib.Unmarshal(old, &od))
		first := docRoot(&od).Content[0]
		head := first.HeadComment
		var got []byte
		var unkept Unkept
		allocs := testing.AllocsPerRun(20, func() {
			first.HeadComment = head // keepEdits takes it off the key
			got, unkept = keepEdits(old, &od, data, nil)
		})
		if string(got) != string(data) || unkept.Comments != 0 || unkept.Fields != nil || unkept.Dropped != nil {
			t.Fatalf("%s: keepEdits changed a file no person edited (unkept %+v):\n%s", name, unkept, got)
		}
		if allocs > 0 {
			t.Errorf("%s: keepEdits made %v allocations on a file no person edited: it read the file again", name, allocs)
		}
	}
}

// A rewrite that keeps none of a file's hand edits (the test a person
// noted is gone) writes keploy's encoding as it is, never parsed and
// written again: a value yaml.v3 does not write back byte for byte (a lone
// newline) is written as a file no person edited holds it.
func TestARewriteThatKeepsNoEditWritesKeploysEncoding(t *testing.T) {
	eachSwitch(t, func(t *testing.T) {
		mapping := &models.Mapping{Version: "api.keploy.io/v1beta1", Kind: "TestMocksMapping", TestSetID: "s",
			TestCases: []models.MappedTestCase{{ID: "o::TestA", Given: []models.GivenValue{{Name: "given:a", Value: "\n"}}}}}
		old := []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: s\ntests:\n    # gone\n    - id: o::TestB\n")
		got, unkept, err := EncodeOver(mapping, old, nil)
		must(t, err)
		data, err := yaml.MarshalGeneric(yaml.FormatYAML, mapping)
		must(t, err)
		if want := utils.GetVersionAsComment() + string(data); string(got) != want || unkept.Comments != 1 {
			t.Fatalf("a rewrite that kept no edit wrote (%d comment line(s) dropped, want 1):\n%s\nwant keploy's encoding:\n%s", unkept.Comments, got, want)
		}
	})
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
	// The head holds each line a comment past it holds too, on its first
	// key (no blank line after it): whichever comment a writer drops, the
	// head's twin is kept and the dropped one is counted.
	commented = "# the login test\n# the second call\n# the api\n# the poll\n# shared by both\n# one left\n# the token\n# the end\n" + commented
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

// A writer outside this package (enterprise's judged mark) rewrites the
// file with EncodeOverF as keploy's own writers do, whatever the switch: a
// YAML file keeps a person's comment and field, and its head when that
// holds a person's comment, keploy's line brought up to this keploy, and
// says the comment it could not place; a head of keploy's line alone is
// left out, as MarkRecordedWith leaves it. A JSON file names the field it
// drops. A file no person edited says nothing.
func TestEncodeOverFWritesAndSaysAsKeploysWriters(t *testing.T) {
	edited := strings.NewReplacer(
		"    - id: o::TestA\n", "    # the login test\n    - id: o::TestA\n      owner_team: payments\n",
		"    - id: o::TestB\n", "    # gone\n    - id: o::TestB\n",
	).Replace(keploysOwn)
	eachSwitch(t, func(t *testing.T) {
		var mapping models.Mapping
		must(t, yamlLib.Unmarshal([]byte(edited), &mapping))
		mapping.TestCases = mapping.TestCases[:1]
		got, err := EncodeOverF(zap.NewNop(), &mapping, yaml.FormatYAML, []byte(edited), "s")
		must(t, err)
		if !strings.HasPrefix(string(got), "version: ") {
			t.Fatalf("EncodeOverF kept a head of keploy's line alone:\n%s", got)
		}
		core, logs := observer.New(zapcore.WarnLevel)
		got, err = EncodeOverF(zap.New(core), &mapping, yaml.FormatYAML, []byte("# a person's note\n"+edited), "s")
		must(t, err)
		if !strings.HasPrefix(string(got), "# a person's note\n"+utils.GetVersionAsComment()+"version: ") || !strings.Contains(string(got), "    # the login test\n    - id: o::TestA\n      owner_team: payments\n") || strings.Contains(string(got), "- id: o::TestB") {
			t.Fatalf("EncodeOverF did not keep the hand edits of the test it keeps:\n%s", got)
		}
		if n := droppedSaid(t, logs); n != 1 {
			t.Fatalf("EncodeOverF said it dropped %d comment line(s), want 1 (# gone): %v", n, logs.All())
		}
		core, logs = observer.New(zapcore.WarnLevel)
		_, err = EncodeOverF(zap.New(core), &mapping, yaml.FormatJSON, []byte(`{"version": "api.keploy.io/v1beta1", "testSetId": "s", "team": "payments"}`), "s")
		must(t, err)
		said := logs.FilterMessageSnippet("a JSON mappings file").All()
		if len(said) != 1 || fmt.Sprint(said[0].ContextMap()["fields"]) != "[team]" {
			t.Fatalf("EncodeOverF did not name the JSON field it dropped: %v", logs.All())
		}
		core, logs = observer.New(zapcore.WarnLevel)
		_, err = EncodeOverF(zap.New(core), &mapping, yaml.FormatYAML, []byte(keploysOwn), "s")
		must(t, err)
		if len(logs.All()) != 0 {
			t.Fatalf("a file no person edited, yet EncodeOverF said %v", logs.All())
		}
	})
}

// A comment on an item of a plain list keploy writes (recordedWith, a test's
// cases) stays on its item through every writer, whatever the switch: the
// n-th old item of a value gives its comments to the n-th new one. An item
// the rewrite no longer holds takes its comment with it, and the count says
// so. Mutation: keep no item's comment; match items by place, not value.
func TestAPlainListItemKeepsItsComment(t *testing.T) {
	file := "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: s\ntests:\n    - id: o::TestA\n      mock_entries:\n        - name: mock-0\n      cases:\n        - case-1 # the slow one\n        - case-2\nrecordedWith:\n    # routed by the agent\n    - s12-routing\n    - other # kept by hand\n"
	eachSwitch(t, func(t *testing.T) {
		for name, write := range fileWriters() {
			dir := writeSet(t, file)
			core, logs := observer.New(zapcore.WarnLevel)
			must(t, write(NewWithFormat(zap.New(core), dir, "mappings", yaml.FormatYAML)))
			got := readSet(t, dir)
			for _, want := range []string{"- case-1 # the slow one\n", "    # routed by the agent\n    - s12-routing\n", "- other # kept by hand\n"} {
				if !strings.Contains(got, want) {
					t.Errorf("%s lost %q:\n%s", name, want, got)
				}
			}
			if len(logs.All()) != 0 {
				t.Errorf("%s kept everything, yet said %v", name, logs.All())
			}
		}
		m := &models.Mapping{Version: "api.keploy.io/v1beta1", Kind: models.MappingKind, TestSetID: "s", RecordedWith: []string{"other", "s12-routing", "other"}}
		old := "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: s\nrecordedWith:\n    - s12-routing # routed by the agent\n    - other # the first\n    - other # the second\n"
		got, unkept, err := EncodeOver(m, []byte(old), nil)
		must(t, err)
		if want := "    - other # the first\n    - s12-routing # routed by the agent\n    - other # the second\n"; !strings.Contains(string(got), want) {
			t.Errorf("the items' comments did not follow their values, want %q in:\n%s", want, got)
		}
		if unkept.Comments != 0 {
			t.Errorf("Comments = %d with every item kept, want 0", unkept.Comments)
		}
		m.RecordedWith = []string{"other"}
		got, unkept, err = EncodeOver(m, []byte(old), nil)
		must(t, err)
		if unkept.Comments != 2 || !strings.Contains(string(got), "- other # the first\n") {
			t.Errorf("Comments = %d after two commented items went, want 2; file:\n%s", unkept.Comments, got)
		}
	})
}

// caseMapsFile holds a comment at every level of a test's case_mocks and
// case_steps: on each map, on a case, on an item of a case's list, after
// the list, and after a step. Its cases are written out of the encoding's
// order: a comment stays on its case wherever the encoding puts the case.
const caseMapsFile = `version: api.keploy.io/v1beta1
kind: TestMocksMapping
test_set_id: s
tests:
    - id: o::TestA
      mock_entries:
        - name: mock-0
          kind: Http
        - name: mock-1
          kind: Http
      cases:
        - case-1 # the case list's own
        - case-2
      # the map of cases
      case_mocks: # after the key
        # the fast case
        case-2:
            - mock-1
        # the slow case
        case-1: # after the case
            # before the mock
            - mock-0 # the login call
            - mock-1 # the first read
            - mock-1 # the second read
            # after the mocks
      case_steps: # after the steps key
        case-2: step-b
        # first step
        case-1: step-a # after the step
`

// Every level of a test's case_mocks and case_steps keeps its comments
// through every writer, whatever the switch: nothing is dropped, and keploy
// says nothing. Mutation: keep no comment under a plain map; no comment on
// its keys; none on the lists under it.
func TestCaseMapsKeepTheirComments(t *testing.T) {
	eachSwitch(t, func(t *testing.T) {
		for name, write := range fileWriters() {
			dir := writeSet(t, caseMapsFile)
			core, logs := observer.New(zapcore.WarnLevel)
			must(t, write(NewWithFormat(zap.New(core), dir, "mappings", yaml.FormatYAML)))
			got := readSet(t, dir)
			for _, want := range []string{
				"        - case-1 # the case list's own\n",
				"      # the map of cases\n      case_mocks: # after the key\n",
				"        # the slow case\n        case-1: # after the case\n            # before the mock\n            - mock-0 # the login call\n            - mock-1 # the first read\n            - mock-1 # the second read\n            # after the mocks\n",
				"        # the fast case\n        case-2:\n            - mock-1\n",
				"      case_steps: # after the steps key\n",
				"        # first step\n        case-1: step-a # after the step\n",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("%s lost %q:\n%s", name, want, got)
				}
			}
			if n := textDroppedComments([]byte(caseMapsFile), []byte(got)); n != 0 || len(logs.All()) != 0 {
				t.Errorf("%s dropped %d comment line(s) and said %v:\n%s", name, n, logs.All(), got)
			}
		}
	})
}

// A case the rewrite no longer holds takes its comments with it, and the
// count says so; a comment never moves to another case or another list. A
// case's list names its mocks: a comment follows its mock through a rename,
// and stays off a call that holds the name now (EncodeOver's renames), as a
// note on a mock entry does. Mutation: match a case's mocks by their old
// names; count a mock renames lacks.
func TestACaseMapCommentStaysOnItsOwnCaseAndMock(t *testing.T) {
	eachSwitch(t, func(t *testing.T) {
		decode := func() *models.Mapping {
			var m models.Mapping
			must(t, yamlLib.Unmarshal([]byte(caseMapsFile), &m))
			return &m
		}
		m := decode()
		delete(m.TestCases[0].CaseMocks, "case-1")
		delete(m.TestCases[0].CaseSteps, "case-1")
		got, unkept, err := EncodeOver(m, []byte(caseMapsFile), nil)
		must(t, err)
		if n := textDroppedComments([]byte(caseMapsFile), got); unkept.Comments != n || n != 9 {
			t.Errorf("Comments = %d, %d gone, want the 9 of the gone case:\n%s", unkept.Comments, n, got)
		}
		if !strings.Contains(string(got), "        # the fast case\n        case-2:\n") {
			t.Errorf("the case that stayed lost its comment:\n%s", got)
		}

		m = decode()
		delete(m.TestCases[0].CaseMocks, "case-2")
		got, unkept, err = EncodeOver(m, []byte(caseMapsFile), nil)
		must(t, err)
		if want := "        # the slow case\n        case-1: # after the case\n"; !strings.Contains(string(got), want) || unkept.Comments != 1 {
			t.Errorf("Comments = %d, want the 1 of the first case, gone; the case after it lost %q:\n%s", unkept.Comments, want, got)
		}

		m = decode()
		m.TestCases[0].CaseMocks["case-1"] = []string{"mock-0", "mock-3", "mock-3"}
		m.TestCases[0].CaseMocks["case-2"] = []string{"mock-3"}
		got, unkept, err = EncodeOver(m, []byte(caseMapsFile), map[string]string{"mock-0": "mock-0", "mock-1": "mock-3"})
		must(t, err)
		if want := "            - mock-0 # the login call\n            - mock-3 # the first read\n            - mock-3 # the second read\n"; !strings.Contains(string(got), want) || unkept.Comments != 0 {
			t.Errorf("Comments = %d; the comments did not follow the renamed mock, want %q in:\n%s", unkept.Comments, want, got)
		}
		if want := "        - case-1 # the case list's own\n"; !strings.Contains(string(got), want) {
			t.Errorf("renames name mocks, and a case is none: want %q in:\n%s", want, got)
		}

		m = decode()
		got, unkept, err = EncodeOver(m, []byte(caseMapsFile), map[string]string{"mock-0": "mock-0"})
		must(t, err)
		if strings.Contains(string(got), "the first read") || strings.Contains(string(got), "the second read") {
			t.Errorf("a comment moved to the call that holds mock-1 now:\n%s", got)
		}
		if !strings.Contains(string(got), "- mock-0 # the login call\n") || unkept.Comments != 3 {
			t.Errorf("Comments = %d, want the 3 on the mock not carried:\n%s", unkept.Comments, got)
		}
	})
}

// A value or a whole file a person wrote in another shape than keploy writes
// holds nothing a comment can stay on: the comment is counted, and never lands
// on an item or a key of keploy's value. Mutation: walk old and new of
// different kinds; read a file that holds no mapping as one with no edits.
func TestACommentOnAValueOfAnotherShapeIsCounted(t *testing.T) {
	const test = "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: s\ntests:\n    - id: o::TestA\n"
	eachSwitch(t, func(t *testing.T) {
		m := &models.Mapping{Version: "api.keploy.io/v1beta1", Kind: models.MappingKind, TestSetID: "s", TestCases: []models.MappedTestCase{{
			ID: "o::TestA", Cases: []string{"case-1", "case-2"}, CaseMocks: map[string][]string{"case-1": {"mock-0"}},
		}}}
		for name, old := range map[string]string{
			"a map where keploy writes a list":  test + "      cases:\n        case-1: a # a note\n        case-2: b\n",
			"a list where keploy writes a map":  test + "      case_mocks:\n        - case-1 # a note\n",
			"a list where keploy writes a file": "- a # a note\n- b\n",
		} {
			got, unkept, err := EncodeOver(m, []byte(old), nil)
			must(t, err)
			if strings.Contains(string(got), "a note") || unkept.Comments != 1 {
				t.Errorf("%s: Comments = %d, want the note counted and not placed:\n%s", name, unkept.Comments, got)
			}
		}
	})
}

// Each line of the file keploy writes with every field it writes keeps a
// comment above it and one after it through every writer, whatever the
// switch: no key or item of the file is a place a comment is lost. A line
// the rewrite removes takes its comments with it, and keploy says how many.
// A field keploy adds to the file with a value of a new shape fails here
// until a comment on it stays.
func TestEveryLineOfKeploysFileKeepsAComment(t *testing.T) {
	var above, after, both strings.Builder
	for i, line := range strings.Split(strings.TrimSuffix(writerFixtures["full"], "\n"), "\n") {
		if i == 0 {
			continue // keploy's line
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
		fmt.Fprintf(&above, "%s# above %d\n%s\n", indent, i, line)
		fmt.Fprintf(&after, "%s # after %d\n", line, i)
		fmt.Fprintf(&both, "%s# above %d\n%s # after %d\n", indent, i, line, i)
	}
	removes := map[string]int{
		"ApplyShares":       1,  // the mock_entries: of the test it took its one mock from
		"UpsertBoots":       18, // startup: and its entry, and the boot it replaced
		"UpsertConcurrency": 8,  // the group it replaced, and the three fields its shared entry lacks
		"UpsertWorker":      15, // the worker entry it replaced
	}
	eachSwitch(t, func(t *testing.T) {
		for _, row := range []struct {
			shape, file string
			perLine     int
		}{{"above", above.String(), 1}, {"after", after.String(), 1}, {"both", both.String(), 2}} {
			for name, write := range fileWriters() {
				dir := writeSet(t, row.file)
				core, logs := observer.New(zapcore.WarnLevel)
				must(t, write(NewWithFormat(zap.New(core), dir, "mappings", yaml.FormatYAML)))
				got := readSet(t, dir)
				if n := textDroppedComments([]byte(row.file), []byte(got)); n != removes[name]*row.perLine || droppedSaid(t, logs) != n {
					t.Errorf("%s lost %d comment line(s) of the file with a comment %s each line, want %d, and said %d:\n%s", name, n, row.shape, removes[name]*row.perLine, droppedSaid(t, logs), got)
				}
			}
		}
	})
}

// The comment a file starts with is kept whatever YAML hangs it on: lines on
// both sides of a document marker or a directive are the file's head, in
// their order, and a comment above the first key of a file written as {...}
// is that key's: it is kept, and it is a hand edit, so a file holding nothing
// else is not taken for keploy's own, keploy's line beside a note there
// included. Mutation: stop the head at a marker; take every comment on the
// first key for the head; take one that starts with keploy's line for it.
func TestACommentAroundTheFirstKeyIsKept(t *testing.T) {
	const body = "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: s\n"
	const flow = "{\n  # a note\n  version: api.keploy.io/v1beta1,\n  kind: TestMocksMapping,\n  test_set_id: s\n}\n"
	eachSwitch(t, func(t *testing.T) {
		m := &models.Mapping{Version: "api.keploy.io/v1beta1", Kind: models.MappingKind, TestSetID: "s"}
		for name, row := range map[string]struct{ old, want string }{
			"both sides of a marker":        {"# one\n---\n# two\n" + body, "# one\n# two\nversion: "},
			"after a marker":                {"---\n# two\n" + body, "# two\nversion: "},
			"around a directive":            {"# one\n%YAML 1.1\n---\n# two\n" + body, "# one\n# two\nversion: "},
			"marker and a comment beyond":   {"# one\n---\n# two\n" + strings.Replace(body, "kind: TestMocksMapping\n", "kind: TestMocksMapping # k\n", 1), "# one\n# two\nversion: "},
			"on the marker line":            {"--- # a note\n" + body, utils.GetVersionAsComment() + "# a note\nversion: "},
			"a {...} file":                  {flow, utils.GetVersionAsComment() + "# a note\nversion: "},
			"a {...} file under a head":     {"# one\n" + flow, "# one\n# a note\nversion: "},
			"keploy's line in a {...} file": {strings.Replace(flow, "  # a note\n", "  "+utils.GetVersionAsComment()+"  # a note\n", 1), utils.GetVersionAsComment() + utils.GetVersionAsComment() + "# a note\nversion: "},
		} {
			got, unkept, err := EncodeOver(m, []byte(row.old), nil)
			must(t, err)
			if !strings.HasPrefix(string(got), row.want) {
				t.Errorf("%s: want the file to start %q:\n%s", name, row.want, got)
			}
			if n := textDroppedComments([]byte(row.old), got); n != 0 || unkept.Comments != 0 {
				t.Errorf("%s: %d comment line(s) gone, Comments = %d, want 0:\n%s", name, n, unkept.Comments, got)
			}
			again, _, err := EncodeOver(m, got, nil)
			must(t, err)
			if string(again) != string(got) {
				t.Errorf("%s: a second rewrite changed the file:\n%s\nto:\n%s", name, got, again)
			}
		}
	})
}
