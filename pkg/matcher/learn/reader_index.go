package learn

import (
	"hash/maphash"
	"strings"
)

// The reader's index: what the places before a call hold of its changed
// words, read lazily, per changed word, for one Decide call. A place's text
// is read the first time a changed word asks about it, in main's caches
// (DriftCache.rawTokens, spansOf: every declared form, every reading), and
// what it answers is kept for the call within its cap (readerBudgetBytes)
// and, in a set, on the set's one budget (claim), the one its tiers keep
// within, never on a budget beside it. Past either the index keeps nothing
// more: a question is answered from the text read again (the caller's, or
// the recording's, through the set's store of payloads), and dropped with
// it, as main's derived caches drop what passes theirs. So a call holds at
// once only what the set's budget lets it keep and the one text it reads,
// however many places it asks about, and it decides as a small set does:
// the budget bounds what is held, never what is read. A place that is a
// recording keeps its handle (textOf), never its text. The index reports
// unread only a recording keploy could not read.

// readerBudget, where a row sets it (>0), caps what one call's index keeps;
// 0 in keploy itself: a quarter of the set's budget (setBudget), within
// that budget, not beside it (claim), so one call's index never takes all
// that the set's words and payloads keep.
var readerBudget int64

// readerBudgetBytes is the cap of what one call's index keeps (readerBudget).
func readerBudgetBytes() int64 {
	if readerBudget > 0 {
		return readerBudget
	}
	return setBudget() / 4
}

// readerIndex is one Decide call's index: what it kept of each text asked
// about, by the text's hash, the bytes that takes (counted on the set's budget
// too where the call is a set's), and how many recordings keploy could not
// read.
type readerIndex struct {
	dc           *DriftCache
	set          *setDerived
	budget, used int64
	seed         maphash.Seed
	texts        map[uint64][]*readerEntry
	unreadTexts  int
}

// readerEntry is what the index keeps of one text: its words, its base64
// runs and its instants, each kept where it fits the budget, and then the
// text itself (counted in the budget once) to tell it from another text of
// its hash. An entry that keeps no answer keeps no byte of its text either,
// so a call that asks about many large recordings holds at once only what
// fits the budget: the caller's handle holds the text while it asks (or, of
// a recording, reads it per question), and nothing here holds it after.
type readerEntry struct {
	n                               int
	text                            string
	tokens                          []map[string]bool
	runs                            map[string]bool
	spans, extraSpans               []instant
	tokensKept, runsKept, spansKept bool
	extraKept                       bool
	// past: an answer of the text did not fit what was left of the
	// budget, which only fills, so the entry keeps none from then on: each
	// question is answered from the text read again, a word by a scan of
	// it (wordIn) that builds no list of its other words.
	past bool
}

// readerText is one text as a caller asks the index about it, and the
// index's entry for it once found. The caller hands the text, which it holds
// while it asks (text), or the recording that holds it (textOf), whose text
// is read only for a question the entry keeps no answer to, through the
// set's bounded store of payloads, and let go once answered: a place keeps
// the handle, never the text. Finding the entry hashes the text, so a caller
// asks through one handle per place: a 498 KB answer hashed per word costs
// more than reading it.
type readerText struct {
	x    *readerIndex
	text string
	// raw is the recording the text is read from (textOf); nil where the
	// caller handed the text. rawUnread: keploy could not read it.
	raw       *Raw
	rawUnread bool
	e         *readerEntry
}

// newReaderIndex is an empty index for one call, reading through dc's
// caches (nil: none).
func newReaderIndex(dc *DriftCache) *readerIndex {
	x := &readerIndex{dc: dc, budget: readerBudgetBytes(), seed: maphash.MakeSeed(), texts: map[uint64][]*readerEntry{}}
	if dc != nil {
		x.set = dc.set
	}
	return x
}

// claim counts n bytes more of what the index keeps, and reports whether
// they fit: within its cap, and, where the call is a set's, on the set's
// one budget, as a tier that lets none go keeps there (tier.keep): it first
// has the set let go of what it is past its budget (budget.letGo: the
// words, then the payloads, each read again where asked for), and past the
// budget still it counts none.
func (x *readerIndex) claim(n int64) bool {
	if x.used+n > x.budget {
		return false
	}
	if s := x.set; s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		b := s.budget
		if need := b.used + n - b.limit; need > 0 && b.letGo != nil {
			b.letGo(need)
		}
		if b.used+n > b.limit {
			return false
		}
		b.used += n
	}
	x.used += n
	return true
}

// release gives the set's budget back what the index keeps, once its call
// is over (the caller's: defer x.release()), and lets go of it: the index
// keeps nothing more, and a question asked after reads the text again.
func (x *readerIndex) release() {
	if s := x.set; s != nil && x.used > 0 {
		s.mu.Lock()
		s.budget.used -= x.used
		s.mu.Unlock()
	}
	x.used, x.budget = 0, 0
	x.texts = map[uint64][]*readerEntry{}
}

// text is s as the index holds it, read lazily; the caller holds s while it
// asks.
func (x *readerIndex) text(s string) *readerText { return &readerText{x: x, text: s} }

