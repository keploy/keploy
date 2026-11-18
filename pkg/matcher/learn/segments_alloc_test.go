package learn

import (
	"fmt"
	"strings"
	"testing"
)

var segmentsSink any

// tokensBy and wordsBy read each stretch of a text once and keep no list of
// the stretches (segmentsOf): a text of thousands of stretches and one word
// costs tokensBy the allocations of a text of a few, and wordsBy those of
// its list of words alone. The list of stretches tokensBy built of each text
// it read was the replay agent's largest allocation. Counted, not timed.
func TestTokensByAndWordsByKeepNoListOfATextsStretches(t *testing.T) {
	const n = 4096
	few, many := strings.Repeat("ab ", 4), strings.Repeat("ab ", n)
	tokens := func(s string) float64 {
		return testing.AllocsPerRun(20, func() { segmentsSink = tokensBy(s, isWord) })
	}
	if f, m := tokens(few), tokens(many); m != f {
		t.Errorf("tokensBy made %v allocations of a text of %d stretches and one word, %v of one of 8: want as many", m, 2*n, f)
	}
	// What a list of n words costs, built as wordsBy builds its own.
	list := testing.AllocsPerRun(20, func() {
		var out []string
		for i := 0; i < n; i++ {
			out = append(out, many[3*i:3*i+2])
		}
		segmentsSink = out
	})
	if w := testing.AllocsPerRun(20, func() { segmentsSink = wordsBy(many, isWord) }); w > list {
		t.Errorf("wordsBy made %v allocations of a text of %d words, where the list of its words alone takes %v", w, n, list)
	}
	// What each reads is as before: every stretch of a word, in order.
	s := "a-1 b_2:c3 ab ab ✓x"
	if got, want := fmt.Sprint(wordsBy(s, idByte)), "[a-1 b_2 c3 ab ab ✓x]"; got != want {
		t.Errorf("wordsBy(%q) = %s, want %s", s, got, want)
	}
	if got, want := len(tokensBy(s, isWord)), len(map[string]bool{"a": true, "1": true, "b": true, "2": true, "c3": true, "ab": true, "x": true}); got != want {
		t.Errorf("tokensBy(%q) holds %d words (%v), want %d", s, got, tokensBy(s, isWord), want)
	}
}

// A reader that stops at a stretch is handed no more (segmentsOf ends where
// its reader does), and the stretches read before it are segmentsBy's.
func TestSegmentsOfStopsWhereItsReaderDoes(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("segmentsOf went on after its reader stopped: %v", r)
		}
	}()
	s := "ab, cd. ef"
	var got []segment
	for seg := range segmentsOf(s, isWord) {
		got = append(got, seg)
		if len(got) == 3 {
			break
		}
	}
	if want := segmentsBy(s, isWord)[:3]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("segmentsOf(%q) read %v before its reader stopped, want %v", s, got, want)
	}
}
