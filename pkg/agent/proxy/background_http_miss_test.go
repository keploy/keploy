package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// In a replay, an HTTP call that is the same request as one of the set's
// HTTP background recordings, made once more than a recording was left to
// answer, is a background miss in the agent's own store: the worker
// scope's liveness probe, answered once in the test, made again there. Its
// miss is listed with the test's misses and counts against no test
// (models.UnmatchedCall.Background), as a Mongo poll's does. A call of
// another path, which no background recording is, stays the test's miss.
func TestAnHTTPProbeMadeOnceMoreThanItsWorkerRecordingIsABackgroundMiss(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	start := time.Now().Add(time.Second)
	probe := r3Mock("probe-1", start.Add(100*time.Millisecond), "GET", "/v1/sys/health", "", `{"n":1}`)
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetMocksWithWindow(nil, []*models.Mock{probe}, models.BaseTime, start.Add(time.Hour))
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{{Mock: probe}})
	mm.ServeExactBackground(models.RecordedWithS12Routing, nil, []string{"probe-1"})
	mm.ServeExactWorker([]string{"probe-1"})
	p.setMockManager(mm)
	t.Cleanup(func() { p.setMockManager(nil) })
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.ServeExactTest("test-0", nil)
	h := httpint.New(zap.NewNop())
	p.ContinueTestErrorCapture()
	if code, body, err := callWithin(h, p, mm, httpGet("/v1/sys/health"), true); err != nil || code != http.StatusOK || body != `{"n":1}` {
		t.Fatalf("the test's first probe got %d %q (%v); want the worker scope's probe-1", code, body, err)
	}
	for _, c := range []struct {
		target     string
		background bool
	}{
		{"/v1/sys/health", true},
		{"/v1/sys/seal-status", false},
	} {
		p.ContinueTestErrorCapture()
		if code, body, err := callWithin(h, p, mm, httpGet(c.target), true); err == nil && code == http.StatusOK {
			t.Fatalf("GET %s was answered %q; want a miss: no recording is left to answer it", c.target, body)
		}
		calls, err := p.GetMockErrors(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(calls) != 1 || calls[0].Background != c.background || calls[0].CountsAgainstTest() == c.background {
			t.Fatalf("GET %s reported %+v; want one miss, Background %v", c.target, calls, c.background)
		}
	}
}
