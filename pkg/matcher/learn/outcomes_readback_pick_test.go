package learn

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// The rows below are built from TestSlowStepFailsOnlyItself's recordings
// (api-server's e2e suite under keploy record, 2026-11-13 09:38 UTC, and
// its round F at 09:39). The test posts its own HTTP server's answer to the
// application, which writes the answer's Date (a second) in four inserts.
// Before them it read back (mock-1780) the record its insert mock-1763 had
// stamped created_at in epoch seconds, a stamp that call learned as its own
// read of the clock. When recorded the stamp and the Date fell in two
// seconds (09:38:01, 09:38:02); in round F they fell in one (09:39:04), and
// each insert was refused as a pick from the read-back: "round F's moment
// stands in mock-1780 resp...created_at.$numberLong, which does not show
// it". The fourth insert, whose read-back keploy did not see, learned the
// Date as a read of the clock its window holds.

const (
	rpCall   = "body.docs.msgs.0.event.response.headers.Date"
	rpDateR  = "Fri, 13 Nov 2026 09:38:02 GMT"
	rpDateF  = "Fri, 13 Nov 2026 09:39:04 GMT"
	rpStampR = "1794562681" // 2026-11-13T09:38:01Z
	rpStampF = "1794562744" // 2026-11-13T09:39:04Z
	rpSentP  = "body.docs.msgs.0.created_at.$numberLong"
	rpAnsP   = "cursor.firstBatch.0.created_at.$numberLong"
)

var (
	rpRecAt = time.Date(2026, 11, 13, 9, 38, 2, 250*int(time.Millisecond), time.UTC)
	rpFAt   = time.Date(2026, 11, 13, 9, 39, 4, 400*int(time.Millisecond), time.UTC)
	rpAnsAt = rpRecAt.Add(-50 * time.Millisecond)
)

// rpSent is mock-1763, the insert that stamped the record, sent at at; its
// call learned the stamp as its own read of the clock at the second where
// clock is set.
func rpSent(at time.Time, rec, live string, clock bool) Earlier {
	e := iSent("mock-1763", at, rpSentP, Number, rec, live)
	if clock {
		e.Seen[0].Clock, e.Seen[0].Offset = true, &models.ClockOffset{Res: time.Second}
	}
	return e
}

// rpAnswer is mock-1780, the find that read the record back, as an earlier
// call's learned leaf.
func rpAnswer(rec, live string) Earlier {
	return Earlier{Mock: "mock-1780", At: rpAnsAt, Own: true, Payload: oPayload("mock-1780", rpAnsAt), Seen: []Seen{
		{Side: Response, Leaf: Leaf{Path: rpAnsP, Noise: rpAnsP, Type: Number, Value: rec}, Live: live}}}
}

// rpRaw is mock-1780 as a recording, its whole answer as recorded and as
// round F served it.
func rpRaw(rec, live string) []Raw {
	doc := func(s string) string {
		return `{"cursor":{"firstBatch":[{"_id":"6af2d505c4cc8b2edc7b2d9a","name":"slow-step","created_at":{"$numberLong":"` + s + `"}}]}}`
	}
	return []Raw{oAnswer("mock-1780", oAt.Sub(rpAnsAt), doc(rec)).ServedAs(1).ServedLive(oAnswer("mock-1780", oAt.Sub(rpAnsAt), doc(live)))}
}

// rpClient is the test case's request, the client's own, holding the Date
// as recorded and as round F's test sent it.
func rpClient() Earlier {
	at := rpRecAt.Add(-100 * time.Millisecond)
	e := Earlier{Mock: givenPlace("post-atg-response-by-id-by-id-1"), At: at, Given: true, Own: true, Seen: []Seen{
		{Side: Request, Leaf: Leaf{Path: "body.headers.Date", Noise: "body.headers.Date", Type: String, Value: rpDateR}, Live: rpDateF}}}
	e.incoming, e.arrivedRec, e.arrivedLive = true, at, rpFAt.Add(-110*time.Millisecond)
	return e
}

// rpDecide decides the insert's Date under keploy mock's client tests (or a
// keploy test case where client is unset), in window w.
func rpDecide(t *testing.T, client bool, w *Window, earlier []Earlier, raw []Raw) Decision {
	t.Helper()
	if client {
		earlier = append([]Earlier{rpClient()}, earlier...)
	}
	in := iCall(rpCall, String, rpDateR, rpDateF, rpRecAt, rpFAt, w, earlier, raw)
	in.Drifts.clientTests = client
	ds := Decide(in)
	if len(ds) != 1 {
		t.Fatalf("%d decisions, want one: %s", len(ds), oDescribe(ds))
	}
	return ds[0]
}

func rpWindow() *Window { return iWin(rpRecAt, rpFAt, 30*time.Millisecond) }

