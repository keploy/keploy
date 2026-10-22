package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// learnCall sends one request through the HTTP integration in record's full
// round (OutgoingOptions.Learn), against a test whose one recorded call is
// rec, with the proxy's miss reporter and learn tracker as the proxy installs
// them. It returns the app's answer and the mock manager.
func learnCall(t *testing.T, p *Proxy, rec *models.Mock, request string) (*http.Response, string, *MockManager) {
	t.Helper()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	now := time.Now()
	rec.Spec.ReqTimestampMock = now
	mm.SetMocksWithWindow([]*models.Mock{rec}, nil, now.Add(-time.Hour), now.Add(time.Hour))
	app, agentEnd := net.Pipe()
	ctx := models.WithMockMismatchReporter(context.Background(), p.sendMockNotFoundError)
	ctx = learn.WithTracker(ctx, p.learnTracker)
	t.Cleanup(func() { _ = app.Close() })
	go func() {
		_ = httpint.New(zap.NewNop()).MockOutgoing(ctx, agentEnd, &models.ConditionalDstCfg{}, mm, models.OutgoingOptions{Learn: true})
		_ = agentEnd.Close()
	}()
	if _, err := app.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(body), mm
}

func batchMock() *models.Mock {
	return &models.Mock{
		Name: "mock-7",
		Kind: models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": models.HTTPClient},
			HTTPReq: &models.HTTPReq{Method: "POST", URL: "/batch", Header: map[string]string{"Host": "deps.example", "Content-Type": "application/json"},
				Body: `{"rid":"r-1","ids":[1,2,3]}`},
			HTTPResp: &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json", "Content-Length": "23"}, Body: `{"ok":true,"rid":"r-1"}`},
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest},
	}
}

func post(body string) string {
	return "POST /batch HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: " +
		strconv.Itoa(len(body)) + "\r\n\r\n" + body
}

// A request the learn step refuses on the order rule is a miss of its test,
// with its reason and the candidate it refused: judging's unstable_order.
func TestALearnStepMissCarriesItsReason(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	p.ContinueTestErrorCapture()
	resp, _, _ := learnCall(t, p, batchMock(), post(`{"rid":"r-1","ids":[3,1,2]}`))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("the app got %d, want 502", resp.StatusCode)
	}
	misses, err := p.GetMockErrors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(misses) != 1 || misses[0].Reason != models.MissUnstableOrder || misses[0].ClosestMock != "mock-7" {
		t.Fatalf("misses %+v", misses)
	}
	if !strings.Contains(misses[0].NextSteps, "body.ids has the recorded elements in another order") {
		t.Fatalf("next steps %q", misses[0].NextSteps)
	}
}

// A request that drifted only in a value is answered from the mock it drifted
// from, and the consumed mock carries what was learned, which the replay
// writes onto the recording; the echo is rendered into the answer.
func TestALearnStepPairingAnswersAndLearns(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	p.ContinueTestErrorCapture()
	resp, body, mm := learnCall(t, p, batchMock(), post(`{"rid":"r-2","ids":[1,2,3]}`))
	if resp.StatusCode != http.StatusOK || body != `{"ok":true,"rid":"r-2"}` {
		t.Fatalf("the app got %d %s", resp.StatusCode, body)
	}
	if misses, _ := p.GetMockErrors(context.Background()); len(misses) != 0 {
		t.Fatalf("misses %+v", misses)
	}
	consumed := mm.GetConsumedMocks()
	if len(consumed) != 1 {
		t.Fatalf("consumed %+v", consumed)
	}
	want := models.MockVariables{"rid": {Recorded: "r-1", Bind: "body.rid", Answer: []string{"body.rid"}, Echo: true}}
	if !reflect.DeepEqual(consumed[0].Variables, want) {
		t.Fatalf("the consumed mock's state carries %+v", consumed[0].Variables)
	}
}

// A value one call of the test sent and a later call uses is followed as a
// variable: the later call is answered with it, and its consumed state says
// where it is used and where it is bound (Binds), which the replay writes onto
// both recordings.
func TestALearnStepFollowsAVariableAcrossCalls(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	p.ContinueTestErrorCapture()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	now := time.Now()
	call := func(name string, at time.Duration, method, target, body, resp string) *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": models.HTTPClient},
			HTTPReq:          &models.HTTPReq{Method: models.Method(method), URL: "http://deps.example" + target, Header: map[string]string{"Host": "deps.example", "Content-Type": "application/json"}, Body: body},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json", "Content-Length": strconv.Itoa(len(resp))}, Body: resp},
			ReqTimestampMock: now.Add(at),
		}, TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest}}
	}
	mm.SetMocksWithWindow([]*models.Mock{
		call("mock-a", 0, "POST", "/orders", `{"order":"o-1"}`, `{}`),
		call("mock-b", time.Second, "GET", "/orders/o-1", "", `{"id":"o-1"}`),
	}, nil, now.Add(-time.Hour), now.Add(time.Hour))
	send := func(request string) (int, string) {
		app, agentEnd := net.Pipe()
		ctx := models.WithMockMismatchReporter(context.Background(), p.sendMockNotFoundError)
		ctx = learn.WithTracker(ctx, p.learnTracker)
		defer app.Close()
		go func() {
			_ = httpint.New(zap.NewNop()).MockOutgoing(ctx, agentEnd, &models.ConditionalDstCfg{}, mm, models.OutgoingOptions{Learn: true})
			_ = agentEnd.Close()
		}()
		if _, err := app.Write([]byte(request)); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(app), nil)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	body := `{"order":"o-9"}`
	if code, _ := send("POST /orders HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body); code != http.StatusOK {
		t.Fatalf("the create got %d", code)
	}
	if code, got := send("GET /orders/o-9 HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n"); code != http.StatusOK || got != `{"id":"o-9"}` {
		t.Fatalf("the read got %d %s", code, got)
	}
	if misses, _ := p.GetMockErrors(context.Background()); len(misses) != 0 {
		t.Fatalf("misses %+v", misses)
	}
	states := map[string]models.MockState{}
	for _, st := range mm.GetConsumedMocks() {
		states[st.Name] = st
	}
	if want := (models.MockVariables{"order": {Recorded: "o-1", Use: []string{"url.path[1]"}, Answer: []string{"body.id"}}}); !reflect.DeepEqual(states["mock-b"].Variables, want) {
		t.Fatalf("mock-b's state carries %+v", states["mock-b"].Variables)
	}
	if want := (map[string]models.MockVariables{"mock-a": {"order": {Recorded: "o-1", Bind: "body.order"}}}); !reflect.DeepEqual(states["mock-b"].Binds, want) {
		t.Fatalf("mock-b's state binds %+v", states["mock-b"].Binds)
	}
}

// r3Mock is a recorded HTTP call as the recorder writes it.
func r3Mock(name string, at time.Time, method, target, body, resp string) *models.Mock {
	return &models.Mock{Version: "api.keploy.io/v1beta1", Name: name, Kind: models.HTTP, Spec: models.MockSpec{
		Metadata:         map[string]string{"type": models.HTTPClient},
		HTTPReq:          &models.HTTPReq{Method: models.Method(method), URL: "http://deps.example" + target, Header: map[string]string{"Host": "deps.example", "Content-Type": "application/json"}, Body: body},
		HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json", "Content-Length": strconv.Itoa(len(resp))}, Body: resp},
		ReqTimestampMock: at,
		ResTimestampMock: at.Add(time.Millisecond),
	}}
}

// r3Send sends one request through the HTTP integration with the proxy's
// tracker and miss reporter.
func r3Send(t *testing.T, p *Proxy, mm integrations.MockMemDb, opts models.OutgoingOptions, request string) (int, string) {
	t.Helper()
	app, agentEnd := net.Pipe()
	ctx := models.WithMockMismatchReporter(context.Background(), p.sendMockNotFoundError)
	ctx = learn.WithTracker(ctx, p.learnTracker)
	defer app.Close()
	go func() {
		_ = httpint.New(zap.NewNop()).MockOutgoing(ctx, agentEnd, &models.ConditionalDstCfg{}, mm, opts)
		_ = agentEnd.Close()
	}()
	if _, err := app.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(b)
}

func r3States(mm *MockManager) map[string]models.MockState {
	out := map[string]models.MockState{}
	for _, st := range mm.GetConsumedMocks() {
		out[st.Name] = st
	}
	return out
}

// With the default environment every HTTP mock loads as session lifetime,
// so keploy test stages every test's HTTP mocks in the session tier. The
// learn step still tells them apart: the open test's are the ones its
// mapping names (or recorded in its window); another test's is never paired,
// even in a full record (the round-3 reviewer's repro).
func TestALearnStepNeverPairsAnotherTestsRecording(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	t1 := r3Mock("mock-t1", now, "GET", "/users/u1", "", `{"name":"alice"}`)
	t2 := r3Mock("mock-t2", now.Add(time.Second), "GET", "/users/u2", "", `{"name":"bob"}`)
	for _, m := range []*models.Mock{t1, t2} {
		m.DeriveLifetime()
	}
	if t1.TestModeInfo.Lifetime != models.LifetimeSession {
		t.Fatalf("precondition: an HTTP mock loads as %v", t1.TestModeInfo.Lifetime)
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	session := pkg.FilterConfigMocksMapping(context.Background(), zap.NewNop(), []*models.Mock{t1, t2}, []string{"mock-t2"})
	mm.SetMocksWithWindow(nil, session, now.Add(500*time.Millisecond), now.Add(2*time.Second))
	p.ContinueTestErrorCapture() // test 2 begins

	code, body := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true},
		"GET /users/u2x HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n")
	st := r3States(mm)
	if _, learnedOnT1 := st["mock-t1"].ReqBodyNoise["url.path.1"]; learnedOnT1 || body == `{"name":"alice"}` {
		t.Fatalf("test 2's call was paired with test 1's recording: answered %d %s, mock-t1 learned %v", code, body, st["mock-t1"].ReqBodyNoise)
	}
	if code != http.StatusOK || body != `{"name":"bob"}` {
		t.Fatalf("test 2's call is its own recording's: %d %s", code, body)
	}
}

