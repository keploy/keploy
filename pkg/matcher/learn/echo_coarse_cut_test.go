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
