package learn

import (
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// NoiseWords names the words of a text that changed, whole where the text
// is one word, and pins none where its skeleton moved.
func TestNoiseWordsNamesTheChangedWordsOfASkeleton(t *testing.T) {
	for _, c := range []struct {
		name, rec, live string
		words           []int
		whole, ok       bool
	}{
		{"one word of a text", "lead r-1 created", "lead r-2 created", []int{1}, false, true},
		{"an instant is one word", "at 2026-10-14T09:02:20.5+05:30 ok", "at 2026-10-14T09:03:21+05:30 ok", []int{1}, false, true},
		{"two words, a separator run between", `{"a": "x1",  "b": "y1"}`, `{"a": "x2",  "b": "y2"}`, []int{3, 6}, false, true},
		{"a first and a last word", "a1 b c1", "a2 b c2", []int{0, 2}, false, true},
		{"one word whole", "r-1", "r-2", []int{0}, true, true},
		{"empty to a word", "", "r-2", []int{0}, true, true},
		{"a word came", "lead r-1", "lead r-2 now", nil, false, false},
		{"a separator changed", "lead r-1 x", "lead r-2,x", nil, false, false},
		{"a separator run grew", "lead r-1 x", "lead r-2  x", nil, false, false},
		{"nothing changed", "lead r-1", "lead r-1", nil, false, false},
	} {
		words, whole, ok := NoiseWords(c.rec, c.live)
		if !reflect.DeepEqual(words, c.words) || whole != c.whole || ok != c.ok {
			t.Errorf("%s: NoiseWords = %v, %v, %v; want %v, %v, %v", c.name, words, whole, ok, c.words, c.whole, c.ok)
		}
	}
}

// A words pattern reads back as the words it names, and a pattern that does
// not read back is a words pattern that leaves no word unchecked; anything
// else is no words pattern.
func TestUncheckedWordsReadsOnlyAWordsPattern(t *testing.T) {
	for _, c := range []struct {
		patterns []string
		words    []int
		isWords  bool
	}{
		{[]string{NoiseWordsPattern([]int{1, 3})}, []int{1, 3}, true},
		{[]string{"^keploy:words 0$"}, []int{0}, true},
		{nil, nil, false},
		{[]string{}, nil, false},
		{[]string{"^r-[0-9]+$"}, nil, false},
		{[]string{NoiseWordsPattern([]int{1}), "x"}, nil, false},
		{[]string{"^keploy:words $"}, nil, true},
		{[]string{"^keploy:words 3,1$"}, nil, true},
		{[]string{"^keploy:words 1,1$"}, nil, true},
		{[]string{"^keploy:words -1$"}, nil, true},
		{[]string{"^keploy:words 01$"}, nil, true},
		{[]string{"^keploy:words 1"}, nil, false},
	} {
		words, isWords := UncheckedWords(c.patterns)
		if !reflect.DeepEqual(words, c.words) || isWords != c.isWords || IsNoiseWords(c.patterns) != c.isWords {
			t.Errorf("UncheckedWords(%q) = %v, %v; want %v, %v", c.patterns, words, isWords, c.words, c.isWords)
		}
	}
}

// WordsAdmit admits a text with the recording's skeleton whose other words
// are the recording's, and nothing else. An unchecked word is unchecked as a
// one-word text learned whole is: at the text's edge it may be empty. A word
// inside the text cannot be: the separators around it would merge into one
// run, and the skeleton changes.
func TestWordsAdmitChecksEveryWordButTheUncheckedOnes(t *testing.T) {
	const rec = "lead r-1 created at s-1"
	for _, c := range []struct {
		name, live string
		words      []int
		want       bool
	}{
		{"the unchecked words changed", "lead r-9 created at s-9", []int{1, 4}, true},
		{"as recorded", rec, []int{1, 4}, true},
		{"a checked word changed", "deal r-9 created at s-9", []int{1, 4}, false},
		{"the last checked word changed", "lead r-9 created on s-9", []int{1, 4}, false},
		{"a word came", "lead r-9 created at s-9 x", []int{1, 4}, false},
		{"a separator changed", "lead r-9,created at s-9", []int{1, 4}, false},
		{"a word past the last", "lead r-9 created at s-1", []int{1, 5}, false},
		{"no words", rec, nil, false},
		{"the unchecked first word empty", " r-1 created at s-1", []int{0}, true},
		{"the unchecked last word empty", "lead r-1 created at ", []int{4}, true},
		{"an unchecked middle word empty", "lead  created at s-1", []int{1}, false},
		{"a checked first word empty", " r-9 created at s-1", []int{1}, false},
	} {
		if got := WordsAdmit(rec, c.live, c.words); got != c.want {
			t.Errorf("%s: WordsAdmit(%q, %v) = %v; want %v", c.name, c.live, c.words, got, c.want)
		}
	}
}

// Two learnings of one place's text by its words leave unchecked the words
// either changed, in order: a later run that moved another word adds it,
// never replaces the first run's. Noise of any other kind keeps what is
// there: whole stays whole, a words pattern stays beside whole noise,
// keploy.yml-style patterns or one that does not read back.
func TestMergeNoiseUnionsTheWordsOfTwoWordsPatterns(t *testing.T) {
	w := func(words ...int) []string { return []string{NoiseWordsPattern(words)} }
	for _, c := range []struct {
		name                     string
		existing, detected, want []string
	}{
		{"another word", w(1), w(3), w(1, 3)},
		{"an earlier word", w(3), w(1), w(1, 3)},
		{"shared words once", w(1, 3), w(0, 3), w(0, 1, 3)},
		{"the detected tail", w(0), w(2, 4), w(0, 2, 4)},
		{"the existing tail", w(2, 4), w(0), w(0, 2, 4)},
		{"an older keploy's whole is read as the words that changed", []string{}, w(1), w(1)},
		{"words beside whole", w(1), []string{}, w(1)},
		{"words beside a pattern", w(1), []string{"^r-[0-9]+$"}, w(1)},
		{"a pattern of another kind is read as the words that changed", []string{"^r-[0-9]+$"}, w(1), w(1)},
		{"whole beside whole", []string{}, []string{}, []string{}},
		{"a pattern of another kind beside whole", []string{}, []string{"^r-[0-9]+$"}, []string{}},
		{"an existing pattern that does not read back", []string{"^keploy:words 01$"}, w(1), []string{"^keploy:words 01$"}},
		{"a detected pattern that does not read back", w(1), []string{"^keploy:words x$"}, w(1)},
	} {
		if got := MergeNoise(c.existing, c.detected); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: MergeNoise(%q, %q) = %q, want %q", c.name, c.existing, c.detected, got, c.want)
		}
	}
	existing := map[string][]string{"body.note": w(1), "body.a": {}}
	got := MergeReqBodyNoise(existing, map[string][]string{"body.note": w(3), "body.b": {}})
	if want := map[string][]string{"body.note": w(1, 3), "body.a": {}, "body.b": {}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("MergeReqBodyNoise = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(existing["body.note"], w(1)) {
		t.Fatalf("MergeReqBodyNoise changed its input: %v", existing)
	}
}

// What a merge onto a recording adds names a place whose words pattern grew,
// as it names a new place, with the switch on and off; a words pattern that
// adds no word adds nothing.
func TestMergeLearnedNamesAWordsPatternThatGrew(t *testing.T) {
	w := func(words ...int) []string { return []string{NoiseWordsPattern(words)} }
	for _, proven := range []string{"1", "0"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", proven)
			for _, c := range []struct {
				detected []string
				added    []string
				want     []string
			}{
				{w(3), []string{"body.new (noise)", "body.note (noise)"}, w(1, 3)},
				{w(1), []string{"body.new (noise)"}, w(1)},
			} {
				m := &models.Mock{Spec: models.MockSpec{ReqBodyNoise: map[string][]string{"body.note": w(1)}}}
				added := MergeLearned(m, models.MockState{ReqBodyNoise: map[string][]string{"body.note": c.detected, "body.new": w(0)}}, nil)
				if !reflect.DeepEqual(added, c.added) || !reflect.DeepEqual(m.Spec.ReqBodyNoise["body.note"], c.want) {
					t.Fatalf("merging %q: added %q noise %v; want %q, %q", c.detected, added, m.Spec.ReqBodyNoise, c.added, c.want)
				}
			}
		})
	}
}

