package learn

import (
	"container/list"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"go.keploy.io/server/v3/pkg/models"
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
// It keeps them in three tiers, within one budget for the set (setBudget,
// from the agent's memory limit), which the three draw on together:
//
//   - held: a payload's instants and their places, its stamps, and a filter
//     of its words of a fixed size (wordFilter). The learn step reads
//     these of every recording before a call, for each
//     changed value of each call, in the same order, so they are kept for
//     the set, never let go to make room for another recording's: what a
//     recording holds of them is small (about 64 bytes an instant and 2 KiB
//     at most of filters), and the set's grow with its recordings. Past the
//     set's budget a recording's are kept by none and read through the
//     test's cache of texts (textCache), once per test, as with no set; the
//     recordings kept stay kept. A set that let the recording read least
//     recently go was read anew on every call past its budget (api-server's
//     round: 14 813 recordings, 335 MB of payloads, 76-86% of the agent's CPU).
//
//   - words: a payload's words and texts (searchedTokens, readAnswerWords,
//     payloadExtra, setSearched), the recording read least recently let go
//     first, for this tier's parts or the others'. A payload's words take
//     more memory than its text, so a set's words kept whole grew with the set's payloads
//     (1.8 GB over 256 recordings of 1 MiB). A recording let go is derived
//     again where a rule next reads it; the word search reads them only
//     where a payload's filter may hold a value's words (mayHold).
//
//   - forms: what a payload's readings decode (decoded: its escapes, its
//     quoted-printable form, its encoded runs' text), kept for the set as
//     held is. Its words and texts (words) are built from them where
//     they were let go, by case-folding alone, so a payload is decoded
//     once for the set, never once per call: decoding
//     it again each time its words were let go was 30% of the agent's CPU
//     on api-server's round, and its instants' reads (instantSpans) 75%
//     before the test that timed out.
//
//   - drifts: what each value an earlier place holds drifted, bounded by no
//     call (driftOf), kept as held is. Every later test reads each place of
//     the run's (an earlier test's call) as a source of each changed value
//     of its calls, so without them each test built every earlier place's
//     drift again at its first call: 410 MB and 310 ms at 20 000 places.
//
// The budget is the set's, not a tier's: a tier of a fixed size below the
// set's working set kept a part of each recording and read the rest anew on
// every call. On api-server's round (15 589 recordings) the held and forms
// tiers (128 and 64 MiB) were full and the words tier (32 MiB, the least
// recently read let go) kept little a call read again, so a late call
// re-decoded, re-split and re-read the instants of the payloads it read, on
// one core on the answer path, past the client's 30 s: in a late profile,
// decoding again was 37% of the agent's CPU and reading instants again 31%.
// Held and forms let none go, so a set past its budget keeps the parts read
// first and reads the rest once per test (textCache); words let go of the
// recordings read least recently to make room for any tier's part.
//
// A part larger than its whole budget is handed to its reader and kept by
// none. Handlers of one set read it at once: where one is reading a part of
// a recording, another that needs it waits for it (derive), so a part is
// read once, not once per handler. Before its rules read them, a call reads
// what they read of each payload (warm), taking only the recordings no
// other handler is reading, so handlers needing the set's payloads at once
// read different ones at the same time, not each in turn behind one reader.
// The agent reads them as it stages the set (WarmSet), in the background,
// so a call finds the set's payloads read, and reads only those the
// staging has not read yet.
type setDerived struct {
	mu sync.Mutex
	// budget is the bytes the four tiers keep in all (setBudget).
	budget *budget
	held   tier[heldParts]
	words  tier[wordParts]
	forms  tier[formParts]
	drifts tier[driftPart]
	// reading are the parts being read (derive), by recording and part:
	// closed when read.
	reading map[readingKey]chan struct{}
	// warming are the recordings a handler took to read (warm), by
	// recording: closed when read.
	warming map[string]chan struct{}
	// waits is how many times a handler waited for another's read, for the
	// rows.
	waits int
	// staged and called are the payloads read for the set (warmOne) by its
	// staging (WarmSet) and by its calls, and finished those of its calls'
	// the staging read the rest of (stageRest), for the rows and the
	// agent's log (SetUse).
	staged, called, finished int
	// reads are the parts of the set's recordings read (derive), by tier
	// and part, for the rows.
	reads map[readTally]int
	// bodies are the payloads of the set's recordings its readers loaded
	// (bodyStore), one more tier on budget.
	bodies *bodyStore
	// offsets are the set's places the offset rule reads, by place and
	// moment (offsetIndex), within offsetBudget: its handlers share them.
	offsets *offsetIndex
	// run is the bytes the tracker's run keeps of the set's tests' places
	// (Tracker.keepRun), on budget (keepRun).
	run int64
}

// driftPart is what setDerived keeps of one drift (drifts).
type driftPart struct{ d *valueDrift }

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
	keys     []instantKey  // partKeys
}

