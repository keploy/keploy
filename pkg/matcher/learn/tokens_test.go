package learn

import (
	"math/rand"
	"strings"
	"testing"
)

// firstTokens says, for each word, what tokenIndex says of it text by text:
// where the first text that holds it as a token holds it, after the lengths
// of the texts before (one byte between each), or past them all. Checked on
// texts of letters and digits, '-', '_', bytes beyond ASCII and other
// separators, with words that are whole runs, runs with '-' or '_' inside
// or at an end, words beyond ASCII, parts of longer runs, words no text
// holds, the same word twice and "".
func TestFirstTokens_IsTokenIndexOfEachWord(t *testing.T) {
	parts := []string{"ab", "a", "b", "A7", "9", "-", "_", " ", ".", "/", "%2F", "x-a", "a-b", "_a", "-a-", "订", "单A7", "é"}
	r := rand.New(rand.NewSource(42))
	text := func() string {
		var b strings.Builder
		for n := r.Intn(14); n >= 0; n-- {
			b.WriteString(parts[r.Intn(len(parts))])
		}
		return b.String()
	}
	want := func(texts []string, w string) int {
		off := 0
		for _, s := range texts {
			if i := tokenIndex(s, w); i >= 0 {
				return off + i
			}
			off += len(s) + 1
		}
		return off
	}
	for round := 0; round < 3000; round++ {
		texts := make([]string, 1+r.Intn(3))
		for i := range texts {
			texts[i] = text()
		}
		var words []string
		for _, s := range texts {
			for _, word := range readings {
				words = append(words, wordsBy(s, word)...)
			}
			if len(s) > 0 {
				i := r.Intn(len(s))
				words = append(words, s[i:i+r.Intn(len(s)-i)+1])
			}
		}
		words = append(words, text(), "", "ab", "ab", "-a", "a_", "订")
		got := firstTokens(texts, words)
		for _, w := range words {
			if p, ok := got[w]; !ok || p != want(texts, w) {
				t.Fatalf("texts %q: word %q at %d (listed: %v); tokenIndex text by text says %d", texts, w, p, ok, want(texts, w))
			}
		}
		if len(got) > len(words) {
			t.Fatalf("texts %q: %d places for %d words", texts, len(got), len(words))
		}
	}
}
