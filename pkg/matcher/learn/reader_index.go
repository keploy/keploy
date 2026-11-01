package learn

import "hash/maphash"

// The reader's index (DESIGN §1, M5): what the places before a call hold
// of its changed words, read lazily, per changed word, for one Decide call.
// A place's text is read the first time a changed word asks about it, in
// main's caches (DriftCache.rawTokens, spansOf: every declared form, every
// reading), and kept for the call within readerBudget bytes. A text past
// the budget is not read: the index says so (unread), and the caller
// refuses every change of the call (New 1), never binding a source it found
// before the budget ran out. A place that is a recording keeps its handle
// (textOf), never its text: the text is read from the set's store of
// payloads only for a question the index has not answered yet.

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
// left undone: past the budget, or of a recording keploy could not read.
type readerIndex struct {
	dc           *DriftCache
	budget, used int64
	seed         maphash.Seed
	texts        map[uint64][]*readerEntry
	unreadTexts  int
}

// readerEntry is what the index keeps of one text: its words, its base64
// runs and its instants, each once read within the budget, and then the
// text itself (counted in the budget once) to tell it from another text of
// its hash. A text that lay past the budget keeps no byte of itself, only
// that it did, so a call that asks about many large recordings holds at once
// only what fits the budget: the caller's handle holds the text while it
// asks (or, of a recording, reads it per question), and nothing here holds
// it after. A later text of the same hash and length is read as that one
// (unread, where it lay past: fail closed).
type readerEntry struct {
	n                                  int
	text                               string
	tokens                             []map[string]bool
	runs                               map[string]bool
	spans                              []instant
	tokensAsked, runsAsked, spansAsked bool
	// past: the text alone did not fit what was left of the budget.
	past bool
}

// readerText is one text as a caller asks the index about it, and the
// index's entry for it once found. The caller hands the text, which it holds
// while it asks (text), or the recording that holds it (textOf), whose text
// is read only for a question the entry has not answered yet, through the
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
	return &readerIndex{dc: dc, budget: readerBudgetBytes(), seed: maphash.MakeSeed(), texts: map[uint64][]*readerEntry{}}
}

// text is s as the index holds it, read lazily; the caller holds s while it
// asks.
func (x *readerIndex) text(s string) *readerText { return &readerText{x: x, text: s} }

// textOf is r's text as the index holds it, read lazily from r (its
// payload, through the set's store, for a lazy Raw) per question the index
// has not answered: the handle holds r, never its text, and the index keeps
// the text only within its budget. A recording keploy could not read
// (Raw.Read) is unread.
func (x *readerIndex) textOf(r Raw) *readerText { return &readerText{x: x, raw: &r} }

// entryOf is s's entry: the one of its hash and length that keeps s, or
// that lay past the budget; a new one where none does.
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
// recording was not read (counted unread).
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

// fits reports whether n more bytes fit the budget, counting a read that
// does not as unread.
func (x *readerIndex) fits(n int64) bool {
	if x.used+n > x.budget {
		x.unreadTexts++
		return false
	}
	return true
}

// own is the bytes keeping s adds to e: its length, where e keeps no text
// yet.
func (e *readerEntry) own(s string) int64 {
	if e.text == "" {
		return int64(len(s))
	}
	return 0
}

// indexOnce reads, once for the call, what one kind of question asks of the
// text (asked marks it asked): a text whose copy alone passes what is left
// of the budget is not read (past: the budget only fills, so it is not read
// again either, nor its recording loaded again); what of derives, and the
// text kept to tell it by, are kept where they fit (true), and nothing is
// kept where they do not (false, counted unread).
func indexOnce[V any](t *readerText, asked func(*readerEntry) *bool, of func(string) V, size func(V) int64, set func(*readerEntry, V)) bool {
	if t.e != nil && t.e.past {
		t.x.unreadTexts++
		return false
	}
	s, ok := t.read()
	if !ok {
		return false
	}
	e, x := t.e, t.x
	if *asked(e) {
		return true
	}
	*asked(e) = true
	if !x.fits(e.own(s) + headerBytes) {
		e.past = true
		return false
	}
	v := of(s)
	n := size(v) + e.own(s)
	if !x.fits(n) {
		return false
	}
	x.used += n
	e.text = s
	set(e, v)
	return true
}

// tokensOf are the text's words in every declared form, by reading
// (isWord, wordByte, idByte), read once for the call; ok false where they
// lie past the budget.
func (t *readerText) tokensOf() ([]map[string]bool, bool) {
	if t.e == nil || !t.e.tokensAsked {
		if !indexOnce(t, func(e *readerEntry) *bool { return &e.tokensAsked }, t.x.dc.rawTokens, tokensBytes, func(e *readerEntry, v []map[string]bool) {
			e.tokens = v
			if e.tokens == nil {
				e.tokens = []map[string]bool{}
			}
		}) {
			return nil, false
		}
	}
	return t.e.tokens, t.e.tokens != nil
}

// runsOf are the text's base64 runs that stand whole (wholeB64Runs), read once
// for the call; ok false where they lie past the budget.
func (t *readerText) runsOf() (map[string]bool, bool) {
	if t.e == nil || !t.e.runsAsked {
		if !indexOnce(t, func(e *readerEntry) *bool { return &e.runsAsked }, wholeB64Runs, wordsBytes, func(e *readerEntry, v map[string]bool) { e.runs = v }) {
			return nil, false
		}
	}
	return t.e.runs, t.e.runs != nil
}

// spansOf are the text's instants (instantSpans), read once for the call;
// ok false where they lie past the budget.
func (t *readerText) spansOf() ([]instant, bool) {
	if t.e == nil || !t.e.spansAsked {
		if !indexOnce(t, func(e *readerEntry) *bool { return &e.spansAsked }, t.x.dc.spansOf, spansBytes, func(e *readerEntry, v []instant) {
			e.spans = v
			if e.spans == nil {
				e.spans = []instant{}
			}
		}) {
			return nil, false
		}
	}
	return t.e.spans, t.e.spans != nil
}

// textNow is the text itself, for a question the index keeps no answer to
// (two places the same text, one holding another's): the caller's, the one
// the entry keeps, or read now from the recording and kept by no one here;
// ok false where the recording was not read, or where its text does not fit
// what is left of the budget (counted unread).
func (t *readerText) textNow() (string, bool) {
	if t.raw == nil {
		return t.text, true
	}
	if t.e != nil && t.e.text != "" {
		return t.e.text, true
	}
	if t.e != nil && t.e.past {
		t.x.unreadTexts++
		return "", false
	}
	s, ok := t.read()
	if !ok || t.e.text == "" && !t.x.fits(int64(len(s))) {
		return "", false
	}
	return s, true
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
	// A base64 token (tokenSpans cuts it as nextB64Run does) is held
	// exactly where it is one of the text's whole runs.
	runs, ok := t.runsOf()
	return runs[tok], ok
}

// wholeB64Runs are s's base64 runs whole, each with all the padding after it,
// that no byte of the base64 alphabets follows: the tokens holdsToken finds
// in s, no byte of the alphabets or padding on either side.
func wholeB64Runs(s string) map[string]bool {
	runs := map[string]bool{}
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
			runs[s[i:k]] = true
		}
		i = k
	}
	return runs
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
// budget, or a recording keploy could not read.
func (x *readerIndex) unread() bool { return x.unreadTexts > 0 }