// formParts are what setDerived keeps of a recording's decoded forms
// (forms): of its text, and of its text with its stamps blanked
// (decodedOf).
type formParts struct {
	d [2]decoded
}

// wordParts are what setDerived keeps of a recording within its budget
// (words).
type wordParts struct {
	tokens   [2][]map[string]bool // by keep
	answer   [2][]map[string]bool // by keep
	extra    string
	searched [2][2]string // by keep, where not plain (partPlain)
}

// part is one part a tier keeps of a recording, a bit of slot.has: wide
// enough for each part by keep (byKeep).
type part uint32

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
	_
	partKeys
	partDecoded // by whether the stamps are blanked: partDecoded<<1
	_
	partDrift
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
	// cap is the bytes this tier keeps at most: the set's budget, or a
	// row's cap on this tier (heldBudget, derivedBudget, formsBudget).
	cap, used int64
	// set is the set's budget, which every tier's parts draw on.
	set *budget
	lru bool
	// full: a tier that does not let go was past its budget for a part,
	// and keeps no more (room).
	full  bool
	order *list.List // of *slot[P], the most recently read first (lru)
	by    map[string]*slot[P]
}

func newTier[P any](cap int64, set *budget, lru bool) tier[P] {
	if cap < 0 || cap > set.limit {
		cap = set.limit
	}
	return tier[P]{cap: cap, set: set, lru: lru, order: list.New(), by: map[string]*slot[P]{}}
}

