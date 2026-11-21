package learn

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A client test names what it makes by its own clock: api-server's e2e
// client creates an app "[E2E] authz-<UnixNano>". The application then
// writes the app with createdAt, the second it reads from its own clock a
// few milliseconds later, and looks the name up (a distinct on
// app_config_audits by app.name). In the recording the name's first ten
// digits are that second every time, so the client's request holds the
// recorded createdAt by a cut. The values below are a recording's own:
// its test cases post-client-v1-apps-9, -11, -1 and -23 and the inserts and
// distincts they led to.

// nameCase is the request that creates the app "[E2E] <label>-<nanos>", sent
// at at.
func nameCase(label string, nanos int64, at time.Time) *models.HTTPReq {
	return &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/client/v1/apps", Timestamp: at,
		Header: map[string]string{"Content-Type": "application/json", "Host": "localhost:8083"},
		Body:   `{"name":"[E2E] ` + label + `-` + strconv.FormatInt(nanos, 10) + `"}`}
}

// nameRun is one app-making test in the recording and in record's full
// round: the client's UnixNano in each, and how long after the client made
// it the application read its clock for the call (fGap 0: as long as in the
// recording).
type nameRun struct {
	label          string
	rNanos, fNanos int64
	gap, fGap      time.Duration
}

func (n nameRun) times() (rReq, fReq, rCall, fCall time.Time) {
	rReq, fReq = time.Unix(0, n.rNanos).Add(100*time.Microsecond), time.Unix(0, n.fNanos).Add(100*time.Microsecond)
	return rReq, fReq, rReq.Add(n.gap), fReq.Add(cmp.Or(n.fGap, n.gap))
}

// createdAt is the insert's app.createdAt of n: each run's second at the
// call, with the client's request before it (placed false: none, as on a
// keploy with no client request source) and the call's own window w (nil:
// the window from just before the request to the call), and raw, recordings
// handed to the call.
func (n nameRun) createdAt(t *testing.T, placed bool, window func(rReq, fReq, rCall, fCall time.Time) *Window, raw ...Raw) []Decision {
	t.Helper()
	rReq, fReq, rCall, fCall := n.times()
	var earlier []Earlier
	if placed {
		earlier = clientPlaced(t, "post-client-v1-apps-9", nameCase(n.label, n.rNanos, rReq), nameCase(n.label, n.fNanos, fReq), fReq)
	}
	w := &Window{Rec: Span{From: rReq.Add(-time.Millisecond), To: rCall}, F: Span{From: fReq.Add(-time.Millisecond), To: fCall}}
	if window != nil {
		w = window(rReq, fReq, rCall, fCall)
	}
	in := iCall("body.docs.msgs.0.app.createdAt.$numberLong", Number, strconv.FormatInt(rCall.Unix(), 10), strconv.FormatInt(fCall.Unix(), 10), rCall, fCall, w, earlier, raw)
	in.Drifts = oClientDrifts()
	return Decide(in)
}

// TestASecondTheApplicationReadsBesideAClientsUnixNanoNameIsItsClockRead:
// the application's own second is a read of its clock, as it is with no
// client request before it, wherever each run's read fell: in the second of
// its run's name (the request holds it by a cut in both runs), in the next
// one (a name's cut names one run's second by one rule alone), or more than
// a second later (only the recorded name holds it). It is never a value
// bound at the name, which no variable can hold as this text, nor a pick
// from the request.
func TestASecondTheApplicationReadsBesideAClientsUnixNanoNameIsItsClockRead(t *testing.T) {
	for what, n := range map[string]nameRun{
		"round F's read in the second of its name": {label: "authz", rNanos: 1794298992286703522, fNanos: 1794299057100000001, gap: 5 * time.Millisecond},
		"round F's read in the next second":        {label: "oauth-devloop", rNanos: 1794298992481378634, fNanos: 1794299056997000001, gap: 5 * time.Millisecond},
		"the recorded read in the next second":     {label: "oauth-devloop", rNanos: 1794298991997000001, fNanos: 1794299057100000001, gap: 5 * time.Millisecond},
		"round F's read a second after its name":   {label: "oauth-devloop", rNanos: 1794298992481378634, fNanos: 1794299056300000001, gap: 5 * time.Millisecond, fGap: 1200 * time.Millisecond},
	} {
		t.Run(what, func(t *testing.T) {
			if d := n.createdAt(t, false, nil); len(d) != 1 || d[0].Outcome != OutcomeClock {
				t.Fatalf("with no client request before the call: %s; want a read of the clock (the row's premise)", oDescribe(d))
			}
			d := n.createdAt(t, true, nil)
			if len(d) != 1 || d[0].Outcome != OutcomeClock || d[0].Clock == nil || d[0].From.Mock != "" {
				t.Fatalf("createdAt beside the client's name: %s; want a read of the application's clock, bound nowhere", oDescribe(d))
			}
		})
	}
}

