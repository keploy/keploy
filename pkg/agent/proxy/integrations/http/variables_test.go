package http

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
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
	perTest  []*models.Mock
	session  []*models.Mock
	table    variables.Table
	consumed map[string]bool
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
			if d.consumed == nil {
				d.consumed = map[string]bool{}
			}
			d.consumed[m.Name] = true
			return true
		}
	}
	return false
}

// UpdateUnFilteredMock keeps a reusable mock; a per-test mock another call
// consumed already is gone (the update fails, as the manager's does).
func (d *varDb) UpdateUnFilteredMock(old, _ *models.Mock) bool {
	return !(old.TestModeInfo.Lifetime == models.LifetimePerTest && d.consumed[old.Name])
}
func (d *varDb) Variables() *variables.Table { return &d.table }

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

// Two connections that sent creates of one shape at once, each checked before
// either bound, are each answered: the second's bind collides, it is checked
// again, and the next recording binds its variable.
func TestVariables_ConcurrentCreatesOfOneShapeAreEachAnswered(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	first := reusable(at(varMock("mock-1", "POST", "http://store/orders", `{"id":"a-rec"}`, 201, `{}`,
		models.MockVariables{"first": bindV("a-rec", "body.id")}), 1))
	second := reusable(at(varMock("mock-2", "POST", "http://store/orders", `{"id":"b-rec"}`, 201, `{}`,
		models.MockVariables{"second": bindV("b-rec", "body.id")}), 2))
	db := &varDb{session: []*models.Mock{first, second}}
	ctx := context.Background()
	x, y := liveReq("POST", "/orders", `{"id":"x"}`), liveReq("POST", "/orders", `{"id":"y"}`)
	pool, _ := splitVariableMocks([]*models.Mock{first, second})
	hitsX, _, _ := h.variablePass(ctx, x, pool, &db.table, db.table.Gen(), nil, nil, nil)
	hitsY, _, _ := h.variablePass(ctx, y, pool, &db.table, db.table.Gen(), nil, nil, nil)
	hx, _ := h.pickHit(hitsX, db, true)
	hy, _ := h.pickHit(hitsY, db, true)
	if hx.mock != first || hy.mock != first {
		t.Fatalf("both checked before either bound should pick the first recording: %s, %s", hx.mock.Name, hy.mock.Name)
	}
	if _, p, retry, err := h.serveVariables(ctx, hx, db, &db.table); err != nil || p != nil || retry {
		t.Fatalf("x: problem %+v retry %v err %v", p, retry, err)
	}
	if _, p, retry, err := h.serveVariables(ctx, hy, db, &db.table); err != nil || p != nil || !retry {
		t.Fatalf("y's collision should be checked again, not failed: problem %+v retry %v err %v", p, retry, err)
	}
	ok, stub, diag := matchVar(t, h, db, y)
	if !ok || stub.Name != "mock-2" {
		t.Fatalf("y checked again: ok=%v stub=%v diag=%+v", ok, stub, diag)
	}
	if b, _ := db.table.Lookup("second"); b.Value != "y" {
		t.Fatalf("second bound as %+v", b)
	}
}

// A call checked in one test binds nothing in the next: a straggler of the
// previous test is checked again against the new one.
func TestVariables_AStragglerBindsNothingInTheNextTest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{perTest: createAndReadBack("ord-rec-1")}
	ctx := context.Background()
	pool, _ := splitVariableMocks(db.perTest)
	hits, _, _ := h.variablePass(ctx, liveReq("POST", "/orders", `{"id":"straggler","item":"pen"}`), pool, &db.table, db.table.Gen(), nil, nil, nil)
	if len(hits) == 0 {
		t.Fatal("the straggler should fit")
	}
	db.table.Reset() // the next test begins
	if _, p, retry, err := h.serveVariables(ctx, hits[0], db, &db.table); err != nil || p != nil || !retry {
		t.Fatalf("a straggler should be checked again: problem %+v retry %v err %v", p, retry, err)
	}
	if got := db.table.Snapshot(); len(got) != 0 {
		t.Fatalf("a straggler bound %+v in the next test", got)
	}
	if len(db.perTest) != 2 {
		t.Fatal("a straggler consumed a mock of the next test")
	}
}