// A late call of test 1 made after test 2's window opened, before its mocks
// moved, still finds its earlier places in what test 1 served: the id that
// flowed from test 1's create is refused, never learned as noise.
func TestALearnStepLateCallKeepsItsEarlierPlaces(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	a := r3Mock("mock-a", now, "POST", "/orders", `{"order":"o-1"}`, `{}`)
	b := r3Mock("mock-b", now.Add(time.Second), "GET", "/orders/o-1", "", `{"status":"new"}`)
	for _, m := range []*models.Mock{a, b} {
		m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived = models.LifetimePerTest, true
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetMocksWithWindow([]*models.Mock{a, b}, nil, now.Add(-time.Hour), now.Add(time.Hour))
	mm.SetServedObserver(p.learnTracker.ServedMock) // as setMockManager installs it
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}

	p.ContinueTestErrorCapture() // test 1 begins
	body := `{"order":"o-9"}`
	if code, _ := r3Send(t, p, mm, opts, "POST /orders HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body); code != http.StatusOK {
		t.Fatalf("the create got %d", code)
	}
	if _, err := p.GetMockErrors(context.Background()); err != nil { // test 1 ends
		t.Fatal(err)
	}
	p.ContinueTestErrorCapture() // test 2's window opens; the mocks have not moved

	code, got := r3Send(t, p, mm, opts, "GET /orders/o-9 HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n")
	if _, noise := r3States(mm)["mock-b"].ReqBodyNoise["url.path.1"]; noise {
		t.Fatalf("the id that flowed from test 1's create became noise (answered %d %s)", code, got)
	}
}

// A reused pool mock (recorded outside every test's window, so a full
// record's to learn onto, and reusable) the learn step paired with an echo
// answers every call of the round: what was learned rides on the consumed
// state, not on the pooled recording. (A test's own mock answers one call of
// its test: TestALearnStepOwnMockAnswersOnceSoTheNextIsPaired.)
func TestALearnStepReusedMockWithAnEchoAnswersAgain(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	m := r3Mock("mock-0", now, "POST", "/ping", `{"rid":"r-1"}`, `{"rid":"r-1"}`)
	m.DeriveLifetime()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{{Start: now.Add(time.Minute), End: now.Add(2 * time.Minute)}})
	mm.SetMocksWithWindow(nil, []*models.Mock{m}, now.Add(-time.Hour), now.Add(time.Hour))
	p.ContinueTestErrorCapture()
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}
	for i, rid := range []string{"r-2", "r-3"} {
		body := `{"rid":"` + rid + `"}`
		code, got := r3Send(t, p, mm, opts, "POST /ping HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body)
		if code != http.StatusOK || got != body {
			misses, _ := p.GetMockErrors(context.Background())
			t.Fatalf("call %d to a reusable mock got %d %s; misses %+v", i+1, code, got, misses)
		}
	}
	if v := r3States(mm)["mock-0"].Variables; v["rid"].Bind != "body.rid" || !v["rid"].Echo {
		t.Fatalf("the consumed state carries the echo: %+v", v)
	}
}

// (Round-4 review repro.) an HTTP mock recorded before the first test, with per-test lifetime
// (what DeriveLifetime gives an HTTP_CLIENT mock under
// KEPLOY_STRICT_MOCK_WINDOW=1), is staged by the agent's own filter into the
// startup tier. The learn step pairs it (its IsFiltered makes it own) and
// serves it with an echo, but consumeWith -> DeleteFilteredMock ->
// deleteStartupMock flags the state from the TREE's mock (mk), not the copy
// carrying what was learned: the consumed state has neither the echo
// variable nor any learned noise, so the recording never gets it.
func TestALearnStepStartupTierMockKeepsWhatWasLearned(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	// The drifting ts shares no word with the answer: a word of a drift
	// that the call's own answer holds is refused (an echo keploy cannot
	// answer), never noise.
	boot := r3Mock("mock-boot", now, "POST", "/token", `{"nonce":"n-1","ts":"t-7"}`, `{"nonce":"n-1"}`)
	own := r3Mock("mock-t1", now.Add(2*time.Second), "GET", "/users/u1", "", `{}`)
	for _, m := range []*models.Mock{boot, own} {
		m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived = models.LifetimePerTest, true
	}
	start, end := now.Add(time.Second), now.Add(3*time.Second)
	perTest, promoted := pkg.FilterPerTestAndLaxPromotedTierAware(context.Background(), zap.NewNop(), []*models.Mock{boot, own}, start, end, false, start)
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedStartupCutoff(start)
	mm.SetMocksWithWindow(perTest, promoted, start, end)
	p.ContinueTestErrorCapture() // test 1 begins; the app's token call is lazy
	body := `{"nonce":"n-2","ts":"t-8"}`
	code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true},
		"POST /token HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body)
	if code != http.StatusOK || got != `{"nonce":"n-2"}` {
		t.Fatalf("precondition: the learn step pairs the start-up call and answers the echo: %d %s", code, got)
	}
	st := r3States(mm)["mock-boot"]
	if _, ok := st.ReqBodyNoise["body.ts"]; !ok || st.Variables["nonce"].Bind == "" {
		t.Fatalf("what F served the start-up call with is not on its consumed state: noise %v variables %+v", st.ReqBodyNoise, st.Variables)
	}
}

// (Round-4 review repro.) a reusable pool mock the learn step pairs twice in
// one test. The first call drifts the path id and the rid (both echoed), the
// second only the rid. SetLearned REPLACES what the tree's copy carried, so
// the consumed state ends with the rid echo alone: the path echo F served
// with is never written, and the next replay misses the first call.
func TestALearnStepReusedMockKeepsEveryEchoItWasServedWith(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	m := r3Mock("mock-0", now, "GET", "/users/u-1?rid=r-1", "", `{"id":"u-1","rid":"r-1"}`)
	m.DeriveLifetime()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{{Start: now.Add(time.Minute), End: now.Add(2 * time.Minute)}})
	mm.SetMocksWithWindow(nil, []*models.Mock{m}, now.Add(-time.Hour), now.Add(time.Hour))
	p.ContinueTestErrorCapture()
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}
	for _, c := range []struct{ target, want string }{
		{"/users/u-2?rid=r-2", `{"id":"u-2","rid":"r-2"}`},
		{"/users/u-1?rid=r-3", `{"id":"u-1","rid":"r-3"}`},
	} {
		code, got := r3Send(t, p, mm, opts, "GET "+c.target+" HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n")
		if code != http.StatusOK || got != c.want {
			t.Fatalf("precondition: %s answered %d %s", c.target, code, got)
		}
	}
	v := r3States(mm)["mock-0"].Variables
	if len(v) < 2 {
		t.Fatalf("the consumed state lost an echo F served the first call with: %+v", v)
	}
}

// (Round-4 review repro.) keploy test's per-test call in mapping mode sends the test's names
// merged with the set's start-up names (models.MergeStartupMockNames), so the
// agent's FilterConfigMocksMapping marks every start-up mock IsFiltered, and
// ownership() reads a start-up mock as the OPEN TEST'S OWN in every test. A
// start-up call made lazily inside a test is then paired in a partial record
// (LearnPool false), which design §7.5 says never changes the pool, and the
// start-up mock may carry that test's variables.
func TestALearnStepStartupMockIsNotTheOpenTestsOwn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	boot := r3Mock("mock-boot", now, "POST", "/token", `{"nonce":"n-1"}`, `{"token":"t"}`)
	t2 := r3Mock("mock-t2", now.Add(2*time.Second), "GET", "/users/u2", "", `{}`)
	for _, m := range []*models.Mock{boot, t2} {
		m.DeriveLifetime()
	}
	names := models.MergeStartupMockNames([]models.MockEntry{{Name: "mock-t2"}}, []string{"mock-boot"})
	session := pkg.FilterConfigMocksMapping(context.Background(), zap.NewNop(), []*models.Mock{boot, t2}, names)
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetMocksWithWindow(nil, session, now.Add(time.Second), now.Add(3*time.Second))
	// keploy test seeds every recorded test's window: the start-up's mock is
	// outside all of them.
	mm.SeedRecordedWindows([]models.TestWindow{{Start: now.Add(time.Second), End: now.Add(3 * time.Second)}})
	p.ContinueTestErrorCapture()
	body := `{"nonce":"n-2"}`
	code, _ := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: false},
		"POST /token HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body)
	if code == http.StatusOK {
		t.Fatalf("a partial record paired the start-up mock as the test's own: learned %v", r3States(mm)["mock-boot"].ReqBodyNoise)
	}
}

