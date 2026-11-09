package learn

import (
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// signInCase is the request api-server's e2e client sends to park a login
// code (post-auth-temp-code-4): its session cookie a token it signed at
// authTime, sent at at.
func signInCase(authTime int64, at time.Time) *models.HTTPReq {
	return &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/auth/temp-code", Timestamp: at,
		Header: map[string]string{"Content-Type": "application/json", "Cookie": "jwt_token_staging=" + signInJWT(authTime), "Host": "localhost:8083"},
		Body:   `{"code":"E2EHANDOFF1"}`}
}

// authTimeCall is the login-code insert's authTime leaf, recorded holding
// rec at rAt and holding live in record's full round, the call reaching
// keploy at fAt, with the places earlier.
func authTimeCall(rec, live int64, rAt, fAt time.Time, earlier []Earlier) Input {
	const p = "body.docs.msgs.0.authTime.$numberLong"
	in := iCall(p, Number, strconv.FormatInt(rec, 10), strconv.FormatInt(live, 10), rAt, fAt, nil, earlier, nil)
	in.Drifts = clockedDrifts()
	return in
}

// TestAClientTestsRequestIsAPlaceOfTheCallsItLedTo: a client test (keploy
// mock's) has no keploy test case to give its request; the requests its
// harness sends the application are its own. The login code's authTime is
// the auth_time of the session token the client signed and sent in its
// cookie. Each request of the test that reaches the application, paired
// with the one the test sent in its recording, is a place of the calls made
// after it in both runs: authTime is a variable of that request's Cookie,
// never a read of the call's clock. A request that reached the application
// after the call in either run is no place of it.
func TestAClientTestsRequestIsAPlaceOfTheCallsItLedTo(t *testing.T) {
	signed := time.Date(2026, 11, 9, 7, 1, 42, 0, time.UTC)
	gap := 69 * time.Second
	recReq, liveReq := signed.Add(497*time.Millisecond), signed.Add(gap+497*time.Millisecond)
	rCall, fCall := recReq.Add(time.Millisecond), liveReq.Add(time.Millisecond)
	rec, live := signed.Unix(), signed.Add(gap).Unix()
	const signIn = "test case post-auth-temp-code-4"
	placed := func(at time.Time) []Earlier {
		tr := NewTracker()
		tr.Begin()
		tr.SetCases([]ClientCase{
			{Name: "get-client-v1-apps-1", Req: &models.HTTPReq{Method: "GET", URL: "http://localhost:8083/client/v1/apps", Timestamp: signed}},
			{Name: "post-auth-temp-code-4", Req: signInCase(rec, recReq)},
		})
		if !tr.Incoming(signInCase(live, liveReq), at) {
			t.Fatal("the run's login-code request paired with no recorded request of the test; want post-auth-temp-code-4")
		}
		return tr.Placed().Earlier
	}

	d := Decide(authTimeCall(rec, live, rCall, fCall, placed(liveReq)))
	if len(d) != 1 || d[0].Outcome != OutcomeValue || d[0].From.Mock != "test case post-auth-temp-code-4" || d[0].From.Path != "header.Cookie" || d[0].Clock != nil {
		t.Fatalf("authTime %d -> %d, the auth_time of the token the client sent before the call: %s; want a Value of test case post-auth-temp-code-4's header.Cookie, no clock", rec, live, oDescribe(d))
	}

	// The same request, reaching the application after the call in
	// record's full round, held nothing the call could have taken.
	d = Decide(authTimeCall(rec, live, rCall, fCall, placed(fCall.Add(time.Microsecond))))
	if len(d) == 1 && d[0].From.Mock == signIn {
		t.Fatalf("a request that reached the application after the call is a place of it: %s", oDescribe(d))
	}
	// And after the call in its recording.
	late := authTimeCall(rec, live, recReq.Add(-time.Microsecond), fCall, placed(liveReq))
	if d = Decide(late); len(d) == 1 && d[0].From.Mock == signIn {
		t.Fatalf("a request recorded after the call is a place of it: %s", oDescribe(d))
	}
	// A request whose recorded instant keploy does not hold.
	untimed := func() []Earlier {
		tr := NewTracker()
		tr.Begin()
		tr.SetCases([]ClientCase{{Name: "post-auth-temp-code-4", Req: signInCase(rec, time.Time{})}})
		tr.Incoming(signInCase(live, liveReq), liveReq)
		return tr.Placed().Earlier
	}
	if d = Decide(authTimeCall(rec, live, rCall, fCall, untimed())); len(d) == 1 && d[0].From.Mock == signIn {
		t.Fatalf("a request whose recorded instant is not known is a place: %s", oDescribe(d))
	}
	// A reading that is not told the call's arrival in record's full round
	// (ClassifyIn) reads no client test's request.
	for _, ln := range ClassifyIn(clockedDrifts(), []string{"body.docs.msgs.0.authTime.$numberLong"}, authTimeCall(rec, live, rCall, fCall, nil).Live, authTimeCall(rec, live, rCall, fCall, nil).Rec, rCall, placed(liveReq), nil, nil, nil) {
		if ln.From.Mock == signIn {
			t.Fatalf("ClassifyIn, told no arrival, read the client's request as a place: %s from %+v", ln.Kind, ln.From)
		}
	}
	// Where the call's arrival in record's full round is not told, keploy
	// cannot show the client sent it before.
	unknown := authTimeCall(rec, live, rCall, time.Time{}, placed(liveReq))
	if d = Decide(unknown); len(d) == 1 && d[0].From.Mock == signIn {
		t.Fatalf("a request is a place of a call whose arrival is not told: %s", oDescribe(d))
	}
}

