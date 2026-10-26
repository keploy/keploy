package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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
	return r3SendIn(t, context.Background(), p, mm, opts, request)
}

// r3SendIn is r3Send with the call's context, which a row cancels to end a
// call that would otherwise wait for good: the row then fails, never hangs.
func r3SendIn(t *testing.T, parent context.Context, p *Proxy, mm *MockManager, opts models.OutgoingOptions, request string) (int, string) {
	t.Helper()
	return r3SendVia(t, parent, httpint.New(zap.NewNop()), p, mm, opts, request)
}

// r3SendVia is r3SendIn through the HTTP integration h: one the row keeps
// across calls, as the agent's proxy keeps its parser.
func r3SendVia(t *testing.T, parent context.Context, h integrations.Integrations, p *Proxy, mm *MockManager, opts models.OutgoingOptions, request string) (int, string) {
	t.Helper()
	app, agentEnd := net.Pipe()
	ctx := models.WithMockMismatchReporter(parent, p.sendMockNotFoundError)
	ctx = learn.WithTracker(ctx, p.learnTracker)
	defer app.Close()
	go func() {
		_ = h.MockOutgoing(ctx, agentEnd, &models.ConditionalDstCfg{}, mm, opts)
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

// Through the agent's MockManager, whose served observer tells the learn
// step's tracker what F served: an application that receives two messages of
// a queue before the test (at the start-up, in the previous test, or the
// first before and the second in it) and builds the insert's visible_after
// on the message it holds (SentTimestamp and a minute). F serving B first,
// the recording's value is built on A and F's on B: the insert is refused,
// naming both receives, wherever before the call F served them, never
// learned as noise. The tracker numbers what the store served across the
// set (learn.Order), so F's order is told wherever it served them.
func TestALearnStepReadsWhatTheStoreServedBeforeTheTestAsGiven(t *testing.T) {
	for _, where := range []string{"at the start-up", "in the previous test", "the first before the test, the second in it"} {
		t.Run(where, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			p := startedDrainProxy(t, 100)
			p.learnTracker = learn.NewTracker()
			now := time.Now()
			// Early in their minutes, whenever the test runs: a sent time
			// late in its minute rounds to the minute the value is cut to,
			// and the echo then names the two receives before the offset
			// rule does.
			sentA := now.Truncate(time.Minute).Add(-10*time.Minute + 4*time.Second + 473*time.Millisecond)
			sentB := sentA.Add(2*time.Minute + 311*time.Millisecond)
			receive := func(name, path string, at, sent time.Time) *models.Mock {
				return r3Mock(name, at, "GET", path, "", `{"Messages":[{"MessageId":"`+name+`","Attributes":{"SentTimestamp":"`+strconv.FormatInt(sent.UnixMilli(), 10)+`"}}]}`)
			}
			minute := func(t time.Time) string { return t.Format("2006-01-02T15:04Z07:00") }
			a := receive("mock-a", "/queue/a", now.Add(-3*time.Second), sentA)
			b := receive("mock-b", "/queue/b", now.Add(-2*time.Second), sentB)
			insert := r3Mock("mock-insert", now.Add(-time.Second), "POST", "/v1/messages", `{"visible_after":"`+minute(sentA.Add(time.Minute))+`"}`, `{}`)
			for _, m := range []*models.Mock{a, b, insert} {
				m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived = models.LifetimePerTest, true
			}
			mm := NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			mm.SetMocksWithWindow([]*models.Mock{a, b, insert}, nil, now.Add(-time.Hour), now.Add(time.Hour))
			mm.SetServedObserver(p.learnTracker.ServedMock) // as setMockManager installs it
			opts := models.OutgoingOptions{Learn: true, LearnPool: true}
			get := func(path string) {
				if code, got := r3Send(t, p, mm, opts, "GET "+path+" HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n"); code != http.StatusOK {
					t.Fatalf("GET %s got %d %s", path, code, got)
				}
			}
			switch where {
			case "at the start-up":
				get("/queue/b")
				get("/queue/a")
				p.ContinueTestErrorCapture()
			case "in the previous test":
				p.ContinueTestErrorCapture()
				get("/queue/b")
				get("/queue/a")
				if _, err := p.GetMockErrors(context.Background()); err != nil {
					t.Fatal(err)
				}
				p.ContinueTestErrorCapture()
			case "the first before the test, the second in it":
				get("/queue/b")
				p.ContinueTestErrorCapture()
				get("/queue/a")
			}
			body := `{"visible_after":"` + minute(sentB.Add(time.Minute)) + `"}`
			code, got := r3Send(t, p, mm, opts, "POST /v1/messages HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body)
			misses, err := p.GetMockErrors(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			said := fmt.Sprintf("%+v", misses)
			if _, noise := r3States(mm)["mock-insert"].ReqBodyNoise["body.visible_after"]; noise || code == http.StatusOK ||
				!strings.Contains(said, "mock-a at spec.resp.body holds as the recording gave it") || !strings.Contains(said, "mock-b at spec.resp.body holds, which record's full round gave the application in its place") {
				t.Fatalf("answered %d %s, misses %s; want the insert refused, built on mock-a and mock-b by an offset", code, got, said)
			}
		})
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
// (request_changed), as the mocks served before test 2 began do not: it is
// learned as a read of the clock.
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
	// One instant whole (an epoch second) in a test's own call, which has no
	// window, read 93 s after its recording (no lead moves it): a read of the
	// clock a replay checks in its call's own span, never noise.
	st := r3States(mm)["mock-t2"]
	if _, ok := st.Clocks["body.at"]; !ok || st.ReqBodyNoise["body.at"] != nil {
		t.Fatalf("test 2's clock read is no read of the clock: %+v", st)
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

// Under exact serving (keploy test's agent under the switch), what the
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
// a read of the clock, whatever the receive's Date header holds. With no
// window (the lead alone, which does not read it: F's clock ran 2 s past the
// lead), it is learned as one too where that header is its server's stamp,
// no place the value came from: a read of the clock a replay checks in its
// call's own span, never noise, which would pass any value at every replay.
// Where the server's clock ran a second ahead (its Date header after its
// answer is no stamp), it is refused for that header: only the window learns
// that one. The instants are fixed in UTC, so the row reads the same in every
// zone.
func TestAStartUpPoolCallsClockReadIsReadInTheWindowDecodeHTTPEnds(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	iso := func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }
	rec := time.Date(2026, 10, 20, 15, 49, 55, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		date  time.Time
		stamp bool
	}{
		{"its server's stamp", rec, true},
		{"its server's clock a second ahead", rec.Add(time.Second), false},
	} {
		for _, window := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, window=%v", tc.name, window), func(t *testing.T) {
				p := startedDrainProxy(t, 100)
				p.learnTracker = learn.NewTracker()
				receive := r3Mock("mock-0", rec, "POST", "/queue/receive", `{"n":10}`, `{"Messages":[{"Body":"m-1"}]}`)
				receive.Spec.HTTPResp.Header["Date"] = tc.date.Format(http.TimeFormat)
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
				if window {
					p.learnTracker.SetAppClock(func(real time.Time) (time.Time, bool) { return real.Add(offset), true })
				}
				p.learnTracker.ServedMockAt(receive, served)
				body := `{"processed_at":"` + iso(served.Add(offset)) + `"}`
				code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true},
					"POST /v1/events HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body)
				st := r3States(mm)["mock-1"]
				noise := st.ReqBodyNoise
				if window || tc.stamp {
					// An instant whole its call's window holds: a read of the
					// clock, still compared at every replay (MockState.Clocks),
					// never noise, its window's or, by its server's stamp
					// alone with no window, its call's own span's.
					_, clock := st.Clocks["body.processed_at"]
					if code != http.StatusOK || got != `{"ok":true}` || !clock || noise["body.processed_at"] != nil {
						t.Fatalf("answered %d %s, learned noise %v clocks %v; want the pool recording, its clock read learned at body.processed_at", code, got, noise, st.Clocks)
					}
					return
				}
				if code == http.StatusOK || noise["body.processed_at"] != nil {
					t.Fatalf("by the lead alone, the server's clock a second ahead: answered %d %s, learned %v; want refused", code, got, noise)
				}
			})
		}
	}
}

// A slot the app picks by its clock from its test case's own request, which
// the agent was handed as the test case opened (TakeTestRequest), is refused,
// naming the request's slot: the learn step's echo reads the request. Handed
// none (a keploy older than its agent), the call is refused for the request
// it could not read, never learned as noise.
func TestALearnStepRefusesAPickFromTheTestCasesRequest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	book := func() *models.Mock {
		m := batchMock()
		m.Spec.HTTPReq.URL, m.Spec.HTTPReq.Body = "/book", `{"slot":"11:14"}`
		m.Spec.HTTPResp.Header["Content-Length"], m.Spec.HTTPResp.Body = "11", `{"ok":true}`
		return m
	}
	body := `{"slots":["11:13","11:14","11:15"]}`
	slots := &models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{{Name: "header.Content-Type", Recorded: "application/json", Sent: "application/json"}, {Name: "body", Recorded: body, Sent: body}}}
	for _, tc := range []struct {
		name string
		req  *models.TestRequest
		want string
	}{
		{"handed its request", slots, "carries a value the test case's own request holds (test case test-1 at body.slots[1])"},
		{"handed none", nil, "keploy could not read the test case's own request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := startedDrainProxy(t, 100)
			p.learnTracker = learn.NewTracker()
			p.ContinueTestErrorCapture()
			p.TakeTestRequest(tc.req)
			req := `{"slot":"11:15"}`
			resp, _, _ := learnCall(t, p, book(), "POST /book HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(req))+"\r\n\r\n"+req)
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("the app got %d, want 502", resp.StatusCode)
			}
			misses, err := p.GetMockErrors(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(misses) != 1 || misses[0].Reason != models.MissRequestChanged || !strings.Contains(misses[0].NextSteps, tc.want) {
				t.Fatalf("misses %+v; want a request_changed naming %q", misses, tc.want)
			}
		})
	}
	// With the switch off, nothing is handed.
	t.Setenv(provenswitch.Env, "0")
	p := &Proxy{learnTracker: learn.NewTracker()}
	p.learnTracker.Begin()
	p.TakeTestRequest(slots)
	if _, earlier, _, _ := p.learnTracker.Test(); len(earlier) != 0 {
		t.Fatalf("switch off, the tracker holds %+v", earlier)
	}
}

