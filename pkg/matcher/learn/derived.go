package learn

import (
	"container/list"
	"strings"
	"sync"
)

// setDerived keeps, for the running test set, what the learn step derives
// from a recording's payload alone, by the recording (Raw.id: MockID, of a
// payload RawOf read whole): the instants its text and its other forms hold
// (payloadMoments), its instants where the offset rule reads them
// (keyedPlaces), its words in every form (searchedTokens), the words the
// word rule's text search can find in it (searchWords), the instants each
// of its fields holds (fieldMoments), its other forms (payloadExtra) and its
// texts with its answer's stamps and clock reads blanked (setSearched). A
// test's drift cache starts empty, so without it every test of a set read the payloads of
// every recording served or recorded before its calls again, and the learn
// step's cost per call grew with the set. It keeps none of a payload's text:
// each word and name it keeps is a copy, so an answer the agent parked on
// disk stays there once its test is done, and nothing is keyed by text. The
// tracker replaces it at each set (NewSet), so it holds at most the set's
// recordings, and a recording staged again for another set is read again.
// What is derived from a payload depends on its text alone, which its
// recording fixes for the set, as Order.GivenPlaces reads it.
//
// What it keeps is within derivedBudget bytes (about, by each part's
// size), the recording read least recently let go first: a payload's words
// take more memory than its text, so a set's words kept whole grew with the
// set's payloads (1.8 GB over 256 recordings of 1 MiB). A recording let go
// is derived again, from its payload, where a rule next reads it; a part
// larger than the whole budget is handed to its reader and kept by none.
type setDerived struct {
	mu     sync.Mutex
	budget int64
	used   int64
	order  *list.List // of *derivedEntry, the most recently read first
	by     map[string]*derivedEntry
	// bodies are the set's lazy Raws' payloads, loaded within a fixed
	// budget (bodyStore), made at the first lazy Raw.
	bodies *bodyStore
}

// derivedEntry is what setDerived keeps of one recording: each part where
// its has bit is set.
type derivedEntry struct {
	id                    string
	moments               []moment
	places                []keyedPlace
	tokens                [2][]map[string]bool // by keep
	words                 [2]map[string]bool   // by keep
	fields                [][]moment
	extra                 string
	searched              [2][2]string // by keep, where not plain
	stamps                []string
	answer                [2][]map[string]bool // by keep
	answerAt              []givenMoment
	forms                 [][]moment
	hasMoments, hasPlaces bool
	hasTokens, hasWords   [2]bool
	hasFields, hasExtra   bool
	hasStamps, hasAt      bool
	hasAnswer             [2]bool
	hasForms              bool
	// hasSearched: searched is kept, or plain says the payload's searched
	// texts are its own text and extra (no stamp or clock read to blank).
	hasSearched, plain [2]bool
	size               int64
	at                 *list.Element
}

// derivedBudget is the bytes of derived data setDerived keeps for a set.
var derivedBudget int64 = 32 << 20

// The sizes setDerived counts: a moment, a keyed place past its strings, a
// word in a set of words past its letters (the map's slot and growth, the
// copy's allocation), and a slice or map's own header.
const (
	momentBytes = 64
	placeBytes  = 128
	wordBytes   = 48
	headerBytes = 48
)

func wordsBytes(words map[string]bool) int64 {
	n := int64(headerBytes)
	for w := range words {
		n += wordBytes + int64(len(w))
	}
	return n
}

func newSetDerived() *setDerived {
	return &setDerived{budget: derivedBudget, order: list.New(), by: map[string]*derivedEntry{}}
}

// find is the entry of id where have says it holds the part asked for,
// marked read most recently; ok is false where it does not.
func (s *setDerived) find(id string, have func(e *derivedEntry) bool) (e *derivedEntry, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e = s.by[id]
	if e == nil || !have(e) {
		return nil, false
	}
	s.order.MoveToFront(e.at)
	return e, true
}

// keep puts a part of size bytes into id's entry (put), unless have says
// another read put it first, then lets go of the recordings read least
// recently until the set's parts are within the budget. A part past the
// whole budget is kept by none and lets go of nothing.
func (s *setDerived) keep(id string, size int64, have func(e *derivedEntry) bool, put func(e *derivedEntry)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.by[id]
	if e != nil && have(e) || size > s.budget {
		return
	}
	if e == nil {
		e = &derivedEntry{id: id}
		e.at = s.order.PushFront(e)
		s.by[id] = e
	} else {
		s.order.MoveToFront(e.at)
	}
	put(e)
	e.size += size
	s.used += size
	for s.used > s.budget {
		last := s.order.Back()
		if last == nil {
			break
		}
		old := last.Value.(*derivedEntry)
		s.order.Remove(last)
		delete(s.by, old.id)
		s.used -= old.size
	}
}