// A call that sent the bound value but whose mock another connection took
// first is checked again; with no recording left it is a miss.
func TestVariables_AMockAnotherCallTookIsCheckedAgain(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{perTest: createAndReadBack("ord-rec-1")}
	ctx := context.Background()
	if ok, _, _ := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"l1","item":"pen"}`)); !ok {
		t.Fatal("the create should match")
	}
	pool, _ := splitVariableMocks(db.perTest)
	read := liveReq("GET", "/orders/l1", "")
	hits, _, _ := h.variablePass(ctx, read, pool, &db.table, db.table.Gen(), nil, nil, nil)
	if ok, _, _ := matchVar(t, h, db, read); !ok {
		t.Fatal("the read-back should match")
	}
	if _, p, retry, err := h.serveVariables(ctx, hits[0], db, &db.table); err != nil || p != nil || !retry {
		t.Fatalf("a mock another call took: problem %+v retry %v err %v", p, retry, err)
	}
	if ok, _, _ := matchVar(t, h, db, read); ok {
		t.Fatal("a second read-back was answered by a recording consumed already")
	}
}

// The evidence says what kind of difference it is: a value of another type is
// a type change against the recording, a use before the binding call and a
// value another variable holds are no field difference (the diff says it).
func TestVariables_TheEvidenceSaysWhatDiffers(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"123","item":"pen"}`, 201, `{}`,
		models.MockVariables{"orderId": bindV("123", "body.id")}), 1)
	db := &varDb{perTest: []*models.Mock{m}}
	_, _, diag := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":456,"item":"pen"}`))
	rep := h.buildHTTPMismatchReport(makeReq("POST", "/orders"), []byte(`{"id":456,"item":"pen"}`), db, nil, nil, nil, diag)
	if len(rep.FieldDiffs) != 1 || rep.FieldDiffs[0].Kind != models.DiffKindTypeChanged || rep.FieldDiffs[0].Expected != `"123"` {
		t.Fatalf("a type change: %+v", rep.FieldDiffs)
	}
	db = &varDb{perTest: createAndReadBack("r")}
	_, _, diag = matchVar(t, h, db, liveReq("GET", "/orders/l1", ""))
	rep = h.buildHTTPMismatchReport(makeReq("GET", "/orders/l1"), nil, db, nil, nil, nil, diag)
	if len(rep.FieldDiffs) != 0 || !strings.Contains(rep.Diff, "before the call that binds it") {
		t.Fatalf("a use before the binding call: %+v %q", rep.FieldDiffs, rep.Diff)
	}
}

