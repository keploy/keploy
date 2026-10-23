package models

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A WordSlot stays comparable: the learn step sorts and dedupes slots by ==.
var _ = WordSlot{} == WordSlot{}

// plainYAML is doc read as plain values, for comparing what two documents
// say rather than how they are written.
func plainYAML(t *testing.T, doc []byte) interface{} {
	t.Helper()
	var v interface{}
	if err := yaml.Unmarshal(doc, &v); err != nil {
		t.Fatalf("%v in\n%s", err, doc)
	}
	return v
}

// plainJSON is doc read as plain values.
func plainJSON(t *testing.T, doc []byte) interface{} {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal(doc, &v); err != nil {
		t.Fatalf("%v in\n%s", err, doc)
	}
	return v
}

// viaJSON is v, plain values, as JSON reads them back (every number a
// float64), to compare with what JSON read.
func viaJSON(t *testing.T, v interface{}) interface{} {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return plainJSON(t, b)
}

// gobTrip is v written and read back by gob, into out.
func gobTrip(t *testing.T, v, out interface{}) {
	t.Helper()
	var b bytes.Buffer
	if err := gob.NewEncoder(&b).Encode(v); err != nil {
		t.Fatal(err)
	}
	if err := gob.NewDecoder(&b).Decode(out); err != nil {
		t.Fatal(err)
	}
}

// readYAMLEntry is doc read as one entry: an error fails the test.
func readYAMLEntry(t *testing.T, doc string) WordsEntry {
	t.Helper()
	var e WordsEntry
	if err := yaml.Unmarshal([]byte(doc), &e); err != nil {
		t.Fatalf("%v in\n%s", err, doc)
	}
	return e
}

// An entry this build writes is written with its version first, then its
// guards and slots, and reads back as it was in YAML, JSON and gob: it is
// readable.
func TestAWordsEntryIsWrittenWithItsVersionFirst(t *testing.T) {
	e := wordsFixture()
	y, err := yaml.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	wantY := "v: 1\ndigest: 9f2c41d07a3be815\nlayout: 4be1c09a5d3e7f21\nslots:\n" +
		"    - seg: 0\n      run: 0\n    - seg: 3\n      kind: token\n    - seg: 5\n      run: 2\n" +
		"    - seg: 7\n      kind: instant\n      layout: iso8601\n      skeleton: d4-d2-d2Td2:d2:d2FZ\n"
	if string(y) != wantY {
		t.Errorf("yaml\n%s\nwant\n%s", y, wantY)
	}
	j, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	wantJ := `{"v":1,"digest":"9f2c41d07a3be815","layout":"4be1c09a5d3e7f21","slots":[{"seg":0,"run":0},{"seg":3,"kind":"token"},{"seg":5,"run":2},{"seg":7,"kind":"instant","layout":"iso8601","skeleton":"d4-d2-d2Td2:d2:d2FZ"}]}`
	if string(j) != wantJ {
		t.Errorf("json %s\nwant %s", j, wantJ)
	}
	var fromY, fromJ, fromG WordsEntry
	if err := yaml.Unmarshal(y, &fromY); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(j, &fromJ); err != nil {
		t.Fatal(err)
	}
	gobTrip(t, e, &fromG)
	for format, got := range map[string]WordsEntry{"yaml": fromY, "json": fromJ, "gob": fromG} {
		if !reflect.DeepEqual(got, e) {
			t.Errorf("%s: read back %+v, want %+v", format, got, e)
		}
		if why := got.Unreadable(); why != "" {
			t.Errorf("%s: an entry this build wrote is unreadable: %s", format, why)
		}
	}

	// An entry with no slot list is written as one with none always was.
	bare := WordsEntry{V: WordsVersion, Digest: "d", Layout: "l"}
	y, _ = yaml.Marshal(bare)
	j, _ = json.Marshal(bare)
	if string(y) != "v: 1\ndigest: d\nlayout: l\nslots: []\n" || string(j) != `{"v":1,"digest":"d","layout":"l","slots":null}` {
		t.Errorf("an entry with no slot list written as\n%s\n%s", y, j)
	}
}

