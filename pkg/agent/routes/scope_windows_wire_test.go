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
	if err := client.PushProvenTable(context.Background(), map[string][]string{"o::TestA": {"mock-3"}}, map[string][]string{"o": {"mock-1", "mock-2"}}, map[string][]string{"o": {"mock-2"}}, map[string]string{"mock-2": "o::TestCRUD"}, nil, nil, nil, nil, map[string][]string{"o::TestA": {"mock-3"}}, nil, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"skippedReads":{"mock-2":"o::TestCRUD"}`) || !strings.Contains(body, `"notRequired":{"o::TestA":["mock-3"]}`) {
		t.Fatalf("the CLI posted %s; want the skipped reads under \"skippedReads\" and the marks under \"notRequired\"", body)
	}
	if !maps.Equal(skipped, map[string]string{"mock-2": "o::TestCRUD"}) || !slices.Equal(ps.notRequired["o::TestA"], []string{"mock-3"}) || !slices.Equal(ps.order, []string{"windows", "worker", "skipped", "notRequired", "pool"}) {
		t.Fatalf("the agent installed skipped reads %v, marks %v, in the order %v; want both, before the pool", skipped, ps.notRequired, ps.order)
	}
}

// skippedTrailsSvc is skippedSvc that also takes the test each worker
// recording trails (scopeTrailsSetter).
type skippedTrailsSvc struct {
	skippedSvc
	trails *map[string]string
}

func (s skippedTrailsSvc) SetScopeTrails(_ context.Context, tr map[string]string) error {
	*s.trails = tr
	s.order = append(s.order, "trails")
	return nil
}

// The skipped tests' reads and the trails are two maps of the same type,
// beside each other on the client's call: the wire keeps them apart, under
// their own keys, and the route installs each with the worker scope. A swap
// compiles, and would name a skipped test where a trail goes.
func TestTheSkippedReadsAndTheTrailsCrossTheScopeTableWireApart(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	ps, skipped, trails := &poolSvc{}, map[string]string{}, map[string]string{}
	a := &Agent{logger: zap.NewNop(), svc: skippedTrailsSvc{skippedSvc{workerSvc{ps}, &skipped, nil}, &trails}}
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		r.Body = io.NopCloser(bytes.NewReader(b))
		a.HandleScopeTable(w, r)
	}))
	t.Cleanup(srv.Close)
	client := agenthttp.New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: srv.URL}})
	worker := map[string][]string{"o": {"mock-1", "mock-2"}}
	if err := client.PushProvenTable(context.Background(), map[string][]string{"o::TestA": {"mock-3"}}, worker, worker, map[string]string{"mock-2": "o::TestCRUD"}, map[string]string{"mock-1": "o::TestA"}, nil, nil, nil, nil, nil, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"skippedReads":{"mock-2":"o::TestCRUD"}`) || !strings.Contains(body, `"trails":{"mock-1":"o::TestA"}`) {
		t.Fatalf("the CLI posted %s; want the skipped reads under \"skippedReads\" and the trails under \"trails\"", body)
	}
	if !maps.Equal(skipped, map[string]string{"mock-2": "o::TestCRUD"}) || !maps.Equal(trails, map[string]string{"mock-1": "o::TestA"}) || !slices.Equal(ps.order, []string{"windows", "worker", "skipped", "trails", "pool"}) {
		t.Fatalf("the agent installed skipped reads %v, trails %v, in the order %v; want each its own, before the pool", skipped, trails, ps.order)
	}
}

// backgroundSvc is workerSvc that also takes the set's background
// recordings' names (scopeBackgroundSetter).
type backgroundSvc struct {
	workerSvc
	background *[]string
	set        *int
}

func (s backgroundSvc) SetScopeBackground(_ context.Context, names []string) error {
	*s.background = names
	*s.set++
	s.order = append(s.order, "background")
	return nil
}

// The set's background recordings (its worker: and startup: mocks) cross
// the scope table's wire under "background", and the route installs them
// before the pool, whose serving stages the set and hands them to the proxy
// with it: installed after, the staging would hand the proxy none, and no
// parser could mark a poll made once more than recorded as the
// application's own. A table that names none installs none, so a set never
// keeps the previous set's; an agent that cannot take them takes the table
// all the same, and every miss stays its test's.
func TestTheBackgroundRecordingsCrossTheScopeTableWireBeforeThePool(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	ps, background, set := &poolSvc{}, []string{}, 0
	a := &Agent{logger: zap.NewNop(), svc: backgroundSvc{workerSvc{ps}, &background, &set}}
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		r.Body = io.NopCloser(bytes.NewReader(b))
		a.HandleScopeTable(w, r)
	}))
	t.Cleanup(srv.Close)
	client := agenthttp.New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: srv.URL}})
	pool := map[string][]string{".": {"mock-0", "mock-3733", "mock-3735"}}
	worker := map[string][]string{".": {"mock-3733", "mock-3735"}}
	if err := client.PushProvenTable(context.Background(), map[string][]string{".::TestGetCurrentUser": {"mock-3734"}}, pool, worker, nil, nil, nil, nil, nil, nil, []string{"mock-3733", "mock-3735", "mock-0"}, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"background":["mock-3733","mock-3735","mock-0"]`) {
		t.Fatalf("the CLI posted %s; want the background recordings under \"background\"", body)
	}
	if !slices.Equal(background, []string{"mock-3733", "mock-3735", "mock-0"}) || !slices.Equal(ps.order, []string{"windows", "worker", "background", "pool"}) {
		t.Fatalf("the agent installed the background recordings %v, in the order %v; want them, before the pool", background, ps.order)
	}
	if err := client.PushProvenTable(context.Background(), map[string][]string{".::TestGetCurrentUser": {"mock-3734"}}, pool, worker, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, `"background"`) || background != nil || set != 2 {
		t.Fatalf("a table naming none posted %s and installed %v (%d installs); want none installed, the previous set's cleared", body, background, set)
	}
	// An agent that cannot take them takes the table.
	b := &Agent{logger: zap.NewNop(), svc: workerSvc{&poolSvc{}}}
	srv2 := httptest.NewServer(http.HandlerFunc(b.HandleScopeTable))
	t.Cleanup(srv2.Close)
	client2 := agenthttp.New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: srv2.URL}})
	if err := client2.PushProvenTable(context.Background(), map[string][]string{".::TestGetCurrentUser": {"mock-3734"}}, pool, worker, nil, nil, nil, nil, nil, nil, []string{"mock-3733"}, nil, nil, nil, false); err != nil {
		t.Fatalf("an agent that takes no background recordings refused the table: %v", err)
	}
}
