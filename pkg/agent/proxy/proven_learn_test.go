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
	p.learnTracker = learn.NewTracker(nil)
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
	p.learnTracker = learn.NewTracker(nil)
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
	want := []models.FieldCorrelation{{RequestPath: "body.rid", ResponsePaths: []string{"body.rid"}, RecordedValue: "r-1"}}
	if !reflect.DeepEqual(consumed[0].Correlations, want) {
		t.Fatalf("the consumed mock's state carries %+v", consumed[0].Correlations)
	}
}