// An agent that serves keploy mock (its --mock-mode) learns its tests as a
// test harness's client tests (learn.Tracker.SetClientTests); one that serves
// keploy test does not.
func TestAMockModeAgentsLearnStepReadsClientTests(t *testing.T) {
	for _, mock := range []bool{false, true} {
		cfg := config.New()
		cfg.Agent.MockMode = mock
		p := New(zap.NewNop(), fakeDestInfo{}, cfg)
		if got := p.learnTracker.ClientTests(); got != mock || p.learnTracker.Drifts().ClientTests() != mock || p.ClientTests() != mock {
			t.Fatalf("mock mode %v: client tests %v", mock, got)
		}
	}
}

// keploy mock's agent reads a wall clock of no zone in its own zone, the
// application's beside it (learn.Tracker.SetAppZone), and, under the proven
// switch, bounds a test's calls by the test's recorded begin it is given
// (TakeRecordedBegin; learn.Tracker.RecordedBegin): the test's first call
// has a window from it. With the switch off it is given nothing.
func TestAMockModeAgentsLearnStepReadsTheAppsZoneAndATestsRecordedBegin(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "switch on", false: "switch off"}[on], func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[bool]string{true: "1", false: ""}[on])
			cfg := config.New()
			cfg.Agent.MockMode = true
			p := New(zap.NewNop(), fakeDestInfo{}, cfg)
			if got := p.learnTracker.Drifts().Zone(); got != time.Local {
				t.Fatalf("the learn step's zone %v; want the agent's own, %v", got, time.Local)
			}
			rec := time.Date(2026, 10, 22, 11, 14, 15, 0, time.UTC)
			paired := &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: rec.Add(time.Second)}}
			p.learnTracker.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
			p.TakeRecordedBegin(rec)
			began := time.Now()
			p.learnTracker.BeginAt(began)
			w := p.learnTracker.CallWindowFrom(time.Time{}, paired, began.Add(time.Millisecond))
			if on && (w == nil || !w.Rec.From.Equal(rec)) {
				t.Fatalf("window %+v; want one from the test's recorded begin", w)
			}
			if !on && w != nil {
				t.Fatalf("switch off: window %+v; want none", w)
			}
		})
	}
}

