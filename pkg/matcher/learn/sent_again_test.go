package learn

import (
	"encoding/base64"
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

func authTimeDecision(ds []Decision) *Decision {
	for i := range ds {
		if ClaimName(ds[i].At) == "auth_time" {
			return &ds[i]
		}
	}
	return nil
}

// TestAJWTTokenLookupPrefersTheRequestCarryingThatExactJWT: during a device
// login, the browser sends its session JWT in Cookie (twice), the CLI sends
// its access JWT in Authorization: Bearer, and logout sends the refresh JWT in
// Cookie (refresh_token=<refresh>; refresh_token_staging=<refresh>). All three
// JWTs share the same auth_time claim. When the server queries refreshToken by
// the whole refresh JWT, its auth_time claim is bound at the logout request
// that carries that exact refresh JWT whole, never refused as two-sources
// against the browser/access JWTs. A call carrying a different JWT (or a bare
// authTime, not a JWT), or matching the token in only one run, or carrying the
// same token in mixed wrappers, still fails closed as two-sources.
func TestAJWTTokenLookupPrefersTheRequestCarryingThatExactJWT(t *testing.T) {
	r := newSentAgainRun()
	r.rCall, r.fCall = r.rReq.Add(4*r.sendGap), r.fReq.Add(4*r.sendGap)
	makeJWT := func(authTime int64, role, sid string) string {
		hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
		pay := base64.RawURLEncoding.EncodeToString([]byte(`{"auth_time":` + strconv.FormatInt(authTime, 10) + `,"role":"` + role + `","sid":"` + sid + `"}`))
		sig := base64.RawURLEncoding.EncodeToString([]byte("sig-" + role + "-" + sid))
		return hdr + "." + pay + "." + sig
	}
	rBrowser, fBrowser := makeJWT(r.rSecond, "browser", "sid-br-r"), makeJWT(r.fSecond, "browser", "sid-br-f")
	rAccess, fAccess := makeJWT(r.rSecond, "access", "sid-ac-r"), makeJWT(r.fSecond, "access", "sid-ac-f")
	rRefresh, fRefresh := makeJWT(r.rSecond, "refresh", "sid-rf-r"), makeJWT(r.fSecond, "refresh", "sid-rf-f")
	rOther, fOther := makeJWT(r.rSecond, "other", "sid-ot-r"), makeJWT(r.fSecond, "other", "sid-ot-f")

	reqs := func(at time.Time, browser, access, refresh string) []*models.HTTPReq {
		return []*models.HTTPReq{
			{Method: "GET", URL: "http://localhost:8083/oauth/device", Timestamp: at, Header: map[string]string{"Cookie": "jwt_token_staging=" + browser}},
			{Method: "POST", URL: "http://localhost:8083/oauth/device/decision", Timestamp: at.Add(r.sendGap), Header: map[string]string{"Cookie": "jwt_token_staging=" + browser}},
			{Method: "GET", URL: "http://localhost:8083/auth/token", Timestamp: at.Add(2 * r.sendGap), Header: map[string]string{"Authorization": "Bearer " + access}},
			{Method: "POST", URL: "http://localhost:8083/auth/logout", Timestamp: at.Add(3 * r.sendGap), Header: map[string]string{"Cookie": "refresh_token=" + refresh + "; refresh_token_staging=" + refresh}},
		}
	}
	earlier := clientSent(t,
		[]string{"get-oauth-device-1", "post-oauth-device-decision-1", "get-auth-token-1", "post-auth-logout-1"},
		reqs(r.rReq, rBrowser, rAccess, rRefresh),
		reqs(r.fReq, fBrowser, fAccess, fRefresh),
	)

	// 1. Refresh JWT in Cookie matches post-auth-logout-1 whole.
	ds := r.clientValue("body.cmd.filter.token", String, rRefresh, fRefresh, earlier)
	authDec := authTimeDecision(ds)
	if authDec == nil || authDec.Outcome != OutcomeValue || authDec.From.Mock != "test case post-auth-logout-1" || authDec.From.Path != "header.Cookie" {
		t.Fatalf("refresh JWT lookup auth_time: %s; want Value from test case post-auth-logout-1 header.Cookie", oDescribe(ds))
	}
	if wantAt := "header.Cookie|cookie.refresh_token|jwt.auth_time#0"; authDec.FromAt.String() != wantAt {
		t.Fatalf("refresh JWT lookup FromAt=%q; want %q", authDec.FromAt.String(), wantAt)
	}
	if wantLine := "did not carry that token whole (as its text, a Bearer credential or a cookie value)"; !strings.Contains(authDec.Line, wantLine) {
		t.Errorf("refresh JWT lookup line %q does not contain %q", authDec.Line, wantLine)
	}

	// 2. Access JWT in Authorization: Bearer matches get-auth-token-1 whole.
	dsAcc := r.clientValue("body.cmd.filter.token", String, rAccess, fAccess, earlier)
	authDec = authTimeDecision(dsAcc)
	if authDec == nil || authDec.Outcome != OutcomeValue || authDec.From.Mock != "test case get-auth-token-1" || authDec.From.Path != "header.Authorization" {
		t.Fatalf("access JWT lookup auth_time: %s; want Value from test case get-auth-token-1 header.Authorization", oDescribe(dsAcc))
	}

	// 3. Browser JWT sent twice in identical Cookie headers narrows to >=2 slots
	// and binds "sent again" at the first send.
	dsBr := r.clientValue("body.cmd.filter.token", String, rBrowser, fBrowser, earlier)
	authDec = authTimeDecision(dsBr)
	if authDec == nil || authDec.Outcome != OutcomeValue || authDec.From.Mock != "test case get-oauth-device-1" || authDec.From.Path != "header.Cookie" {
		t.Fatalf("browser JWT sent twice auth_time: %s; want Value from test case get-oauth-device-1 header.Cookie", oDescribe(dsBr))
	}
	if wantAt := "header.Cookie|jwt.auth_time#0"; authDec.FromAt.String() != wantAt {
		t.Fatalf("browser JWT sent twice FromAt=%q; want %q", authDec.FromAt.String(), wantAt)
	}
	if wantAgain := "; sent again: test case post-oauth-device-decision-1 header.Cookie"; !strings.Contains(authDec.Line, wantAgain) {
		t.Errorf("browser JWT sent twice line %q does not contain %q", authDec.Line, wantAgain)
	}

	// 4. Multi-JWT Cookie header (jwt_token_staging=<browser>; refresh_token=<refresh>)
	// binds cookie.refresh_token|jwt.auth_time, NOT the earlier jwt_token_staging pair.
	multiCookieEarlier := clientSent(t,
		[]string{"get-oauth-device-1", "post-auth-logout-1"},
		[]*models.HTTPReq{
			{Method: "GET", URL: "http://localhost:8083/oauth/device", Timestamp: r.rReq, Header: map[string]string{"Cookie": "jwt_token_staging=" + rBrowser}},
			{Method: "POST", URL: "http://localhost:8083/auth/logout", Timestamp: r.rReq.Add(r.sendGap), Header: map[string]string{"Cookie": "jwt_token_staging=" + rBrowser + "; refresh_token=" + rRefresh}},
		},
		[]*models.HTTPReq{
			{Method: "GET", URL: "http://localhost:8083/oauth/device", Timestamp: r.fReq, Header: map[string]string{"Cookie": "jwt_token_staging=" + fBrowser}},
			{Method: "POST", URL: "http://localhost:8083/auth/logout", Timestamp: r.fReq.Add(r.sendGap), Header: map[string]string{"Cookie": "jwt_token_staging=" + fBrowser + "; refresh_token=" + fRefresh}},
		},
	)
	dsMulti := r.clientValue("body.cmd.filter.token", String, rRefresh, fRefresh, multiCookieEarlier)
	authDec = authTimeDecision(dsMulti)
	if authDec == nil || authDec.Outcome != OutcomeValue || authDec.From.Mock != "test case post-auth-logout-1" {
		t.Fatalf("multi-JWT Cookie lookup auth_time: %s; want Value from test case post-auth-logout-1", oDescribe(dsMulti))
	}
	if wantAt := "header.Cookie|cookie.refresh_token|jwt.auth_time#0"; authDec.FromAt.String() != wantAt {
		t.Fatalf("multi-JWT Cookie lookup bound wrong pair: FromAt=%q; want %q", authDec.FromAt.String(), wantAt)
	}

	// 5. Mixed-wrapper cases (same JWT in Bearer + Cookie, or in different Cookie headers) stay two-sources.
	for name, pairs := range map[string][2][]*models.HTTPReq{
		"Bearer and Cookie holding same JWT": {
			{
				{Method: "GET", URL: "http://localhost:8083/auth/token", Timestamp: r.rReq, Header: map[string]string{"Authorization": "Bearer " + rRefresh}},
				{Method: "POST", URL: "http://localhost:8083/auth/logout", Timestamp: r.rReq.Add(r.sendGap), Header: map[string]string{"Cookie": "refresh_token=" + rRefresh}},
			},
			{
				{Method: "GET", URL: "http://localhost:8083/auth/token", Timestamp: r.fReq, Header: map[string]string{"Authorization": "Bearer " + fRefresh}},
				{Method: "POST", URL: "http://localhost:8083/auth/logout", Timestamp: r.fReq.Add(r.sendGap), Header: map[string]string{"Cookie": "refresh_token=" + fRefresh}},
			},
		},
		"two different Cookie wrappers holding same JWT": {
			{
				{Method: "POST", URL: "http://localhost:8083/auth/refresh", Timestamp: r.rReq, Header: map[string]string{"Cookie": "refresh_token=" + rRefresh}},
				{Method: "POST", URL: "http://localhost:8083/auth/logout", Timestamp: r.rReq.Add(r.sendGap), Header: map[string]string{"Cookie": "refresh_token=" + rRefresh + "; refresh_token_staging=" + rRefresh}},
			},
			{
				{Method: "POST", URL: "http://localhost:8083/auth/refresh", Timestamp: r.fReq, Header: map[string]string{"Cookie": "refresh_token=" + fRefresh}},
				{Method: "POST", URL: "http://localhost:8083/auth/logout", Timestamp: r.fReq.Add(r.sendGap), Header: map[string]string{"Cookie": "refresh_token=" + fRefresh + "; refresh_token_staging=" + fRefresh}},
			},
		},
	} {
		mixEarlier := clientSent(t, []string{"req-1", "req-2"}, pairs[0], pairs[1])
		dsMix := r.clientValue("body.cmd.filter.token", String, rRefresh, fRefresh, mixEarlier)
		authDec = authTimeDecision(dsMix)
		if authDec == nil || authDec.Refusal == nil || authDec.Refusal.Kind != RefusalTwoSources {
			t.Errorf("%s: %s; want refused (two-sources)", name, oDescribe(dsMix))
		}
	}

	// 6. Both-runs safety guards: slot must carry the exact JWT in BOTH runs,
	// and the field must parse as a JWT in BOTH runs.
	recOnlyEarlier := clientSent(t,
		[]string{"get-oauth-device-1", "post-auth-logout-1"},
		[]*models.HTTPReq{
			{Method: "GET", URL: "http://localhost:8083/oauth/device", Timestamp: r.rReq, Header: map[string]string{"Cookie": "jwt_token_staging=" + rBrowser}},
			{Method: "POST", URL: "http://localhost:8083/auth/logout", Timestamp: r.rReq.Add(r.sendGap), Header: map[string]string{"Cookie": "refresh_token=" + rRefresh}},
		},
		[]*models.HTTPReq{
			{Method: "GET", URL: "http://localhost:8083/oauth/device", Timestamp: r.fReq, Header: map[string]string{"Cookie": "jwt_token_staging=" + fBrowser}},
			{Method: "POST", URL: "http://localhost:8083/auth/logout", Timestamp: r.fReq.Add(r.sendGap), Header: map[string]string{"Cookie": "refresh_token=" + fOther}},
		},
	)
	if authDec = authTimeDecision(r.clientValue("body.cmd.filter.token", String, rRefresh, fRefresh, recOnlyEarlier)); authDec == nil || authDec.Refusal == nil || authDec.Refusal.Kind != RefusalTwoSources {
		t.Errorf("logout carries exact JWT in recording only: want refused (two-sources); got %+v", authDec)
	}

	liveOnlyEarlier := clientSent(t,
		[]string{"get-oauth-device-1", "post-auth-logout-1"},
		[]*models.HTTPReq{
			{Method: "GET", URL: "http://localhost:8083/oauth/device", Timestamp: r.rReq, Header: map[string]string{"Cookie": "jwt_token_staging=" + rBrowser}},
			{Method: "POST", URL: "http://localhost:8083/auth/logout", Timestamp: r.rReq.Add(r.sendGap), Header: map[string]string{"Cookie": "refresh_token=" + rOther}},
		},
		[]*models.HTTPReq{
			{Method: "GET", URL: "http://localhost:8083/oauth/device", Timestamp: r.fReq, Header: map[string]string{"Cookie": "jwt_token_staging=" + fBrowser}},
			{Method: "POST", URL: "http://localhost:8083/auth/logout", Timestamp: r.fReq.Add(r.sendGap), Header: map[string]string{"Cookie": "refresh_token=" + fRefresh}},
		},
	)
	if authDec = authTimeDecision(r.clientValue("body.cmd.filter.token", String, rRefresh, fRefresh, liveOnlyEarlier)); authDec == nil || authDec.Refusal == nil || authDec.Refusal.Kind != RefusalTwoSources {
		t.Errorf("logout carries exact JWT in round F only: want refused (two-sources); got %+v", authDec)
	}

	// Field parses as a JWT in one run only: slot carries the exact text of both
	// runs (so carriesToken WOULD match if the okR && okL JWT guard were removed).
	// oneSource must stay unnarrowed (len == 2, again == "").
	x := newReaderIndex(nil)
	makeSlot := func(mock, path, recText, liveText string) evPlace {
		return evPlace{
			mock: mock, path: path, name: mock + " " + path,
			rec: recText, live: liveText,
			t: &placeText{rec: x.text(recText), live: x.text(liveText)},
		}
	}
	pBr := makeSlot("test case get-oauth-device-1", "header.Cookie", "jwt_token_staging="+rBrowser, "jwt_token_staging="+fBrowser)
	notJWTR, notJWTF := "not-a-jwt-"+nanos(r.rSecond), "not-a-jwt-"+nanos(r.fSecond)
	for _, tc := range []struct {
		label string
		pRf   evPlace
		c     changedInstant
	}{
		{
			label: "JWT in rec only",
			pRf:   makeSlot("test case post-auth-logout-1", "header.Cookie", "refresh_token="+rRefresh, "refresh_token="+notJWTF),
			c:     changedInstant{text: true, rec: rRefresh, live: notJWTF},
		},
		{
			label: "JWT in live only",
			pRf:   makeSlot("test case post-auth-logout-1", "header.Cookie", "refresh_token="+notJWTR, "refresh_token="+fRefresh),
			c:     changedInstant{text: true, rec: notJWTR, live: fRefresh},
		},
	} {
		if !carriesToken(tc.pRf.path, tc.pRf.rec, tc.pRf.live, tc.c.rec, tc.c.live) {
			t.Fatalf("%s: precondition failed (slot must carry both texts so test exercises okR && okL guard)", tc.label)
		}
		if got, again := oneSource([]evPlace{pBr, tc.pRf}, tc.c); len(got) != 2 || again != "" {
			t.Errorf("oneSource(%s) = len %d, %q; want len 2, empty", tc.label, len(got), again)
		}
	}

	// Duplicate cookie names (skipped by cookieSpans) must NOT narrow, so narrowing
	// and cookieTokenFor share one addressing model and never bind the wrong JWT pair.
	dupCookieEarlier := clientSent(t,
		[]string{"get-oauth-device-1", "post-auth-logout-1"},
		[]*models.HTTPReq{
			{Method: "GET", URL: "http://localhost:8083/oauth/device", Timestamp: r.rReq, Header: map[string]string{"Cookie": "jwt_token_staging=" + rBrowser}},
			{Method: "POST", URL: "http://localhost:8083/auth/logout", Timestamp: r.rReq.Add(r.sendGap), Header: map[string]string{"Cookie": "jwt_token_staging=" + rBrowser + "; refresh_token=" + rRefresh + "; refresh_token=" + rRefresh}},
		},
		[]*models.HTTPReq{
			{Method: "GET", URL: "http://localhost:8083/oauth/device", Timestamp: r.fReq, Header: map[string]string{"Cookie": "jwt_token_staging=" + fBrowser}},
			{Method: "POST", URL: "http://localhost:8083/auth/logout", Timestamp: r.fReq.Add(r.sendGap), Header: map[string]string{"Cookie": "jwt_token_staging=" + fBrowser + "; refresh_token=" + fRefresh + "; refresh_token=" + fRefresh}},
		},
	)
	if authDec = authTimeDecision(r.clientValue("body.cmd.filter.token", String, rRefresh, fRefresh, dupCookieEarlier)); authDec == nil || authDec.Refusal == nil || authDec.Refusal.Kind != RefusalTwoSources {
		t.Errorf("duplicate refresh_token cookie name: want refused (two-sources); got %+v", authDec)
	}

	// 7. Unrelated JWT not carried by any request stays refused (two-sources).
	dsOther := r.clientValue("body.cmd.filter.token", String, rOther, fOther, earlier)
	authDec = authTimeDecision(dsOther)
	if authDec == nil || authDec.Refusal == nil || authDec.Refusal.Kind != RefusalTwoSources {
		t.Fatalf("unrelated JWT lookup auth_time: %s; want refused (two-sources)", oDescribe(dsOther))
	}

	// 8. Bare authTime (not a JWT) beside different JWT headers stays refused (two-sources).
	dsBare := Decide(authTimeCall(r.rSecond, r.fSecond, r.rCall, r.fCall, earlier))
	if len(dsBare) != 1 || dsBare[0].Refusal == nil || dsBare[0].Refusal.Kind != RefusalTwoSources {
		t.Fatalf("bare authTime beside distinct JWT headers: %s; want refused (two-sources)", oDescribe(dsBare))
	}
}

func TestCarriesToken(t *testing.T) {
	const (
		tokR = "eyJhbGciOiJIUzI1NiJ9.eyJhdXRoX3RpbWUiOjE3OTU0MjYzMzF9.sig_rec"
		tokL = "eyJhbGciOiJIUzI1NiJ9.eyJhdXRoX3RpbWUiOjE3OTU0MjY0MDB9.sig_live"
	)
	cases := []struct {
		name  string
		path  string
		pRec  string
		pLive string
		want  bool
	}{
		{"exact whole text", "body.token", tokR, tokL, true},
		{"exact whole text with suffix", "body.token", tokR + "x", tokL + "x", false},
		{"exact whole text with prefix", "body.token", "x" + tokR, "x" + tokL, false},
		{"Bearer exact", "header.Authorization", "Bearer " + tokR, "Bearer " + tokL, true},
		{"bearer lowercase", "header.Authorization", "bearer " + tokR, "bearer " + tokL, true},
		{"Bearer with suffix", "header.Authorization", "Bearer " + tokR + "x", "Bearer " + tokL + "x", false},
		{"Bearer with prefix", "header.Authorization", "Bearer x" + tokR, "Bearer x" + tokL, false},
		{"Bearer with extra word", "header.Authorization", "Bearer " + tokR + " extra", "Bearer " + tokL + " extra", false},
		{"Token scheme", "header.Authorization", "Token " + tokR, "Token " + tokL, false},
		{"Bearer tab rejected by StepBearer", "header.Authorization", "Bearer\t" + tokR, "Bearer\t" + tokL, false},
		{"Bearer leading space rejected", "header.Authorization", " Bearer " + tokR, " Bearer " + tokL, false},
		{"Bearer trailing space rejected", "header.Authorization", "Bearer " + tokR + " ", "Bearer " + tokL + " ", false},
		{"Cookie pair exact", "header.Cookie", "a=1; refresh_token=" + tokR + "; b=2", "a=1; refresh_token=" + tokL + "; b=2", true},
		{"Set-Cookie pair exact", "header.Set-Cookie", "refresh_token=" + tokR + "; HttpOnly; Path=/", "refresh_token=" + tokL + "; HttpOnly; Path=/", true},
		{"Cookie pair suffix", "header.Cookie", "refresh_token=" + tokR + "x", "refresh_token=" + tokL + "x", false},
		{"Cookie pair prefix", "header.Cookie", "refresh_token=x" + tokR, "refresh_token=x" + tokL, false},
		{"Cookie quoted value", "header.Cookie", `refresh_token="` + tokR + `"`, `refresh_token="` + tokL + `"`, false},
		{"Cookie trailing space and tab trimmed", "header.Cookie", "refresh_token=" + tokR + " \t; HttpOnly", "refresh_token=" + tokL + " \t; HttpOnly", true},
		{"Cookie name with no equals", "header.Cookie", tokR + "; HttpOnly", tokL + "; HttpOnly", false},
		{"Cookie duplicate name rejected (matches cookieSpans)", "header.Cookie", "refresh_token=" + tokR + "; refresh_token=" + tokR, "refresh_token=" + tokL + "; refresh_token=" + tokL, false},
		{"Cookie different pair names across runs rejected", "header.Cookie", "a=" + tokR, "b=" + tokL, false},
		{"mixed carrier Bearer in R and Cookie in F rejected", "header.Cookie", "Bearer " + tokR, "refresh_token=" + tokL, false},
		{"empty text", "header.Cookie", "", "", false},
		{"non-Cookie header with key=tok rejected", "header.X-Foo", "refresh_token=" + tokR, "refresh_token=" + tokL, false},
		{"body leaf with key=tok rejected", "body.meta", "refresh_token=" + tokR, "refresh_token=" + tokL, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := carriesToken(tc.path, tc.pRec, tc.pLive, tokR, tokL); got != tc.want {
				t.Errorf("carriesToken(%q, %q, %q, tokR, tokL) = %v; want %v", tc.path, tc.pRec, tc.pLive, got, tc.want)
			}
		})
	}
}