// budget is the bytes a set's tiers keep in all: one account, so a part is
// kept while the set is within it, whichever tier the part is of. letGo
// frees need bytes for a part: the words tier lets go of its recordings
// read least recently, then, where that is not enough, the payloads tier
// (bodyStore) of its own (newSetDerived).
type budget struct {
	limit, used int64
	letGo       func(need int64)
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

// keep puts p, of size bytes, into id's slot (put), within the tier's cap
// and the set's budget. Where the tier lets go (words, payloads), it then
// has the budget let go of what the set is past it (budget.letGo: words
// first), and lets go of its own recordings read least recently until both
// hold; where it does not, it first has the budget let go of what the part
// needs past the set's budget, and keeps none past it (full). A part past
// the whole of either is kept by none and lets go of nothing. A
// recording's first part counts its slot.
func (t *tier[P]) keep(id string, p part, size int64, put func(*P)) {
	sl := t.by[id]
	if sl != nil && sl.has&p != 0 {
		return
	}
	if sl == nil {
		size += slotBytes + int64(len(id))
	}
	if size > t.cap || size > t.set.limit {
		return
	}
	if !t.lru {
		if t.used+size > t.cap {
			t.full = true
			return
		}
		if need := t.set.used + size - t.set.limit; need > 0 && t.set.letGo != nil {
			t.set.letGo(need)
		}
		if t.set.used+size > t.set.limit {
			t.full = true
			return
		}
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
	t.set.used += size
	if !t.lru {
		return
	}
	if need := t.set.used - t.set.limit; need > 0 && t.set.letGo != nil {
		t.set.letGo(need)
	}
	for t.used > t.cap || t.set.used > t.set.limit {
		if !t.letGoLast() {
			break
		}
	}
}

// letGo lets go of the tier's recordings read least recently until need
// bytes are freed or it keeps none (budget.letGo), and is what it freed.
func (t *tier[P]) letGo(need int64) (freed int64) {
	for freed < need {
		before := t.used
		if !t.letGoLast() {
			return freed
		}
		freed += before - t.used
	}
	return freed
}

// letGoLast lets go of the recording the tier read least recently; false
// where it keeps none.
func (t *tier[P]) letGoLast() bool {
	last := t.order.Back()
	if last == nil {
		return false
	}
	old := last.Value.(*slot[P])
	t.order.Remove(last)
	delete(t.by, old.id)
	t.used -= old.size
	t.set.used -= old.size
	return true
}

// drop lets go of all the tier keeps, and keeps nothing more.
func (t *tier[P]) drop() {
	t.set.used -= t.used
	t.used, t.cap, t.full = 0, 0, true
	t.by = map[string]*slot[P]{}
	t.order.Init()
}

// room is whether the tier may keep another part: one that lets go always
// may; one that does not, until a part did not fit in what was left of its
// budget (full), so a part it will not keep is not read as if it would on
// every call.
func (t *tier[P]) room() bool { return t.lru || !t.full }

// tierOf names one of setDerived's tiers in a readingKey.
type tierOf uint8

const (
	tierWords tierOf = iota
	tierHeld
	tierForms
)

// readTally is a tier's part, as reads counts its reads.
type readTally struct {
	tier tierOf
	part part
}

type readingKey struct {
	id   string
	part part
	tier tierOf
}

// derive is part p of x's recording that t keeps (get), read by read where
// t keeps none and kept by t (put, of size bytes). Where another handler is
// reading it, it waits for that read and takes what it kept; a part that read
// was not kept (past t's budget) is read again by each. read is told whether
// t may keep what it reads (room): a part t will not keep is read through the
// test's cache of texts, once per test.
func derive[P, V any](s *setDerived, t *tier[P], in tierOf, id string, p part, get func(*P) V, read func(kept bool) V, put func(*P, V), size func(V) int64) V {
	k := readingKey{id, p, in}
	for waited := false; ; waited = true {
		s.mu.Lock()
		if sl := t.find(id, p); sl != nil {
			v := get(&sl.p)
			s.mu.Unlock()
			return v
		}
		room := t.room()
		if !room || waited {
			s.reads[readTally{in, p}]++
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
		s.reads[readTally{in, p}]++
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

// setBudget is the bytes one set's tiers keep in all (held, words, forms
// and its payloads, bodyStore, together): learnBudget where a row sets it,
// else a quarter of the agent's memory limit (memoryLimit), at most
// maxSetBudget and at least minSetBudget, and minSetBudget where no limit
// is known. Each set reads it when it starts, so a limit the agent sets
// after it started (its memory guard's GOMEMLIMIT) holds from its next set.
func setBudget() int64 {
	if learnBudget > 0 {
		return learnBudget
	}
	return budgetOf(memoryLimit())
}

// budgetOf is setBudget of an agent whose memory limit is limit bytes (0:
// none known). A quarter, because the Go heap grows to about twice what it
// keeps before it collects (GOGC 100): a set's data at a quarter of the
// limit takes about half of it at its peak, and leaves the other half to
// the rest of the agent (on api-server's lane, 12 GiB: 3 GiB kept, against
// a peak of 2.7 GB for the whole agent with the old tiers' 224 MiB).
func budgetOf(limit int64) int64 {
	b := limit / 4
	if b > maxSetBudget {
		b = maxSetBudget
	}
	if b < minSetBudget {
		b = minSetBudget
	}
	return b
}

// learnBudget, where a row sets it (>0), is setBudget; 0 in keploy itself.
var learnBudget int64

// The bounds of setBudget: at most 4 GiB of a set's derived data, however
// much memory the agent has, and at least 64 MiB.
const (
	maxSetBudget = 4 << 30
	minSetBudget = 64 << 20
)

// derivedBudget, formsBudget and heldBudget, where a row sets them (0 or
// more), cap the words, forms and held tiers within the set's budget;
// noCap, no cap of a tier's own, in keploy itself.
var derivedBudget, formsBudget, heldBudget, driftBudget int64 = noCap, noCap, noCap, noCap

const noCap = -1

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
	b := &budget{limit: setBudget()}
	s := &setDerived{budget: b, held: newTier[heldParts](heldBudget, b, false), words: newTier[wordParts](derivedBudget, b, true), forms: newTier[formParts](formsBudget, b, false), drifts: newTier[driftPart](driftBudget, b, false), reading: map[readingKey]chan struct{}{}, reads: map[readTally]int{}, warming: map[string]chan struct{}{}, offsets: newOffsetIndex(offsetBudget)}
	s.bodies = newSetBodyStore(&s.mu, b, bodyBudget)
	// The words go first: a recording's words are read again from its
	// payload, where one let go is decoded again (a JSON encode and decode
	// of the whole recording) before any of its parts can be read.
	b.letGo = func(need int64) {
		if freed := s.words.letGo(need); freed < need {
			s.bodies.kept.letGo(need - freed)
		}
	}
	return s
}

// drop lets go of all s keeps, once its set is over (Tracker.NewSet), and
// keeps nothing more: a handler still reading the earlier set reads what it
// needs once per test (textCache), a recording's payload once per test
// (DriftCache.RawOf) or once per Raw (LazyRawOf), and the agent holds one
// set's budget at a time, not one for each set a handler still reads.
func (s *setDerived) drop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held.drop()
	s.words.drop()
	s.forms.drop()
	s.drifts.drop()
	s.bodies.kept.drop()
	s.bodies.over = true
}

// keepsPayloads reports whether the set's store keeps the payloads a test
// reads (DriftCache.RawOf): not once the set is over (drop) or where a row
// gives the store no room, nor once a tier that lets none go was refused a
// part (held or forms full). Past that, the words and payloads have let go
// of all they kept, and a payload read lazily would be read again at each
// read, where a test's own reads it once: the set's payloads are then read
// once per test, as each part the set keeps no more is.
func (s *setDerived) keepsPayloads() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies.kept.cap > 0 && !s.bodies.over && s.held.room() && s.forms.room()
}

// usedBytes is the bytes s counts as kept in all its tiers, its payloads'
// among them, each tier's count, not the budget's, for the rows.
func (s *setDerived) usedBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held.used + s.words.used + s.forms.used + s.drifts.used + s.bodies.kept.used
}

// keepRun counts size bytes more of the run's places (Tracker.keepRun) on
// the set's budget, and reports whether they are within the run's share of
// it (runCap); past it, it counts none. The run has the first claim on its
// share: the tiers that let go (words, payloads) make room for it
// (budget.letGo), and those that do not keep no more once the set is past
// its budget. So a set whose never-let-go tiers filled its budget at its
// staging (WarmSet) still keeps its tests' values, and holds an eighth more
// than its budget at most; a run with no room at all would refuse every
// value a later test sent.
func (s *setDerived) keepRun(size int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.run+size > s.budget.limit/runShare {
		return false
	}
	s.run += size
	s.budget.used += size
	if need := s.budget.used - s.budget.limit; need > 0 && s.budget.letGo != nil {
		s.budget.letGo(need)
	}
	return true
}

// runCap is the run's share of the set's budget (keepRun), for its reason.
func (s *setDerived) runCap() int64 { return s.budget.limit / runShare }

// runShare: the run keeps its places within an eighth of the set's budget
// (keepRun): 8 MiB at the least budget, 512 MiB at the most.
const runShare = 8

// driftOf is what the drifted value (rec, live) changed, read with the lead
// where lead holds, bounded by no call (newDrift), kept for the set (drifts)
// where s is a tracker's. It reads no zone: of a call not known, a clock read
// is bounded by none (readNoLaterThan), the only rule that reads the zone, so
// such a drift is the same in every zone (TestADriftBoundedByNoCallIsTheSameInEveryZone).
func (s *setDerived) driftOf(rec, live string, lead bool) *valueDrift {
	read := func(bool) *valueDrift { return newDrift(rec, live, nil, lead, time.Time{}, nil) }
	if s == nil {
		return read(false)
	}
	// The lead and the value's two sides, the recorded one by its length:
	// no two drifts share an id.
	id := make([]byte, 0, len(rec)+len(live)+32)
	id = strconv.AppendBool(id, lead)
	id = strconv.AppendInt(append(id, ' '), int64(len(rec)), 10)
	id = append(append(append(id, ' '), rec...), live...)
	// Two handlers that build one drift at once each build it, as a test's
	// own cache does (DriftCache.ofIn): it is no recording's part to wait
	// for (derive), and either is the drift.
	var d *valueDrift
	s.mu.Lock()
	if sl := s.drifts.find(string(id), partDrift); sl != nil {
		d = sl.p.d
	}
	s.mu.Unlock()
	if d != nil {
		return d
	}
	d = read(true)
	s.mu.Lock()
	if s.drifts.room() {
		s.drifts.keep(string(id), partDrift, driftBytes(d), func(e *driftPart) { e.d = d })
	}
	s.mu.Unlock()
	return d
}

// driftBytes is about what drift d holds (newDrift): its texts in each
// form, its words, its pieces and their words, its ids' words and its
// instants.
func driftBytes(d *valueDrift) int64 {
	n := int64(unsafe.Sizeof(*d)) + int64(len(d.rec)+len(d.live)+len(d.decRec)+len(d.decLive)+len(d.encodedRec)+len(d.unparsed))
	for _, f := range d.forms {
		n += headerBytes + int64(len(f[0])+len(f[1]))
	}
	for _, ws := range [][]map[string]bool{d.lost, d.gained, d.movedRec, d.movedLive, d.recTok, d.liveTok, {d.clock.rec, d.clock.live, d.wholes}} {
		for _, w := range ws {
			n += wordsBytes(w)
		}
	}
	for i, p := range d.pieces {
		n += headerBytes + int64(len(p.rec)+len(p.live))
		for _, side := range d.pieceWords[i] {
			for _, w := range side {
				n += wordBytes + int64(len(w))
			}
		}
	}
	for _, side := range d.idParts {
		for w, others := range side {
			n += wordBytes + int64(len(w)) + wordsBytes(others)
		}
	}
	for _, side := range d.oneWord {
		n += wordsBytes(side)
	}
	// idBits and otherSide hold the words of idParts and of the word maps
	// above: their entries, not the words again.
	for i := range d.idBits {
		n += int64(len(d.idBits[i]))*(wordBytes+8) + int64(len(d.otherSide[i]))*headerBytes
	}
	n += int64(len(d.clock.reads))*int64(unsafe.Sizeof(models.ClockRead{})) + int64(len(d.instants))*int64(unsafe.Sizeof(driftedInstant{}))
	n += int64(len(d.recKeys)+len(d.liveKeys))*(wordBytes+int64(unsafe.Sizeof(instantKey{}))) + momentBytes*int64(len(d.recMoments)+len(d.liveMoments))
	return n
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
	return derive(s, &s.held, tierHeld, x.id, p, get, read, put, size)
}

// wordPart is part p of x's recording that s keeps within its budget
// (words); see derive.
func wordPart[V any](s *setDerived, x Raw, p part, get func(*wordParts) V, read func(kept bool) V, put func(*wordParts, V), size func(V) int64) V {
	return derive(s, &s.words, tierWords, x.id, p, get, read, put, size)
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
		return copyWords(tokensWith(x, keep, func(y Raw) (string, decoded) { return dc.decodedOf(s, y) }))
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
// tells which values the search can find in x, and which words x's words
// (searchedTokens) cannot hold, without reading its text or its words
// (mayHold, mayHoldWords): searchedTokens reads the same forms, each blanked
// by itself where searched blanks the other forms joined by a line break,
// which no instant's layout holds and no word byte is, so each form holds
// the same instants and the same words of letters and digits either way.
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
		pair := dc.searched(x, keep)
		texts := pair[:]
		n := 0
		for _, text := range texts {
			eachWord(text, func(string) { n++ })
		}
		f := newWordFilter(n)
		for _, text := range texts {
			eachWord(text, f.add)
		}
		return f
	}, func(e *heldParts, v wordFilter) { e.filter[k] = v }, func(v wordFilter) int64 { return headerBytes + 8*int64(len(v)) })
	return out, out != nil
}