// (Round-5 review repro.) with the default environment an HTTP mock loads as
// session lifetime, reusable. A test sends one app-minted session id on two
// identical calls (recorded mock-a, mock-b). Each of the test's own mocks
// answers one call of it (design §5.2): the first call pairs mock-a and,
// with nothing earlier, learns the id as noise there; the second is not
// answered by mock-a through that noise, but pairs mock-b, where the id is
// the variable bound at mock-a. So CI refuses a second call that sends
// another id than the first.
func TestALearnStepOwnMockAnswersOnceSoTheNextIsPaired(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	mk := func(name string, at time.Time) *models.Mock {
		m := r3Mock(name, at, "POST", "/events", `{"session":"s-1","kind":"view"}`, `{"ok":true}`)
		m.DeriveLifetime()
		return m
	}
	a, b := mk("mock-a", now), mk("mock-b", now.Add(time.Second))
	if a.TestModeInfo.Lifetime != models.LifetimeSession {
		t.Fatalf("precondition: lifetime %v", a.TestModeInfo.Lifetime)
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetMocksWithWindow(nil, []*models.Mock{a, b}, now.Add(-time.Hour), now.Add(time.Hour))
	mm.SetServedObserver(p.learnTracker.ServedMock)
	p.ContinueTestErrorCapture()
	body := `{"session":"s-9","kind":"view"}`
	for i := 0; i < 2; i++ {
		code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true},
			"POST /events HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body)
		if code != http.StatusOK {
			misses, _ := p.GetMockErrors(context.Background())
			t.Fatalf("F call %d: %d %s %+v", i, code, got, misses)
		}
	}
	st := r3States(mm)
	if v := st["mock-b"].Variables["session"]; v.Recorded != "s-1" || len(v.Use) != 1 || v.Use[0] != "body.session" {
		t.Fatalf("the second call's id is the variable bound at mock-a: mock-b %+v", st["mock-b"])
	}
	if bv := st["mock-b"].Binds["mock-a"]["session"]; bv.Bind != "body.session" {
		t.Fatalf("the bind rides onto mock-a: %+v", st["mock-b"].Binds)
	}

	// A third identical call: both answered, so it is a miss, never mock-a
	// again through what F learned onto it.
	code, _ := r3Send(t, p, mm, models.OutgoingOptions{Learn: true},
		"POST /events HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body)
	if code == http.StatusOK {
		t.Fatal("a call more than the test recorded was answered by a mock that answered already")
	}
}

// (Round-5 review repro.) every HTTP mock loads as session lifetime by
// default, so test 1's recordings stay visible in test 2. A visible
// recording of another recorded test is no earlier place of test 2's calls:
// test 1's call in the same second does not refuse test 2's own clock read
// (request_changed), as the mocks served before test 2 began do not.
func TestALearnStepAnotherTestsVisibleRecordingIsNoSource(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	m1 := r3Mock("mock-t1", now.Add(500*time.Millisecond), "POST", "/ping", `{"at":1760436000}`, `{}`)
	m2 := r3Mock("mock-t2", now.Add(2500*time.Millisecond), "POST", "/pong", `{"at":1760436000}`, `{}`)
	for _, m := range []*models.Mock{m1, m2} {
		m.DeriveLifetime()
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{{Start: now, End: now.Add(time.Second)}, {Start: now.Add(2 * time.Second), End: now.Add(3 * time.Second)}})
	session := pkg.FilterConfigMocksMapping(context.Background(), zap.NewNop(), []*models.Mock{m1, m2}, []string{"mock-t2"})
	mm.SetMocksWithWindow(nil, session, now.Add(2*time.Second), now.Add(3*time.Second))
	mm.SetServedObserver(p.learnTracker.ServedMock)
	p.ContinueTestErrorCapture() // test 1 (nothing called)
	_, _ = p.GetMockErrors(context.Background())
	p.ContinueTestErrorCapture() // test 2
	body := `{"at":1760436093}`
	code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true},
		"POST /pong HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body)
	if code != http.StatusOK {
		misses, _ := p.GetMockErrors(context.Background())
		t.Fatalf("test 2's own clock read was refused because test 1's recording (same second) is visible: %d %s %+v", code, got, misses)
	}
	if _, ok := r3States(mm)["mock-t2"].ReqBodyNoise["body.at"]; !ok {
		t.Fatalf("test 2's clock read is noise: %+v", r3States(mm)["mock-t2"])
	}
}

// (Round-6 review repro, real MockManager with keploy test's
// staging: default session lifetime, the test's mapping, seeded windows.) A
// Jaeger-traced app sends its per-test trace id on both calls, each with a
// fresh span id. In F the second call's uber-trace-id shares the trace id
// with the first call's, next to a fresh span in both: it flowed, so the
// call is refused, never learned as noise.
func TestALearnStepATraceIdNextToAFreshSpanIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	T1, T2 := "4bf92f3577b34da6a3ce929d0e0e4736", "0af7651916cd43dd8448eb211c80319c"
	a := r3Mock("mock-a", now.Add(100*time.Millisecond), "GET", "/users/7", "", `{"id":7}`)
	b := r3Mock("mock-b", now.Add(200*time.Millisecond), "GET", "/orders/7", "", `{"orders":[]}`)
	a.Spec.HTTPReq.Header["Uber-Trace-Id"] = T1 + ":00f067aa0ba902b7:a1b2c3d4e5f60718:1"
	b.Spec.HTTPReq.Header["Uber-Trace-Id"] = T1 + ":11f067aa0ba902b8:a1b2c3d4e5f60718:1"
	for _, m := range []*models.Mock{a, b} {
		m.DeriveLifetime()
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{{Start: now, End: now.Add(time.Second)}})
	session := pkg.FilterConfigMocksMapping(context.Background(), zap.NewNop(), []*models.Mock{a, b}, []string{"mock-a", "mock-b"})
	mm.SetMocksWithWindow(nil, session, now, now.Add(time.Second))
	mm.SetServedObserver(p.learnTracker.ServedMock)
	p.ContinueTestErrorCapture()
	opts := models.OutgoingOptions{Learn: true}
	if code, got := r3Send(t, p, mm, opts, "GET /users/7 HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nUber-Trace-Id: "+T2+":b7ad6b7169203331:0102030405060708:1\r\n\r\n"); code != http.StatusOK {
		t.Fatalf("precondition: %d %s", code, got)
	}
	code, got := r3Send(t, p, mm, opts, "GET /orders/7 HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nUber-Trace-Id: "+T2+":c7ad6b7169203332:0102030405060708:1\r\n\r\n")
	st := r3States(mm) // GetConsumedMocks drains: read once
	if _, noise := st["mock-b"].ReqBodyNoise["header.uber-trace-id"]; noise || code == http.StatusOK {
		t.Fatalf("the trace id that flowed from mock-a was learned on mock-b (answered %d %s): mock-b noise %v", code, got, st["mock-b"].ReqBodyNoise)
	}
}

// (Round-6 review repro.) an EXTRA call identical to an own mock that
// answered already (a cold-cache race in F: two goroutines fetch the flags)
// is one call more than recorded (design §5.2): a miss. The spent mock still
// competes, so the call is never paired with a farther unanswered key-mate
// (same method, host and segment count), which would learn the path segment
// that tells the endpoints apart as noise onto that other recording.
func TestALearnStepAnExtraIdenticalCallIsAMiss(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	f := r3Mock("mock-flags", now.Add(100*time.Millisecond), "GET", "/v1/flags", "", `{"beta":true}`)
	pr := r3Mock("mock-profile", now.Add(200*time.Millisecond), "GET", "/v1/profile", "", `{"name":"alice"}`)
	for _, m := range []*models.Mock{f, pr} {
		m.DeriveLifetime()
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{{Start: now, End: now.Add(time.Second)}})
	session := pkg.FilterConfigMocksMapping(context.Background(), zap.NewNop(), []*models.Mock{f, pr}, []string{"mock-flags", "mock-profile"})
	mm.SetMocksWithWindow(nil, session, now, now.Add(time.Second))
	mm.SetServedObserver(p.learnTracker.ServedMock)
	p.ContinueTestErrorCapture()
	opts := models.OutgoingOptions{Learn: true}
	flags := "GET /v1/flags HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n"
	if code, got := r3Send(t, p, mm, opts, flags); code != http.StatusOK || got != `{"beta":true}` {
		t.Fatalf("precondition: %d %s", code, got)
	}
	code, got := r3Send(t, p, mm, opts, flags) // the extra call
	st := r3States(mm)
	if _, noise := st["mock-profile"].ReqBodyNoise["url.path.1"]; noise || got == `{"name":"alice"}` {
		t.Fatalf("the extra flags call was paired with mock-profile: answered %d %s, mock-profile learned %v", code, got, st["mock-profile"].ReqBodyNoise)
	}
}