// TestASecondNoWindowHoldsBesideAClientsUnixNanoNameIsNoClockRead: the
// exemption is for a read the call's window holds. With no such read the
// value is never learned as a clock read or as the test's own value: where
// both runs' names hold it by a cut it stays bound at the name (which an
// adapter that binds one text refuses), and where only the recorded name
// does it is refused, naming the request.
func TestASecondNoWindowHoldsBesideAClientsUnixNanoNameIsNoClockRead(t *testing.T) {
	// A window that ended before either run's second began holds no read.
	early := func(rReq, fReq, _, _ time.Time) *Window {
		return &Window{Rec: Span{From: rReq.Add(-time.Hour), To: rReq.Add(-time.Minute)}, F: Span{From: fReq.Add(-time.Hour), To: fReq.Add(-time.Minute)}}
	}
	const where = "test case post-client-v1-apps-9"
	both := nameRun{label: "authz", rNanos: 1794298992286703522, fNanos: 1794299057100000001, gap: 5 * time.Millisecond}
	if d := both.createdAt(t, true, early); len(d) != 1 || d[0].Outcome != OutcomeValue || d[0].From.Mock != where || !d[0].From.Client || d[0].Clock != nil {
		t.Fatalf("both names hold the second and no window holds a read: %s; want it bound at %s, no clock", oDescribe(d), where)
	}
	one := nameRun{label: "oauth-devloop", rNanos: 1794298992481378634, fNanos: 1794299056300000001, gap: 5 * time.Millisecond, fGap: 1200 * time.Millisecond}
	d := one.createdAt(t, true, early)
	if len(d) != 1 || d[0].Outcome != OutcomeRefused || d[0].Refusal == nil || d[0].Refusal.Kind != RefusalNoClockRead ||
		!slices.ContainsFunc(d[0].Refusal.Places, func(p string) bool { return strings.HasPrefix(p, where) }) {
		t.Fatalf("only the recorded name holds the second and no window holds a read: %s; want refused as no read of the clock, naming %s", oDescribe(d), where)
	}
}

// TestAClientsUnixNanoNameThatEndsInZeroIsBoundAtItsRequest: one UnixNano in
// ten ends in 0, and reads as an instant of a coarser unit than the other
// run's. The name's word is still the request's word in both runs: a Value
// bound at the client's request, as any other name is.
func TestAClientsUnixNanoNameThatEndsInZeroIsBoundAtItsRequest(t *testing.T) {
	for what, n := range map[string]nameRun{
		"the recorded name ends in 0":  {label: "app-crud", rNanos: 1794298991945296440, fNanos: 1794299056377735377},
		"the recorded name ends in 00": {label: "assert-types", rNanos: 1794299090578927200, fNanos: 1794299153464262809},
		"round F's name ends in 0":     {label: "app-crud", rNanos: 1794298991945296441, fNanos: 1794299056377735370},
		"both end in 0":                {label: "app-crud", rNanos: 1794298991945296440, fNanos: 1794299056377735370},
	} {
		t.Run(what, func(t *testing.T) {
			n.gap = 2 * time.Millisecond
			rReq, fReq, rCall, fCall := n.times()
			name := func(v int64) string { return "[E2E] " + n.label + "-" + strconv.FormatInt(v, 10) }
			in := iCall("body.cmd.query.app.name", String, name(n.rNanos), name(n.fNanos), rCall, fCall, nil,
				clientPlaced(t, "post-client-v1-apps-1", nameCase(n.label, n.rNanos, rReq), nameCase(n.label, n.fNanos, fReq), fReq), nil)
			in.Drifts = oClientDrifts()
			d := Decide(in)
			if len(d) != 1 || d[0].Outcome != OutcomeValue || d[0].From.Mock != "test case post-client-v1-apps-1" || d[0].From.Path != "body.name" || !d[0].From.Client || d[0].Clock != nil {
				t.Fatalf("the name %q -> %q the client sent: %s; want a Value of test case post-client-v1-apps-1's body.name, a client test's", name(n.rNanos), name(n.fNanos), oDescribe(d))
			}
		})
	}
}