// The report renders the call as it was received; only its field diffs leave
// a variable's place out, and only for a call to the mock's route.
func TestVariables_TheReportRendersTheCallAsReceived(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"r1","item":"pen"}`, 201, `{}`,
		models.MockVariables{"id": bindV("r1", "body.id")}), 1)
	db := &varDb{perTest: []*models.Mock{m}}
	body := `{"id":"l1","item":"book"}`
	_, _, diag := matchVar(t, h, db, liveReq("POST", "/orders", body))
	req := makeReq("POST", "/orders")
	req.Header = http.Header{"Content-Type": []string{"application/json"}}
	rep := h.buildHTTPMismatchReport(req, []byte(body), db, nil, nil, nil, diag)
	if !strings.Contains(rep.ReceivedReq, "l1") || strings.Contains(rep.ReceivedReq, "r1") {
		t.Fatalf("the received call is rendered as %q", rep.ReceivedReq)
	}
	r2 := httpRequest("GET", "/orderz/l1")
	read := at(varMock("mock-2", "GET", "http://store/orders/r1", "", 200, `{}`, models.MockVariables{"id": useV("r1", []string{"url.path[1]"})}), 2)
	got, _ := asRecordedForDiff(r2, nil, read)
	if got.URL.Path != "/orderz/l1" {
		t.Fatalf("a call to another route had a variable put back: %s", got.URL.Path)
	}
}

func httpRequest(method, path string) *http.Request {
	r := makeReq(method, path)
	r.Header = http.Header{}
	return r
}

// With the switch off the miss report of a call against a mock with
// variables is main's: its field diffs name every place that differs.
func TestVariables_SwitchOffTheReportIsAsOnMain(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	h := newHTTP()
	m := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"r1","item":"pen"}`, 201, `{}`,
		models.MockVariables{"id": bindV("r1", "body.id")}), 1)
	db := &varDb{perTest: []*models.Mock{m}}
	req := makeReq("POST", "/orders")
	req.Header = http.Header{"Content-Type": []string{"application/json"}}
	rep := h.buildHTTPMismatchReport(req, []byte(`{"id":"l1","item":"book"}`), db, nil, nil, nil, &matchDiag{phase: models.MatchPhaseBody, candidates: 1, schemaMatched: []*models.Mock{m}})
	paths := map[string]bool{}
	for _, d := range rep.FieldDiffs {
		paths[d.Path] = true
	}
	if !paths["body.id"] || !paths["body.item"] {
		t.Fatalf("switch off: field diffs %+v", rep.FieldDiffs)
	}
}

// A call made after its test's scope began is answered in it: the scope it is
// checked in is the one it binds in.
func TestVariables_ACallAfterTheScopeBeganIsAnswered(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{perTest: createAndReadBack("ord-rec-1")}
	db.table.Reset() // a test began
	// A safety net for a call that would be checked again forever.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ok, _, _, err := h.match(ctx, liveReq("POST", "/orders", `{"id":"l1","item":"pen"}`), db, nil, nil, nil, true, false, false, true, true)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

// swapDb begins the next test (its pool swapped in, then its scope begun) as
// soon as a call has read the previous test's pool.
type swapDb struct {
	varDb
	next    []*models.Mock
	swapped bool
}

func (d *swapDb) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	pool, err := d.varDb.GetPerTestMocksInWindow()
	if !d.swapped {
		d.swapped = true
		d.perTest = d.next
		d.table.Reset()
	}
	return pool, err
}

// A call checked against the previous test's pool binds nothing in the test
// that began meanwhile: it is checked again against the new pool.
func TestVariables_ACallOfThePreviousPoolBindsNothingInTheNextTest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &swapDb{}
	db.perTest = createAndReadBack("ord-rec-1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ok, stub, _, err := h.match(ctx, liveReq("POST", "/orders", `{"id":"straggler","item":"pen"}`), db, nil, nil, nil, true, false, false, true, true)
	if err != nil || ok {
		t.Fatalf("the straggler: ok=%v stub=%v err=%v", ok, stub, err)
	}
	if got := db.table.Snapshot(); len(got) != 0 {
		t.Fatalf("the straggler bound %+v in the next test", got)
	}
}

