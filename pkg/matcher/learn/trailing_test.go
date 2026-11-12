package learn

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// trailSent is the place test-1's own call left the run (Earlier.Run): its
// request sent id r in the recording and f in round F, the id test-1
// minted. The call decided is test-2's, which carries it.
func trailSent(r, f string) []Earlier {
	sent := oSent(r, f)
	sent[0].Own, sent[0].Run = false, true
	return sent
}

func trailInput(r, live string, trail Trail) Input {
	in := oInput("body.user", r, live, trailSent(r, "Zq81Lm0XpVb4"), nil)
	in.Trails = trail
	return in
}

// A call keploy record marked trailing test-1 (it carries the id test-1's
// own call sent, and was recorded after test-1 ended) reads the place
// test-1's call left the run as its own: the id is bound there, as the
// run's variable (From.Run), which a replay keeps past test-1's end, and
// the line names the test the call trails.
func TestATrailingCallBindsTheIDItsTestSentAsTheRunsVariable(t *testing.T) {
	const r, f = "rAHcEraS7Qx1", "Zq81Lm0XpVb4"
	d := oWord(t, Decide(trailInput(r, f, Trail{Test: "test-1", Mocks: []string{"mock-9", "mock-0"}})), r, f)
	if d.Outcome != OutcomeValue || d.From.Mock != "mock-0" || d.From.Path != "body.user" || !d.From.Run || d.From.InStartUp {
		t.Fatalf("the trailing call's id: %s, from %+v; want a value bound at mock-0 body.user as the run's variable", oDescribe([]Decision{d}), d.From)
	}
	if !strings.Contains(d.Line, "sent in test-1, which this call trails") {
		t.Errorf("the line %q does not name the test the call trails", d.Line)
	}
}

