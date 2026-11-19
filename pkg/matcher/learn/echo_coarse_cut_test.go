package learn

import (
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// An audit's nanosecond clock read (an insert's auditedAtNanos, a
// $numberLong text) its call's window holds in both runs, where round F
// read it on an hour whose whole second a recording F served before the
// call holds ("1795035600", 21:00:00, recorded on that second by another
// call of the set). The echo of the places outside the call's scope takes
// the hour, F's moment truncated to it, for a pick from that recording; but
// it names the read only at the hour, a cut two stamps of the runs' timing
// share by chance, as an answer of the scope may (the instant order's
// step 5 reads such a hold as the call's own read where its window holds
// it). So the read is a Clock, whether the hour stands in a recording of
// the call's own scope while another test's answer holds an unrelated
// instant (the outside echo then ran over the call's own places: refused
// instant-pick-outside-scope before) or in another test's answer
// (instant-another-scope before). Where no window holds the read, the
// refusal stands; and a place that holds round F's very nanosecond is a
// copy at a fine cut, refused at once, window or not.
func TestOutcomeANanosecondReadOnTheHourOfAServedSecondIsAClock(t *testing.T) {
	rAudit, fAudit := int64(1795035151700359825), int64(1795035685607825904)
	rCall, fCall := time.Unix(0, rAudit).Add(1149*time.Microsecond), time.Unix(0, fAudit).Add(1149*time.Microsecond)
	const p = "body.docs.msgs.0.auditedAtNanos.$numberLong"
	win := &Window{Rec: Span{From: rCall.Add(-5 * time.Millisecond), To: rCall}, F: Span{From: fCall.Add(-5 * time.Millisecond), To: fCall}}
	pg := func(name string, at time.Time, word string) Raw {
		return RawOf(&models.Mock{Name: name, Kind: models.PostgresV3, Spec: models.MockSpec{
			ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
			PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
				SQLNormalized: "SELECT audited_at FROM " + name,
				Response:      &models.PostgresV3Response{Rows: []models.PostgresV3Cells{{{Value: word}}}, CommandComplete: "SELECT 1"},
			}},
		}}, false)
	}
	hour := pg("mock-audits", time.Unix(1795035600, 500_000_000), "1795035600").ServedAs(1).ServedAsRecorded()
	other := rCall.Add(-40 * time.Second)
	unrelated := pg("mock-213", other, other.UTC().Format(time.RFC3339)).ServedAs(2).ServedAsRecorded().OfAnotherTest()
	decide := func(w *Window, raw ...Raw) Decision {
		in := iCall(p, String, strconv.FormatInt(rAudit, 10), strconv.FormatInt(fAudit, 10), rCall, fCall, w, nil, raw)
		in.Drifts = oClientDrifts()
		d := Decide(in)
		if len(d) != 1 {
			t.Fatalf("%d decisions: %s", len(d), oDescribe(d))
		}
		return d[0]
	}
	refused := func(what string, d Decision, check string) {
		t.Helper()
		if d.Refusal == nil || d.Refusal.Check != check {
			t.Errorf("%s: %s; want refused %s", what, oDescribe([]Decision{d}), check)
		}
	}

	iClock(t, "the hour in a recording of the scope, another test's answer beside it", decide(win, hour, unrelated))
	iClock(t, "the hour in another test's answer", decide(win, hour.OfAnotherTest(), unrelated))
	iClock(t, "the hour alone", decide(win, hour))
	iClock(t, "another test's unrelated answer alone", decide(win, unrelated))

	refused("the hour in a recording of the scope, no window", decide(nil, hour, unrelated), "instant-pick-outside-scope")
	refused("the hour in another test's answer, no window", decide(nil, hour.OfAnotherTest(), unrelated), "instant-another-scope")

	// A copy at a fine cut stays refused, whatever step refuses it first.
	exact := pg("mock-copy", fCall.Add(-2*time.Millisecond), strconv.FormatInt(fAudit, 10)).ServedAs(1).ServedAsRecorded().OfAnotherTest()
	if d := decide(win, exact, unrelated); d.Refusal == nil {
		t.Errorf("round F's nanosecond in another test's answer: %s; want refused", oDescribe([]Decision{d}))
	}
}

// echoPg is a recording of one PostgreSQL row holding word, recorded at at.
func echoPg(name string, at time.Time, word string) Raw {
	return RawOf(&models.Mock{Name: name, Kind: models.PostgresV3, Spec: models.MockSpec{
		ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
		PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
			SQLNormalized: "SELECT audited_at FROM " + name,
			Response:      &models.PostgresV3Response{Rows: []models.PostgresV3Cells{{{Value: word}}}, CommandComplete: "SELECT 1"},
		}},
	}}, false)
}