// (Round-6 review repro.) default HTTP mocks load as session lifetime with
// cursor+saturate consumption. A test reads a counter three times (recorded
// answers 1, 2, 3) and reports the last value it read. The test's own
// recordings answer in recorded order, each once, with no stateful cursor
// over a group that answering shrinks: 1, 2, 3, so the report is the one
// recorded and nothing is learned on it.
func TestALearnStepOwnRecordingsAnswerInRecordedOrder(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	var all []*models.Mock
	for i, n := range []string{"1", "2", "3"} {
		all = append(all, r3Mock("mock-c"+n, now.Add(time.Duration(100*(i+1))*time.Millisecond), "GET", "/counter", "", `{"n":`+n+`}`))
	}
	all = append(all, r3Mock("mock-report", now.Add(500*time.Millisecond), "POST", "/report", `{"last":3}`, `{}`))
	var names []string
	for _, m := range all {
		m.DeriveLifetime()
		names = append(names, m.Name)
	}
	if all[0].TestModeInfo.Consume != models.ConsumeCursorSaturate {
		t.Fatalf("precondition: consume %v", all[0].TestModeInfo.Consume)
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{{Start: now, End: now.Add(time.Second)}})
	session := pkg.FilterConfigMocksMapping(context.Background(), zap.NewNop(), all, names)
	mm.SetMocksWithWindow(nil, session, now, now.Add(time.Second))
	mm.SetServedObserver(p.learnTracker.ServedMock)
	p.ContinueTestErrorCapture()
	opts := models.OutgoingOptions{Learn: true}
	var got []string
	for i := 0; i < 3; i++ {
		_, body := r3Send(t, p, mm, opts, "GET /counter HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n")
		got = append(got, body)
	}
	last := got[2][len(`{"n":`) : len(got[2])-1]
	report := `{"last":` + last + `}`
	_, _ = r3Send(t, p, mm, opts, "POST /report HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(report))+"\r\n\r\n"+report)
	if got[0] != `{"n":1}` || got[1] != `{"n":2}` || got[2] != `{"n":3}` {
		t.Fatalf("F answered the recorded reads out of order: %v; then mock-report learned %v", got, r3States(mm)["mock-report"].ReqBodyNoise)
	}
}

// (Round-7 review repro.) per-test mocks (KEPLOY_STRICT_MOCK_WINDOW's
// lifetime) are deleted from the store when they answer. An own mock that
// answered still competes as spent: an extra identical call is one call more
// than recorded, a miss, never paired with the farther key-mate of another
// endpoint (which would learn the path segment that tells them apart).
func TestALearnStepAConsumedPerTestMockStillCompetes(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	f := r3Mock("mock-flags", now.Add(100*time.Millisecond), "GET", "/v1/flags", "", `{"beta":true}`)
	pr := r3Mock("mock-profile", now.Add(200*time.Millisecond), "GET", "/v1/profile", "", `{"name":"alice"}`)
	for _, m := range []*models.Mock{f, pr} {
		m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived = models.LifetimePerTest, true
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{{Start: now, End: now.Add(time.Second)}})
	mm.SetMocksWithWindow([]*models.Mock{f, pr}, nil, now, now.Add(time.Second))
	mm.SetServedObserver(p.learnTracker.ServedMock)
	p.ContinueTestErrorCapture()
	opts := models.OutgoingOptions{Learn: true}
	flags := "GET /v1/flags HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n"
	if code, got := r3Send(t, p, mm, opts, flags); code != http.StatusOK || got != `{"beta":true}` {
		t.Fatalf("precondition: %d %s", code, got)
	}
	if per, _ := mm.GetPerTestMocksInWindow(); len(per) != 1 {
		t.Fatalf("precondition: the answered per-test mock left the store: %d left", len(per))
	}
	code, got := r3Send(t, p, mm, opts, flags) // the extra call
	st := r3States(mm)
	if _, noise := st["mock-profile"].ReqBodyNoise["url.path.1"]; noise || code == http.StatusOK {
		t.Fatalf("the extra flags call was paired with mock-profile: answered %d %s, mock-profile learned %v", code, got, st["mock-profile"].ReqBodyNoise)
	}
}

// (Round-7 review repro.) a protocol-phase mock (a handshake, a token
// fetch: type config) recorded inside the test's window is never the test's
// own: it answers any number of the test's identical requests, as the pool
// does (design §5.2), and is never paired.
func TestALearnStepAProtocolPhaseMockAnswersAgain(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	cfg := r3Mock("mock-flags", now.Add(100*time.Millisecond), "GET", "/v1/flags", "", `{"beta":true}`)
	cfg.Spec.Metadata["type"] = "config"
	cfg.Spec.Metadata["passthrough"] = string(models.PassThroughRecordOne)
	cfg.DeriveLifetime()
	if !cfg.IsProtocolPhase() {
		t.Fatal("precondition: protocol-phase")
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{{Start: now, End: now.Add(time.Second)}})
	session := pkg.FilterConfigMocksMapping(context.Background(), zap.NewNop(), []*models.Mock{cfg}, nil)
	mm.SetMocksWithWindow(nil, session, now, now.Add(time.Second))
	mm.SetServedObserver(p.learnTracker.ServedMock)
	p.ContinueTestErrorCapture()
	flags := "GET /v1/flags HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n"
	for i := 0; i < 2; i++ {
		if code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true}, flags); code != http.StatusOK {
			misses, _ := p.GetMockErrors(context.Background())
			t.Fatalf("call %d to a protocol-phase mock missed: %d %s %+v", i, code, got, misses)
		}
	}
}

// (Round-8 review.) per-test tier (consumed mocks are deleted), concurrent identical calls,
// more calls than recordings: each recording answers once, the extra calls
// miss, and the same-key recording of another endpoint is never paired
// (gone spent mocks still compete), under -race.
func TestALearnStepConcurrentExtraCallsOnPerTestMocks(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, calls := range []int{3, 5, 8} {
		for round := 0; round < 5; round++ {
			p := startedDrainProxy(t, 100)
			p.learnTracker = learn.NewTracker()
			now := time.Now()
			var all []*models.Mock
			var names []string
			for i := 0; i < 3; i++ {
				all = append(all, r3Mock("mock-c"+strconv.Itoa(i), now.Add(time.Duration(100*(i+1))*time.Millisecond), "GET", "/v1/counter", "", `{"n":`+strconv.Itoa(i)+`}`))
			}
			all = append(all, r3Mock("mock-other", now.Add(600*time.Millisecond), "GET", "/v1/other", "", `{"other":true}`))
			for _, m := range all {
				m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived = models.LifetimePerTest, true
				names = append(names, m.Name)
			}
			mm := NewMockManager(nil, nil, zap.NewNop())
			mm.SeedRecordedWindows([]models.TestWindow{{Start: now, End: now.Add(time.Second)}})
			perTest := pkg.FilterTcsMocksMapping(context.Background(), zap.NewNop(), all, names)
			mm.SetMocksWithWindow(perTest, nil, now, now.Add(time.Second))
			mm.SetServedObserver(p.learnTracker.ServedMock)
			p.ContinueTestErrorCapture()
			opts := models.OutgoingOptions{Learn: true}
			var wg sync.WaitGroup
			var mu sync.Mutex
			got := map[string]int{}
			misses := 0
			for i := 0; i < calls; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					code, body := r3Send(t, p, mm, opts, "GET /v1/counter HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n")
					mu.Lock()
					defer mu.Unlock()
					if code == http.StatusOK {
						got[body]++
					} else {
						misses++
					}
				}()
			}
			wg.Wait()
			st := r3States(mm)
			mm.Close()
			if len(st["mock-other"].ReqBodyNoise) > 0 || got[`{"other":true}`] > 0 {
				t.Fatalf("calls=%d: mock-other was paired: %v %v", calls, got, st["mock-other"].ReqBodyNoise)
			}
			for body, n := range got {
				if n > 1 {
					t.Fatalf("calls=%d: %s answered %d times: %v", calls, body, n, got)
				}
			}
			if want := calls - 3; misses != want {
				t.Fatalf("calls=%d: %d misses, want %d (%v)", calls, misses, want, got)
			}
		}
	}
}

// (Round-8 review.) a set seeded with no recorded windows leaves the learn
// step none, not the previous set's.
func TestSeedRecordedWindowsOfASetWithNoneForgetsThePrevious(t *testing.T) {
	now := time.Now()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{{Start: now, End: now.Add(time.Second)}})
	if inside, known := mm.InRecordedTestWindow(now.Add(time.Millisecond)); !inside || !known {
		t.Fatalf("precondition: inside %v known %v", inside, known)
	}
	mm.SeedRecordedWindows(nil)
	if _, known := mm.InRecordedTestWindow(now.Add(time.Millisecond)); known {
		t.Fatal("the previous set's windows outlived a set with none")
	}
}

// The learn step's recorded windows follow the set boundary as the release
// rule's do: a set's seed is installed by its staging call, with the trees it
// describes, and a replay session that seeds none (keploy mock replay, which
// sends its scope table instead) places no recording by the last session's,
// on the same agent; a session's parked seed whose staging never came is not
// the next one's.
func TestANewSessionPlacesNoRecordingByTheLastSessionsWindows(t *testing.T) {
	now := time.Now()
	in := now.Add(time.Millisecond)
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	// keploy test: reset, seed, stage.
	mm.ResetForReplaySession()
	mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-1", Start: now, End: now.Add(time.Second)}})
	if _, known := mm.InRecordedTestWindow(in); known {
		t.Fatal("a set's windows placed recordings before its staging call")
	}
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, now.Add(time.Hour))
	if inside, known := mm.InRecordedTestWindow(in); !inside || !known {
		t.Fatalf("after the set's staging: inside %v known %v", inside, known)
	}
	// keploy mock replay on the same agent: reset, stage, no windows.
	mm.ResetForReplaySession()
	if inside, known := mm.InRecordedTestWindow(in); !inside || !known {
		t.Fatal("before the next session's staging the manager no longer describes the last set")
	}
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, now.Add(time.Hour))
	if _, known := mm.InRecordedTestWindow(in); known {
		t.Fatal("the last session's windows place a recording of a session that sent none")
	}
	// A session whose staging never came leaves its seed to no one.
	mm.ResetForReplaySession()
	mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-2", Start: now, End: now.Add(time.Second)}})
	mm.ResetForReplaySession()
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, now.Add(time.Hour))
	if _, known := mm.InRecordedTestWindow(in); known {
		t.Fatal("an aborted session's windows were installed for the next one")
	}
	// A session that seeds its own places by them.
	mm.ResetForReplaySession()
	mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-9", Start: now.Add(time.Minute), End: now.Add(2 * time.Minute)}})
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, now.Add(time.Hour))
	if inside, known := mm.InRecordedTestWindow(in); inside || !known {
		t.Fatalf("by the new session's windows: inside %v known %v", inside, known)
	}
}

