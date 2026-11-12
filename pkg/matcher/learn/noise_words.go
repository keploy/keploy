package learn

import (
	"slices"
	"sort"
	"strconv"
	"strings"
)

// A text learned as noise leaves only the words that changed unchecked; the
// rest of it is still compared (founder, 2026-10-22: "learn only the words
// that changed, and keep checking the rest").
//
// A text's words are what lies between its separators: ASCII whitespace and
// the delimiters " ' { } [ ] ( ) , ; & < > | \ = ? #. A word keeps its ':',
// '/', '+', '-', '.' and '_', so an instant (2026-10-14T09:02:20.5+05:30),
// a path or a URL, a UUID and a dotted or dashed id are each one word. Two
// runs of a text have the same skeleton when they hold the same number of
// words with the same separators between them, in order.
//
// The learned noise of such a text is one pattern (NoiseWordsPattern) naming
// the words, counted from 0, that changed between the recording and record's
// full round; a replay admits a value with the recording's skeleton whose
// other words are the recording's (WordsAdmit). The pattern is a regular
// expression that matches only itself, so a reader that takes learned noise
// as keploy.yml's (a field is noise where its value matches a pattern)
// compares the field: it never leaves the whole text unchecked.

// noiseWordsPrefix and noiseWordsSuffix wrap the word numbers of a learned
// words pattern.
const (
	noiseWordsPrefix = "^keploy:words "
	noiseWordsSuffix = "$"
)

// isWordSep reports whether b separates two words of a text.
func isWordSep(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\v', '\f',
		'"', '\'', '{', '}', '[', ']', '(', ')', ',', ';', '&', '<', '>', '|', '\\', '=', '?', '#':
		return true
	}
	return false
}

// splitWords splits text into its words and the separators between them:
// len(words) == len(seps)+1, each separator a maximal run of separator bytes,
// so only the first and the last word may be empty.
func splitWords(text string) (words, seps []string) {
	start := 0
	for i := 0; i < len(text); {
		if !isWordSep(text[i]) {
			i++
			continue
		}
		j := i
		for j < len(text) && isWordSep(text[j]) {
			j++
		}
		words, seps = append(words, text[start:i]), append(seps, text[i:j])
		start, i = j, j
	}
	return append(words, text[start:]), seps
}

// NoiseWords says which words of a text, recorded as rec and seen in record's
// full round as live, changed: their numbers, counted from 0, in order.
// whole is true where the text is one word in both runs (no separator), which
// is noise whole, as a value that is no text is. ok is false where the two
// runs' skeletons differ (a separator moved, a word came or went): then no
// word's place in the text can be pinned, and keploy checks the text whole
// rather than leave any of it unchecked. ok is false too where nothing
// changed.
func NoiseWords(rec, live string) (words []int, whole, ok bool) {
	rw, rs := splitWords(rec)
	lw, ls := splitWords(live)
	if len(rs) != len(ls) {
		return nil, false, false
	}
	for i := range rs {
		if rs[i] != ls[i] {
			return nil, false, false
		}
	}
	for i := range rw {
		if rw[i] != lw[i] {
			words = append(words, i)
		}
	}
	if len(words) == 0 {
		return nil, false, false
	}
	return words, len(rw) == 1, true
}

// OneWord reports whether text is one word: it holds no separator
// (isWordSep). Keploy learns a text of more than one word by its words
// (NoiseWords), so noise whole at such a text is an older keploy's, and no
// longer leaves it unchecked: noise whole is honoured only at a text of one
// word (founder, 2026-11-03). Elsewhere the text is compared, and a change
// in it asks for the test to be recorded again.
func OneWord(text string) bool {
	for i := 0; i < len(text); i++ {
		if isWordSep(text[i]) {
			return false
		}
	}
	return true
}

// NoiseWordsPattern is the learned noise pattern that leaves words of a text
// (their numbers, counted from 0, in order: NoiseWords) unchecked, and
// nothing else: "^keploy:words 1,3$".
func NoiseWordsPattern(words []int) string {
	var b strings.Builder
	b.WriteString(noiseWordsPrefix)
	for i, w := range words {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(w))
	}
	b.WriteString(noiseWordsSuffix)
	return b.String()
}

