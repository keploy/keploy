package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

func TestALearnStepVariableRidesAScopedHold(t *testing.T) {
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
	p.SetMappedUniverse([]string{"mock-a", "mock-b"})
	const worker = 5151
	p.SetWorkerScope(worker, []string{"mock-a", "mock-b"})
	view := p.scopedFor(worker, mm)
	send := func(request string) (int, string) {
		app, agentEnd := net.Pipe()
		ctx := models.WithMockMismatchReporter(context.Background(), p.sendMockNotFoundError)
		ctx = learn.WithTracker(ctx, p.learnTracker)
		defer app.Close()
		go func() {
			_ = httpint.New(zap.NewNop()).MockOutgoing(ctx, agentEnd, &models.ConditionalDstCfg{}, view, models.OutgoingOptions{Learn: true})
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
	if want := (map[string]models.MockVariables{"mock-a": {"order": {Recorded: "o-1", Bind: "body.order", Words: wordsLearnedOf(t, "o-1", "o-9")}}}); !reflect.DeepEqual(states["mock-b"].Binds, want) {
		t.Fatalf("mock-b's state binds %+v", states["mock-b"].Binds)
	}
}

// A worker's view made before exact serving began passes straight through
// while it is on (232b72864): the learn step then reads ownership as on the
// bare manager, from what exact serving shows the open test, never from the
// view's stale allowlist. The open test's own drifted call is paired.
func TestALearnStepOnAStaleScopedViewUnderExactServingPairsTheOpenTestsOwn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	call := func(name string, at time.Duration, body string) *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": models.HTTPClient},
			HTTPReq:          &models.HTTPReq{Method: "POST", URL: "http://deps.example/orders", Header: map[string]string{"Host": "deps.example", "Content-Type": "application/json"}, Body: body},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json", "Content-Length": "2"}, Body: `{}`},
			ReqTimestampMock: now.Add(at),
		}, TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest}}
	}
	old, own := call("mock-a", 0, `{"rid":"r-0"}`), call("mock-c", time.Second, `{"rid":"r-1"}`)
	mm := stagedManager(t, []*models.Mock{old, own}, nil)
	const worker = 5152
	p.SetWorkerScope(worker, []string{"mock-a"}) // the previous test's scope
	view := p.scopedFor(worker, mm)
	mm.ServeExactPool("k", nil)
	mm.ServeExactTest("T", []*models.Mock{own})
	p.ContinueTestErrorCapture()
	app, agentEnd := net.Pipe()
	ctx := learn.WithTracker(models.WithMockMismatchReporter(context.Background(), p.sendMockNotFoundError), p.learnTracker)
	defer app.Close()
	go func() {
		_ = httpint.New(zap.NewNop()).MockOutgoing(ctx, agentEnd, &models.ConditionalDstCfg{}, view, models.OutgoingOptions{Learn: true})
		_ = agentEnd.Close()
	}()
	body := `{"rid":"r-9"}`
	if _, err := app.Write([]byte("POST /orders HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		misses, _ := p.GetMockErrors(context.Background())
		t.Fatalf("the open test's own drifted call got %d, misses %+v", resp.StatusCode, misses)
	}
}
