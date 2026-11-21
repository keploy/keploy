package learn

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A staged set's staging (WarmSet) builds the filter of each recording's
// words (stageFilter), so none of its calls builds one: the set's first
// call that reads a word's places (decider.places) asks every recording
// before it its filter, and built each of them while its client waited
// (api-server's mongo round: up to 84 ms of one call). What the calls
// decide, scan, look up and read is what they do with no staging: the
// staging only builds, ahead of them, what the first call would have.
// Counted, not timed, over 300 and 3,000 recordings.
func TestASetsStagingBuildsEachRecordingsWordFilterSoItsCallsBuildNone(t *testing.T) {
	for _, n := range []int{300, 3000} {
		got, builds, scans, lookups, asks, _ := costSetStaged(n, false)
		sGot, sBuilds, sScans, sLookups, sAsks, use := costSetStaged(n, true)
		t.Logf("%d recordings: staging %+v; filters built by the calls %v staged, %v not", n, use, sBuilds, builds)
		if use.Staged != n || use.Filters != n || use.FiltersPast != 0 {
			t.Fatalf("over %d recordings the staging read %d and built %d filters (%d past the budget); want all %d, none past", n, use.Staged, use.Filters, use.FiltersPast, n)
		}
		if builds != [2]int{n, 0} {
			t.Fatalf("over %d recordings the calls of the set with no staging built %v filters; want each recording's once, in the first test: [%d 0]", n, builds, n)
		}
		if sBuilds != [2]int{0, 0} {
			t.Errorf("over %d recordings the calls of the staged set built %v filters; want none: the staging built every one", n, sBuilds)
		}
		if sGot != got {
			t.Errorf("over %d recordings the staged set's tests decide %q; the set with no staging decides %q", n, sGot, got)
		}
		if sScans != scans || sLookups != lookups || sAsks != asks {
			t.Errorf("over %d recordings the staged set's calls scanned %v, looked up %v and read %v places; with no staging %v, %v and %v", n, sScans, sLookups, sAsks, scans, lookups, asks)
		}
	}
}

// The staging builds the recordings' filters (stageFilters) only once its
// first pass read every recording within the set's budget, and then only
// with the budget's spare: where the first pass filled the budget
// (SetUse.HeldFull), it builds none, and the set keeps what it kept with no
// filter staged; where a filter does not fit the spare, it stops there
// (SetUse.FiltersPast), and the calls that ask the rest a word build them,
// or scan them, as with no staging. Either way the staged set's tests
// decide what the set with no staging decides at that budget. Over budgets
// from one the first pass fills to one it leaves a little spare in.
func TestASetsStagingLeavesTheFiltersPastItsBudgetToItsCalls(t *testing.T) {
	defer func(b int64) { learnBudget = b }(learnBudget)
	const n = 300
	filled, past := false, false
	for _, b := range []int64{8 << 10, 64 << 10, 512 << 10, 1 << 20, 3 << 19} {
		learnBudget = b
		got, _, _, _, _, _ := costSetStaged(n, false)
		sGot, sBuilds, _, _, _, use := costSetStaged(n, true)
		t.Logf("budget %d: staging %+v; filters built by the staged set's calls %v", b, use, sBuilds)
		if sGot != got {
			t.Errorf("at a budget of %d bytes the staged set's tests decide %q; the set with no staging decides %q", b, sGot, got)
		}
		switch {
		case use.HeldFull && (use.Filters != 0 || use.FiltersPast != 0):
			t.Errorf("at a budget of %d bytes the first pass filled it (read %d of %d), and the staging still built %d filters (%d past): want none", b, use.Staged, n, use.Filters, use.FiltersPast)
		case use.HeldFull:
			filled = true
		case use.FiltersPast > 0:
			past = true
			if use.Filters+use.FiltersPast >= n {
				t.Errorf("at a budget of %d bytes the staging built %d filters and counted %d past: want it stopped at the first that did not fit, short of all %d", b, use.Filters, use.FiltersPast, n)
			}
		}
	}
	if !filled || !past {
		t.Fatalf("no budget had the first pass fill it (%v), or none left a filter past its spare (%v): the row reads neither fallback", filled, past)
	}
}

