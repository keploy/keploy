package learn

import (
	"cmp"
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
// the window from just before the request to the call).
func (n nameRun) createdAt(t *testing.T, placed bool, window func(rReq, fReq, rCall, fCall time.Time) *Window) []Decision {
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
	in := iCall("body.docs.msgs.0.app.createdAt.$numberLong", Number, strconv.FormatInt(rCall.Unix(), 10), strconv.FormatInt(fCall.Unix(), 10), rCall, fCall, w, earlier, nil)
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

// TestASecondAClientsRequestHoldsWholeIsStillAPickFromIt: the exemptions are
// for a finer instant and for a leaf that moved with the value. A second the
// client's request holds whole, at its own cut, where round F's request
// holds another that is not the call's, stays a pick from the request,
// refused. So does the UnixNano name in a keploy test's own request, which
// keploy sent as recorded: that reading is main's and is not changed.
func TestASecondAClientsRequestHoldsWholeIsStillAPickFromIt(t *testing.T) {
	n := nameRun{label: "authz", rNanos: 1794298992286703522, fNanos: 1794299056997000001, gap: 5 * time.Millisecond}
	rReq, fReq, rCall, fCall := n.times()
	const p = "body.docs.msgs.0.app.createdAt.$numberLong"
	w := &Window{Rec: Span{From: rReq.Add(-time.Millisecond), To: rCall}, F: Span{From: fReq.Add(-time.Millisecond), To: fCall}}
	rec, live := strconv.FormatInt(rCall.Unix(), 10), strconv.FormatInt(fCall.Unix(), 10)
	pick := func(what string, d []Decision) {
		t.Helper()
		if len(d) != 1 || d[0].Outcome != OutcomeRefused || d[0].Refusal == nil || d[0].Refusal.Kind != RefusalPick {
			t.Errorf("%s: %s; want refused, a pick from the request", what, oDescribe(d))
		}
	}

	slot := func(nanos int64, at time.Time, second int64) *models.HTTPReq {
		r := nameCase(n.label, nanos, at)
		r.Body = `{"slot":` + strconv.FormatInt(second, 10) + `}`
		return r
	}
	in := iCall(p, Number, rec, live, rCall, fCall, w,
		clientPlaced(t, "post-client-v1-apps-9", slot(n.rNanos, rReq, rCall.Unix()), slot(n.fNanos, fReq, fCall.Unix()-7), fReq), nil)
	in.Drifts = oClientDrifts()
	pick("the recorded second whole in the client's request, another in round F's", Decide(in))

	body := nameCase(n.label, n.rNanos, rReq).Body
	own := GivenRequest(&models.TestRequest{Test: "post-client-v1-apps-9", Parts: []models.TestRequestPart{jsonType, {Name: "body", Recorded: body, Sent: body}}})
	pick("the UnixNano name in a keploy test's own request, sent as recorded", Decide(iCall(p, Number, rec, live, rCall, fCall, w, []Earlier{own}, nil)))
}
