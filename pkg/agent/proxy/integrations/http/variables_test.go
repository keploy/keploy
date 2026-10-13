package http

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
)

// varDb is the mock store of a replay under the proven switch: per-test mocks
// consumed on match, reusable mocks kept, a stateful cursor, and the replay's
// variable table.
type varDb struct {
	mockMemDb
	perTest []*models.Mock
	session []*models.Mock
	table   variables.Table
}

func (d *varDb) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	return append([]*models.Mock(nil), d.perTest...), nil
}
func (d *varDb) GetSessionMocks() ([]*models.Mock, error) {
	return append([]*models.Mock(nil), d.session...), nil
}
func (d *varDb) DeleteFilteredMock(m models.Mock) bool {
	for i, x := range d.perTest {
		if x.Name == m.Name {
			d.perTest = append(d.perTest[:i:i], d.perTest[i+1:]...)
			return true
		}
	}
	return false
}
func (d *varDb) UpdateUnFilteredMock(_, _ *models.Mock) bool { return true }
func (d *varDb) Variables() *variables.Table                 { return &d.table }

var recordedAt = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

// varMock is a recorded HTTP call; seq orders the recording.
func varMock(name, method, rawURL, reqBody string, status int, respBody string, vars models.MockVariables) *models.Mock {
	m := &models.Mock{
		Name: name,
		Kind: models.Kind(models.HTTP),
		Spec: models.MockSpec{
			HTTPReq: &models.HTTPReq{
				Method: models.Method(method),
				URL:    rawURL,
				Header: map[string]string{"Content-Type": "application/json"},
				Body:   reqBody,
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: status,
				Header:     map[string]string{"Content-Type": "application/json"},
				Body:       respBody,
			},
			Variables: vars,
		},
	}
	if u, err := url.Parse(rawURL); err == nil && len(u.Query()) > 0 {
		m.Spec.HTTPReq.URLParams = map[string]string{}
		for k := range u.Query() {
			m.Spec.HTTPReq.URLParams[k] = u.Query().Get(k)
		}
	}
	m.TestModeInfo.Lifetime = models.LifetimePerTest
	return m
}

func at(m *models.Mock, seq int) *models.Mock {
	m.Spec.ReqTimestampMock = recordedAt.Add(time.Duration(seq) * time.Second)
	return m
}

func reusable(m *models.Mock) *models.Mock {
	m.TestModeInfo.Lifetime = models.LifetimeSession
	m.TestModeInfo.Consume = models.ConsumeCursorSaturate
	return m
}

func liveReq(method, rawURL, body string) *req {
	u, _ := url.Parse(rawURL)
	return &req{
		method: method,
		url:    u,
		header: http.Header{"Content-Type": []string{"application/json"}},
		body:   []byte(body),
		raw:    []byte(body),
	}
}

func bindV(recorded, bind string, answer ...string) models.MockVariable {
	return models.MockVariable{Recorded: recorded, Bind: bind, Answer: answer}
}

func useV(recorded string, use []string, answer ...string) models.MockVariable {
	return models.MockVariable{Recorded: recorded, Use: use, Answer: answer}
}

// createAndReadBack: the create binds orderId where the app first sends it to
// the dependency (answered with it); the read-back asks for it and is
// answered with it.
func createAndReadBack(recorded string) []*models.Mock {
	return []*models.Mock{
		at(varMock("mock-1", "POST", "http://store/orders", `{"id":"`+recorded+`","item":"pen"}`, 201, `{"id":"`+recorded+`"}`,
			models.MockVariables{"orderId": bindV(recorded, "body.id", "body.id")}), 1),
		at(varMock("mock-2", "GET", "http://store/orders/"+recorded, "", 200, `{"id":"`+recorded+`","item":"pen","n":1}`,
			models.MockVariables{"orderId": useV(recorded, []string{"url.path[1]"}, "body.id")}), 2),
	}
}

func matchVar(t *testing.T, h *HTTP, db *varDb, r *req) (bool, *models.Mock, *matchDiag) {
	t.Helper()
	ok, stub, diag, err := h.match(context.Background(), r, db, nil, nil, nil, true, false, false, true, true)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	return ok, stub, diag
}

func wantProblem(t *testing.T, ok bool, diag *matchDiag, why string) *varProblem {
	t.Helper()
	if ok {
		t.Fatal("the call was answered; want a miss with the variable's evidence")
	}
	if diag == nil || diag.variable == nil || diag.phase != models.MatchPhaseVariable {
		t.Fatalf("want a variable miss, got %+v", diag)
	}
	if !strings.Contains(diag.variable.why, why) {
		t.Fatalf("evidence %q does not say %q", diag.variable.why, why)
	}
	return diag.variable
}

