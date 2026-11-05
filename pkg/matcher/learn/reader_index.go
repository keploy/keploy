package learn

import (
	"cmp"
	"encoding/json"
	"fmt"
	"hash/maphash"
	"index/suffixarray"
	"io"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
	"weak"
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
// once what the set's budget lets it keep, and what one question reads: the
// text and a few times the text in its forms and its largest token (about 6x
// on one long string, under 1x on a JSON answer of many small fields). That
// holds however many places it asks about, and it decides as a small set
// does: the budget bounds what is kept, never what is read. A place that is a
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
// read. peak is the most a question of fields (fieldsWith) held at once by
// the index's own count: what it keeps, the text read now, the view as it
// is built, and the field it reads with that field's forms.
type readerIndex struct {
	dc                 *DriftCache
	set                *setDerived
	budget, used, peak int64
	seed               maphash.Seed
	texts              map[uint64][]*readerEntry
	unreadTexts        int
	// momentAsks counts the instants holdsMoment asks byMoment of: what
	// keeps a call's work near linear in the instants it decides.
	momentAsks int
	// filters are the set's filters of its recordings' words (filtersOf);
	// nil where the call is no set's.
	filters *setFilters
}

// readerEntry is what the index keeps of one text: its other declared forms,
// its base64 runs and its instants, each kept where it fits the budget, and
// then the text itself (counted in the budget once) to tell it from another
// text of its hash. An entry that keeps no answer keeps no byte of its text
// either, so a call that asks about many large recordings holds at once only
// what fits the budget: the caller's handle holds the text while it asks (or,
// of a recording, reads it per question), and nothing here holds it after.
type readerEntry struct {
	n    int
	text string
	// forms: the text's other declared forms (textForms past the text
	// itself), one per line, where a word is looked for besides the text.
	forms     answer[string]
	runs      answer[map[string]bool]
	spans     answer[[]instant]
	formSpans answer[momentIndex]
	fields    answer[fieldWords]
	subs      answer[*suffixarray.Index]
	// past: an answer of the text did not fit what was left of the
	// budget, which only fills, so the entry keeps none from then on: each
	// question is answered from the text read again.
	past bool
}

// answer is one kind of answer an entry keeps: v, once kept (ok). big:
// building it passed the room left of the index's cap, which only shrinks
// within a call, so each later question of its kind is a scan, building
// none again.
type answer[V any] struct {
	v       V
	ok, big bool
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
	// scans counts the questions contains answered by a scan of the text
	// through this handle, before the text's suffix array.
	scans int
	// filter is the set's entry for the filter of the text's words
	// (setFilter), once asked; filterKey the bytes its key keeps there.
	filter    *filterEntry
	filterKey int64
}

// newReaderIndex is an empty index for one call, reading through dc's
// caches (nil: none).
func newReaderIndex(dc *DriftCache) *readerIndex {
	x := &readerIndex{dc: dc, budget: readerBudgetBytes(), seed: maphash.MakeSeed(), texts: map[uint64][]*readerEntry{}}
	if dc != nil {
		x.set = dc.set
	}
	if x.set != nil {
		x.filters = filtersOf(x.set)
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
	if x.used+n > x.budget || x.set != nil && !setClaim(x.set, n) {
		return false
	}
	x.used += n
	return true
}

// setClaim counts n bytes more on s's one budget, as a tier that lets none
// go keeps there (tier.keep): it first has the set let go of what it is past
// its budget (budget.letGo), and past the budget still it counts none.
func setClaim(s *setDerived, n int64) bool {
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
	return true
}

// release gives the set's budget back what the index keeps, once its call
// is over (the caller's: defer x.release()), and lets go of it, every
// entry's answers included, those a caller's handle still reaches: the
// index keeps nothing more, and a question asked after, through any
// handle, reads the text again.
func (x *readerIndex) release() {
	if s := x.set; s != nil && x.used > 0 {
		s.mu.Lock()
		s.budget.used -= x.used
		s.mu.Unlock()
	}
	for _, es := range x.texts {
		for _, e := range es {
			*e = readerEntry{n: e.n}
		}
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
// its recording's fields), given the room left of the index's cap once the
// text is counted (-1: none, past the budget). That answer is kept, with the
// text that tells it apart, where both fit what is left of the budget. of
// reports false where building it would pass that room (it counts what it
// builds as it goes, and stops there). Past the budget, or past the room,
// scan (where given) answers the caller's one question from the text read
// now, and builds no answer (scanned true); else the answer is the caller's
// for this question alone. Nothing here holds either, so the same question
// asked again reads the text again (through main's caches, which keep what
// fits theirs). ok is false only where the recording was not read.
func indexOnce[V any](t *readerText, slot func(*readerEntry) *answer[V], of func(s string, fields []rawField, room int64) (V, bool), size func(V) int64, scan func(string, []rawField)) (v V, scanned, ok bool) {
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
	room := int64(-1)
	if !a.big && x.mayKeep(e, s) {
		room = x.budget - x.used - e.own(s)
	}
	if room >= 0 || scan == nil {
		var fits bool
		if v, fits = of(s, fields, room); fits && room >= 0 {
			if x.claim(size(v) + e.own(s)) {
				e.text = s
				a.v, a.ok = v, true
			} else {
				e.past = true
			}
		}
		if fits {
			return v, false, true
		}
		a.big = true
	}
	scan(s, fields)
	var none V
	return none, true, true
}

// uncapped is of, for an answer built whole however little room is left: what
// it builds is counted once built (claim), as main's caches count theirs.
func uncapped[V any](of func(string, []rawField) V) func(string, []rawField, int64) (V, bool) {
	return func(s string, fields []rawField, _ int64) (V, bool) { return of(s, fields), true }
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

// formsOf are s's other declared forms (textForms past s itself), one per
// line, s being the text read now: a recording's as the set keeps them,
// derived once for the set from s (payloadExtra: the recording is not read
// again for them), a handed text's through the test's cache of texts
// (rawExtra).
func (t *readerText) formsOf(s string) string {
	if t.raw == nil {
		return t.x.dc.rawExtra(s)
	}
	now := *t.raw
	now.src, now.rText, now.rStamps, now.rRead = nil, s, nil, true
	return t.x.dc.payloadExtra(now)
}

// spansOf are the text's instants (instantSpans), read once for the call
// where they fit the budget; ok false where the recording was not read.
func (t *readerText) spansOf() ([]instant, bool) {
	v, _, ok := indexOnce(t, func(e *readerEntry) *answer[[]instant] { return &e.spans }, uncapped(func(s string, _ []rawField) []instant { return t.x.dc.spansOf(s) }), spansBytes, nil)
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

// containsScans is how many questions of a text contains answers through
// one handle by a scan of it before it reads the text's suffix array:
// building one costs some tens of scans, so a text asked of a few words is
// only scanned, and one asked of thousands (an answer of thousands of
// instants, each changed) is scanned this often at most.
const containsScans = 16

// contains reports whether the text holds w anywhere, as strings.Contains
// of it does: by a scan of the text read now for the handle's first
// containsScans questions, then from the text's suffix array, built once
// for the call where it fits what is left of the index's cap, and by a scan
// past it (indexOnce). read is false where the recording was not read.
func (t *readerText) contains(w string) (held, read bool) {
	if t.scans < containsScans {
		t.scans++
		s, ok := t.textNow()
		return ok && strings.Contains(s, w), ok
	}
	ix, scanned, ok := indexOnce(t, func(e *readerEntry) *answer[*suffixarray.Index] { return &e.subs },
		func(s string, _ []rawField, room int64) (*suffixarray.Index, bool) {
			if suffixBytes(len(s)) > room {
				return nil, false
			}
			return suffixarray.New([]byte(s)), true
		}, func(ix *suffixarray.Index) int64 { return suffixBytes(len(ix.Bytes())) },
		func(s string, _ []rawField) { held = strings.Contains(s, w) })
	if scanned || !ok {
		return held, ok
	}
	return w == "" || len(ix.Lookup([]byte(w), 1)) > 0, true
}

// suffixBytes is about what the suffix array of an n-byte text holds: a
// copy of the text and a 4-byte place per byte.
func suffixBytes(n int) int64 { return headerBytes + 5*int64(n) }

// holdsWord reports whether the text holds w as a word in any of main's
// readings (isWord, wordByte, idByte) and any declared form; read is false
// where the recording was not read (held is false then).
func (t *readerText) holdsWord(w string) (held, read bool) {
	return t.wordIn(w, 0, 1, 2)
}

// wordIn reports whether the text holds w as a word in any of the readings
// rs (indices of main's readings) and any declared form, as main's lists of
// the text's words hold it: by a scan of the text and of its other forms
// (formsOf), kept for the call with the text where both fit the budget; past
// it, by a scan of the text read again and its forms (textForms) derived
// for the question, kept by no one, not by the set either. A recording of
// the set is first asked through the set's filter of its words (ruledOut),
// and scanned only where the filter may hold w: a call asks a few words of
// each of thousands of places, so a scan of each place for each word cost
// more than all the rest of the call. read is false where the recording was
// not read.
func (t *readerText) wordIn(w string, rs ...int) (held, read bool) {
	if t.ruledOut(w) {
		return false, true
	}
	var text string
	forms, scanned, ok := indexOnce(t, func(e *readerEntry) *answer[string] { return &e.forms },
		uncapped(func(s string, _ []rawField) string { text = s; return t.formsOf(s) }), stringBytes,
		func(s string, _ []rawField) { held = heldIn(textForms(s), w, rs...) })
	if scanned || !ok {
		return held, ok
	}
	if t.e.forms.ok {
		text = t.e.text
	}
	return heldIn([]string{text, forms}, w, rs...), true
}

// ruledOut reports whether the set's filter of t's recording (setFilter)
// shows that no form of its text holds w as a word in any reading: a run of
// w's letters and digits is none of its words. Where a reading holds w at a
// place, no byte that reading reads as a word byte stands beside w there,
// and each reading reads every letter and digit so: each run of w's letters
// and digits is a whole run of the form's (eachWord), which the filter
// holds. A text the caller handed and a view (no recording's identity) are
// asked through the set's filter of their text's words, by the text itself
// (textEntry). A w of no letter or digit, a text shorter than
// filterTextMin, and a text the set keeps no filter of are never ruled
// out: they are scanned.
func (t *readerText) ruledOut(w string) bool {
	f := t.setFilter()
	if f == nil {
		return false
	}
	filterLookups.Add(1)
	out := false
	eachWord(w, func(run string) { out = out || !f.has(run) })
	return out
}

// filterLookups counts the words asked of recordings' filters (ruledOut),
// for the rows that bound a call's work.
var filterLookups atomic.Int64

// setFilters are the filters a set keeps of its recordings' words
// (setFilter), by recording (Raw.id), of the words of texts with no
// recording's identity (textEntry), by the text (filterTextKey, read with
// seeds), and the words of other tests' recordings handed to its calls
// (outsideWords), by recording. builds, textBuilds and wordsBuilds count
// the filters and the words read, for the rows.
type setFilters struct {
	mu          sync.Mutex
	of          map[string]*filterEntry
	builds      int
	texts       map[filterTextKey]*filterEntry
	seeds       [2]maphash.Seed
	textBuilds  int
	words       map[string][]*wordsEntry
	wordsBuilds int
	// lists, post, nextList and listBuilds are the set's index of its
	// outside recordings' word lists (setLists): each list's entry, by the
	// list (listsKey), and by word the ids of the lists that hold it.
	// postLive is what lists and post count for the live lists, postGone
	// what they keep counted for the slots lists let go emptied
	// (dropLists).
	lists      map[weak.Pointer[[]string]]*listsEntry
	post       map[string][]int32
	nextList   int32
	listBuilds int
	postLive   int64
	postGone   int64
	// runs and nextText are the set's postings of its filters' words
	// (postRuns): by run of letters and digits, the ids of the filter
	// entries whose text holds it, in order.
	runs     map[string][]int32
	nextText int32
}

// filterTextKey is a text by what it holds: its length and two hashes of
// it by the set's two seeds, drawn at random for the set. Two texts that differ
// share a key about once in 2^128 pairs, by chance alone: no text keploy
// is handed can be chosen to share another's, as no one outside the set
// reads its seeds.
type filterTextKey struct {
	n    int
	a, b uint64
}

// Slots of the set's maps of filter entries, at 7/16 of their tables
// filled (as postSlotBytes): a recording's (setFilters.of: its id's header
// and the entry's pointer, 24 bytes, and their control byte), 64 bytes
// past its id's letters; a text's (texts: its key, 24 bytes, and the
// pointer), 96, whose table of 1,024 slots takes whole pages.
const (
	filterIDSlotBytes   = 64
	filterTextSlotBytes = 96
)

// filterTextMin is the shortest text with no recording's identity that a
// set keeps a filter of (textEntry): a shorter one is scanned for each word
// in fewer bytes than finding its filter reads (its two hashes, and the
// set's lock).
const filterTextMin = 256

// filterEntry is one recording's filter, or one text's, built once for the
// set (once): nil where the recording was not read then, or where the
// filter did not fit the set's budget, and the text is scanned in each call
// as before, never read again to build one.
type filterEntry struct {
	once sync.Once
	f    wordFilter
	// byText: the entry is a text's (textEntry), not a recording's.
	byText bool
	// id names the entry in the set's postings of its words (postRuns);
	// -1 where they hold none of its words.
	id int32
}

// setsFilters are each set's filters (filtersOf), by the set, held weakly:
// a set the tracker let go (NewSet) takes its filters, and what they count
// on its budget, with it.
var setsFilters sync.Map // weak.Pointer[setDerived] -> *setFilters

// filtersOf are s's filters.
func filtersOf(s *setDerived) *setFilters {
	k := weak.Make(s)
	if v, ok := setsFilters.Load(k); ok {
		return v.(*setFilters)
	}
	v, loaded := setsFilters.LoadOrStore(k, &setFilters{of: map[string]*filterEntry{}, texts: map[filterTextKey]*filterEntry{},
		seeds: [2]maphash.Seed{maphash.MakeSeed(), maphash.MakeSeed()}, words: map[string][]*wordsEntry{},
		lists: map[weak.Pointer[[]string]]*listsEntry{}, post: map[string][]int32{}, runs: map[string][]int32{}})
	if !loaded {
		runtime.AddCleanup(s, func(k weak.Pointer[setDerived]) { setsFilters.Delete(k) }, k)
	}
	return v.(*setFilters)
}

// setFilter is the set's filter of the words of t's recording: of its text
// and of its other declared forms (textForms), as wordIn reads them, by runs
// of letters and digits (eachWord, wordFilter), built the first time a call
// of the set asks the recording a word, and kept for the set on its budget
// (setClaim). A recording's identity fixes its text for the set (Raw.id), so
// every later call asks the filter, not the text, for a word it cannot hold.
// A text with no recording's identity (a handed text, a view) has its
// filter by its text (textEntry) instead. Calls asking at once wait for the
// one building it. nil where t is in no set's call, where its text has no
// entry (textEntry), or where the entry keeps none.
func (t *readerText) setFilter() wordFilter {
	if t.filter == nil {
		fs := t.x.filters
		if fs == nil {
			return nil
		}
		if t.raw == nil || t.raw.id == "" {
			e, ok := t.textEntry(fs)
			if !ok {
				return nil
			}
			t.filter = e
		} else {
			fs.mu.Lock()
			e := fs.of[t.raw.id]
			if e == nil {
				e = &filterEntry{}
				fs.of[strings.Clone(t.raw.id)] = e
			}
			fs.mu.Unlock()
			t.filter, t.filterKey = e, filterIDSlotBytes+allocBytes(len(t.raw.id), false)
		}
	}
	e := t.filter
	e.once.Do(func() { e.f = t.buildFilter() })
	return e.f
}

// textEntry is the set's entry for the filter of the words of t's text, t
// having no recording's identity: a text the caller handed, or a view, which
// round F's calls each hand anew (Raw.viewOf reads a view per call, and a
// call's places hand their leaves' texts). Two handles of one text, in one
// call or in two, find one entry, by the text (filterTextKey), so the set
// builds its filter once, at the first ask, and every later call asks the
// filter, not the text. A text shorter than filterTextMin, and a view keploy could
// not read (the scan counts it unread), have none (ok false), and are
// scanned.
func (t *readerText) textEntry(fs *setFilters) (e *filterEntry, ok bool) {
	s := t.text
	if t.raw != nil {
		b := t.raw.payload()
		if !b.read {
			return nil, false
		}
		s = b.text
	}
	if len(s) < filterTextMin {
		return nil, false
	}
	k := filterTextKey{n: len(s), a: maphash.String(fs.seeds[0], s), b: maphash.String(fs.seeds[1], s)}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if e = fs.texts[k]; e == nil {
		e = &filterEntry{byText: true}
		fs.texts[k] = e
	}
	t.filterKey = filterTextSlotBytes
	return e, true
}

// buildFilter is the filter of t's recording's words (setFilter), or of its
// text's (textEntry), counted on the set's budget with its entry's key
// (filterKey); nil where the recording was not read or the filter does not
// fit.
func (t *readerText) buildFilter() wordFilter {
	t.filter.id = -1
	s, _, ok := t.read()
	if !ok {
		return nil
	}
	// Its forms derived for the build alone (textForms), as a scan past the
	// budget derives them: the set keeps none of them for the filter.
	forms := textForms(s)
	n := 0
	for _, text := range forms {
		eachWord(text, func(string) { n++ })
	}
	f := newWordFilter(n)
	for _, text := range forms {
		eachWord(text, f.add)
	}
	fs := t.x.filters
	fs.mu.Lock()
	if t.filter.byText {
		fs.textBuilds++
	} else {
		fs.builds++
	}
	fs.mu.Unlock()
	if !setClaim(t.x.set, allocBytes(uintptr(cap(f))*unsafe.Sizeof(f[0]), false)+allocBytes(unsafe.Sizeof(filterEntry{}), true)+t.filterKey) {
		return nil
	}
	postRuns(fs, t.x.set, t.filter, forms)
	return f
}

// postRuns adds e's words, each run of letters and digits of every form of
// its text (forms), to the set's postings (setFilters.runs) under a new id,
// counted on the set's budget as the postings hold them (postBytes): a run
// they hold no list of yet as its slot, its letters and its id, any other
// as one id more. e keeps id -1 past the budget. Where the filter of a
// text may hold a word by chance, the postings name exactly the texts that
// hold each run, so a call finds the places that may hold a word without
// asking every place (placeIndex). Each new run's letters are copied: the
// postings keep no text alive. A run another text posts between the count
// and the posting takes one id, less than its count: the postings never
// hold more than they counted, and never let a run go.
func postRuns(fs *setFilters, s *setDerived, e *filterEntry, forms []string) {
	seen := map[string]bool{}
	for _, text := range forms {
		eachWord(text, func(run string) { seen[run] = true })
	}
	n := int64(headerBytes)
	fs.mu.Lock()
	for run := range seen {
		n += postBytes(run, fs.runs[run] == nil)
	}
	fs.mu.Unlock()
	if !setClaim(s, n) {
		return
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	e.id = fs.nextText
	fs.nextText++
	for run := range seen {
		if ids, ok := fs.runs[run]; ok {
			fs.runs[run] = append(ids, e.id)
		} else {
			fs.runs[strings.Clone(run)] = []int32{e.id}
		}
	}
}

// postBytes is what a map of postings (setFilters.runs, post) holds for one
// id more under w: the id, at twice its list's length (append doubles a
// full list), and, fresh (the map holds no list of w yet), its list of one
// id, w's slot and w's letters, each as the runtime allocates them
// (allocBytes): the list keeps the whole 16-byte block it shares with other
// small objects alive alone once the rest is garbage, and a run past 32 KiB
// takes whole pages.
//
// A slot is its key's and its list's headers, 40 bytes, and their control
// byte, at 7/16 of its table filled: the least a table holds just after it
// grows or splits. A table of 1,024 slots, the most one holds, takes its
// 41,984 bytes of groups as 6 pages (49,152 bytes), so its slot is 48
// bytes at 7/16: 110 bytes a word, with the table's own header.
func postBytes(w string, fresh bool) int64 {
	if !fresh {
		return postIDBytes
	}
	return allocBytes(4, false) + postSlotBytes + allocBytes(len(w), false)
}

const (
	postIDBytes   = 8
	postSlotBytes = 112
)

// allocBytes is what the runtime holds for one object of n bytes, the one
// way every structure the set's budget bounds counts its allocations: n
// rounded up to its size class (sizeClasses), and past the largest whole
// 8 KiB pages. An object that holds pointers (ptrs) and passes 512 bytes
// carries an 8-byte header first; one that holds none and is under 16
// bytes shares a 16-byte block with other small objects, which it keeps
// alive alone once they are garbage, so it counts the block. 0 bytes take
// nothing.
func allocBytes[N int | int64 | uintptr](n N, ptrs bool) int64 {
	size := int64(n)
	switch {
	case size <= 0:
		return 0
	case !ptrs && size < 16:
		return 16
	case ptrs && size > 512 && size+8 <= sizeClasses[len(sizeClasses)-1]:
		size += 8
	}
	if size > sizeClasses[len(sizeClasses)-1] {
		return (size + pageBytes - 1) / pageBytes * pageBytes
	}
	i, _ := slices.BinarySearch(sizeClasses[:], size)
	return sizeClasses[i]
}

// pageBytes is the runtime's page: an object past its largest size class
// takes whole pages.
const pageBytes = 8 << 10

// sizeClasses are the runtime's size classes (internal/runtime/gc): a small
// object takes the least that holds it.
var sizeClasses = [...]int64{8, 16, 24, 32, 48, 64, 80, 96, 112, 128, 144, 160, 176, 192, 208, 224, 240, 256, 288, 320,
	352, 384, 416, 448, 480, 512, 576, 640, 704, 768, 896, 1024, 1152, 1280, 1408, 1536, 1792, 2048, 2304, 2688, 3072,
	3200, 3456, 4096, 4864, 5376, 6144, 6528, 6784, 6912, 8192, 9472, 9728, 10240, 10880, 12288, 13568, 14336, 16384,
	18432, 19072, 20480, 21760, 24576, 27264, 28672, 32768}

// postID is the id the set's postings name t's text by (postRuns), its
// filter built at the first ask (setFilter); -1 where they hold none of its
// words: t is in no set's call, its text has no entry, or the entry's
// words did not fit the set's budget.
func (t *readerText) postID() int32 {
	if t.setFilter() == nil {
		return -1
	}
	return t.filter.id
}

// holdingRuns is, in order, the ids of the set's texts whose words hold
// every run of runs (postRuns); ok false where runs is empty, which every
// text may hold.
func (fs *setFilters) holdingRuns(runs []string) (ids []int32, ok bool) {
	if len(runs) == 0 {
		return nil, false
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	lists := make([][]int32, len(runs))
	for i, run := range runs {
		lists[i] = fs.runs[run]
	}
	slices.SortFunc(lists, func(a, b []int32) int { return len(a) - len(b) })
next:
	for _, id := range lists[0] {
		for _, l := range lists[1:] {
			if _, in := slices.BinarySearch(l, id); !in {
				continue next
			}
		}
		ids = append(ids, id)
	}
	return ids, true
}

// heldIn reports whether one of a text's declared forms (textForms) holds w
// as a word in any of the readings rs. by: the readings of rs that read
// every byte of w as a word byte, asked once for every form; no other
// reading holds w as a word anywhere.
func heldIn(forms []string, w string, rs ...int) bool {
	var by uint8
	for _, r := range rs {
		by |= 1 << r
	}
	for i := 0; i < len(w); i++ {
		by &= wordMask[w[i]]
	}
	if w == "" || by == 0 {
		return false
	}
	return slices.ContainsFunc(forms, func(f string) bool { return heldBy(f, w, by) })
}

// wordMask is, by byte, the readings that read it as a word byte: bit r of
// wordMask[c] is readings[r](c), so a word's check reads a table, not a
// function a byte.
var wordMask = func() (m [256]uint8) {
	for r, word := range readings {
		for c := range 256 {
			if word(byte(c)) {
				m[c] |= 1 << r
			}
		}
	}
	return m
}()

// heldScans counts the scans heldBy makes of a text for a word, for the
// rows that bound a call's work.
var heldScans atomic.Int64

// heldBy reports whether w is a word of s in any of the readings by (bits
// of wordMask; each reads every byte of w as a word byte) as each reads it
// (tokensBy(s, readings[r])[w]) without reading s's other words: w stands
// in s, and no byte that reading reads as a word byte stands right before
// or after. One scan finds each place w stands in s for every reading.
func heldBy(s, w string, by uint8) bool {
	heldScans.Add(1)
	for off := 0; off <= len(s)-len(w); {
		i := strings.Index(s[off:], w)
		if i < 0 {
			return false
		}
		i += off
		var around uint8
		if i > 0 {
			around = wordMask[s[i-1]]
		}
		if e := i + len(w); e < len(s) {
			around |= wordMask[s[e]]
		}
		if by&^around != 0 {
			return true
		}
		off = i + 1
	}
	return false
}

// fieldAt names one field of a recording's text (rawBody.fields, whose
// paths round F's view of it shares): its path, whether it is the field's
// name rather than its value, and the recording's field it stands in (in),
// so places compare as the field and the path in it: a header "a.b" holding
// {"c":..} and a header "a" holding {"b":{"c":..}} both spell a.b.c, and are
// two places. A text the caller hands, or a recording read with no fields,
// is one field: the zero fieldAt.
type fieldAt struct {
	path string
	key  bool
	in   string
}

// fieldWords is what the index keeps of a text's fields: each field, and,
// by word, the fields (indices into at) whose own text holds it.
type fieldWords struct {
	at []fieldAt
	of map[string][]int32
}

// The sizes a field view counts: a fieldAt (two strings' headers and a
// bool), and a word's entry in its map past its letters and its list: the
// slot (a string's header and a slice's) doubled for the map's growth, as
// wordBytes counts map[string]bool's. A list counts what it holds (its
// capacity, 4 bytes an index), as append grew it.
const (
	fieldAtBytes   = 40
	fieldWordBytes = 80
)

func fieldWordsBytes(f fieldWords) int64 {
	n := int64(headerBytes + fieldAtBytes*cap(f.at))
	for _, a := range f.at {
		n += int64(len(a.path) + len(a.in))
	}
	for w, at := range f.of {
		n += fieldWordBytes + int64(len(w)+4*cap(at))
	}
	return n
}

// fieldsWith are the fields of the text that hold w as a word, in any of
// main's readings and any declared form of the field's own text (holdsWord,
// field by field): a recording's by its fields, so a word that moved to
// another field in round F's view is not at its recorded field. Read once
// for the call where the view fits what is left of the index's cap as it is
// built (counted word by word, with the field it reads and that field's
// forms, and dropped once past it); else, by a scan of the fields of the text
// read now, which keeps nothing. Either way the question's peak is the
// index's count of what these hold: the text, the view's slots and lists by
// their capacity, and the field read with its forms (the decoder's garbage
// and a map's growth as it happens are outside it). read is false where the
// recording was not read.
func (t *readerText) fieldsWith(w string) (at []fieldAt, read bool) {
	x := t.x
	fw, scanned, ok := indexOnce(t, func(e *readerEntry) *answer[fieldWords] { return &e.fields },
		func(s string, fields []rawField, room int64) (fieldWords, bool) {
			fw, n, fits, own := fieldWords{of: map[string][]int32{}}, int64(headerBytes), room >= 0, t.e.own(s)
			eachField(s, fields, func(f fieldAt, text string) {
				if !fits {
					return
				}
				forms, held := formsHeld(text)
				k, ats := int32(len(fw.at)), append(fw.at, f)
				head := int64(fieldAtBytes*(cap(ats)-cap(fw.at)) + len(f.path) + len(f.in))
				if fits = n+head+held <= room; fits {
					fw.at = ats
					n += head
				}
				for _, form := range forms {
					for _, word := range readings {
						for i, j := 0, 0; i < len(form) && fits; i = max(j, i+1) {
							for j = i; j < len(form) && word(form[j]); {
								j++
							}
							l, seen := fw.of[form[i:j]]
							if j == i || seen && l[len(l)-1] == k {
								continue
							}
							w, more, grown := form[i:j], int64(0), append(l, k)
							if !seen {
								w, more = strings.Clone(w), fieldWordBytes+int64(len(w))
							}
							if more += 4 * int64(cap(grown)-cap(l)); n+more+held <= room {
								n += more
								fw.of[w] = grown
							} else {
								fits = false
							}
						}
					}
				}
				x.peak = max(x.peak, x.used+own+n+held)
			})
			return fw, fits
		}, fieldWordsBytes, func(s string, fields []rawField) {
			own := t.e.own(s)
			eachField(s, fields, func(f fieldAt, text string) {
				forms, held := formsHeld(text)
				x.peak = max(x.peak, x.used+own+held)
				if heldIn(forms, w, 0, 1, 2) {
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

// formsHeld are text's declared forms (textForms), and what reading them
// holds by the index's count: the text as the walk's decoder buffers it (up
// to twice its length: its buffer doubles), and each form, the text among
// them.
func formsHeld(text string) ([]string, int64) {
	forms, n := textForms(text), 2*int64(len(text))
	for _, f := range forms {
		n += int64(len(f))
	}
	return forms, n
}

// eachField calls yield with each field of s, the text read now, and the
// field's text: its recording's fields (fields, read with s), a value that
// is wholly JSON (an HTTP body) by its own leaves below its path
// (walkJSON), else s whole as one field. A value with no leaf (blank, a
// null) is one field too. Its field's name is never read as JSON. Each
// stands in its recording's field (in) as no other place of s spells it:
// textFields joins keys with dots, so a jsonb key "a.b" and key a's "b"
// share a path. Where a key may read so (oddKey), every field is spelled by
// its keys (keyedPath); where no field is a key, fields at one path stand
// by their order among them, the first by its path alone. A text with a key
// "" at its top holds no field: textFields writes that key's object's
// fields as the top's own, so nothing in s tells the two apart.
func eachField(s string, fields []rawField, yield func(fieldAt, string)) {
	if len(fields) == 0 {
		yield(fieldAt{}, s)
	}
	var seen map[string]int
	var keys []fieldAt
	odd, keyed := keysOf(s, fields)
	if !keyed {
		seen = map[string]int{}
	}
	for _, f := range fields {
		if f.end > len(s) {
			continue
		}
		if f.key && f.path == "" {
			return
		}
		text, in := s[f.start:f.end], f.path
		if odd {
			in = keyedPath(&keys, text, f)
		} else if seen != nil {
			if seen[f.path]++; seen[f.path] > 1 {
				in += "\x00" + strconv.Itoa(seen[f.path]-1)
			}
		}
		values, leaves := 0, 0
		if !f.key {
			values = walkJSON(text, false, func(string, bool, string) { leaves++ })
		}
		if values == 0 || leaves == 0 {
			yield(fieldAt{f.path, f.key, in}, text)
			continue
		}
		walkJSON(text, values > 1, func(path string, key bool, leaf string) {
			yield(fieldAt{strings.TrimSuffix(f.path+"."+path, "."), key, in}, leaf)
		})
	}
}

// keysOf reports whether one of fields is a key (keyed), and one an oddKey
// (odd). With no oddKey every path textFields writes reads back one way.
func keysOf(s string, fields []rawField) (odd, keyed bool) {
	for _, f := range fields {
		if f.key && f.end <= len(s) {
			if oddKey(s[f.start:f.end]) {
				return true, true
			}
			keyed = true
		}
	}
	return false, keyed
}

// oddKey reports whether textFields' dotted path may read key k as more
// than one key: it holds '.' or '[' (a ']' reads so only beside a '['). A
// key "" below the top is the empty name between two dots ("a..b").
func oddKey(k string) bool { return strings.ContainsAny(k, ".[") }

// keyedPath is f's path as its keys spell it, an oddKey quoted in brackets
// (["a.b"], as pathKey quotes it), which no other key or index spells, and
// any other key as textFields writes it, so a path no oddKey is on is
// spelled as in a text with none. textFields writes each key (text, its
// name) before its value's fields, so the key a field stands below is the
// latest one in keys whose path, less array indices, is the field's
// parent's; keys are those on the way to f, each with its spelling.
func keyedPath(keys *[]fieldAt, text string, f rawField) string {
	p := f.path
	if f.key {
		p = strings.TrimSuffix(strings.TrimSuffix(p, text), ".")
	}
	c, i := *keys, len(*keys)-1
	indices := func(r string) bool { return strings.Trim(r, "[]0123456789") == "" }
	for i >= 0 && (!strings.HasPrefix(p, c[i].path) || !indices(p[len(c[i].path):])) {
		i--
	}
	in := p
	if i >= 0 {
		in = c[i].in + p[len(c[i].path):]
	}
	if f.key {
		if oddKey(text) {
			in = string(pathKey([]byte(in), text))
		} else {
			in += f.path[len(p):] // the key as textFields wrote it below p
		}
		*keys = append(c[:i+1], fieldAt{path: f.path, in: in})
	}
	return in
}

// walkJSON walks text as JSON values token by token, each key and scalar to
// yield at its path, so a large body costs its largest token: no tree, no
// rewritten copy. A path is spelled as textFields spells it (a.b[0].c), but a
// key no nesting could spell alike (pathKey) is quoted, so two places of one
// text never share a path. several puts each value below its index, as an
// array's element is (NDJSON, a streaming API's body). A repeated key's
// values all stand at that key; a null is no leaf. It gives how many values
// text holds, none where text is not wholly JSON values (scalars glued with
// no object or array in the text, as in "007" or "2026-11-02", are text, not
// values; in a text that holds an object or array every value, glued or not,
// stands at its own index, so a word that moved between them, or within the
// object, is not at one place) or nests deeper than encoding/json reads (its Token refuses past its
// depth, which bounds the walk's recursion).
func walkJSON(text string, several bool, yield func(path string, key bool, leaf string)) (values int) {
	d := json.NewDecoder(strings.NewReader(text))
	d.UseNumber()
	var path []byte
	index := func(i int) { path = append(strconv.AppendInt(append(path, '['), int64(i), 10), ']') }
	var walk func() bool
	walk = func() bool {
		tok, err := d.Token()
		if err != nil {
			return false
		}
		t, open := tok.(json.Delim)
		if s, isText := tok.(string); isText {
			yield(string(path), false, s)
		} else if tok != nil && !open { // a json.Number or a bool, written as textFields writes it
			yield(string(path), false, fmt.Sprint(tok))
		}
		if !open {
			return true
		}
		base := len(path)
		for i := 0; d.More(); i++ {
			if t == '[' {
				index(i)
			} else if k, err := d.Token(); err != nil {
				return false
			} else {
				name, _ := k.(string)
				path = pathKey(path, name)
				yield(string(path), true, name)
			}
			if !walk() {
				return false
			}
			path = path[:base]
		}
		_, err = d.Token()
		return err == nil
	}
	glued, nested := false, false
	for end := -1; d.More(); values++ {
		start := int(d.InputOffset())
		glued, nested = glued || start == end, nested || text[start] == '{' || text[start] == '['
		if path = path[:0]; several {
			index(values)
		}
		if !walk() {
			return 0
		}
		end = int(d.InputOffset())
	}
	if _, err := d.Token(); err != io.EOF || glued && !nested {
		return 0
	}
	return values
}

// pathKey is path with key k below it: ".k" (no dot at the root), as
// textFields writes it, or, for a key that path syntax could read as more
// than one key (empty, or holding '.', '[', ']', a quote or a backslash),
// the key quoted in brackets (["a.b"]), which no other key or index spells.
func pathKey(path []byte, k string) []byte {
	if k == "" || strings.ContainsAny(k, `.[]"\`) {
		return append(strconv.AppendQuote(append(path, '['), k), ']')
	}
	if len(path) > 0 {
		path = append(path, '.')
	}
	return append(path, k...)
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
		uncapped(func(s string, _ []rawField) map[string]bool {
			runs := map[string]bool{}
			eachB64Run(s, t.x.dc.spansOf(s), func(a, b int) bool { runs[s[a:b]] = true; return true })
			return runs
		}), wordsBytes, func(s string, _ []rawField) {
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
// holdsWord, the instants of the text and its forms from one read of it,
// kept in the orders momentIndex looks them up by. Past the budget each
// question asks of every instant of the text read now, and orders none.
func (t *readerText) holdsMoment(m moment) (held, fine, read bool) {
	dc := t.x.dc
	asked := func() { t.x.momentAsks++ }
	forms := func(s string) []instant { return slices.Concat(dc.spansOf(s), dc.spansOf(dc.rawExtra(s))) }
	ix, scanned, ok := indexOnce(t, func(e *readerEntry) *answer[momentIndex] { return &e.formSpans },
		uncapped(func(s string, _ []rawField) momentIndex { return newMomentIndex(forms(s)) }), momentIndexBytes,
		func(s string, _ []rawField) {
			for _, sp := range forms(s) {
				if !sp.keyed {
					continue
				}
				asked()
				h, f := byMoment(sp.moment(), m)
				held, fine = held || h, fine || f
			}
		})
	if !ok {
		return false, false, false
	}
	if !scanned {
		held, fine = ix.holds(m, asked)
	}
	return held, fine, true
}

// momentIndex is the moments of a text's instants that name one
// (instant.keyed), in the orders holds looks them up by, so that a
// question asks byMoment only of the instants cutsNone cannot turn away:
//   - odd: a time of day, or an instant in a zone not a whole number of
//     minutes off UTC, which cutsNone turns away from no moment;
//   - at: every other instant, by its moment (key.at);
//   - minute: those of at whole at the minute in UTC, the only ones a value
//     finer than them is built on (cutsNone's finer);
//   - wall: those of at in a zone off UTC, by the wall clock they write
//     (key.at plus their offset), the only ones a value of no zone is the
//     wall clock of.
type momentIndex struct {
	moments          []moment
	odd              []int32
	at, minute, wall momentOrder
}

// momentOrder is some of an index's moments by a key, ascending.
type momentOrder []orderedMoment

// orderedMoment is one moment of an order: its key, and its index in
// momentIndex.moments (i).
type orderedMoment struct {
	key int64
	i   int32
}

// newMomentIndex is the index of spans' instants that name a moment.
func newMomentIndex(spans []instant) momentIndex {
	var ix momentIndex
	minute := int64(time.Minute)
	add := func(o *momentOrder, key int64, i int32) {
		*o = append(*o, orderedMoment{key, i})
	}
	for _, sp := range spans {
		if !sp.keyed {
			continue
		}
		m := sp.moment()
		i := int32(len(ix.moments))
		ix.moments = append(ix.moments, m)
		z := m.offset()
		if m.key.day || z%minute != 0 {
			ix.odd = append(ix.odd, i)
			continue
		}
		add(&ix.at, m.key.at, i)
		if whole(m.key.at, 0, minute) {
			add(&ix.minute, m.key.at, i)
		}
		if z != 0 {
			add(&ix.wall, m.key.at+z, i)
		}
	}
	for _, o := range []momentOrder{ix.at, ix.minute, ix.wall} {
		slices.SortFunc(o, func(a, b orderedMoment) int { return cmp.Compare(a.key, b.key) })
	}
	return ix
}

// momentIndexBytes is about what ix holds: each moment, and each of its
// places in the orders.
func momentIndexBytes(ix momentIndex) int64 {
	return headerBytes + momentBytes*int64(len(ix.moments)) + 4*int64(len(ix.odd)) + 16*int64(len(ix.at)+len(ix.minute)+len(ix.wall))
}

// holds is holdsMoment's answer of ix: whether an instant names m by
// moment (byMoment), and whether one does so at a fine cut. asked counts
// each instant byMoment is asked of.
//
// It asks first of the instants that may hold m at a fine cut: the odd
// ones, and those that stand within a millisecond of m, as instants or one
// as the other's wall clock. A cut finer than a millisecond moves an
// instant by less than one, and a value finer than an instant is built
// only on a unit longer than F's lead (cutsEach's finerUnits), so no other
// instant holds m at a fine cut. Where none of those holds m, it asks of
// the instants cutsNone does not turn away from m (each of its three
// reaches, coarserReach), and the first that holds m answers. A moment of a
// time of day, or of a zone not a whole number of minutes off UTC, which
// cutsNone turns away from nothing, is asked of every instant.
func (ix *momentIndex) holds(m moment, asked func()) (held, fine bool) {
	ask := func(i int32) bool {
		asked()
		h, f := byMoment(ix.moments[i], m)
		held, fine = held || h, fine || f
		return fine
	}
	minute := int64(time.Minute)
	if m.key.day || m.offset()%minute != 0 {
		for i := range ix.moments {
			if ask(int32(i)) {
				break
			}
		}
		return held, fine
	}
	at, mz, ms := m.key.at, m.offset(), int64(time.Millisecond)
	for _, i := range ix.odd {
		if ask(i) {
			return held, fine
		}
	}
	if ix.at.each(at, ms, ask) || !m.zoned && ix.wall.each(at, ms, ask) || held {
		return held, fine
	}
	first := func(i int32) bool {
		ask(i)
		return held
	}
	unzoned := func(i int32) bool { return !ix.moments[i].zoned && first(i) }
	reach := coarserReach(m)
	_ = ix.at.each(at, reach, first) ||
		!m.typed && ix.minute.each(at, nsPerDay, first) ||
		!m.zoned && ix.wall.each(at, reach, first) ||
		mz != 0 && ix.at.each(at+mz, 0, unzoned) ||
		mz != 0 && !m.typed && ix.minute.each(at+mz, nsPerDay, unzoned)
	return held, fine
}

// each calls f with each moment of o whose key stands within r of c, from
// the first at or past c up, then down from it, until f returns true;
// it reports whether f did.
func (o momentOrder) each(c, r int64, f func(int32) bool) bool {
	p, _ := slices.BinarySearchFunc(o, c, func(e orderedMoment, c int64) int { return cmp.Compare(e.key, c) })
	for j := p; j < len(o) && o[j].key <= c+r; j++ {
		if f(o[j].i) {
			return true
		}
	}
	for j := p - 1; j >= 0 && o[j].key >= c-r; j-- {
		if f(o[j].i) {
			return true
		}
	}
	return false
}

// coarserReach is how far from m, an instant of a zone whole minutes off
// UTC, an instant may stand as an instant and hold m by being cut to a
// unit m is whole at (cutsNone's coarser): a day where m is whole at the
// minute, else the longest unit m is whole at in UTC: one of a second or
// less, as each unit past a second is whole minutes.
func coarserReach(m moment) int64 {
	if whole(m.key.at, 0, int64(time.Minute)) {
		return nsPerDay
	}
	var reach int64
	for _, d := range precisionUnits {
		if u := int64(d); whole(m.key.at, 0, u) {
			reach = u
		}
	}
	return reach
}

// unread reports whether the call asked about a recording keploy could not
// read (Raw.Read): what the budget does not keep is read again, never
// unread.
func (x *readerIndex) unread() bool { return x.unreadTexts > 0 }
