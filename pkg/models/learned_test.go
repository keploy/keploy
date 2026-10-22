package models

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func wordsFixture() WordsEntry {
	return WordsEntry{Digest: "9f2c41d07a3be815", Layout: "4be1c09a5d3e7f21", Slots: []WordSlot{
		{Seg: 0, Run: 0},
		{Seg: 3, Kind: WordSlotToken},
		{Seg: 5, Run: 2},
		{Seg: 7, Kind: WordSlotInstant, Layout: "iso8601", Skeleton: "d4-d2-d2Td2:d2:d2FZ"},
	}}
}

// A slot is written with its run only where the run names one: a slot of
// one run always (run 0 too), a token or an instant never, unless it is
// malformed and carries one, which is kept so the entry stays void.
func TestAWordSlotIsWrittenWithItsRunOnlyWhereTheRunNamesOne(t *testing.T) {
	for _, tc := range []struct {
		slot WordSlot
		yaml string
		json string
	}{
		{WordSlot{Seg: 0, Run: 0}, "seg: 0\nrun: 0\n", `{"seg":0,"run":0}`},
		{WordSlot{Seg: 5, Run: 2}, "seg: 5\nrun: 2\n", `{"seg":5,"run":2}`},
		{WordSlot{Seg: 3, Kind: WordSlotToken}, "seg: 3\nkind: token\n", `{"seg":3,"kind":"token"}`},
		{WordSlot{Seg: 7, Kind: WordSlotInstant, Layout: "iso8601", Skeleton: "d4-d2-d2"},
			"seg: 7\nkind: instant\nlayout: iso8601\nskeleton: d4-d2-d2\n",
			`{"seg":7,"kind":"instant","layout":"iso8601","skeleton":"d4-d2-d2"}`},
		{WordSlot{Seg: 3, Run: 1, Kind: WordSlotToken}, "seg: 3\nrun: 1\nkind: token\n", `{"seg":3,"run":1,"kind":"token"}`},
	} {
		y, err := yaml.Marshal(tc.slot)
		if err != nil {
			t.Fatal(err)
		}
		if string(y) != tc.yaml {
			t.Errorf("%+v: yaml %q, want %q", tc.slot, y, tc.yaml)
		}
		j, err := json.Marshal(tc.slot)
		if err != nil {
			t.Fatal(err)
		}
		if string(j) != tc.json {
			t.Errorf("%+v: json %s, want %s", tc.slot, j, tc.json)
		}
		var fromYAML, fromJSON WordSlot
		if err := yaml.Unmarshal(y, &fromYAML); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(j, &fromJSON); err != nil {
			t.Fatal(err)
		}
		if fromYAML != tc.slot || fromJSON != tc.slot {
			t.Errorf("%+v read back as %+v (yaml), %+v (json)", tc.slot, fromYAML, fromJSON)
		}
	}
}

// A variable's words are stored with it, under variables.<name>.words, in
// YAML and JSON, and read back as they were; a variable with none is written
// as before, with no words key.
func TestAVariablesWordsRoundTrip(t *testing.T) {
	e := wordsFixture()
	in := MockVariables{"ref": {Recorded: "ORD-1 ref=a1", Bind: "body.ref", Echo: true, Words: &e}, "id": {Recorded: "7", Bind: "body.id"}}
	y, err := yaml.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	j, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var fromYAML, fromJSON MockVariables
	if err := yaml.Unmarshal(y, &fromYAML); err != nil {
		t.Fatalf("%v in\n%s", err, y)
	}
	if err := json.Unmarshal(j, &fromJSON); err != nil {
		t.Fatal(err)
	}
	for format, got := range map[string]MockVariables{"yaml": fromYAML, "json": fromJSON} {
		if !reflect.DeepEqual(got, in) {
			t.Errorf("%s: read back %+v, want %+v", format, got, in)
		}
	}
	if strings.Count(string(y), "words:") != 1 || strings.Count(string(j), `"words"`) != 1 {
		t.Errorf("only the variable with words may write the key:\n%s\n%s", y, j)
	}

	bare := MockVariables{"id": {Recorded: "7", Bind: "body.id"}}
	y, _ = yaml.Marshal(bare)
	j, _ = json.Marshal(bare)
	if string(y) != "id:\n    recorded: \"7\"\n    bind: body.id\n" || string(j) != `{"id":{"recorded":"7","bind":"body.id"}}` {
		t.Errorf("a variable with no words is written otherwise than before:\n%s\n%s", y, j)
	}
}

// A copy of a mock's variables, or of the mock, shares no words with it.
func TestACopyOfWordsSharesNoSlot(t *testing.T) {
	e := wordsFixture()
	vars := MockVariables{"ref": {Recorded: "r", Bind: "body.ref", Words: &e}}
	c := vars.Clone()
	if !reflect.DeepEqual(c, vars) {
		t.Fatalf("clone %+v, want %+v", c, vars)
	}
	c["ref"].Words.Slots[0].Seg = 99
	c["ref"].Words.Digest = "changed"
	if vars["ref"].Words.Slots[0].Seg != 0 || vars["ref"].Words.Digest != e.Digest {
		t.Error("a clone of the variables shares the words with the original")
	}

	m := &Mock{Spec: MockSpec{LearnedWords: map[string]WordsEntry{"body.note": wordsFixture()}}}
	d := m.DeepCopy()
	if !reflect.DeepEqual(d.Spec.LearnedWords, m.Spec.LearnedWords) {
		t.Fatalf("deep copy's words %+v, want %+v", d.Spec.LearnedWords, m.Spec.LearnedWords)
	}
	d.Spec.LearnedWords["body.note"].Slots[0].Seg = 99
	d.Spec.LearnedWords["body.other"] = WordsEntry{}
	if m.Spec.LearnedWords["body.note"].Slots[0].Seg != 0 || len(m.Spec.LearnedWords) != 1 {
		t.Error("a deep copy of the mock shares its words with the original")
	}
	if (&Mock{}).DeepCopy().Spec.LearnedWords != nil {
		t.Error("a deep copy of a mock with no words holds some")
	}
	if got := (WordsEntry{Slots: []WordSlot{}}).Clone(); got.Slots == nil {
		t.Error("a clone of an entry with an empty slot list holds none, and is written otherwise")
	}
}
