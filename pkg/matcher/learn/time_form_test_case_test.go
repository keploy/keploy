package learn

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// tTokenRequest is a test case's request that carries a bearer token whose
// iat claim is recIat as recorded and liveIat as the test sent it in round
// F, beside a claim that is no time (uid).
func tTokenRequest(recIat, liveIat time.Time, uid string) Earlier {
	token := func(iat time.Time) string {
		s := strconv.FormatInt(iat.Unix(), 10)
		e := strconv.FormatInt(iat.Add(time.Hour).Unix(), 10)
		return "Bearer " + signedToken(`{"sub":"68d4148edcd121253694486c","uid":`+uid+`,"iat":`+s+`,"exp":`+e+`}`, "sig")
	}
	return GivenRequest(&models.TestRequest{Test: "post-query-29", Parts: []models.TestRequestPart{
		{Name: "url", Recorded: "http://localhost:8080/api/query", Sent: "http://localhost:8080/api/query"},
		{Name: "header.Authorization", Recorded: token(recIat), Sent: token(liveIat)},
	}})
}

// A time an application writes (an event's created_at, in epoch seconds,
// as a MongoDB $numberLong's text) in the same second its test minted the
// token it sent: when recorded the value is the token's iat, and in round F
// the token was minted a second before the write. keploy cannot tell a read
// of the clock from a pick by it among the request's times; both are the
// application's clock, so under the time default (keploy mock) the value is
// a time checked by its form alone, and strict times (keploy test) refuse it
// as before. A time at a key (an _id) names what the call writes, and a
// value whose moment lies far from its call in both runs is no time: both
// keep the refusal under the default too.
func TestTheTimeDefaultKeepsATimeTheTestCasesTokenHeld(t *testing.T) {
	ms := time.Millisecond
	rAt := time.Date(2026, 11, 18, 8, 55, 44, 520*1e6, time.UTC)
	fAt := rAt.Add(FullRoundLead + 37*time.Second)
	rRead, fRead := rAt.Add(-300*ms), fAt.Add(-300*ms)
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	w := iWin(rAt, fAt, 900*ms)
	call := func(path string, rec, live string, uid string) Input {
		in := costCall(clockedDrifts(), path, rec, live, rAt, []Earlier{tTokenRequest(rRead, fRead.Add(-time.Second), uid)}, nil, nil, w)
		in.Arrived = fAt
		return in
	}
	const p = "body.docs.msgs.0.eventDetails.job.created_at.$numberLong"
	in := call(p, secs(rRead), secs(fRead), "7")
	ds := Decide(in)
	if len(ds) != 1 || ds[0].Refusal == nil || ds[0].Refusal.Kind != RefusalPick || ds[0].Refusal.Check != "instant-pick-test-case" {
		t.Fatalf("strict times: %s; want refused as a pick from the test case's request", oDescribe(ds))
	}
	if !strings.Contains(ds[0].Line, "test case post-query-29 at header.Authorization") {
		t.Errorf("strict times: %q; want the line to name the token's place", ds[0].Line)
	}
	form := TimeForm(Leaf{Type: String, Value: secs(rRead)})
	if form == "" {
		t.Fatalf("an epoch second's text has no form")
	}
	tStrictThenDefault(t, "created_at in the token's second", in, RefusalPick, true, form)
	in.TimeForm = true
	if ds := Decide(in); len(ds) == 1 && ds[0].Clock != nil {
		o := *ds[0].Clock
		if ok, why := CheckClockReadForm(p, o, Leaf{Type: String, Value: secs(fAt.Add(time.Hour))}, Span{}, false, true); !ok {
			t.Errorf("another epoch second at replay: %s; want it passed", why)
		}
		for _, bad := range []string{strconv.FormatInt(fAt.UnixMilli(), 10), "2026-11-18T08:55:44Z", "soon"} {
			if ok, _ := CheckClockReadForm(p, o, Leaf{Type: String, Value: bad}, Span{}, false, true); ok {
				t.Errorf("FALSE GREEN: %q at replay passed a check by the epoch second's form; want refused", bad)
			}
		}
	}

	// At a key: an id the call writes, never checked by its form alone.
	tStrictThenDefault(t, "the token's second at an _id", call("body.docs.msgs.0._id.$numberLong", secs(rRead), secs(fRead), "7"), RefusalPick, false, "")

	// No time: the value and the token's uid claim name a moment two years
	// before the call in both runs, which no form of a time near its call
	// holds.
	old, oldF := rRead.Add(-2*366*24*time.Hour), fRead.Add(-2*366*24*time.Hour)
	far := call(p, secs(old), secs(oldF), secs(old))
	far.Earlier = []Earlier{tTokenRequest(rRead, fRead.Add(-time.Second), secs(old))}
	for _, timeForm := range []bool{false, true} {
		far.TimeForm = timeForm
		ds := Decide(far)
		if len(ds) != 1 || ds[0].Outcome == OutcomeClock || ds[0].Refusal == nil {
			t.Errorf("FALSE GREEN? a value far from its call that the token's uid claim holds (time default %v): %s; want refused", timeForm, oDescribe(ds))
		}
	}
}

// A pick from the test case's request at a word of a text that changed
// around it is checked by its form at its word, as any read the time
// default settles there (amongBinds), where every other changed word is put
// back; strict times keep it refused with no check by form. A pick of
// another check (an answer's) stays refused at the word under both.
func TestATestCasePickAmongBoundWordsSettlesAsTheTimeDefault(t *testing.T) {
	rAt, fAt := oAt, oAt.Add(73*time.Minute)
	r, f := rAt.Add(-time.Second), fAt.Add(-time.Second)
	rs, fs := strconv.FormatInt(r.Unix(), 10), strconv.FormatInt(f.Unix(), 10)
	rec := "sent " + rs + " by job 7"
	word := models.ClockWordOf(rec, len("sent "), len("sent "+rs))
	rm := moment{key: instantKey{at: r.UnixNano()}, res: time.Second, zoned: true}
	lm := moment{key: instantKey{at: f.UnixNano()}, res: time.Second, zoned: true}
	for _, c := range []struct {
		what              string
		check             string
		timeForm, settled bool
	}{
		{"a pick from the test case's request", checkPickTestCase, true, true},
		{"the same under strict times", checkPickTestCase, false, false},
		{"a pick from an answer", "instant-pick", true, false},
	} {
		in := oInput("body.note", rec, "sent "+fs+" by job 8", nil, nil)
		in.Window = iWin(rAt, fAt, 5*time.Second)
		in.Caps, in.TimeForm = amongCaps, c.timeForm
		dc := &decider{in: in}
		ci := changedInstant{r: rs, l: fs, rm: &rm, lm: &lm, text: true, noWord: noWordAround, around: word}
		d := dc.refuse("body.note", variables.Address{}, 1, rs, fs, RefusalPick, c.check, "as learned", nil)
		got := dc.timeDefault("body.note", variables.Address{}, 1, ci, d, false)
		if got.Refusal == nil || got.Refusal.Kind != RefusalPick || got.Clock != nil {
			t.Errorf("%s: the decision %s; want the refusal kept, its check by form held apart (around)", c.what, oDescribe([]Decision{got}))
		}
		switch {
		case c.settled && (dc.around == nil || dc.around.Clock == nil || dc.around.Clock.Form == "" || dc.around.Clock.Word != word):
			t.Errorf("%s: around %+v; want a Clock checked by its form at its word", c.what, dc.around)
		case !c.settled && dc.around != nil:
			t.Errorf("FALSE GREEN? %s: around %s; want none (it stays refused)", c.what, oDescribe([]Decision{*dc.around}))
		}
	}
}