func TestVariables_BindThenUseAndAnswer(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{perTest: createAndReadBack("ord-rec-1")}

	ok, stub, _ := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"ord-live-9","item":"pen"}`))
	if !ok || stub.Name != "mock-1" || stub.Spec.HTTPResp.Body != `{"id":"ord-live-9"}` {
		t.Fatalf("the create should be answered by mock-1 with this run's id: ok=%v stub=%+v", ok, stub)
	}
	if b, bound := db.table.Lookup("orderId"); !bound || b.Value != "ord-live-9" || b.Recorded != "ord-rec-1" || b.Mock != "mock-1" || b.Place != "body.id" {
		t.Fatalf("orderId bound as %+v (%v)", b, bound)
	}
	ok, stub, _ = matchVar(t, h, db, liveReq("GET", "/orders/ord-live-9", ""))
	if !ok || stub.Name != "mock-2" {
		t.Fatalf("the read-back of this run's id should be answered by mock-2: ok=%v stub=%v", ok, stub)
	}
	if want := `{"id":"ord-live-9","item":"pen","n":1}`; stub.Spec.HTTPResp.Body != want {
		t.Fatalf("answered %s, want %s", stub.Spec.HTTPResp.Body, want)
	}
	if len(db.perTest) != 0 {
		t.Fatalf("both mocks should be consumed, left %d", len(db.perTest))
	}
}

func TestVariables_AUseOfAnotherValueIsAMissWithEvidence(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct{ name, asked string }{
		{"another id", "ord-other-3"},
		{"the recorded id (a stale constant)", "ord-rec-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHTTP()
			db := &varDb{perTest: createAndReadBack("ord-rec-1")}
			if ok, _, _ := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"ord-live-9","item":"pen"}`)); !ok {
				t.Fatal("the create should match")
			}
			ok, _, diag := matchVar(t, h, db, liveReq("GET", "/orders/"+tc.asked, ""))
			p := wantProblem(t, ok, diag, "this call sent")
			if p.mock != "mock-2" || p.name != "orderId" || p.place != "url.path[1]" || p.expected != "ord-live-9" || p.actual != tc.asked {
				t.Fatalf("evidence %+v", p)
			}
			rep := h.buildHTTPMismatchReport(makeReq("GET", "/orders/"+tc.asked), nil, db, nil, nil, nil, diag)
			if rep.MatchPhase != models.MatchPhaseVariable || rep.ClosestMock != "mock-2" || len(rep.FieldDiffs) != 1 ||
				rep.FieldDiffs[0].Kind != models.DiffKindVariable || rep.FieldDiffs[0].Expected != "ord-live-9" || rep.FieldDiffs[0].Actual != tc.asked {
				t.Fatalf("report %+v", rep)
			}
			if !strings.Contains(rep.Diff, "orderId") || !strings.Contains(rep.Diff, "mock-1 body.id") || rep.NextSteps == "" {
				t.Fatalf("the report must name the variable and where it was bound: diff %q next %q", rep.Diff, rep.NextSteps)
			}
			if len(db.perTest) != 1 {
				t.Fatal("the read-back's mock must not be consumed by a call it did not answer")
			}
		})
	}
}

