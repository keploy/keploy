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
// (keyedPlaces), its words in every form (searchedTokens), a filter of the
// words the word rule's text search can find in it (searchWords), the
// instants each of its fields holds (fieldMoments), its other forms
// (payloadExtra) and its texts with its answer's stamps and clock reads
// blanked (setSearched). A test's drift cache starts empty, so without it
// every test of a set read the payloads of every recording served or
// recorded before its calls again, and the learn step's cost per call grew
// with the set. It keeps none of a payload's text: each word and name it
// keeps is a copy, so an answer the agent parked on disk stays there once
// its test is done, and nothing is keyed by text. The tracker replaces it at
// each set (NewSet), so it holds at most the set's recordings, and a
// recording staged again for another set is read again. What is derived
// from a payload depends on its text alone, which its recording fixes for
// the set, as Order.GivenPlaces reads it.
//
// It keeps them in two parts, each within its own budget:
//
//   - held: a payload's instants and their places, its stamps, and a filter
//     of its words of a fixed size (wordFilter), within heldBudget. The
//     learn step reads these of every recording before a call, for each
//     changed value of each call, in the same order, so they are kept for
//     the set, never let go to make room for another recording's: what a
//     recording holds of them is small (about 64 bytes an instant and 2 KiB
//     at most of filters), and the set's grow with its recordings. Past the
//     budget a recording's are kept by none and read through the test's
//     cache of texts (textCache), once per test, as with no set; the
//     recordings kept stay kept. A set that let the recording read least
//     recently go was read anew on every call past its budget (api-server's
//     round: 14 813 recordings, 335 MB of payloads, 76-86% of the agent's CPU).
//   - words: a payload's words and texts (searchedTokens, readAnswerWords,
//     payloadExtra, setSearched), within derivedBudget, the recording read
//     least recently let go first. A payload's words take more memory than
//     its text, so a set's words kept whole grew with the set's payloads
//     (1.8 GB over 256 recordings of 1 MiB). A recording let go is derived
//     again where a rule next reads it; the word search reads them only
//     where a payload's filter may hold a value's words (mayHold).
//
// A part larger than its whole budget is handed to its reader and kept by
// none. Handlers of one set read it at once: where one is reading a part of
// a recording, another that needs it waits for it (derive), so a part is
// read once, not once per handler.
type setDerived struct {
	mu    sync.Mutex
	held  tier[heldParts]
	words tier[wordParts]
	// reading are the parts being read (derive), by recording and part:
	// closed when read.
	reading map[readingKey]chan struct{}
	// waits is how many times a handler waited for another's read, for the
	// rows.
	waits int
	// bodies are the set's lazy Raws' payloads, loaded within a fixed
	// budget (bodyStore), made at the first lazy Raw.
	bodies *bodyStore
	// offsets are the set's places the offset rule reads, by place and
	// moment (offsetIndex), within offsetBudget: its handlers share them.
	offsets *offsetIndex
}

// heldParts are what setDerived keeps of a recording for the set (held).
type heldParts struct {
	moments  []moment
	places   []keyedPlace
	fields   [][]moment
	forms    [][]moment
	stamps   []string
	answerAt []givenMoment
	filter   [2]wordFilter // by keep
	plain    [2]bool       // by keep (partPlain)
}

// wordParts are what setDerived keeps of a recording within its budget
// (words).
type wordParts struct {
	tokens   [2][]map[string]bool // by keep
	answer   [2][]map[string]bool // by keep
	extra    string
	searched [2][2]string // by keep, where not plain (partPlain)
}

// part is one part a tier keeps of a recording, a bit of slot.has.
type part uint16

const (
	partMoments part = 1 << iota
	partPlaces
	partFields
	partForms
	partStamps
	partAt
	partFilter // by keep: partFilter<<k
	_
	// partPlain: the payload's searched texts are its own text and
	// payloadExtra (no stamp or clock read to blank), by keep.
	partPlain
	_
	partTokens // by keep
	_
	partAnswer // by keep
	_
	partExtra
	partSearched // by keep
)

