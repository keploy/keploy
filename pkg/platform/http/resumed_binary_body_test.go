package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// The /incoming stream a restarted agent answers asks for body objects as the
// first one did. A stream that asked only at the start kept its bytes up to the
// restart, and from then on got every binary body with U+FFFD in its place,
// with nothing said, for the rest of the recording.
func TestResumedIncomingStreamKeepsBinaryBodies(t *testing.T) {
	const reqBody = "\x89PNG\r\n\x1a\n\x00\xff\xfe\x01upload"
	const respBody = "\x08\x96\x01\x12\x04\xc3\x28\xff\xfeproto"
	agent := &restartingAgent{conns: map[string]int{}, refuse: map[string]int{}, hang: map[string]int{}, slow: map[string]int{}}
	inner := agent.handler(t)
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/incoming" {
			inner.ServeHTTP(w, r)
			return
		}
		mu.Lock()
		asked = append(asked, r.Header.Get("Keploy-Body-Encoding"))
		n := len(asked)
		mu.Unlock()
		tc := &models.TestCase{Name: fmt.Sprintf("test-%d", n), Kind: models.HTTP,
			HTTPReq:  models.HTTPReq{Method: http.MethodPost, Body: reqBody},
			HTTPResp: models.HTTPResp{StatusCode: http.StatusOK, Body: respBody}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(models.TestCaseJSON(tc, models.ReadsBodyObjects(r.Header)))
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{}
	cfg.Agent.AgentURI = srv.URL + "/agent"
	client := New(zap.NewNop(), nil, cfg)
	storeSession(t, cfg.Agent.AgentURI, true)
	s := client.session()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	grp, gctx := errgroup.WithContext(ctx)
	gctx = context.WithValue(gctx, models.ErrGroupKey, grp)

	incoming, err := client.GetIncoming(gctx, models.IncomingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.MakeAgentReadyForDockerCompose(gctx); err != nil {
		t.Fatal(err)
	}
	first, _ := recv(t, incoming)
	if err := s.reapply(gctx, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	second, _ := recv(t, incoming)
	s.end()

	mu.Lock()
	defer mu.Unlock()
	if len(asked) < 2 || asked[0] != "base64" || asked[1] != "base64" {
		t.Errorf("Keploy-Body-Encoding per /incoming connection: %q", asked)
	}
	for i, tc := range []*models.TestCase{first, second} {
		if tc == nil {
			t.Fatalf("connection %d: no test case", i+1)
		}
		if tc.HTTPReq.Body != reqBody || tc.HTTPResp.Body != respBody {
			t.Errorf("connection %d: request %q, response %q", i+1, tc.HTTPReq.Body, tc.HTTPResp.Body)
		}
	}
}