// seen is reserved: an entry carries it as written, through YAML, JSON and
// gob and from one to another, and it makes no entry unreadable.
func TestSeenIsCarriedAndNeverRead(t *testing.T) {
	in := "v: 1\ndigest: d\nlayout: l\nslots: []\nseen:\n    at: \"2026-10-22T10:00:00Z\"\n    values:\n        - ORD-1 paid 100\n        - \"7\"\n        - 7\n"
	e := readYAMLEntry(t, in)
	if why := e.Unreadable(); why != "" {
		t.Fatalf("seen made the entry unreadable: %s", why)
	}
	if e.Seen == "" || e.Unknown != "" {
		t.Fatalf("seen read as %q, unknown %q: want it in Seen only", e.Seen, e.Unknown)
	}
	y, err := yaml.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if string(y) != in {
		t.Errorf("an entry with seen written back as\n%s\nwant\n%s", y, in)
	}
	j, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var fromJ, fromG WordsEntry
	if err := json.Unmarshal(j, &fromJ); err != nil {
		t.Fatal(err)
	}
	gobTrip(t, fromJ, &fromG)
	y2, err := yaml.Marshal(fromG)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plainYAML(t, y2), plainYAML(t, []byte(in))) {
		t.Errorf("seen through json and gob written back as\n%s\nwant\n%s", y2, in)
	}
}

// A key this build does not know, of the entry or of a slot, makes the entry
// unreadable (written by a newer keploy), and is carried as written: a
// rewrite in YAML or JSON, and a trip through gob, write it back, after the
// keys this build knows.
func TestAKeyThisBuildDoesNotKnowIsCarriedAndVoidsTheEntry(t *testing.T) {
	for _, tc := range []struct{ name, doc string }{
		{"entry", "v: 1\ndigest: d\nlayout: l\nslots:\n    - seg: 5\n      run: 0\nonly:\n    - 1\n    - 3\n"},
		{"slot", "v: 1\ndigest: d\nlayout: l\nslots:\n    - seg: 5\n      run: 0\n      class: hex\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := readYAMLEntry(t, tc.doc)
			if why := e.Unreadable(); why != WordsWrittenByNewer {
				t.Fatalf("Unreadable = %q, want %q", why, WordsWrittenByNewer)
			}
			y, err := yaml.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			if string(y) != tc.doc {
				t.Errorf("written back as\n%s\nwant\n%s", y, tc.doc)
			}
			j, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			var fromJ, fromG WordsEntry
			if err := json.Unmarshal(j, &fromJ); err != nil {
				t.Fatal(err)
			}
			gobTrip(t, fromJ, &fromG)
			if !reflect.DeepEqual(fromG, e) {
				t.Errorf("through json and gob read as %+v, want %+v", fromG, e)
			}
			if why := fromG.Unreadable(); why != WordsWrittenByNewer {
				t.Errorf("through json and gob Unreadable = %q", why)
			}
		})
	}
}