// byKeep is p, a part kept by keep, for keep.
func byKeep(p part, keep bool) part { return p << keepIndex(keep) }

// slot is what a tier keeps of one recording: each part where has holds it.
type slot[P any] struct {
	id   string
	p    P
	has  part
	size int64
	at   *list.Element
}

// tier is one of setDerived's parts, within budget bytes (about, by each
// part's size). One that lets go (lru) lets go of the recordings read least
// recently to keep a part; one that does not keeps none past its budget.
type tier[P any] struct {
	budget, used int64
	lru          bool
	// full: a tier that does not let go was past its budget for a part,
	// and keeps no more (room).
	full  bool
	order *list.List // of *slot[P], the most recently read first (lru)
	by    map[string]*slot[P]
}

func newTier[P any](budget int64, lru bool) tier[P] {
	return tier[P]{budget: budget, lru: lru, order: list.New(), by: map[string]*slot[P]{}}
}

// find is id's slot where it holds p, marked read most recently.
func (t *tier[P]) find(id string, p part) *slot[P] {
	sl := t.by[id]
	if sl == nil || sl.has&p == 0 {
		return nil
	}
	if t.lru {
		t.order.MoveToFront(sl.at)
	}
	return sl
}

// keep puts p, of size bytes, into id's slot (put), then, where the tier
// lets go, lets go of the recordings read least recently until it is within
// its budget. A part past the whole budget is kept by none and lets go of
// nothing; where the tier does not let go, nor is one past what is left of
// it. A recording's first part counts its slot.
func (t *tier[P]) keep(id string, p part, size int64, put func(*P)) {
	sl := t.by[id]
	if sl != nil && sl.has&p != 0 {
		return
	}
	if sl == nil {
		size += slotBytes + int64(len(id))
	}
	if size > t.budget {
		return
	}
	if !t.lru && t.used+size > t.budget {
		t.full = true
		return
	}
	if sl == nil {
		sl = &slot[P]{id: strings.Clone(id)}
		if t.lru {
			sl.at = t.order.PushFront(sl)
		}
		t.by[id] = sl
	} else if t.lru {
		t.order.MoveToFront(sl.at)
	}
	put(&sl.p)
	sl.has |= p
	sl.size += size
	t.used += size
	for t.lru && t.used > t.budget {
		last := t.order.Back()
		if last == nil {
			break
		}
		old := last.Value.(*slot[P])
		t.order.Remove(last)
		delete(t.by, old.id)
		t.used -= old.size
	}
}

// room is whether the tier may keep another part: one that lets go always
// may; one that does not, until a part did not fit in what was left of its
// budget (full), so a part it will not keep is not read as if it would on
// every call.
func (t *tier[P]) room() bool { return t.lru || !t.full }

type readingKey struct {
	id     string
	part   part
	inHeld bool
}

// derive is part p of x's recording that t keeps (get), read by read where
// t keeps none and kept by t (put, of size bytes). Where another handler is
// reading it, it waits for that read and takes what it kept; a part that read
// was not kept (past t's budget) is read again by each. read is told whether
// t may keep what it reads (room): a part t will not keep is read through the
// test's cache of texts, once per test.
func derive[P, V any](s *setDerived, t *tier[P], inHeld bool, id string, p part, get func(*P) V, read func(kept bool) V, put func(*P, V), size func(V) int64) V {
	k := readingKey{id, p, inHeld}
	for waited := false; ; waited = true {
		s.mu.Lock()
		if sl := t.find(id, p); sl != nil {
			v := get(&sl.p)
			s.mu.Unlock()
			return v
		}
		room := t.room()
		if !room || waited {
			s.mu.Unlock()
			return read(room)
		}
		if ch := s.reading[k]; ch != nil {
			s.waits++
			s.mu.Unlock()
			<-ch
			continue
		}
		ch := make(chan struct{})
		s.reading[k] = ch
		s.mu.Unlock()
		return readOnce(s, t, k, ch, get, read, put, size)
	}
}

