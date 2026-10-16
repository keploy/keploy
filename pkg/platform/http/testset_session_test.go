package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// testSetAgent is an agent that notes the set's begins and ends and the test
// filters it is sent, in order.
type testSetAgent struct {
	mu      sync.Mutex
	calls   []string
	begins  []models.TestSetBegin
	filters []models.MockFilterParams
}

func (a *testSetAgent) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	switch r.URL.Path {
	case "/agent" + models.BeginTestSetPath:
		var req models.BeginTestSetReq
		_ = json.Unmarshal(b, &req)
		a.calls = append(a.calls, "begin "+req.TestSet.TestSetID)
		a.begins = append(a.begins, req.TestSet)
	case "/agent" + models.EndTestSetPath:
		var req models.EndTestSetReq
		_ = json.Unmarshal(b, &req)
		a.calls = append(a.calls, "end "+req.TestSetID)
	case "/agent/updatemockparams":
		var req models.UpdateMockParamsReq
		_ = json.Unmarshal(b, &req)
		a.calls = append(a.calls, "test "+req.FilterParams.AfterTime.Format(time.TimeOnly))
		a.filters = append(a.filters, req.FilterParams)
	}
	a.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(models.AgentResp{IsSuccess: true})
}

func (a *testSetAgent) take() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.calls
	a.calls = nil
	return out
}

func newTestSetClient(t *testing.T) (*AgentClient, *testSetAgent) {
	t.Helper()
	agent := &testSetAgent{}
	srv := httptest.NewServer(http.HandlerFunc(agent.handler))
	t.Cleanup(srv.Close)
	cfg := &config.Config{}
	cfg.Agent.AgentURI = srv.URL + "/agent"
	client := New(zap.NewNop(), nil, cfg)
	storeSession(t, cfg.Agent.AgentURI, true)
	return client, agent
}

// A compose agent docker compose starts again mid-set is set up from what the
// session recorded: it is told the set began — its first recorded test, its
// recorded windows — and then which test runs. The set's begin used to be a
// test filter (from 2000-01-01 to now) that the latest test's filter replaced,
// so a restarted agent was never told the set began: its startup cutoff was
// the running test's start, and every mock recorded before that test was read
// back from disk and served as app-startup traffic.
func TestRestartedAgentIsBegunBeforeItsLatestTest(t *testing.T) {
	client, agent := newTestSetClient(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	begin := models.TestSetBegin{TestSetID: "test-set-0", FirstRecordedTestStart: t0,
		RecordedWindows: []models.TestWindow{{TestCase: "test-1", Start: t0, End: t0.Add(time.Second)}, {TestCase: "test-2", Start: t0.Add(time.Minute), End: t0.Add(time.Minute + time.Second)}}}
	if err := client.BeginTestSet(ctx, begin); err != nil {
		t.Fatal(err)
	}
	for _, w := range begin.RecordedWindows {
		if err := client.UpdateMockParams(ctx, models.MockFilterParams{AfterTime: w.Start, BeforeTime: w.End}); err != nil {
			t.Fatal(err)
		}
	}
	agent.take()

	if err := client.session().reapply(ctx, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	got := agent.take()
	if len(got) != 2 || got[0] != "begin test-set-0" || got[1] != "test 10:01:00" {
		t.Fatalf("the restarted agent was set up with %v; want the set's begin, then test-2's filter", got)
	}
	agent.mu.Lock()
	last := agent.begins[len(agent.begins)-1]
	agent.mu.Unlock()
	if !last.FirstRecordedTestStart.Equal(t0) || len(last.RecordedWindows) != 2 {
		t.Fatalf("the restarted agent began the set with %+v; want its first recorded test and both windows", last)
	}

	// The next set's begin forgets this set's last test, and its end is not
	// made again: a restarted agent is begun with the set that runs.
	if err := client.EndTestSet(ctx, "test-set-0"); err != nil {
		t.Fatal(err)
	}
	if err := client.BeginTestSet(ctx, models.TestSetBegin{TestSetID: "test-set-1"}); err != nil {
		t.Fatal(err)
	}
	agent.take()
	if err := client.session().reapply(ctx, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if got := agent.take(); len(got) != 1 || got[0] != "begin test-set-1" {
		t.Fatalf("the restarted agent was set up with %v; want test-set-1's begin only", got)
	}
}
