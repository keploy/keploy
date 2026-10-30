package learn

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Each test of a set reads the payloads of the recordings before its calls
// (DriftCache.RawOf), and reading one is a JSON encode and decode of the
// whole recording. Read once per test, a set's tests read every recording
// once each: on api-server's round, about half of the agent's CPU. A
// recording whose answer is in memory is now read once for the set, into
// the set's store of payloads, whatever test reads it, and learns what one
// read whole learns. A recording whose answer the agent parked on disk is
// still read as it is, once per test, and the set's store never keeps its
// text without its answer under the recording's identity. The next set
// reads its recordings again, once; a test of a set that is over reads
// them once per test and keeps none. Counted (rawOfNow's reads), never
// timed.
func TestARecordingIsDecodedOncePerSetAcrossTests(t *testing.T) {
	at := eRecAt
	const p = "Req.query.bind[0]"
	const tests = 3
	src := &models.Mock{Name: "src", Kind: models.PostgresV3, Spec: models.MockSpec{
		ReqTimestampMock: at.Add(-time.Second / 2),
		ResTimestampMock: at.Add(-time.Second/2 + time.Millisecond),
		PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
			SQLNormalized: "SELECT note FROM src",
			Response:      &models.PostgresV3Response{Rows: []models.PostgresV3Cells{{{Value: "id a1b2c3d4 kept"}}}, CommandComplete: "SELECT 1"},
		}},
	}}
	mocks := append(fillers(10, at.Add(-time.Second)), src)
	rec := Shape{Leaves: map[string]Leaf{p: {Path: p, Type: String, Value: "a1b2c3d4"}}}
	live := Shape{Leaves: map[string]Leaf{p: {Path: p, Type: String, Value: "z9y8x7w6"}}}
	classify := func(dc *DriftCache, raw []Raw) []Learned {
		dc.appClock = true
		return ClassifyFollowing(dc, []string{p}, live, rec, at, nil, raw, nil, nil, nil)
	}
	eager := make([]Raw, len(mocks))
	texts := make([]string, len(mocks))
	for i, m := range mocks {
		eager[i] = RawOf(m, true).ServedAs(i + 1).ServedAsRecorded()
		texts[i] = eager[i].payload().text
	}
	want := classify(noSet(NewDriftCache()), eager)
	// In the mode that makes every Raw read now a lazy one read again at
	// each use (everyRawLazy), a Raw read once per test is read at each use
	// by design: the counts of those reads hold in the other modes.
	lazyMode := everyRawLazy != nil
	if len(want) != 1 {
		t.Fatalf("the row's call learns %v read whole: want one verdict to compare", want)
	}

	// read is one test's call: it reads every recording before it, each
	// payload whole, and learns from them; reads is how many payloads that
	// read.
	read := func(dc *DriftCache) (raw []Raw, reads int64, got []Learned) {
		before := payloadDecodes.Load()
		raw = make([]Raw, len(mocks))
		for i, m := range mocks {
			raw[i] = dc.RawOf(m, true).ServedAs(i + 1).ServedAsRecorded()
			if b := raw[i].payload(); !b.read || b.text != texts[i] {
				t.Fatalf("%s: the payload a test reads is not the one RawOf reads", m.Name)
			}
		}
		got = classify(dc, raw)
		return raw, payloadDecodes.Load() - before, got
	}

	tr := NewTracker()
	tr.NewSet()
	var total int64
	var stale *DriftCache
	for test := 1; test <= tests; test++ {
		tr.Begin()
		dc := tr.Drifts()
		raw, n, got := read(dc)
		total += n
		if !reflect.DeepEqual(got, want) {
			t.Errorf("test %d learned %v, want %v (read whole)", test, got, want)
		}
		if raw[0].src == nil {
			t.Errorf("test %d read %s now, not from the set's store of payloads", test, mocks[0].Name)
		}
		stale = dc
		tr.End()
	}
	if want := int64(len(mocks)); total != want {
		t.Errorf("%d tests of a set read the payloads of its %d recordings %d times: want %d, once each for the set", tests, len(mocks), total, want)
	}
	ended := tr.derived
	if u := ended.use(); u.Decoded != len(mocks) || u.Bodies == 0 {
		t.Errorf("the set's use says %d payloads decoded and %d bytes kept: want %d decoded and the payloads kept", u.Decoded, u.Bodies, len(mocks))
	}

	// A recording whose answer the agent parked on disk is read as it is,
	// once per test, and the set's store keeps nothing of it.
	parked := getAnswer("parked", "http://api/items/parked", `{"note":"q7-r8s9"}`, at.Add(-time.Second/4))
	resp := parked.Spec.HTTPResp
	parked.Spec.HTTPResp = nil
	parked.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) { return resp, nil, nil })
	tr.Begin()
	dc := tr.Drifts()
	before, loads := payloadDecodes.Load(), dc.Loads()
	for i := 0; i < 2; i++ {
		if x := dc.RawOf(parked, true); strings.Contains(x.payload().text, "q7-r8s9") || x.src != nil && x.src.store == dc.set.bodies {
			t.Errorf("a recording whose answer is parked was read with its answer, or from the set's store: want it read as it is, without it")
		}
	}
	if n := payloadDecodes.Load() - before; n != 1 && !lazyMode {
		t.Errorf("a test read a parked recording's payload %d times: want once for the test", n)
	}
	if _, ok := dc.set.bodies.readOf(MockID(parked)); ok || dc.Loads() != loads {
		t.Errorf("the set's store loaded (%d) or keeps the text of a recording whose answer is parked, read without it", dc.Loads()-loads)
	}
	tr.End()

	// The next set reads its recordings once, as the first did, and keeps
	// nothing of the set before it.
	tr.NewSet()
	if u := ended.usedBytes(); u != 0 {
		t.Errorf("a set that is over keeps %d bytes of its payloads: want none", u)
	}
	tr.Begin()
	if _, n, got := read(tr.Drifts()); n != int64(len(mocks)) || !reflect.DeepEqual(got, want) {
		t.Errorf("the next set's first test read %d payloads of %d recordings and learned %v: want each read once, and %v", n, len(mocks), got, want)
	}
	tr.End()

	// A handler still reading the set that is over reads each payload once
	// for its test, and the set over loads and keeps none of them.
	loads = ended.bodies.loads
	raw, n, got := read(stale)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a test of a set that is over learned %v, want %v", got, want)
	}
	if !lazyMode && (n != int64(len(mocks)) || raw[0].src != nil) {
		t.Errorf("a test of a set that is over read %d payloads of %d recordings (lazily: %v): want each read once for the test, now", n, len(mocks), raw[0].src != nil)
	}
	if u := ended.usedBytes(); u != 0 || ended.bodies.loads != loads {
		t.Errorf("a set that is over loaded %d payloads for a late test and keeps %d bytes of them: want none", ended.bodies.loads-loads, u)
	}
	// A lazy Raw of the set that is over (a parked answer's, LazyRawOf) is
	// read now, once: its store keeps no more, and read lazily it would be
	// loaded again at each read.
	if x := stale.LazyRawOf(mocks[0], true, nil); x.src != nil || x.payload().text != texts[0] {
		t.Errorf("a lazy Raw of a set that is over is read lazily (%v), from a store that keeps no more: want it read now", x.src != nil)
	}

	// A set past its budget (a tier that lets none go was refused a part)
	// has let go of its payloads: read lazily, each would be read again at
	// each read. Its tests read each payload once for the test.
	tr.NewSet()
	tr.derived.mu.Lock()
	tr.derived.held.full = true
	tr.derived.mu.Unlock()
	for test := 1; test <= 2; test++ {
		tr.Begin()
		raw, n, got := read(tr.Drifts())
		if !reflect.DeepEqual(got, want) {
			t.Errorf("test %d of a set past its budget learned %v, want %v", test, got, want)
		}
		if !lazyMode && (n != int64(len(mocks)) || raw[0].src != nil) {
			t.Errorf("test %d of a set past its budget read %d payloads of %d recordings (lazily: %v): want each read once for the test, now", test, n, len(mocks), raw[0].src != nil)
		}
		tr.End()
	}
}

