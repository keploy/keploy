package learn

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// The other way about (excusedLive): a random id the application makes
// (api-server's appId) whose 4-hex group stands by chance in what record's
// full round gave the application in an earlier answer of its own test,
// served changed (F gave the application its own id there: another id of
// the test, F's), where the recording gave another id that the recorded
// value took none of the words of, is learned as varying under the rule a
// copy follows its source: a copy moves with its source, so the recorded
// value, which took nothing of what the recording gave there, is no copy
// of it. Everywhere else the word stays refused, naming why: a word the
// application sent there itself (its request), a view giving the word at a
// field the recording lacks, a recorded value that took a word of what the
// recording gave there (in any case) or holds the word too, a place that
// lost no word, one answered at the call or with no answer time. keploy test
// (ClassifyGiven) refuses it as before. Nothing reads the id's shape.
func TestAChanceWordOfWhatTheFullRoundGaveIsLearnedAsVarying(t *testing.T) {
	const p = "body.appId"
	const (
		recID   = "bb7ed9ee-670e-4aa1-982f-b54b2b73982f" // the earlier answer's id, as recorded
		liveID  = "e1f2a3b4-c5d6-48d1-8a9b-0c1d2e3f4a5b" // F's id there: "48d1" by chance
		chance  = "5c0e9a7b-1f2d-48d1-9a3b-c4d5e6f70812" // the call's random appId F saw
		other   = "0c1d2e3f-4a5b-4c7d-8e9f-a0b1c2d3e4f5" // the call's appId, as recorded
		sharing = "bb7ed9ee-4a5b-4c7d-8e9f-a0b1c2d3e4f5" // a recorded appId that took recID's first group
		inside  = "0c1d2e3f-4a5b-4c7d-8e9f-b54b2b7398"   // a recorded appId whose last group stands inside recID's
	)
	before := r1Insert.Add(-time.Minute)
	src := func(id string, req, res time.Time) *models.Mock {
		m := receiveOf("mock-31", id, req)
		m.Spec.ResTimestampMock = res
		return m
	}
	viewOf := func(live *models.Mock) func(dc *DriftCache, m *models.Mock) Raw {
		return func(dc *DriftCache, m *models.Mock) Raw {
			return dc.RawOf(m, true).ServedAs(3).ServedLive(dc.RawOf(live, true))
		}
	}
	okSrc := src(recID, before, before.Add(time.Second))
	okView := src(liveID, before, before.Add(time.Second))
	// The word in what the application sent there itself (its request),
	// where it sent another id at record.
	sent := func(id string) *models.Mock {
		m := src(recID, before, before.Add(time.Second))
		m.Spec.HTTPReq.Body = strings.Replace(m.Spec.HTTPReq.Body, `"WaitTimeSeconds":4`, `"WaitTimeSeconds":4,"After":"`+id+`"`, 1)
		return m
	}
	sentRec, sentView := sent(recID), sent(liveID)
	// The word at a field the recording lacks.
	newField := src(recID, before, before.Add(time.Second))
	newField.Spec.HTTPResp.Header = map[string]string{"X-Trace": liveID}
	for k, v := range okSrc.Spec.HTTPResp.Header {
		newField.Spec.HTTPResp.Header[k] = v
	}
	// The word at the id's field and at a field the recording lacks.
	twoFields := src(liveID, before, before.Add(time.Second))
	twoFields.Spec.HTTPResp.Header = newField.Spec.HTTPResp.Header
	// A view that only added the word: the place lost none.
	added := src(recID+" 48d1", before, before.Add(time.Second))
	for _, m := range []*models.Mock{okView, sentRec, newField, added} {
		if m.Spec.HTTPReq.Body == okSrc.Spec.HTTPReq.Body && m.Spec.HTTPResp.Body == okSrc.Spec.HTTPResp.Body && len(m.Spec.HTTPResp.Header) == len(okSrc.Spec.HTTPResp.Header) {
			t.Fatal("the view did not change")
		}
	}
	for _, row := range []struct {
		name      string
		m         *models.Mock
		raw       func(dc *DriftCache, m *models.Mock) Raw
		rec, live string
		want      string // "" noise under ClassifyFollowing
	}{
		{"F gave its own id there, the recording another id", okSrc, viewOf(okView), other, chance, ""},
		{"a word the application sent there itself", sentRec, viewOf(sentView), other, chance, "in what the application sent there and not in an answer it was given"},
		{"the word at a field the recording lacks", okSrc, viewOf(newField), other, chance, "which the recording lacks"},
		{"the word at the id's field and at a field the recording lacks", okSrc, viewOf(twoFields), other, chance, "which the recording lacks"},
		{"the recorded value took a word of what the recording gave there", okSrc, viewOf(okView), sharing, "bb7ed9ee-1f2d-48d1-9a3b-c4d5e6f70812", "and the recorded value took a word of it"},
		{"the recorded value took a word of what the recording gave there, in another case", okSrc, viewOf(okView), strings.ToUpper(sharing[:8]) + sharing[8:], "BB7ED9EE-1f2d-48d1-9a3b-c4d5e6f70812", "and the recorded value took a word of it"},
		{"what the recording gave there holds a word the recorded value lost", okSrc, viewOf(okView), inside, chance, "and it holds a word the recorded value lost"},
		{"the place lost no word", okSrc, viewOf(added), other, chance, "which holds no word record's full round's lacks"},
		{"the recorded value holds the word too, in another order", okSrc, viewOf(okView), "48d1-0c1d2e3f-4a5b", "0c1d2e3f-4a5b-48d1", "the recorded value holds that word too"},
		{"answered at the call", src(recID, before, r1Insert), viewOf(src(liveID, before, r1Insert)), other, chance, "the recording did not give the application that payload before the call"},
		{"no answer time", src(recID, before, time.Time{}), viewOf(src(liveID, before, time.Time{})), other, chance, "keploy cannot read when the recording answered mock-31"},
	} {
		for _, follow := range []bool{false, true} {
			dc := clockedDrifts()
			x := row.raw(dc, row.m)
			if !x.Live() {
				t.Fatalf("%s: no view", row.name)
			}
			classify := ClassifyGiven
			if follow {
				classify = ClassifyFollowing
			}
			ln := classify(dc, []string{p}, strLeaf(p, row.live), strLeaf(p, row.rec), r1Insert, nil, []Raw{x}, nil, nil, nil)[0]
			refused := "where the recording did not give the application that word"
			switch {
			case follow && row.want == "":
				if ln.Kind != KindNoise || !strings.Contains(ln.Why, "where the recording gave the application another value, which the recorded value took none of the words of") {
					t.Errorf("%s: %s (%s); want noise naming why", row.name, ln.Kind, ln.Why)
				}
			case follow:
				if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, refused) || !strings.Contains(ln.Why, row.want) {
					t.Errorf("%s: %s (%s); want it refused: %s", row.name, ln.Kind, ln.Why, row.want)
				}
			default:
				if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, refused) {
					t.Errorf("%s, keploy test: %s (%s); want it refused as before", row.name, ln.Kind, ln.Why)
				}
			}
		}
	}
}