// eachWord hands each word of s as isWord reads it (segmentsBy), in order,
// keeping none of them.
func eachWord(s string, each func(string)) {
	for i := 0; i < len(s); {
		if !isWord(s[i]) {
			i++
			continue
		}
		j := i + 1
		for j < len(s) && isWord(s[j]) {
			j++
		}
		each(s[i:j])
		i = j
	}
}

// mayHoldWords reports whether x's words (searchedTokens(x, keep)) may hold
// any of words, each by its runs of letters and digits (runs[i]:
// wordsBy(words[i], isWord)), as x's filter tells (searchWords): a word of
// any reading (readings) is a run of bytes that each continue a word of
// letters and digits or a wider one, with none beside it, so each of its
// runs of letters and digits is a word of letters and digits of the same
// form, which the filter holds. A word no run of which the filter lacks,
// one of no letter or digit among them, and any word of an x the set keeps
// no filter of, may.
func (dc *DriftCache) mayHoldWords(x Raw, keep bool, runs [][]string) bool {
	words, ok := dc.searchWords(x, keep)
	if !ok {
		return true
	}
	for _, r := range runs {
		if mayHold(words, r) {
			return true
		}
	}
	return false
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
		text, d := dc.decodedOf(s, x.withoutStamps())
		return strings.Clone(d.extraOf(text))
	}, func(e *wordParts, v string) { e.extra = v }, func(v string) int64 { return headerBytes + int64(len(v)) })
}