// keptBytes is the bytes setDerived counts as kept, for the rows.
func (s *setDerived) keptBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used
}

// setOf is the set cache that keeps x's derived data (setDerived), nil where
// none does: x's text must be all of its recording's payload (Raw.id) and dc
// a tracker's (newDrifts).
func (dc *DriftCache) setOf(x Raw) *setDerived {
	if dc == nil || x.id == "" {
		return nil
	}
	return dc.set
}

// payloadMoments are the keyed instants x's text and its other forms hold
// (heldIn's), read once for the set where dc keeps x's (setOf), once per
// test otherwise.
func (dc *DriftCache) payloadMoments(x Raw) []moment {
	read := func(spans func(string) []instant, extra string) []moment {
		var out []moment
		for _, text := range []string{x.payload().text, extra} {
			if text == "" {
				continue
			}
			for _, sp := range spans(text) {
				if sp.keyed {
					out = append(out, sp.moment())
				}
			}
		}
		return out
	}
	s := dc.setOf(x)
	if s == nil {
		return read(dc.spansOf, dc.rawExtra(x.payload().text))
	}
	has := func(e *derivedEntry) bool { return e.hasMoments }
	if e, ok := s.find(x.id, has); ok {
		return e.moments
	}
	// Read now, never into the test's cache of texts (textCache), which
	// would key them by the payload's text.
	m := read(instantSpans, dc.payloadExtra(x))
	s.keep(x.id, headerBytes+momentBytes*int64(len(m)), has, func(e *derivedEntry) { e.moments, e.hasMoments = m, true })
	return m
}

// setPlaces is keyedPlaces(x), read once for the set where dc keeps x's
// (setOf), each name a copy; ok is false where it does not.
func (dc *DriftCache) setPlaces(x Raw) (out []keyedPlace, ok bool) {
	s := dc.setOf(x)
	if s == nil {
		return nil, false
	}
	has := func(e *derivedEntry) bool { return e.hasPlaces }
	if e, ok := s.find(x.id, has); ok {
		return e.places, true
	}
	out = dc.readPlaces(x)
	size := int64(headerBytes)
	for i := range out {
		k := &out[i]
		k.k.path, k.k.label, k.where = strings.Clone(k.k.path), strings.Clone(k.k.label), strings.Clone(k.where)
		size += placeBytes + int64(len(k.k.path)+len(k.k.label)+len(k.where))
	}
	s.keep(x.id, size, has, func(e *derivedEntry) { e.places, e.hasPlaces = out, true })
	return out, true
}

// setTokens is searchedTokens(x, keep), read once for the set where dc
// keeps x's (setOf), each word a copy; ok is false where it does not.
func (dc *DriftCache) setTokens(x Raw, keep bool) (out []map[string]bool, ok bool) {
	s := dc.setOf(x)
	if s == nil {
		return nil, false
	}
	k := keepIndex(keep)
	has := func(e *derivedEntry) bool { return e.hasTokens[k] }
	if e, ok := s.find(x.id, has); ok {
		return e.tokens[k], true
	}
	out, size := copyWords(tokensIn(x, keep))
	s.keep(x.id, size, has, func(e *derivedEntry) { e.tokens[k], e.hasTokens[k] = out, true })
	return out, true
}

// copyWords is words, by reading, with each word a copy, and about the
// bytes the copy holds.
func copyWords(words []map[string]bool) ([]map[string]bool, int64) {
	out := make([]map[string]bool, len(words))
	size := int64(headerBytes)
	for i, read := range words {
		out[i] = make(map[string]bool, len(read))
		for w, v := range read {
			out[i][strings.Clone(w)] = v
		}
		size += wordsBytes(out[i])
	}
	return out, size
}

