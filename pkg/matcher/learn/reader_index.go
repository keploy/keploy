package learn

import (
	"hash/maphash"
	"strings"
)

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

// readerIndex is one Decide call's index: what it kept of each text asked
// about, by the text's hash, the bytes that takes, and how many reads it
// left undone past the budget.
type readerIndex struct {
	dc           *DriftCache
	budget, used int64
	seed         maphash.Seed
	texts        map[uint64][]*readerEntry
	unreadTexts  int
}

// readerEntry is what the index keeps of one text: its words and instants,
// each once read within the budget, and then the text itself (counted in
// the budget) to tell it from another text of its hash. A text that lay past
// the budget keeps no byte of itself, only that it did, so a call that asks
// about many large recordings holds at once only what fits the budget: the
// caller's handle holds the text while it asks, and nothing here holds it
// after. A later text of the same hash and length is read as that one
// (unread, where it lay past: fail closed).
type readerEntry struct {
	n                       int
	text                    string
	tokens                  []map[string]bool
	spans                   []instant
	tokensAsked, spansAsked bool
}

// readerText is one text as a caller asks the index about it: the text,
// which the caller holds while it asks, and the index's entry for it.
// Finding it hashes the text, so a caller asks text once per place and
// keeps the handle: a 498 KB answer hashed per word costs more than reading
// it.
type readerText struct {
	x    *readerIndex
	text string
	e    *readerEntry
}

// newReaderIndex is an empty index for one call, reading through dc's
// caches (nil: none).
func newReaderIndex(dc *DriftCache) *readerIndex {
	return &readerIndex{dc: dc, budget: readerBudgetBytes(), seed: maphash.MakeSeed(), texts: map[uint64][]*readerEntry{}}
}

// text is s as the index holds it, read lazily.
func (x *readerIndex) text(s string) *readerText {
	h := maphash.String(x.seed, s)
	for _, e := range x.texts[h] {
		if e.n == len(s) && (e.text == s || e.text == "") {
			return &readerText{x: x, text: s, e: e}
		}
	}
	e := &readerEntry{n: len(s)}
	x.texts[h] = append(x.texts[h], e)
	return &readerText{x: x, text: s, e: e}
}

// fits reports whether n more bytes fit the budget, counting a read that
// does not as unread.
func (x *readerIndex) fits(n int64) bool {
	if x.used+n > x.budget {
		x.unreadTexts++
		return false
	}
	return true
}

// keep counts n bytes read of t within the budget, the text's own among
// them, and keeps the text to tell it by.
func (t *readerText) keep(n int64) {
	t.x.used += n
	t.e.text = t.text
}

// tokensOf are the text's words in every declared form, by reading
// (isWord, wordByte, idByte), read once for the call; ok false where they
// lie past the budget. A text whose copy alone passes what is left is not
// read at all.
func (t *readerText) tokensOf() ([]map[string]bool, bool) {
	e := t.e
	if !e.tokensAsked {
		e.tokensAsked = true
		x := t.x
		if !x.fits(int64(len(t.text)) + headerBytes) {
			return nil, false
		}
		tok := x.dc.rawTokens(t.text)
		n := tokensBytes(tok) + int64(len(t.text))
		if !x.fits(n) {
			return nil, false
		}
		t.keep(n)
		e.tokens = tok
		if e.tokens == nil {
			e.tokens = []map[string]bool{}
		}
	}
	return e.tokens, e.tokens != nil
}

// spansOf are the text's instants (instantSpans), read once for the call;
// ok false where they lie past the budget.
func (t *readerText) spansOf() ([]instant, bool) {
	e := t.e
	if !e.spansAsked {
		e.spansAsked = true
		x := t.x
		if !x.fits(int64(len(t.text)) + headerBytes) {
			return nil, false
		}
		s := x.dc.spansOf(t.text)
		n := spansBytes(s) + int64(len(t.text))
		if !x.fits(n) {
			return nil, false
		}
		t.keep(n)
		e.spans = s
		if e.spans == nil {
			e.spans = []instant{}
		}
	}
	return e.spans, e.spans != nil
}

// holdsWord reports whether the text holds w as a word in any of main's
// readings (isWord, wordByte, idByte) and any declared form; read is false
// where the text lay past the budget (held is false then).
func (t *readerText) holdsWord(w string) (held, read bool) {
	sets, ok := t.tokensOf()
	if !ok {
		return false, false
	}
	for _, set := range sets {
		if set[w] {
			return true, true
		}
	}
	return false, true
}

// holdsToken reports whether the text holds tok whole, as a token of its
// kind: an idByte token (any declared form), or a base64 run, no byte of
// the base64 alphabets or its padding on either side; read as holdsWord.
func (t *readerText) holdsToken(kind tokenKind, tok string) (held, read bool) {
	if kind == tokenID {
		sets, ok := t.tokensOf()
		if !ok {
			return false, false
		}
		if len(sets) == 0 {
			return false, true
		}
		return sets[len(sets)-1][tok], true
	}
	if tok == "" {
		return false, true
	}
	text := t.text
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

// holdsMoment reports whether an instant of the text names m by moment
// (byMoment), and whether one does so at a fine cut; read as holdsWord.
func (t *readerText) holdsMoment(m moment) (held, fine, read bool) {
	spans, ok := t.spansOf()
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

// holdsWord, holdsToken and holdsMoment ask text once (readerText's); a
// caller asking one place about many words keeps x.text(s) instead.
func (x *readerIndex) holdsWord(text, w string) (held, read bool) { return x.text(text).holdsWord(w) }

func (x *readerIndex) holdsToken(text string, kind tokenKind, tok string) (held, read bool) {
	return x.text(text).holdsToken(kind, tok)
}

func (x *readerIndex) holdsMoment(text string, m moment) (held, fine, read bool) {
	return x.text(text).holdsMoment(m)
}

// unread reports whether the call's index left a text unread: past its
// budget.
func (x *readerIndex) unread() bool { return x.unreadTexts > 0 }