// readOnce reads part k of a recording as derive's sole reader of it, keeps
// it, and lets the handlers waiting for it go, whatever read does.
func readOnce[P, V any](s *setDerived, t *tier[P], k readingKey, ch chan struct{}, get func(*P) V, read func(kept bool) V, put func(*P, V), size func(V) int64) V {
	defer func() {
		s.mu.Lock()
		delete(s.reading, k)
		s.mu.Unlock()
		close(ch)
	}()
	v := read(true)
	n := size(v)
	s.mu.Lock()
	defer s.mu.Unlock()
	t.keep(k.id, k.part, n, func(e *P) { put(e, v) })
	if sl := t.find(k.id, k.part); sl != nil {
		return get(&sl.p)
	}
	return v
}

// derivedBudget is the bytes of a set's payloads' words and texts setDerived
// keeps (words).
var derivedBudget int64 = 32 << 20

// heldBudget is the bytes of a set's payloads' instants, places, stamps and
// word filters setDerived keeps (held): a hard ceiling, far past what a
// set's recordings hold of them (api-server's round of 14 813 recordings
// holds tens of MiB).
var heldBudget int64 = 128 << 20

// The sizes setDerived counts: a moment, a keyed place past its strings, a
// word in a set of words past its letters (the map's slot and growth, the
// copy's allocation), a slice or map's own header, and a recording's slot in
// a tier past its id.
const (
	momentBytes = 64
	placeBytes  = 128
	wordBytes   = 48
	headerBytes = 48
	slotBytes   = 256
)

func wordsBytes(words map[string]bool) int64 {
	n := int64(headerBytes)
	for w := range words {
		n += wordBytes + int64(len(w))
	}
	return n
}

func newSetDerived() *setDerived {
	return &setDerived{held: newTier[heldParts](heldBudget, false), words: newTier[wordParts](derivedBudget, true), reading: map[readingKey]chan struct{}{}, offsets: newOffsetIndex(offsetBudget)}
}

// keptBytes is the bytes setDerived counts as kept of the set's words and
// texts (words), for the rows.
func (s *setDerived) keptBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.words.used
}

// heldBytes is the bytes setDerived counts as kept of the set's instants,
// places, stamps and word filters (held), for the rows.
func (s *setDerived) heldBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held.used
}

// heldPart is part p of x's recording that s keeps for the set (held); see
// derive.
func heldPart[V any](s *setDerived, x Raw, p part, get func(*heldParts) V, read func(kept bool) V, put func(*heldParts, V), size func(V) int64) V {
	return derive(s, &s.held, true, x.id, p, get, read, put, size)
}

// wordPart is part p of x's recording that s keeps within its budget
// (words); see derive.
func wordPart[V any](s *setDerived, x Raw, p part, get func(*wordParts) V, read func(kept bool) V, put func(*wordParts, V), size func(V) int64) V {
	return derive(s, &s.words, false, x.id, p, get, read, put, size)
}

func momentsBytes(m []moment) int64 { return headerBytes + momentBytes*int64(len(m)) }

// fieldsBytes is about what a moments-by-field holds: a slice header for
// each field, and its moments.
func fieldsBytes(f [][]moment) int64 {
	n := int64(headerBytes)
	for _, m := range f {
		n += 24 + momentBytes*int64(len(m))
	}
	return n
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
	return heldPart(s, x, partMoments, func(e *heldParts) []moment { return e.moments }, func(kept bool) []moment {
		if !kept {
			return read(dc.spansOf, dc.payloadExtra(x))
		}
		// Read now, never into the test's cache of texts (textCache),
		// which would key them by the payload's text.
		return read(instantSpans, dc.payloadExtra(x))
	}, func(e *heldParts, m []moment) { e.moments = m }, momentsBytes)
}