// tokenCase is api-server's e2e client's GraphQL request with the session
// token it signed in for at second signedIn (its iat and auth_time claims),
// sent at at; date, where set, is a Date header inside a recorded exchange
// the body uploads.
func tokenCase(signedIn int64, at time.Time, date string) *models.HTTPReq {
	body := `{"query":"mutation { createApp }"}`
	if date != "" {
		body = `{"query":"mutation { createApp }","headers":{"Date":"` + date + `"}}`
	}
	return &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/query", Timestamp: at,
		Header: map[string]string{"Content-Type": "application/json", "Host": "localhost:8083", "Authorization": "Bearer " + signInJWT(signedIn)},
		Body:   body}
}

// tokenRun is one test of the recording and of record's full round whose
// client signs in, then sends the request (rReq, fReq) that leads to the
// app_config_audits insert a few milliseconds later, whose app.createdAt
// is the second the application reads from its own clock. The recording's
// values are its own: test case post-query-12, sent at
// 2026-11-10T10:01:31.738Z with a token signed in at 1794304891, the second
// the insert's createdAt then holds.
type tokenRun struct {
	rSigned, fSigned int64
	rReq, fReq       time.Time
	rDate, fDate     string
}

const tokenTest, tokenPath = "post-query-12", "body.docs.msgs.0.app.createdAt.$numberLong"

func (n tokenRun) calls() (rCall, fCall time.Time) {
	return n.rReq.Add(7 * time.Millisecond), n.fReq.Add(7 * time.Millisecond)
}

// createdAt decides the insert's app.createdAt of n with the client's
// request before it (placed false: none), in window (nil: from just before
// the request to the call, which holds each run's read).
func (n tokenRun) createdAt(t *testing.T, placed bool, window *Window) []Decision {
	t.Helper()
	rCall, fCall := n.calls()
	var earlier []Earlier
	if placed {
		earlier = clientPlaced(t, tokenTest, tokenCase(n.rSigned, n.rReq, n.rDate), tokenCase(n.fSigned, n.fReq, n.fDate), n.fReq)
	}
	if window == nil {
		window = &Window{Rec: Span{From: n.rReq.Add(-time.Millisecond), To: rCall}, F: Span{From: n.fReq.Add(-time.Millisecond), To: fCall}}
	}
	in := iCall(tokenPath, Number, strconv.FormatInt(rCall.Unix(), 10), strconv.FormatInt(fCall.Unix(), 10), rCall, fCall, window, earlier, nil)
	in.Drifts = oClientDrifts()
	return Decide(in)
}