// The read-back of a stamp the application's own clock wrote, which met the
// call's moment in one second in round F alone, is no pick: the call's
// window holds the read, so the Date is the read of the clock it is
// wherever the read-back stands (the fourth insert's decision), under
// client tests and for a keploy test case, the read-back a learned leaf or
// a recording.
func TestOutcomeReadBackOfAClockStampThatMetRoundFIsNoPick(t *testing.T) {
	for _, client := range []bool{true, false} {
		for _, form := range []string{"learned", "recording"} {
			ear, raw := []Earlier{rpSent(rpRecAt.Add(-600*time.Millisecond), rpStampR, rpStampF, true)}, []Raw(nil)
			if form == "learned" {
				ear = append(ear, rpAnswer(rpStampR, rpStampF))
			} else {
				raw = rpRaw(rpStampR, rpStampF)
			}
			d := rpDecide(t, client, rpWindow(), ear, raw)
			if d.Outcome != OutcomeClock || d.Clock == nil || !d.Clock.Plain() {
				t.Errorf("client tests %v, the read-back %s: %s; want a plain read of the clock in the call's window", client, form, oDescribe([]Decision{d}))
			}
		}
	}
}

// What stays a pick: the read-back with no send behind it, a send learned as
// a plain value, a send after the read-back, another test's send, a
// read-back whose text is not the send's in either run, and one no window
// holds.
func TestOutcomeReadBackPickStandsWhereTheSendDoesNotExplainIt(t *testing.T) {
	sentAt := rpRecAt.Add(-600 * time.Millisecond)
	for _, row := range []struct {
		what string
		ear  []Earlier
		w    *Window
	}{
		{"no send", nil, rpWindow()},
		{"a send learned as a plain value", []Earlier{rpSent(sentAt, rpStampR, rpStampF, false)}, rpWindow()},
		{"a send after the read-back", []Earlier{rpSent(rpAnsAt.Add(10*time.Millisecond), rpStampR, rpStampF, true)}, rpWindow()},
		{"a send whose recorded stamp the read-back does not hold", []Earlier{rpSent(sentAt, "1794562680", rpStampF, true)}, rpWindow()},
		{"a send whose round F stamp the read-back does not hold", []Earlier{rpSent(sentAt, rpStampR, "1794562743", true)}, rpWindow()},
		{"a window that does not hold the read", []Earlier{rpSent(sentAt, rpStampR, rpStampF, true)},
			iWin(rpRecAt.Add(-3*time.Second), rpFAt.Add(-3*time.Second), 30*time.Millisecond)},
	} {
		d := rpDecide(t, true, row.w, append(row.ear, rpAnswer(rpStampR, rpStampF)), nil)
		oRefused(t, row.what, d, RefusalPick, "mock-1780 "+rpAnsP)
	}

	// Another test's send: keploy reads a send only within the call's test
	// and the start-up.
	in := iCall(rpCall, String, rpDateR, rpDateF, rpRecAt, rpFAt, rpWindow(),
		[]Earlier{rpSent(sentAt, rpStampR, rpStampF, true), rpAnswer(rpStampR, rpStampF)}, nil)
	in.Scope = func(m string) Scope {
		if m == "mock-1763" {
			return ScopeTest("test-1")
		}
		return ScopeTest("test-2")
	}
	ds := Decide(in)
	if len(ds) != 1 {
		t.Fatalf("another test's send: %d decisions, want one: %s", len(ds), oDescribe(ds))
	}
	oRefused(t, "another test's send", ds[0], RefusalPick, "mock-1780 "+rpAnsP)
}

// An unchanged answer that lists both stamps (a timetable) holds round F's
// moment in both runs: no read-back of a send, still a pick, though it
// holds the send's text as recorded and as round F sent it.
func TestOutcomeReadBackAnUnchangedListStaysAPick(t *testing.T) {
	const list = "[1794562681,1794562744]"
	ans := Earlier{Mock: "mock-1780", At: rpAnsAt, Own: true, Payload: oPayload("mock-1780", rpAnsAt), Seen: []Seen{
		{Side: Response, Leaf: Leaf{Path: "cursor.firstBatch.0.slots", Noise: "cursor.firstBatch.0.slots", Type: String, Value: list}, Live: list}}}
	d := rpDecide(t, true, rpWindow(), []Earlier{rpSent(rpRecAt.Add(-600*time.Millisecond), rpStampR, rpStampF, true), ans}, nil)
	oRefused(t, "an unchanged list of both stamps", d, RefusalPick, "mock-1780 cursor.firstBatch.0.slots")
}

// At the millisecond two reads do not meet in one cut as the runs' timing
// falls: a read-back that holds round F's exact millisecond, and not the
// recorded one, stays a pick, its send a clock read or not.
func TestOutcomeReadBackAtTheMillisecondStaysAPick(t *testing.T) {
	const (
		p      = "body.docs.msgs.0.event.at.$numberLong"
		stampR = "1794562681650" // 09:38:01.650
		stampF = "1794562744400" // 09:39:04.400, round F's call
		callR  = "1794562682250" // 09:38:02.250
	)
	sent := iSent("mock-1763", rpRecAt.Add(-600*time.Millisecond), rpSentP, Number, stampR, stampF)
	sent.Seen[0].Clock, sent.Seen[0].Offset = true, &models.ClockOffset{Res: time.Millisecond}
	in := iCall(p, Number, callR, stampF, rpRecAt, rpFAt, rpWindow(), []Earlier{sent, rpAnswer(stampR, stampF)}, nil)
	ds := Decide(in)
	if len(ds) != 1 {
		t.Fatalf("%d decisions, want one: %s", len(ds), oDescribe(ds))
	}
	oRefused(t, "a millisecond read-back", ds[0], RefusalPick, "mock-1780 "+rpAnsP)
}