// A capture window keploy test opens for a test case begins the learn step's
// test awaiting its request, in the window's own step, until the filter that
// opens the test brings it (TakeTestRequest); a window opened otherwise
// (keploy mock's, or with the switch off) awaits none.
func TestATestCasesCaptureWindowAwaitsItsRequest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for name, open := range map[string]func(*Proxy){
		"begin":    (*Proxy).BeginTestCaseCapture,
		"continue": (*Proxy).ContinueTestCaseCapture,
	} {
		p := startedDrainProxy(t, 100)
		p.learnTracker = learn.NewTracker()
		open(p)
		if _, _, _, _, awaiting := p.learnTracker.Places(); !awaiting {
			t.Fatalf("%s: the test does not await its request", name)
		}
		p.TakeTestRequest(&models.TestRequest{Test: "test-1"})
		if _, earlier, _, _, awaiting := p.learnTracker.Places(); awaiting || len(earlier) != 1 || earlier[0].Mock != "test case test-1" {
			t.Fatalf("%s: after the request: %+v, awaiting %v", name, earlier, awaiting)
		}
	}
	// A window that closes before the filter brought the request is said at
	// the default level; one whose request came says nothing.
	for name, give := range map[string]bool{"closed unread": false, "its request given": true} {
		core, logs := observer.New(zapcore.InfoLevel)
		p := startedDrainProxyLogging(t, 100, zap.New(core))
		p.learnTracker = learn.NewTracker()
		p.sayLearnWaits()
		p.BeginTestCaseCapture()
		if give {
			p.TakeTestRequest(&models.TestRequest{Test: "test-1"})
		}
		p.EndTestErrorCapture()
		warned := logs.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet("closed before the filter that opens the test brought its request").Len()
		if want := map[bool]int{false: 1, true: 0}[give]; warned != want {
			t.Fatalf("%s: %d warnings that the window closed without its request; want %d", name, warned, want)
		}
	}
	for name, open := range map[string]func(*Proxy){
		"keploy mock's begin":    (*Proxy).BeginTestErrorCapture,
		"keploy mock's continue": (*Proxy).ContinueTestErrorCapture,
	} {
		p := startedDrainProxy(t, 100)
		p.learnTracker = learn.NewTracker()
		open(p)
		if _, _, _, _, awaiting := p.learnTracker.Places(); awaiting {
			t.Fatalf("%s: the test awaits a request", name)
		}
	}
}

