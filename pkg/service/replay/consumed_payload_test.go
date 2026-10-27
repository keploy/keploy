package replay

// keploy test hands the agent the set's consumed-mock history with every mock
// filter. These run RunTestSet through the real agent client
// (pkg/platform/http) against the real agent routes (pkg/agent/routes): at
// every filter the agent's service must be handed exactly the history the CLI
// held at that call, while what goes on the wire for a test stays the size of
// what was consumed since the filter before it, whatever the test's index.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	httpclient "go.keploy.io/server/v3/pkg/platform/http"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// consumedAgentKind is the release of the agent a replay talks to, as far as
// the consumed history goes.
type consumedAgentKind int

const (
	// keepsHistory keeps the history between filters (models.ConsumedSync),
	// and reads it for its per-test mocks only.
	keepsHistory consumedAgentKind = iota
	// saysScopeOnly reads the history for its per-test mocks only
	// (models.ConsumedScopeHeader) and keeps none.
	saysScopeOnly
	// saysNothing is an agent from v3.0.0-beta1 through v3.3.22.
	saysNothing
)

// consumedSvc is the agent's service: it records the consumed history each
// filter hands it.
type consumedSvc struct {
	agentsvc.Service
	perTest bool
	mu      sync.Mutex
	handed  []map[string]models.MockState
}

func (s *consumedSvc) UpdateMockParams(_ context.Context, p models.MockFilterParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handed = append(s.handed, maps.Clone(p.TotalConsumedMocks))
	return nil
}

func (s *consumedSvc) ReadsConsumedForPerTestOnly(svc agentsvc.Service) bool {
	return s.perTest && svc == agentsvc.Service(s)
}

// consumedReq is one /updatemockparams request as it went on the wire.
type consumedReq struct {
	entries int
	sync    *models.ConsumedSync
}

// consumedWire records every /updatemockparams request. To stand in for an
// agent that keeps no history it takes the request's ConsumedSync out before
// the routes read it: such an agent never reads it, and never answers it.
type consumedWire struct {
	mu    sync.Mutex
	keeps bool
	reqs  []consumedReq
}

func (w *consumedWire) wrap(t *testing.T, next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent/updatemockparams" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			var req models.UpdateMockParamsReq
			if err := json.Unmarshal(body, &req); err != nil {
				t.Error(err)
			}
			w.mu.Lock()
			w.reqs = append(w.reqs, consumedReq{entries: len(req.FilterParams.TotalConsumedMocks), sync: req.ConsumedSync})
			w.mu.Unlock()
			if !w.keeps {
				var raw map[string]json.RawMessage
				if err := json.Unmarshal(body, &raw); err != nil {
					t.Error(err)
				}
				delete(raw, "consumedSync")
				if body, err = json.Marshal(raw); err != nil {
					t.Error(err)
				}
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		next.ServeHTTP(rw, r)
	})
}

// wiredInstr sends the replay's filters through the real agent client before
// the stand-in instrumentation takes them, and records the CLI's consumed
// history as each filter carried it, at the call.
type wiredInstr struct {
	*prInstr
	client *httpclient.AgentClient
	mu     sync.Mutex
	cli    []map[string]models.MockState
}

func (w *wiredInstr) UpdateMockParams(ctx context.Context, p models.MockFilterParams) error {
	w.mu.Lock()
	w.cli = append(w.cli, maps.Clone(p.TotalConsumedMocks))
	w.mu.Unlock()
	if err := w.client.UpdateMockParams(ctx, p); err != nil {
		return err
	}
	return w.prInstr.UpdateMockParams(ctx, p)
}

// consumedTrace is what one replay of a set sent, filter by filter.
type consumedTrace struct {
	h      *prRun
	status models.TestSetStatus
	cli    []map[string]models.MockState
	handed []map[string]models.MockState
	wire   []consumedReq
}