// An entry of a version this build does not read is carried whole, whatever
// shape a newer keploy gave its keys: it decodes, inside a mock's variables
// too, admits nothing, and is written back saying what it said.
func TestAnEntryOfANewerVersionIsCarriedWhole(t *testing.T) {
	doc := "v: 2\ndigest: d\nslots:\n    seg5: {run: 0, class: hex}\nlayout: [a, b]\nseen: x\n"
	e := readYAMLEntry(t, doc)
	if e.V != 2 || e.Digest != "" || e.Slots != nil || e.Seen != "" {
		t.Fatalf("a v2 entry read into this version's fields: %+v", e)
	}
	if why := e.Unreadable(); why != WordsWrittenByNewer {
		t.Fatalf("Unreadable = %q, want %q", why, WordsWrittenByNewer)
	}
	y, err := yaml.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plainYAML(t, y), plainYAML(t, []byte(doc))) {
		t.Errorf("written back as\n%s\nwant what\n%s\nsays", y, doc)
	}
	if !strings.HasPrefix(string(y), "v: 2\n") {
		t.Errorf("the version is not written first:\n%s", y)
	}
	j, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plainJSON(t, j), viaJSON(t, plainYAML(t, []byte(doc)))) {
		t.Errorf("written in json as %s", j)
	}
	var fromJ WordsEntry
	if err := json.Unmarshal(j, &fromJ); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromJ, e) {
		t.Errorf("read back from json as %+v, want %+v", fromJ, e)
	}

	vars := "ref:\n    recorded: r\n    bind: body.ref\n    words:\n" + indent(doc, "        ")
	var mv MockVariables
	if err := yaml.Unmarshal([]byte(vars), &mv); err != nil {
		t.Fatalf("a mock's variables holding a v2 entry do not decode: %v", err)
	}
	var fromG MockVariables
	gobTrip(t, mv, &fromG)
	y, err = yaml.Marshal(fromG)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plainYAML(t, y), plainYAML(t, []byte(vars))) {
		t.Errorf("variables through gob written back as\n%s\nwant what\n%s\nsays", y, vars)
	}
}

// indent is s with each line prefixed.
func indent(s, prefix string) string {
	lines := strings.SplitAfter(s, "\n")
	var b strings.Builder
	for _, l := range lines {
		if l != "" {
			b.WriteString(prefix + l)
		}
	}
	return b.String()
}

// An entry with no version was not written as keploy writes words: it
// admits nothing, and is written back with none. A version that is not an
// integer is one this build does not know, and is carried.
func TestAnEntryWithNoVersionAdmitsNothing(t *testing.T) {
	doc := "digest: d\nlayout: l\nslots: []\n"
	e := readYAMLEntry(t, doc)
	if why := e.Unreadable(); why != WordsUnversioned {
		t.Errorf("Unreadable = %q, want %q", why, WordsUnversioned)
	}
	y, err := yaml.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plainYAML(t, y), plainYAML(t, []byte(doc))) || strings.Contains(string(y), "v:") {
		t.Errorf("written back as\n%s\nwant what\n%s\nsays, with no version", y, doc)
	}
	if why := (WordsEntry{}).Unreadable(); why != WordsUnversioned {
		t.Errorf("the zero entry's Unreadable = %q, want %q", why, WordsUnversioned)
	}
	for _, v := range []string{`"1"`, "1.0", "two", "[1]"} {
		doc := "v: " + v + "\ndigest: d\nlayout: l\nslots: []\n"
		e := readYAMLEntry(t, doc)
		if why := e.Unreadable(); why != WordsWrittenByNewer {
			t.Errorf("v: %s: Unreadable = %q, want %q", v, why, WordsWrittenByNewer)
		}
		y, err := yaml.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(plainYAML(t, y), plainYAML(t, []byte(doc))) {
			t.Errorf("v: %s: written back as\n%s", v, y)
		}
		j, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		var fromJ WordsEntry
		if err := json.Unmarshal(j, &fromJ); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(fromJ, e) {
			t.Errorf("v: %s: through json %s read as %+v, want %+v", v, j, fromJ, e)
		}
	}
}