// Every other call stays refused as main's flip rules a value another test
// sent: with no trail, with a trail that names no test, and with a trail of
// another test (the place is not one of its recordings). The refusal names
// test-1's place and the mappings edits.
func TestAValueAnotherTestSentStaysRefusedUnlessTheCallTrailsThatTest(t *testing.T) {
	const r, f = "rAHcEraS7Qx1", "Zq81Lm0XpVb4"
	for _, tc := range []struct {
		name  string
		trail Trail
	}{
		{"no trail", Trail{}},
		{"a trail that names no test", Trail{Mocks: []string{"mock-0"}}},
		{"a trail of another test", Trail{Test: "test-0", Mocks: []string{"mock-7"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := oWord(t, Decide(trailInput(r, f, tc.trail)), r, f)
			oRefused(t, tc.name, d, RefusalAnotherScope, "mock-0", r)
			if !oEdits(d.Refusal, EditMoveToMocks, EditMoveToStartup, EditNoiseAtField) {
				t.Errorf("no mappings edit to act on: %+v", d.Refusal)
			}
			if strings.Contains(d.Line, "trails") {
				t.Errorf("the line %q names a trail the call does not have", d.Line)
			}
		})
	}
}

// A trailing call that carries another id than the one its test sent in
// round F follows nothing of the test's: the place sent the recorded id
// whole and round F's other value did not follow, so the word is refused,
// never bound or learned fresh.
func TestATrailingCallCarryingAnotherIDIsRefused(t *testing.T) {
	const r, other = "rAHcEraS7Qx1", "Pk20Wd7YcRt5"
	d := oWord(t, Decide(trailInput(r, other, Trail{Test: "test-1", Mocks: []string{"mock-0"}})), r, other)
	if d.Outcome != OutcomeRefused {
		t.Fatalf("a trailing call with another id: %s; want refused", oDescribe([]Decision{d}))
	}
}

// Only a place an earlier test's call left the run is the trailed test's:
// an exchange the call's own test was served (Own, no Run) is bound as the
// test's own variable, trailed or not, and a start-up's place as the
// start-up's.
func TestATrailReadsOnlyTheRunsPlacesAsTheTrailedTests(t *testing.T) {
	const r, f = "rAHcEraS7Qx1", "Zq81Lm0XpVb4"
	trail := Trail{Test: "test-1", Mocks: []string{"mock-0"}}
	own := oInput("body.user", r, f, oSent(r, f), nil)
	own.Trails = trail
	if d := oWord(t, Decide(own), r, f); d.Outcome != OutcomeValue || d.From.Run || strings.Contains(d.Line, "trails") {
		t.Fatalf("the test's own exchange: %s, from %+v; want the test's own variable", oDescribe([]Decision{d}), d.From)
	}
	startUp := oSent(r, f)
	startUp[0].Own, startUp[0].Run, startUp[0].StartUp, startUp[0].At = false, true, true, oAt.Add(-time.Minute)
	in := oInput("body.user", r, f, startUp, nil)
	in.Trails = trail
	if d := oWord(t, Decide(in), r, f); d.Outcome != OutcomeValue || !d.From.InStartUp || strings.Contains(d.Line, "trails") {
		t.Fatalf("the start-up's exchange: %s, from %+v; want the start-up's variable", oDescribe([]Decision{d}), d.From)
	}
}

// Where mappings.yaml places the exchange (Input.Scope), its list decides:
// one it lists in the call's own test is the test's own variable, trailed
// or not; one it lists in the trailed test's that the call's own test was
// served (no place an earlier test's call left the run) stays refused, as
// a recording of another test round F served the call's test.
func TestATrailReadsMappingsPlaceOfTheExchange(t *testing.T) {
	const r, f = "rAHcEraS7Qx1", "Zq81Lm0XpVb4"
	trail := Trail{Test: "test-1", Mocks: []string{"mock-0"}}
	scope := func(s Scope) func(string) Scope { return func(string) Scope { return s } }
	in := trailInput(r, f, trail)
	in.Scope = scope(ScopeTest("test-2"))
	if d := oWord(t, Decide(in), r, f); d.Outcome != OutcomeValue || d.From.Run || strings.Contains(d.Line, "trails") {
		t.Fatalf("listed in the call's own test: %s, from %+v; want the test's own variable", oDescribe([]Decision{d}), d.From)
	}
	in = oInput("body.user", r, f, oSent(r, f), nil)
	in.Trails, in.Scope = trail, scope(ScopeTest("test-1"))
	oRefused(t, "the trailed test's recording served in the call's test", oWord(t, Decide(in), r, f), RefusalAnotherScope, "mock-0", r)
}

// A trailing call the round made inside the trailed test's own window (the
// application ran its clean-up before the test's last answer, sooner than
// it did at record) reads the place the test was served there as its own,
// as every call of the test does, and binds the id there as the run's
// variable, never the test's own: a replay where the call lands after the
// test ended binds it as well. The test's own place of a recording its
// list does not hold stays the test's own variable, and a trailing call
// landing in another test's window gets the run's variable only from a
// place the trailed test left the run (TestATrailReadsOnlyTheRunsPlacesAsTheTrailedTests).
func TestATrailingCallInItsTestsOwnWindowBindsTheRunsVariable(t *testing.T) {
	const r, f = "rAHcEraS7Qx1", "Zq81Lm0XpVb4"
	in := oInput("body.user", r, f, oSent(r, f), nil)
	in.Test, in.Trails = "test-1", Trail{Test: "test-1", Mocks: []string{"mock-0"}}
	d := oWord(t, Decide(in), r, f)
	if d.Outcome != OutcomeValue || d.From.Mock != "mock-0" || !d.From.Run || d.From.InStartUp || !strings.Contains(d.Line, "sent in test-1, which this call trails") {
		t.Fatalf("the trailing call in test-1's own window: %s, from %+v; want a value bound at mock-0 as the run's variable, naming the trail", oDescribe([]Decision{d}), d.From)
	}
	in.Trails = Trail{Test: "test-1", Mocks: []string{"mock-7"}}
	if d := oWord(t, Decide(in), r, f); d.Outcome != OutcomeValue || d.From.Run || strings.Contains(d.Line, "trails") {
		t.Fatalf("a place the trailed test's list does not hold: %s, from %+v; want the test's own variable", oDescribe([]Decision{d}), d.From)
	}
	in.Trails = Trail{}
	if d := oWord(t, Decide(in), r, f); d.Outcome != OutcomeValue || d.From.Run || strings.Contains(d.Line, "trails") {
		t.Fatalf("no trail: %s, from %+v; want the test's own variable", oDescribe([]Decision{d}), d.From)
	}
	// A start-up's place stays the start-up's, and one mappings.yaml lists
	// in another test stays refused as that test's, in the trailed test's
	// own window too.
	trail := Trail{Test: "test-1", Mocks: []string{"mock-0"}}
	startUp := oSent(r, f)
	startUp[0].Own, startUp[0].Run, startUp[0].StartUp, startUp[0].At = false, true, true, oAt.Add(-time.Minute)
	in = oInput("body.user", r, f, startUp, nil)
	in.Test, in.Trails = "test-1", trail
	if d := oWord(t, Decide(in), r, f); d.Outcome != OutcomeValue || !d.From.InStartUp || strings.Contains(d.Line, "trails") {
		t.Fatalf("the start-up's exchange in test-1's window: %s, from %+v; want the start-up's variable, naming no trail", oDescribe([]Decision{d}), d.From)
	}
	in = oInput("body.user", r, f, oSent(r, f), nil)
	in.Test, in.Trails = "test-1", trail
	in.Scope = func(string) Scope { return ScopeTest("test-0") }
	oRefused(t, "an exchange mappings.yaml lists in test-0, in test-1's window", oWord(t, Decide(in), r, f), RefusalAnotherScope, "mock-0", r)
}

// trailInsert is test-1's insert of the id it minted, recorded at at: its
// request carries the id (an app inserting the row it made), its answer
// none.
func trailInsert(id string, at time.Time) *models.Mock {
	return &models.Mock{Name: "mock-31", Kind: models.HTTP, Spec: models.MockSpec{
		Metadata:         map[string]string{"operation": "POST", "type": "HTTP_CLIENT"},
		HTTPReq:          &models.HTTPReq{Method: "POST", ProtoMajor: 1, ProtoMinor: 1, URL: "/apps", Header: map[string]string{"Content-Type": "application/json"}, Body: `{"appId":"` + id + `"}`},
		HTTPResp:         &models.HTTPResp{StatusCode: 201, Header: map[string]string{"Content-Type": "application/json"}, Body: `{"ok":true}`},
		ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
	}}
}

// trailRawInput is a trailing call carrying live where the recording held
// r, decided in test's window, whose one place is test-1's insert of the id
// (trailInsert): round F served it with the id f in its request (its view),
// in the call's own test, or only in an older test (older).
func trailRawInput(r, f, live, test string, older bool, trail Trail) Input {
	dc := oDrifts()
	at := oAt.Add(-time.Minute)
	x := dc.RawOf(trailInsert(r, at), true).ServedAs(3).ServedLive(dc.RawOf(trailInsert(f, at), true))
	if older {
		x = x.ServedInAnOlderTest()
	}
	in := oInput("body.app_id", r, live, nil, []Raw{x})
	in.Drifts, in.Test, in.Trails = dc, test, trail
	return in
}

// The place a trailed test's call sent its minted id reaches the learn step
// as that test's recording, with round F's view of it, where keploy served
// the call from its mappings (no exchange of it leaf by leaf, so the test's
// end left the run no place of it): in the test's own window, where the
// round ran the clean-up sooner, and in a later test's window, where F gave
// the recording only in the trailed test, an older one. A call trailing that
// test binds the id there as the run's variable, which a replay keeps past
// the test's end, and the line names the trail.
func TestATrailingCallBindsTheIDAtItsTestsRecording(t *testing.T) {
	const r, f = "ced721e3-5d3a-4fb3-9283-0b1ecc742299", "4db9566f-0893-4ade-bf9c-97361f46945e"
	trail := Trail{Test: "test-1", Mocks: []string{"mock-9", "mock-31"}}
	for _, tc := range []struct {
		name  string
		test  string
		older bool
	}{
		{"in the trailed test's own window", "test-1", false},
		{"in a later test's window", "test-2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := oWord(t, Decide(trailRawInput(r, f, f, tc.test, tc.older, trail)), "ced721e3", "4db9566f")
			if d.Outcome != OutcomeValue || d.From.Mock != "mock-31" || !d.From.Run || d.From.InStartUp {
				t.Fatalf("%s: %s, from %+v; want a value bound at mock-31 as the run's variable", tc.name, oDescribe([]Decision{d}), d.From)
			}
			if !strings.Contains(d.Line, "sent in test-1, which this call trails") {
				t.Errorf("the line %q does not name the test the call trails", d.Line)
			}
		})
	}
}