// The set's payloads are one more tier on its one budget, and the budget
// lets them go after the words: a part another tier needs past the budget
// takes the words' room first, the words read least recently first, and
// then the payloads read least recently. A payload kept past the budget
// takes the words' room before it lets go of another payload. A payload
// let go is read again (a JSON encode and decode of its recording) before
// any of its parts can be; a recording's words are read again from its
// payload. Counted in the tiers' bytes, never timed.
func TestASetsPayloadsLetGoAfterItsWords(t *testing.T) {
	defer func(b int64) { learnBudget = b }(learnBudget)
	learnBudget = 1 << 20
	s := newSetDerived()
	const size = 4 << 10
	id := func(kind string, i int) string { return fmt.Sprintf("%s-%04d", kind, i) }
	slot := int64(slotBytes + len(id("w", 0)))
	body := func(i int) func() (*models.Mock, error) {
		return func() (*models.Mock, error) {
			return getAnswer(id("b", i), "http://api/items/"+fmt.Sprint(i), `{"note":"`+strings.Repeat("x", size)+`"}`, eRecAt), nil
		}
	}
	keepWords := func(from, to int) {
		s.mu.Lock()
		defer s.mu.Unlock()
		for i := from; i < to; i++ {
			s.words.keep(id("w", i), partTokens, size, func(*wordParts) {})
		}
	}

	// A third of the budget in payloads, then words to fill the rest.
	bodies := 0
	for s.bodies.kept.used < s.budget.limit/3 {
		s.bodies.get(id("b", bodies), id("b", bodies), body(bodies))
		bodies++
	}
	words := int((s.budget.limit - s.budget.used) / (size + slot))
	keepWords(0, words)
	if bodies < 20 || words < 100 || len(s.bodies.kept.by) != bodies || len(s.words.by) != words {
		t.Fatalf("the set keeps %d of %d payloads and %d of %d words' recordings, %d of %d bytes: the row needs both kept and the budget full",
			len(s.bodies.kept.by), bodies, len(s.words.by), words, s.budget.used, s.budget.limit)
	}
	// The first payload is read again: it is the one read most recently.
	s.bodies.get(id("b", 0), id("b", 0), body(0))

	// A payload past the budget takes the words' room, not a payload's.
	s.bodies.get(id("b", bodies), id("b", bodies), body(bodies))
	bodies++
	if len(s.bodies.kept.by) != bodies {
		t.Errorf("a payload kept past the budget let go of %d payloads, with %d words' recordings kept: want the words to go first", bodies-len(s.bodies.kept.by), len(s.words.by))
	}
	if len(s.words.by) == words || s.words.by[id("w", 0)] != nil {
		t.Errorf("a payload kept past the budget let go of %d words' recordings (the one read least recently kept: %v): want the least recently read to go", words-len(s.words.by), s.words.by[id("w", 0)] != nil)
	}

	// Held parts past the budget take all the words' room, then the
	// payloads read least recently, one at a time.
	s.mu.Lock()
	kept := 0
	for len(s.words.by) > 0 {
		s.held.keep(id("h", kept), partMoments, size, func(*heldParts) {})
		kept++
		if len(s.bodies.kept.by) != bodies && len(s.words.by) > 0 {
			t.Errorf("a held part let go of a payload while the words kept %d recordings", len(s.words.by))
			break
		}
	}
	for i := 0; i < 3; i++ {
		s.held.keep(id("h", kept), partMoments, size, func(*heldParts) {})
		kept++
	}
	gone := bodies - len(s.bodies.kept.by)
	if gone == 0 || gone > 4 {
		t.Errorf("three held parts past the words' room let go of %d payloads: want about one each", gone)
	}
	if s.bodies.kept.by[id("b", 0)] == nil {
		t.Errorf("the budget let go of the payload read most recently")
	}
	for i := 1; i <= gone; i++ {
		if s.bodies.kept.by[id("b", i)] != nil {
			t.Errorf("the budget kept %s, among the %d payloads read least recently, and let go of later ones", id("b", i), gone)
			break
		}
	}
	if !s.held.room() || len(s.held.by) != kept {
		t.Errorf("held keeps %d of %d parts (full %v): want all, the words and payloads let go", len(s.held.by), kept, s.held.full)
	}
	if used := s.held.used + s.words.used + s.forms.used + s.bodies.kept.used; used != s.budget.used || used > s.budget.limit {
		t.Errorf("the tiers keep %d bytes, the budget counts %d of %d: want them equal and within it", used, s.budget.used, s.budget.limit)
	}
	s.mu.Unlock()

	// A payload let go is read again where it is next read, once.
	loads := s.bodies.loads
	s.bodies.get(id("b", 1), id("b", 1), body(1))
	s.bodies.get(id("b", 1), id("b", 1), body(1))
	if n := s.bodies.loads - loads; n != 1 {
		t.Errorf("a payload let go was loaded %d times on its next two reads: want once", n)
	}
}
