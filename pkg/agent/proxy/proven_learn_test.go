package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
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
func r3Send(t *testing.T, p *Proxy, mm *MockManager, opts models.OutgoingOptions, request string) (int, string) {
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

// A reused mock the learn step paired with an echo answers every call of the
// round: what was learned rides on the consumed state, not on the pooled
// recording.
func TestALearnStepReusedMockWithAnEchoAnswersAgain(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	m := r3Mock("mock-0", now, "POST", "/ping", `{"rid":"r-1"}`, `{"rid":"r-1"}`)
	m.DeriveLifetime()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
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