// Under exact serving (S12: keploy test's agent under the switch), what the
// learn step learned as a variable rides the consumed mock's state, as on
// every other door: persistLearned writes it onto the recording.
func TestALearnStepVariableRidesExactServing(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, exact := range []bool{false, true} {
		p := startedDrainProxy(t, 100)
		p.learnTracker = learn.NewTracker()
		p.ContinueTestErrorCapture()
		mm := NewMockManager(nil, nil, zap.NewNop())
		t.Cleanup(mm.Close)
		rec := batchMock()
		now := time.Now()
		rec.Spec.ReqTimestampMock = now
		if exact {
			mm.SetMocksWithWindow([]*models.Mock{rec}, nil, models.BaseTime, now.Add(time.Hour))
			mm.ServeExactTest("test-1", []*models.Mock{rec})
		} else {
			mm.SetMocksWithWindow([]*models.Mock{rec}, nil, now.Add(-time.Hour), now.Add(time.Hour))
		}
		mm.SetServedObserver(p.learnTracker.ServedMock)
		app, agentEnd := net.Pipe()
		ctx := models.WithMockMismatchReporter(context.Background(), p.sendMockNotFoundError)
		ctx = learn.WithTracker(ctx, p.learnTracker)
		go func() {
			_ = httpint.New(zap.NewNop()).MockOutgoing(ctx, agentEnd, &models.ConditionalDstCfg{}, mm, models.OutgoingOptions{Learn: true})
			_ = agentEnd.Close()
		}()
		if _, err := app.Write([]byte(post(`{"rid":"r-2","ids":[1,2,3]}`))); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(app), nil)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		_ = app.Close()
		consumed := mm.GetConsumedMocks()
		if resp.StatusCode != http.StatusOK || len(consumed) != 1 || len(consumed[0].Variables) == 0 {
			t.Fatalf("exact=%v: answered %d %s; consumed %+v: the learned echo does not ride the state", exact, resp.StatusCode, body, consumed)
		}
	}
}

// Staged as the agent stages a set under its default strict mock window: a
// test's own recordings stay on disk until that test opens, and the trees
// hold the pool. At start-up a pool refresh recorded between tests, whose
// token test 1's own login answered, is refused as it is with every
// recording in memory: the learn step reads every recording made before the
// pool's, the on-disk ones too (MockManager.RecordedBefore over the agent's
// DiskMocks), and the token came from the login.
func TestAPoolMocksEarlierPlacesAreReadFromTheDiskToo(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	base := time.Now().Add(-time.Hour)
	w1 := models.TestWindow{TestCase: "test-1", Start: base.Add(time.Minute), End: base.Add(2 * time.Minute)}
	w2 := models.TestWindow{TestCase: "test-2", Start: base.Add(4 * time.Minute), End: base.Add(5 * time.Minute)}
	login := r3Mock("mock-1", w1.Start.Add(time.Second), "POST", "/v1/login", `{"user":"u-7"}`, `{"token":"tok-0c41b2"}`)
	login.TestModeInfo.Lifetime = models.LifetimePerTest
	refresh := r3Mock("mock-2", base.Add(3*time.Minute), "POST", "/v1/sessions/refresh", `{"token":"tok-0c41b2"}`, `{"ok":true}`)
	refresh.DeriveLifetime()
	disk, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disk.Close() })
	if err := disk.Add(login); err != nil {
		t.Fatal(err)
	}
	disk.Finalize()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{w1, w2})
	mm.SetMocksWithWindow(nil, []*models.Mock{refresh}, models.BaseTime, time.Now())
	mm.SetRecordedBefore(disk.RangeRecordedBefore)
	mm.ServeExactPool(models.PoolKeyNone, []string{"mock-2"})
	body := `{"token":"tok-9d8e7f"}`
	code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true},
		"POST /v1/sessions/refresh HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body)
	if code == http.StatusOK && got == `{"ok":true}` {
		t.Fatalf("the refresh was answered from the pool's recording, its token learned as noise: %+v", r3States(mm)["mock-2"].ReqBodyNoise)
	}
	if noise := r3States(mm)["mock-2"].ReqBodyNoise; len(noise) != 0 {
		t.Fatalf("learned %v onto the pool's recording", noise)
	}
}

// A test's own call reads its earlier places in its own test, as before: a
// row of another test the agent keeps on disk is no earlier place of it, so
// a value that coincides with one is still learned.
func TestATestsOwnCallIsNotRefusedForAnotherTestsRowOnDisk(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	base := time.Now().Add(-time.Hour)
	w1 := models.TestWindow{TestCase: "test-1", Start: base.Add(time.Minute), End: base.Add(2 * time.Minute)}
	w2 := models.TestWindow{TestCase: "test-2", Start: base.Add(4 * time.Minute), End: base.Add(5 * time.Minute)}
	other := r3Mock("mock-1", w1.Start.Add(time.Second), "GET", "/v1/notes", "", `{"note":"n-0c41b2"}`)
	other.TestModeInfo.Lifetime = models.LifetimePerTest
	own := r3Mock("mock-5", w2.Start.Add(time.Second), "POST", "/v1/notes", `{"note":"n-0c41b2"}`, `{"ok":true}`)
	own.TestModeInfo.Lifetime = models.LifetimePerTest
	disk, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disk.Close() })
	for _, m := range []*models.Mock{other, own} {
		if err := disk.Add(m); err != nil {
			t.Fatal(err)
		}
	}
	disk.Finalize()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{w1, w2})
	mm.SetMocksWithWindow([]*models.Mock{own}, nil, w2.Start, w2.End)
	mm.SetRecordedBefore(disk.RangeRecordedBefore)
	mm.ServeExactTest("test-2", []*models.Mock{own})
	p.ContinueTestErrorCapture()
	body := `{"note":"n-9d8e7f"}`
	code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true},
		"POST /v1/notes HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body)
	if code != http.StatusOK || got != `{"ok":true}` {
		t.Fatalf("the test's own call was refused (%d %s): another test's row on disk read as its earlier place", code, got)
	}
}

// Through the HTTP integration, the call's window ends when decodeHTTP had
// the request whole: a start-up pool call's clock read in it is learned as
// noise, where the lead alone (its minute and second words of the
// receive's Date header) refuses it.
func TestAStartUpPoolCallsClockReadIsReadInTheWindowDecodeHTTPEnds(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	iso := func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }
	rec := time.Now().Add(-time.Hour).Truncate(time.Second)
	receive := r3Mock("mock-0", rec, "POST", "/queue/receive", `{"n":10}`, `{"Messages":[{"Body":"m-1"}]}`)
	receive.Spec.HTTPResp.Header["Date"] = rec.Format(http.TimeFormat)
	event := r3Mock("mock-1", rec.Add(4*time.Millisecond), "POST", "/v1/events", `{"processed_at":"`+iso(rec.Add(3*time.Millisecond))+`"}`, `{"ok":true}`)
	event.DeriveLifetime()
	receive.DeriveLifetime()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-1", Start: rec.Add(time.Minute), End: rec.Add(2 * time.Minute)}})
	mm.SetMocksWithWindow(nil, []*models.Mock{receive, event}, models.BaseTime, time.Now())
	mm.ServeExactPool(models.PoolKeyNone, []string{"mock-1"})
	served := time.Now()
	offset := rec.Add(learn.FullRoundLead + 2*time.Second).Sub(served)
	p.learnTracker.SetAppClock(func(real time.Time) (time.Time, bool) { return real.Add(offset), true })
	p.learnTracker.ServedMockAt(receive, served)
	body := `{"processed_at":"` + iso(served.Add(offset)) + `"}`
	code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true},
		"POST /v1/events HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body)
	if code != http.StatusOK || got != `{"ok":true}` {
		t.Fatalf("answered %d %s; want the pool recording, its clock read learned", code, got)
	}
	if noise := r3States(mm)["mock-1"].ReqBodyNoise; noise["body.processed_at"] == nil {
		t.Fatalf("learned %v; want body.processed_at", noise)
	}
}

// (LEARN-vs-NOTOWNED) In record's full round under exact serving, a call
// that is another test's recording exactly (a poll of test A's made in test
// B's window) is that recording made elsewhere, not test B's own call
// changed: it did not change. The learn step names it (NotOwned, which
// judging reads to bake it into the worker scope, S12) before it pairs the
// call with B's recording of the same operation: it never answers it with
// B's recording, nor learns the value that tells the two apart as noise
// there, which every later replay would then pass on.
func TestALearnStepNamesACallMadeElsewhereBeforePairingIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	a := r3Mock("mock-a", now, "GET", "/v1/jobs?cursor=c1", "", `{"jobs":["j1"]}`)
	b := r3Mock("mock-b", now.Add(time.Second), "GET", "/v1/jobs?cursor=c2", "", `{"jobs":[]}`)
	for _, m := range []*models.Mock{a, b} {
		m.DeriveLifetime()
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{
		{Start: now.Add(-100 * time.Millisecond), End: now.Add(500 * time.Millisecond)},
		{Start: now.Add(900 * time.Millisecond), End: now.Add(1500 * time.Millisecond)},
	})
	mm.SetMocksWithWindow(nil, []*models.Mock{a, b}, models.BaseTime, now.Add(time.Hour))
	mm.SetServedObserver(p.learnTracker.ServedMock)
	mm.ServeExactTest("test-b", []*models.Mock{b})
	p.ContinueTestErrorCapture() // test B begins

	code, body := r3Send(t, p, mm, models.OutgoingOptions{Learn: true},
		"GET /v1/jobs?cursor=c1 HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n")
	st := r3States(mm)
	if _, noise := st["mock-b"].ReqBodyNoise["url.query.cursor"]; noise || code == http.StatusOK {
		t.Fatalf("test A's call made in test B was answered from test B's recording (%d %s), which learned %v", code, body, st["mock-b"].ReqBodyNoise)
	}
	misses, err := p.GetMockErrors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(misses) != 1 || !misses[0].NotOwned || misses[0].ClosestMock != "mock-a" {
		t.Fatalf("the call made elsewhere is a miss naming mock-a as not owned; misses %+v", misses)
	}

	// B's own call, made after it, is still B's recording's.
	code, body = r3Send(t, p, mm, models.OutgoingOptions{Learn: true},
		"GET /v1/jobs?cursor=c2 HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n")
	if code != http.StatusOK || body != `{"jobs":[]}` {
		t.Fatalf("test B's own call: %d %s", code, body)
	}
}