// A carried value keeps the type YAML gave each of its scalars, through YAML,
// JSON and gob.
func TestACarriedValueKeepsItsTypes(t *testing.T) {
	doc := "v: 1\ndigest: d\nlayout: l\nslots: []\nx:\n" +
		"    int: 7\n    zeros: \"007\"\n    octal: 007\n    big: 123456789012345678901234567890\n" +
		"    float: 1.5\n    exp: 1e5\n    inf: .inf\n    yes: true\n    str: \"true\"\n    nil: null\n" +
		"    text: \"ü: a # b\"\n    lines: \"a\\nb\"\n    empty: \"\"\n    list:\n        - {a: 1}\n        - []\n        - {}\n"
	e := readYAMLEntry(t, doc)
	y, err := yaml.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var fromJ, fromG WordsEntry
	j, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(j, &fromJ); err != nil {
		t.Fatal(err)
	}
	gobTrip(t, fromJ, &fromG)
	y2, err := yaml.Marshal(fromG)
	if err != nil {
		t.Fatal(err)
	}
	type typed struct {
		X struct {
			Int   int         `yaml:"int"`
			Zeros string      `yaml:"zeros"`
			Octal int         `yaml:"octal"`
			Big   string      `yaml:"big"`
			Float float64     `yaml:"float"`
			Exp   float64     `yaml:"exp"`
			Inf   string      `yaml:"inf"`
			Yes   bool        `yaml:"yes"`
			Str   string      `yaml:"str"`
			Nil   interface{} `yaml:"nil"`
			Text  string      `yaml:"text"`
			Lines string      `yaml:"lines"`
			Empty string      `yaml:"empty"`
			List  []interface{}
		} `yaml:"x"`
	}
	for name, out := range map[string][]byte{"yaml": y, "json then gob": y2} {
		var got typed
		if err := yaml.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s: %v in\n%s", name, err, out)
		}
		x := got.X
		if x.Int != 7 || x.Zeros != "007" || x.Octal != 7 || x.Big != "123456789012345678901234567890" ||
			x.Float != 1.5 || x.Exp != 1e5 || x.Inf != ".inf" || !x.Yes || x.Str != "true" || x.Nil != nil ||
			x.Text != "ü: a # b" || x.Lines != "a\nb" || x.Empty != "" {
			t.Errorf("%s: a carried value's scalars read back as %+v from\n%s", name, x, out)
		}
		var str interface{}
		if err := yaml.Unmarshal(out, &str); err != nil {
			t.Fatal(err)
		}
		m := str.(map[string]interface{})["x"].(map[string]interface{})
		if _, ok := m["str"].(string); !ok {
			t.Errorf("%s: the string \"true\" read back as %T", name, m["str"])
		}
		if _, ok := m["zeros"].(string); !ok {
			t.Errorf("%s: the string \"007\" read back as %T", name, m["zeros"])
		}
	}
}

// A known key of the wrong type, a key held twice, a key that is not a
// string and an entry that is not a mapping fail the decode, as a malformed
// field anywhere in a mock does; the same key in an entry of a newer version
// is carried.
func TestAMalformedEntryFailsItsDecode(t *testing.T) {
	for _, doc := range []string{
		"v: 1\ndigest: [1]\nlayout: l\nslots: []\n",
		"v: 1\ndigest: d\nlayout: l\nslots: {a: 1}\n",
		"v: 1\ndigest: d\nlayout: l\nslots:\n    - seg: x\n",
		"v: 1\ndigest: d\ndigest: e\nlayout: l\nslots: []\n",
		"v: 1\ndigest: d\nlayout: l\nslots: []\n? [k]\n: v\n",
		"[1, 2]\n",
	} {
		var e WordsEntry
		if err := yaml.Unmarshal([]byte(doc), &e); err == nil {
			t.Errorf("decoded\n%s\nas %+v: want an error", doc, e)
		}
	}
	for _, doc := range []string{`{"v":1,"digest":[1],"layout":"l","slots":[]}`, `[1]`, `{"v":1,"slots":[{"seg":"x"}]}`} {
		var e WordsEntry
		if err := json.Unmarshal([]byte(doc), &e); err == nil {
			t.Errorf("decoded %s as %+v: want an error", doc, e)
		}
	}
	e := readYAMLEntry(t, "v: 2\ndigest: [1]\n")
	if why := e.Unreadable(); why != WordsWrittenByNewer {
		t.Errorf("a v2 entry's Unreadable = %q", why)
	}
}