// A mismatch never falls through to a lenient pass: a recording of another
// order, which main's id-shaped URL pass would serve, does not answer it.
func TestVariables_AMismatchIsNotServedByALenientPass(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	mocks := createAndReadBack("4f2a9c71")
	other := at(varMock("mock-9", "GET", "http://store/orders/77d01b3e", "", 200, `{"id":"77d01b3e"}`, nil), 3)
	db := &varDb{perTest: append(mocks, other)}
	if ok, _, _ := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"9b0e66aa","item":"pen"}`)); !ok {
		t.Fatal("the create should match")
	}
	ok, stub, diag := matchVar(t, h, db, liveReq("GET", "/orders/5c3d2e1f", ""))
	if ok {
		t.Fatalf("a read-back of an id nobody made was answered by %s", stub.Name)
	}
	wantProblem(t, ok, diag, "this call sent")
	// An exact recording of this very call still answers it.
	ok, stub, _ = matchVar(t, h, db, liveReq("GET", "/orders/77d01b3e", ""))
	if !ok || stub.Name != "mock-9" {
		t.Fatalf("an exact recording of the call should answer it: ok=%v stub=%v", ok, stub)
	}
}

func TestVariables_AUseBeforeItsBindingCallIsAMiss(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{perTest: createAndReadBack("ord-rec-1")}
	ok, _, diag := matchVar(t, h, db, liveReq("GET", "/orders/ord-live-9", ""))
	wantProblem(t, ok, diag, "before the call that binds it")
}

func TestVariables_ABindingPlaceHoldsTheBoundValue(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	first := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"r1"}`, 201, `{}`,
		models.MockVariables{"orderId": bindV("r1", "body.id")}), 1)
	again := at(varMock("mock-2", "POST", "http://audit/log", `{"order":"r1"}`, 201, `{}`,
		models.MockVariables{"orderId": bindV("r1", "body.order")}), 2)
	db := &varDb{perTest: []*models.Mock{first, again}}
	if ok, _, _ := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"l1"}`)); !ok {
		t.Fatal("the create should match")
	}
	ok, _, diag := matchVar(t, h, db, liveReq("POST", "/log", `{"order":"l2"}`))
	p := wantProblem(t, ok, diag, "at a place that binds it")
	if p.expected != "l1" || p.actual != "l2" {
		t.Fatalf("evidence %+v", p)
	}
	if ok, _, _ := matchVar(t, h, db, liveReq("POST", "/log", `{"order":"l1"}`)); !ok {
		t.Fatal("a second binding place holding the bound value should match")
	}
}

// Two variables recorded as two values stay two values: the app sending the
// user's id where it sent the order's is caught at the binding place.
func TestVariables_AVariableCannotTakeAnothersValue(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	login := at(varMock("mock-1", "POST", "http://auth/login", `{"user":"u-rec"}`, 200, `{}`,
		models.MockVariables{"userId": bindV("u-rec", "body.user")}), 1)
	create := at(varMock("mock-2", "POST", "http://store/orders", `{"id":"o-rec"}`, 201, `{}`,
		models.MockVariables{"orderId": bindV("o-rec", "body.id")}), 2)
	db := &varDb{perTest: []*models.Mock{login, create}}
	if ok, _, _ := matchVar(t, h, db, liveReq("POST", "/login", `{"user":"u-live"}`)); !ok {
		t.Fatal("the login should match")
	}
	ok, _, diag := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"u-live"}`))
	p := wantProblem(t, ok, diag, "which is variable userId's value")
	if p.name != "orderId" || p.actual != "u-live" {
		t.Fatalf("evidence %+v", p)
	}
}

func TestVariables_AnEmptyValueBindsNothing(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, body := range []string{`{"id":null,"item":"pen"}`, `{"id":"","item":"pen"}`} {
		h := newHTTP()
		db := &varDb{perTest: createAndReadBack("ord-rec-1")}
		ok, _, diag := matchVar(t, h, db, liveReq("POST", "/orders", body))
		if body == `{"id":null,"item":"pen"}` {
			// null is another JSON type than the recorded string.
			wantProblem(t, ok, diag, "another JSON type")
			continue
		}
		wantProblem(t, ok, diag, "an empty value")
	}
}

func TestVariables_AVariablePlaceKeepsItsJSONType(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"123","item":"pen"}`, 201, `{}`,
		models.MockVariables{"orderId": bindV("123", "body.id")}), 1)
	db := &varDb{perTest: []*models.Mock{m}}
	ok, _, diag := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":456,"item":"pen"}`))
	p := wantProblem(t, ok, diag, "another JSON type")
	if p.expected != `"123"` || p.actual != "456" {
		t.Fatalf("evidence %+v", p)
	}
}

func TestVariables_TheRestOfTheCallIsComparedExactly(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{perTest: createAndReadBack("ord-rec-1")}
	for _, body := range []string{
		`{"id":"ord-live-9","item":"book"}`,      // a value drifted
		`{"id":"ord-live-9","item":"pen","x":1}`, // a field added
		`{"id":"ord-live-9"}`,                    // a field removed
	} {
		ok, stub, _ := matchVar(t, h, db, liveReq("POST", "/orders", body))
		if ok {
			t.Fatalf("%s was answered by %s", body, stub.Name)
		}
	}
	if _, bound := db.table.Lookup("orderId"); bound {
		t.Fatal("a call that was not answered bound orderId")
	}
}

