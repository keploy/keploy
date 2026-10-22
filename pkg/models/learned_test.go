package models

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func wordsFixture() WordsEntry {
	return WordsEntry{V: WordsVersion, Digest: "9f2c41d07a3be815", Layout: "4be1c09a5d3e7f21", Slots: []WordSlot{
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

// Two entries of one leaf in one layout union their slots: what either run
// saw change may change. Any other pair is void, and stays void through
// every later merge, so the leaf is compared whole.
func TestMergeWordsEntryUnionsOrVoids(t *testing.T) {
	a := WordsEntry{Digest: "9f2c41d07a3be815", Layout: "4be1c09a5d3e7f21", Slots: []WordSlot{{Seg: 5, Run: 0}, {Seg: 1, Run: 2}}}
	b := WordsEntry{Digest: "9f2c41d07a3be815", Layout: "4be1c09a5d3e7f21", Slots: []WordSlot{{Seg: 1, Run: 2}, {Seg: 3, Kind: WordSlotToken}, {Seg: 3, Run: 1}}}
	got := MergeWordsEntry(a, b)
	want := []WordSlot{{Seg: 1, Run: 2}, {Seg: 3, Kind: WordSlotToken}, {Seg: 5, Run: 0}}
	if got.Void() || !reflect.DeepEqual(got.Slots, want) || got.Digest != a.Digest || got.Layout != a.Layout {
		t.Fatalf("union = %+v, want slots %+v", got, want)
	}
	if a.Slots[0].Seg != 5 || len(a.Slots) != 2 || len(b.Slots) != 3 {
		t.Fatalf("the merge changed its inputs: %+v %+v", a, b)
	}

	for _, c := range []struct {
		name string
		b    WordsEntry
	}{
		{"another digest", WordsEntry{Digest: "0000000000000000", Layout: a.Layout, Slots: a.Slots}},
		{"another layout", WordsEntry{Digest: a.Digest, Layout: "0000000000000000", Slots: a.Slots}},
		{"a void one", VoidWords(a.Digest)},
	} {
		v := MergeWordsEntry(a, c.b)
		if why := v.Unreadable(); why != "" {
			t.Errorf("%s: the void entry is unreadable (%s): it is this version's, and void", c.name, why)
		}
		if !v.Void() || len(v.Slots) != 0 {
			t.Errorf("%s: merged %+v, want void", c.name, v)
		}
		if again := MergeWordsEntry(v, a); !again.Void() {
			t.Errorf("%s: a void entry merged with a sound one is %+v, want void still", c.name, again)
		}
	}
}

// MergeWords keeps a place either side holds, merges a place both hold, and
// shares no slot with its inputs.
func TestMergeWordsKeepsEveryPlace(t *testing.T) {
	if MergeWords(nil, map[string]WordsEntry{}) != nil {
		t.Error("two empty maps merge to a map")
	}
	e := wordsFixture()
	a := map[string]WordsEntry{"body.note": e}
	b := map[string]WordsEntry{"header.x-ref": {Digest: "1111111111111111", Layout: "2222222222222222", Slots: []WordSlot{{Seg: 0, Run: 0}}},
		"body.note": {Digest: e.Digest, Layout: e.Layout, Slots: []WordSlot{{Seg: 9, Run: 1}}}}
	got := MergeWords(a, b)
	if len(got) != 2 || len(got["body.note"].Slots) != len(e.Slots)+1 || got["header.x-ref"].Digest != "1111111111111111" {
		t.Fatalf("merged %+v", got)
	}
	got["body.note"].Slots[0].Seg = 77
	if a["body.note"].Slots[0].Seg == 77 || len(a) != 1 {
		t.Error("the merged map shares its slots with an input")
	}
}