// TestAClientTestsRequestPairsByOrderMethodAndPath: the n-th request of the
// run with a method and a path (its record ids blanked) pairs with the n-th
// one of the test's recording with them, whatever the test's other requests
// did between; its query is not read. One left over, one of a call the
// recording never made, and one sent while no test is open give nothing.
func TestAClientTestsRequestPairsByOrderMethodAndPath(t *testing.T) {
	at := time.Date(2026, 11, 9, 7, 1, 42, 0, time.UTC)
	req := func(method models.Method, url, body string) *models.HTTPReq {
		return &models.HTTPReq{Method: method, URL: url, Body: body, Timestamp: at, Header: map[string]string{"Content-Type": "application/json"}}
	}
	cases := []ClientCase{
		{Name: "post-client-v1-apps-1", Req: req("POST", "http://localhost:8083/client/v1/apps", `{"name":"findme-1"}`)},
		{Name: "get-client-v1-apps-1", Req: req("GET", "http://localhost:8083/client/v1/apps?search=findme-1", "")},
		{Name: "post-client-v1-apps-2", Req: req("POST", "http://localhost:8083/client/v1/apps", `{"name":"findme-2"}`)},
		{Name: "delete-client-v1-apps-by-id-1", Req: req("DELETE", "http://localhost:8083/client/v1/apps/68d4148edcd121253694486c", "")},
	}
	cases = append([]ClientCase{{Name: "post-client-v1-apps-0"}}, cases...)
	if i, ok := PairCase(cases, nil, nil); ok {
		t.Fatalf("no request paired with %d", i)
	}
	tr := NewTracker()
	tr.Begin()
	tr.SetCases(cases)
	if tr.Incoming(cases[1].Req, time.Time{}) {
		t.Fatal("a request whose arrival is not told paired")
	}
	for _, c := range []struct {
		live *models.HTTPReq
		want string
	}{
		{req("GET", "http://localhost:8083/client/v1/apps?search=findme-7", ""), "get-client-v1-apps-1"},
		{req("POST", "http://localhost:8083/client/v1/apps", `{"name":"findme-7"}`), "post-client-v1-apps-1"},
		{req("POST", "http://localhost:8083/client/v1/apps", `{"name":"findme-8"}`), "post-client-v1-apps-2"},
		{req("POST", "http://localhost:8083/client/v1/apps", `{"name":"findme-9"}`), ""},
		{req("PUT", "http://localhost:8083/client/v1/apps", `{}`), ""},
		{req("DELETE", "http://localhost:8083/client/v1/apps/69a0000000000000000000ff", ""), "delete-client-v1-apps-by-id-1"},
	} {
		before := len(tr.Placed().Earlier)
		got := tr.Incoming(c.live, at)
		after := tr.Placed().Earlier
		switch {
		case got != (c.want != ""):
			t.Fatalf("%s %s paired %v; want %q", c.live.Method, c.live.URL, got, c.want)
		case c.want == "" && len(after) != before:
			t.Fatalf("%s %s paired with none, and gave a place", c.live.Method, c.live.URL)
		case c.want != "" && (len(after) != before+1 || after[len(after)-1].Mock != "test case "+c.want):
			t.Fatalf("%s %s gave %+v; want test case %s", c.live.Method, c.live.URL, after, c.want)
		}
	}
	tr.Begin()
	if tr.Incoming(cases[1].Req, at) || len(tr.Placed().Earlier) != 0 {
		t.Fatal("the next test pairs with the previous test's recorded requests, or keeps its places")
	}
	tr.SetCases(cases)
	tr.Incoming(cases[1].Req, at)
	tr.End()
	if tr.Incoming(cases[3].Req, at) {
		t.Fatal("a request sent while no test is open paired")
	}
	tr.NewSet()
	if n := len(tr.Placed().Earlier); n != 0 {
		t.Fatalf("a new set keeps %d of the last test's requests as places", n)
	}
	for url, want := range map[string]string{"http://h:1/a/12/b?q=1": "GET /a/{id}/b", "http://h": "GET http://h"} {
		if got := CaseKey("get", url); got != want {
			t.Errorf("CaseKey(get, %s) = %q; want %q", url, got, want)
		}
	}
}