// pollsOfAAndB stages, under exact serving, test A's poll (mock-a, cursor=c1)
// and test B's poll of the same operation (mock-b, cursor=c2), each recorded
// in its test's window, for record's full round.
func pollsOfAAndB(t *testing.T) (*Proxy, *MockManager, *models.Mock, *models.Mock) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	a := r3Mock("mock-a", now, "GET", "/v1/jobs?cursor=c1", "", `{"jobs":["j1"]}`)
	b := r3Mock("mock-b", now.Add(time.Second), "GET", "/v1/jobs?cursor=c2", "", `{"jobs":[]}`)
	for _, m := range []*models.Mock{a, b} {
		m.DeriveLifetime()
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{
		{Start: now.Add(-100 * time.Millisecond), End: now.Add(500 * time.Millisecond)},
		{Start: now.Add(900 * time.Millisecond), End: now.Add(1500 * time.Millisecond)},
	})
	mm.SetMocksWithWindow(nil, []*models.Mock{a, b}, models.BaseTime, now.Add(time.Hour))
	mm.SetServedObserver(p.learnTracker.ServedMock)
	return p, mm, a, b
}

const pollOfA = "GET /v1/jobs?cursor=c1 HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n"

// (LEARN-vs-NOTOWNED) A call that is another test's recording once more than
// it holds is that recording made again, not the open test's call changed:
// the same poll of test A's made twice in test B's window (A recorded it
// once), or A's poll made in A and then again in B's window. The first call
// made elsewhere is named not owned (judging bakes it into the worker
// scope); one more is a miss naming the recording it repeats. Neither is
// answered with B's recording, and B's recording learns nothing from
// either: every later replay would pass whatever cursor B sent.
func TestALearnStepRefusesACallMadeElsewhereOnceMoreThanItsRecordingHolds(t *testing.T) {
	t.Run("two polls of A in B's window", func(t *testing.T) {
		p, mm, _, b := pollsOfAAndB(t)
		mm.ServeExactTest("test-b", []*models.Mock{b})
		p.ContinueTestErrorCapture()
		c1, b1 := r3Send(t, p, mm, models.OutgoingOptions{Learn: true}, pollOfA)
		c2, b2 := r3Send(t, p, mm, models.OutgoingOptions{Learn: true}, pollOfA)
		st := r3States(mm)
		if _, noise := st["mock-b"].ReqBodyNoise["url.query.cursor"]; noise || c1 == http.StatusOK || c2 == http.StatusOK {
			t.Fatalf("a poll of A in B was answered from B's recording (%d %s, then %d %s), which learned %v", c1, b1, c2, b2, st["mock-b"].ReqBodyNoise)
		}
		misses, err := p.GetMockErrors(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(misses) != 2 || !misses[0].NotOwned || misses[0].ClosestMock != "mock-a" || misses[1].NotOwned || misses[1].ClosestMock != "mock-a" || misses[1].Reason != "" {
			t.Fatalf("want the first poll named mock-a not owned and the second mock-a again; misses %+v", misses)
		}
	})
	t.Run("A's poll again in B's window", func(t *testing.T) {
		p, mm, a, b := pollsOfAAndB(t)
		mm.ServeExactTest("test-a", []*models.Mock{a})
		p.ContinueTestErrorCapture()
		if code, body := r3Send(t, p, mm, models.OutgoingOptions{Learn: true}, pollOfA); code != http.StatusOK || body != `{"jobs":["j1"]}` {
			t.Fatalf("A's poll in A: %d %s", code, body)
		}
		if misses, err := p.GetMockErrors(context.Background()); err != nil || len(misses) != 0 {
			t.Fatalf("A's poll in A missed: %+v, %v", misses, err)
		}
		mm.ServeExactTest("test-b", []*models.Mock{b})
		p.ContinueTestErrorCapture()
		code, body := r3Send(t, p, mm, models.OutgoingOptions{Learn: true}, pollOfA)
		st := r3States(mm)
		if _, noise := st["mock-b"].ReqBodyNoise["url.query.cursor"]; noise || code == http.StatusOK {
			t.Fatalf("A's poll again in B was answered from B's recording (%d %s), which learned %v", code, body, st["mock-b"].ReqBodyNoise)
		}
		misses, err := p.GetMockErrors(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(misses) != 1 || misses[0].NotOwned || misses[0].ClosestMock != "mock-a" || misses[0].Reason != "" {
			t.Fatalf("want A's poll again named mock-a, not as not owned; misses %+v", misses)
		}
		// B's own call is still B's recording's.
		p.ContinueTestErrorCapture()
		if code, body = r3Send(t, p, mm, models.OutgoingOptions{Learn: true}, "GET /v1/jobs?cursor=c2 HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n"); code != http.StatusOK || body != `{"jobs":[]}` {
			t.Fatalf("test B's own call: %d %s", code, body)
		}
	})
}

// (LEARN-vs-NOTOWNED, fail closed) The learn step asks whether a call is a
// recording made elsewhere before it pairs the call with a recording of its
// scope it drifted from. Test A's recording is on the agent's disk store,
// test B's in the trees, and A's poll is made in B's window. A lookup that
// cannot read what it has to look in (a recording kept on disk that cannot
// be read back, a store a re-staging closed) cannot tell whether the call is
// A's: so nothing is answered, learned or named for it, and its miss says
// keploy could not look, which makes the read of the test's misses
// incomplete (models.ErrUnmatchedCallsUnread): the test has no verdict. A
// replay round's miss (no learn step) is reported so too. Nothing was named
// by the lookup that could not look: once the store reads again, the same
// call is named A's recording, not owned. With the read working it is named
// so at once.
func TestALearnStepThatCannotLookUpACallMadeElsewhereAnswersAndLearnsNothing(t *testing.T) {
	readFails := errors.New("read mock-a: input/output error")
	for _, tc := range []struct {
		name string
		opts models.OutgoingOptions
		// fail is how the store fails, nil when it reads.
		fail func(a *models.Mock) RecordingSource
	}{
		{name: "the disk read works", opts: models.OutgoingOptions{Learn: true}},
		{name: "a recording kept on disk cannot be read back", opts: models.OutgoingOptions{Learn: true},
			fail: func(*models.Mock) RecordingSource {
				return func(kind models.Kind, _ []string, visit func(string, func() (*models.Mock, error)) bool) error {
					if kind == models.HTTP {
						visit("mock-a", func() (*models.Mock, error) { return nil, readFails })
					}
					return nil
				}
			}},
		{name: "the disk store cannot be read", opts: models.OutgoingOptions{Learn: true},
			fail: func(*models.Mock) RecordingSource {
				return func(models.Kind, []string, func(string, func() (*models.Mock, error)) bool) error {
					return errors.New("disk mocks: store closed")
				}
			}},
		{name: "a replay round's miss, a recording on disk cannot be read back",
			fail: func(*models.Mock) RecordingSource {
				return func(kind models.Kind, _ []string, visit func(string, func() (*models.Mock, error)) bool) error {
					if kind == models.HTTP {
						visit("mock-a", func() (*models.Mock, error) { return nil, readFails })
					}
					return nil
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			p := startedDrainProxy(t, 100)
			p.learnTracker = learn.NewTracker()
			now := time.Now()
			a := r3Mock("mock-a", now, "GET", "/v1/jobs?cursor=c1", "", `{"jobs":["j1"]}`)
			b := r3Mock("mock-b", now.Add(time.Second), "GET", "/v1/jobs?cursor=c2", "", `{"jobs":[]}`)
			for _, m := range []*models.Mock{a, b} {
				m.DeriveLifetime()
			}
			mm := NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			mm.SeedRecordedWindows([]models.TestWindow{
				{Start: now.Add(-100 * time.Millisecond), End: now.Add(500 * time.Millisecond)},
				{Start: now.Add(900 * time.Millisecond), End: now.Add(1500 * time.Millisecond)},
			})
			mm.SetMocksWithWindow(nil, []*models.Mock{b}, models.BaseTime, now.Add(time.Hour))
			reads := RecordingSource(func(kind models.Kind, _ []string, visit func(string, func() (*models.Mock, error)) bool) error {
				if kind == models.HTTP {
					visit("mock-a", func() (*models.Mock, error) { return a, nil })
				}
				return nil
			})
			if tc.fail != nil {
				mm.SetRecordingSource(tc.fail(a))
			} else {
				mm.SetRecordingSource(reads)
			}
			mm.SetServedObserver(p.learnTracker.ServedMock)
			mm.ServeExactTest("test-b", []*models.Mock{b})
			p.ContinueTestErrorCapture()

			code, body := r3Send(t, p, mm, tc.opts, pollOfA)
			// Read once: a read of what was consumed takes it.
			learned := r3States(mm)["mock-b"].ReqBodyNoise
			if _, noise := learned["url.query.cursor"]; noise || code == http.StatusOK {
				t.Fatalf("test A's call made in test B was answered from test B's recording (%d %s), which learned %v", code, body, learned)
			}
			misses, err := p.GetMockErrors(context.Background())
			if len(misses) != 1 || misses[0].Reason != "" {
				t.Fatalf("want one miss with no learn-step reason; misses %+v", misses)
			}
			if tc.fail == nil {
				if err != nil || !misses[0].NotOwned || misses[0].ClosestMock != "mock-a" {
					t.Fatalf("with the read working, want the call named mock-a not owned, and the read whole; misses %+v, %v", misses, err)
				}
				return
			}
			if !errors.Is(err, models.ErrUnmatchedCallsUnread) || !strings.Contains(err.Error(), "could not tell whether this call is a recording made elsewhere") {
				t.Fatalf("a miss keploy could not tell from a recording made elsewhere left the read of the test's misses whole (%v): the test would have a verdict", err)
			}
			if misses[0].NotOwned {
				t.Fatalf("the call was named not owned by a lookup that could not look: %+v", misses[0])
			}
			if !strings.Contains(misses[0].NextSteps, "could not read all the test set's recordings") {
				t.Fatalf("the miss does not say keploy could not look: %q", misses[0].NextSteps)
			}

			// The store reads again: the lookup that could not look named
			// nothing, so the same call is A's recording, not owned.
			mm.SetRecordingSource(reads)
			p.ContinueTestErrorCapture()
			if code, body := r3Send(t, p, mm, tc.opts, pollOfA); code == http.StatusOK {
				t.Fatalf("A's call in B was answered once the store read again: %d %s", code, body)
			}
			misses, err = p.GetMockErrors(context.Background())
			if err != nil || len(misses) != 1 || !misses[0].NotOwned || misses[0].ClosestMock != "mock-a" {
				t.Fatalf("once the store reads, want the call named mock-a not owned; misses %+v, %v", misses, err)
			}
			if learned := r3States(mm)["mock-b"].ReqBodyNoise; len(learned) != 0 {
				t.Fatalf("test B's recording learned %v", learned)
			}
		})
	}
}

// comparingStore is a mock manager that counts the recordings its lookups of
// a recording made elsewhere compare with a call, and the digests they
// compute, of every kind of lookup.
type comparingStore struct {
	*MockManager
	compared, digested atomic.Int64
}

func (c *comparingStore) counted(match func(*models.Mock) bool) func(*models.Mock) bool {
	return func(m *models.Mock) bool {
		c.compared.Add(1)
		return match(m)
	}
}

func (c *comparingStore) FindStagedElsewhere(kind models.Kind, q integrations.ElsewhereQuery) (string, bool, error) {
	q.Match = c.counted(q.Match)
	if digest := q.Digest; digest != nil {
		q.Digest = func(m *models.Mock) any {
			c.digested.Add(1)
			return digest(m)
		}
	}
	return c.MockManager.FindStagedElsewhere(kind, q)
}

func (c *comparingStore) FindStagedNotOwned(kind models.Kind, match func(*models.Mock) bool) string {
	return c.MockManager.FindStagedNotOwned(kind, c.counted(match))
}

func (c *comparingStore) FindStagedNotOwnedByKeys(kind models.Kind, keys []string, match func(*models.Mock) bool) string {
	return c.MockManager.FindStagedNotOwnedByKeys(kind, keys, c.counted(match))
}

// The lookup of a recording made elsewhere runs on every call of record's
// full round the learn step would pair, and most of that round's calls
// drift: it must not cost a comparison with every recording staged. With
// hundreds of other tests' recordings of the very operation (one method, one
// path, a JSON body each) staged, a drifted call of the open test compares
// with none of them but the one that learned a field (its own noise excuses
// what a digest reads, so it has none): their bodies' digests are not its. A
// call that is one of them is compared with that one (and the one with no
// digest) alone, and named; so is a call that is the one that learned, modulo
// what it learned. Each recording's digest is computed once for the staging,
// not once per call.
func TestALearnStepLooksUpARecordingMadeElsewhereWithoutComparingEveryOne(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 1000)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	pad := ""
	for i := 0; i < 40; i++ {
		pad += fmt.Sprintf(`"f%d":"v%d",`, i, i)
	}
	body := func(id int, ts string) string {
		return fmt.Sprintf(`{%s"id":%d,"ts":"%s"}`, pad, id, ts)
	}
	const others, calls = 400, 10
	var all, own []*models.Mock
	for j := 0; j < others; j++ {
		m := r3Mock(fmt.Sprintf("other-%d", j), now.Add(time.Duration(j)*time.Microsecond), "POST", "/v1/orders", body(100000+j, "2024-01-01T00:00:00Z"), `{"ok":true}`)
		m.DeriveLifetime()
		all = append(all, m)
	}
	// Two of them are calls of the open test's last two recordings, made
	// elsewhere: other-moved exactly, other-learned modulo the field it
	// learned.
	moved := r3Mock("other-moved", now.Add(time.Duration(others)*time.Microsecond), "POST", "/v1/orders", body(calls-2, "2023-01-01T00:00:00Z"), `{"ok":true}`)
	learned := r3Mock("other-learned", now.Add(time.Duration(others+1)*time.Microsecond), "POST", "/v1/orders", body(calls-1, "2022-01-01T00:00:00Z"), `{"ok":true}`)
	learned.Spec.ReqBodyNoise = map[string][]string{"body.ts": {}}
	for _, m := range []*models.Mock{moved, learned} {
		m.DeriveLifetime()
		all = append(all, m)
	}
	for i := 0; i < calls; i++ {
		m := r3Mock(fmt.Sprintf("own-%d", i), now.Add(time.Second+time.Duration(i)*time.Millisecond), "POST", "/v1/orders", body(i, "2024-01-01T00:00:00Z"), `{"ok":true}`)
		m.DeriveLifetime()
		all = append(all, m)
		own = append(own, m)
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{
		{Start: now.Add(-100 * time.Millisecond), End: now.Add(500 * time.Millisecond)},
		{Start: now.Add(900 * time.Millisecond), End: now.Add(1500 * time.Millisecond)},
	})
	mm.SetMocksWithWindow(nil, all, models.BaseTime, now.Add(time.Hour))
	mm.SetServedObserver(p.learnTracker.ServedMock)
	store := &comparingStore{MockManager: mm}
	mm.ServeExactTest("test-b", own)
	p.ContinueTestErrorCapture()
	post := func(b string) string {
		return fmt.Sprintf("POST /v1/orders HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(b), b)
	}
	for i := 0; i < calls-2; i++ {
		before := store.compared.Load()
		if code, _ := r3Send(t, p, store, models.OutgoingOptions{Learn: true}, post(body(i, "2026-10-20T00:00:00Z"))); code != http.StatusOK {
			t.Fatalf("drifted call %d: %d", i, code)
		}
		if n := store.compared.Load() - before; n != 1 {
			t.Fatalf("drifted call %d compared with %d recordings made elsewhere; want other-learned alone, the one with no digest", i, n)
		}
	}
	if d, staged := store.digested.Load(), int64(others+2+calls); d > staged {
		t.Fatalf("%d digests computed for %d staged recordings: one is computed more than once", d, staged)
	}
	// A call of the open test's next recording that is other-moved's
	// exactly.
	before := store.compared.Load()
	if code, _ := r3Send(t, p, store, models.OutgoingOptions{Learn: true}, post(body(calls-2, "2023-01-01T00:00:00Z"))); code == http.StatusOK {
		t.Fatal("a call that is another test's recording was answered from the open test's")
	}
	if n := store.compared.Load() - before; n < 1 || n > 2 {
		t.Fatalf("a call that is other-moved compared with %d recordings, want other-moved (and other-learned)", n)
	}
	// One of its last that is other-learned's, modulo the field it learned.
	if code, _ := r3Send(t, p, store, models.OutgoingOptions{Learn: true}, post(body(calls-1, "2026-10-20T00:00:00Z"))); code == http.StatusOK {
		t.Fatal("a call that is another test's recording, modulo what it learned, was answered from the open test's")
	}
	misses, err := p.GetMockErrors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(misses) != 2 || !misses[0].NotOwned || misses[0].ClosestMock != "other-moved" || !misses[1].NotOwned || misses[1].ClosestMock != "other-learned" {
		t.Fatalf("want the calls named other-moved and other-learned, not owned; misses %+v", misses)
	}
}

// keploy.yml's url noise of several patterns keeps one digest table for the
// staging: each staged recording is digested once whatever the order of the
// patterns (a map's), as with none. The patterns are applied in turn, and a
// digest is kept under them in their order (requestDigestOf), so they come
// in one order on every call: in the map's, new per call, every staged
// recording was digested, and kept, again for each order a call came with.
func TestALearnStepDigestsEachStagedRecordingOnceWhateverItsURLNoise(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 1000)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	body := func(id int, ts string) string { return fmt.Sprintf(`{"id":%d,"ts":"%s"}`, id, ts) }
	const others, calls = 200, 30
	var all, own []*models.Mock
	for j := 0; j < others; j++ {
		m := r3Mock(fmt.Sprintf("other-%d", j), now.Add(time.Duration(j)*time.Microsecond), "POST", "/v1/orders", body(100000+j, "2024-01-01T00:00:00Z"), `{"ok":true}`)
		m.DeriveLifetime()
		all = append(all, m)
	}
	for i := 0; i < calls; i++ {
		m := r3Mock(fmt.Sprintf("own-%d", i), now.Add(time.Second+time.Duration(i)*time.Millisecond), "POST", "/v1/orders", body(i, "2024-01-01T00:00:00Z"), `{"ok":true}`)
		m.DeriveLifetime()
		all = append(all, m)
		own = append(own, m)
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{
		{Start: now.Add(-100 * time.Millisecond), End: now.Add(500 * time.Millisecond)},
		{Start: now.Add(900 * time.Millisecond), End: now.Add(1500 * time.Millisecond)},
	})
	mm.SetMocksWithWindow(nil, all, models.BaseTime, now.Add(time.Hour))
	mm.SetServedObserver(p.learnTracker.ServedMock)
	store := &comparingStore{MockManager: mm}
	mm.ServeExactTest("test-b", own)
	p.ContinueTestErrorCapture()
	opts := models.OutgoingOptions{Learn: true, NoiseConfig: map[string]map[string][]string{
		"url": {`/v9/a[0-9]+`: {}, `/v9/b[0-9]+`: {}, `/v9/c[0-9]+`: {}, `/v9/d[0-9]+`: {}, `/v9/e[0-9]+`: {}},
	}}
	post := func(b string) string {
		return fmt.Sprintf("POST /v1/orders HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(b), b)
	}
	for i := 0; i < calls; i++ {
		if code, _ := r3Send(t, p, store, opts, post(body(i, "2026-10-20T00:00:00Z"))); code != http.StatusOK {
			t.Fatalf("drifted call %d: %d", i, code)
		}
	}
	if d, staged := store.digested.Load(), int64(len(all)); d == 0 || d > staged {
		t.Fatalf("%d digests computed for %d staged recordings over %d calls; want each recording's once", d, staged, calls)
	}
}

// The lookup of a recording made elsewhere reads every part of a request, not
// its body alone: with hundreds of other tests' recordings of the very GET
// operation staged, whose bodies are all empty, a drifted call of the open
// test compares with none of them when they differ from it in their query
// (a poll's cursor), or in a header the comparison reads. A call that is one
// of them is compared with that one alone, and named not owned.
func TestALearnStepLooksUpARecordingMadeElsewhereByEveryPartOfTheRequest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 1000)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	get := func(name string, at time.Time, target, ref string) *models.Mock {
		m := r3Mock(name, at, "GET", target, "", `{"ok":true}`)
		m.Spec.HTTPReq.Header["X-Order-Ref"] = ref
		m.DeriveLifetime()
		return m
	}
	const others = 300
	var all, own []*models.Mock
	for j := 0; j < others; j++ {
		at := now.Add(time.Duration(2*j) * time.Microsecond)
		all = append(all,
			get(fmt.Sprintf("other-poll-%d", j), at, fmt.Sprintf("/v1/jobs?cursor=x%d&ts=T0", j), "r"),
			get(fmt.Sprintf("other-config-%d", j), at.Add(time.Microsecond), "/v1/config", fmt.Sprintf("r%d", j)))
	}
	at := func(i int) time.Time { return now.Add(time.Second + time.Duration(i)*time.Millisecond) }
	own = append(own,
		get("poll-0", at(0), "/v1/jobs?cursor=o0&ts=T0", "r"),
		get("poll-1", at(1), "/v1/jobs?cursor=o1&ts=T0", "r"),
		get("config-0", at(2), "/v1/config", "QX81"),
		get("config-1", at(3), "/v1/config", "QX82"))
	all = append(all, own...)
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{
		{Start: now.Add(-100 * time.Millisecond), End: now.Add(500 * time.Millisecond)},
		{Start: now.Add(900 * time.Millisecond), End: now.Add(1500 * time.Millisecond)},
	})
	mm.SetMocksWithWindow(nil, all, models.BaseTime, now.Add(time.Hour))
	mm.SetServedObserver(p.learnTracker.ServedMock)
	store := &comparingStore{MockManager: mm}
	mm.ServeExactTest("test-b", own)
	p.ContinueTestErrorCapture()
	call := func(target, ref string) string {
		return fmt.Sprintf("GET %s HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nX-Order-Ref: %s\r\n\r\n", target, ref)
	}
	for _, tc := range []struct {
		what, request string
		answered      bool
		compared      int64
	}{
		{"poll-0 with its ts drifted", call("/v1/jobs?cursor=o0&ts=T1", "r"), true, 0},
		{"config-0 with its X-Order-Ref drifted", call("/v1/config", "ZT44"), true, 0},
		{"other-poll-9's call", call("/v1/jobs?cursor=x9&ts=T0", "r"), false, 1},
		{"other-config-7's call", call("/v1/config", "r7"), false, 1},
	} {
		before := store.compared.Load()
		code, _ := r3Send(t, p, store, models.OutgoingOptions{Learn: true}, tc.request)
		if (code == http.StatusOK) != tc.answered {
			misses, _ := p.GetMockErrors(context.Background())
			t.Fatalf("%s: %d, want answered %v; misses %+v", tc.what, code, tc.answered, misses)
		}
		if n := store.compared.Load() - before; n != tc.compared {
			t.Fatalf("%s compared with %d recordings made elsewhere, want %d", tc.what, n, tc.compared)
		}
	}
	if d, staged := store.digested.Load(), int64(len(all)); d > staged {
		t.Fatalf("%d digests computed for %d staged recordings: one is computed more than once", d, staged)
	}
	misses, err := p.GetMockErrors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(misses) != 2 || !misses[0].NotOwned || misses[0].ClosestMock != "other-poll-9" || !misses[1].NotOwned || misses[1].ClosestMock != "other-config-7" {
		t.Fatalf("want the calls named other-poll-9 and other-config-7, not owned; misses %+v", misses)
	}
}

