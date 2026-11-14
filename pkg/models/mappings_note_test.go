package models

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// noteRows are notes a harness may send, each of which a mapping must give
// back byte for byte: YAML's own syntax, white space at either end, line
// breaks of every kind, control characters, and the longest keploy keeps.
var noteRows = []string{
	"frames /a:2 /b:0; draws 17",
	"  lead and trail  ",
	"line\nbreak\r\nand\rcr\n",
	"\ttab\x00nul\x1bescape\u0085nel\u2028ls\u2029ps\ufeffbom",
	`{"json": [1, "two"], "q": 'x'}`,
	"- dash\n? key\n: value\n# hash\n--- \n...",
	"null", "~", "true", "0x1F", "1e3", "&anchor *alias !tag |",
	"é 日本 😀",
	strings.Repeat("n", MaxScopeNote),
}

// A test entry's note is written and read back as it was sent, in YAML and
// in JSON; an entry with none writes no note key.
func TestMappedTestCaseKeepsItsNoteByteForByte(t *testing.T) {
	for _, note := range noteRows {
		tc := MappedTestCase{ID: "ui::t", Note: note}
		y, err := yaml.Marshal(Mapping{Version: "api.keploy.io/v1beta1", Kind: "MockMapping", TestCases: []MappedTestCase{tc}})
		if err != nil {
			t.Fatalf("%q: %v", note, err)
		}
		var m Mapping
		if err := yaml.Unmarshal(y, &m); err != nil {
			t.Fatalf("%q: %v\n%s", note, err, y)
		}
		if len(m.TestCases) != 1 || m.TestCases[0].Note != note {
			t.Fatalf("YAML gave back %+v for %q:\n%s", m.TestCases, note, y)
		}
		j, err := json.Marshal(tc)
		if err != nil {
			t.Fatalf("%q: %v", note, err)
		}
		var back MappedTestCase
		if err := json.Unmarshal(j, &back); err != nil || back.Note != note {
			t.Fatalf("JSON gave back %q (%v) for %q", back.Note, err, note)
		}
	}
	y, err := yaml.Marshal(MappedTestCase{ID: "ui::t", Note: "draws 7"})
	if err != nil || !strings.Contains(string(y), "harness_note: draws 7\n") {
		t.Fatalf("the note's key is harness_note: %q (%v)", y, err)
	}
	y, err = yaml.Marshal(MappedTestCase{ID: "ui::t"})
	if err != nil || strings.Contains(string(y), "note") {
		t.Fatalf("an entry with no note wrote %q (%v)", y, err)
	}
}

// A note that is not a string is an error, never an entry read without it.
func TestMappedTestCaseRefusesANoteThatIsNotAString(t *testing.T) {
	var tc MappedTestCase
	if err := yaml.Unmarshal([]byte("id: ui::t\nharness_note: [a, b]\n"), &tc); err == nil || !strings.Contains(err.Error(), "(harness_note) is not a string") {
		t.Fatalf("YAML: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"id":"ui::t","harness_note":{"a":1}}`), &tc); err == nil || !strings.Contains(err.Error(), "(harness_note) is not a string") {
		t.Fatalf("JSON: %v", err)
	}
}

// A note: a hand edit wrote on a test entry, of any shape, is the hand
// edit's: no error, and no harness note.
func TestMappedTestCaseLeavesAHandWrittenNoteAlone(t *testing.T) {
	for _, y := range []string{"id: ui::t\nnote: checked by hand\n", "id: ui::t\nnote: [a, b]\n"} {
		var tc MappedTestCase
		if err := yaml.Unmarshal([]byte(y), &tc); err != nil || tc.Note != "" {
			t.Fatalf("YAML %q: note %q, %v", y, tc.Note, err)
		}
	}
	for _, j := range []string{`{"id":"ui::t","note":"checked by hand"}`, `{"id":"ui::t","note":{"a":1}}`} {
		var tc MappedTestCase
		if err := json.Unmarshal([]byte(j), &tc); err != nil || tc.Note != "" {
			t.Fatalf("JSON %q: note %q, %v", j, tc.Note, err)
		}
	}
}