// tokenRuns are the ways one run's second stands whole in the client's
// request while the other run's does not: the client signed in during the
// second of the application's read in one run, and a second or more before
// it in the other; or an exchange the request uploads was answered then.
func tokenRuns() map[string]tokenRun {
	rReq := time.Date(2026, 11, 10, 10, 1, 31, 738_298_271, time.UTC)
	fReq := time.Date(2026, 11, 10, 10, 2, 34, 12_000_000, time.UTC)
	httpDate := func(at time.Time) string { return at.UTC().Format(http.TimeFormat) }
	return map[string]tokenRun{
		"the recorded token was signed in that second, round F's the second before": {rSigned: 1794304891, fSigned: fReq.Unix() - 1, rReq: rReq, fReq: fReq},
		"round F's token was signed in that second, the recorded one two before":    {rSigned: 1794304889, fSigned: fReq.Unix(), rReq: rReq, fReq: fReq},
		"round F's uploaded Date header holds that second, the recorded one another": {rSigned: 1794304880, fSigned: fReq.Unix() - 9, rReq: rReq, fReq: fReq,
			rDate: httpDate(rReq.Add(-3 * time.Second)), fDate: httpDate(fReq)},
		"the recorded uploaded Date header holds that second, round F's another": {rSigned: 1794304880, fSigned: fReq.Unix() - 9, rReq: rReq, fReq: fReq,
			rDate: httpDate(rReq), fDate: httpDate(fReq.Add(-3 * time.Second))},
	}
}

// TestASecondTheApplicationReadsInTheSecondAClientsTokenWasSignedIsItsClockRead:
// a client signs in and sends its request within a second, so the token's
// issued-at second is the second the application then reads from its own
// clock, in one run and not in the other, or in both where the client sent
// one token in both runs. The read the call's window holds explains the
// value, and the second the request holds is no source beside it, so no pick
// comes from it: the value is that read, as it is with no client request
// before it, and replay compares it in its own window.
func TestASecondTheApplicationReadsInTheSecondAClientsTokenWasSignedIsItsClockRead(t *testing.T) {
	runs := tokenRuns()
	// The client sent one token in both runs, signed in at the recorded
	// second: the read the call's window holds goes before it too.
	same := runs["the recorded token was signed in that second, round F's the second before"]
	same.fSigned = same.rSigned
	runs["a token the client sent unchanged in both runs"] = same
	for what, n := range runs {
		t.Run(what, func(t *testing.T) {
			if d := n.createdAt(t, false, nil); len(d) != 1 || d[0].Outcome != OutcomeClock {
				t.Fatalf("with no client request before the call: %s; want a read of the clock (the row's premise)", oDescribe(d))
			}
			d := n.createdAt(t, true, nil)
			if len(d) != 1 || d[0].Outcome != OutcomeClock || d[0].Clock == nil || d[0].From.Mock != "" {
				t.Fatalf("createdAt beside the client's request: %s; want a read of the application's clock, bound nowhere", oDescribe(d))
			}
		})
	}
}

// TestASecondAClientsRequestHoldsWholeWithNoReadIsStillAPickFromIt: the
// precedence is for a read the call's window holds. With no such read, the
// second one run's request holds whole stays a pick from the request,
// refused, as does one in a leaf the client sent unchanged in both runs (one
// input, two values). So does the UnixNano name in a keploy test's own
// request, which keploy sent as recorded, read as main reads it.
func TestASecondAClientsRequestHoldsWholeWithNoReadIsStillAPickFromIt(t *testing.T) {
	pick := func(t *testing.T, what string, d []Decision) {
		t.Helper()
		if len(d) != 1 || d[0].Outcome != OutcomeRefused || d[0].Refusal == nil || d[0].Refusal.Kind != RefusalPick ||
			!slices.ContainsFunc(d[0].Refusal.Places, func(p string) bool { return strings.HasPrefix(p, "test case "+tokenTest) }) {
			t.Errorf("%s: %s; want refused, a pick from test case %s", what, oDescribe(d), tokenTest)
		}
	}
	for what, n := range tokenRuns() {
		t.Run(what, func(t *testing.T) {
			// A window that ended before either run's second began holds no read.
			early := &Window{Rec: Span{From: n.rReq.Add(-time.Hour), To: n.rReq.Add(-time.Minute)}, F: Span{From: n.fReq.Add(-time.Hour), To: n.fReq.Add(-time.Minute)}}
			pick(t, "no window holds the read", n.createdAt(t, true, early))
		})
	}

	// The client sent one token in both runs, signed in at the recorded
	// second, and no window holds a read: the same input, round F's value
	// another, a pick.
	same := tokenRuns()["the recorded token was signed in that second, round F's the second before"]
	same.fSigned = same.rSigned
	pick(t, "a token the client sent unchanged in both runs, no window holds the read",
		same.createdAt(t, true, &Window{Rec: Span{From: same.rReq.Add(-time.Hour), To: same.rReq.Add(-time.Minute)}, F: Span{From: same.fReq.Add(-time.Hour), To: same.fReq.Add(-time.Minute)}}))

	n := nameRun{label: "authz", rNanos: 1794298992286703522, fNanos: 1794299056997000001, gap: 5 * time.Millisecond}
	rReq, fReq, rCall, fCall := n.times()
	const p = "body.docs.msgs.0.app.createdAt.$numberLong"
	w := &Window{Rec: Span{From: rReq.Add(-time.Millisecond), To: rCall}, F: Span{From: fReq.Add(-time.Millisecond), To: fCall}}
	rec, live := strconv.FormatInt(rCall.Unix(), 10), strconv.FormatInt(fCall.Unix(), 10)
	body := nameCase(n.label, n.rNanos, rReq).Body
	own := GivenRequest(&models.TestRequest{Test: "post-client-v1-apps-9", Parts: []models.TestRequestPart{jsonType, {Name: "body", Recorded: body, Sent: body}}})
	d := Decide(iCall(p, Number, rec, live, rCall, fCall, w, []Earlier{own}, nil))
	if len(d) != 1 || d[0].Outcome != OutcomeRefused || d[0].Refusal == nil || d[0].Refusal.Kind != RefusalPick {
		t.Errorf("the UnixNano name in a keploy test's own request, sent as recorded: %s; want refused, a pick from the request", oDescribe(d))
	}
}