// A carried value that aliases its way past the bound fails the decode, and
// fails it at once.
func TestACarriedAliasBombFailsItsDecode(t *testing.T) {
	doc := "v: 1\ndigest: d\nlayout: l\nslots: []\n" +
		"a: &a [x, x, x, x, x, x, x, x, x, x]\n" +
		"b: &b [*a, *a, *a, *a, *a, *a, *a, *a, *a, *a]\n" +
		"c: &c [*b, *b, *b, *b, *b, *b, *b, *b, *b, *b]\n" +
		"d: &d [*c, *c, *c, *c, *c, *c, *c, *c, *c, *c]\n" +
		"e: &e [*d, *d, *d, *d, *d, *d, *d, *d, *d, *d]\n" +
		"f: [*e, *e, *e, *e, *e, *e, *e, *e, *e, *e]\n"
	var e WordsEntry
	err := yaml.Unmarshal([]byte(doc), &e)
	if err == nil || !strings.Contains(err.Error(), errCarriedTooLarge.Error()) {
		t.Errorf("an alias bomb decoded (%v): want %v", err, errCarriedTooLarge)
	}
}

// A mock's words travel by gob, as a recording's mocks do from the agent:
// every field, carried keys included, arrives.
func TestAMocksWordsTravelByGob(t *testing.T) {
	newer := readYAMLEntry(t, "v: 1\ndigest: d\nlayout: l\nslots:\n    - seg: 1\n      run: 0\n      class: hex\nseen: [a]\nonly: 3\n")
	m := Mock{Spec: MockSpec{
		LearnedWords: map[string]WordsEntry{"body.note": wordsFixture(), "body.ref": newer},
		Variables:    MockVariables{"ref": {Recorded: "r", Bind: "body.ref", Words: &newer}},
	}}
	var got Mock
	gobTrip(t, &m, &got)
	if !reflect.DeepEqual(got.Spec.LearnedWords, m.Spec.LearnedWords) || !reflect.DeepEqual(got.Spec.Variables, m.Spec.Variables) {
		t.Errorf("through gob: words %+v, variables %+v; want %+v, %+v", got.Spec.LearnedWords, got.Spec.Variables, m.Spec.LearnedWords, m.Spec.Variables)
	}
	st := MockState{LearnedWords: m.Spec.LearnedWords}
	j, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back MockState
	if err := json.Unmarshal(j, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.LearnedWords, st.LearnedWords) {
		t.Errorf("a mock state's words through json: %+v, want %+v", back.LearnedWords, st.LearnedWords)
	}
}

// isJSONNumber is JSON's number grammar, each part of it; isJSONInteger is a
// number of it with no fraction and no exponent.
func TestTheJSONNumberGrammar(t *testing.T) {
	for s, want := range map[string]bool{
		"0": true, "-0": true, "7": true, "9": true, "19": true, "90": true, "-12": true,
		"1.5": true, "1.9": true, "1e0": true, "1e10": true, "0.25": true, "1e5": true, "1E5": true, "1e+5": true, "1e-5": true, "1.5e3": true, "-1.5E-3": true, "1e59": true,
		"": false, "-": false, "01": false, "+1": false, "1.": false, ".5": false, "-.5": false, "e5": false, "E5": false,
		"1e": false, "1e+": false, "1E-": false, "1x": false, "0x1F": false, "1.2.3": false, "--1": false, "1e5.0": false,
		"a": false, ":": false, "1_000": false, "1.5a": false, "1.5-": false, "1.a": false, "1e5a": false, "1e5-": false, "1ee5": false,
	} {
		if got := isJSONNumber(s); got != want {
			t.Errorf("isJSONNumber(%q) = %v, want %v", s, got, want)
		}
	}
	for raw, want := range map[string]bool{"1": true, " 1 ": true, "-3": true, "1.0": false, "1.5": false, "1e3": false, "1E3": false, `"1"`: false} {
		if got := isJSONInteger(json.RawMessage(raw)); got != want {
			t.Errorf("isJSONInteger(%q) = %v, want %v", raw, got, want)
		}
	}
}

