package learn

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// e2eQuery is a GraphQL request api-server's e2e client sends to /query, op
// its operation and vars its variables (JSON), sent at at.
func e2eQuery(op, vars string, at time.Time) *models.HTTPReq {
	return &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/query", Timestamp: at,
		Header: map[string]string{"Content-Type": "application/json", "Host": "localhost:8083"},
		Body:   `{"query":"mutation ` + op + ` { ok }","variables":` + vars + `}`}
}

// clientSent is the earlier places of a client test's call: its recording's
// requests rec, named names, each paired in record's full round with the
// request of live in order, which reached the application at its Timestamp.
func clientSent(t *testing.T, names []string, rec, live []*models.HTTPReq) []Earlier {
	t.Helper()
	tr := NewTracker()
	tr.Begin()
	cases := make([]ClientCase, len(rec))
	for i, r := range rec {
		cases[i] = ClientCase{Name: names[i], Req: r}
	}
	tr.SetCases(cases)
	for _, l := range live {
		if !tr.Incoming(l, "", l.Timestamp) {
			t.Fatalf("the run's %s %s paired with none of the test's recorded requests", l.Method, l.URL)
		}
	}
	return tr.Placed().Earlier
}

// sentAgainRun is a client test's two runs, made of nanos read from its
// clock in each: requests sent 40 ms apart, and the call 40 ms after them.
type sentAgainRun struct {
	rNanos, fNanos     int64
	rReq, fReq         time.Time
	rCall, fCall       time.Time
	rSecond, fSecond   int64
	rMillis, fMillis   int64
	rSigned, fSigned   time.Time
	sendGap, callAfter time.Duration
}

func newSentAgainRun() sentAgainRun {
	r := sentAgainRun{rNanos: 1794313743296254249, sendGap: 40 * time.Millisecond}
	gap := 69*time.Second + 3*time.Millisecond
	r.rReq = time.Unix(0, r.rNanos).Add(131 * time.Microsecond)
	r.fNanos, r.fReq = r.rNanos+gap.Nanoseconds(), r.rReq.Add(gap)
	r.callAfter = 2 * r.sendGap
	r.rCall, r.fCall = r.rReq.Add(r.callAfter), r.fReq.Add(r.callAfter)
	r.rSigned, r.fSigned = time.Unix(0, r.rNanos).Truncate(time.Second), time.Unix(0, r.fNanos).Truncate(time.Second)
	r.rSecond, r.fSecond = r.rSigned.Unix(), r.fSigned.Unix()
	r.rMillis, r.fMillis = r.rNanos/1e6, r.fNanos/1e6
	return r
}

// nanos is n as the e2e client writes it in a name.
func nanos(n int64) string { return strconv.FormatInt(n, 10) }

// clientValue decides the call at path, recorded rec and live in record's full
// round, after a client test's requests (earlier).
func (r sentAgainRun) clientValue(path string, typ Type, rec, live string, earlier []Earlier) []Decision {
	in := iCall(path, typ, rec, live, r.rCall, r.fCall, nil, earlier, nil)
	in.Drifts = oClientDrifts()
	return Decide(in)
}

// TestASuiteRenamedIsBoundAtTheRequestThatSentItsWholeName: a client test
// creates a suite named "e2e-vh-<nanos>" (InsertTestSuite) and renames it
// "e2e-vh-<nanos>-renamed" (UpdateTestSuite); the application writes the new
// name whole (the testSuiteEvents insert's delta.name). The second request's
// name is the field's whole text in both runs: the value is bound there. The
// first holds only the nanos inside another text, and does not compete, and
// the line says so. Two different texts that each hold the nanos, neither the
// field's whole, stay two sources.
func TestASuiteRenamedIsBoundAtTheRequestThatSentItsWholeName(t *testing.T) {
	r := newSentAgainRun()
	name := func(n int64) string { return "e2e-vh-" + nanos(n) }
	input := func(n int64) string { return `{"testSuiteInput":{"name":"` + name(n) + `"}}` }
	renamed := func(n int64) string {
		return `{"testSuiteInput":{"id":"4fd24164-a1aa-40dd-b8f2-29b0c6c2855c","name":"` + name(n) + `-renamed"}}`
	}
	earlier := clientSent(t, []string{"post-query-36", "post-query-38"},
		[]*models.HTTPReq{e2eQuery("InsertTestSuite", input(r.rNanos), r.rReq), e2eQuery("UpdateTestSuite", renamed(r.rNanos), r.rReq.Add(r.sendGap))},
		[]*models.HTTPReq{e2eQuery("InsertTestSuite", input(r.fNanos), r.fReq), e2eQuery("UpdateTestSuite", renamed(r.fNanos), r.fReq.Add(r.sendGap))})
	const p = "body.docs.msgs.0.delta.name"
	d := r.clientValue(p, String, name(r.rNanos)+"-renamed", name(r.fNanos)+"-renamed", earlier)
	if len(d) != 1 || d[0].Outcome != OutcomeValue || d[0].From.Mock != "test case post-query-38" || d[0].From.Path != "body.variables.testSuiteInput.name" || !d[0].From.Client || d[0].Clock != nil {
		t.Fatalf("the renamed suite's name: %s; want a Value of test case post-query-38's body.variables.testSuiteInput.name, a client test's, no clock", oDescribe(d))
	}
	if want := "; test case post-query-36 body.variables.testSuiteInput.name held only its word"; !strings.Contains(d[0].Line, want) {
		t.Errorf("the line %q does not say %q", d[0].Line, want)
	}

	// A name that is neither request's whole text: two sources still.
	d = r.clientValue(p, String, name(r.rNanos)+"-copy", name(r.fNanos)+"-copy", earlier)
	if len(d) != 1 || d[0].Refusal == nil || d[0].Refusal.Kind != RefusalTwoSources {
		t.Fatalf("a name each request holds only a word of, in two different texts: %s; want refused, two sources", oDescribe(d))
	}
}