func TestVariables_KnownNoiseIsNotCompared(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	mk := func() *models.Mock {
		m := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"r1","at":"2026-01-01T00:00:00Z","n":1}`, 201, `{}`,
			models.MockVariables{"orderId": bindV("r1", "body.id")}), 1)
		m.Spec.ReqBodyNoise = map[string][]string{"body.at": {}}
		return m
	}
	db := &varDb{perTest: []*models.Mock{mk()}}
	if ok, _, _ := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"l1","at":"2026-10-14T09:00:00Z","n":1}`)); !ok {
		t.Fatal("a drift under the mock's learned noise should still fit")
	}
	db = &varDb{perTest: []*models.Mock{mk()}}
	if ok, _, _ := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"l1","at":12,"n":1}`)); ok {
		t.Fatal("noise lets a value change, not its type")
	}
}

func TestVariables_URLQueryAndHeaderPlaces(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	tok := at(varMock("mock-1", "GET", "http://auth/session?nonce=n-rec", "", 200, `{"ok":true}`,
		models.MockVariables{"nonce": bindV("n-rec", "url.query.nonce")}), 1)
	call := at(varMock("mock-2", "GET", "http://api/items", "", 200, `{"items":[]}`,
		models.MockVariables{"nonce": useV("n-rec", []string{"header.X-Nonce"})}), 2)
	call.Spec.HTTPReq.Header["X-Nonce"] = "n-rec"
	db := &varDb{perTest: []*models.Mock{tok, call}}
	if ok, _, _ := matchVar(t, h, db, liveReq("GET", "/session?nonce=n-live", "")); !ok {
		t.Fatal("the query place should bind")
	}
	r := liveReq("GET", "/items", "")
	r.header.Set("X-Nonce", "n-other")
	ok, _, diag := matchVar(t, h, db, r)
	if p := wantProblem(t, ok, diag, "this call sent"); p.place != "header.X-Nonce" {
		t.Fatalf("evidence %+v", p)
	}
	r.header.Set("X-Nonce", "n-live")
	if ok, _, _ := matchVar(t, h, db, r); !ok {
		t.Fatal("a header holding the bound value should match")
	}
}

// A dependency polled for the same order answers its recordings in record
// order, as main's stateful cursor serves them, and stays on the last.
func TestVariables_RepeatedRecordingsAnswerInRecordOrder(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	create := reusable(at(varMock("mock-1", "POST", "http://jobs/jobs", `{"id":"j-rec"}`, 201, `{}`,
		models.MockVariables{"jobId": bindV("j-rec", "body.id")}), 1))
	var polls []*models.Mock
	for i, state := range []string{"pending", "pending", "done"} {
		polls = append(polls, reusable(at(varMock("mock-"+string(rune('2'+i)), "GET", "http://jobs/jobs/j-rec", "", 200, `{"state":"`+state+`"}`,
			models.MockVariables{"jobId": useV("j-rec", []string{"url.path[1]"})}), 2+i)))
	}
	// The pool hands them out least-recently-served first, not in record order.
	db := &varDb{session: []*models.Mock{polls[2], create, polls[0], polls[1]}}
	if ok, _, _ := matchVar(t, h, db, liveReq("POST", "/jobs", `{"id":"j-live"}`)); !ok {
		t.Fatal("the create should match")
	}
	var got []string
	for i := 0; i < 4; i++ {
		ok, stub, _ := matchVar(t, h, db, liveReq("GET", "/jobs/j-live", ""))
		if !ok {
			t.Fatalf("poll %d was a miss", i)
		}
		got = append(got, stub.Spec.HTTPResp.Body)
	}
	want := []string{`{"state":"pending"}`, `{"state":"pending"}`, `{"state":"done"}`, `{"state":"done"}`}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("polls answered %v, want %v", got, want)
	}
}

// Two creates of one shape, recorded as two variables: each live create binds
// the next variable in record order, and each read-back finds its own.
func TestVariables_TwoCreatesOfOneShapeBindTwoVariables(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	mocks := []*models.Mock{
		reusable(at(varMock("mock-1", "POST", "http://store/orders", `{"id":"a-rec"}`, 201, `{}`,
			models.MockVariables{"first": bindV("a-rec", "body.id")}), 1)),
		reusable(at(varMock("mock-2", "POST", "http://store/orders", `{"id":"b-rec"}`, 201, `{}`,
			models.MockVariables{"second": bindV("b-rec", "body.id")}), 2)),
		reusable(at(varMock("mock-3", "GET", "http://store/orders/b-rec", "", 200, `{"which":"second"}`,
			models.MockVariables{"second": useV("b-rec", []string{"url.path[1]"})}), 3)),
		reusable(at(varMock("mock-4", "GET", "http://store/orders/a-rec", "", 200, `{"which":"first"}`,
			models.MockVariables{"first": useV("a-rec", []string{"url.path[1]"})}), 4)),
	}
	// The pool hands them out in another order than they were recorded in.
	db := &varDb{session: []*models.Mock{mocks[1], mocks[3], mocks[0], mocks[2]}}
	for _, id := range []string{"a-live", "b-live"} {
		if ok, _, _ := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"`+id+`"}`)); !ok {
			t.Fatalf("the create of %s should match", id)
		}
	}
	for id, which := range map[string]string{"a-live": "first", "b-live": "second"} {
		ok, stub, _ := matchVar(t, h, db, liveReq("GET", "/orders/"+id, ""))
		if !ok || stub.Spec.HTTPResp.Body != `{"which":"`+which+`"}` {
			t.Fatalf("the read-back of %s: ok=%v stub=%+v", id, ok, stub)
		}
	}
	// A third create was never recorded: no variable is left to bind.
	ok, _, diag := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"c-live"}`))
	wantProblem(t, ok, diag, "at a place that binds it")
}

// An echo belongs to its call: a reusable mock answers each call with that
// call's own value, and the test's table is untouched.
func TestVariables_AnEchoIsTheCallsOwn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := reusable(at(varMock("mock-1", "POST", "http://pay/charge", `{"key":"k-rec","amount":5}`, 200, `{"key":"k-rec","ok":true}`,
		models.MockVariables{"chargeKey": {Recorded: "k-rec", Bind: "body.key", Answer: []string{"body.key"}, Echo: true}}), 1))
	db := &varDb{session: []*models.Mock{m}}
	for _, k := range []string{"k-1", "k-2"} {
		ok, stub, _ := matchVar(t, h, db, liveReq("POST", "/charge", `{"key":"`+k+`","amount":5}`))
		if !ok || stub.Spec.HTTPResp.Body != `{"key":"`+k+`","ok":true}` {
			t.Fatalf("the charge with key %s: ok=%v stub=%+v", k, ok, stub)
		}
	}
	if len(db.table.Snapshot()) != 0 {
		t.Fatalf("an echo entered the test's table: %+v", db.table.Snapshot())
	}
}

// A mock that only answers a variable is served by the variable pass: with the
// value the test bound, and never before it is bound.
func TestVariables_AnAnswerOnlyMock(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	create := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"r1"}`, 201, `{}`,
		models.MockVariables{"orderId": bindV("r1", "body.id")}), 2)
	latest := at(varMock("mock-2", "GET", "http://store/orders/latest", "", 200, `{"id":"r1"}`,
		models.MockVariables{"orderId": {Recorded: "r1", Answer: []string{"body.id"}}}), 3)
	db := &varDb{perTest: []*models.Mock{create, latest}}
	ok, _, diag := matchVar(t, h, db, liveReq("GET", "/orders/latest", ""))
	wantProblem(t, ok, diag, "before the call that binds it")
	if ok, _, _ := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"l1"}`)); !ok {
		t.Fatal("the create should match")
	}
	ok, stub, _ := matchVar(t, h, db, liveReq("GET", "/orders/latest", ""))
	if !ok || stub.Spec.HTTPResp.Body != `{"id":"l1"}` {
		t.Fatalf("the latest order: ok=%v stub=%+v", ok, stub)
	}
}

func TestVariables_ACallRecordedWithAnotherValueIsAnotherTests(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	create := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"r1"}`, 201, `{}`,
		models.MockVariables{"orderId": bindV("r1", "body.id")}), 1)
	read := at(varMock("mock-2", "GET", "http://store/orders/r2", "", 200, `{}`,
		models.MockVariables{"orderId": useV("r2", []string{"url.path[1]"})}), 2)
	db := &varDb{perTest: []*models.Mock{create, read}}
	if ok, _, _ := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"l1"}`)); !ok {
		t.Fatal("the create should match")
	}
	// The read-back was recorded with another value than the one the create
	// bound: it is another test's call, never this one's.
	ok, stub, diag := matchVar(t, h, db, liveReq("GET", "/orders/l1", ""))
	if ok || (diag != nil && diag.variable != nil) {
		t.Fatalf("a call recorded with another value of orderId answered or explained the read-back: ok=%v stub=%v diag=%+v", ok, stub, diag)
	}
}

func TestVariables_AnAnswerIsWrittenAsItsPlaceIsTyped(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	create := at(varMock("mock-1", "POST", "http://store/orders", `{"n":1001}`, 201, `{"n":1001,"s":"1001"}`,
		models.MockVariables{"n": bindV("1001", "body.n", "body.n", "body.s")}), 1)
	db := &varDb{perTest: []*models.Mock{create}}
	ok, stub, _ := matchVar(t, h, db, liveReq("POST", "/orders", `{"n":2002}`))
	if !ok || stub.Spec.HTTPResp.Body != `{"n":2002,"s":"2002"}` {
		t.Fatalf("answered %v %+v", ok, stub)
	}
	// A recorded answer that does not hold the variable's recorded value at
	// its answer place is refused, never answered as recorded.
	for _, resp := range []string{`{"n":7}`, `{"s":"other"}`} {
		place := "body.n"
		if strings.Contains(resp, `"s"`) {
			place = "body.s"
		}
		bad := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"r1"}`, 201, resp,
			models.MockVariables{"id": bindV("r1", "body.id", place)}), 1)
		db = &varDb{perTest: []*models.Mock{bad}}
		ok, _, diag := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"l1"}`))
		wantProblem(t, ok, diag, "cannot hold variable id")
		if len(db.perTest) != 1 {
			t.Fatal("a mock that could not be answered must not be consumed")
		}
		if _, bound := db.table.Lookup("id"); bound {
			t.Fatal("a call that was not answered bound its variable")
		}
	}
}

func TestVariables_ADigestOfTheRecordedBodyIsComputedAgain(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	recBody := `{"id":"r1"}`
	sum := md5.Sum([]byte(recBody))
	m := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"r1"}`, 201, recBody,
		models.MockVariables{"id": bindV("r1", "body.id", "body.id")}), 1)
	m.Spec.HTTPResp.Header["Content-MD5"] = base64.StdEncoding.EncodeToString(sum[:])
	db := &varDb{perTest: []*models.Mock{m}}
	ok, stub, _ := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"live-2"}`))
	if !ok {
		t.Fatal("should match")
	}
	want := md5.Sum([]byte(`{"id":"live-2"}`))
	if got := stub.Spec.HTTPResp.Header["Content-MD5"]; got != base64.StdEncoding.EncodeToString(want[:]) {
		t.Fatalf("Content-MD5 %q is not the digest of the answered body", got)
	}
	if m.Spec.HTTPResp.Body != recBody {
		t.Fatal("the pooled mock's response was written")
	}
}

func TestVariables_ADeclarationKeployCannotFollowIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for name, vars := range map[string]models.MockVariables{
		"a place the recording lacks":  {"orderId": bindV("r1", "body.missing")},
		"another recorded value":       {"orderId": bindV("r9", "body.id")},
		"a place outside the grammar":  {"orderId": bindV("r1", "body..id")},
		"an echo with no binding call": {"orderId": {Recorded: "r1", Answer: []string{"body.id"}, Echo: true}},
		"no recorded value":            {"orderId": {Bind: "body.id"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHTTP()
			m := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"r1"}`, 201, `{"id":"r1"}`, vars), 1)
			db := &varDb{perTest: []*models.Mock{m}}
			ok, _, diag := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"r1"}`))
			wantProblem(t, ok, diag, "cannot be followed")
		})
	}
}

func TestVariables_SwitchOffTheBlockIsNotRead(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	h := newHTTP()
	db := &varDb{perTest: createAndReadBack("ord-rec-1")}
	// As on main: the exact pass answers a call that holds the recorded value,
	// as recorded, and nothing is bound.
	ok, stub, _ := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"ord-rec-1","item":"pen"}`))
	if !ok || stub.Name != "mock-1" || stub.Spec.HTTPResp.Body != `{"id":"ord-rec-1"}` {
		t.Fatalf("switch off: ok=%v stub=%+v", ok, stub)
	}
	ok, stub, _ = matchVar(t, h, db, liveReq("GET", "/orders/ord-rec-1", ""))
	if !ok || stub.Spec.HTTPResp.Body != `{"id":"ord-rec-1","item":"pen","n":1}` {
		t.Fatalf("switch off: answered %v %+v", ok, stub)
	}
	if len(db.table.Snapshot()) != 0 {
		t.Fatal("switch off bound a variable")
	}
}

