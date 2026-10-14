package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
)

// orderProxy logs when a test's pool goes in and when its variables' scope
// begins.
type orderProxy struct {
	workerPool
	mu     sync.Mutex
	events []string
	tbl    variables.Table
}

func (p *orderProxy) log(e string) { p.mu.Lock(); p.events = append(p.events, e); p.mu.Unlock() }
func (p *orderProxy) SetMocks(context.Context, []*models.Mock, []*models.Mock) error {
	p.log("stage")
	return nil
}

// ServeExactTest is the run's pool going in under the switch (exact serving).
func (p *orderProxy) ServeExactTest(test string, owned []*models.Mock) error {
	p.log("pool")
	return p.workerPool.ServeExactTest(test, owned)
}
func (p *orderProxy) SetWorkerScope(uint32, []string) { p.log("worker-pool") }
func (p *orderProxy) ResetVariables()                 { p.tbl.Reset(); p.log("reset") }

type orderHooks struct {
	AgentHook
	p *orderProxy
}

func (h *orderHooks) BeginTestClock(_ context.Context, test string, _ time.Time) error {
	h.p.log("clock-begin " + test)
	return nil
}
func (h *orderHooks) EndTestClock(_ context.Context, test string) error {
	h.p.log("clock-end " + test)
	return nil
}
func (h *orderHooks) TimeFile() string { return "" }

func orderAgent(t *testing.T) (*Agent, *orderProxy) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	p := &orderProxy{}
	useHooks(t, &orderHooks{p: p})
	a := newReplayAgent()
	a.Proxy = p
	return a, p
}

// A test's variables' scope begins after its pool is in, whichever way the
// pool goes in: the run's pool (worker 0) or a worker's view.
func TestATestsVariablesBeginAfterItsPool(t *testing.T) {
	a, p := orderAgent(t)
	a.clientMocks.Store(uint64(0), &ClientMockStorage{filtered: []*models.Mock{{Name: "m1", Kind: models.HTTP}, {Name: "m2", Kind: models.HTTP}}})
	ctx := context.Background()
	_ = a.SetScopeTable(ctx, map[string][]string{"pkg::TestA": {"m1"}, "pkg::TestW": {"m2"}})
	if err := a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = a.EndScopeAt(ctx, "pkg::TestA", 0, time.Now())
	if err := a.BeginScopeAt(ctx, "pkg::TestW", 7, time.Now()); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(p.events, ", ")
	for _, want := range []string{"clock-begin pkg::TestA, stage, pool, reset", "worker-pool, reset"} {
		if !strings.Contains(got, want) {
			t.Fatalf("events %s; want %q in order", got, want)
		}
	}
}

// A begin that fails narrowed nothing, and begins no scope: nothing another
// test bound meanwhile is forgotten.
func TestAFailedBeginBeginsNoScope(t *testing.T) {
	a, p := orderAgent(t)
	ctx := context.Background()
	_ = a.SetScopeTable(ctx, map[string][]string{"pkg::TestA": {"m1"}})
	// No mocks are stored, so narrowing the pool fails.
	if err := a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()); err == nil {
		t.Fatal("the begin should fail")
	}
	for _, e := range p.events {
		if e == "reset" {
			t.Fatalf("a failed begin began a scope; events %v", p.events)
		}
	}
}