// Every other reading of such a recording stays as it was: with no trail, a
// trail that names no test or a trail of another test, the recording is the
// test's own variable in its own window and refused as another test's in a
// later one (the pinned "a value from another test is refused"); a
// recording of the trailed test's list that round F served in the call's
// own test, a later one, is that test's own; a start-up recording is the
// start-up's; one mappings.yaml lists in another test stays refused; and a
// trailing call carrying another id than the one F's view shows binds
// nothing.
func TestATrailReadsATestsRecordingOnlyWhereThatTestLeftIt(t *testing.T) {
	const r, f = "ced721e3-5d3a-4fb3-9283-0b1ecc742299", "4db9566f-0893-4ade-bf9c-97361f46945e"
	trail := Trail{Test: "test-1", Mocks: []string{"mock-31"}}
	for _, no := range []struct {
		name  string
		trail Trail
	}{
		{"no trail", Trail{}},
		{"a trail that names no test", Trail{Mocks: []string{"mock-31"}}},
		{"a trail of another test", Trail{Test: "test-0", Mocks: []string{"mock-7"}}},
	} {
		t.Run(no.name, func(t *testing.T) {
			d := oWord(t, Decide(trailRawInput(r, f, f, "test-1", false, no.trail)), "ced721e3", "4db9566f")
			if d.Outcome != OutcomeValue || d.From.Mock != "mock-31" || d.From.Run || strings.Contains(d.Line, "trails") {
				t.Fatalf("in the test's own window: %s, from %+v; want the test's own variable", oDescribe([]Decision{d}), d.From)
			}
			oRefused(t, no.name+", in a later window", oWord(t, Decide(trailRawInput(r, f, f, "test-2", true, no.trail)), "ced721e3", "4db9566f"), RefusalAnotherScope, "mock-31", "ced721e3")
		})
	}
	// Served in the call's own test, a later one than the trailed test.
	if d := oWord(t, Decide(trailRawInput(r, f, f, "test-2", false, trail)), "ced721e3", "4db9566f"); d.Outcome != OutcomeValue || d.From.Run || strings.Contains(d.Line, "trails") {
		t.Fatalf("served in the call's own later test: %s, from %+v; want the test's own variable", oDescribe([]Decision{d}), d.From)
	}
	// The start-up's, as mappings.yaml places it.
	in := trailRawInput(r, f, f, "test-1", false, trail)
	in.Scope = func(string) Scope { return ScopeStartup }
	if d := oWord(t, Decide(in), "ced721e3", "4db9566f"); d.Outcome != OutcomeValue || !d.From.InStartUp || strings.Contains(d.Line, "trails") {
		t.Fatalf("the start-up's recording: %s, from %+v; want the start-up's variable, naming no trail", oDescribe([]Decision{d}), d.From)
	}
	// Listed in another test, in the trailed test's own window.
	in = trailRawInput(r, f, f, "test-1", false, trail)
	in.Scope = func(string) Scope { return ScopeTest("test-0") }
	oRefused(t, "a recording mappings.yaml lists in test-0", oWord(t, Decide(in), "ced721e3", "4db9566f"), RefusalAnotherScope, "mock-31", "ced721e3")
	// In a later window, mappings.yaml's list decides: one it lists in the
	// trailed test is the trailed test's place, one it lists in another
	// test stays refused as that test's.
	in = trailRawInput(r, f, f, "test-2", true, trail)
	in.Scope = func(string) Scope { return ScopeTest("test-1") }
	if d := oWord(t, Decide(in), "ced721e3", "4db9566f"); d.Outcome != OutcomeValue || !d.From.Run || !strings.Contains(d.Line, "sent in test-1, which this call trails") {
		t.Fatalf("listed in the trailed test, in a later window: %s, from %+v; want the run's variable, naming the trail", oDescribe([]Decision{d}), d.From)
	}
	in.Scope = func(string) Scope { return ScopeTest("test-0") }
	oRefused(t, "listed in test-0, in a later window", oWord(t, Decide(in), "ced721e3", "4db9566f"), RefusalAnotherScope, "mock-31", "ced721e3")
	// A recording of the list read as another test's (Raw.OfAnotherTest) in
	// the trailed test's own window stays refused as one, and one round F
	// never served shows nothing of what F gave, trailed or not.
	dc := oDrifts()
	at := oAt.Add(-time.Minute)
	elsewhere := dc.RawOf(trailInsert(r, at), true).ServedAs(3).ServedLive(dc.RawOf(trailInsert(f, at), true)).OfAnotherTest()
	in = oInput("body.app_id", r, f, nil, []Raw{elsewhere})
	in.Drifts, in.Test, in.Trails = dc, "test-1", trail
	oRefused(t, "a recording read as another test's, in the trailed test's window", oWord(t, Decide(in), "ced721e3", "4db9566f"), RefusalAnotherScope, "mock-31", "ced721e3")
	in = oInput("body.app_id", r, f, nil, []Raw{dc.RawOf(trailInsert(r, at), false).ServedInAnOlderTest()})
	in.Drifts, in.Test, in.Trails = dc, "test-2", trail
	oRefused(t, "a recording round F never served, in a later window", oWord(t, Decide(in), "ced721e3", "4db9566f"), RefusalUnknown, "mock-31", "ced721e3")
	// Another id than F's view shows, in a later window.
	const other = "9a8b7c6d-1e2f-4a3b-8c4d-5e6f7a8b9c0d"
	if d := oWord(t, Decide(trailRawInput(r, f, other, "test-2", true, trail)), "ced721e3", "9a8b7c6d"); d.Outcome == OutcomeValue && d.From.Mock == "mock-31" {
		t.Fatalf("a trailing call with another id: %s, from %+v; want nothing bound at mock-31", oDescribe([]Decision{d}), d.From)
	}
}