// An entry of this version writes its digest, layout and slots always (an
// empty guard too, so it reads back malformed, as written), and one of any
// other version v where set and each known field only where set.
func TestWhatAnEntryWrites(t *testing.T) {
	for _, tc := range []struct {
		e    WordsEntry
		want string
	}{
		{WordsEntry{V: WordsVersion}, "v: 1\ndigest: \"\"\nlayout: \"\"\nslots: []\n"},
		{WordsEntry{V: WordsVersion + 1}, "v: 2\n"},
		{WordsEntry{V: WordsVersion + 1, Digest: "d"}, "v: 2\ndigest: d\n"},
		{WordsEntry{V: WordsVersion + 1, Layout: "l"}, "v: 2\nlayout: l\n"},
		{WordsEntry{V: WordsVersion + 1, Slots: []WordSlot{}}, "v: 2\nslots: []\n"},
		// A known key the carried keys hold too is written once, as the
		// field says.
		{WordsEntry{V: WordsVersion + 1, Digest: "d", Unknown: `{"digest":"x","z":1}`}, "v: 2\ndigest: d\nz: 1\n"},
		// Carried keys are written after the known ones, sorted.
		{WordsEntry{V: WordsVersion + 1, Unknown: `{"z":1,"a":2,"m":3}`}, "v: 2\na: 2\nm: 3\nz: 1\n"},
	} {
		y, err := yaml.Marshal(tc.e)
		if err != nil || string(y) != tc.want {
			t.Errorf("%+v written as %q (%v), want %q", tc.e, y, err, tc.want)
		}
	}
	j, err := json.Marshal(WordsEntry{V: WordsVersion + 1, Digest: "d", Unknown: `{"digest":"x","z":1}`})
	if err != nil || string(j) != `{"v":2,"digest":"d","z":1}` {
		t.Errorf("JSON %s (%v)", j, err)
	}
}

// What cannot be written is an error, never a key written with nothing: a
// slot's or an entry's carried keys that are no JSON object, and a carried
// value that is no JSON value or holds none.
func TestWhatCannotBeWrittenFailsTheWrite(t *testing.T) {
	for name, v := range map[string]interface{}{
		"a slot's carried keys":        WordSlot{Unknown: "not json"},
		"an entry's carried keys":      WordsEntry{V: WordsVersion + 1, Unknown: "[1]"},
		"a slot of an entry":           WordsEntry{V: WordsVersion, Slots: []WordSlot{{Unknown: "not json"}}},
		"a carried value of no JSON":   WordsEntry{V: WordsVersion, Seen: "{"},
		"a carried value holding none": WordsEntry{V: WordsVersion, Seen: "# nothing"},
	} {
		if y, err := yaml.Marshal(v); err == nil {
			t.Errorf("%s: written as YAML\n%s", name, y)
		}
		if _, isSeen := v.(WordsEntry); isSeen && strings.HasPrefix(name, "a carried value") {
			continue // JSON writes a carried value as its raw text
		}
		if j, err := json.Marshal(v); err == nil {
			t.Errorf("%s: written as JSON %s", name, j)
		}
		// Its own writer says so, never handing JSON a key with nothing.
		if j, err := v.(json.Marshaler).MarshalJSON(); err == nil {
			t.Errorf("%s: MarshalJSON wrote %q", name, j)
		}
	}
	// Carried keys that do not read are no version: an entry with no v is
	// unversioned, whatever they hold.
	if why := (WordsEntry{Unknown: "not json"}).Unreadable(); why != WordsUnversioned {
		t.Errorf("an entry with no v and carried keys that do not read: %q, want %q", why, WordsUnversioned)
	}
}