// textOf is r's text as the index holds it, read lazily from r (its
// payload, through the set's store, for a lazy Raw) per question the index
// keeps no answer to: the handle holds r, never its text, and the index
// keeps the text only within its budget. A recording keploy could not read
// (Raw.Read) is unread.
func (x *readerIndex) textOf(r Raw) *readerText { return &readerText{x: x, raw: &r} }

// entryOf is s's entry: the one of its hash and length that keeps s, or
// that keeps nothing yet; a new one where none does.
func (x *readerIndex) entryOf(s string) *readerEntry {
	h := maphash.String(x.seed, s)
	for _, e := range x.texts[h] {
		if e.n == len(s) && (e.text == s || e.text == "") {
			return e
		}
	}
	e := &readerEntry{n: len(s)}
	x.texts[h] = append(x.texts[h], e)
	return e
}

// read is the text, read now from its recording where the caller handed
// one, with its entry found where t has none yet; ok false where the
// recording was not read (counted unread, once per handle).
func (t *readerText) read() (string, bool) {
	s := t.text
	if t.raw != nil {
		if t.rawUnread {
			return "", false
		}
		b := t.raw.payload()
		if !b.read {
			t.rawUnread = true
			t.x.unreadTexts++
			return "", false
		}
		s = b.text
	}
	if t.e == nil {
		t.e = t.x.entryOf(s)
	}
	return s, true
}

// own is the bytes keeping s adds to e: its length, where e keeps no text
// yet.
func (e *readerEntry) own(s string) int64 {
	if e.text == "" {
		return int64(len(s))
	}
	return 0
}

// indexOnce is what one kind of question asks of the text: the answer the
// entry keeps (kept marks it kept), or the one of derives from the text read
// now. That answer is kept, with the text that tells it apart, where both
// fit what is left of the budget; past it, the answer is the caller's for
// this question alone and nothing here holds it, so the same question asked
// again reads the text again (through main's caches, which keep what fits
// theirs). ok is false only where the recording was not read.
func indexOnce[V any](t *readerText, kept func(*readerEntry) *bool, get func(*readerEntry) V, of func(string) V, size func(V) int64, keep func(*readerEntry, V)) (V, bool) {
	if t.e != nil && *kept(t.e) {
		return get(t.e), true
	}
	s, ok := t.read()
	if !ok {
		var none V
		return none, false
	}
	e, x := t.e, t.x
	if *kept(e) {
		return get(e), true
	}
	if !x.mayKeep(e, s) {
		return of(s), true
	}
	v := of(s)
	if x.claim(size(v) + e.own(s)) {
		e.text = s
		keep(e, v)
		*kept(e) = true
	} else {
		e.past = true
	}
	return v, true
}

// mayKeep reports whether an answer of s may still be kept in e: e is not
// past the budget, and s itself fits what is left of it (marking e past
// where it does not).
func (x *readerIndex) mayKeep(e *readerEntry, s string) bool {
	if !e.past && x.used+e.own(s)+headerBytes > x.budget {
		e.past = true
	}
	return !e.past
}

// tokensOf are the text's words in every declared form, by reading
// (isWord, wordByte, idByte), read once for the call where they fit the
// budget; ok false where the recording was not read.
func (t *readerText) tokensOf() ([]map[string]bool, bool) {
	return indexOnce(t, func(e *readerEntry) *bool { return &e.tokensKept }, func(e *readerEntry) []map[string]bool { return e.tokens },
		t.x.dc.rawTokens, tokensBytes, func(e *readerEntry, v []map[string]bool) { e.tokens = v })
}

// runsOf are the text's base64 runs that stand whole (wholeB64Runs), read
// once for the call where they fit the budget; ok false where the recording
// was not read.
func (t *readerText) runsOf() (map[string]bool, bool) {
	return indexOnce(t, func(e *readerEntry) *bool { return &e.runsKept }, func(e *readerEntry) map[string]bool { return e.runs },
		wholeB64Runs, wordsBytes, func(e *readerEntry, v map[string]bool) { e.runs = v })
}

// spansOf are the text's instants (instantSpans), read once for the call
// where they fit the budget; ok false where the recording was not read.
func (t *readerText) spansOf() ([]instant, bool) {
	return indexOnce(t, func(e *readerEntry) *bool { return &e.spansKept }, func(e *readerEntry) []instant { return e.spans },
		t.x.dc.spansOf, spansBytes, func(e *readerEntry, v []instant) { e.spans = v })
}

// extraSpansOf are the instants of the text's other declared forms
// (rawExtra: decoded, case-folded, its encoded runs' text), read once for
// the call where they fit the budget; ok false where the recording was not
// read.
func (t *readerText) extraSpansOf() ([]instant, bool) {
	dc := t.x.dc
	return indexOnce(t, func(e *readerEntry) *bool { return &e.extraKept }, func(e *readerEntry) []instant { return e.extraSpans },
		func(s string) []instant { return dc.spansOf(dc.rawExtra(s)) }, spansBytes, func(e *readerEntry, v []instant) { e.extraSpans = v })
}