// The field diffs of a call against a mock with variables leave out every
// kind of variable place; a variable place that changed its JSON type is
// named.
func TestVariables_TheClosestMockDiffLeavesEveryVariablePlaceOut(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := at(varMock("mock-1", "POST", "http://store/orders/o-rec?trace=t-rec", `{"id":"i-rec","n":"5","item":"pen"}`, 201, `{}`,
		models.MockVariables{
			"order": useV("o-rec", []string{"url.path[1]"}),
			"trace": useV("t-rec", []string{"url.query.trace"}),
			"ref":   useV("k-rec", []string{"header.X-Ref"}),
			"id":    useV("i-rec", []string{"body.id"}),
			"n":     useV("5", []string{"body.n"}),
		}), 1)
	m.Spec.HTTPReq.Header["X-Ref"] = "k-rec"
	db := &varDb{perTest: []*models.Mock{m}}
	req := makeReq("POST", "/orders/o-live")
	req.URL.RawQuery = "trace=t-live"
	req.Header = http.Header{"Content-Type": []string{"application/json"}, "X-Ref": []string{"k-live"}}
	body := `{"id":"i-live","n":6,"item":"book"}`
	rep := h.buildHTTPMismatchReport(req, []byte(body), db, nil, nil, nil, &matchDiag{phase: models.MatchPhaseBody, candidates: 1, schemaMatched: []*models.Mock{m}, pool: []*models.Mock{m}})
	paths := map[string]bool{}
	for _, d := range rep.FieldDiffs {
		paths[d.Path] = true
	}
	for _, p := range []string{"path", "query.trace", "body.id"} {
		if paths[p] {
			t.Fatalf("the diff names variable place %s: %+v", p, rep.FieldDiffs)
		}
	}
	if !paths["body.item"] || !paths["body.n"] {
		t.Fatalf("the diff must name the changed field and the type change at a variable place: %+v", rep.FieldDiffs)
	}
	if !strings.Contains(rep.ReceivedReq, "o-live") || !strings.Contains(rep.ReceivedReq, "k-live") {
		t.Fatalf("the received call is not rendered as received: %q", rep.ReceivedReq)
	}
}

// resetOnCursorDb begins the next test (its scope reset) the first time a
// call reads the stateful cursor, after the variable pass checked it.
type resetOnCursorDb struct {
	varDb
	done bool
}

func (d *resetOnCursorDb) MockCursorIndex(key string, n int) int {
	if !d.done {
		d.done = true
		d.table.Reset()
	}
	return d.varDb.MockCursorIndex(key, n)
}