// setPlaces is keyedPlaces(x), read once for the set where dc keeps x's
// (setOf), each name a copy; ok is false where it does not.
func (dc *DriftCache) setPlaces(x Raw) (out []keyedPlace, ok bool) {
	s := dc.setOf(x)
	if s == nil {
		return nil, false
	}
	return heldPart(s, x, partPlaces, func(e *heldParts) []keyedPlace { return e.places }, func(bool) []keyedPlace {
		out := dc.readPlaces(x)
		for i := range out {
			k := &out[i]
			k.k.path, k.k.label, k.where = strings.Clone(k.k.path), strings.Clone(k.k.label), strings.Clone(k.where)
		}
		return out
	}, func(e *heldParts, v []keyedPlace) { e.places = v }, placesBytes), true
}

// setTokens is searchedTokens(x, keep), read once for the set where dc
// keeps x's (setOf), each word a copy; ok is false where it does not.
func (dc *DriftCache) setTokens(x Raw, keep bool) (out []map[string]bool, ok bool) {
	s := dc.setOf(x)
	if s == nil {
		return nil, false
	}
	k := keepIndex(keep)
	return wordPart(s, x, byKeep(partTokens, keep), func(e *wordParts) []map[string]bool { return e.tokens[k] }, func(bool) []map[string]bool {
		return copyWords(tokensIn(x, keep))
	}, func(e *wordParts, v []map[string]bool) { e.tokens[k] = v }, tokensBytes), true
}

// copyWords is words, by reading, with each word a copy.
func copyWords(words []map[string]bool) []map[string]bool {
	out := make([]map[string]bool, len(words))
	for i, read := range words {
		out[i] = make(map[string]bool, len(read))
		for w, v := range read {
			out[i][strings.Clone(w)] = v
		}
	}
	return out
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
	return heldPart(s, x, partStamps, func(e *heldParts) []string { return e.stamps }, func(bool) []string {
		out := read()
		for i := range out {
			out[i] = strings.Clone(out[i])
		}
		return out
	}, func(e *heldParts, v []string) { e.stamps = v }, func(v []string) int64 {
		size := int64(headerBytes)
		for _, t := range v {
			size += headerBytes + int64(len(t))
		}
		return size
	})
}

// answerWords are readAnswerWords(x, keep), read once for the set where dc
// keeps x's (setOf), each word a copy; once per call otherwise.
func (dc *DriftCache) answerWords(x Raw, keep bool) []map[string]bool {
	s := dc.setOf(x)
	if s == nil {
		return dc.readAnswerWords(x, keep)
	}
	k := keepIndex(keep)
	return wordPart(s, x, byKeep(partAnswer, keep), func(e *wordParts) []map[string]bool { return e.answer[k] }, func(bool) []map[string]bool {
		return copyWords(dc.readAnswerWords(x, keep))
	}, func(e *wordParts, v []map[string]bool) { e.answer[k] = v }, tokensBytes)
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
	kept := heldPart(s, x, partAt, func(e *heldParts) []givenMoment { return e.answerAt }, func(bool) []givenMoment {
		kept := dc.readAnswerMoments(x)
		for i := range kept {
			a := &kept[i]
			a.mock, a.where = strings.Clone(a.mock), strings.Clone(a.where)
		}
		return kept
	}, func(e *heldParts, v []givenMoment) { e.answerAt = v }, func(v []givenMoment) int64 {
		size := int64(headerBytes)
		for _, a := range v {
			size += momentBytes + int64(len(a.mock)+len(a.where))
		}
		return size
	})
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
	return heldPart(s, x, partForms, func(e *heldParts) [][]moment { return e.forms }, func(kept bool) [][]moment {
		return readFields(dc, x, kept, true)
	}, func(e *heldParts, v [][]moment) { e.forms = v }, fieldsBytes), true
}