// stampTexts are the texts of x's answer's stamps (Raw: an HTTP-date its
// answer holds), as stampsEchoed reads them, each a copy, read once for the
// set where dc keeps x's (setOf), from x's payload otherwise.
func (dc *DriftCache) stampTexts(x Raw) []string {
	read := func() []string {
		xb := x.payload()
		out := make([]string, len(xb.stamps))
		for i, f := range xb.stamps {
			out[i] = xb.text[f.start:f.end]
		}
		return out
	}
	s := dc.setOf(x)
	if s == nil {
		return read()
	}
	has := func(e *derivedEntry) bool { return e.hasStamps }
	if e, ok := s.find(x.id, has); ok {
		return e.stamps
	}
	out := read()
	size := int64(headerBytes)
	for i := range out {
		out[i] = strings.Clone(out[i])
		size += headerBytes + int64(len(out[i]))
	}
	s.keep(x.id, size, has, func(e *derivedEntry) { e.stamps, e.hasStamps = out, true })
	return out
}

// answerWords are readAnswerWords(x, keep), read once for the set where dc
// keeps x's (setOf), each word a copy; once per call otherwise.
func (dc *DriftCache) answerWords(x Raw, keep bool) []map[string]bool {
	s := dc.setOf(x)
	if s == nil {
		return dc.readAnswerWords(x, keep)
	}
	k := keepIndex(keep)
	has := func(e *derivedEntry) bool { return e.hasAnswer[k] }
	if e, ok := s.find(x.id, has); ok {
		return e.answer[k]
	}
	out, size := copyWords(dc.readAnswerWords(x, keep))
	s.keep(x.id, size, has, func(e *derivedEntry) { e.answer[k], e.hasAnswer[k] = out, true })
	return out
}

// answerMoments are readAnswerMoments(x), its instants and their places
// read once for the set where dc keeps x's (setOf), each name a copy, with
// what x itself says of when it was recorded (late, noTime); once per call
// otherwise.
func (dc *DriftCache) answerMoments(x Raw) []givenMoment {
	s := dc.setOf(x)
	if s == nil {
		return dc.readAnswerMoments(x)
	}
	has := func(e *derivedEntry) bool { return e.hasAt }
	var kept []givenMoment
	if e, ok := s.find(x.id, has); ok {
		kept = e.answerAt
	} else {
		kept = dc.readAnswerMoments(x)
		size := int64(headerBytes)
		for i := range kept {
			a := &kept[i]
			a.mock, a.where = strings.Clone(a.mock), strings.Clone(a.where)
			size += momentBytes + int64(len(a.mock)+len(a.where))
		}
		s.keep(x.id, size, has, func(e *derivedEntry) { e.answerAt, e.hasAt = kept, true })
	}
	out := make([]givenMoment, len(kept))
	for i, a := range kept {
		a.mock, a.at, a.late, a.noTime = x.mock, x.at, x.late, x.noTime
		out[i] = a
	}
	return out
}

// fieldForms are the keyed instants each field of x holds in its text and
// in its other forms (as heldAt reads a field; none of a key's), by the
// field's index, read once for the set where dc keeps x's (setOf); ok is
// false where it does not.
func (dc *DriftCache) fieldForms(x Raw) (out [][]moment, ok bool) {
	s := dc.setOf(x)
	if s == nil {
		return nil, false
	}
	has := func(e *derivedEntry) bool { return e.hasForms }
	if e, ok := s.find(x.id, has); ok {
		return e.forms, true
	}
	xb := x.payload()
	out = make([][]moment, len(xb.fields))
	for i, f := range xb.fields {
		if f.key {
			continue
		}
		text := xb.text[f.start:f.end]
		for _, form := range []string{text, extraOf(text)} {
			for _, sp := range instantSpans(form) {
				if sp.keyed {
					out[i] = append(out[i], sp.moment())
				}
			}
		}
	}
	size := int64(headerBytes)
	for _, f := range out {
		size += headerBytes + momentBytes*int64(len(f))
	}
	s.keep(x.id, size, has, func(e *derivedEntry) { e.forms, e.hasForms = out, true })
	return out, true
}

// keepIndex is the index of keep in derivedEntry's parts by keep.
func keepIndex(keep bool) int {
	if keep {
		return 1
	}
	return 0
}