// A call whose answer could not be rendered because a test began meanwhile is
// checked again against the new test, and its evidence is that check's.
func TestVariables_AMissCheckedAcrossABeginIsCheckedAgain(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	create := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"r1"}`, 201, `{}`,
		models.MockVariables{"orderId": bindV("r1", "body.id")}), 1)
	latest := reusable(at(varMock("mock-2", "GET", "http://store/orders/latest", "", 200, `{"id":"r1"}`,
		models.MockVariables{"orderId": {Recorded: "r1", Answer: []string{"body.id"}}}), 2))
	// Recorded twice, so the call reads the stateful cursor.
	latest2 := reusable(at(varMock("mock-3", "GET", "http://store/orders/latest", "", 200, `{"id":"r1"}`,
		models.MockVariables{"orderId": {Recorded: "r1", Answer: []string{"body.id"}}}), 3))
	db := &resetOnCursorDb{}
	db.perTest = []*models.Mock{create}
	db.session = []*models.Mock{latest, latest2}
	if ok, _, _ := matchVar(t, h, &db.varDb, liveReq("POST", "/orders", `{"id":"l1"}`)); !ok {
		t.Fatal("the create should match")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ok, _, diag, err := h.match(ctx, liveReq("GET", "/orders/latest", ""), db, nil, nil, nil, true, false, false, true, true)
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if diag == nil || diag.variable == nil || !strings.Contains(diag.variable.why, "before the call that binds it") {
		t.Fatalf("the evidence is not the check against the new test: %+v", diag)
	}
}

// A value the test case's request gave is no variable's value the app made:
// an app whose counter mints the same number for an order is not taken for
// one that sent the user's value where the order's goes.
func TestVariables_AGivenValueIsNoOtherVariables(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	create := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"5"}`, 201, `{}`,
		models.MockVariables{"orderId": bindV("5", "body.id")}), 1)
	db := &varDb{perTest: []*models.Mock{create}}
	db.table.Start([]variables.Binding{{Name: "userId", Value: "1", Recorded: "1", Mock: "test case test-2", Place: "request"}})
	if ok, _, diag := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"1"}`)); !ok {
		t.Fatalf("an order id equal to the user id the request gave was refused: %+v", diag)
	}
}

// A call that names a template key's variable with another recorded value than
// the key's is refused with evidence: a key holds one recorded value for the
// set, so it is no other test's call.
func TestVariables_AGivenKeyRecordedWithAnotherValueIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	read := at(varMock("mock-1", "GET", "http://store/users/u_rec_9999", "", 200, `{}`,
		models.MockVariables{"id": useV("u_rec_9999", []string{"url.path[1]"})}), 1)
	db := &varDb{perTest: []*models.Mock{read}}
	db.table.Start([]variables.Binding{{Name: "id", Value: "u_live", Recorded: "u_rec_0001", Mock: "test case test-2", Place: "request"}})
	ok, _, diag := matchVar(t, h, db, liveReq("GET", "/users/u_live", ""))
	wantProblem(t, ok, diag, "the learn step and the templates disagree")
}

// Evidence of a value that differs wins over a declaration keploy cannot
// follow: a reusable mock of another test that names a template key with its
// own recorded value does not hide this test's wrong id.
func TestVariables_AValueThatDiffersIsTheEvidence(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	other := reusable(at(varMock("mock-b", "GET", "http://store/users/o_123", "", 200, `{}`,
		models.MockVariables{"id": useV("o_123", []string{"url.path[1]"})}), 1))
	mine := reusable(at(varMock("mock-a", "GET", "http://store/users/u_rec", "", 200, `{}`,
		models.MockVariables{"id": useV("u_rec", []string{"url.path[1]"})}), 2))
	db := &varDb{session: []*models.Mock{other, mine}}
	db.table.Start([]variables.Binding{{Name: "id", Value: "u_live", Recorded: "u_rec", Mock: "test case test-a", Place: "request"}})
	ok, _, diag := matchVar(t, h, db, liveReq("GET", "/users/u_wrong", ""))
	p := wantProblem(t, ok, diag, "this call sent")
	if p.mock != "mock-a" || p.expected != "u_live" || p.actual != "u_wrong" {
		t.Fatalf("evidence %+v", p)
	}
}

// pollAt is a reusable recording of POST /poll carrying token T: repeated,
// the call takes the recordings in record order.
func pollAt(name string, seq int, answer string, vars models.MockVariables) *models.Mock {
	return reusable(at(varMock(name, "POST", "http://svc/poll", `{"token":"T"}`, 200, `{"status":"`+answer+`"}`, vars), seq))
}

func servedNames(t *testing.T, h *HTTP, db *varDb, live string, calls int) []string {
	t.Helper()
	var got []string
	for i := 0; i < calls; i++ {
		ok, stub, diag := matchVar(t, h, db, liveReq("POST", "http://svc/poll", live))
		if !ok {
			t.Fatalf("call %d missed: %+v", i+1, diag)
		}
		got = append(got, stub.Name)
	}
	return got
}

// A repeated call takes the recordings in record order, as one without
// variables does: the recording that uses the token the first one binds is
// still the second call's, although the first call came before its bind.
func TestVariables_ARepeatedCallTakesTheRecordingsInRecordOrder(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name   string
		v1, v2 models.MockVariables
		live   string
	}{
		{"without variables", nil, nil, `{"token":"T"}`},
		{"bind, then use", models.MockVariables{"token": bindV("T", "body.token")}, models.MockVariables{"token": useV("T", []string{"body.token"})}, `{"token":"L"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &varDb{session: []*models.Mock{pollAt("a-1", 1, "pending", tc.v1), pollAt("a-2", 2, "done", tc.v2)}}
			if got := servedNames(t, newHTTP(), db, tc.live, 3); strings.Join(got, " ") != "a-1 a-2 a-2" {
				t.Fatalf("served %v, want a-1 a-2 a-2", got)
			}
		})
	}
}

