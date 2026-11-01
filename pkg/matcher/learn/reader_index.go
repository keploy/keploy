package learn

import (
	"encoding/json"
	"hash/maphash"
	"io"
	"slices"
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
	n                int
	text             string
	tokens           answer[[]map[string]bool]
	runs             answer[map[string]bool]
	spans, formSpans answer[[]instant]
	fields           answer[fieldWords]
	// past: an answer of the text did not fit what was left of the
	// budget, which only fills, so the entry keeps none from then on: each
	// question is answered from the text read again, a word by a scan of
	// it (wordIn) that builds no list of its other words.
	past bool
}

// answer is one kind of answer an entry keeps: v, once kept (ok).
type answer[V any] struct {
	v  V
	ok bool
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
// one, with that recording's fields (none for a handed text), and with its
// entry found where t has none yet; ok false where the recording was not
// read (counted unread, once per handle).
func (t *readerText) read() (string, []rawField, bool) {
	if t.raw == nil {
		if t.e == nil {
			t.e = t.x.entryOf(t.text)
		}
		return t.text, nil, true
	}
	if t.rawUnread {
		return "", nil, false
	}
	b := t.raw.payload()
	if !b.read {
		t.rawUnread = true
		t.x.unreadTexts++
		return "", nil, false
	}
	if t.e == nil {
		t.e = t.x.entryOf(b.text)
	}
	return b.text, b.fields, true
}

// own is the bytes keeping s adds to e: its length, where e keeps no text
// yet.
func (e *readerEntry) own(s string) int64 {
	if e.text == "" {
		return int64(len(s))
	}
	return 0
}

// indexOnce is what one kind of question (slot) asks of the text: the
// answer the entry keeps, or the one of builds from the text read now (and
// its recording's fields). That answer is kept, with the text that tells it
// apart, where both fit what is left of the budget. Past it, scan (where
// given) answers the caller's one question from the text read now, and
// builds no answer (scanned true); else the answer is the caller's for
// this question alone. Nothing here holds either, so the same question
// asked again reads the text again (through main's caches, which keep what
// fits theirs). The text is read once per question. ok is false only
// where the recording was not read.
func indexOnce[V any](t *readerText, slot func(*readerEntry) *answer[V], of func(string, []rawField) V, size func(V) int64, scan func(string, []rawField)) (v V, scanned, ok bool) {
	if t.e != nil && slot(t.e).ok {
		return slot(t.e).v, false, true
	}
	s, fields, ok := t.read()
	if !ok {
		return v, false, false
	}
	e, x, a := t.e, t.x, slot(t.e)
	if a.ok {
		return a.v, false, true
	}
	if !x.mayKeep(e, s) {
		if scan != nil {
			scan(s, fields)
			return v, true, true
		}
		return of(s, fields), false, true
	}
	v = of(s, fields)
	if x.claim(size(v) + e.own(s)) {
		e.text = s
		a.v, a.ok = v, true
	} else {
		e.past = true
	}
	return v, false, true
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

// tokens are the text's words in every declared form, by reading (isWord,
// wordByte, idByte), read once for the call where they fit the budget, scan
// answering past it (indexOnce).
func (t *readerText) tokens(scan func(string, []rawField)) ([]map[string]bool, bool, bool) {
	return indexOnce(t, func(e *readerEntry) *answer[[]map[string]bool] { return &e.tokens },
		func(s string, _ []rawField) []map[string]bool { return t.x.dc.rawTokens(s) }, tokensBytes, scan)
}

// spansOf are the text's instants (instantSpans), read once for the call
// where they fit the budget; ok false where the recording was not read.
func (t *readerText) spansOf() ([]instant, bool) {
	v, _, ok := indexOnce(t, func(e *readerEntry) *answer[[]instant] { return &e.spans }, func(s string, _ []rawField) []instant { return t.x.dc.spansOf(s) }, spansBytes, nil)
	return v, ok
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
	s, _, ok := t.read()
	return s, ok
}

// holdsWord reports whether the text holds w as a word in any of main's
// readings (isWord, wordByte, idByte) and any declared form; read is false
// where the recording was not read (held is false then).
func (t *readerText) holdsWord(w string) (held, read bool) {
	return t.wordIn(w, 0, 1, 2)
}

// wordIn reports whether the text holds w as a word in any of the readings
// rs (indices of main's readings) and any declared form: from the words the
// entry keeps (tokens), or, past the budget, by a scan of the text read
// now and its other declared forms (textForms, the forms main's word lists
// read), which reads no other word of it and keeps nothing, in no cache
// either. read is false where the recording was not read.
func (t *readerText) wordIn(w string, rs ...int) (held, read bool) {
	in := func(f string) bool {
		return slices.ContainsFunc(rs, func(r int) bool { return heldBy(f, w, readings[r]) })
	}
	sets, scanned, ok := t.tokens(func(s string, _ []rawField) { held = slices.ContainsFunc(textForms(s), in) })
	if scanned || !ok {
		return held, ok
	}
	return slices.ContainsFunc(rs, func(r int) bool { return r < len(sets) && sets[r][w] }), true
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

// fieldAt names one field of a recording's text (rawBody.fields, whose
// paths round F's view of it shares): its path, and whether it is the
// field's name rather than its value. A text the caller hands, or a
// recording read with no fields, is one field: the zero fieldAt.
type fieldAt struct {
	path string
	key  bool
}

// fieldWords is what the index keeps of a text's fields: each field, and,
// by word, the fields (indices into at) whose own text holds it.
type fieldWords struct {
	at []fieldAt
	of map[string][]int32
}

func fieldWordsBytes(f fieldWords) int64 {
	n := int64(headerBytes) * int64(1+len(f.at))
	for _, a := range f.at {
		n += int64(len(a.path))
	}
	for w, at := range f.of {
		n += wordBytes + int64(len(w)+4*len(at))
	}
	return n
}

// fieldsWith are the fields of the text that hold w as a word, in any of
// main's readings and any declared form of the field's own text (holdsWord,
// field by field): a recording's by its fields, so a word that moved to
// another field in round F's view is not at its recorded field. A text
// that does not hold w (holdsWord) has no field with it, and no field is
// read for it: a word only a field's own text decoded twice holds (a JSON
// string whose content is itself escaped) is no word of the text. Read
// once for the call where they fit the budget; past it, by a scan of the
// fields of the text read now, which keeps nothing. read is false where
// the recording was not read.
func (t *readerText) fieldsWith(w string) (at []fieldAt, read bool) {
	if held, ok := t.holdsWord(w); !held {
		return nil, ok
	}
	fw, scanned, ok := indexOnce(t, func(e *readerEntry) *answer[fieldWords] { return &e.fields },
		func(s string, fields []rawField) fieldWords {
			fw := fieldWords{of: map[string][]int32{}}
			eachField(s, fields, func(f fieldAt, text string) {
				k := int32(len(fw.at))
				fw.at = append(fw.at, f)
				for _, set := range tokensOfForms(textForms(text)) {
					for w := range set {
						if l := fw.of[w]; len(l) == 0 || l[len(l)-1] != k {
							fw.of[w] = append(l, k)
						}
					}
				}
			})
			return fw
		}, fieldWordsBytes, func(s string, fields []rawField) {
			eachField(s, fields, func(f fieldAt, text string) {
				if slices.ContainsFunc(textForms(text), func(form string) bool {
					return slices.ContainsFunc(readings, func(r func(byte) bool) bool { return heldBy(form, w, r) })
				}) {
					at = append(at, f)
				}
			})
		})
	if scanned || !ok {
		return at, ok
	}
	for _, k := range fw.of[w] {
		at = append(at, fw.at[k])
	}
	return at, true
}

// eachField calls yield with each field of s, the text read now, and the
// field's text: its recording's fields (fields, read with s), a field that
// is JSON (an HTTP body: jsonValues) by its own fields below its path
// (textFields), else s whole as one field.
func eachField(s string, fields []rawField, yield func(fieldAt, string)) {
	if len(fields) == 0 {
		yield(fieldAt{}, s)
	}
	for _, f := range fields {
		if f.end > len(s) {
			continue
		}
		text := s[f.start:f.end]
		v, ok := jsonValues(text)
		if f.key || !ok {
			yield(fieldAt{f.path, f.key}, text)
			continue
		}
		doc, subs := textFields(v)
		for _, g := range subs {
			yield(fieldAt{strings.TrimSuffix(f.path+"."+g.path, "."), g.key}, doc[g.start:g.end])
		}
	}
}

// jsonValues is text read as JSON where it opens with an object or an
// array: one value, or several (NDJSON, a streaming API's body), each then
// below its index as an array's element is. ok is false where text is not
// wholly JSON values: a body that is no JSON is one field, as main reads it.
func jsonValues(text string) (any, bool) {
	if !strings.ContainsAny(text[:min(1, len(text))], "{[") {
		return nil, false
	}
	d := json.NewDecoder(strings.NewReader(text))
	d.UseNumber()
	var vs []any
	for {
		var v any
		if err := d.Decode(&v); err == io.EOF {
			break
		} else if err != nil {
			return nil, false
		}
		vs = append(vs, v)
	}
	if len(vs) == 1 {
		return vs[0], true
	}
	return vs, len(vs) > 0
}

// inOneField reports whether one field holds rw in r's text and lw in l's
// (fieldsWith): a recording and round F's view of it, whose fields share
// their paths, hold a changed word at one place in both runs only there.
// read is false where either recording was not read.
func inOneField(r *readerText, rw string, l *readerText, lw string) (same, read bool) {
	rf, okR := r.fieldsWith(rw)
	lf, okL := l.fieldsWith(lw)
	for _, f := range rf {
		same = same || slices.Contains(lf, f)
	}
	return same && okR && okL, okR && okL
}

// holdsToken reports whether the text holds tok whole, as a token of its
// kind: an idByte token (any declared form), or a base64 run that stands
// whole outside the text's instants (eachB64Run, the rule tokenSpans cuts
// it by); read as holdsWord. Past the budget a run is found by a scan of
// the text read now, which lists none of its other runs.
func (t *readerText) holdsToken(kind tokenKind, tok string) (held, read bool) {
	if kind == tokenID {
		return t.wordIn(tok, len(readings)-1)
	}
	runs, scanned, ok := indexOnce(t, func(e *readerEntry) *answer[map[string]bool] { return &e.runs },
		func(s string, _ []rawField) map[string]bool {
			runs := map[string]bool{}
			eachB64Run(s, t.x.dc.spansOf(s), func(a, b int) bool { runs[s[a:b]] = true; return true })
			return runs
		}, wordsBytes, func(s string, _ []rawField) {
			eachB64Run(s, t.x.dc.spansOf(s), func(a, b int) bool { held = s[a:b] == tok; return !held })
		})
	if scanned || !ok {
		return held, ok
	}
	return runs[tok], true
}

// eachB64Run calls yield with [start, end) of each of s's base64 runs that
// stand whole outside its instants (spans), in order, until it returns
// false: a run of the base64 alphabets, standard or URL-safe, that no byte
// of them comes right before, with all the padding ('=') after it, that no
// byte of them follows, an instant's bounds standing as the text's ends
// ("QUJDREVG+" of "QUJDREVG+2026-10-31T10:50:13Z"). It is the one rule the
// reader cuts a base64 token by (tokenSpans) and finds one in a text by
// (holdsToken), so every token it cuts is held by its own text: a run a
// byte of the alphabets follows past its padding (`tok=` of
// `tok=ab+cd/ef==`) is none, and its words are judged one by one.
func eachB64Run(s string, spans []instant, yield func(start, end int) bool) {
	from := 0
	gap := func(to int) bool {
		for i := from; i < to; {
			if !isB64Byte(s[i]) {
				i++
				continue
			}
			j := i
			for j < to && isB64Byte(s[j]) {
				j++
			}
			k := j
			for k < to && s[k] == '=' {
				k++
			}
			if (k == to || !isB64Byte(s[k])) && !yield(i, k) {
				return false
			}
			i = k
		}
		return true
	}
	for _, in := range spans {
		if in.start >= from && !gap(in.start) {
			return
		}
		from = max(from, in.end)
	}
	gap(len(s))
}

// holdsMoment reports whether an instant of the text names m by moment
// (byMoment), and whether one does so at a fine cut, in the text or any of
// its declared forms, the forms main reads an instant in (holdsExact(text)
// || holdsExact(rawExtra(text))) and holdsWord reads a word in; read as
// holdsWord, the instants of the text and its forms from one read of it.
func (t *readerText) holdsMoment(m moment) (held, fine, read bool) {
	dc := t.x.dc
	spans, _, ok := indexOnce(t, func(e *readerEntry) *answer[[]instant] { return &e.formSpans },
		func(s string, _ []rawField) []instant {
			return slices.Concat(dc.spansOf(s), dc.spansOf(dc.rawExtra(s)))
		}, spansBytes, nil)
	for _, sp := range spans {
		if !sp.keyed {
			continue
		}
		if h, f := byMoment(sp.moment(), m); h {
			held, fine = true, fine || f
		}
	}
	return held, fine, ok
}

// unread reports whether the call asked about a recording keploy could not
// read (Raw.Read): what the budget does not keep is read again, never
// unread.
func (x *readerIndex) unread() bool { return x.unreadTexts > 0 }
