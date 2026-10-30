package learn

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// countTokenReads counts the reads of a filler payload's words
// (tokensOfForms) until the row ends.
func countTokenReads(t *testing.T) *atomic.Int64 {
	read := tokensOfForms
	t.Cleanup(func() { tokensOfForms = read })
	var n atomic.Int64
	tokensOfForms = func(forms []string) []map[string]bool {
		if len(forms) > 0 && strings.Contains(forms[0], "filler") {
			n.Add(1)
		}
		return read(forms)
	}
	return &n
}

// The word rule's search of a value's changed words in the earlier payloads
// (unaccounted: wordsHeld) read every earlier recording's words, in every
// reading and form, on every call: a set's words are more than its budget
// keeps (api-server's round under keploy mock record: 14 813 recordings, 335
// MB of payloads), so the recording read least recently was let go first,
// and a call that reads them all in the same order found none kept. The
// handlers in the learn step at once each held the words they were reading
// (1.2 GB of api-server's heap), and allocated 2.19 TB over the round (27%
// of the agent's CPU in GC). A recording's words are now read only where its
// filter of words, kept for the set, may hold a run of letters and digits of
// a changed word (mayHoldWords): of a set of the round's size, a call reads
// the words of at most one recording in fifty that hold none of them (the
// filter's false hits, about one in a hundred), and learns what a cache of
// no tracker learns.
func TestAValuesWordsAreReadOnlyFromEarlierPayloadsThatMayHoldThem(t *testing.T) {
	if testing.Short() {
		t.Skip("reads 14 813 recordings")
	}
	const n = 14813
	row := newHeldRow(n, 8)
	want := row.classify(noSet(NewDriftCache()))
	read := countTokenReads(t)
	tr := NewTracker()
	tr.NewSet()
	for test := 1; test <= 2; test++ {
		tr.Begin()
		for call := 1; call <= 2; call++ {
			read.Store(0)
			got := row.classify(tr.Drifts())
			if r := read.Load(); r > n/50 {
				t.Errorf("test %d, call %d: the word rule read the words of %d of %d earlier recordings, none of which holds a changed word; want at most %d (one in fifty)", test, call, r, n, n/50)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("test %d, call %d: learned %+v, want %+v (a cache of no tracker's)", test, call, got, want)
			}
		}
		tr.End()
	}
}

// A test's call on a set of api-server's size, past the set's first, reads
// what the set keeps of its recordings and allocates a few KiB for each of
// them, never its payload's words: it allocated every earlier recording's
// words (70 KiB a recording of the row's, more of api-server's payloads).
// The bound is in the row's name. Most of what is left is the moments of
// the earlier recordings each value's echo files anew (earlierMoments).
func TestALaterTestsCallOnAnApiServerSizeSetAllocatesAtMostFourKiBARecording(t *testing.T) {
	if testing.Short() {
		t.Skip("reads 14 813 recordings")
	}
	const n = 14813
	row := newHeldRow(n, 8)
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	row.classify(tr.Drifts())
	tr.End()
	tr.Begin()
	defer tr.End()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	row.classify(tr.Drifts())
	runtime.ReadMemStats(&after)
	alloc := int64(after.TotalAlloc - before.TotalAlloc)
	t.Logf("a later test's call on a set of %d recordings allocated %d KiB (%d B a recording)", n, alloc>>10, alloc/n)
	if bound := int64(n << 12); alloc > bound {
		t.Errorf("a later test's call on a set of %d recordings allocated %d KiB (%d B a recording), past %d KiB (4 KiB a recording)", n, alloc>>10, alloc/n, bound>>10)
	}
}

// keepsNoMoreHeld is tr's set past the bytes it keeps of its recordings'
// instants, places and filters of words (heldBudget): it keeps no more of
// them, and reads each recording's through the test's cache of texts.
func keepsNoMoreHeld(tr *Tracker) {
	s := tr.derived
	s.mu.Lock()
	s.held.full = true
	s.mu.Unlock()
}