func TestVariables_AMockWithoutTheBlockMatchesAsOnMain(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	plain := varMock("mock-9", "GET", "http://store/health", "", 200, `{"up":true}`, nil)
	db := &varDb{perTest: append(createAndReadBack("r"), plain)}
	ok, stub, _ := matchVar(t, h, db, liveReq("GET", "/health", ""))
	if !ok || stub.Name != "mock-9" {
		t.Fatalf("a mock without variables: ok=%v stub=%v", ok, stub)
	}
}

// Outside its variable places a call is compared as recorded: a URL segment
// that is no variable's does not drift, whatever it looks like.
func TestVariables_AURLSegmentOutsideTheVariablesIsExact(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	create := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"o-rec"}`, 201, `{}`,
		models.MockVariables{"orderId": bindV("o-rec", "body.id")}), 1)
	read := at(varMock("mock-2", "GET", "http://store/stores/1001/orders/o-rec", "", 200, `{}`,
		models.MockVariables{"orderId": useV("o-rec", []string{"url.path[3]"})}), 2)
	db := &varDb{perTest: []*models.Mock{create, read}}
	if ok, _, _ := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"o-live"}`)); !ok {
		t.Fatal("the create should match")
	}
	// main's id-shaped URL pass takes 1001 and 2002 for one id.
	if ok, stub, _ := matchVar(t, h, db, liveReq("GET", "/stores/2002/orders/o-live", "")); ok {
		t.Fatalf("a read-back from another store was answered by %s", stub.Name)
	}
	if ok, _, _ := matchVar(t, h, db, liveReq("GET", "/stores/1001/orders/o-live", "")); !ok {
		t.Fatal("the read-back from the recorded store should match")
	}
}

