package proxy

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// caseBindsProxy is a proxy serving test-1 exactly, whose one call carries
// variable key (recorded app-1) with use places only, as a client test
// case's variable is written; its learn step has begun no test, so it asks
// the ingress for no request of its own.
func caseBindsProxy(t *testing.T) (*Proxy, *MockManager, *observer.ObservedLogs) {
	t.Helper()
	own := varMockOf("own-1", "/apps", "app-1", true, models.LifetimePerTest)
	mm := stagedManager(t, []*models.Mock{own}, nil)
	mm.NewVariableSet()
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.OpenExactTest("test-1", []*models.Mock{own}, nil)
	core, logs := observer.New(zap.WarnLevel)
	return &Proxy{mockManager: mm, learnTracker: learn.NewTracker(), logger: zap.New(core)}, mm, logs
}

// appsCase is test case name's recorded POST /apps request, sending app-1
// as its body's name, with binds bound at it.
func appsCase(name string, binds ...models.ClientCaseBind) models.ClientCase {
	return models.ClientCase{Name: name, Binds: binds, Req: models.HTTPReq{Method: "POST", URL: "http://api/apps",
		Header: map[string]string{"Content-Type": "application/json"}, Body: `{"name":"app-1"}`}}
}

// liveApps is a POST /apps request of the run sending body.
func liveApps(body string) *models.HTTPReq {
	return &models.HTTPReq{Method: "POST", URL: "http://api/apps", Header: map[string]string{"Content-Type": "application/json"}, Body: body}
}

var keyAtName = models.ClientCaseBind{Name: "key", Recorded: "app-1", Place: "body.name"}

// A variable a client test's request bound in its recording binds from the
// request of the run that pairs with it, before the application is given
// it: the call that carries the value this run's test sent is answered with
// it, and one carrying the recorded value is not. Until the request comes,
// the variable is noted as unbound, saying where the test binds it.
func TestAClientTestsRequestBindsTheVariableBoundAtItsTestCase(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	at := time.Date(2026, 11, 9, 7, 1, 42, 0, time.UTC)
	p, mm, logs := caseBindsProxy(t)
	p.TakeClientCases("test-1", []models.ClientCase{appsCase("post-apps-1", keyAtName)})
	if !p.WantsIncoming() {
		t.Fatal("a variable is bound at a request not yet paired: the proxy asks the ingress for none")
	}
	if why := mm.vars.Unbound("key"); !strings.Contains(why, "test case post-apps-1's request, at body.name, and no request of the test reached the application") {
		t.Fatalf("before its request came the variable is noted %q", why)
	}
	p.TakeIncoming(liveApps(`{"name":"app-2"}`), "", at)
	if p.WantsIncoming() {
		t.Fatal("every request a variable is bound at was paired: the proxy still asks for requests")
	}
	b, ok := mm.vars.Lookup("key")
	if !ok || b.Value != "app-2" || b.Recorded != "app-1" || b.Mock != "test case post-apps-1" || b.Place != "request body.name" || !b.Given {
		t.Fatalf("bound %+v (%v); want app-2 given by test case post-apps-1's request body.name", b, ok)
	}
	if why := mm.vars.Unbound("key"); why != "" {
		t.Fatalf("a bound variable is noted %q", why)
	}
	if got := postKey(t, mm, "/apps", "app-1"); strings.HasPrefix(got, "200 ") {
		t.Fatalf("a call carrying the recorded value, not the one the test sent, was answered %s", got)
	}
	if got := postKey(t, mm, "/apps", "app-2"); got != `200 {"echo":"app-2"}` {
		t.Fatalf("the call carrying the value the test sent was answered %s", got)
	}
	if logs.Len() != 0 {
		t.Fatalf("warned %v", logs.All())
	}
}

// A request binds nothing where keploy cannot show it holds the value the
// recording's request held: none of the recorded requests pairs with it, it
// holds no value at the place, its recorded request does not hold the
// variable's recorded value there, or keploy could not read it. Each leaves
// the variable unbound, noted with why, so every call that uses it misses.
// The n-th request of a key pairs with the n-th recorded one.
func TestAClientTestsRequestBindsNothingItCannotShow(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	at := time.Date(2026, 11, 9, 7, 1, 42, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		cases  []models.ClientCase
		live   []*models.HTTPReq
		unread string
		bound  string
		note   string
	}{
		{name: "no pair", cases: []models.ClientCase{appsCase("post-apps-1", keyAtName)},
			live: []*models.HTTPReq{{Method: "POST", URL: "http://api/suites", Body: `{"name":"app-2"}`}}, note: "no request of the test reached the application"},
		{name: "no value there", cases: []models.ClientCase{appsCase("post-apps-1", keyAtName)},
			live: []*models.HTTPReq{liveApps(`{"other":"app-2"}`)}, note: "which bound nothing this run: it holds no value at body.name this run"},
		{name: "empty value there", cases: []models.ClientCase{appsCase("post-apps-1", keyAtName)},
			live: []*models.HTTPReq{liveApps(`{"name":""}`)}, note: "it holds no value at body.name this run"},
		{name: "another recorded value", cases: []models.ClientCase{appsCase("post-apps-1", models.ClientCaseBind{Name: "key", Recorded: "app-9", Place: "body.name"})},
			live: []*models.HTTPReq{liveApps(`{"name":"app-2"}`)}, note: `does not hold the variable's recorded value "app-9" at body.name`},
		{name: "unread", cases: []models.ClientCase{appsCase("post-apps-1", keyAtName)}, unread: "its body is encoded",
			live: []*models.HTTPReq{liveApps(`{"name":"app-2"}`)}, note: "which bound nothing this run: keploy could not read it: its body is encoded"},
		{name: "second of a key", cases: []models.ClientCase{appsCase("post-apps-1"), appsCase("post-apps-2", keyAtName)},
			live: []*models.HTTPReq{liveApps(`{"name":"app-2"}`), liveApps(`{"name":"app-3"}`)}, bound: "app-3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, mm, logs := caseBindsProxy(t)
			p.TakeClientCases("test-1", tc.cases)
			for _, l := range tc.live {
				p.TakeIncoming(l, tc.unread, at)
			}
			b, ok := mm.vars.Lookup("key")
			if tc.bound != "" {
				if !ok || b.Value != tc.bound {
					t.Fatalf("bound %+v (%v); want %s", b, ok, tc.bound)
				}
				return
			}
			if ok {
				t.Fatalf("bound %+v", b)
			}
			if why := mm.vars.Unbound("key"); !strings.Contains(why, tc.note) {
				t.Fatalf("noted %q; want %q", why, tc.note)
			}
			if logs.Len() != 0 {
				t.Fatalf("warned %v: the note is what says why", logs.All())
			}
		})
	}
}