// holderMock is a recording made at at, answered with a row whose one cell
// is value.
func holderMock(name string, at time.Time, value string) *models.Mock {
	return &models.Mock{Name: name, Kind: models.PostgresV3, Spec: models.MockSpec{
		ReqTimestampMock: at,
		ResTimestampMock: at.Add(time.Millisecond),
		PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
			SQLNormalized: "SELECT v FROM holders",
			Response:      &models.PostgresV3Response{Rows: []models.PostgresV3Cells{{{Value: value}}}, CommandComplete: "SELECT 1"},
		}},
	}}
}

// A word a value took in F that an earlier recording, which F did not serve,
// holds in its answer is refused where a set reads its filter of words
// first, as with no set: a word of letters and digits; one of '-' or '_'
// between them (an id of the URL-safe alphabets), whose runs of letters and
// digits alone the filter holds, never the word whole; one of no letter or
// digit, which the filter tells nothing of; and any, past the bytes the set
// keeps of its recordings' filters (heldBudget), where there is no filter
// to read.
func TestAWordAnEarlierRecordingHoldsIsRefusedWhereTheSetReadsItsFilterFirst(t *testing.T) {
	at := eRecAt
	const p = "Req.query.bind[0]"
	for _, c := range []struct {
		name, rec, live, held string
		full                  bool
	}{
		{"letters and digits", "a1b2c3d4", "k5m4n6p8", "k5m4n6p8", false},
		{"an id with '_' inside", "abc-def", "abc_def", "abc_def", false},
		{"no letter or digit", "x --", "x __", "__", false},
		{"past the held budget", "a1b2c3d4", "k5m4n6p8", "k5m4n6p8", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			mocks := fillers(40, at)
			holder := holderMock("holder", at.Add(-20500*time.Millisecond), `{"note": "`+c.held+`"}`)
			raw := func() []Raw {
				out := make([]Raw, 0, len(mocks)+1)
				for i, m := range mocks {
					out = append(out, RawOf(m, true).ServedAs(i+1).ServedAsRecorded())
				}
				return append(out, RawOf(holder, false))
			}
			rec := Shape{Leaves: map[string]Leaf{p: {Path: p, Type: String, Value: c.rec}}}
			live := Shape{Leaves: map[string]Leaf{p: {Path: p, Type: String, Value: c.live}}}
			classify := func(dc *DriftCache) []Learned {
				return ClassifyFollowing(dc, []string{p}, live, rec, at, nil, raw(), nil, nil, nil)
			}
			want := classify(noSet(NewDriftCache()))
			if want[0].Kind != KindUnaccounted || !strings.Contains(want[0].Why, "holder") {
				t.Fatalf("a cache of no tracker learned %s (%s), want it refused, naming the holder: the row refuses nothing", want[0].Kind, want[0].Why)
			}
			tr := NewTracker()
			tr.NewSet()
			if c.full {
				keepsNoMoreHeld(tr)
			}
			for test := 1; test <= 2; test++ {
				tr.Begin()
				if got := classify(tr.Drifts()); !reflect.DeepEqual(got, want) {
					t.Errorf("test %d: learned %s (%s), want %s (%s), a cache of no tracker's", test, got[0].Kind, got[0].Why, want[0].Kind, want[0].Why)
				}
				tr.End()
			}
		})
	}
}