// A declaration keploy cannot follow speaks only for the calls of its own
// route: another call is matched as it would be without it.
func TestVariables_ABrokenDeclarationLeavesOtherCallsAlone(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	broken := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"r1"}`, 201, `{}`,
		models.MockVariables{"orderId": bindV("r1", "body.missing")}), 1)
	// Answered by main's lenient body pass only: the live body differs.
	other := at(varMock("mock-2", "POST", "http://store/items", `{"name":"pen","n":1}`, 201, `{"ok":true}`, nil), 2)
	db := &varDb{perTest: []*models.Mock{broken, other}}
	ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/items", `{"name":"pen","n":2}`))
	if !ok || stub.Name != "mock-2" {
		t.Fatalf("a call of another route: ok=%v stub=%v diag=%+v", ok, stub, diag)
	}
}

// Under the switch a correlation written on a mock by any other means is not
// honoured: an echo is a variable of its call.
func TestVariables_ACorrelationIsNotHonouredUnderTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &mockMemDb{mocks: []*models.Mock{correlationMock(recUUID, true)}, updateUnFilteredReturn: true}
	live := jsonReq("POST", "/pay", `{"amount":10,"idempotencyKey":"`+newUUID+`"}`)
	_, stub, _, err := h.match(context.Background(), live, db, nil, nil, nil, true, false, false, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if stub != nil && strings.Contains(stub.Spec.HTTPResp.Body, newUUID) {
		t.Fatalf("a correlation was honoured under the switch: %s", stub.Spec.HTTPResp.Body)
	}
}

// After a variable did not hold, only an exact recording of the call answers
// it: a recording of the same route with another body does not.
func TestVariables_AfterAMismatchOnlyAnExactBodyAnswers(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	create := at(varMock("mock-1", "POST", "http://store/carts", `{"cart":"c-rec"}`, 201, `{}`,
		models.MockVariables{"cartId": bindV("c-rec", "body.cart")}), 1)
	add := at(varMock("mock-2", "POST", "http://store/items", `{"cart":"c-rec","item":"pen"}`, 201, `{}`,
		models.MockVariables{"cartId": useV("c-rec", []string{"body.cart"})}), 2)
	plain := at(varMock("mock-3", "POST", "http://store/items", `{"cart":"c-zzz","item":"pen"}`, 201, `{"plain":true}`, nil), 3)
	db := &varDb{perTest: []*models.Mock{create, add, plain}}
	if ok, _, _ := matchVar(t, h, db, liveReq("POST", "/carts", `{"cart":"c-live"}`)); !ok {
		t.Fatal("the create should match")
	}
	ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/items", `{"cart":"c-other","item":"pen"}`))
	if ok {
		t.Fatalf("a call whose variable did not hold was answered by %s", stub.Name)
	}
	wantProblem(t, ok, diag, "this call sent")
}

func TestVariables_EveryPlaceOfACallHoldsItsVariable(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for name, vars := range map[string]models.MockVariable{
		"a bind and a use in one call": {Recorded: "r1", Bind: "body.id", Use: []string{"header.X-Id"}},
		"an echo and its use":          {Recorded: "r1", Bind: "body.id", Use: []string{"header.X-Id"}, Echo: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHTTP()
			m := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"r1"}`, 201, `{}`, models.MockVariables{"id": vars}), 1)
			m.Spec.HTTPReq.Header["X-Id"] = "r1"
			db := &varDb{perTest: []*models.Mock{m}}
			r := liveReq("POST", "/orders", `{"id":"l1"}`)
			r.header.Set("X-Id", "l2")
			ok, _, diag := matchVar(t, h, db, r)
			wantProblem(t, ok, diag, "this call sent")
			r.header.Set("X-Id", "l1")
			if ok, _, _ := matchVar(t, h, db, r); !ok {
				t.Fatal("a call holding one value at both places should match")
			}
		})
	}
}