// The recording's own documents: TestAuthorizationDirectives creates an app,
// which the application mints the id of and inserts (mock-807, a Mongo
// insert into app_name_reservations, appId), and its client sends the id
// back in a GraphQL query (test case post-query-15, variables.appID). The
// application's worker deletes the app's sessions by that id after the test
// answered (mock-841, a delete on uiSessions, q.app_id): keploy record
// marked that worker: recording trailing the test. In record's full round
// the delete lands in the test's own window, after the query. directivesCall
// is that delete, its id recorded rID and fID in round F, mock-807 the
// test's own place (served in its window) and post-query-15 the client's.
func directivesCall(t *testing.T, rID, fID string) Input {
	t.Helper()
	rAt := time.Date(2026, 11, 10, 13, 27, 42, 979528237, time.UTC)
	gap := 3*time.Minute + 41*time.Second
	query := func(id string, at time.Time) *models.HTTPReq {
		return &models.HTTPReq{Method: "POST", ProtoMajor: 1, ProtoMinor: 1, URL: "http://localhost:8083/query", Timestamp: at,
			Header: map[string]string{"Content-Type": "application/json", "Host": "localhost:8083"},
			Body:   `{"query":"query GetSchemaCoverage($appID: String!, $baseURL: String!, $schema: String!, $branchId: String) {\n  getSchemaCoverage(appID: $appID, baseURL: $baseURL, schema: $schema, branchId: $branchId) {\n    coveragePercentage\n  }\n}\n","variables":{"appID":"` + id + `","baseURL":"http://localhost:8083","schema":"type Query { ping: String }"}}`}
	}
	qAt := time.Date(2026, 11, 10, 13, 27, 42, 948756071, time.UTC)
	insertAt := time.Date(2026, 11, 10, 13, 27, 42, 945562684, time.UTC)
	insert := Earlier{Mock: "mock-807", At: insertAt, Own: true, Payload: oPayload("mock-807", insertAt), Seen: []Seen{
		{Side: Request, Leaf: oText("body.docs.msgs.0.appId", rID), Live: fID, Place: "body.docs.msgs.0.appId"},
	}}
	earlier := append(clientPlaced(t, "post-query-15", query(rID, qAt), query(fID, qAt.Add(gap)), qAt.Add(gap)), insert)
	in := oInput("body.docs.msgs.0.q.app_id", rID, fID, earlier, nil)
	in.At, in.Arrived = rAt, rAt.Add(gap)
	in.Test, in.Mock = "TestAuthorizationDirectives", "mock-841"
	return in
}