// setSearched is searched(x, keep) of an x whose set s keeps it: the texts
// with x's stamps or clock reads blanked, as copies, by the recording; a
// payload with none to blank is searched as its own text (the set's store
// of payloads, bodyStore) and payloadExtra, and s keeps only that it is
// (held).
func (dc *DriftCache) setSearched(s *setDerived, x Raw, keep bool) [2]string {
	k := keepIndex(keep)
	if dc.setPlain(s, x, keep) {
		return [2]string{x.payload().text, dc.payloadExtra(x)}
	}
	return wordPart(s, x, byKeep(partSearched, keep), func(e *wordParts) [2]string { return e.searched[k] }, func(bool) [2]string {
		y := x
		if keep {
			y = x.withoutStamps()
		}
		t, d := dc.decodedOf(s, y)
		return blankedPairOf(t, d.extraOf(t), clockKeys(y))
	}, func(e *wordParts, v [2]string) { e.searched[k] = v }, func(v [2]string) int64 { return headerBytes + pairBytes(v) })
}

// setPlain is whether searched(x, keep) of an x whose set s keeps it is
// its own text and payloadExtra (no stamp or clock read to blank), read
// once for the set.
func (dc *DriftCache) setPlain(s *setDerived, x Raw, keep bool) bool {
	k := keepIndex(keep)
	return heldPart(s, x, byKeep(partPlain, keep), func(e *heldParts) bool { return e.plain[k] }, func(bool) bool {
		y := x
		if keep {
			y = x.withoutStamps()
		}
		return len(clockKeys(y)) == 0 && len(y.payload().stamps) == 0
	}, func(e *heldParts, v bool) { e.plain[k] = v }, func(bool) int64 { return 0 })
}