func TestVariables_AnEchoOfAnEmptyValueIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := reusable(at(varMock("mock-1", "POST", "http://pay/charge", `{"key":"k-rec"}`, 200, `{"key":"k-rec"}`,
		models.MockVariables{"key": {Recorded: "k-rec", Bind: "body.key", Answer: []string{"body.key"}, Echo: true}}), 1))
	db := &varDb{session: []*models.Mock{m}}
	ok, _, diag := matchVar(t, h, db, liveReq("POST", "/charge", `{"key":""}`))
	wantProblem(t, ok, diag, "an empty value")
}

func TestVariables_AHeaderAnswerIsServed(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"r1"}`, 201, `{}`,
		models.MockVariables{"id": bindV("r1", "body.id", "header.X-Order")}), 1)
	m.Spec.HTTPResp.Header["X-Order"] = "r1"
	db := &varDb{perTest: []*models.Mock{m}}
	ok, stub, _ := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"l1"}`))
	if !ok || stub.Spec.HTTPResp.Header["X-Order"] != "l1" || m.Spec.HTTPResp.Header["X-Order"] != "r1" {
		t.Fatalf("ok=%v served %+v (pooled %+v)", ok, stub, m.Spec.HTTPResp.Header)
	}
}

func TestVariables_MoreDeclarationsKeployRefuses(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for name, vars := range map[string]models.MockVariables{
		"an answer at a request place": {"id": bindV("r1", "body.id", "url.path[0]")},
		"one place twice":              {"a": bindV("r1", "header.X-Id"), "b": useV("r1", []string{"header.x-id"})},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHTTP()
			m := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"r1"}`, 201, `{}`, vars), 1)
			m.Spec.HTTPReq.Header["X-Id"] = "r1"
			db := &varDb{perTest: []*models.Mock{m}}
			r := liveReq("POST", "/orders", `{"id":"r1"}`)
			r.header.Set("X-Id", "r1")
			ok, _, diag := matchVar(t, h, db, r)
			wantProblem(t, ok, diag, "cannot be followed")
		})
	}
}

// The known noise of a call with variables is the user's body noise and the
// obfuscator's redacted values, as for any other call.
func TestVariables_UserNoiseAndRedactedValuesAreKnownNoise(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"r1","at":"t1","secret":"REDACTED_1"}`, 201, `{}`,
		models.MockVariables{"id": bindV("r1", "body.id")}), 1)
	m.Noise = []string{`^REDACTED_\d+$`}
	db := &varDb{perTest: []*models.Mock{m}}
	live := liveReq("POST", "/orders", `{"id":"l1","at":"t2","secret":"sk_live_9"}`)
	ok, _, _, err := h.match(context.Background(), live, db, nil, map[string][]string{"at": {}}, nil, true, false, false, true, true)
	if err != nil || !ok {
		t.Fatalf("a drift under the user's noise and a redacted value should fit: ok=%v err=%v", ok, err)
	}
	db = &varDb{perTest: []*models.Mock{at(varMock("mock-1", "POST", "http://store/orders", `{"id":"r1","at":"t1","secret":"REDACTED_1"}`, 201, `{}`,
		models.MockVariables{"id": bindV("r1", "body.id")}), 1)}}
	if ok, _, _, _ := h.match(context.Background(), live, db, nil, map[string][]string{"at": {}}, nil, true, false, false, true, true); ok {
		t.Fatal("without the obfuscator's patterns the secret is compared")
	}
}

// A call answered by a mock with variables, and diffed against one it did not
// fit, never names a variable's place as the difference.
func TestVariables_TheClosestMockDiffLeavesTheVariablesOut(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"r1","item":"pen"}`, 201, `{}`,
		models.MockVariables{"id": bindV("r1", "body.id")}), 1)
	db := &varDb{perTest: []*models.Mock{m}}
	live := liveReq("POST", "/orders", `{"id":"l1","item":"book"}`)
	ok, _, diag := matchVar(t, h, db, live)
	if ok {
		t.Fatal("a create of another item was answered")
	}
	req := makeReq("POST", "/orders")
	req.Header = http.Header{"Content-Type": []string{"application/json"}}
	rep := h.buildHTTPMismatchReport(req, live.body, db, nil, nil, nil, diag)
	for _, d := range rep.FieldDiffs {
		if d.Path == "body.id" {
			t.Fatalf("the diff names the variable's place: %+v", rep.FieldDiffs)
		}
	}
	found := false
	for _, d := range rep.FieldDiffs {
		found = found || d.Path == "body.item"
	}
	if !found {
		t.Fatalf("the diff does not name the field that differs: %+v", rep.FieldDiffs)
	}
}