// The forms a recording's word filter is built from, read from the
// decoding of its text the set keeps (keptForms), are the forms textForms
// derives of that text, byte for byte and in order: its escapes, its
// quoted-printable form, its encoded runs and their case-foldings, of a
// text that holds an instant too. So the filter the staging and the calls
// now build with no second decode of the text is the one they built
// before.
func TestAFiltersFormsReadFromTheSetsKeptDecodingAreTheTextsOwnFormsByteForByte(t *testing.T) {
	bodies := []string{
		`{"note":"plain lower words","ref":"ref-00001"}`,
		`{"note":"Café \"Quoted\" Line\nTwo","ref":"A1B2C3D4"}`,
		`{"note":"=C3=A9t=C3=A9 Soft=\r\nBreak","ref":"QP-0002"}`,
		`{"note":"aGVsbG8gV29ybGQgZnJvbSBhIGJhc2U2NCBydW4=","ref":"B64-0003"}`,
		`{"note":"Seen At 2026-10-24T09:55:53.9Z","ref":"Stamp-0004"}`,
	}
	mocks := make([]*models.Mock, len(bodies))
	for i, b := range bodies {
		mocks[i] = getAnswer(fmt.Sprintf("forms-%d", i), fmt.Sprintf("http://api/forms/%d", i), b, eRecAt.Add(-time.Duration(len(bodies)-i)*time.Second))
	}
	tr := NewTracker()
	tr.NewSet()
	if use := <-tr.WarmSet(srcOf(mocks)); use.Staged != len(mocks) || use.Filters != len(mocks) {
		t.Fatalf("the staging of %d recordings reported %+v: want each read and its filter built", len(mocks), use)
	}
	tr.Begin()
	defer tr.End()
	dc := tr.Drifts()
	ix := newReaderIndex(dc)
	defer ix.release()
	var dec, qp, b64 bool
	for i, m := range mocks {
		x := dc.LazyRawOf(m, false, withAnswer(m))
		text := x.payload().text
		d, ok := dc.set.keptDecoded(x.id)
		if !ok {
			t.Fatalf("recording %d: the staged set keeps no decoding of its text", i)
		}
		dec, qp, b64 = dec || d.hasDec, qp || d.hasQ, b64 || d.b != ""
		got, ok := ix.textOf(x).keptForms(text)
		if want := textForms(text); !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("recording %d's forms read from the set's decoding are %q (%v); its text's own forms are %q", i, got, ok, want)
		}
	}
	if !dec || !qp || !b64 {
		t.Fatalf("the row's texts decode escapes %v, quoted-printable %v, an encoded run %v: it compares not every form", dec, qp, b64)
	}
	if _, ok := (&readerText{x: ix, text: bodies[0]}).keptForms(bodies[0]); ok {
		t.Errorf("a handed text, of no recording, read forms from the set's decoding: want none (it has no identity)")
	}
}

// The staging builds each recording's word filter (stageFilters) from the
// decoding of its text its first pass kept (keptForms), so a staged set's
// payloads are each decoded once, by the staging, its filters included,
// and none again by its tests' calls, which build no filter. Before, each
// filter's build decoded its text a second time (textForms). The tests
// decide, scan, look up and read what they do with no staging.
func TestARecordingsStagedFilterIsBuiltFromTheSetsDecodingOfItsTextNotASecondDecode(t *testing.T) {
	const n = 300
	want, wBuilds, wScans, wLookups, wAsks := costSet(n)
	mocks := costMocks(n)
	raw := make([]Raw, len(mocks))
	for i, m := range mocks {
		raw[i] = RawOf(m, false)
	}
	counts := countDecodes(t, raw, nil)
	got, builds, scans, lookups, asks, use := costSetOf(mocks, true)
	payloads, decodes := decodesOf(counts)
	t.Logf("staging %+v; filters built by the calls %v; %d payloads decoded %d times", use, builds, payloads, decodes)
	if use.Staged != n || use.Filters != n || builds != [2]int{0, 0} || wBuilds != [2]int{n, 0} {
		t.Fatalf("the staging read %d and built %d filters, its tests' calls built %v (with no staging %v): want all %d by the staging, none by the calls", use.Staged, use.Filters, builds, wBuilds, n)
	}
	if payloads != n || decodes != n {
		t.Errorf("the staged set decoded %d of its %d payloads %d times: want each once", payloads, n, decodes)
	}
	if got != want || scans != wScans || lookups != wLookups || asks != wAsks {
		t.Errorf("the staged set's tests decide %q, scan %v, look up %v and read %v places; with no staging %q, %v, %v and %v", got, scans, lookups, asks, want, wScans, wLookups, wAsks)
	}
}

// A recording whose text the set keeps no decoding of (past its forms
// tier's cap) has no filter staged (stageFilter): the staging decodes no text
// for a filter, counts it left to the calls (SetUse.FiltersPast), and goes
// on to the next. The calls that ask it a word build its filter as with no
// staging, and the tests decide what they decide with none.
func TestAStagingLeavesTheFilterOfARecordingWhoseDecodingTheSetKeepsNotToItsCalls(t *testing.T) {
	defer func(b int64) { formsBudget = b }(formsBudget)
	const n = 300
	formsBudget = 0
	want, _, _, _, _, _ := costSetStaged(n, false)
	mocks := costMocks(n)
	raw := make([]Raw, len(mocks))
	for i, m := range mocks {
		raw[i] = RawOf(m, false)
	}
	counts := countDecodes(t, raw, nil)
	tr := NewTracker()
	tr.NewSet()
	use := <-tr.WarmSet(srcOf(mocks))
	payloads, decodes := decodesOf(counts)
	t.Logf("staging %+v; %d payloads decoded %d times", use, payloads, decodes)
	if use.Staged != n || use.Filters != 0 || use.FiltersPast != n || use.Forms != 0 {
		t.Fatalf("with no decoding kept the staging reported %+v: want all %d read, no filter built, each left to the calls", use, n)
	}
	if payloads != n || decodes != n {
		t.Errorf("the staging decoded %d of the set's %d payloads %d times: want each once, by its first pass; its filters' pass decodes none", payloads, n, decodes)
	}
	got := [2]string{}
	for c := range got {
		tr.Begin()
		dc := tr.Drifts()
		got[c] = oDescribe(Decide(costCall(dc, "body.ids", "ids a1b2c3d4 b2c3d4e5 c3d4e5f6 d4e5f6a7 e5f6a7b8", "ids f0e9d8c7 e9d8c7b6 d8c7b6a5 c7b6a5f4 b6a5f4e3", eRecAt, nil, lazyOf(dc, mocks), nil, nil)))
		tr.End()
	}
	if got != want {
		t.Errorf("the set its staging left every filter of decides %q; with no staging %q", got, want)
	}
}