// decodedOf is the text y's searches decode, its stamps blanked
// (blankStamps; withoutStamps keeps them), and what its readings decode of
// it (decoded), decoded once for the set s, which keeps y's (forms): its
// words and other forms are read from them, never by decoding it again.
// Past the set's budget each is decoded once per test (textCache).
func (dc *DriftCache) decodedOf(s *setDerived, y Raw) (string, decoded) {
	yb := y.payload()
	text, i := yb.text, 0
	if len(yb.stamps) > 0 {
		text, i = blankStamps(text, yb.stamps), 1
	}
	return text, derive(s, &s.forms, tierForms, y.id, partDecoded<<i, func(e *formParts) decoded { return e.d[i] }, func(kept bool) decoded {
		if !kept {
			return derivedOfText(dc, textDecoded, text, func(own string) decoded { return decodeForms(own) }, func(d decoded) int64 { return d.size() })
		}
		return decodeForms(text).clone()
	}, func(e *formParts, d decoded) { e.d[i] = d }, func(d decoded) int64 { return d.size() })
}

// searchedKeys are the keys of the instants searched(x, false) holds, in
// its text or its other forms, read once for the set where dc keeps x's
// (setOf), sorted, each once: whether it holds a moment as an instant of
// its own (holdsExact) is told without reading its texts again. ok is
// false where the set keeps none.
func (dc *DriftCache) searchedKeys(x Raw) (out []instantKey, ok bool) {
	s := dc.setOf(x)
	if s == nil {
		return nil, false
	}
	return heldPart(s, x, partKeys, func(e *heldParts) []instantKey { return e.keys }, func(kept bool) []instantKey {
		var keys []instantKey
		if dc.setPlain(s, x, false) {
			// Its searched texts are its text and its other forms, whose
			// instants payloadMoments holds.
			for _, m := range dc.payloadMoments(x) {
				keys = append(keys, m.key)
			}
		} else {
			spans := instantSpans
			if !kept {
				spans = dc.spansOf
			}
			for _, text := range dc.searched(x, false) {
				if text == "" {
					continue
				}
				for _, sp := range spans(text) {
					if sp.keyed {
						keys = append(keys, sp.key)
					}
				}
			}
		}
		slices.SortFunc(keys, func(a, b instantKey) int {
			if a.at != b.at {
				if a.at < b.at {
					return -1
				}
				return 1
			}
			switch {
			case a.day == b.day:
				return 0
			case b.day:
				return -1
			}
			return 1
		})
		return slices.Compact(keys)
	}, func(e *heldParts, v []instantKey) { e.keys = v }, func(v []instantKey) int64 { return headerBytes + 16*int64(len(v)) }), true
}