// textNow is the text itself, for a question the index keeps no answer to
// (two places the same text, one holding another's): the caller's, the one
// the entry keeps, or read now from the recording, the caller's for this
// question alone and kept by no one here; ok false where the recording was
// not read (counted unread).
func (t *readerText) textNow() (string, bool) {
	if t.raw == nil {
		return t.text, true
	}
	if t.e != nil && t.e.text != "" {
		return t.e.text, true
	}
	return t.read()
}

// holdsWord reports whether the text holds w as a word in any of main's
// readings (isWord, wordByte, idByte) and any declared form; read is false
// where the recording was not read (held is false then).
func (t *readerText) holdsWord(w string) (held, read bool) {
	return t.wordIn(w, 0, 1, 2)
}

// wordIn reports whether the text holds w as a word in any of the readings
// rs (indices of main's readings) and any declared form: from the words the
// entry keeps (tokensOf), or, past the budget, by a scan of the text read
// now and its other declared forms (textForms, the forms main's word lists
// read), which reads no other word of it and keeps nothing, in no cache
// either. read is false where the recording was not read.
func (t *readerText) wordIn(w string, rs ...int) (held, read bool) {
	if t.e == nil || !t.e.tokensKept {
		s, ok := t.read()
		if !ok {
			return false, false
		}
		if !t.e.tokensKept && !t.x.mayKeep(t.e, s) {
			for _, f := range textForms(s) {
				for _, r := range rs {
					if heldBy(f, w, readings[r]) {
						return true, true
					}
				}
			}
			return false, true
		}
	}
	sets, ok := t.tokensOf()
	if !ok {
		return false, false
	}
	for _, r := range rs {
		if r < len(sets) && sets[r][w] {
			return true, true
		}
	}
	return false, true
}

// heldBy reports whether w is a word of s as word reads it (tokensBy(s,
// word)[w]) without reading s's other words: w stands in s, every byte of
// it continues a word, and no byte that does stands right before or after.
func heldBy(s, w string, word func(byte) bool) bool {
	if w == "" {
		return false
	}
	for i := 0; i < len(w); i++ {
		if !word(w[i]) {
			return false
		}
	}
	for off := 0; off <= len(s)-len(w); {
		i := strings.Index(s[off:], w)
		if i < 0 {
			return false
		}
		i += off
		if (i == 0 || !word(s[i-1])) && (i+len(w) == len(s) || !word(s[i+len(w)])) {
			return true
		}
		off = i + 1
	}
	return false
}

// holdsToken reports whether the text holds tok whole, as a token of its
// kind: an idByte token (any declared form), or a base64 run that stands
// whole (wholeB64Spans, the rule tokenSpans cuts it by); read as holdsWord.
func (t *readerText) holdsToken(kind tokenKind, tok string) (held, read bool) {
	if kind == tokenID {
		return t.wordIn(tok, len(readings)-1)
	}
	runs, ok := t.runsOf()
	return runs[tok], ok
}

// wholeB64Runs are s's base64 runs that stand whole (wholeB64Spans): the
// tokens holdsToken finds in s.
func wholeB64Runs(s string) map[string]bool {
	runs := map[string]bool{}
	for _, sp := range wholeB64Spans(s) {
		runs[s[sp[0]:sp[1]]] = true
	}
	return runs
}

// wholeB64Spans are s's base64 runs that stand whole, as s[start:end] each,
// in order: a run of the base64 alphabets, standard or URL-safe, that no
// byte of them comes right before, with all the padding ('=') after it,
// that no byte of them follows. It is the one rule the reader cuts a base64
// token by (tokenSpans) and finds one in a text by (holdsToken), so every
// token it cuts is held by its own text: a run a byte of the alphabets
// follows past its padding (`tok=` of `tok=ab+cd/ef==`) is none, and its
// words are judged one by one.
func wholeB64Spans(s string) [][2]int {
	var out [][2]int
	for i := 0; i < len(s); {
		if !isB64Byte(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && isB64Byte(s[j]) {
			j++
		}
		k := j
		for k < len(s) && s[k] == '=' {
			k++
		}
		if k == len(s) || !isB64Byte(s[k]) {
			out = append(out, [2]int{i, k})
		}
		i = k
	}
	return out
}

// holdsMoment reports whether an instant of the text names m by moment
// (byMoment), and whether one does so at a fine cut, in the text or any of
// its declared forms, the forms main reads an instant in (holdsExact(text)
// || holdsExact(rawExtra(text))) and holdsWord reads a word in; read as
// holdsWord.
func (t *readerText) holdsMoment(m moment) (held, fine, read bool) {
	for _, of := range []func() ([]instant, bool){t.spansOf, t.extraSpansOf} {
		spans, ok := of()
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
		if fine {
			break
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

// unread reports whether the call asked about a recording keploy could not
// read (Raw.Read): what the budget does not keep is read again, never
// unread.
func (x *readerIndex) unread() bool { return x.unreadTexts > 0 }