// A word of the value F saw that no one field of F's view of a payload holds
// (only its text across two fields, or a form of the whole) shows nothing of
// what the recording gave at a field, and stays refused; nor does a payload
// with no view. The classifier reaches neither today: a word the view holds
// is held by one of its fields, and only a payload with a view is read.
func TestAChanceWordNoOneFieldOfTheViewHoldsIsRefused(t *testing.T) {
	const (
		recID  = "bb7ed9ee-670e-4aa1-982f-b54b2b73982f"
		liveID = "e1f2a3b4-c5d6-48d1-8a9b-0c1d2e3f4a5b"
		chance = "5c0e9a7b-1f2d-48d1-9a3b-c4d5e6f70812"
		other  = "0c1d2e3f-4a5b-4c7d-8e9f-a0b1c2d3e4f5"
	)
	before := r1Insert.Add(-time.Minute)
	dc := clockedDrifts()
	served := dc.RawOf(receiveOf("mock-31", recID, before), true).ServedAs(3)
	x := served.ServedLive(dc.RawOf(receiveOf("mock-31", liveID, before), true))
	v := dc.of(other, chance)
	none := func(string) bool { return false }
	if view, _ := dc.recordedThere(x, v, none); view != fNone {
		t.Errorf("a word no field holds: %v; want fNone", view)
	}
	if view, _ := dc.recordedThere(served, v, func(string) bool { return true }); view != fNone {
		t.Errorf("a payload with no view: %v; want fNone", view)
	}
	f := &follows{dc: dc, rec: other, live: chance, at: r1Insert}
	if f.excusedLive(x, v, 0, "zzzz", r1Insert, "why", "mock-31's payload") || !strings.Contains(f.refused, "no one field of what record's full round gave there holds that word") {
		t.Errorf("a word no field holds: excused, or refused as %q", f.refused)
	}
	if f.first != "" || f.moved != "" || f.sources["mock-31"] {
		t.Errorf("a refused word was noted: first %q, moved %q, sources %v", f.first, f.moved, f.sources)
	}
}