// A recording whose variables do not hold (here one of a variable no call
// binds) is passed over: the next recording in record order that holds
// answers, each in turn.
func TestVariables_TheCursorPassesARecordingWhoseVariablesDoNotHold(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	db := &varDb{session: []*models.Mock{
		pollAt("r-1", 1, "one", models.MockVariables{"token": bindV("T", "body.token")}),
		pollAt("r-2", 2, "two", models.MockVariables{"other": useV("T", []string{"body.token"})}),
		pollAt("r-3", 3, "three", models.MockVariables{"token": useV("T", []string{"body.token"})}),
	}}
	if got := servedNames(t, newHTTP(), db, `{"token":"L"}`, 3); strings.Join(got, " ") != "r-1 r-3 r-3" {
		t.Fatalf("served %v, want r-1 r-3 r-3", got)
	}
}

// meAt is a reusable recording of GET /me whose X-Tok header carries the
// token its test logged in with (recorded as rec): a call fits the recordings
// of every test, and those of its own test hold.
func meAt(name string, seq int, rec string) *models.Mock {
	m := varMock(name, "GET", "http://svc/me", "", 200, `{"me":"`+name+`"}`,
		models.MockVariables{"token": useV(rec, []string{"header.X-Tok"})})
	m.Spec.HTTPReq.Header["X-Tok"] = rec
	return reusable(at(m, seq))
}

// loginAs begins a test that logs in: its token, recorded as rec, is live.
func loginAs(t *testing.T, h *HTTP, db *varDb, rec, live string) {
	t.Helper()
	db.table.Reset()
	db.perTest = []*models.Mock{at(varMock("login-"+rec, "POST", "http://svc/login", `{"tok":"`+rec+`"}`, 200, `{}`,
		models.MockVariables{"token": bindV(rec, "body.tok")}), 0)}
	if ok, _, diag := matchVar(t, h, db, liveReq("POST", "http://svc/login", `{"tok":"`+live+`"}`)); !ok {
		t.Fatalf("the login missed: %+v", diag)
	}
}

func getMe(t *testing.T, h *HTTP, db *varDb, live string, calls int) string {
	t.Helper()
	var got []string
	for i := 0; i < calls; i++ {
		r := liveReq("GET", "http://svc/me", "")
		r.header.Set("X-Tok", live)
		ok, stub, diag := matchVar(t, h, db, r)
		if !ok {
			t.Fatalf("call %d missed: %+v", i+1, diag)
		}
		got = append(got, stub.Name)
	}
	return strings.Join(got, " ")
}

// Two tests repeat one request, each with its own token: each takes its own
// recordings in record order, the other test's holding their places.
func TestVariables_EachTestTakesItsRecordingsInRecordOrder(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{session: []*models.Mock{meAt("a-1", 1, "tA"), meAt("a-2", 2, "tA"), meAt("b-1", 3, "tB"), meAt("b-2", 4, "tB")}}
	loginAs(t, h, db, "tA", "LA")
	a := getMe(t, h, db, "LA", 2)
	loginAs(t, h, db, "tB", "LB")
	if b := getMe(t, h, db, "LB", 2); a != "a-1 a-2" || b != "b-1 b-2" {
		t.Fatalf("test A took %s, test B %s; want a-1 a-2, b-1 b-2", a, b)
	}
}

// A test whose recordings come after another test's that did not run takes
// its own in order, passing over the other test's.
func TestVariables_ATestPassesAnotherTestsRecordings(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{session: []*models.Mock{meAt("a-1", 1, "tA"), meAt("b-1", 2, "tB"), meAt("b-2", 3, "tB")}}
	loginAs(t, h, db, "tB", "LB")
	if got := getMe(t, h, db, "LB", 2); got != "b-1 b-2" {
		t.Fatalf("test B took %s, want b-1 b-2", got)
	}
	db = &varDb{session: []*models.Mock{meAt("r-1", 1, "tA"), meAt("r-2", 2, "tB"), meAt("r-3", 3, "tB"), meAt("r-4", 4, "tA"), meAt("r-5", 5, "tA")}}
	loginAs(t, h, db, "tA", "LA")
	if got := getMe(t, h, db, "LA", 3); got != "r-1 r-4 r-5" {
		t.Fatalf("took %s, want r-1 r-4 r-5", got)
	}
}