// readFields are the keyed instants each field of x holds (none of a
// key's), by the field's index: in its text, and where forms says in its
// other forms (extraOf) too. Where the set keeps none (kept), each text is
// read through the test's cache of texts.
func readFields(dc *DriftCache, x Raw, kept, forms bool) [][]moment {
	spans, extra := instantSpans, extraOf
	if !kept {
		spans, extra = dc.spansOf, dc.rawExtra
	}
	xb := x.payload()
	out := make([][]moment, len(xb.fields))
	for i, f := range xb.fields {
		if f.key {
			continue
		}
		text := xb.text[f.start:f.end]
		read := []string{text}
		if forms {
			read = append(read, extra(text))
		}
		for _, form := range read {
			for _, sp := range spans(form) {
				if sp.keyed {
					out[i] = append(out[i], sp.moment())
				}
			}
		}
	}
	return out
}

// keepIndex is the index of keep in a part kept by keep.
func keepIndex(keep bool) int {
	if keep {
		return 1
	}
	return 0
}

// searchWords is a filter of the words of x as the word rule searches it
// (searched(x, keep): its text and its other forms), read by letters and
// digits alone (isWord), once for the set where dc keeps x's (setOf); ok is
// false where it does not, or where the set keeps no more (heldBudget). It
// tells which values the search can find in x without reading its text
// (mayHold).
func (dc *DriftCache) searchWords(x Raw, keep bool) (out wordFilter, ok bool) {
	s := dc.setOf(x)
	if s == nil {
		return nil, false
	}
	k := keepIndex(keep)
	out = heldPart(s, x, byKeep(partFilter, keep), func(e *heldParts) wordFilter { return e.filter[k] }, func(kept bool) wordFilter {
		if !kept {
			// Past the set's budget: the search reads the payload's
			// texts, as with no set, rather than its words each call.
			return nil
		}
		texts := dc.searched(x, keep)
		n := 0
		for _, text := range texts {
			for _, seg := range segmentsBy(text, isWord) {
				if seg.word {
					n++
				}
			}
		}
		f := newWordFilter(n)
		for _, text := range texts {
			for _, seg := range segmentsBy(text, isWord) {
				if seg.word {
					f.add(seg.text)
				}
			}
		}
		return f
	}, func(e *heldParts, v wordFilter) { e.filter[k] = v }, func(v wordFilter) int64 { return headerBytes + 8*int64(len(v)) })
	return out, out != nil
}

// mayHold reports whether a text whose words' filter (searchWords) is words
// can hold v as a token (ContainsToken), by v's runs of letters and digits
// (runs: wordsBy(v, isWord)): where it holds v at i as a token, each run of
// v stands there with no letter or digit beside it (inside v, a character
// that is neither; at v's ends, tokenAt's own check), so it is one of the
// text's words. Where a run is not in the filter, the text does not hold v;
// where each is, it may (the filter holds every word of the text, and a few
// it does not hold), and only then is the text read.
func mayHold(words wordFilter, runs []string) bool {
	for _, r := range runs {
		if !words.has(r) {
			return false
		}
	}
	return true
}

// wordFilter is a set of words that holds every word added to it and may
// hold a word that was not (a Bloom filter): filterHashes bits by word, of
// about filterBitsPerWord bits a word, at most filterMaxBytes bytes, so a
// recording's filter is the same small size whatever its payload's words
// take. It holds a word that was not added about once in a hundred below
// that size; a payload of more words than it is sized for may hold more of
// them, which costs a read of the payload, never a value the search misses.
type wordFilter []uint64

const (
	filterHashes      = 4
	filterBitsPerWord = 10
	filterMinBytes    = 32
	filterMaxBytes    = 2 << 10
)

// filterHash is w's hash in every filter: FNV-1a, its bits spread
// (splitmix64's finalizer). It is the same in every process, so which
// words a filter holds that were never added is the same on every run.
func filterHash(w string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(w); i++ {
		h ^= uint64(w[i])
		h *= 1099511628211
	}
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}