// The order the two runs gave the application the value's candidate
// sources is read for a word let go the other way about too: a place F's
// view gave the word at is a candidate source. Where F's serves show it
// nowhere among the earlier answers (the two runs then gave them in a
// different order), or keploy does not know where F served another
// candidate source among them, the value is refused for the order.
func TestAChanceWordOfWhatTheFullRoundGaveIsOrderedWithTheOtherSources(t *testing.T) {
	const p = "body.appId"
	const (
		recID  = "bb7ed9ee-670e-4aa1-982f-b54b2b73982f"
		liveID = "e1f2a3b4-c5d6-48d1-8a9b-0c1d2e3f4a5b"
		chance = "5c0e9a7b-1f2d-48d1-9a3b-c4d5e6f70812"
		other  = "0c1d2e3f-4a5b-4c7d-8e9f-a0b1c2d3e4f5"
	)
	before := r1Insert.Add(-time.Minute)
	for _, row := range []struct {
		name string
		raw  func(dc *DriftCache) []Raw
		want string // "" noise
	}{
		{"one source, served at a known place", func(dc *DriftCache) []Raw {
			return []Raw{dc.RawOf(receiveOf("mock-31", recID, before), true).ServedAs(3).ServedLive(dc.RawOf(receiveOf("mock-31", liveID, before), true))}
		}, ""},
		{"one source, served at a place keploy does not know", func(dc *DriftCache) []Raw {
			return []Raw{dc.RawOf(receiveOf("mock-31", recID, before), true).ServedLive(dc.RawOf(receiveOf("mock-31", liveID, before), true))}
		}, "gave the application the earlier answers in a different order"},
		{"and another test's chance word, served at a place keploy does not know", func(dc *DriftCache) []Raw {
			return []Raw{
				dc.RawOf(receiveOf("mock-31", recID, before), true).ServedAs(3).ServedLive(dc.RawOf(receiveOf("mock-31", liveID, before), true)),
				dc.RawOf(receiveOf("mock-32", "aaaaaaaa-1f2d-4222-8333-bbbbbbbbbbbb", before.Add(5*time.Second)), true).ServedAsRecorded(),
			}
		}, "keploy does not know where record's full round served mock-"},
	} {
		dc := clockedDrifts()
		ln := ClassifyFollowing(dc, []string{p}, strLeaf(p, chance), strLeaf(p, other), r1Insert, nil, row.raw(dc), nil, nil, nil)[0]
		switch {
		case row.want == "" && ln.Kind != KindNoise:
			t.Errorf("%s: %s (%s); want noise", row.name, ln.Kind, ln.Why)
		case row.want != "" && (ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, row.want)):
			t.Errorf("%s: %s (%s); want it refused for the order: %s", row.name, ln.Kind, ln.Why, row.want)
		}
	}
}