// wordsKnown learned the texts at its paths by their words, and excuses
// nothing whole.
type wordsKnown map[string][]int

func (wordsKnown) Excused(Leaf) (bool, bool) { return false, false }

func (k wordsKnown) Words(l Leaf) ([]int, bool) {
	w, ok := k[l.Path]
	return w, ok
}

// A text a Known learned by its words is the same where only those words
// changed, and drifted where any other did or its skeleton moved: the learn
// step reads it as it reads any value. A Known that learned no words reads
// it whole, and a value that is no text is never read by words.
func TestCompareReadsATextLearnedByItsWordsWordByWord(t *testing.T) {
	rec := body(t, "k", `{"note":"lead r-1 created","n":1}`)
	for _, c := range []struct {
		name  string
		live  string
		known Known
		drift []string
	}{
		{"its word changed", `{"note":"lead r-2 created","n":1}`, wordsKnown{"body.note": {1}}, nil},
		{"another word changed", `{"note":"deal r-2 created","n":1}`, wordsKnown{"body.note": {1}}, []string{"body.note"}},
		{"its skeleton moved", `{"note":"lead r-2 created now","n":1}`, wordsKnown{"body.note": {1}}, []string{"body.note"}},
		{"another place's words", `{"note":"lead r-2 created","n":1}`, wordsKnown{"body.other": {1}}, []string{"body.note"}},
		{"no words known", `{"note":"lead r-2 created","n":1}`, known{}, []string{"body.note"}},
		{"a number is no text", `{"note":"lead r-1 created","n":2}`, wordsKnown{"body.n": {0}}, []string{"body.n"}},
	} {
		if d := Compare(body(t, "k", c.live), rec, c.known); d.Structural != "" || !reflect.DeepEqual(d.Drift, c.drift) {
			t.Errorf("%s: structural %q drift %v, want drift %v", c.name, d.Structural, d.Drift, c.drift)
		}
	}
}