// A slot that is no mapping (YAML) or no object (JSON) fails its decode; a
// JSON null slot or entry is the zero one, as encoding/json reads a null; and
// an integer key beside v is carried, v read from v alone.
func TestHowASlotAndAnEntryDecode(t *testing.T) {
	var e WordsEntry
	if err := yaml.Unmarshal([]byte("v: 1\ndigest: d\nlayout: l\nslots: [5]\n"), &e); err == nil {
		t.Errorf("a slot that is no mapping decoded: %+v", e)
	}
	if err := json.Unmarshal([]byte(`{"v":1,"digest":"d","layout":"l","slots":[5]}`), &e); err == nil {
		t.Errorf("a slot that is no object decoded: %+v", e)
	}
	e = WordsEntry{}
	if err := json.Unmarshal([]byte(`{"v":1,"digest":"d","layout":"l","slots":[null]}`), &e); err != nil || len(e.Slots) != 1 || e.Slots[0] != (WordSlot{}) {
		t.Errorf("a null slot: %+v (%v), want one zero slot", e, err)
	}
	var words map[string]WordsEntry
	if err := json.Unmarshal([]byte(`{"body.note":null}`), &words); err != nil || !reflect.DeepEqual(words["body.note"], WordsEntry{}) {
		t.Errorf("a null entry: %+v (%v), want the zero entry", words, err)
	}
	e = readYAMLEntry(t, "v: 1\ndigest: d\nlayout: l\nslots: []\nx: 7\n")
	if e.V != WordsVersion || !e.Unknown.has("x") || e.Digest != "d" {
		t.Fatalf("an integer key beside v: %+v", e)
	}
	if y, _ := yaml.Marshal(e); !strings.HasPrefix(string(y), "v: 1\ndigest: d\n") || !strings.Contains(string(y), "x: 7") {
		t.Errorf("written back as\n%s", y)
	}
}

// A carried value keeps each scalar as YAML resolves it, where JSON cannot
// write it as YAML does (an integer in hex, a fraction with no leading
// digit, -0, one past every integer type) and where JSON cannot write it at
// all (NaN, as its text); its text is written as it is, no character escaped
// for HTML; and one with a key twice, at any depth, fails the decode.
func TestACarriedValueIsWhatYAMLReadsIt(t *testing.T) {
	doc := "v: 1\ndigest: d\nlayout: l\nslots: []\nx:\n" +
		"    hex: -0x1F\n    uhex: 0xFFFFFFFFFFFFFFFF\n    half: .5\n    negzero: -0\n    nan: .nan\n" +
		"    huge: 123456789012345678901234567890\n    html: \"a<b&c>\"\n"
	e := readYAMLEntry(t, doc)
	j, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"hex":-31`, `"uhex":18446744073709551615`, `"half":0.5`, `"negzero":-0`, `"nan":".nan"`, `"huge":123456789012345678901234567890`} {
		if !strings.Contains(string(j), want) {
			t.Errorf("%s lacks %s", j, want)
		}
	}
	// The carried text is held as compact JSON with no character escaped
	// for HTML (an encoder writing the mock may escape it; YAML never does).
	if !strings.Contains(string(e.Unknown), `"html":"a<b&c>"`) {
		t.Errorf("carried as %s", e.Unknown)
	}
	for _, bad := range []string{"x: {a: 1, a: 2}\n", "x: {a: {b: 1, b: 2}}\n"} {
		var e WordsEntry
		if err := yaml.Unmarshal([]byte("v: 1\ndigest: d\nlayout: l\nslots: []\n"+bad), &e); err == nil {
			t.Errorf("%q decoded: %+v", bad, e)
		}
	}
	// The bound counts every node: a value of exactly maxCarriedNodes
	// nodes is carried, one of one more is too large.
	seq := func(n int) string {
		return "v: 1\ndigest: d\nlayout: l\nslots: []\nx: [" + strings.TrimSuffix(strings.Repeat("1,", n), ",") + "]\n"
	}
	if err := yaml.Unmarshal([]byte(seq(maxCarriedNodes-1)), &e); err != nil {
		t.Errorf("a value of %d nodes: %v", maxCarriedNodes, err)
	}
	if err := yaml.Unmarshal([]byte(seq(maxCarriedNodes)), &e); err == nil || !strings.Contains(err.Error(), errCarriedTooLarge.Error()) {
		t.Errorf("a value of %d nodes: %v, want too large", maxCarriedNodes+1, err)
	}
}