// TestAValueTheClientSentAgainIsBoundAtItsFirstSend: a client test sends the
// one value it made in two requests, byte for byte the same text in each run:
// a suite name it created and then filtered by (InsertTestSuite, then
// getAPITestSuitesMetaCursorPaginated), and the session cookie it signed in
// with on two requests of a device login. It is one value sent again, never
// two reads: the variable is bound at the request that sent it first, and the
// line names where it was sent again. The same holds of a stamp of a
// millisecond. A bare stamp of a second sent twice may be two reads of the
// clock within one second, and stays two sources.
func TestAValueTheClientSentAgainIsBoundAtItsFirstSend(t *testing.T) {
	r := newSentAgainRun()
	name := func(n int64) string { return "e2e-del-" + nanos(n) }
	created := func(n int64) string {
		return `{"appID":"bda32527-92b2-4eeb-8ae8-f5a16805eace","testSuiteInput":{"description":"version-history e2e","name":"` + name(n) + `"}}`
	}
	filtered := func(n int64) string {
		return `{"appId":"bda32527-92b2-4eeb-8ae8-f5a16805eace","filters":{"deletedOnly":true,"name":"` + name(n) + `"},"pageSize":50}`
	}
	suite := clientSent(t, []string{"post-query-45", "post-query-49"},
		[]*models.HTTPReq{e2eQuery("InsertTestSuite", created(r.rNanos), r.rReq), e2eQuery("GetAPITestSuitesMetaCursorPaginated", filtered(r.rNanos), r.rReq.Add(r.sendGap))},
		[]*models.HTTPReq{e2eQuery("InsertTestSuite", created(r.fNanos), r.fReq), e2eQuery("GetAPITestSuitesMetaCursorPaginated", filtered(r.fNanos), r.fReq.Add(r.sendGap))})
	const again = "; sent again: test case post-query-49 body.variables.filters.name, first sent at test case post-query-45 body.variables.testSuiteInput.name"
	for _, form := range []string{"%s", "^%s$"} {
		rec, live := strings.Replace(form, "%s", name(r.rNanos), 1), strings.Replace(form, "%s", name(r.fNanos), 1)
		d := r.clientValue("body.cmd.pipeline.6.$match.name.$regex", String, rec, live, suite)
		if len(d) != 1 || d[0].Outcome != OutcomeValue || d[0].From.Mock != "test case post-query-45" || d[0].From.Path != "body.variables.testSuiteInput.name" || !d[0].From.Client || d[0].Clock != nil {
			t.Fatalf("the suite name %q, sent twice: %s; want a Value of test case post-query-45's body.variables.testSuiteInput.name, the first send, no clock", rec, oDescribe(d))
		}
		if !strings.Contains(d[0].Line, again) {
			t.Errorf("the line %q does not say %q", d[0].Line, again)
		}
	}

	// The session cookie the client signed in with, sent on two requests of
	// its device login: the login code's authTime is its auth_time.
	cookied := func(authTime int64, req *models.HTTPReq) *models.HTTPReq {
		req.Header = map[string]string{"Cookie": "jwt_token_staging=" + signInJWT(authTime), "Host": "localhost:8083"}
		return req
	}
	device := func(authTime int64, at time.Time) []*models.HTTPReq {
		return []*models.HTTPReq{
			cookied(authTime, &models.HTTPReq{Method: "GET", URL: "http://localhost:8083/oauth/device?user_code=WDJB-MJHT", Timestamp: at}),
			cookied(authTime, &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/oauth/device/decision", Timestamp: at.Add(r.sendGap), Body: "user_code=WDJB-MJHT&decision=approve"}),
		}
	}
	cookie := clientSent(t, []string{"get-oauth-device-1", "post-oauth-device-decision-1"}, device(r.rSecond, r.rReq), device(r.fSecond, r.fReq))
	d := Decide(authTimeCall(r.rSecond, r.fSecond, r.rCall, r.fCall, cookie))
	if len(d) != 1 || d[0].Outcome != OutcomeValue || d[0].From.Mock != "test case get-oauth-device-1" || d[0].From.Path != "header.Cookie" || !d[0].From.Client || d[0].Clock != nil {
		t.Fatalf("authTime %d -> %d, the auth_time of the cookie sent twice: %s; want a Value of test case get-oauth-device-1's header.Cookie, the first send, no clock", r.rSecond, r.fSecond, oDescribe(d))
	}
	if want := "; sent again: test case post-oauth-device-decision-1 header.Cookie, first sent at test case get-oauth-device-1 header.Cookie"; !strings.Contains(d[0].Line, want) {
		t.Errorf("the line %q does not say %q", d[0].Line, want)
	}

	// A bare stamp, the header's whole text, sent on two requests.
	stamped := func(stamp string, at time.Time) []*models.HTTPReq {
		return []*models.HTTPReq{
			{Method: "POST", URL: "http://localhost:8083/auth/temp-code", Timestamp: at, Header: map[string]string{"X-Signed-At": stamp, "Host": "localhost:8083"}},
			{Method: "POST", URL: "http://localhost:8083/auth/temp-code/confirm", Timestamp: at.Add(r.sendGap), Header: map[string]string{"X-Signed-At": stamp, "Host": "localhost:8083"}},
		}
	}
	stamps := func(rec, live int64) []Earlier {
		return clientSent(t, []string{"post-auth-temp-code-1", "post-auth-temp-code-2"}, stamped(nanos(rec), r.rReq), stamped(nanos(live), r.fReq))
	}
	d = Decide(authTimeCall(r.rSecond, r.fSecond, r.rCall, r.fCall, stamps(r.rSecond, r.fSecond)))
	if len(d) != 1 || d[0].Refusal == nil || d[0].Refusal.Kind != RefusalTwoSources {
		t.Fatalf("authTime %d -> %d, a bare stamp of a second sent twice: %s; want refused, two sources", r.rSecond, r.fSecond, oDescribe(d))
	}
	d = Decide(authTimeCall(r.rMillis, r.fMillis, r.rCall, r.fCall, stamps(r.rMillis, r.fMillis)))
	if len(d) != 1 || d[0].Outcome != OutcomeValue || d[0].From.Mock != "test case post-auth-temp-code-1" || d[0].From.Path != "header.X-Signed-At" {
		t.Fatalf("authTime %d -> %d, a stamp of a millisecond sent twice: %s; want a Value of test case post-auth-temp-code-1's header.X-Signed-At, the first send", r.rMillis, r.fMillis, oDescribe(d))
	}
}

// TestASlotIsTheValuesSourceOnlyByItsTextInBothRuns: the rules that pick one
// of a client test's requests as a value's source read each request's text
// in both runs. A request whose whole text is the field's in one run only is
// not its source; requests that hold one text in one run only were not sent
// one value again; a stamp of a second, bare in one run, may be two reads in
// one second. Each stays two sources. A stamp of a second inside a longer
// text sent again is one value.
func TestASlotIsTheValuesSourceOnlyByItsTextInBothRuns(t *testing.T) {
	r := newSentAgainRun()
	name := func(n int64) string { return "e2e-vh-" + nanos(n) }
	suite := func(rec, live [2]string, field [2]string) []Decision {
		vars := func(s string) string { return `{"testSuiteInput":{"name":"` + s + `"}}` }
		earlier := clientSent(t, []string{"post-query-36", "post-query-38"},
			[]*models.HTTPReq{e2eQuery("InsertTestSuite", vars(rec[0]), r.rReq), e2eQuery("UpdateTestSuite", vars(rec[1]), r.rReq.Add(r.sendGap))},
			[]*models.HTTPReq{e2eQuery("InsertTestSuite", vars(live[0]), r.fReq), e2eQuery("UpdateTestSuite", vars(live[1]), r.fReq.Add(r.sendGap))})
		return r.clientValue("body.docs.msgs.0.delta.name", String, field[0], field[1], earlier)
	}
	rn, fn := name(r.rNanos), name(r.fNanos)
	for what, ds := range map[string][]Decision{
		"the field's whole text at the second request in the recording only": suite([2]string{rn, rn + "-renamed"}, [2]string{fn, fn + "-renamed-2"}, [2]string{rn + "-renamed", fn + "-renamed"}),
		"the field's whole text at the second request in round F only":       suite([2]string{rn, rn + "-renamed-2"}, [2]string{fn, fn + "-renamed"}, [2]string{rn + "-renamed", fn + "-renamed"}),
		"one text at both requests in the recording only":                    suite([2]string{rn, rn}, [2]string{fn, fn + "-x"}, [2]string{"^" + rn + "$", "^" + fn + "$"}),
		"one text at both requests in round F only":                          suite([2]string{rn, rn + "-x"}, [2]string{fn, fn}, [2]string{"^" + rn + "$", "^" + fn + "$"}),
	} {
		if len(ds) != 1 || ds[0].Refusal == nil || ds[0].Refusal.Kind != RefusalTwoSources {
			t.Errorf("%s: %s; want refused, two sources", what, oDescribe(ds))
		}
	}

	// A stamp each request sends as its X-Signed-At header, and the field.
	stamped := func(rec, live string, field [2]string) []Decision {
		reqs := func(s string, at time.Time) []*models.HTTPReq {
			return []*models.HTTPReq{
				{Method: "POST", URL: "http://localhost:8083/auth/temp-code", Timestamp: at, Header: map[string]string{"X-Signed-At": s, "Host": "localhost:8083"}},
				{Method: "POST", URL: "http://localhost:8083/auth/temp-code/confirm", Timestamp: at.Add(r.sendGap), Header: map[string]string{"X-Signed-At": s, "Host": "localhost:8083"}},
			}
		}
		earlier := clientSent(t, []string{"post-auth-temp-code-1", "post-auth-temp-code-2"}, reqs(rec, r.rReq), reqs(live, r.fReq))
		return r.clientValue("body.docs.msgs.0.signedAt", String, field[0], field[1], earlier)
	}
	// An encoder that leaves out a zero fraction writes one run's at a second.
	rs, fs := r.rSigned.Format(time.RFC3339Nano), r.fSigned.Add(123*time.Millisecond).Format(time.RFC3339Nano)
	for what, ds := range map[string][]Decision{
		"bare at a second in the recording": stamped(rs, fs, [2]string{rs, fs}),
		"bare at a second in round F":       stamped(fs, rs, [2]string{fs, rs}),
	} {
		if len(ds) != 1 || ds[0].Refusal == nil || ds[0].Refusal.Kind != RefusalTwoSources {
			t.Errorf("a stamp sent twice, %s: %s; want refused, two sources", what, oDescribe(ds))
		}
	}
	for _, form := range []string{"%s-e2e", "e2e-%s"} {
		rec, live := strings.Replace(form, "%s", nanos(r.rSecond), 1), strings.Replace(form, "%s", nanos(r.fSecond), 1)
		ds := Decide(authTimeCall(r.rSecond, r.fSecond, r.rCall, r.fCall, clientSent(t, []string{"post-auth-temp-code-1", "post-auth-temp-code-2"},
			[]*models.HTTPReq{{Method: "POST", URL: "http://localhost:8083/auth/temp-code", Timestamp: r.rReq, Header: map[string]string{"X-Signed": rec}}, {Method: "POST", URL: "http://localhost:8083/auth/temp-code/confirm", Timestamp: r.rReq.Add(r.sendGap), Header: map[string]string{"X-Signed": rec}}},
			[]*models.HTTPReq{{Method: "POST", URL: "http://localhost:8083/auth/temp-code", Timestamp: r.fReq, Header: map[string]string{"X-Signed": live}}, {Method: "POST", URL: "http://localhost:8083/auth/temp-code/confirm", Timestamp: r.fReq.Add(r.sendGap), Header: map[string]string{"X-Signed": live}}})))
		if len(ds) != 1 || ds[0].Outcome != OutcomeValue || ds[0].From.Mock != "test case post-auth-temp-code-1" || ds[0].From.Path != "header.X-Signed" {
			t.Errorf("authTime %d -> %d, a stamp of a second in the longer text %q sent twice: %s; want a Value of test case post-auth-temp-code-1's header.X-Signed, the first send", r.rSecond, r.fSecond, rec, oDescribe(ds))
		}
	}
}