// newWordFilter is an empty filter for about n words, of filterMinBytes at
// least: a payload of few words holds a word it never had about once in
// a hundred thousand reads.
func newWordFilter(n int) wordFilter {
	size := filterMinBytes / 8
	for size*64 < n*filterBitsPerWord && size*8 < filterMaxBytes {
		size <<= 1
	}
	return make(wordFilter, size)
}

// bits are the filterHashes bits that hold w in f, as add and has read them.
func (f wordFilter) bits(w string, each func(word int, bit uint64) bool) bool {
	h := filterHash(w)
	h1, h2 := uint32(h), uint32(h>>32)|1
	mask := uint32(len(f)*64 - 1)
	for i := uint32(0); i < filterHashes; i++ {
		b := (h1 + i*h2) & mask
		if !each(int(b>>6), 1<<(b&63)) {
			return false
		}
	}
	return true
}

func (f wordFilter) add(w string) {
	f.bits(w, func(word int, bit uint64) bool { f[word] |= bit; return true })
}

func (f wordFilter) has(w string) bool {
	return f.bits(w, func(word int, bit uint64) bool { return f[word]&bit != 0 })
}

// fieldMoments are the keyed instants each field of x holds (spansOf of its
// text and of its other forms, as echoRaw reads it; none of a key's), by the
// field's index, read
// once for the set where dc keeps x's (setOf); ok is false where it does
// not.
func (dc *DriftCache) fieldMoments(x Raw) (out [][]moment, ok bool) {
	s := dc.setOf(x)
	if s == nil {
		return nil, false
	}
	return heldPart(s, x, partFields, func(e *heldParts) [][]moment { return e.fields }, func(kept bool) [][]moment {
		return readFields(dc, x, kept, true)
	}, func(e *heldParts, v [][]moment) { e.fields = v }, fieldsBytes), true
}

// payloadExtra is rawExtra of x's text, read once for the set where dc keeps
// x's (setOf), by the recording and as a copy, never by its text; once per
// test otherwise.
func (dc *DriftCache) payloadExtra(x Raw) string {
	s := dc.setOf(x)
	if s == nil {
		return dc.rawExtra(x.payload().text)
	}
	return wordPart(s, x, partExtra, func(e *wordParts) string { return e.extra }, func(bool) string {
		return strings.Clone(extraOf(x.payload().text))
	}, func(e *wordParts, v string) { e.extra = v }, func(v string) int64 { return headerBytes + int64(len(v)) })
}

// setSearched is searched(x, keep) of an x whose set s keeps it: the texts
// with x's stamps or clock reads blanked, as copies, by the recording; a
// payload with none to blank is searched as its own text (the set's store
// of payloads, bodyStore) and payloadExtra, and s keeps only that it is
// (held).
func (dc *DriftCache) setSearched(s *setDerived, x Raw, keep bool) [2]string {
	k := keepIndex(keep)
	plain := heldPart(s, x, byKeep(partPlain, keep), func(e *heldParts) bool { return e.plain[k] }, func(bool) bool {
		y := x
		if keep {
			y = x.withoutStamps()
		}
		return len(clockKeys(y)) == 0 && len(y.payload().stamps) == 0
	}, func(e *heldParts, v bool) { e.plain[k] = v }, func(bool) int64 { return 0 })
	if plain {
		return [2]string{x.payload().text, dc.payloadExtra(x)}
	}
	return wordPart(s, x, byKeep(partSearched, keep), func(e *wordParts) [2]string { return e.searched[k] }, func(bool) [2]string {
		y := x
		if keep {
			y = x.withoutStamps()
		}
		yb := y.payload()
		return blankedPair(yb.text, yb.stamps, clockKeys(y))
	}, func(e *wordParts, v [2]string) { e.searched[k] = v }, func(v [2]string) int64 { return headerBytes + pairBytes(v) })
}