// A call no test lists (a worker: recording keploy record marked trailing a
// test) has no client test's request among its places: a value bound there
// is a variable of the test whose list holds the call, and none holds this
// one, so learning the delete's id there failed the whole record round ("a
// variable of mock-841 bound at test case post-query-15, from a recording
// no test lists"). It reads its words as main does: each word of the id
// stays bound at the trailed test's insert, the run's variable, which a
// replay keeps past the test's end. The test's own call (no trail) still
// binds the id at the client's request, which comes first.
func TestATrailingCallIsNoCallOfItsTestsClientRequests(t *testing.T) {
	const rID, fID = "961ad63b-9151-4789-a680-acd90efec8f2", "0b7d4c1e-2f6a-4a39-8e15-d3c9a7b5e204"
	in := directivesCall(t, rID, fID)
	in.Trails = Trail{Test: "TestAuthorizationDirectives", Mocks: []string{"mock-807"}}
	ds := Decide(in)
	if len(ds) != 5 {
		t.Fatalf("the trailing delete's id: %s; want its five words decided", oDescribe(ds))
	}
	for _, d := range ds {
		if d.Outcome != OutcomeValue || d.From.Mock != "mock-807" || d.From.Path != "body.docs.msgs.0.appId" || !d.From.Run || d.From.Client || !strings.Contains(d.Line, "which this call trails") {
			t.Fatalf("the trailing delete's word %q -> %q: %s, from %+v; want it bound at mock-807's appId as the run's variable, no client's request", d.Recorded, d.Full, oDescribe([]Decision{d}), d.From)
		}
	}
	// Only the client's requests leave its places: a value the test gave
	// keploy (Tracker.GiveValue) stays one, as on main.
	gave := directivesCall(t, rID, fID)
	gave.Trails = in.Trails
	gave.Earlier = append(gave.Earlier, TestGiven("test TestAuthorizationDirectives", "given:appID", rID, fID))
	for _, d := range Decide(gave) {
		if !strings.Contains(d.Line, "given:appID") || d.From.Client {
			t.Fatalf("the trailing delete's word %q -> %q beside a value the test gave: %s, from %+v; want it read with the given, no client's request", d.Recorded, d.Full, oDescribe([]Decision{d}), d.From)
		}
	}
	own := directivesCall(t, rID, fID)
	for _, d := range Decide(own) {
		if d.Outcome != OutcomeValue || d.From.Mock != "test case post-query-15" || !d.From.Client {
			t.Fatalf("the test's own call's word %q -> %q: %s, from %+v; want it bound at the client's request", d.Recorded, d.Full, oDescribe([]Decision{d}), d.From)
		}
	}
	// Where mappings.yaml places the call (Input.Scope), its list decides:
	// the test's own list keeps the client's request a place; worker:, the
	// start-up's and another test's list do not.
	for _, tc := range []struct {
		scope  Scope
		client bool
	}{
		{ScopeTest("TestAuthorizationDirectives"), true},
		{ScopeWorker, false},
		{ScopeStartup, false},
		{ScopeTest("TestAppCRUD"), false},
	} {
		in := directivesCall(t, rID, fID)
		in.Scope = func(mock string) Scope {
			if mock == "mock-841" {
				return tc.scope
			}
			return ScopeTest("TestAuthorizationDirectives")
		}
		for _, d := range Decide(in) {
			if tc.client && (!d.From.Client || d.From.Mock != "test case post-query-15") || !tc.client && (d.From.Client || d.From.Mock != "mock-807") || d.Outcome != OutcomeValue {
				t.Fatalf("the call listed in %q, word %q -> %q: %s, from %+v; want bound at the client's request: %v", tc.scope, d.Recorded, d.Full, oDescribe([]Decision{d}), d.From, tc.client)
			}
		}
	}
}