// A text is one word where it holds no separator: noise whole is honoured
// only there.
func TestOneWordIsATextWithNoSeparator(t *testing.T) {
	for text, want := range map[string]bool{
		"":                            true,
		"r-1":                         true,
		"2026-10-14T09:02:20.5+05:30": true,
		"/v1/leads/r-1":               true,
		"lead r-1 created":            false,
		"r-1,v2":                      false,
		" r-1":                        false,
		"r-1\n":                       false,
		`"r-1"`:                       false,
		"a=b":                         false,
	} {
		if got := OneWord(text); got != want {
			t.Errorf("OneWord(%q) = %v, want %v", text, got, want)
		}
	}
}

// A key sent more than once is one text of its values in the order sent,
// each value's '%' and '&' escaped, so the text says each value apart: its
// skeleton holds one '&' between each two values, and its words are each
// value's words, value by value. A key sent once is its value.
func TestValuesTextSaysEachValueApart(t *testing.T) {
	for _, c := range []struct {
		vs   []string
		want string
	}{
		{[]string{"lead r-1"}, "lead r-1"},
		{[]string{"a&b"}, "a&b"},
		{[]string{"lead r-1", "x"}, "lead r-1&x"},
		{[]string{"a&b", "c"}, "a%26b&c"},
		{[]string{"a", "b&c"}, "a&b%26c"},
		{[]string{"50%", "%26"}, "50%25&%2526"},
		{[]string{"", "x", ""}, "&x&"},
	} {
		got := ValuesText(c.vs)
		if got != c.want {
			t.Fatalf("ValuesText(%q) = %q; want %q", c.vs, got, c.want)
		}
		if len(c.vs) > 1 && !reflect.DeepEqual(valuesOf(got), c.vs) {
			t.Fatalf("valuesOf(%q) = %q; want %q", got, valuesOf(got), c.vs)
		}
	}
	// Two lists the joined text alone would not tell apart.
	if ValuesText([]string{"a&b", "c"}) == ValuesText([]string{"a", "b&c"}) {
		t.Fatal("two lists of other values read as one text")
	}
}

