package learn

import "strings"

// The reader's index (DESIGN §1, M5): what the places before a call hold
// of its changed words, read lazily, per changed word, for one Decide call.
// A place's text is read the first time a changed word asks about it, in
// main's caches (DriftCache.rawTokens, spansOf: every declared form, every
// reading), and kept for the call within readerBudget bytes. A text past
// the budget is not read: the index says so (unread), and the caller
// refuses every change of the call (New 1), never binding a source it found
// before the budget ran out.

// readerBudget, where a row sets it (>0), is the bytes one call's index
// keeps; 0 in keploy itself: a quarter of the set's budget (setBudget,
// which is a quarter of the agent's memory limit, so at most a sixteenth of
// it).
var readerBudget int64

// readerBudgetBytes is the bytes one call's index keeps (readerBudget).
func readerBudgetBytes() int64 {
	if readerBudget > 0 {
		return readerBudget
	}
	return setBudget() / 4
}

// readerIndex is one Decide call's index: what it has read of each text,
// by the text (a nil entry: the text lay past the budget), the bytes that
// takes, and how many texts it left unread.
type readerIndex struct {
	dc           *DriftCache
	budget, used int64
	tokens       map[string][]map[string]bool
	spans        map[string][]instant
	unreadTexts  int
}

// newReaderIndex is an empty index for one call, reading through dc's
// caches (nil: none).
func newReaderIndex(dc *DriftCache) *readerIndex {
	return &readerIndex{dc: dc, budget: readerBudgetBytes(), tokens: map[string][]map[string]bool{}, spans: map[string][]instant{}}
}

// fits reports whether n more bytes fit the budget, counting a text that
// does not as unread.
func (x *readerIndex) fits(n int64) bool {
	if x.used+n > x.budget {
		x.unreadTexts++
		return false
	}
	return true
}

// tokensOf are text's words in every declared form, by reading (isWord,
// wordByte, idByte), read once for the call; ok false where they lie past
// the budget. A text whose copy alone passes what is left is not read at
// all.
func (x *readerIndex) tokensOf(text string) ([]map[string]bool, bool) {
	if t, ok := x.tokens[text]; ok {
		return t, t != nil
	}
	if !x.fits(int64(len(text)) + headerBytes) {
		x.tokens[text] = nil
		return nil, false
	}
	t := x.dc.rawTokens(text)
	n := tokensBytes(t) + int64(len(text))
	if !x.fits(n) {
		x.tokens[text] = nil
		return nil, false
	}
	x.used += n
	x.tokens[text] = t
	return t, true
}

// spansOf are text's instants (instantSpans), read once for the call; ok
// false where they lie past the budget.
func (x *readerIndex) spansOf(text string) ([]instant, bool) {
	if s, ok := x.spans[text]; ok {
		return s, s != nil
	}
	if !x.fits(int64(len(text)) + headerBytes) {
		x.spans[text] = nil
		return nil, false
	}
	s := x.dc.spansOf(text)
	n := spansBytes(s) + int64(len(text))
	if !x.fits(n) {
		x.spans[text] = nil
		return nil, false
	}
	x.used += n
	if s == nil {
		s = []instant{}
	}
	x.spans[text] = s
	return s, true
}

// holdsWord reports whether text holds w as a word in any of main's
// readings (isWord, wordByte, idByte) and any declared form; read is false
// where text lay past the budget (held is false then).
func (x *readerIndex) holdsWord(text, w string) (held, read bool) {
	t, ok := x.tokensOf(text)
	if !ok {
		return false, false
	}
	for _, set := range t {
		if set[w] {
			return true, true
		}
	}
	return false, true
}

// holdsToken reports whether text holds tok whole, as a token of its kind:
// an idByte token (any declared form), or a base64 run, no byte of the
// base64 alphabets or its padding on either side; read as holdsWord.
func (x *readerIndex) holdsToken(text string, kind tokenKind, tok string) (held, read bool) {
	if kind == tokenID {
		t, ok := x.tokensOf(text)
		if !ok {
			return false, false
		}
		return t[len(t)-1][tok], true
	}
	if tok == "" {
		return false, true
	}
	for i := 0; i+len(tok) <= len(text); {
		j := indexFrom(text, tok, i)
		if j < 0 {
			break
		}
		end := j + len(tok)
		if (j == 0 || !isB64Byte(text[j-1])) && (end == len(text) || !isB64Byte(text[end]) && text[end] != '=') {
			return true, true
		}
		i = j + 1
	}
	return false, true
}

// indexFrom is the index of the first v in s at or after i, -1 where none.
func indexFrom(s, v string, i int) int {
	if j := strings.Index(s[i:], v); j >= 0 {
		return i + j
	}
	return -1
}

// holdsMoment reports whether an instant of text names m by moment
// (byMoment), and whether one does so at a fine cut; read as holdsWord.
func (x *readerIndex) holdsMoment(text string, m moment) (held, fine, read bool) {
	spans, ok := x.spansOf(text)
	if !ok {
		return false, false, false
	}
	for _, sp := range spans {
		if !sp.keyed {
			continue
		}
		if h, f := byMoment(sp.moment(), m); h {
			held, fine = true, fine || f
		}
	}
	return held, fine, true
}

// unread reports whether the call's index left a text unread: past its
// budget.
func (x *readerIndex) unread() bool { return x.unreadTexts > 0 }