// The other side of the outside echo's cut: a place outside the call's
// scope, or one the echo reads beside another test's answer, that names the
// moment at a cut finer than a millisecond (an instant whose microsecond is
// the value, the very nanosecond the call recorded) is a pick from it,
// refused at once, though the call's own window holds the read; one that
// names it at a second or at the hour defers to that window, as an answer of
// the scope does, whether it stands in another test's answer or in a
// recording of the set F gave earlier.
func TestOutcomeAFineHoldOutsideTheScopeIsRefusedThoughTheWindowHoldsTheRead(t *testing.T) {
	rNs, fNs := int64(1795035151700359825), int64(1795035685607825904)
	hourAt := time.Unix(1795035600, 500_000_000)
	hour := echoPg("mock-audits", hourAt, "1795035600").ServedAs(1).ServedAsRecorded()
	type call struct {
		path, rec, live string
		rAt, fAt        time.Time
	}
	decide := func(c call, window bool, given []Raw, raw ...Raw) Decision {
		var w *Window
		if window {
			w = &Window{Rec: Span{From: c.rAt.Add(-5 * time.Millisecond), To: c.rAt}, F: Span{From: c.fAt.Add(-5 * time.Millisecond), To: c.fAt}}
		}
		in := iCall(c.path, String, c.rec, c.live, c.rAt, c.fAt, w, nil, raw)
		in.Drifts = oClientDrifts()
		if given != nil {
			in.Given = func() ([]GivenPlaces, error) {
				gs := make([]GivenPlaces, 0, len(given))
				for _, g := range given {
					gs = append(gs, PlacesOf(g))
				}
				return gs, nil
			}
		}
		d := Decide(in)
		if len(d) != 1 {
			t.Fatalf("%d decisions: %s", len(d), oDescribe(d))
		}
		return d[0]
	}
	refused := func(what string, d Decision, check string) {
		t.Helper()
		if d.Refusal == nil || d.Refusal.Check != check {
			t.Errorf("%s: %s; want refused %s", what, oDescribe([]Decision{d}), check)
		}
	}
	unrelatedAt := func(c call) Raw {
		other := c.rAt.Add(-40 * time.Second)
		return echoPg("mock-213", other, other.UTC().Format(time.RFC3339)).ServedAs(2).ServedAsRecorded().OfAnotherTest()
	}

	// A microsecond read: F's nanosecond instant names it at the microsecond.
	us := call{path: "body.docs.msgs.0.auditedAtMicros.$numberLong", rec: strconv.FormatInt(rNs/1000, 10), live: strconv.FormatInt(fNs/1000, 10)}
	us.rAt, us.fAt = time.UnixMicro(rNs/1000).Add(1149*time.Microsecond), time.UnixMicro(fNs/1000).Add(1149*time.Microsecond)
	before := us.rAt.Add(-2 * time.Millisecond)
	fWord := strconv.FormatInt(fNs, 10)
	inScope := echoPg("mock-c", before, fWord).ServedAs(1).ServedAsRecorded()
	refused("F's nanosecond in a recording of the scope, another test's answer beside it", decide(us, true, nil, inScope, unrelatedAt(us)), "instant-pick-outside-scope")
	refused("the hour, and F's nanosecond in another test's answer", decide(us, true, nil, hour, inScope.OfAnotherTest(), unrelatedAt(us)), "instant-another-scope")

	// A nanosecond read: the very nanosecond the call recorded, in another
	// test's answer recorded before the call.
	ns := call{path: "body.docs.msgs.0.auditedAtNanos.$numberLong", rec: strconv.FormatInt(rNs, 10), live: fWord}
	ns.rAt, ns.fAt = time.Unix(0, rNs).Add(1149*time.Microsecond), time.Unix(0, fNs).Add(1149*time.Microsecond)
	recCopy := echoPg("mock-c", ns.rAt.Add(-2*time.Millisecond), ns.rec).ServedAs(1).ServedAsRecorded().OfAnotherTest()
	refused("the hour, and the recorded nanosecond in another test's answer", decide(ns, true, nil, hour, recCopy, unrelatedAt(ns)), "instant-another-scope")

	// A second read: the recorded second in another test's answer names it
	// at the second, a cut two stamps of the runs' timing can share.
	rS, fS := time.Unix(1795035151, 0).UTC(), time.Unix(1795035685, 0).UTC()
	sec := call{path: "body.docs.msgs.0.auditedAt", rec: rS.Format(time.RFC3339), live: fS.Format(time.RFC3339), rAt: rS.Add(700 * time.Millisecond), fAt: fS.Add(607 * time.Millisecond)}
	recSecond := echoPg("mock-c", sec.rAt.Add(-2*time.Millisecond), strconv.FormatInt(rS.Unix(), 10)).ServedAs(1).ServedAsRecorded().OfAnotherTest()
	iClock(t, "the hour, and the recorded second in another test's answer", decide(sec, true, nil, hour, recSecond, unrelatedAt(sec)))
	refused("the hour, and the recorded second in another test's answer, no window", decide(sec, false, nil, hour, recSecond, unrelatedAt(sec)), "instant-another-scope")

	// The hour in a recording of the set F gave earlier.
	iClock(t, "the hour in a recording of the set", decide(ns, true, []Raw{hour}, unrelatedAt(ns)))
	refused("the hour in a recording of the set, no window", decide(ns, false, []Raw{hour}, unrelatedAt(ns)), "instant-another-scope")
}