// UncheckedWords reads learned noise patterns of one place: the words they
// leave unchecked, and true, where they are one words pattern
// (NoiseWordsPattern); false otherwise (none, which is the place's noise
// whole, or patterns of another kind). A words pattern that does not read
// back (no number, one out of order or repeated, one that is not a plain
// decimal) leaves no word unchecked: it is a words pattern, and its words
// are none, so the place is compared whole.
func UncheckedWords(patterns []string) ([]int, bool) {
	if len(patterns) != 1 || !strings.HasPrefix(patterns[0], noiseWordsPrefix) || !strings.HasSuffix(patterns[0], noiseWordsSuffix) {
		return nil, false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(patterns[0], noiseWordsPrefix), noiseWordsSuffix)
	var words []int
	for _, f := range strings.Split(body, ",") {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 || strconv.Itoa(n) != f || len(words) > 0 && n <= words[len(words)-1] {
			return nil, true
		}
		words = append(words, n)
	}
	return words, true
}

// IsNoiseWords reports whether learned noise patterns of one place are a
// words pattern (UncheckedWords): the place is not noise whole.
func IsNoiseWords(patterns []string) bool {
	_, ok := UncheckedWords(patterns)
	return ok
}

// WordsAdmit reports whether live, a replay's text, is rec, its recording,
// but for the words learned unchecked (UncheckedWords): the same skeleton,
// and every other word as recorded. A word number past the recording's last
// word admits nothing, and no words admit nothing.
func WordsAdmit(rec, live string, words []int) bool {
	if len(words) == 0 {
		return false
	}
	rw, rs := splitWords(rec)
	lw, ls := splitWords(live)
	if len(rs) != len(ls) || words[len(words)-1] >= len(rw) {
		return false
	}
	for i := range rs {
		if rs[i] != ls[i] {
			return false
		}
	}
	unchecked := make(map[int]bool, len(words))
	for _, w := range words {
		unchecked[w] = true
	}
	for i := range rw {
		if !unchecked[i] && rw[i] != lw[i] {
			return false
		}
	}
	return true
}

// MergeNoise is the learned noise of one place two learnings each learned
// noise at: existing, and detected. Two words patterns leave unchecked the
// words either does, their union: both name words of the place's one
// recorded text, and the learn step learns words only where a run kept that
// text's skeleton (NoiseWords), so a word's number names the same word in
// both. A words pattern detected where existing is noise whole, or noise of
// another kind, takes its place: the learn step learns words only at a text
// of more than one word (OneWord), where keploy no longer honours noise
// whole, so that noise is an older keploy's, and the text is read by the
// words that changed now. Anything else keeps existing (learned noise is
// monotonic): a words pattern stays as it is beside noise whole or of
// another kind, and beside one that does not read back (UncheckedWords),
// which unchecks no word.
func MergeNoise(existing, detected []string) []string {
	ew, existingWords := UncheckedWords(existing)
	if _, detectedWords := UncheckedWords(detected); !existingWords && detectedWords {
		out := make([]string, len(detected))
		copy(out, detected)
		return out
	}
	if len(ew) == 0 {
		out := make([]string, len(existing))
		copy(out, existing)
		return out
	}
	dw, _ := UncheckedWords(detected)
	words := append(append(make([]int, 0, len(ew)+len(dw)), ew...), dw...)
	sort.Ints(words)
	return []string{NoiseWordsPattern(slices.Compact(words))}
}

// WordsKnown is a Known that learned some texts by their words (NoiseWords).
// Words says which words the noise learned at path leaves unchecked
// (UncheckedWords), and true where it learned the text there so. Such a
// text is not Excused: Compare reads its leaf as the same where the live
// text is the recorded one but for those words (WordsAdmit), and as drifted
// otherwise, so the learn step reads the rest of the text as it reads any
// value.
type WordsKnown interface {
	Words(l Leaf) ([]int, bool)
}

// WordsOf is known's Words at the leaf l where it is a WordsKnown; none
// otherwise.
func WordsOf(known Known, l Leaf) ([]int, bool) {
	if wk, ok := known.(WordsKnown); ok {
		return wk.Words(l)
	}
	return nil, false
}

// wordsSame reports whether known learned the text at the recorded leaf r
// by its words and the live leaf l's text is r's but for those words.
func wordsSame(known Known, l, r Leaf) bool {
	if r.Type != String {
		return false
	}
	words, _ := WordsOf(known, r)
	return WordsAdmit(r.Value, l.Value, words)
}