// consumedRun replays a set of tests whose every test consumes five session
// mocks of its own, while the set's three per-test mocks are consumed by the
// first test, against an agent of kind. Tests named in wrongBody fail on
// their answer; with retry their passing siblings then run again in a retry
// cycle.
func consumedRun(t *testing.T, tests int, kind consumedAgentKind, retry bool, wrongBody map[string]bool) consumedTrace {
	t.Helper()
	h := newPartialRunHarness(t, tests, 0)
	h.replayer.config.RetryPassing = retry
	for i := 0; i < 3; i++ {
		h.mocks.filtered = append(h.mocks.filtered, &models.Mock{Name: fmt.Sprintf("per-test-%d", i)})
	}

	svc := &consumedSvc{perTest: kind != saysNothing}
	router := chi.NewRouter()
	routes.DefaultRoutes{}.New(router, svc, zap.NewNop())
	wire := &consumedWire{keeps: kind == keepsHistory}
	srv := httptest.NewServer(wire.wrap(t, router))
	t.Cleanup(srv.Close)
	client := httpclient.New(zap.NewNop(), nil, &config.Config{
		Agent: config.Agent{SetupOptions: models.SetupOptions{AgentURI: srv.URL + "/agent"}},
	})
	wired := &wiredInstr{prInstr: h.instr, client: client}
	h.replayer.instrumentation = wired

	var mu sync.Mutex
	call := 0
	h.replayer.hookImpl = prHooks{wrongBody: wrongBody, consumedFor: func() []models.MockState {
		mu.Lock()
		defer mu.Unlock()
		call++
		var out []models.MockState
		for j := 0; j < 5; j++ {
			name := fmt.Sprintf("session-%d-%d", call, j)
			out = append(out, models.MockState{Name: name, Usage: models.Updated, Lifetime: models.LifetimeSession, Type: "mocks"})
		}
		if call == 2 { // the first test (call 1 drains the setup)
			for i := 0; i < 3; i++ {
				name := fmt.Sprintf("per-test-%d", i)
				out = append(out, models.MockState{Name: name, Usage: models.Deleted, Lifetime: models.LifetimePerTest, Type: "mocks"})
			}
		}
		return out
	}}

	status := h.run(t)
	wired.mu.Lock()
	defer wired.mu.Unlock()
	svc.mu.Lock()
	defer svc.mu.Unlock()
	wire.mu.Lock()
	defer wire.mu.Unlock()
	tr := consumedTrace{h: h, status: status, cli: wired.cli, handed: svc.handed, wire: wire.reqs}
	if len(tr.cli) < tests {
		t.Fatalf("precondition: %d filters for %d tests", len(tr.cli), tests)
	}
	// One request per filter: none of these agents ever needs one sent twice.
	if len(tr.wire) != len(tr.cli) || len(tr.handed) != len(tr.cli) {
		t.Fatalf("%d filters went out as %d requests and reached the service %d times; want one each",
			len(tr.cli), len(tr.wire), len(tr.handed))
	}
	return tr
}

// sameConsumed reports whether a and b name the same states; no map and an
// empty one are the same history.
func sameConsumed(a, b map[string]models.MockState) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// changedSince is the number of mocks whose state in now is not their state in
// before.
func changedSince(before, now map[string]models.MockState) int {
	n := 0
	for name, s := range now {
		if b, ok := before[name]; !ok || !reflect.DeepEqual(b, s) {
			n++
		}
	}
	return n
}

// The count row: with an agent that keeps the history, every filter after the
// first carries only what the tests consumed since the filter before it, so
// the per-test request does not grow with the number of tests already run,
// and the agent's service is still handed exactly the CLI's whole history at
// every filter, under the proven switch and without it.
func TestRunTestSetSendsAnAgentThatKeepsTheHistoryOnlyWhatChanged(t *testing.T) {
	for _, proven := range []bool{false, true} {
		t.Run(fmt.Sprintf("proven=%v", proven), func(t *testing.T) {
			setSwitch(t, proven)
			const tests = 40
			tr := consumedRun(t, tests, keepsHistory, false, nil)
			if tr.status != models.TestSetStatusPassed {
				t.Fatalf("precondition: the set reported %q; want PASSED", tr.status)
			}
			var held map[string]models.MockState
			synced := 0
			for i, req := range tr.wire {
				if !sameConsumed(tr.handed[i], tr.cli[i]) {
					t.Fatalf("filter %d: the service was handed %d consumed entries; the CLI's history had %d", i, len(tr.handed[i]), len(tr.cli[i]))
				}
				if tr.cli[i] == nil {
					continue // a filter that carries no history (exact serving's per-test call)
				}
				if req.sync == nil {
					t.Fatalf("filter %d carried the history with no ConsumedSync: the agent cannot keep it", i)
				}
				if synced == 0 {
					if !req.sync.Full {
						t.Fatalf("filter %d is the history's first send and built on nothing: %+v", i, req.sync)
					}
				} else {
					if req.sync.Full {
						t.Fatalf("filter %d sent the whole history (%d entries) to an agent that holds it", i, req.entries)
					}
					if want := changedSince(held, tr.cli[i]); req.entries != want {
						t.Fatalf("filter %d carried %d entries; %d changed since the filter before it", i, req.entries, want)
					}
					// One test's consumption: five session mocks, and the
					// first test's three per-test ones.
					if req.entries > 8 {
						t.Fatalf("filter %d carried %d entries; no test consumed more than 8", i, req.entries)
					}
				}
				held = tr.cli[i]
				synced++
			}
			// Under the proven switch a test's own filter carries no history
			// (exact serving): the set's staging is the only one that does.
			if !proven && synced < 2 {
				t.Fatalf("precondition: %d filters carried the history; nothing was sent built on an earlier one", synced)
			}
		})
	}
}

