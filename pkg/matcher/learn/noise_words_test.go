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
// are the recording's, and nothing else.
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

func (k wordsKnown) Words(path string) ([]int, bool) {
	w, ok := k[path]
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
