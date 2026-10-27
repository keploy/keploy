package learn

import (
	"strings"
	"sync"
)

// setDerived keeps, for the running test set, what the learn step derives
// from a recording's payload alone, by the recording (Raw.id: MockID, of a
// payload RawOf read whole): the instants its text and its other forms hold
// (payloadMoments), its instants where the offset rule reads them
// (keyedPlaces) and its words in every form (searchedTokens). A test's drift
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
}

// derivedWords names a payload's words as searchedTokens reads them: with
// its answer's stamps blanked or kept.
type derivedWords struct {
	id   string
	keep bool
}

func newSetDerived() *setDerived {
	return &setDerived{moments: map[string][]moment{}, places: map[string][]keyedPlace{}, tokens: map[derivedWords][]map[string]bool{}}
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