// (LEARN-vs-NOTOWNED) A call made elsewhere once more than its recording
// holds is that recording again whatever the learn step made of it, not only
// when it would pair it: here A's poll, made in A, is made again in B's
// window, where B recorded two polls of the operation that each differ from
// it in another place, so the learn step pairs it with neither (a tie,
// request_changed). The miss names mock-a with no reason and no field said
// to differ: read as B's call changed (its cursor, c2 to c1), its next steps
// offered keploy.yml noise for the cursor, which answers A's call with B's
// recording on the next replay: a false pass.
func TestALearnStepMissThatPairsNothingOnACallMadeElsewhereAgainNamesItsRecording(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	a := r3Mock("mock-a", now, "GET", "/v1/jobs?cursor=c1&page=1", "", `{"jobs":["j1"]}`)
	b1 := r3Mock("mock-b1", now.Add(time.Second), "GET", "/v1/jobs?cursor=c2&page=1", "", `{"jobs":[]}`)
	b2 := r3Mock("mock-b2", now.Add(time.Second+time.Millisecond), "GET", "/v1/jobs?cursor=c1&page=2", "", `{"jobs":[]}`)
	for _, m := range []*models.Mock{a, b1, b2} {
		m.DeriveLifetime()
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{
		{Start: now.Add(-100 * time.Millisecond), End: now.Add(500 * time.Millisecond)},
		{Start: now.Add(900 * time.Millisecond), End: now.Add(1500 * time.Millisecond)},
	})
	mm.SetMocksWithWindow(nil, []*models.Mock{a, b1, b2}, models.BaseTime, now.Add(time.Hour))
	mm.SetServedObserver(p.learnTracker.ServedMock)
	poll := func(query string) string {
		return "GET /v1/jobs?" + query + " HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n"
	}
	mm.ServeExactTest("test-a", []*models.Mock{a})
	p.ContinueTestErrorCapture()
	if code, body := r3Send(t, p, mm, models.OutgoingOptions{Learn: true}, poll("cursor=c1&page=1")); code != http.StatusOK || body != `{"jobs":["j1"]}` {
		t.Fatalf("A's poll in A: %d %s", code, body)
	}
	if misses, err := p.GetMockErrors(context.Background()); err != nil || len(misses) != 0 {
		t.Fatalf("A's poll in A missed: %+v, %v", misses, err)
	}
	mm.ServeExactTest("test-b", []*models.Mock{b1, b2})
	p.ContinueTestErrorCapture()
	if code, body := r3Send(t, p, mm, models.OutgoingOptions{Learn: true}, poll("cursor=c1&page=1")); code == http.StatusOK {
		t.Fatalf("A's poll again in B was answered: %s", body)
	}
	misses, err := p.GetMockErrors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(misses) != 1 || misses[0].ClosestMock != "mock-a" || misses[0].NotOwned || misses[0].Reason != "" || len(misses[0].FieldDiffs) != 0 || misses[0].Diff != "" || strings.Contains(misses[0].NextSteps, "noise") {
		t.Fatalf("want A's poll again named mock-a, with no reason, nothing differing and no noise offered; misses %+v", misses)
	}
	st := r3States(mm)
	for _, name := range []string{"mock-b1", "mock-b2"} {
		if len(st[name].ReqBodyNoise) != 0 {
			t.Fatalf("%s learned %v", name, st[name].ReqBodyNoise)
		}
	}
	// B's own calls are still B's recordings'.
	p.ContinueTestErrorCapture()
	for _, q := range []string{"cursor=c2&page=1", "cursor=c1&page=2"} {
		if code, body := r3Send(t, p, mm, models.OutgoingOptions{Learn: true}, poll(q)); code != http.StatusOK || body != `{"jobs":[]}` {
			t.Fatalf("test B's own call %s: %d %s", q, code, body)
		}
	}
}
