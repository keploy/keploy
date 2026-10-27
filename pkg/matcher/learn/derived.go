package learn

import (
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
type setDerived struct {
	mu      sync.Mutex
	moments map[string][]moment
	places  map[string][]keyedPlace
	tokens  map[derivedWords][]map[string]bool
	words   map[derivedWords]map[string]bool
	fields  map[string][][]moment
}

// derivedWords names a payload's words as searchedTokens reads them: with
// its answer's stamps blanked or kept.
type derivedWords struct {
	id   string
	keep bool
}

func newSetDerived() *setDerived {
	return &setDerived{moments: map[string][]moment{}, places: map[string][]keyedPlace{}, tokens: map[derivedWords][]map[string]bool{},
		words: map[derivedWords]map[string]bool{}, fields: map[string][][]moment{}}
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
		for _, text := range []string{x.text, dc.rawExtra(x.text)} {
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
	s.mu.Lock()
	m, ok := s.moments[x.id]
	s.mu.Unlock()
	if ok {
		return m
	}
	m = read()
	s.mu.Lock()
	s.moments[x.id] = m
	s.mu.Unlock()
	return m
}

// setPlaces is keyedPlaces(x), read once for the set where dc keeps x's
// (setOf), each name a copy; ok is false where it does not.
func (dc *DriftCache) setPlaces(x Raw) (out []keyedPlace, ok bool) {
	s := dc.setOf(x)
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	out, ok = s.places[x.id]
	s.mu.Unlock()
	if ok {
		return out, true
	}
	out = dc.readPlaces(x)
	for i := range out {
		k := &out[i]
		k.k.path, k.k.label, k.where = strings.Clone(k.k.path), strings.Clone(k.k.label), strings.Clone(k.where)
	}
	s.mu.Lock()
	s.places[x.id] = out
	s.mu.Unlock()
	return out, true
}

// setTokens is searchedTokens(x, keep), read once for the set where dc
// keeps x's (setOf), each word a copy; ok is false where it does not.
func (dc *DriftCache) setTokens(x Raw, keep bool) (out []map[string]bool, ok bool) {
	s := dc.setOf(x)
	if s == nil {
		return nil, false
	}
	k := derivedWords{id: x.id, keep: keep}
	s.mu.Lock()
	out, ok = s.tokens[k]
	s.mu.Unlock()
	if ok {
		return out, true
	}
	read := dc.readTokens(x, keep)
	out = make([]map[string]bool, len(read))
	for i, words := range read {
		out[i] = make(map[string]bool, len(words))
		for w, v := range words {
			out[i][strings.Clone(w)] = v
		}
	}
	s.mu.Lock()
	s.tokens[k] = out
	s.mu.Unlock()
	return out, true
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
	k := derivedWords{id: x.id, keep: keep}
	s.mu.Lock()
	out, ok = s.words[k]
	s.mu.Unlock()
	if ok {
		return out, true
	}
	out = map[string]bool{}
	for _, text := range dc.searched(x, keep) {
		for _, seg := range segmentsBy(text, isWord) {
			if seg.word && !out[seg.text] {
				out[strings.Clone(seg.text)] = true
			}
		}
	}
	s.mu.Lock()
	s.words[k] = out
	s.mu.Unlock()
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
	s.mu.Lock()
	out, ok = s.fields[x.id]
	s.mu.Unlock()
	if ok {
		return out, true
	}
	out = make([][]moment, len(x.fields))
	for i, f := range x.fields {
		if f.key {
			continue
		}
		for _, sp := range dc.spansOf(x.text[f.start:f.end]) {
			if sp.keyed {
				out[i] = append(out[i], sp.moment())
			}
		}
	}
	s.mu.Lock()
	s.fields[x.id] = out
	s.mu.Unlock()
	return out, true
}
