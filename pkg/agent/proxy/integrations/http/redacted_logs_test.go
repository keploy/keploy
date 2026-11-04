package http

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// untoldStore wraps a store and forwards none of its optional methods, so
// it does not tell the recordings that answered (AnsweredRedacting).
type untoldStore struct{ integrations.MockMemDb }

// The request matcher's error line for a call body that is JSON but no
// object (bodyMatch) names the body redacted by every recording of the
// pool, as the call's miss is: the candidate compared redacted nothing,
// while another recording of the pool, or one that answered a call before
// and is only on the store's answered list, redacted the key the call sends
// its secret under. The rest of the body still shows. A store that does not
// tell its answered recordings has the body redacted whole, and so does the
// exported PerformBodyMatch, whose caller hands no pool.
func TestTheBodyErrorLineIsRedactedByThePool(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	jsonHdr := map[string]string{"Content-Type": "application/json"}
	body := `[{"assertion":"` + liveSecret + `","note":"kept1"}]`
	for _, c := range []struct {
		name  string
		whole bool
	}{
		{"pool", false},
		{"answered-before", false},
		{"a-store-that-does-not-tell", true},
		{"exported", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			core, logs := observer.New(zap.DebugLevel)
			h := newHTTP()
			h.Logger = zap.New(core)
			orders := recorded("orders", "/orders", jsonHdr, `{"item":"alpha1"}`)
			login := recorded("login", "/login", jsonHdr, `{"assertion":"REDACTED_abc","user":"u1"}`)
			login.Noise = []string{`^REDACTED_abc$`}
			var store integrations.MockMemDb
			switch c.name {
			case "pool":
				store = &mockMemDb{mocks: []*models.Mock{orders, login}, updateUnFilteredReturn: true}
			case "answered-before":
				store = answeredStore{mockMemDb: &mockMemDb{mocks: []*models.Mock{orders}, updateUnFilteredReturn: true}, answered: []*models.Mock{login}, complete: true}
			case "a-store-that-does-not-tell":
				store = untoldStore{&mockMemDb{mocks: []*models.Mock{orders, login}, updateUnFilteredReturn: true}}
			}
			if c.name == "exported" {
				if _, err := h.PerformBodyMatch(context.Background(), []*models.Mock{orders}, []byte(body)); err != nil {
					t.Fatalf("PerformBodyMatch: %v", err)
				}
			} else {
				_, _, _, _ = h.match(context.Background(), sent("/orders", jsonHdr, body), store, nil, nil, nil, true, false, true, true, true)
			}
			lines := logs.FilterMessage("failed to unmarshal the request body").All()
			if len(lines) != 1 {
				t.Fatalf("logged %d lines for the body that is no object, want 1", len(lines))
			}
			got, _ := lines[0].ContextMap()["Req"].(string)
			switch {
			case c.whole && got != valueRedacted:
				t.Errorf("the line names the body %q, want it redacted whole", got)
			case !c.whole && !strings.Contains(got, "kept1"):
				t.Errorf("the line names the body %q, want the field no recording redacted shown", got)
			}
			loggedNone(t, logs, liveSecret)
		})
	}
}