// mayHoldInstant reports whether x's searched texts (searched(x, false)) may
// hold m as an instant of its own (holdsExact), as the set's keys of them
// tell (searchedKeys); any x the set keeps none of may.
func (dc *DriftCache) mayHoldInstant(x Raw, m moment) bool {
	keys, ok := dc.searchedKeys(x)
	return !ok || slices.Contains(keys, m.key)
}

// warmed stands for each recording a handler has read (warm).
var warmed = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

// warm reads, for each recording of raw the set keeps (setOf), what the
// rules of every call read of its payload (warmOne), once for the set. A
// handler reads, in raw's order, each recording no other handler has
// taken, and only once none is left to take waits for those others are
// reading. So handlers that need a set's payloads at once read different
// ones at the same time, instead of each waiting in turn on the one reading
// the first (derive): one payload read is never a wait for every handler.
// The set's staging (WarmSet) is one more reader, taking one recording at a
// time: a call waits for it only on a recording of its own the staging is
// reading, and a call after the staging read the set's takes none, finding
// each read under one look at the set.
func (dc *DriftCache) warm(raw []Raw) {
	if dc == nil || dc.set == nil {
		return
	}
	s := dc.set
	s.mu.Lock()
	var left []Raw
	for _, x := range raw {
		if dc.setOf(x) == nil {
			continue
		}
		if ch, ok := s.warming[x.id]; ok && ch == warmed {
			continue
		}
		left = append(left, x)
	}
	s.mu.Unlock()
	var others []chan struct{}
	for _, x := range left {
		ch, mine := s.take(x)
		if ch == nil {
			break
		}
		if !mine {
			if ch != warmed {
				others = append(others, ch)
			}
			continue
		}
		dc.warmOne(s, x, ch, false)
	}
	for _, ch := range others {
		select {
		case <-ch:
		default:
			s.mu.Lock()
			s.waits++
			s.mu.Unlock()
			<-ch
		}
	}
}