// Every run of letters and digits of every word a recording's readings hold
// (searchedTokens) is in the recording's filter of words (searchWords), so a
// filter that lacks a run of a changed word proves the recording holds none
// of it (mayHoldWords): its text, decoded, case-folded, quoted-printable and
// base64, a word of '_' or '-' between letters and digits, a word of another
// script, and its answer's own clock read beside its words (the forms each
// blanked by itself where the filter's are blanked joined), with its stamps
// searched and not.
func TestEveryRunOfAWordARecordingsReadingsHoldIsInItsFilterOfWords(t *testing.T) {
	holder := func(value string) *models.Mock { return holderMock("holder", eRecAt, `{"note": "`+value+`"}`) }
	for _, c := range []struct {
		name  string
		mock  *models.Mock
		clock bool
	}{
		{"letters and digits", holder("a1b2c3d4 k5m4"), false},
		{"an id with '_' and '-'", holder("abc_def-ghi9 j_k"), false},
		{"escaped and case-folded", holder(`Ab\u0043d\u00e9F x\nYz`), false},
		{"quoted-printable", holder("caf=C3=A9 =41BC=44"), false},
		{"base64", holder("token c2VjcmV0LVRva2VuIHh5ejEyMw=="), false},
		{"another script", holder("héllo wörld 日本"), false},
		{"its own clock reads and stamp beside its words", configReceive(`{"Note": "at id9 Ab\u0043d x2026"}`), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := RawOf(c.mock, false)
			if c.clock && len(x.AnswerClock()) == 0 {
				t.Fatalf("the answer holds no clock read of its own: the row blanks nothing")
			}
			tr := NewTracker()
			tr.NewSet()
			tr.Begin()
			defer tr.End()
			dc := tr.Drifts()
			for _, keep := range []bool{false, true} {
				words, ok := dc.searchWords(x, keep)
				if !ok {
					t.Fatalf("keep=%v: the set keeps no filter of the recording's words", keep)
				}
				n := 0
				for _, forms := range dc.searchedTokens(x, keep) {
					for w := range forms {
						for _, r := range wordsBy(w, isWord) {
							n++
							if !words.has(r) {
								t.Errorf("keep=%v: the recording's readings hold %q, whose run %q its filter of words lacks", keep, w, r)
							}
						}
					}
				}
				if n == 0 {
					t.Fatalf("keep=%v: the recording's readings hold no word", keep)
				}
			}
		})
	}
}

// A call's own places (ownPlaces) are its recordings but another test's, in
// order, read where they stand: raw itself where none is another test's, a
// part of it where they all come last (as a set's later test reads the
// earlier tests'), and a copy only where one stands among the call's own. A
// copy per call of a set's recordings was 277 MB of api-server's heap at
// once over its handlers.
func TestACallsOwnPlacesAreItsRecordingsWhereTheyStandUnlessAnotherTestsComesBetween(t *testing.T) {
	own := func(i int) Raw { return RawOf(holderMock(fmt.Sprintf("own-%d", i), eRecAt, "v"), false) }
	other := func(i int) Raw { return own(i).OfAnotherTest() }
	names := func(raw []Raw) []string {
		var out []string
		for _, x := range raw {
			out = append(out, x.Mock())
		}
		return out
	}
	for _, c := range []struct {
		name   string
		raw    []Raw
		want   []string
		shared bool
	}{
		{"none another test's", []Raw{own(1), own(2), own(3)}, []string{"own-1", "own-2", "own-3"}, true},
		{"another test's all last", []Raw{own(1), own(2), other(3), other(4)}, []string{"own-1", "own-2"}, true},
		{"another test's between", []Raw{own(1), other(2), own(3), other(4)}, []string{"own-1", "own-3"}, false},
		{"another test's first", []Raw{other(1), own(2)}, []string{"own-2"}, false},
		{"all another test's", []Raw{other(1), other(2)}, nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := ownPlaces(c.raw)
			if !reflect.DeepEqual(names(got), c.want) {
				t.Fatalf("own places %q, want %q", names(got), c.want)
			}
			if shared := len(got) == 0 || &got[0] == &c.raw[0]; shared != c.shared {
				t.Errorf("own places read where raw stands: %v, want %v", shared, c.shared)
			}
			if cap(got) > len(got) && len(got) > 0 && &got[0] == &c.raw[0] {
				t.Errorf("own places of raw's array have room past them (%d of %d): an append would write over another test's place", cap(got), len(got))
			}
		})
	}
}
