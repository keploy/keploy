package learn

import (
	"strconv"
	"strings"
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
		if !tr.Incoming(signInCase(live, liveReq), "", at) {
			t.Fatal("the run's login-code request paired with no recorded request of the test; want post-auth-temp-code-4")
		}
		return tr.Placed().Earlier
	}

	d := Decide(authTimeCall(rec, live, rCall, fCall, placed(liveReq)))
	if len(d) != 1 || d[0].Outcome != OutcomeValue || d[0].From.Mock != "test case post-auth-temp-code-4" || d[0].From.Path != "header.Cookie" || d[0].Clock != nil || !d[0].From.Client {
		t.Fatalf("authTime %d -> %d, the auth_time of the token the client sent before the call: %s (client test's request: %v); want a Value of test case post-auth-temp-code-4's header.Cookie, a client test's, no clock", rec, live, oDescribe(d), len(d) == 1 && d[0].From.Client)
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
		tr.Incoming(signInCase(live, liveReq), "", liveReq)
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

// A client test's request left out of a call's places, as one that reached
// the application after the call, leaves the call's other places in: the
// value an exchange answered the application before the call binds there.
func TestALateClientRequestLeavesTheCallsOtherPlaces(t *testing.T) {
	signed := time.Date(2026, 11, 9, 7, 1, 42, 0, time.UTC)
	gap := 69 * time.Second
	recReq, liveReq := signed.Add(497*time.Millisecond), signed.Add(gap+497*time.Millisecond)
	rCall, fCall := recReq.Add(time.Millisecond), liveReq.Add(time.Millisecond)
	rec, live := signed.Unix(), signed.Add(gap).Unix()
	tr := NewTracker()
	tr.Begin()
	tr.SetCases([]ClientCase{{Name: "post-auth-temp-code-4", Req: signInCase(rec, recReq)}})
	if !tr.Incoming(signInCase(live, liveReq), "", fCall.Add(time.Microsecond)) {
		t.Fatal("the run's login-code request paired with no recorded request of the test")
	}
	earlier := tr.Placed().Earlier
	if len(earlier) != 1 {
		t.Fatalf("placed %d; want the client test's request", len(earlier))
	}
	at := recReq.Add(-100 * time.Millisecond)
	payload := RawOf(&models.Mock{Name: "mock-7", Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond)}}, true)
	served := Earlier{Mock: "mock-7", At: at, Own: true, Payload: payload.ServedAs(1), Seen: []Seen{{Side: Response,
		Leaf: Leaf{Path: "body.session.auth_time", Type: Number, Value: strconv.FormatInt(rec, 10)}, Live: strconv.FormatInt(live, 10)}}}
	d := Decide(authTimeCall(rec, live, rCall, fCall, append(earlier, served)))
	if len(d) != 1 || d[0].Outcome != OutcomeValue || d[0].From.Mock != "mock-7" || d[0].From.Client {
		t.Fatalf("authTime %d -> %d, served by mock-7 before the call, the client's request late: %s; want a Value of mock-7", rec, live, oDescribe(d))
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
	if i, ok := PairCase(cases, nil, nil, true); ok {
		t.Fatalf("no request paired with %d", i)
	}
	tr := NewTracker()
	tr.Begin()
	tr.SetCases(cases)
	if tr.Incoming(cases[1].Req, "", time.Time{}) {
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
		got := tr.Incoming(c.live, "", at)
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
	if tr.Incoming(cases[1].Req, "", at) || len(tr.Placed().Earlier) != 0 {
		t.Fatal("the next test pairs with the previous test's recorded requests, or keeps its places")
	}
	tr.SetCases(cases)
	tr.Incoming(cases[1].Req, "", at)
	tr.End()
	if tr.Incoming(cases[3].Req, "", at) {
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
// kept apart from its test case, or that sends a file in a form (recorded
// so, or sent so), is a place keploy could not read, which says why.
func TestAClientTestsRequestKeployCannotReadIsSaid(t *testing.T) {
	rec := &models.HTTPReq{Method: "POST", URL: "http://h/upload", Form: []models.FormData{{Key: "f", FileNames: []string{"a.txt"}, Paths: []string{"/tmp/a"}}}}
	if got := IncomingRequest("post-upload-1", rec, rec); got.Unread == "" || len(got.Parts) != 0 {
		t.Fatalf("a form request sending a file: %+v; want one keploy could not read, with no parts", got)
	}
	kept := &models.HTTPReq{Method: "POST", URL: "http://h/upload", BodyRef: models.BodyRef{Path: "assets/b"}}
	if got := IncomingRequest("post-upload-1", kept, &models.HTTPReq{Method: "POST", URL: "http://h/upload"}); got.Unread == "" {
		t.Fatalf("a body kept apart: %+v; want one keploy could not read", got)
	}
	plain := &models.HTTPReq{Method: "POST", URL: "http://h/upload"}
	for name, c := range map[string][2]*models.HTTPReq{
		"a file sent in record's full round alone": {plain, rec}, "a body kept apart in record's full round alone": {plain, kept},
		"no recorded request": {nil, plain}, "no request of the run": {plain, nil},
		// The ingress gives a form as its raw body: the recording's form says
		// what it is.
		"a form recorded, sent as the ingress reads it": {rec, {Method: "POST", URL: "http://h/upload", Body: "--b\r\nContent-Disposition: form-data; name=\"f\"\r\n\r\na\r\n--b--\r\n"}},
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

// TestAClientTestIsAskedForItsRequestsWhileItsRecordingSentOneNotYetPaired:
// the ingress reads a request's body before the application is given it only
// while a test is open whose recording sent a request no request of the run
// paired with yet; a test with none, a test whose requests all paired, and
// no test open ask for none.
func TestAClientTestIsAskedForItsRequestsWhileItsRecordingSentOneNotYetPaired(t *testing.T) {
	at := time.Date(2026, 11, 9, 7, 1, 42, 0, time.UTC)
	post := &models.HTTPReq{Method: "POST", URL: "http://api/apps", Body: `{"name":"app-1"}`, Timestamp: at}
	var nilTracker *Tracker
	tr := NewTracker()
	switch {
	case nilTracker.WantsIncoming():
		t.Fatal("no tracker asks for requests")
	case tr.WantsIncoming():
		t.Fatal("a tracker with no test open asks for requests")
	}
	tr.SetCases([]ClientCase{{Name: "post-apps-1", Req: post}})
	if tr.WantsIncoming() {
		t.Fatal("cases given while no test is open ask for requests")
	}
	tr.Begin()
	if tr.WantsIncoming() {
		t.Fatal("a test with no recorded request asks for requests")
	}
	tr.SetCases([]ClientCase{{Name: "post-apps-1", Req: post}})
	if !tr.WantsIncoming() {
		t.Fatal("a test whose recorded request is not paired yet asks for none")
	}
	if !tr.Incoming(post, "", at) || tr.WantsIncoming() {
		t.Fatal("a test whose requests all paired still asks for requests")
	}
	tr.SetCases([]ClientCase{{Name: "post-apps-1", Req: post}})
	tr.End()
	if tr.WantsIncoming() {
		t.Fatal("an ended test asks for requests")
	}
}

// TestAClientTestsRequestKeployCouldNotReadRefusesWhatChangedAfterIt: a
// request whose body the ingress did not read before the application was
// given it pairs as any request does, but its place is one keploy could not
// read, saying why: every value that changed in a call after it is refused,
// never bound to a part of it nor learned as noise.
func TestAClientTestsRequestKeployCouldNotReadRefusesWhatChangedAfterIt(t *testing.T) {
	at := time.Date(2026, 11, 9, 7, 1, 42, 0, time.UTC)
	rec := &models.HTTPReq{Method: "POST", URL: "http://api/upload", Body: "a", Timestamp: at.Add(-time.Hour)}
	tr := NewTracker()
	tr.Begin()
	tr.SetCases([]ClientCase{{Name: "post-upload-1", Req: rec}})
	const why = "its body declares no length"
	if !tr.Incoming(&models.HTTPReq{Method: "POST", URL: "http://api/upload"}, why, at) {
		t.Fatal("an unread request did not pair")
	}
	got := tr.Placed().Earlier
	if len(got) != 1 || got[0].Unread != why || len(got[0].Seen) != 0 || !got[0].incoming ||
		!got[0].arrivedRec.Equal(rec.Timestamp) || !got[0].arrivedLive.Equal(at) || got[0].Mock != "test case post-upload-1" {
		t.Fatalf("placed %+v", got)
	}
	if r := requestUnread(got); !strings.Contains(r, why) {
		t.Fatalf("what changed after it is refused with %q, want it to say %q", r, why)
	}
}

// appNameCase is the request a client test sends to create an app it names
// "[E2E] app-crud-<nanos>", nanos read from its own clock (api-server's e2e
// client: time.Now().UnixNano()), sent at at.
func appNameCase(nanos int64, at time.Time) *models.HTTPReq {
	return &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/client/v1/apps", Timestamp: at,
		Header: map[string]string{"Content-Type": "application/json", "Host": "localhost:8083"},
		Body:   `{"endpoint":"https://example.com","name":"[E2E] app-crud-` + strconv.FormatInt(nanos, 10) + `"}`}
}

// clientPlaced is the earlier places of a client test's call, the test's
// recording holding the one request rec sent at rAt, which pairs with live,
// sent at fAt in record's full round.
func clientPlaced(t *testing.T, name string, rec, live *models.HTTPReq, fAt time.Time) []Earlier {
	t.Helper()
	tr := NewTracker()
	tr.Begin()
	tr.SetCases([]ClientCase{{Name: name, Req: rec}})
	if !tr.Incoming(live, "", fAt) {
		t.Fatalf("the run's request paired with no recorded request of the test; want %s", name)
	}
	return tr.Placed().Earlier
}

// TestAClientTestsUnixNanoNameIsBoundAtItsRequest: a client test names the
// app it creates by its own clock (UnixNano) and sends the name in its
// request; the application writes it whole (the app_name_reservations
// insert). The name is a Value bound at that request's body.name, a client
// test's request: never a read of the call's clock, nor a value the
// application made. A value the call took from a recording keploy served
// stays that recording's, whatever client requests came before it.
func TestAClientTestsUnixNanoNameIsBoundAtItsRequest(t *testing.T) {
	rNanos := int64(1794207702156240454)
	rReq := time.Unix(0, rNanos).Add(131 * time.Microsecond)
	gap := 69*time.Second + 3*time.Millisecond
	fNanos, fReq := rNanos+gap.Nanoseconds(), rReq.Add(gap)
	rCall, fCall := rReq.Add(2*time.Millisecond), fReq.Add(2*time.Millisecond)
	name := func(n int64) string { return "[E2E] app-crud-" + strconv.FormatInt(n, 10) }
	const p = "body.docs.msgs.0.name"
	in := iCall(p, String, name(rNanos), name(fNanos), rCall, fCall, nil, clientPlaced(t, "post-client-v1-apps-1", appNameCase(rNanos, rReq), appNameCase(fNanos, fReq), fReq), nil)
	in.Drifts = oClientDrifts()
	d := Decide(in)
	if len(d) != 1 || d[0].Outcome != OutcomeValue || d[0].From.Mock != "test case post-client-v1-apps-1" || d[0].From.Path != "body.name" || !d[0].From.Client || d[0].Clock != nil {
		t.Fatalf("the app name %q -> %q the client sent: %s (client test's request: %v); want a Value of test case post-client-v1-apps-1's body.name, a client test's, no clock", name(rNanos), name(fNanos), oDescribe(d), len(d) == 1 && d[0].From.Client)
	}

	// The body read whole holds it only through its JSON's name: one slot.
	// Two leaves of the body, or two requests, that hold it are two slots,
	// two separate places the application could have read it from.
	twoSlots := func(rec, live []*models.HTTPReq) []Decision {
		tr := NewTracker()
		tr.Begin()
		cases := make([]ClientCase, len(rec))
		for i, r := range rec {
			cases[i] = ClientCase{Name: "case-" + strconv.Itoa(i), Req: r}
		}
		tr.SetCases(cases)
		for _, l := range live {
			if !tr.Incoming(l, "", fReq) {
				t.Fatalf("the run's %s %s paired with none of the test's recorded requests", l.Method, l.URL)
			}
		}
		in := iCall(p, String, name(rNanos), name(fNanos), rCall, fCall, nil, tr.Placed().Earlier, nil)
		in.Drifts = oClientDrifts()
		return Decide(in)
	}
	withSlug := func(n int64, at time.Time) *models.HTTPReq {
		r := appNameCase(n, at)
		r.Body = `{"name":"` + name(n) + `","slug":"` + name(n) + `"}`
		return r
	}
	// Two leaves, one's name the start of the other's: two slots still.
	withNames := func(n int64, at time.Time) *models.HTTPReq {
		r := appNameCase(n, at)
		r.Body = `{"name":"` + name(n) + `","names":"` + name(n) + `"}`
		return r
	}
	plain := func(n int64, at time.Time) *models.HTTPReq {
		return &models.HTTPReq{Method: "PUT", URL: "http://localhost:8083/client/v1/apps/917ded3b-8816-4ebe-84f9-16d652f87600/title", Timestamp: at,
			Header: map[string]string{"Content-Type": "text/plain"}, Body: name(n)}
	}
	for what, ds := range map[string][]Decision{
		"two leaves of the body":                       twoSlots([]*models.HTTPReq{withSlug(rNanos, rReq)}, []*models.HTTPReq{withSlug(fNanos, fReq)}),
		"two leaves, one name the other's start":       twoSlots([]*models.HTTPReq{withNames(rNanos, rReq)}, []*models.HTTPReq{withNames(fNanos, fReq)}),
		"a plain body and another request's JSON leaf": twoSlots([]*models.HTTPReq{plain(rNanos, rReq), appNameCase(rNanos, rReq)}, []*models.HTTPReq{plain(fNanos, fReq), appNameCase(fNanos, fReq)}),
	} {
		if len(ds) != 1 || ds[0].Refusal == nil || ds[0].Refusal.Kind != RefusalTwoSources {
			t.Errorf("the app name held at %s: %s; want refused, two sources", what, oDescribe(ds))
		}
	}

	// An id the call took from a recording's answer, beside a client
	// request the test sent before it.
	before := oAt.Add(-3 * time.Second)
	placed := clientPlaced(t, "post-client-v1-apps-1", appNameCase(rNanos, before), appNameCase(fNanos, before), before)
	served := oServed("mock-20", false, false, Response, oText("body.id", "17"), "18", time.Millisecond)
	w := oWord(t, Decide(oInput("body.key", "uploads/17/a.png", "uploads/18/a.png", append(placed, served...), nil)), "17", "18")
	if w.Outcome != OutcomeValue || w.From.Mock != "mock-20" || w.From.Client {
		t.Fatalf("uploads/17 beside mock-20's id 17 to 18, after a client request: %s (client test's request: %v); want a Value from mock-20, no client test's", oDescribe([]Decision{w}), w.From.Client)
	}
}

// TestClassifyNamesAClientTestsRequestAndTheVariableItBinds: the learn
// step's classify (ClassifyArrived) reads the app name a client test sent as
// a variable of the request that sent it, a client test's (Place.Client),
// bound at the place a replay reads in the paired request (Place.Var,
// body.name of a JSON body). A body that declares no media type is read by
// its bytes: its leaf is the name's place still, and binds no variable.
func TestClassifyNamesAClientTestsRequestAndTheVariableItBinds(t *testing.T) {
	rNanos := int64(1794207702156240454)
	rReq := time.Unix(0, rNanos).Add(131 * time.Microsecond)
	gap := 69*time.Second + 3*time.Millisecond
	fNanos, fReq := rNanos+gap.Nanoseconds(), rReq.Add(gap)
	rCall, fCall := rReq.Add(2*time.Millisecond), fReq.Add(2*time.Millisecond)
	name := func(n int64) string { return "[E2E] app-crud-" + strconv.FormatInt(n, 10) }
	const p = "body.docs.msgs.0.name"
	sh := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	undeclared := func(n int64, at time.Time) *models.HTTPReq {
		r := appNameCase(n, at)
		delete(r.Header, "Content-Type")
		return r
	}
	for _, c := range []struct {
		what    string
		req     func(int64, time.Time) *models.HTTPReq
		wantVar string
	}{
		{"a JSON body", appNameCase, "body.name"},
		{"a body that declares no media type", undeclared, ""},
	} {
		earlier := clientPlaced(t, "post-client-v1-apps-1", c.req(rNanos, rReq), c.req(fNanos, fReq), fReq)
		ln := ClassifyArrived(oClientDrifts(), []string{p}, sh(name(fNanos)), sh(name(rNanos)), rCall, fCall, earlier, nil, nil, nil, nil)[0]
		if ln.Kind != KindVariable || ln.From.Mock != "test case post-client-v1-apps-1" || ln.From.Path != "body.name" || !ln.From.Client || ln.From.Var != c.wantVar {
			t.Errorf("the app name sent in %s: %s (%s) from %s %s, a client test's request: %v, variable place %q; want a variable of test case post-client-v1-apps-1's body.name, a client test's, variable place %q",
				c.what, ln.Kind, ln.Why, ln.From.Mock, ln.From.Path, ln.From.Client, ln.From.Var, c.wantVar)
		}
	}
}

// TestARequestKeployTestSentIsNoClientTestsRequest: the auth_time a keploy
// test case's request carries in its token binds there as it does in a
// client test's, but the place is a request keploy test sent, no client
// test's (Place.Client), which the adapter refuses as one it cannot follow.
func TestARequestKeployTestSentIsNoClientTestsRequest(t *testing.T) {
	at := time.Date(2026, 11, 9, 7, 1, 42, 498_000_000, time.UTC)
	rec, live := at.Unix(), at.Add(69*time.Second).Unix()
	req := &models.TestRequest{Test: "post-auth-temp-code-4", Parts: []models.TestRequestPart{
		jsonType,
		{Name: "header.Cookie", Recorded: "jwt_token_staging=" + signInJWT(rec), Sent: "jwt_token_staging=" + signInJWT(live)},
		{Name: "body", Recorded: `{"code":"E2EHANDOFF1"}`, Sent: `{"code":"E2EHANDOFF1"}`},
	}}
	d := Decide(authTimeCall(rec, live, at, at.Add(69*time.Second), []Earlier{GivenRequest(req)}))
	if len(d) != 1 || d[0].Outcome != OutcomeValue || d[0].From.Mock != "test case post-auth-temp-code-4" || !d[0].From.Given || d[0].From.Client {
		t.Fatalf("authTime %d -> %d in the request keploy test sent: %s (given %v, client test's %v); want a Value of that request, given, no client test's", rec, live, oDescribe(d), len(d) == 1 && d[0].From.Given, len(d) == 1 && d[0].From.Client)
	}
}

// A place GivenPlaceCase names a test case of is a test case's request
// (givenPlace): a recording's is not, nor one with no test case's name
// after "test case ", nor the request of a test case keploy cannot name
// (givenPlace("")).
func TestGivenPlaceCaseNamesOnlyATestCase(t *testing.T) {
	for place, want := range map[string]string{"test case post-client-v1-apps-1": "post-client-v1-apps-1", "test case ": "", givenPlace(""): "", "mock-20": "", "test-2": ""} {
		if got, ok := GivenPlaceCase(place); ok != (want != "") || ok && got != want {
			t.Errorf("GivenPlaceCase(%q) = %q, %v; want %q, %v", place, got, ok, want, want != "")
		}
	}
}

// TestAClientRequestStillComingIsUnreadUntilItsBodyEnds: a request whose
// body is still coming pairs as it arrives, as one keploy could not read,
// so what changed in a call before its body ended is refused; once done
// gives it whole it is a place as any request is, and a done after the next
// test began, or a second done, changes nothing.
func TestAClientRequestStillComingIsUnreadUntilItsBodyEnds(t *testing.T) {
	at := time.Date(2026, 11, 9, 7, 1, 42, 0, time.UTC)
	rec := &models.HTTPReq{Method: "POST", URL: "http://api/apps", Header: map[string]string{"Content-Type": "application/json"}, Body: `{"name":"app-1"}`, Timestamp: at.Add(-time.Hour)}
	live := &models.HTTPReq{Method: "POST", URL: "http://api/apps", Header: map[string]string{"Content-Type": "application/json"}}
	tr := NewTracker()
	tr.Begin()
	tr.SetCases([]ClientCase{{Name: "post-apps-1", Req: rec}})
	if done := tr.IncomingComing(&models.HTTPReq{Method: "GET", URL: "http://api/other"}, at); done != nil {
		t.Fatal("a request that pairs with none is taken as coming")
	}
	done := tr.IncomingComing(live, at)
	if done == nil || tr.WantsIncoming() {
		t.Fatal("a coming request did not pair")
	}
	got := tr.Placed().Earlier
	if len(got) != 1 || got[0].Unread != ComingUnread || !got[0].incoming || !got[0].arrivedRec.Equal(rec.Timestamp) || !got[0].arrivedLive.Equal(at) {
		t.Fatalf("placed while coming %+v", got)
	}
	if r := requestUnread(got); !strings.Contains(r, ComingUnread) {
		t.Fatalf("what changed while its body came is refused with %q", r)
	}
	whole := *live
	whole.Body = `{"name":"app-2"}`
	done(&whole, "", nil)
	got = tr.Placed().Earlier
	if len(got) != 1 || got[0].Unread != "" || got[0].Mock != "test case post-apps-1" || !got[0].arrivedLive.Equal(at) || !holdsSeen(got[0], "body.name", "app-1", "app-2") {
		t.Fatalf("placed once whole %+v", got)
	}
	if r := requestUnread(got); r != "" {
		t.Fatalf("once whole, what changed is refused with %q", r)
	}
	done(live, "its body did not come whole", nil)
	if got := tr.Placed().Earlier; got[0].Unread != "" {
		t.Fatalf("a second done changed the place: %+v", got)
	}

	// Its body ends once the next test began: that test's places stay,
	// though its first place stands where the coming request's stood.
	done = tr.IncomingComing(live, at)
	if done != nil {
		t.Fatal("a request with no recorded request left paired")
	}
	tr.Begin()
	tr.SetCases([]ClientCase{{Name: "post-apps-1", Req: rec}})
	done = tr.IncomingComing(live, at)
	tr.Begin()
	tr.SetCases([]ClientCase{{Name: "post-apps-2", Req: rec}})
	if !tr.Incoming(&whole, "", at) {
		t.Fatal("the next test's request did not pair")
	}
	done(live, "its body did not come whole", nil)
	if got := tr.Placed().Earlier; len(got) != 1 || got[0].Unread != "" || got[0].Mock != "test case post-apps-2" {
		t.Fatalf("a done after the next test began changed its places: %+v", got)
	}
	// Or once a new set began.
	tr.NewSet()
	tr.Begin()
	tr.SetCases([]ClientCase{{Name: "post-apps-3", Req: rec}, {Name: "post-apps-4", Req: rec}})
	done = tr.IncomingComing(live, at)
	tr.NewSet()
	tr.Begin()
	tr.SetCases([]ClientCase{{Name: "post-apps-5", Req: rec}})
	tr.Incoming(&whole, "", at)
	done(live, "its body did not come whole", nil)
	if got := tr.Placed().Earlier; len(got) != 1 || got[0].Unread != "" || got[0].Mock != "test case post-apps-5" {
		t.Fatalf("a done after a new set began changed its places: %+v", got)
	}
}

// holdsSeen reports whether e holds the leaf path, recorded rec and sent live.
func holdsSeen(e Earlier, path, rec, live string) bool {
	for _, s := range e.Seen {
		if s.Leaf.Path == path && s.Leaf.Value == rec && s.Live == live {
			return true
		}
	}
	return false
}

// TestAClientRequestsFormTextPartsArePlacesOfItsCalls: a client test's
// multipart form is read part by part, as a keploy test case's is: the n-th
// part of a name at form.<name>, then form.<name>#<n+1>, a part one side
// alone sent "" on the other. A part that sends a file, or a value kept
// apart, keploy does not read: the request says why.
func TestAClientRequestsFormTextPartsArePlacesOfItsCalls(t *testing.T) {
	rec := &models.HTTPReq{Method: "POST", URL: "http://api/apps", Form: []models.FormData{{Key: "name", Values: []string{"app-1"}}, {Key: "name", Values: []string{"x"}}}}
	live := &models.HTTPReq{Method: "POST", URL: "http://api/apps", Form: []models.FormData{{Key: "name", Values: []string{"app-2"}}, {Key: "suite", Values: []string{"s-2"}}}}
	req := IncomingRequest("post-apps-1", rec, live)
	want := map[string][2]string{"form.name": {"app-1", "app-2"}, "form.name#2": {"x", ""}, "form.suite": {"", "s-2"}}
	n := 0
	for _, p := range req.Parts {
		if w, ok := want[p.Name]; ok {
			n++
			if p.Recorded != w[0] || p.Sent != w[1] {
				t.Fatalf("part %s recorded %q sent %q, want %q %q", p.Name, p.Recorded, p.Sent, w[0], w[1])
			}
		}
	}
	if req.Unread != "" || n != len(want) {
		t.Fatalf("parts %+v (unread %q), want %v", req.Parts, req.Unread, want)
	}
	if e := GivenRequest(req); e.Unread != "" || !holdsSeen(e, "form.name", "app-1", "app-2") || requestUnread([]Earlier{e}) != "" {
		t.Fatalf("placed %+v", e)
	}
	for _, f := range []models.FormData{{Key: "f", FileNames: []string{"a.txt"}}, {Key: "v", Paths: []string{"/tmp/v"}}, {Key: "c", FileContents: [][]byte{[]byte("x")}}} {
		live := &models.HTTPReq{Method: "POST", URL: "http://api/apps", Form: []models.FormData{f}}
		if req := IncomingRequest("post-apps-1", rec, live); !strings.Contains(req.Unread, "form part "+f.Key+" sends a file or a value kept apart") || len(req.Parts) != 0 {
			t.Fatalf("a form part %+v gave %+v", f, req)
		}
	}
}

// TestRequestsOfOneCallPairByTheirWordsThenByOrder: where a test sent a
// call more than once, a request of the run pairs with the recorded one
// that holds the most of its words, so two workers' requests that
// reach the application in the other order than in the recording each pair
// with their own; requests keploy cannot tell apart pair by order.
func TestRequestsOfOneCallPairByTheirWordsThenByOrder(t *testing.T) {
	req := func(url, body string, form ...string) *models.HTTPReq {
		r := &models.HTTPReq{Method: "POST", URL: url, Body: body, Header: map[string]string{"Content-Type": "application/json", "X-Trace": "beta"}}
		if len(form) > 0 {
			r.Form = []models.FormData{{Key: "kind", Values: form}}
		}
		return r
	}
	hdr := func(r *models.HTTPReq, trace string) *models.HTTPReq {
		r.Header["X-Trace"] = trace
		return r
	}
	far := strings.Repeat("x ", pairWordsBytes/2)
	for _, c := range []struct {
		name string
		recs []*models.HTTPReq
		used []bool
		live *models.HTTPReq
		body bool
		want int
	}{
		{name: "a fixed word of the body tells the second from the first, arriving first",
			recs: []*models.HTTPReq{req("http://api/apps", `{"kind":"alpha","name":"n-111"}`), req("http://api/apps", `{"kind":"beta","name":"n-222"}`)},
			live: req("http://api/apps", `{"kind":"beta","name":"n-999"}`), body: true, want: 1},
		{name: "and the first, arriving second",
			recs: []*models.HTTPReq{req("http://api/apps", `{"kind":"alpha","name":"n-111"}`), req("http://api/apps", `{"kind":"beta","name":"n-222"}`)},
			live: req("http://api/apps", `{"kind":"alpha","name":"n-999"}`), body: true, want: 0},
		{name: "only the values the run made differ: by order",
			recs: []*models.HTTPReq{req("http://api/apps", `{"kind":"alpha","name":"n-111"}`), req("http://api/apps", `{"kind":"alpha","name":"n-222"}`)},
			live: req("http://api/apps", `{"kind":"alpha","name":"n-999"}`), body: true, want: 0},
		{name: "a word of the query tells",
			recs: []*models.HTTPReq{req("http://api/apps?page=1", ""), req("http://api/apps?page=2", "")},
			live: req("http://api/apps?page=2", ""), body: true, want: 1},
		{name: "a piece of an instant the run made tells nothing: by order",
			recs: []*models.HTTPReq{req("http://api/apps?since=2026-11-09T10:00:01Z", `{"at":"10:00:01","id":"0c1d-fade"}`), req("http://api/apps?since=2026-11-09T10:00:03Z", `{"at":"10:00:03","id":"77aa-beef"}`)},
			live: req("http://api/apps?since=2026-11-10T04:30:03Z", `{"at":"04:30:03","id":"1234-beef"}`), body: true, want: 0},
		{name: "a record's id in the path tells",
			recs: []*models.HTTPReq{req("http://api/apps/12", ""), req("http://api/apps/13", "")},
			live: req("http://api/apps/13", ""), body: true, want: 1},
		{name: "a body keploy has not read tells nothing: by order",
			recs: []*models.HTTPReq{req("http://api/apps", `{"kind":"alpha"}`), req("http://api/apps", `{"kind":"beta"}`)},
			live: req("http://api/apps", `{"kind":"beta"}`), body: false, want: 0},
		{name: "its URL still tells",
			recs: []*models.HTTPReq{req("http://api/apps?page=1", `{"kind":"beta"}`), req("http://api/apps?page=2", `{"kind":"alpha"}`), req("http://api/apps?page=3", `{"kind":"beta"}`)},
			live: req("http://api/apps?page=2", `{"kind":"beta"}`), body: false, want: 1},
		{name: "a value of a form part tells",
			recs: []*models.HTTPReq{req("http://api/apps", "", "alpha"), req("http://api/apps", "", "beta")},
			live: req("http://api/apps", "", "beta"), body: true, want: 1},
		{name: "a form keploy has not read tells nothing",
			recs: []*models.HTTPReq{req("http://api/apps", "", "alpha"), req("http://api/apps", "", "beta")},
			live: req("http://api/apps", "", "beta"), body: false, want: 0},
		{name: "a header's word tells nothing",
			recs: []*models.HTTPReq{hdr(req("http://api/apps", ""), "alpha"), hdr(req("http://api/apps", ""), "gamma")},
			live: hdr(req("http://api/apps", ""), "gamma"), body: true, want: 0},
		{name: "the most words win over the first that holds one",
			recs: []*models.HTTPReq{req("http://api/apps", `{"kind":"alpha","size":"big"}`), req("http://api/apps", `{"kind":"beta","size":"big"}`), req("http://api/apps", `{"kind":"beta","size":"small"}`)},
			live: req("http://api/apps", `{"kind":"beta","size":"small"}`), body: true, want: 2},
		{name: "a word counts once however often its recording holds it",
			recs: []*models.HTTPReq{req("http://api/apps", `{"kind":"alpha alpha alpha","size":"big"}`), req("http://api/apps", `{"kind":"beta","size":"small"}`), req("http://api/apps", `{"kind":"gamma","size":"big"}`)},
			live: req("http://api/apps", `{"kind":"alpha beta","size":"small"}`), body: true, want: 1},
		{name: "a paired request is not among those left",
			recs: []*models.HTTPReq{req("http://api/apps", `{"kind":"beta","size":"small"}`), req("http://api/apps", `{"kind":"alpha","size":"big"}`), req("http://api/apps", `{"kind":"gamma","size":"big"}`)},
			used: []bool{true, false, false},
			live: req("http://api/apps", `{"kind":"beta","size":"small"}`), body: true, want: 1},
		{name: "one left pairs whatever it holds",
			recs: []*models.HTTPReq{req("http://api/apps", `{"kind":"beta"}`), req("http://api/apps", `{"kind":"alpha"}`)},
			used: []bool{true, false},
			live: req("http://api/apps", `{"kind":"beta"}`), body: true, want: 1},
		{name: "a word within the first 64 KiB of a body tells",
			recs: []*models.HTTPReq{req("http://api/apps", far[:len(far)-6]+" alpha"), req("http://api/apps", far[:len(far)-6]+"  beta")},
			live: req("http://api/apps", far[:len(far)-6]+"  beta"), body: true, want: 1},
		{name: "one past them does not: by order",
			recs: []*models.HTTPReq{req("http://api/apps", far+"alpha"), req("http://api/apps", far+"beta")},
			live: req("http://api/apps", far+"beta"), body: true, want: 0},
		{name: "none left of the call",
			recs: []*models.HTTPReq{req("http://api/apps", ""), req("http://api/apps", "")},
			used: []bool{true, true},
			live: req("http://api/apps", ""), body: true, want: -1},
	} {
		var cases []ClientCase
		for i, r := range c.recs {
			cases = append(cases, ClientCase{Name: strconv.Itoa(i), Req: r})
		}
		if got, ok := PairCase(cases, c.used, c.live, c.body); got != c.want || ok != (c.want >= 0) {
			t.Errorf("%s: paired with %d (%v); want %d", c.name, got, ok, c.want)
		}
	}
}

// TestAPairingWordIsAWholeValue: a word requests pair by ends at a space, a
// control byte and each mark that parts values, and at nothing else, so a
// value a run made stays one word.
func TestAPairingWordIsAWholeValue(t *testing.T) {
	var got []string
	pairWords(&models.HTTPReq{URL: "u:1", Body: "a\tb\nc\"d'e`f,g;h&i=j?k/l{m}n[o]p(q)r<s>t\x00v 2026-11-09T10:00:01.5Z n_1+x%20y@z:w|!~*#$^"}, true, func(w string) { got = append(got, w) })
	if want := "u:1 a b c d e f g h i j k l m n o p q r s t v 2026-11-09T10:00:01.5Z n_1+x%20y@z:w|!~*#$^"; strings.Join(got, "\n") != strings.ReplaceAll(want, " ", "\n") {
		t.Fatalf("words %q; want those of %q", got, want)
	}
}

// TestTwoWorkersRequestsArrivingSwappedAreEachPlacedAtTheirOwn: the learn
// step is given each of two requests of one call, sent by two workers of
// the test in the other order than in its recording, as a place of the
// recorded request that holds its fixed word; a body still coming pairs by
// order, as the replay's bind does.
func TestTwoWorkersRequestsArrivingSwappedAreEachPlacedAtTheirOwn(t *testing.T) {
	at := time.Date(2026, 11, 9, 7, 1, 42, 0, time.UTC)
	req := func(body string) *models.HTTPReq {
		return &models.HTTPReq{Method: "POST", URL: "http://api/apps", Body: body, Timestamp: at}
	}
	cases := []ClientCase{{Name: "post-apps-1", Req: req(`{"kind":"alpha","name":"n-111"}`)}, {Name: "post-apps-2", Req: req(`{"kind":"beta","name":"n-222"}`)}}
	tr := NewTracker()
	tr.Begin()
	tr.SetCases(cases)
	if !tr.Incoming(req(`{"kind":"beta","name":"n-888"}`), "", at) || !tr.Incoming(req(`{"kind":"alpha","name":"n-999"}`), "", at.Add(time.Millisecond)) {
		t.Fatal("a request of the run paired with none")
	}
	placed := tr.Placed().Earlier
	if len(placed) != 2 || placed[0].Mock != "test case post-apps-2" || placed[1].Mock != "test case post-apps-1" {
		t.Fatalf("placed %+v; want the first at post-apps-2 and the second at post-apps-1", placed)
	}
	tr.Begin()
	tr.SetCases(cases)
	if tr.IncomingComing(req(""), at) == nil {
		t.Fatal("a request whose body is coming paired with none")
	}
	if placed := tr.Placed().Earlier; len(placed) != 1 || placed[0].Mock != "test case post-apps-1" {
		t.Fatalf("a request whose body is coming placed %+v; want post-apps-1, the first in order", placed)
	}
	tr.Begin()
	tr.SetCases(cases)
	if !tr.Incoming(req(`{"kind":"beta","name":"n-888"}`), "its body is larger than keploy reads", at) {
		t.Fatal("a request keploy could not read paired with none")
	}
	if placed := tr.Placed().Earlier; len(placed) != 1 || placed[0].Mock != "test case post-apps-1" {
		t.Fatalf("a request keploy could not read placed %+v; want post-apps-1, the first in order", placed)
	}
}