// searchWords are the words of x as the word rule searches it (searched(x,
// keep): its text and its other forms), read by letters and digits alone
// (isWord), each a copy, once for the set where dc keeps x's (setOf); ok is
// false where it does not. They tell which values the search can find in x
// without reading its text (mayHold).
func (dc *DriftCache) searchWords(x Raw, keep bool) (out map[string]bool, ok bool) {
	s := dc.setOf(x)
	if s == nil {
		return nil, false
	}
	k := keepIndex(keep)
	has := func(e *derivedEntry) bool { return e.hasWords[k] }
	if e, ok := s.find(x.id, has); ok {
		return e.words[k], true
	}
	out = map[string]bool{}
	for _, text := range dc.searched(x, keep) {
		for _, seg := range segmentsBy(text, isWord) {
			if seg.word && !out[seg.text] {
				out[strings.Clone(seg.text)] = true
			}
		}
	}
	s.keep(x.id, wordsBytes(out), has, func(e *derivedEntry) { e.words[k], e.hasWords[k] = out, true })
	return out, true
}

// mayHold reports whether a text whose words (searchWords) are words can
// hold v as a token (ContainsToken), by v's runs of letters and digits
// (runs: wordsBy(v, isWord)): where it holds v at i as a token, each run of
// v stands there with no letter or digit beside it (inside v, a character
// that is neither; at v's ends, tokenAt's own check), so it is one of the
// text's words. Where a run is not, the text does not hold v; where each
// is, it may (a value of one run, a word of the text, it does), and only
// then is the text read.
func mayHold(words map[string]bool, runs []string) bool {
	for _, r := range runs {
		if !words[r] {
			return false
		}
	}
	return true
}

// fieldMoments are the keyed instants each field of x holds (spansOf of its
// text, as echoRaw reads it; none of a key's), by the field's index, read
// once for the set where dc keeps x's (setOf); ok is false where it does
// not.
func (dc *DriftCache) fieldMoments(x Raw) (out [][]moment, ok bool) {
	s := dc.setOf(x)
	if s == nil {
		return nil, false
	}
	has := func(e *derivedEntry) bool { return e.hasFields }
	if e, ok := s.find(x.id, has); ok {
		return e.fields, true
	}
	xb := x.payload()
	out = make([][]moment, len(xb.fields))
	for i, f := range xb.fields {
		if f.key {
			continue
		}
		for _, sp := range instantSpans(xb.text[f.start:f.end]) {
			if sp.keyed {
				out[i] = append(out[i], sp.moment())
			}
		}
	}
	size := int64(headerBytes)
	for _, f := range out {
		size += headerBytes + momentBytes*int64(len(f))
	}
	s.keep(x.id, size, has, func(e *derivedEntry) { e.fields, e.hasFields = out, true })
	return out, true
}

// payloadExtra is rawExtra of x's text, read once for the set where dc keeps
// x's (setOf), by the recording and as a copy, never by its text; once per
// test otherwise.
func (dc *DriftCache) payloadExtra(x Raw) string {
	s := dc.setOf(x)
	if s == nil {
		return dc.rawExtra(x.payload().text)
	}
	has := func(e *derivedEntry) bool { return e.hasExtra }
	if e, ok := s.find(x.id, has); ok {
		return e.extra
	}
	out := strings.Clone(extraOf(x.payload().text))
	s.keep(x.id, headerBytes+int64(len(out)), has, func(e *derivedEntry) { e.extra, e.hasExtra = out, true })
	return out
}

// setSearched is searched(x, keep) of an x whose set s keeps it: the texts
// with x's stamps or clock reads blanked, as copies, by the recording; a
// payload with none to blank is searched as its own text (the set's store
// of payloads, bodyStore) and payloadExtra, and s keeps only that it is.
func (dc *DriftCache) setSearched(s *setDerived, x Raw, keep bool) [2]string {
	k := keepIndex(keep)
	has := func(e *derivedEntry) bool { return e.hasSearched[k] }
	if e, ok := s.find(x.id, has); ok {
		if e.plain[k] {
			return [2]string{x.payload().text, dc.payloadExtra(x)}
		}
		return e.searched[k]
	}
	y := x
	if keep {
		y = x.withoutStamps()
	}
	keys := clockKeys(y)
	yb := y.payload()
	if len(keys) == 0 && len(yb.stamps) == 0 {
		s.keep(x.id, 0, has, func(e *derivedEntry) { e.plain[k], e.hasSearched[k] = true, true })
		return [2]string{yb.text, dc.payloadExtra(x)}
	}
	out := blankedPair(yb.text, yb.stamps, keys)
	s.keep(x.id, headerBytes+pairBytes(out), has, func(e *derivedEntry) { e.searched[k], e.hasSearched[k] = out, true })
	return out
}