// A call past the last of a test's recordings stays on its last, as a
// saturating cursor does, and does not take the next test's place.
func TestVariables_AnExtraCallStaysOnTheTestsLastRecording(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{session: []*models.Mock{meAt("a-1", 1, "tA"), meAt("a-2", 2, "tA"), meAt("b-1", 3, "tB"), meAt("b-2", 4, "tB")}}
	loginAs(t, h, db, "tA", "LA")
	a := getMe(t, h, db, "LA", 3)
	loginAs(t, h, db, "tB", "LB")
	if b := getMe(t, h, db, "LB", 2); a != "a-1 a-2 a-2" || b != "b-1 b-2" {
		t.Fatalf("test A took %s, test B %s; want a-1 a-2 a-2, b-1 b-2", a, b)
	}
}

// Recordings of one call whose recorded requests differ only at their
// variable places (two charges, each with a fresh key the dependency echoes)
// are taken in record order, each once, and the last answers a call past
// them.
func TestVariables_EchoRecordingsOfOneCallAnswerInRecordOrder(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	echo := func(name, key string, seq int) *models.Mock {
		return reusable(at(varMock(name, "POST", "http://pay/charge", `{"key":"`+key+`"}`, 200, `{"echo":"`+key+`","n":"`+name+`"}`,
			models.MockVariables{"key": {Recorded: key, Bind: "body.key", Answer: []string{"body.echo"}, Echo: true}}), seq))
	}
	db := &varDb{session: []*models.Mock{echo("mock-b", "k-2", 2), echo("mock-a", "k-1", 1)}}
	var got []string
	for i, live := range []string{"x-1", "x-2", "x-3"} {
		ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/charge", `{"key":"`+live+`"}`))
		if !ok {
			t.Fatalf("charge %d was a miss: %+v", i+1, diag)
		}
		got = append(got, stub.Spec.HTTPResp.Body)
	}
	want := []string{`{"echo":"x-1","n":"mock-a"}`, `{"echo":"x-2","n":"mock-b"}`, `{"echo":"x-3","n":"mock-b"}`}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("charges answered %v, want %v", got, want)
	}
}

// A call whose set of fitting recordings grows from one call to the next
// carries on through them in record order: the first two charges (no X-Auth)
// fit only the two recordings made without it, the next two fit all four (as
// TestMatch_StatefulCursorKeyIsTheRequest pins for identical recordings).
func TestVariables_ACallWhoseFittingRecordingsGrowCarriesOn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	echo := func(name, key string, seq int, auth bool) *models.Mock {
		m := varMock(name, "POST", "http://pay/charge", `{"key":"`+key+`"}`, 200, `{"echo":"`+key+`","n":"`+name+`"}`,
			models.MockVariables{"key": {Recorded: key, Bind: "body.key", Answer: []string{"body.echo"}, Echo: true}})
		if auth {
			m.Spec.HTTPReq.Header["X-Auth"] = "t"
		}
		return reusable(at(m, seq))
	}
	db := &varDb{session: []*models.Mock{echo("e-1", "k-1", 1, false), echo("e-2", "k-2", 2, false), echo("e-3", "k-3", 3, true), echo("e-4", "k-4", 4, true)}}
	var got []string
	for i, auth := range []bool{false, false, true, true} {
		r := liveReq("POST", "/charge", `{"key":"x-`+strconv.Itoa(i)+`"}`)
		if auth {
			r.header.Set("X-Auth", "t")
		}
		ok, stub, diag := matchVar(t, h, db, r)
		if !ok {
			t.Fatalf("charge %d was a miss: %+v", i+1, diag)
		}
		got = append(got, stub.Name)
	}
	if g := strings.Join(got, " "); g != "e-1 e-2 e-3 e-4" {
		t.Fatalf("charges answered by %s, want e-1 e-2 e-3 e-4", g)
	}
}

