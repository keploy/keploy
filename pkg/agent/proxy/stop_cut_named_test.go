package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/pkg/agent/proxy"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// incomingOnly is an agent service whose incoming proxy hands the record
// route the test cases sent on tc. Any other call panics (nil Service).
type incomingOnly struct {
	agentsvc.Service
	tc chan *models.TestCase
}

func (s incomingOnly) StartIncomingProxy(context.Context, models.IncomingOptions) (chan *models.TestCase, error) {
	return s.tc, nil
}

// The stop's cut of a stuck parser, from the parser to the test case it cost,
// as the agent runs them. The record route streams the recording's test
// cases; a connection's parser never reads the request the connection
// carried, and the hang watchdog cuts it as the recording stops, on the
// process's manager the record route reads. The test case over that request
// is left out, not streamed, and named in the one WARN the record route says
// of the stop's cut as its session ends; the proxy's WARN names the
// connection.
func TestTheStopsCutOfAStuckParserNamesTheTestCaseItCost(t *testing.T) {
	syncMock.Get().SetWatermark(nil)
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)

	tcs := make(chan *models.TestCase, 1)
	router := chi.NewRouter()
	routes.DefaultRoutes{}.New(router, incomingOnly{tc: tcs}, logger)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	body, _ := json.Marshal(models.IncomingReq{})
	client := &http.Client{Timeout: time.Minute}
	resp, err := client.Post(srv.URL+"/agent/incoming", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the record route answered %d", resp.StatusCode)
	}

	sent, arrived := proxy.CutAParserStuckAtTheStop(t, logger)
	if n := logs.FilterMessage(proxy.StopCutWarnMsg).Len(); n != 1 {
		t.Fatalf("%d WARNs say the stop cut the parser, want 1", n)
	}

	const name = "over-the-unread-request"
	tcs <- &models.TestCase{Name: name, Kind: models.HTTP,
		HTTPReq: models.HTTPReq{Timestamp: sent}, HTTPResp: models.HTTPResp{Timestamp: arrived}}
	close(tcs)
	streamed, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	if bytes.Contains(streamed, []byte(name)) {
		t.Errorf("the test case over the request the cut parser never read is streamed, without its mock")
	}
	deadline := time.Now().Add(30 * time.Second)
	for logs.FilterMessage("agent: recording complete").Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no summary once the stream ended")
		}
		time.Sleep(time.Millisecond)
	}

	said := logs.FilterMessageSnippet("the recording's stop left out test cases").All()
	if len(said) != 1 {
		t.Fatalf("%d lines name the test cases the stop's cut left out, want 1", len(said))
	}
	e := said[0]
	f := e.ContextMap()
	if e.Level != zapcore.WarnLevel {
		t.Errorf("the stop's cut is said at %v, want WARN", e.Level)
	}
	if got := fmt.Sprint(f["test_cases"]); got != "["+name+"]" {
		t.Errorf("the WARN names %s, want [%s]", got, name)
	}
	if n, _ := f["test_cases_left_out"].(int64); n != 1 {
		t.Errorf("the WARN counts %v test cases left out, want 1", f["test_cases_left_out"])
	}
	if f["reason"] != "the parser made no progress after the stop" {
		t.Errorf("the WARN gives the reason %q", f["reason"])
	}
}
