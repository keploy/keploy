package routes

import (
	"bytes"
	"context"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	agenthttp "go.keploy.io/server/v3/pkg/platform/http"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/agent"
)

// refusedWindowSvc answers every window read with one test keploy refused
// at its begin.
type refusedWindowSvc struct{ agent.Service }

const refusedWhy = "t1 began with the pid 4242: keploy narrows a test to its own mocks by the pid of its test process only in a run recorded with the suite mark or an app-start mark"

func (refusedWindowSvc) GetScopeWindows(context.Context) ([]models.ScopeWindow, error) {
	return []models.ScopeWindow{{Name: "t1", PID: 4242, Outcome: models.ScopeOutcomeFailed, BeginRefused: refusedWhy}}, nil
}

func (s refusedWindowSvc) FinalScopeWindows(ctx context.Context) ([]models.ScopeWindow, error) {
	return s.GetScopeWindows(ctx)
}

// The replay fails a test keploy refused at its begin by its window's
// BeginRefused, so the refusal must cross the agent's wire: the agent's
// answer to a windows read, read by the CLI's client, still names the
// refused test and why, with the switch off and on. Without it the replay
// passes on the test command's exit, a test refused or not.
func TestARefusedBeginCrossesTheWindowsWire(t *testing.T) {
	a := &Agent{logger: zap.NewNop(), svc: refusedWindowSvc{}}
	srv := httptest.NewServer(http.HandlerFunc(a.HandleScopeWindows))
	t.Cleanup(srv.Close)
	client := agenthttp.New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: srv.URL}})
	for _, on := range []string{"", "1"} {
		t.Setenv(provenswitch.Env, on)
		got, err := client.GetScopeWindows(context.Background())
		if err != nil {
			t.Fatalf("switch %q: %v", on, err)
		}
		if len(got) != 1 || got[0].Name != "t1" || got[0].PID != 4242 || got[0].BeginRefused != refusedWhy {
			t.Fatalf("switch %q: the CLI read %+v, want t1 (pid 4242) refused at its begin, saying why", on, got)
		}
	}
}

// The skipped tests' reads and the marks cross the scope table's wire: the
// CLI's client posts them under the keys the agent's route reads, spelled as
// every other key of the table and as a window's SkippedReads
// ("skippedReads"), and the route installs both before the pool. A key the
// two sides spelled apart would drop the field with no error: the agent would
// then serve a skipped test's read as the rest of worker:, hidden from a call
// one more than the open test recorded.
func TestTheSkippedReadsAndTheMarksCrossTheScopeTableWire(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	ps, skipped := &poolSvc{}, map[string]string{}
	a := &Agent{logger: zap.NewNop(), svc: bothSvc{skippedSvc{workerSvc{ps}, &skipped, nil}}}
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		r.Body = io.NopCloser(bytes.NewReader(b))
		a.HandleScopeTable(w, r)
	}))
	t.Cleanup(srv.Close)
	client := agenthttp.New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: srv.URL}})
	if err := client.PushProvenTable(context.Background(), map[string][]string{"o::TestA": {"mock-3"}}, map[string][]string{"o": {"mock-1", "mock-2"}}, map[string][]string{"o": {"mock-2"}}, map[string]string{"mock-2": "o::TestCRUD"}, nil, nil, nil, map[string][]string{"o::TestA": {"mock-3"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"skippedReads":{"mock-2":"o::TestCRUD"}`) || !strings.Contains(body, `"notRequired":{"o::TestA":["mock-3"]}`) {
		t.Fatalf("the CLI posted %s; want the skipped reads under \"skippedReads\" and the marks under \"notRequired\"", body)
	}
	if !maps.Equal(skipped, map[string]string{"mock-2": "o::TestCRUD"}) || !slices.Equal(ps.notRequired["o::TestA"], []string{"mock-3"}) || !slices.Equal(ps.order, []string{"windows", "worker", "skipped", "notRequired", "pool"}) {
		t.Fatalf("the agent installed skipped reads %v, marks %v, in the order %v; want both, before the pool", skipped, ps.notRequired, ps.order)
	}
}
