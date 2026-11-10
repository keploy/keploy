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

// caseBindsProxy is a proxy serving the pool exactly, staged with test-1's
// one call, which carries variable key (recorded app-1) with use places only,
// as a client test case's variable is written; its learn step has begun no
// test, so it asks the ingress for no request of its own. open opens test-1's
// view and begins its variables, as the agent's begin does once it gave the
// proxy the test's cases (ProvenBegin: TakeClientCases, then the view).
func caseBindsProxy(t *testing.T) (p *Proxy, mm *MockManager, logs *observer.ObservedLogs, open func()) {
	t.Helper()
	own := varMockOf("own-1", "/apps", "app-1", true, models.LifetimePerTest)
	mm = stagedManager(t, []*models.Mock{own}, nil)
	mm.NewVariableSet()
	mm.ServeExactPool(models.PoolKeyNone, nil)
	core, logs := observer.New(zap.WarnLevel)
	return &Proxy{mockManager: mm, learnTracker: learn.NewTracker(), logger: zap.New(core)}, mm, logs,
		func() { mm.OpenExactTest("test-1", []*models.Mock{own}, nil) }
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
// it, and one carrying the recorded value is not. In the agent's order, the
// test's cases given before its view opens (ProvenBegin), the variable is
// noted as unbound from the scope's first, saying where the test binds it,
// and nothing is warned: a call that uses it before its request came misses
// with that reason, and one after binds.
func TestAClientTestsRequestBindsTheVariableBoundAtItsTestCase(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	at := time.Date(2026, 11, 9, 7, 1, 42, 0, time.UTC)
	p, mm, logs, open := caseBindsProxy(t)
	p.TakeClientCases("test-1", []models.ClientCase{appsCase("post-apps-1", keyAtName)})
	open()
	if logs.Len() != 0 {
		t.Fatalf("warned at the test's begin %v", logs.All())
	}
	if !p.WantsIncoming() {
		t.Fatal("a variable is bound at a request not yet paired: the proxy asks the ingress for none")
	}
	if why := mm.vars.Unbound("key"); !strings.Contains(why, "test case post-apps-1's request, at body.name, and no request of the test reached the application") {
		t.Fatalf("before its request came the variable is noted %q", why)
	}
	if got := postKey(t, mm, "/apps", "app-2"); strings.HasPrefix(got, "200 ") {
		t.Fatalf("a call before the request that binds its variable was answered %s", got)
	}
	p.TakeIncoming(liveApps(`{"name":"app-2"}`), "", nil, at)
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

// The notes a test's cases leave for its scope are its own: another test's
// opening drops them, so its scope is noted nothing, and the test's next
// opening, with none given again, is noted nothing either. Cases given with
// no variable bound at them leave none, and drop what was left.
func TestACaseBindsNoteIsLeftForItsOwnTestsScopeOnly(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p, mm, logs, open := caseBindsProxy(t)
	p.TakeClientCases("test-1", []models.ClientCase{appsCase("post-apps-1", keyAtName)})
	mm.OpenExactTest("test-2", nil, nil)
	if why := mm.vars.Unbound("key"); why != "" {
		t.Fatalf("another test's scope is noted %q", why)
	}
	open()
	if why := mm.vars.Unbound("key"); why != "" {
		t.Fatalf("the test's scope, opened after another's took its notes, is noted %q", why)
	}
	mm.ServeExactPool(models.PoolKeyNone, nil)
	p.TakeClientCases("test-1", []models.ClientCase{appsCase("post-apps-1", keyAtName)})
	p.TakeClientCases("test-1", []models.ClientCase{appsCase("post-apps-1")})
	open()
	if why := mm.vars.Unbound("key"); why != "" || logs.Len() != 0 {
		t.Fatalf("cases with no variable bound at them left the earlier cases' note: %q, warned %v", why, logs.All())
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
			p, mm, logs, open := caseBindsProxy(t)
			p.TakeClientCases("test-1", tc.cases)
			open()
			for _, l := range tc.live {
				p.TakeIncoming(l, tc.unread, nil, at)
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
	p, mm, _, open := caseBindsProxy(t)
	p.TakeClientCases("test-1", []models.ClientCase{appsCase("post-apps-1", keyAtName)})
	open()
	p.TakeIncoming(liveApps(`{"name":"app-2"}`), "", nil, at)
	if _, ok := mm.vars.Lookup("key"); ok || p.WantsIncoming() || mm.vars.Unbound("key") != "" {
		t.Fatal("the switch off: a client test's request bound or noted its variable, or the proxy asks for requests")
	}

	t.Setenv(provenswitch.Env, "1")
	p, mm, _, open = caseBindsProxy(t)
	p.TakeClientCases("test-1", []models.ClientCase{appsCase("post-apps-1", keyAtName)})
	open()
	t.Setenv(provenswitch.Env, "")
	p.TakeIncoming(liveApps(`{"name":"app-2"}`), "", nil, at)
	t.Setenv(provenswitch.Env, "1")
	if _, ok := mm.vars.Lookup("key"); ok || !p.WantsIncoming() {
		t.Fatal("a request taken with the switch off bound its test's variable, or was paired")
	}

	p, _, _, _ = caseBindsProxy(t)
	p.TakeClientCases("test-1", []models.ClientCase{appsCase("post-apps-1")})
	if p.WantsIncoming() {
		t.Fatal("no variable is bound at the test's requests: the proxy asks for them")
	}

	p, mm, logs, open := caseBindsProxy(t)
	open()
	p.TakeClientCases("test-2", []models.ClientCase{appsCase("post-apps-1", keyAtName, models.ClientCaseBind{Name: "key2", Recorded: "app-9", Place: "body.name"})})
	p.TakeIncoming(liveApps(`{"name":"app-2"}`), "", nil, at)
	if _, ok := mm.vars.Lookup("key"); ok || mm.vars.Unbound("key") != "" || mm.vars.Unbound("key2") != "" {
		t.Fatal("a test keploy does not serve bound or noted a variable in the served one")
	}
	if logs.Len() != 2 {
		t.Fatalf("warned %v; want the bind and the note of the request refused", logs.All())
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
	_, mm, _, open = caseBindsProxy(t)
	open()
	mm.ServeExactPool(models.PoolKeyNone, nil)
	if err := mm.NoteExactTest("", "key", "why"); err == nil || mm.vars.Unbound("key") != "" {
		t.Fatalf("a note naming no test, after a test's variables began and the pool came back, was taken: %v", err)
	}

	p = &Proxy{learnTracker: learn.NewTracker()}
	p.TakeClientCases("test-1", []models.ClientCase{appsCase("post-apps-1", keyAtName)})
	p.TakeIncoming(liveApps(`{"name":"app-2"}`), "", nil, at)
	if p.WantsIncoming() {
		t.Fatal("no mock manager: the request did not pair")
	}
}

// A request whose body is still coming pairs as it arrives and binds its
// variable once its body ended (TakeComing's done), never before: a call
// before then misses. One whose body did not come whole binds nothing and
// notes why.
func TestAComingRequestBindsItsVariableOnceItsBodyEnded(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	at := time.Date(2026, 11, 9, 7, 1, 42, 0, time.UTC)
	p, mm, _, open := caseBindsProxy(t)
	p.learnTracker.Begin()
	p.TakeClientCases("test-1", []models.ClientCase{appsCase("post-apps-1", keyAtName)})
	open()
	if done, _ := p.TakeComing(&models.HTTPReq{Method: "GET", URL: "http://api/other"}, at, nil); done != nil {
		t.Fatal("a request that pairs with none is taken as coming")
	}
	live := liveApps("")
	sofar := learn.NewRequestWords()
	done, words := p.TakeComing(live, at, func() *learn.RequestWords { return sofar })
	if done == nil || !words || p.WantsIncoming() {
		t.Fatal("a coming request did not pair, or the learn step that took it reads no words")
	}
	if got := p.learnTracker.Placed().Earlier; len(got) != 1 || got[0].Words() != sofar {
		t.Fatalf("the learn step is not told the words that passed of the coming body: %+v", got)
	}
	if _, ok := mm.vars.Lookup("key"); ok {
		t.Fatal("bound before its body came")
	}
	if why := mm.vars.Unbound("key"); !strings.Contains(why, "test case post-apps-1's request, at body.name, which bound nothing this run: keploy could not read it: "+learn.ComingUnread) {
		t.Fatalf("while its body came the variable is noted %q", why)
	}
	if got := postKey(t, mm, "/apps", "app-2"); strings.HasPrefix(got, "200 ") {
		t.Fatalf("a call before its body ended was answered %s", got)
	}
	done(liveApps(`{"name":"app-2"}`), "", nil)
	if b, ok := mm.vars.Lookup("key"); !ok || b.Value != "app-2" || b.Mock != "test case post-apps-1" || b.Place != "request body.name" {
		t.Fatalf("bound %+v (%v) once its body ended", b, ok)
	}
	if got := postKey(t, mm, "/apps", "app-2"); got != `200 {"echo":"app-2"}` {
		t.Fatalf("the call after its body ended was answered %s", got)
	}
	if got := p.learnTracker.Placed().Earlier; len(got) != 1 || got[0].Unread != "" || got[0].Mock != "test case post-apps-1" {
		t.Fatalf("the learn step placed %+v once its body ended", got)
	}

	// Under the switch off it takes nothing.
	p, _, _, open = caseBindsProxy(t)
	p.TakeClientCases("test-1", []models.ClientCase{appsCase("post-apps-1", keyAtName)})
	open()
	t.Setenv(provenswitch.Env, "0")
	if done, words := p.TakeComing(liveApps(""), at, nil); done != nil || words {
		t.Fatal("the switch off took a coming request")
	}
	t.Setenv(provenswitch.Env, "1")

	p, mm, _, open = caseBindsProxy(t)
	p.TakeClientCases("test-1", []models.ClientCase{appsCase("post-apps-1", keyAtName)})
	open()
	// No test of the learn step is open: the request pairs for its
	// variables alone, and no words of its body are read for it.
	done, words = p.TakeComing(liveApps(""), at, nil)
	if done == nil || words {
		t.Fatalf("a request that pairs for its variables alone: taken %v, its words read %v", done != nil, words)
	}
	done(liveApps(""), "its body did not come whole: unexpected EOF", nil)
	if _, ok := mm.vars.Lookup("key"); ok {
		t.Fatal("a body that did not come whole bound its variable")
	}
	if why := mm.vars.Unbound("key"); !strings.Contains(why, "did not come whole") {
		t.Fatalf("noted %q", why)
	}
}

// A variable bound in a header or the URL of a request binds as the request
// arrives, though its body is still coming, or is one keploy could not
// read: keploy has its URL and headers whole. The call the application
// makes from them before the body ended is answered. Once the body ended
// the variable stays as bound, bound once; one bound in the body binds
// then, and not where keploy could not read the body.
func TestAComingRequestBindsAVariableOfItsHeadersAsItArrives(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	at := time.Date(2026, 11, 9, 7, 1, 42, 0, time.UTC)
	keyAtHeader := models.ClientCaseBind{Name: "key", Recorded: "app-1", Place: "header.X-App"}
	idAtPath := models.ClientCaseBind{Name: "id", Recorded: "apps", Place: "url.path[0]"}
	otherAtName := models.ClientCaseBind{Name: "other", Recorded: "app-1", Place: "body.name"}
	recorded := func() models.ClientCase {
		c := appsCase("post-apps-1", keyAtHeader, idAtPath, otherAtName)
		c.Req.Header["X-App"] = "app-1"
		return c
	}
	sent := func(body string) *models.HTTPReq {
		live := liveApps(body)
		live.Header["X-App"] = "app-2"
		return live
	}
	bound := func(t *testing.T, mm *MockManager, name, value, place string) {
		t.Helper()
		if b, ok := mm.vars.Lookup(name); !ok || b.Value != value || b.Mock != "test case post-apps-1" || b.Place != "request "+place {
			t.Fatalf("%s bound %+v (%v), want %q at %s", name, b, ok, value, place)
		}
	}
	for _, end := range []string{"", "its body is larger than keploy reads"} {
		p, mm, logs, open := caseBindsProxy(t)
		p.learnTracker.Begin()
		p.TakeClientCases("test-1", []models.ClientCase{recorded()})
		open()
		done, _ := p.TakeComing(sent(""), at, nil)
		if done == nil {
			t.Fatal("the coming request did not pair")
		}
		bound(t, mm, "key", "app-2", "header.X-App")
		bound(t, mm, "id", "apps", "url.path[0]")
		if _, ok := mm.vars.Lookup("other"); ok {
			t.Fatal("a variable of the body bound before the body came")
		}
		if why := mm.vars.Unbound("other"); !strings.Contains(why, "at body.name, which bound nothing this run: keploy could not read it: "+learn.ComingUnread) {
			t.Fatalf("while the body came its variable is noted %q", why)
		}
		if got := postKey(t, mm, "/apps", "app-2"); got != `200 {"echo":"app-2"}` {
			t.Fatalf("the call made from the headers before the body ended was answered %s", got)
		}
		// The body ends with another value in the header's place: what
		// bound as the request arrived stays.
		whole := sent(`{"name":"app-3"}`)
		whole.Header["X-App"] = "app-9"
		done(whole, end, nil)
		bound(t, mm, "key", "app-2", "header.X-App")
		if _, ok := mm.vars.Lookup("other"); ok != (end == "") {
			t.Fatalf("the body ended %q: its variable bound %v", end, ok)
		}
		if end == "" {
			bound(t, mm, "other", "app-3", "body.name")
		} else if why := mm.vars.Unbound("other"); !strings.Contains(why, "keploy could not read it: "+end) {
			t.Fatalf("a body keploy could not read: its variable is noted %q", why)
		}
		if logs.Len() != 0 {
			t.Fatalf("binding at the headers, then at the body, warned: %+v", logs.All())
		}
	}

	// A request that came whole whose body keploy could not read binds the
	// variables of its URL and headers too, and none of its body.
	p, mm, _, open := caseBindsProxy(t)
	p.TakeClientCases("test-1", []models.ClientCase{recorded()})
	open()
	p.TakeIncoming(sent(""), "its body is encoded", nil, at)
	bound(t, mm, "key", "app-2", "header.X-App")
	if _, ok := mm.vars.Lookup("other"); ok {
		t.Fatal("a body keploy could not read bound its variable")
	}
	// A place that does not read is no place of the headers.
	if atHead(&models.HTTPReq{URL: "http://api/apps"}, "trailer.X") {
		t.Fatal("a place that does not read is read as one of the headers")
	}
}

// Two requests of one call that two workers of a test send, reaching the
// application in the other order than in its recording, each pair with
// their own recorded request by its fixed word, as the learn step paired
// them: the variable bound at the second binds from the run's request that
// holds its word, though it came first, and the other request leaves it. A
// request keploy could not read pairs by order, as the learn step's does.
func TestSwappedRequestsOfOneCallBindAtTheirOwnTestCase(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	at := time.Date(2026, 11, 9, 7, 1, 42, 0, time.UTC)
	cases := func() []models.ClientCase {
		first, second := appsCase("post-apps-1"), appsCase("post-apps-2", keyAtName)
		first.Req.Body, second.Req.Body = `{"kind":"alpha","name":"app-0"}`, `{"kind":"beta","name":"app-1"}`
		return []models.ClientCase{first, second}
	}
	p, mm, logs, open := caseBindsProxy(t)
	p.TakeClientCases("test-1", cases())
	open()
	p.TakeIncoming(liveApps(`{"kind":"beta","name":"app-2"}`), "", nil, at)
	if b, ok := mm.vars.Lookup("key"); !ok || b.Value != "app-2" || b.Mock != "test case post-apps-2" {
		t.Fatalf("the second test case's request, sent first, bound %+v (%v); want app-2 at test case post-apps-2", b, ok)
	}
	if p.WantsIncoming() {
		t.Fatal("the request the variable is bound at was paired: the proxy still asks for requests")
	}
	p.TakeIncoming(liveApps(`{"kind":"alpha","name":"app-3"}`), "", nil, at.Add(time.Millisecond))
	if b, _ := mm.vars.Lookup("key"); b.Value != "app-2" {
		t.Fatalf("the first test case's request, sent second, bound %+v", b)
	}
	if got := postKey(t, mm, "/apps", "app-2"); got != `200 {"echo":"app-2"}` {
		t.Fatalf("the call carrying the value its own request sent was answered %s", got)
	}
	if logs.Len() != 0 {
		t.Fatalf("warned %v", logs.All())
	}

	p, mm, _, open = caseBindsProxy(t)
	p.TakeClientCases("test-1", cases())
	open()
	p.TakeIncoming(liveApps(`{"kind":"beta","name":"app-2"}`), "its body is larger than keploy reads", nil, at)
	if _, ok := mm.vars.Lookup("key"); ok || !p.WantsIncoming() {
		t.Fatal("a request keploy could not read paired by its body's words, not with the first test case in order")
	}
	p.TakeIncoming(liveApps(`{"kind":"alpha","name":"app-3"}`), "", nil, at.Add(time.Millisecond))
	if b, ok := mm.vars.Lookup("key"); !ok || b.Value != "app-3" || b.Mock != "test case post-apps-2" {
		t.Fatalf("the request left for the second test case bound %+v (%v); want app-3", b, ok)
	}
}