// take is the channel closed once x's payload is read (warmOne), and
// whether the caller is to read it: the first to ask for x takes it. nil
// where the set keeps no more (held has no room: past its budget, or after
// its NewSet), where nobody reads it for the set.
func (s *setDerived) take(x Raw) (ch chan struct{}, mine bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.held.room() {
		return nil, false
	}
	ch, taken := s.warming[x.id]
	if !taken {
		ch = make(chan struct{})
		s.warming[strings.Clone(x.id)] = ch
	}
	return ch, !taken
}

// warmOne reads what the rules of every call read of x's payload: its
// decoded forms and instants (payloadMoments), its words' filter
// (searchWords) and its searched instants (searchedKeys), then lets the
// handlers waiting for it go, whatever the reads do. staged says the set's
// staging read it (WarmSet), not a call. The staging also reads what a
// set's first drifted call reads of every recording before it, whatever
// its drift: its keyed places (setPlaces, the offset rule's), its answer's
// stamps (stampTexts), and its fields' instants as the echo reads them
// (fieldMoments, fieldForms), so no call reads a part of every recording
// on its answer path (api-server's round: 15 589 recordings, read by the
// set's first drifted call while its client waited). A call's own warm
// reads only the three the rules read of every payload of every call; it
// reads the rest where its rules reach them, as with no staging, and the
// staging reads them where it comes to a recording a call took first
// (stageRest).
func (dc *DriftCache) warmOne(s *setDerived, x Raw, ch chan struct{}, staged bool) {
	defer func() {
		s.mu.Lock()
		s.warming[x.id] = warmed
		if staged {
			s.staged++
		} else {
			s.called++
		}
		s.mu.Unlock()
		close(ch)
	}()
	dc.payloadMoments(x)
	dc.searchWords(x, false)
	dc.searchedKeys(x)
	if staged {
		dc.stagedParts(x)
	}
}

// stagedParts reads the parts of x's payload the staging reads beyond a
// call's warm (warmOne): its keyed places, its answer's stamps and its
// fields' instants as the echo reads them.
func (dc *DriftCache) stagedParts(x Raw) {
	dc.setPlaces(x)
	dc.stampTexts(x)
	dc.fieldMoments(x)
	dc.fieldForms(x)
}

// stageRest is the staging's read (WarmSet) of a recording a call took
// first (warm): the call read only the three parts every call's rules read
// of it, and the staging reads the rest (stagedParts), so no later call
// reads one of them on its answer path. Before, the staging passed over
// such a recording, and the set's next drifted call read those four parts
// of it while its client waited. It does not wait for the call's read to
// end: a part the call, or any other, is reading it waits for, and takes
// what that read keeps (derive), so each part is read once.
func (dc *DriftCache) stageRest(s *setDerived, x Raw) {
	dc.stagedParts(x)
	s.mu.Lock()
	s.finished++
	s.mu.Unlock()
}