// A recorded value that took a word of the payload whose view gives the
// chance word, at a field the search of that payload otherwise blanks (its
// answer's own clock stamp, the Date header: rules 2 and 3), stays refused
// under the rule a copy follows its source, naming that field: the value may
// have followed the payload's words wherever F gave them (recordedTook reads
// stamps as any text). keploy test refuses it as before.
func TestAChanceWordStaysRefusedWhereTheRecordedValueTookAWordOfThePayloadsStamp(t *testing.T) {
	const p = "body.appId"
	const (
		recID  = "bb7ed9ee-670e-4aa1-982f-b54b2b73982f"
		liveID = "e1f2a3b4-c5d6-48d1-8a9b-0c1d2e3f4a5b"
		chance = "5c0e9a7b-1f2d-48d1-9a3b-c4d5e6f70812"
		other  = "0c1d2e3f-4a5b-4c7d-8e9f-a0b1c2d3e4f5"
	)
	before := r1Insert.Add(-time.Minute)
	src, view := receiveOf("mock-31", recID, before), receiveOf("mock-31", liveID, before)
	// The answer's own stamp: its Date header, in the span it was asked and
	// answered in, as both runs gave it.
	for _, m := range []*models.Mock{src, view} {
		m.Spec.HTTPResp.Header["Date"] = m.Spec.ResTimestampMock.Format(http.TimeFormat)
	}
	if x := RawOf(src, true); len(x.payload().stamps) != 1 || !strings.HasPrefix(src.Spec.HTTPResp.Header["Date"], "Thu, ") || strings.Contains(src.Spec.HTTPResp.Body, "Thu") {
		t.Fatalf("the answer's Date header %q is not its one stamp, or not the only text holding Thu", src.Spec.HTTPResp.Header["Date"])
	}
	for _, row := range []struct {
		name, rec, want string
	}{
		{"after the id", other + "-Thu", "(its characters 38 to 40) stands in mock-31 at spec.resp.header.Date, as the recording gave it, so the value may have followed that payload's words"},
		{"before the id", "Thu-" + other, "(its characters 1 to 3) stands in mock-31 at spec.resp.header.Date, as the recording gave it, so the value may have followed that payload's words"},
	} {
		for _, follow := range []bool{false, true} {
			dc := clockedDrifts()
			x := dc.RawOf(src, true).ServedAs(3).ServedLive(dc.RawOf(view, true))
			classify := ClassifyGiven
			if follow {
				classify = ClassifyFollowing
			}
			ln := classify(dc, []string{p}, strLeaf(p, chance), strLeaf(p, row.rec), r1Insert, nil, []Raw{x}, nil, nil, nil)[0]
			switch {
			case follow && (ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, row.want)):
				t.Errorf("%s: %s (%s); want it refused: %s", row.name, ln.Kind, ln.Why, row.want)
			case !follow && (ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "where the recording did not give the application that word")):
				t.Errorf("%s, keploy test: %s (%s); want it refused as before", row.name, ln.Kind, ln.Why)
			}
		}
	}
}

// A place whose recorded value holds the drifted value's recorded moment in
// another layout (epoch seconds or milliseconds against an RFC 3339 instant),
// and whose value in F holds the moment F saw, drifted with the value
// (sources: holdsMomentOf), so it stays where the value may have come from
// (movedFrom), though it lost no word the value holds and holds none the
// value lost. A place holding other moments shows the value no copy. The
// classifier's moment echo reads such an instant before any word is let go
// the other way about, so movedFrom's own check is pinned here, as
// recordedThere's are above.
func TestAPlaceHoldingTheValuesMomentInAnotherLayoutStaysWhereItMayHaveComeFrom(t *testing.T) {
	const rec, live = "2026-10-22T11:14:16Z", "2026-10-23T09:05:47Z"
	for _, row := range []struct {
		name, edRec, edLive, want string
	}{
		{"epoch seconds", "1792667656", "1792746347", "and the value drifted with it"},
		{"epoch milliseconds", "1792667656000", "1792746347000", "and the value drifted with it"},
		{"other moments", "1792600000", "1792700000", ""},
		{"the recorded moment at record only", "1792667656", "1792700000", ""},
	} {
		dc := clockedDrifts()
		if got := movedFrom(dc.of(row.edRec, row.edLive), dc.of(rec, live)); got != row.want {
			t.Errorf("%s: %q; want %q", row.name, got, row.want)
		}
	}
}