// Two callers of one request, each with its own token, take their own
// recordings in turn however their calls interleave: a call's turn is among
// the recordings whose variables hold for it, not a count shared with the
// other caller's (one count per request handed caller B's first call the
// count's first place, and caller A then never got a-1).
func TestVariables_TwoCallersTakeTheirOwnRecordingsInTurn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	me := func(name string, seq int, v, rec string) *models.Mock {
		m := varMock(name, "GET", "http://svc/me", "", 200, `{"me":"`+name+`"}`,
			models.MockVariables{v: useV(rec, []string{"header.X-Tok"})})
		m.Spec.HTTPReq.Header["X-Tok"] = rec
		return reusable(at(m, seq))
	}
	login := func(v, rec string) *models.Mock {
		return at(varMock("login-"+v, "POST", "http://svc/login", `{"tok":"`+rec+`"}`, 200, `{}`,
			models.MockVariables{v: bindV(rec, "body.tok")}), 0)
	}
	db := &varDb{
		perTest: []*models.Mock{login("tokA", "tA"), login("tokB", "tB")},
		session: []*models.Mock{me("a-1", 1, "tokA", "tA"), me("a-2", 2, "tokA", "tA"), me("b-1", 3, "tokB", "tB"), me("b-2", 4, "tokB", "tB")},
	}
	for _, live := range []string{"LA", "LB"} {
		if ok, _, diag := matchVar(t, h, db, liveReq("POST", "http://svc/login", `{"tok":"`+live+`"}`)); !ok {
			t.Fatalf("login %s missed: %+v", live, diag)
		}
	}
	var got []string
	for _, live := range []string{"LB", "LA", "LA", "LB"} {
		r := liveReq("GET", "http://svc/me", "")
		r.header.Set("X-Tok", live)
		ok, stub, diag := matchVar(t, h, db, r)
		if !ok {
			t.Fatalf("a call with %s missed: %+v", live, diag)
		}
		got = append(got, stub.Name)
	}
	if g := strings.Join(got, " "); g != "b-1 a-1 a-2 b-2" {
		t.Fatalf("served %s, want b-1 a-1 a-2 b-2", g)
	}
}

// With stateful mocks off, the first recording that fits and holds answers
// every call, as it does without variables.
func TestVariables_StatefulMocksOffServeTheFirstRecording(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{session: []*models.Mock{
		pollAt("a-1", 1, "pending", models.MockVariables{"token": bindV("T", "body.token")}),
		pollAt("a-2", 2, "done", models.MockVariables{"token": useV("T", []string{"body.token"})}),
	}}
	for i := 0; i < 2; i++ {
		ok, stub, _, err := h.match(context.Background(), liveReq("POST", "http://svc/poll", `{"token":"L"}`), db, nil, nil, nil, true, false, false, false, true)
		if err != nil || !ok || stub.Name != "a-1" {
			t.Fatalf("call %d: ok=%v stub=%v err=%v, want a-1", i+1, ok, stub, err)
		}
	}
}

// Only reusable stateful recordings take turns: a session recording that
// replays the same answer every time (ConsumeReuse) is not one of them, so
// calls past the stateful ones stay on their last.
func TestVariables_OnlyStatefulRecordingsTakeTurns(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	same := pollAt("s-3", 3, "same", models.MockVariables{"token": useV("T", []string{"body.token"})})
	same.TestModeInfo.Consume = models.ConsumeReuse
	db := &varDb{session: []*models.Mock{
		pollAt("a-1", 1, "pending", models.MockVariables{"token": bindV("T", "body.token")}),
		pollAt("a-2", 2, "done", models.MockVariables{"token": useV("T", []string{"body.token"})}),
		same,
	}}
	if got := servedNames(t, newHTTP(), db, `{"token":"L"}`, 3); strings.Join(got, " ") != "a-1 a-2 a-2" {
		t.Fatalf("served %v, want a-1 a-2 a-2", got)
	}
}