// A paired call's repeat note follows a missed call's rule, through the real
// store. Five tests recorded one call (an app cache): test-1's mock-s,
// test-2's mock-t, test-3's mock-u, test-5's mock-v, and test-4 recorded
// none. In record's full round test-1 makes it (mock-s answers) and test-2
// makes it twice: its own mock-t answers the first, and the second, one call
// more than test-2 recorded, is paired with mock-u and carries no note, as
// its miss would carry none (it repeats test-2's own recorded call, so a note
// saying the test recorded no such call would be false). test-4's call,
// which it recorded nothing like, is paired with mock-v and carries the note
// naming a recording an earlier test's call consumed.
func TestAPairedCallsRepeatNoteFollowsTheMissesRule(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	rec := func(name string, at time.Duration) *models.Mock {
		return r3Mock(name, now.Add(at), "GET", "/rates?base=CHF", "", `{"rate":1.1}`)
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	mm.SetMocksWithWindow([]*models.Mock{rec("mock-s", 100*time.Millisecond), rec("mock-t", 1500*time.Millisecond), rec("mock-u", 3000*time.Millisecond), rec("mock-v", 4500*time.Millisecond)}, nil, models.BaseTime, now.Add(time.Hour))
	mm.ServeExactBackground(models.RecordedWithS12Routing, nil, nil)
	mm.SetPairing(true)
	p.setMockManager(mm)
	t.Cleanup(func() { p.setMockManager(nil) })
	get := "GET /rates?base=CHF HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n"
	opts := models.OutgoingOptions{Learn: true}
	answers := func(t *testing.T) []models.UnmatchedCall {
		t.Helper()
		calls, err := p.GetMockErrors(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var out []models.UnmatchedCall
		for _, c := range calls {
			if c.Answer == "" {
				t.Fatalf("a miss %+v; every call here is answered", c)
			}
			out = append(out, c)
		}
		return out
	}

	p.ContinueTestErrorCapture()
	mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-s")})
	if code, got := r3Send(t, p, mm, opts, get); code != http.StatusOK {
		t.Fatalf("test-1's call: %d %s", code, got)
	}
	if a := answers(t); len(a) != 0 {
		t.Fatalf("test-1's own call was reported %+v", a)
	}
	p.ContinueTestErrorCapture()
	mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-t")})
	for i := 1; i <= 2; i++ {
		if code, got := r3Send(t, p, mm, opts, get); code != http.StatusOK {
			t.Fatalf("test-2's call %d: %d %s", i, code, got)
		}
	}
	if a := answers(t); len(a) != 1 || a[0].ClosestMock != "mock-u" || a[0].Repeat != nil {
		t.Fatalf("test-2's answers are %+v; want one pairing with mock-u, with no repeat note", a)
	}
	p.ContinueTestErrorCapture()
	mm.ServeExactTest("test-4", nil)
	if code, got := r3Send(t, p, mm, opts, get); code != http.StatusOK {
		t.Fatalf("test-4's call: %d %s", code, got)
	}
	a := answers(t)
	if len(a) != 1 || a[0].ClosestMock != "mock-v" || a[0].Repeat == nil || (a[0].Repeat.Mock != "mock-s" && a[0].Repeat.Mock != "mock-t") {
		t.Fatalf("test-4's answers are %+v; want one pairing with mock-v, noting mock-s or mock-t", a)
	}
}
