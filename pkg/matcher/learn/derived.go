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
// word rule's text search can find in it (searchWords) and the instants each
// of its fields holds (fieldMoments). A test's drift
// cache starts empty, so without it every test of a set read the payloads of
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
	hasMoments, hasPlaces bool
	hasTokens, hasWords   [2]bool
	hasFields             bool
	size                  int64
	at                    *list.Element
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
// recently until the set's parts are within the budget: the entry just
// kept too, where it alone is past it.
func (s *setDerived) keep(id string, size int64, have func(e *derivedEntry) bool, put func(e *derivedEntry)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.by[id]
	if e != nil && have(e) {
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
	read := func() []moment {
		var out []moment
		for _, text := range []string{x.payload().text, dc.rawExtra(x.payload().text)} {
			if text == "" {
				continue
			}
			for _, sp := range dc.spansOf(text) {
				if sp.keyed {
					out = append(out, sp.moment())
				}
			}
		}
		return out
	}
	s := dc.setOf(x)
	if s == nil {
		return read()
	}
	has := func(e *derivedEntry) bool { return e.hasMoments }
	if e, ok := s.find(x.id, has); ok {
		return e.moments
	}
	m := read()
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
	read := dc.readTokens(x, keep)
	out = make([]map[string]bool, len(read))
	size := int64(headerBytes)
	for i, words := range read {
		out[i] = make(map[string]bool, len(words))
		for w, v := range words {
			out[i][strings.Clone(w)] = v
		}
		size += wordsBytes(out[i])
	}
	s.keep(x.id, size, has, func(e *derivedEntry) { e.tokens[k], e.hasTokens[k] = out, true })
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
		for _, sp := range dc.spansOf(xb.text[f.start:f.end]) {
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