// A retry cycle starts again from the set's baseline. The agent keeps the
// history it was sent, and entries are only ever added to it, so the rewound
// history must reach it as a new one, whole: the per-test mocks the first
// cycle consumed are servable again.
func TestRunTestSetSendsARetryCycleItsBaselineWhole(t *testing.T) {
	setSwitch(t, false)
	const tests = 6
	tr := consumedRun(t, tests, keepsHistory, true, map[string]bool{fmt.Sprintf("test-%d", tests): true})
	if tr.status != models.TestSetStatusFailed {
		t.Fatalf("precondition: the set reported %q; want FAILED (test-%d answers wrong, so a retry cycle runs)", tr.status, tests)
	}
	consumedPerTest := false
	rewound := false
	for i, req := range tr.wire {
		if !sameConsumed(tr.handed[i], tr.cli[i]) {
			t.Fatalf("filter %d: the service was handed %d consumed entries; the CLI's history had %d", i, len(tr.handed[i]), len(tr.cli[i]))
		}
		_, perTest := tr.handed[i]["per-test-0"]
		consumedPerTest = consumedPerTest || perTest
		if consumedPerTest && !perTest && req.sync != nil && req.sync.Full {
			rewound = true
		}
	}
	if !consumedPerTest {
		t.Fatal("precondition: no filter carried the per-test mocks the first test consumed")
	}
	if !rewound {
		t.Fatal("no filter after the first cycle sent the agent the rewound history whole; the retry " +
			"cycle's tests are filtered against what the first cycle consumed and fail with match_phase=no_mocks")
	}
}

// An agent that says it reads the history for its per-test mocks only, and
// keeps none, is sent the per-test entries alone with every filter, as before.
func TestRunTestSetSendsAnAgentThatSaysItsScopeOnlyThePerTestEntries(t *testing.T) {
	setSwitch(t, false)
	const tests = 30
	tr := consumedRun(t, tests, saysScopeOnly, false, nil)
	// The first filter follows the store, before the agent has answered
	// anything: the whole history, which is empty or nearly so then. From the
	// second test on, the three consumed per-test entries, however many
	// session mocks the tests before it consumed.
	for i := range tr.wire[2:] {
		i += 2
		if got := len(tr.handed[i]); got != 3 {
			t.Fatalf("filter %d carried %d consumed entries; want the 3 per-test ones", i, got)
		}
		for name, st := range tr.handed[i] {
			if want, ok := tr.cli[i][name]; !ok || !reflect.DeepEqual(st, want) {
				t.Fatalf("filter %d carried %q as %+v; the CLI holds %+v", i, name, st, want)
			}
		}
		if tr.wire[i].sync != nil && !tr.wire[i].sync.Full {
			t.Fatalf("filter %d built on a state of the history an agent that keeps none never held: %+v", i, tr.wire[i].sync)
		}
	}
}

// An agent that has not said how it reads the history gets all of it with
// every filter, exactly as before: agents from v3.0.0-beta1 through v3.3.22
// also applied it to the session pool.
func TestRunTestSetSendsTheWholeConsumedHistoryToAnAgentThatDoesNotSayItsScope(t *testing.T) {
	setSwitch(t, false)
	const tests = 12
	tr := consumedRun(t, tests, saysNothing, false, nil)
	for i := range tr.wire {
		if !sameConsumed(tr.handed[i], tr.cli[i]) || tr.wire[i].entries != len(tr.cli[i]) {
			t.Fatalf("filter %d carried %d entries; the CLI's history had %d", i, tr.wire[i].entries, len(tr.cli[i]))
		}
	}
	if last := tr.cli[len(tr.cli)-1]; len(last) < 5*(tests-1) {
		t.Fatalf("the last filter carried %d entries; the whole history by then holds at least %d", len(last), 5*(tests-1))
	}
}

// What a test's verdict is does not depend on how the history reached the
// agent: the same set, against an agent that keeps it and one that is sent
// all of it every time, gets the same verdicts, with the proven switch on and
// off.
func TestRunTestSetVerdictsDoNotDependOnHowTheHistoryIsSent(t *testing.T) {
	for _, proven := range []bool{false, true} {
		t.Run(fmt.Sprintf("proven=%v", proven), func(t *testing.T) {
			setSwitch(t, proven)
			const tests = 8
			wrong := map[string]bool{"test-3": true}
			kept := consumedRun(t, tests, keepsHistory, true, wrong)
			whole := consumedRun(t, tests, saysNothing, true, wrong)
			if kept.status != whole.status {
				t.Fatalf("the set reported %q against an agent that keeps the history, %q against one sent all of it", kept.status, whole.status)
			}
			verdicts := func(tr consumedTrace) map[string]models.TestStatus {
				out := map[string]models.TestStatus{}
				for _, r := range tr.h.report.report.Tests {
					out[r.TestCaseID] = r.Status
				}
				return out
			}
			if a, b := verdicts(kept), verdicts(whole); len(a) != tests || !reflect.DeepEqual(a, b) {
				t.Fatalf("verdicts against an agent that keeps the history %v; against one sent all of it %v", a, b)
			}
			if len(kept.cli) != len(whole.cli) {
				t.Fatalf("%d filters against an agent that keeps the history, %d against one sent all of it", len(kept.cli), len(whole.cli))
			}
			for i := range kept.cli {
				if !sameConsumed(kept.handed[i], whole.handed[i]) {
					t.Fatalf("filter %d: the service was handed %d entries by deltas, %d whole", i, len(kept.handed[i]), len(whole.handed[i]))
				}
			}
		})
	}
}