// The debug line of each variable a served call binds writes the value
// redacted at a place a recording of the pool redacted, though the
// recording that answers redacted nothing there: another of the pool did,
// or one that answered a call before and is only on the store's answered
// list. A value at a place no recording redacted shows.
func TestTheVariableBoundLineIsRedactedByThePool(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, answered := range []bool{false, true} {
		t.Run(map[bool]string{false: "pool", true: "answered-before"}[answered], func(t *testing.T) {
			core, logs := observer.New(zap.DebugLevel)
			h := newHTTP()
			h.Logger = zap.New(core)
			m := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"ord-rec-1","item":"pen"}`, 201, `{}`,
				models.MockVariables{"orderId": bindV("ord-rec-1", "body.id"), "item": bindV("pen", "body.item")}), 1)
			login := at(varMock("login", "POST", "http://store/login", `{"id":"REDACTED_abc"}`, 200, `{}`, nil), 2)
			login.Noise = []string{`^REDACTED_abc$`}
			db := &varDb{perTest: []*models.Mock{m}}
			var store integrations.MockMemDb = answeredVarDb{varDb: db, answered: []*models.Mock{login}}
			if !answered {
				db.perTest = append(db.perTest, login)
				store = db
			}
			ok, _, _, err := h.match(context.Background(), liveReq("POST", "/orders", `{"id":"`+liveBound+`","item":"pencil"}`), store, nil, nil, nil, true, false, false, true, true)
			if err != nil || !ok {
				t.Fatalf("the binding call should be served: %v, %v", ok, err)
			}
			wantBound(t, logs, map[string]string{"orderId": valueRedacted, "item": "pencil"})
			loggedNone(t, logs, liveBound)
		})
	}
}

// wantBound checks the values the "variable bound" lines wrote.
func wantBound(t *testing.T, logs *observer.ObservedLogs, want map[string]string) {
	t.Helper()
	got := map[string]string{}
	for _, e := range logs.FilterMessage("variable bound").All() {
		v, _ := e.ContextMap()["variable"].(string)
		got[v], _ = e.ContextMap()["value"].(string)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("logged the bound values as %v, want %v", got, want)
			return
		}
	}
}

// A call served through the strict index (strictIndexed), which reads no
// pool, logs each bound value redacted by the recordings of the pool the
// store holds (storePlaces): another recording of the generation redacted
// the place, the one that answers did not.
func TestTheVariableBoundLineOfAnIndexedCallIsRedactedByTheStoresPool(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	core, logs := observer.New(zap.DebugLevel)
	h := newHTTP()
	h.Logger = zap.New(core)
	login := at(varMock("login", "POST", "http://store/login", `{"id":"REDACTED_abc"}`, 200, `{}`, nil), 4)
	login.Noise = []string{`^REDACTED_abc$`}
	db := &genVarDb{varDb: &varDb{
		perTest: []*models.Mock{
			at(varMock("ping", "GET", "http://store/ping", "", 200, `{}`, nil), 1),
			at(varMock("bind", "POST", "http://store/orders", `{"id":"ord-rec-1","item":"pen"}`, 201, `{"id":"ord-rec-1"}`,
				models.MockVariables{"orderId": bindV("ord-rec-1", "body.id", "body.id"), "item": bindV("pen", "body.item")}), 2),
		},
		session: []*models.Mock{login},
	}, gen: 1}
	for _, c := range []struct{ method, path, body, want string }{
		{"GET", "/ping", "", "ping"},
		{"POST", "/orders", `{"id":"` + liveBound + `","item":"pen"}`, "bind"},
	} {
		ok, stub, _, err := h.matchOf(context.Background(), liveReq(c.method, c.path, c.body), db, db, nil, nil, nil, true, false, false, true, true)
		if err != nil || !ok || stub.Name != c.want {
			t.Fatalf("%s %s: served %v (ok %v, err %v); want %s", c.method, c.path, stub, ok, err, c.want)
		}
	}
	if !h.strictIx.has {
		t.Fatal("the generation's index was not built: the row is of a call served through it")
	}
	wantBound(t, logs, map[string]string{"orderId": valueRedacted, "item": "pen"})
	loggedNone(t, logs, liveBound)
}

// failingDb is a store whose per-test or session read fails.
type failingDb struct {
	*varDb
	perTest, session bool
}

func (d failingDb) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	if d.perTest {
		return nil, errors.New("per-test read failed")
	}
	return d.varDb.GetPerTestMocksInWindow()
}

func (d failingDb) GetSessionMocks() ([]*models.Mock, error) {
	if d.session {
		return nil, errors.New("session read failed")
	}
	return d.varDb.GetSessionMocks()
}

// storePlaces reads the places of the store's per-test and session
// recordings and of those that answered before, and every place when either
// read fails or the store does not tell what answered.
func TestStorePlacesReadsTheStoresPool(t *testing.T) {
	perTest := recorded("orders", "/orders", nil, `{"item":"alpha1"}`)
	perTest.Noise = []string{`^alpha1$`}
	session := recorded("login", "/login", nil, `{"assertion":"REDACTED_abc"}`)
	session.Noise = []string{`^REDACTED_abc$`}
	db := &varDb{perTest: []*models.Mock{perTest}, session: []*models.Mock{session}}
	p := storePlaces(failingDb{varDb: db}, nil, "/orders")
	if p.body || p.urlUnread || !p.field("item") || !p.field("assertion") || p.field("note") {
		t.Errorf("read the store's pool as body %v, url unread %v, fields %v", p.body, p.urlUnread, p.fields)
	}
	// The recordings that answered before count, and a store that does not
	// tell them has every place redacted.
	answered := answeredVarDb{varDb: &varDb{perTest: []*models.Mock{perTest}}, answered: []*models.Mock{session}}
	if p := storePlaces(answered, nil, "/orders"); p.body || !p.field("assertion") {
		t.Errorf("a recording that answered before: body %v, fields %v; want its field redacted", p.body, p.fields)
	}
	answered.hiding = true
	if p := storePlaces(answered, nil, "/orders"); !p.body || !p.urlUnread {
		t.Errorf("a store that does not tell: body %v, url unread %v; want every place", p.body, p.urlUnread)
	}
	for _, f := range []failingDb{{varDb: db, perTest: true}, {varDb: db, session: true}} {
		if p := storePlaces(f, nil, "/orders"); !p.body || !p.urlUnread {
			t.Errorf("a store whose read fails (per-test %v, session %v): body %v, url unread %v; want every place", f.perTest, f.session, p.body, p.urlUnread)
		}
	}
}

// Another parser's call served by the variable pass (VariableMatch) logs
// each bound value redacted at a place a recording of the pool it passed
// redacted, though the one that answers did not.
func TestVariableMatch_TheBoundLineIsRedactedByThePool(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	core, logs := observer.New(zap.DebugLevel)
	h := newHTTP()
	h.Logger = zap.New(core)
	m := sqsSend("mock-1", "dedup-rec", models.MockVariables{"dedup": bindV("dedup-rec", "body.MessageDeduplicationId")})
	other := sqsSend("mock-3", "REDACTED_xyz", nil)
	other.Spec.HTTPReq.Body = strings.Replace(other.Spec.HTTPReq.Body, "order 1", "order 3", 1)
	other.Noise = []string{`^REDACTED_xyz$`}
	db := &varDb{perTest: []*models.Mock{m}}
	consume := func(x *models.Mock) bool { return db.DeleteFilteredMock(*x) }
	res, err := h.VariableMatch(context.Background(), ReadVariableScope(db), sqsCall(liveBound), sdkNoise(), []*models.Mock{m, other}, db, consume, nil)
	if err != nil || res.Outcome != VariableServed {
		t.Fatalf("the row is of a variable bound by a served call: %+v, %v", res, err)
	}
	wantBound(t, logs, map[string]string{"dedup": valueRedacted})
	loggedNone(t, logs, liveBound)
}

// Another parser's call handed with no URL is read as one with an empty URL
// (no path), by the pass and by its why alike: it binds a variable, and a
// later call that breaks it gets a why that carries no secret, with no
// panic.
func TestVariableMatch_ACallWithNoURL(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	const rec = "REDACTED_abc"
	send := func(name string, vars models.MockVariables) *models.Mock {
		m := sqsSend(name, rec, vars)
		m.Noise = []string{`^REDACTED_abc$`}
		return m
	}
	m := send("mock-1", models.MockVariables{"dedup": bindV(rec, "body.MessageDeduplicationId")})
	db := &varDb{perTest: []*models.Mock{m}}
	consume := func(x *models.Mock) bool { return db.DeleteFilteredMock(*x) }
	// match is VariableMatch of the call dedup with no URL; a panic fails
	// the row.
	match := func(dedup string, pool []*models.Mock) (res VariableResult, err error) {
		c := sqsCall(dedup)
		c.URL = nil
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("VariableMatch of a call with no URL panicked: %v", r)
			}
		}()
		return h.VariableMatch(context.Background(), ReadVariableScope(db), c, sdkNoise(), pool, db, consume, nil)
	}
	if res, err := match(liveBound, []*models.Mock{m}); err != nil || res.Outcome != VariableServed {
		t.Fatalf("the call with no URL should bind the variable: %+v, %v", res, err)
	}
	use := send("mock-2", models.MockVariables{"dedup": useV(rec, []string{"body.MessageDeduplicationId"})})
	res, err := match(liveSecret, []*models.Mock{use})
	if err != nil || res.Outcome != VariableExactOnly || !strings.Contains(res.Problem, "mock-2") {
		t.Fatalf("want the variable's why for mock-2, got %+v, %v", res, err)
	}
	if strings.Contains(res.Problem, liveSecret) || strings.Contains(res.Problem, liveBound) {
		t.Errorf("the why carries a secret: %s", res.Problem)
	}
}