// TestASecondBesideAClientsNameAndAnotherTestsAnswerIsItsClockRead: a client
// test's UnixNano name holds round F's second by a cut, and an answer of an
// earlier test (a Vault lookup whose Date header is that second) holds the
// recorded one. On their own, neither is a source and the value is the read
// of the clock the call's window holds. Together they read as a copy of
// what the earlier test gave the application, a source outside the scope;
// the read the window holds goes before the client's name, so the value is
// that read, as main reads it. With no read the window holds it is no Clock.
func TestASecondBesideAClientsNameAndAnotherTestsAnswerIsItsClockRead(t *testing.T) {
	n := nameRun{label: "list-apps", rNanos: 1794298992286703522, fNanos: 1794299057100000001, gap: 5 * time.Millisecond}
	rReq, _, _, _ := n.times()
	at := rReq.Add(-100 * time.Millisecond)
	vault := oAnswer("mock-213", oAt.Sub(at), at.UTC().Format(http.TimeFormat)).ServedAs(1).ServedAsRecorded().OfAnotherTest()
	if d := n.createdAt(t, false, nil, vault); len(d) != 1 || d[0].Outcome != OutcomeClock {
		t.Fatalf("the earlier test's answer alone: %s; want a read of the clock (the row's premise)", oDescribe(d))
	}
	if d := n.createdAt(t, true, nil); len(d) != 1 || d[0].Outcome != OutcomeClock {
		t.Fatalf("the client's name alone: %s; want a read of the clock (the row's premise)", oDescribe(d))
	}
	if d := n.createdAt(t, true, nil, vault); len(d) != 1 || d[0].Outcome != OutcomeClock || d[0].Clock == nil || d[0].From.Mock != "" {
		t.Fatalf("createdAt beside the client's name and the earlier test's answer: %s; want a read of the application's clock, bound nowhere", oDescribe(d))
	}
	early := func(rReq, fReq, _, _ time.Time) *Window {
		return &Window{Rec: Span{From: rReq.Add(-time.Hour), To: rReq.Add(-time.Minute)}, F: Span{From: fReq.Add(-time.Hour), To: fReq.Add(-time.Minute)}}
	}
	if d := n.createdAt(t, true, early, vault); len(d) != 1 || d[0].Outcome == OutcomeClock || d[0].Clock != nil {
		t.Errorf("no window holds the read: %s; want no read of the clock", oDescribe(d))
	}
}
