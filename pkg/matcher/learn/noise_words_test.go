package learn

import (
	"reflect"
	"testing"
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