// TestAClientTestsRequestKeployCannotReadIsSaid: a request whose body is
// kept apart from its test case, or that sends a form, is a place keploy
// could not read, which says why.
func TestAClientTestsRequestKeployCannotReadIsSaid(t *testing.T) {
	rec := &models.HTTPReq{Method: "POST", URL: "http://h/upload", Form: []models.FormData{{Key: "f", Values: []string{"a"}}}}
	if got := IncomingRequest("post-upload-1", rec, rec); got.Unread == "" || len(got.Parts) != 0 {
		t.Fatalf("a form request: %+v; want one keploy could not read, with no parts", got)
	}
	kept := &models.HTTPReq{Method: "POST", URL: "http://h/upload", BodyRef: models.BodyRef{Path: "assets/b"}}
	if got := IncomingRequest("post-upload-1", kept, &models.HTTPReq{Method: "POST", URL: "http://h/upload"}); got.Unread == "" {
		t.Fatalf("a body kept apart: %+v; want one keploy could not read", got)
	}
	plain := &models.HTTPReq{Method: "POST", URL: "http://h/upload"}
	for name, c := range map[string][2]*models.HTTPReq{
		"a form sent in record's full round alone": {plain, rec}, "a body kept apart in record's full round alone": {plain, kept},
		"no recorded request": {nil, plain}, "no request of the run": {plain, nil},
	} {
		if got := IncomingRequest("post-upload-1", c[0], c[1]); got.Unread == "" || len(got.Parts) != 0 {
			t.Errorf("%s: %+v; want one keploy could not read, with no parts", name, got)
		}
	}
	// Its parts: the URL, each header either run sent once, the body, and
	// a body kept as its bytes' base64 where either run sent one.
	got := IncomingRequest("post-apps-1",
		&models.HTTPReq{Method: "POST", URL: "http://h/a?x=1", Header: map[string]string{"A": "1", "B": "2"}, Body: "r"},
		&models.HTTPReq{Method: "POST", URL: "http://h/a?x=2", Header: map[string]string{"B": "3", "C": "4"}, Body: "l", Binary: "Ymlu"})
	want := []models.TestRequestPart{{Name: "url", Recorded: "http://h/a?x=1", Sent: "http://h/a?x=2"}, {Name: "header.A", Recorded: "1"}, {Name: "header.B", Recorded: "2", Sent: "3"},
		{Name: "header.C", Sent: "4"}, {Name: "body", Recorded: "r", Sent: "l"}, {Name: "binary", Sent: "Ymlu"}}
	if got.Unread != "" || len(got.Parts) != len(want) {
		t.Fatalf("parts %+v; want %+v", got, want)
	}
	for i := range want {
		if got.Parts[i] != want[i] {
			t.Fatalf("part %d %+v; want %+v", i, got.Parts[i], want[i])
		}
	}
}