// Two runs of a key sent more than once hold the same values when they send
// the same values in any order, as many times each.
func TestSameValuesIsTheSameValuesInAnyOrder(t *testing.T) {
	for _, c := range []struct {
		a, b []string
		want bool
	}{
		{[]string{"a", "b"}, []string{"b", "a"}, true},
		{[]string{"a", "b", "a"}, []string{"a", "a", "b"}, true},
		{[]string{"a", "b"}, []string{"a", "c"}, false},
		{[]string{"a", "a", "b"}, []string{"a", "b", "b"}, false},
		{[]string{"a", "b"}, []string{"a", "b", "b"}, false},
		{[]string{"a&b", "c"}, []string{"c", "a&b"}, true},
		{[]string{"a&b", "c"}, []string{"a", "b&c"}, false},
	} {
		if got := SameValues(ValuesText(c.a), ValuesText(c.b)); got != c.want {
			t.Fatalf("SameValues(%q, %q) = %v; want %v", c.a, c.b, got, c.want)
		}
	}
}

// A value that changed at its place is one the other run sends at another
// place that changed: the values moved, and no value pairs with the one it
// changed from. A value changed in place, beside values that kept theirs, did
// not move.
func TestValuesMovedIsAChangedValueSentAtAnotherChangedPlace(t *testing.T) {
	for _, c := range []struct {
		rec, live []string
		want      bool
	}{
		{[]string{"a1", "b"}, []string{"a2", "b"}, false},
		{[]string{"a1", "b"}, []string{"b", "a2"}, true},
		{[]string{"a1", "b", "c"}, []string{"a1", "c", "x"}, true},
		{[]string{"a1", "b", "c"}, []string{"x", "y", "c"}, false},
		{[]string{"b", "a1"}, []string{"a2", "b"}, true},
		{[]string{"a", "a"}, []string{"a", "b"}, false},
		{[]string{"b", "a1"}, []string{"b", "b"}, false},
		{[]string{"a1", "b"}, []string{"a1", "b", "c"}, false},
	} {
		if got := ValuesMoved(ValuesText(c.rec), ValuesText(c.live)); got != c.want {
			t.Fatalf("ValuesMoved(%q, %q) = %v; want %v", c.rec, c.live, got, c.want)
		}
	}
}

// Compare reads a key's values sent in another order as the same leaf, and
// any other change of them as a drift; a leaf of one value, or one side's
// leaf alone of values, is read by its text. No variable binds at such a
// leaf.
func TestCompareReadsAKeysValuesInAnyOrderAsTheSame(t *testing.T) {
	shape := func(l Leaf) Shape { return Shape{Key: "k", Leaves: map[string]Leaf{l.Path: l}} }
	values := func(values bool, vs ...string) Leaf {
		return Leaf{Path: "url.query.id", Noise: "url.query.id", Type: String, Value: ValuesText(vs), Values: values}
	}
	for _, c := range []struct {
		name      string
		rec, live Leaf
		drift     bool
	}{
		{"the same values in another order", values(true, "a", "b"), values(true, "b", "a"), false},
		{"a value changed", values(true, "a", "b"), values(true, "a", "c"), true},
		{"the recording's leaf is no values", values(false, "a", "b"), values(true, "b", "a"), true},
		{"the call's leaf is no values", values(true, "a", "b"), values(false, "b", "a"), true},
	} {
		d := Compare(shape(c.live), shape(c.rec), None)
		if d.Structural != "" || (len(d.Drift) == 1) != c.drift {
			t.Fatalf("%s: drift %v, structural %q; want drift %v", c.name, d.Drift, d.Structural, c.drift)
		}
	}
	if _, ok := VarPlace(values(true, "a", "b"), false); ok {
		t.Fatal("a key's values are a variable's place")
	}
	if _, ok := VarPlace(values(false, "a"), false); !ok {
		t.Fatal("a query value is no variable's place")
	}
}