// The proxy asks the ingress for requests only under the proven switch and
// while a request a variable is bound at is not paired, and a request taken
// with the switch off neither pairs nor binds: a test whose
// requests bind none asks for none (its learn step aside). A test the proxy
// does not serve binds and notes nothing, and says so; a proxy that serves
// no replay pairs the requests all the same, and binds nothing.
func TestAClientTestsRequestsAreTakenOnlyWhereTheyBind(t *testing.T) {
	at := time.Date(2026, 11, 9, 7, 1, 42, 0, time.UTC)
	t.Setenv(provenswitch.Env, "")
	p, mm, _ := caseBindsProxy(t)
	p.TakeClientCases("test-1", []models.ClientCase{appsCase("post-apps-1", keyAtName)})
	p.TakeIncoming(liveApps(`{"name":"app-2"}`), "", at)
	if _, ok := mm.vars.Lookup("key"); ok || p.WantsIncoming() || mm.vars.Unbound("key") != "" {
		t.Fatal("the switch off: a client test's request bound or noted its variable, or the proxy asks for requests")
	}

	t.Setenv(provenswitch.Env, "1")
	p, mm, _ = caseBindsProxy(t)
	p.TakeClientCases("test-1", []models.ClientCase{appsCase("post-apps-1", keyAtName)})
	t.Setenv(provenswitch.Env, "")
	p.TakeIncoming(liveApps(`{"name":"app-2"}`), "", at)
	t.Setenv(provenswitch.Env, "1")
	if _, ok := mm.vars.Lookup("key"); ok || !p.WantsIncoming() {
		t.Fatal("a request taken with the switch off bound its test's variable, or was paired")
	}

	p, _, _ = caseBindsProxy(t)
	p.TakeClientCases("test-1", []models.ClientCase{appsCase("post-apps-1")})
	if p.WantsIncoming() {
		t.Fatal("no variable is bound at the test's requests: the proxy asks for them")
	}

	p, mm, logs := caseBindsProxy(t)
	p.TakeClientCases("test-2", []models.ClientCase{appsCase("post-apps-1", keyAtName, models.ClientCaseBind{Name: "key2", Recorded: "app-9", Place: "body.name"})})
	p.TakeIncoming(liveApps(`{"name":"app-2"}`), "", at)
	if _, ok := mm.vars.Lookup("key"); ok || mm.vars.Unbound("key") != "" || mm.vars.Unbound("key2") != "" {
		t.Fatal("a test keploy does not serve bound or noted a variable in the served one")
	}
	if logs.Len() != 4 {
		t.Fatalf("warned %v; want the two notes, the bind and the note of the request refused", logs.All())
	}
	for _, e := range logs.All() {
		if !strings.Contains(e.ContextMap()["why"].(string), "test-2 is not the test keploy serves now") {
			t.Fatalf("warned %v", e.ContextMap())
		}
	}

	mm = stagedManager(t, nil, nil)
	mm.NewVariableSet()
	mm.ServeExactPool(models.PoolKeyNone, nil)
	if err := mm.NoteExactTest("", "key", "why"); err == nil {
		t.Fatal("a note with no test open was taken")
	}
	mm.ServeExactTest("test-1", nil)
	if err := mm.NoteExactTest("test-1", "key", "why"); err == nil || !strings.Contains(err.Error(), "no scope of test-1's variables has begun") {
		t.Fatalf("a note before the test's variables began: %v", err)
	}
	_, mm, _ = caseBindsProxy(t)
	mm.ServeExactPool(models.PoolKeyNone, nil)
	if err := mm.NoteExactTest("", "key", "why"); err == nil || mm.vars.Unbound("key") != "" {
		t.Fatalf("a note naming no test, after a test's variables began and the pool came back, was taken: %v", err)
	}

	p = &Proxy{learnTracker: learn.NewTracker()}
	p.TakeClientCases("test-1", []models.ClientCase{appsCase("post-apps-1", keyAtName)})
	p.TakeIncoming(liveApps(`{"name":"app-2"}`), "", at)
	if p.WantsIncoming() {
		t.Fatal("no mock manager: the request did not pair")
	}
}
