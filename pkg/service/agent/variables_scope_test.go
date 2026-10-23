package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
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

// OpenExactTest is the run's pool going in with the test's variables begun,
// in one step (coreAgent.ExactTestOpener).
func (p *orderProxy) OpenExactTest(test string, owned []*models.Mock, given []models.VariableBinding) error {
	p.tbl.Start(given)
	p.log("open " + test + bindsOf(given))
	return p.workerPool.ServeExactTest(test, owned)
}

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

// A test's variables' scope begins as its pool goes in: in the step that
// opens it under exact serving (the run's pool, worker 0), or once a worker's
// view is in (a worker pid, which the proven wire never sends).
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
	for _, want := range []string{"clock-begin pkg::TestA, stage, open pkg::TestA", "worker-pool, reset"} {
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
		if e == "reset" || strings.HasPrefix(e, "open ") {
			t.Fatalf("a failed begin began a scope; events %v", p.events)
		}
	}
}

// windowProxy stages a test's window and keeps no test variables.
type windowProxy struct {
	workerPool
	mu     sync.Mutex
	events []string
}

func (p *windowProxy) log(e string) { p.mu.Lock(); p.events = append(p.events, e); p.mu.Unlock() }
func (p *windowProxy) SetMocksWithWindow(context.Context, []*models.Mock, []*models.Mock, time.Time, time.Time) error {
	p.log("window")
	return nil
}

// stepProxy is a windowProxy that keeps test variables, begun apart from a
// test's window going in (coreAgent.VariableBinder): two steps.
type stepProxy struct{ windowProxy }

func (p *stepProxy) StartVariables(given []models.VariableBinding) bool {
	p.log("start" + bindsOf(given))
	return true
}
func (p *stepProxy) BoundVariables() ([]models.VariableBinding, bool) { return nil, true }

// openerProxy is a stepProxy that opens a test's window with its variables in
// one step (coreAgent.WindowedTestOpener).
type openerProxy struct{ stepProxy }

func (p *openerProxy) OpenTestWithWindow(_ context.Context, _, _ []*models.Mock, _, _ time.Time, given []models.VariableBinding) error {
	p.log("open-window" + bindsOf(given))
	return nil
}

// unwindowedOpener keeps test variables and has a window opener, but stages
// with SetMocks, not a test's window (no coreAgent.WindowedProxy): it cannot
// open a test's window at all.
type unwindowedOpener struct {
	workerPool
	mu     sync.Mutex
	events []string
}

func (p *unwindowedOpener) log(e string) { p.mu.Lock(); p.events = append(p.events, e); p.mu.Unlock() }
func (p *unwindowedOpener) SetMocks(context.Context, []*models.Mock, []*models.Mock) error {
	p.log("set-mocks")
	return nil
}
func (p *unwindowedOpener) StartVariables([]models.VariableBinding) bool { p.log("start"); return true }
func (p *unwindowedOpener) BoundVariables() ([]models.VariableBinding, bool) {
	return nil, true
}
func (p *unwindowedOpener) OpenTestWithWindow(context.Context, []*models.Mock, []*models.Mock, time.Time, time.Time, []models.VariableBinding) error {
	p.log("open-window")
	return nil
}

// apartProxy keeps test variables, and serves a test exactly, but cannot
// begin its variables as it opens it.
type apartProxy struct {
	workerPool
	resets int
}

func (p *apartProxy) ResetVariables() { p.resets++ }

var (
	_ coreAgent.ExactTestOpener    = (*orderProxy)(nil)
	_ coreAgent.WindowedTestOpener = (*openerProxy)(nil)
	_ coreAgent.WindowedTestOpener = (*unwindowedOpener)(nil)
)

func bindsOf(bs []models.VariableBinding) string {
	var out string
	for _, b := range bs {
		out += " bind:" + b.Name + "=" + b.Value
	}
	return out
}

// A test of keploy mock replay opens with its variables in one step, none
// bound: nothing begins them after it opened, which would leave a window
// between its mocks going live and its variables beginning. A proxy that
// keeps test variables but cannot begin them so refuses the begin; one that
// keeps none opens the test with nothing to begin.
func TestATestOfKeployMockReplayOpensWithItsVariables(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	stored := &ClientMockStorage{filtered: []*models.Mock{{Name: "m1", Kind: models.HTTP}}}
	ctx := context.Background()
	t.Run("one step", func(t *testing.T) {
		a, p := orderAgent(t)
		a.clientMocks.Store(uint64(0), stored)
		_ = a.SetScopeTable(ctx, map[string][]string{"pkg::TestA": {"m1"}})
		if err := a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()); err != nil {
			t.Fatal(err)
		}
		got := strings.Join(p.events, ", ")
		if !strings.Contains(got, "clock-begin pkg::TestA, ") || !strings.HasSuffix(got, ", open pkg::TestA") {
			t.Fatalf("events %s; want the test opened with its variables, last", got)
		}
		for _, apart := range []string{"reset", "pool"} {
			if strings.Contains(got, apart) {
				t.Fatalf("events %s: the test's variables began apart from its opening (%s)", got, apart)
			}
		}
	})
	t.Run("keeps variables, opens apart", func(t *testing.T) {
		p := &apartProxy{}
		a := newReplayAgent()
		a.Proxy = p
		a.clientMocks.Store(uint64(0), stored)
		_ = a.SetScopeTable(ctx, map[string][]string{"pkg::TestA": {"m1"}})
		if err := a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()); err == nil {
			t.Fatal("a proxy that cannot begin a test's variables as it opens the test opened it")
		}
		if test, _, _, _ := p.ExactTestOutcome(); test != "" || p.resets != 0 {
			t.Fatalf("the refused begin opened %q and forgot the variables %d times", test, p.resets)
		}
	})
	t.Run("keeps none", func(t *testing.T) {
		p := &workerPool{}
		a := newReplayAgent()
		a.Proxy = p
		a.clientMocks.Store(uint64(0), stored)
		_ = a.SetScopeTable(ctx, map[string][]string{"pkg::TestA": {"m1"}})
		if err := a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()); err != nil {
			t.Fatal(err)
		}
		if test, _, _, _ := p.ExactTestOutcome(); test != "pkg::TestA" {
			t.Fatalf("opened %q", test)
		}
	})
}

// A keploy test case's window opens with the test's variables in one step,
// the values its request gave bound, on a proxy that can. A proxy that keeps
// test variables but cannot begin them so refuses it, before anything is
// staged; one that keeps none stages the window when nothing is given, and
// refuses given values. A filter that opens no test case is refused: it would
// begin a test's variables nowhere.
func TestAKeployTestCasesWindowOpensWithItsVariables(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	open := models.MockFilterParams{
		AfterTime: time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC), BeforeTime: time.Date(2026, 10, 1, 10, 0, 5, 0, time.UTC),
		Variables: []models.VariableBinding{{Name: "id", Value: "u1"}},
	}
	none := open
	none.Variables = nil
	opener, twoSteps, plain := &openerProxy{}, &stepProxy{}, &windowProxy{}
	unwindowed := &unwindowedOpener{}
	for _, tc := range []struct {
		name   string
		proxy  coreAgent.Proxy
		events *[]string
		params models.MockFilterParams
		opens  bool
		want   string
	}{
		{"one step", opener, &opener.events, open, true, "open-window bind:id=u1"},
		{"keeps variables, opens apart", twoSteps, &twoSteps.events, none, false, ""},
		{"keeps none, nothing given", plain, &plain.events, none, true, "window"},
		{"keeps none, a value given", plain, &plain.events, open, false, "window"},
		{"keeps variables, stages no window", unwindowed, &unwindowed.events, open, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newReplayAgent()
			a.Proxy = tc.proxy
			a.clientMocks.Store(uint64(0), &ClientMockStorage{})
			ctx := context.Background()
			if err := a.OpenTestCase(ctx, tc.params); (err == nil) != tc.opens {
				t.Fatalf("opened: %v; want %v", err, tc.opens)
			}
			if got := strings.Join(*tc.events, ", "); got != tc.want {
				t.Fatalf("events %s; want %s", got, tc.want)
			}
			if err := a.OpenTestCase(ctx, models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: time.Now()}); err == nil {
				t.Fatal("a set's staging opened a test case")
			}
		})
	}
}

// requestProxy is an openerProxy whose learn step reads a test case's own
// request (coreAgent.TestRequestTaker).
type requestProxy struct{ openerProxy }

func (p *requestProxy) AwaitTestRequest() { p.log("await") }

func (p *requestProxy) TakeTestRequest(req *models.TestRequest) {
	if req == nil {
		p.log("request none")
		return
	}
	p.log("request " + req.Test)
}

var _ coreAgent.TestRequestTaker = (*requestProxy)(nil)

// A keploy test case's filter hands its request to the learn step before its
// window opens (the replay sends the request once the filter is answered);
// a filter that carries none (a keploy older than its agent) is handed as
// none, which the learn step reads as a request it could not read. A set's
// staging hands nothing.
func TestAKeployTestCaseHandsItsRequestToTheLearnStepFirst(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	open := models.MockFilterParams{
		AfterTime: time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC), BeforeTime: time.Date(2026, 10, 1, 10, 0, 5, 0, time.UTC),
		TestRequest: &models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{{Name: "url", Recorded: "/a", Sent: "/a"}}},
	}
	unsent := open
	unsent.TestRequest = nil
	for _, tc := range []struct {
		name   string
		params models.MockFilterParams
		want   string
	}{
		{"with its request", open, "request test-1, open-window"},
		{"from a keploy older than its agent", unsent, "request none, open-window"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &requestProxy{}
			a := newReplayAgent()
			a.Proxy = p
			a.clientMocks.Store(uint64(0), &ClientMockStorage{})
			if err := a.OpenTestCase(context.Background(), tc.params); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(p.events, ", "); got != tc.want {
				t.Fatalf("events %s; want %s", got, tc.want)
			}
			_ = a.OpenTestCase(context.Background(), models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: time.Now()})
			if got := strings.Join(p.events, ", "); got != tc.want {
				t.Fatalf("after a staging, events %s; want %s", got, tc.want)
			}
		})
	}
}

// keploy test opening a test's capture window tells the learn step the test
// has a request of its own, which its opening filter brings: until then the
// learn step reads it as one it could not read. With the switch off it tells
// nothing.
func TestKeployTestsCaptureWindowAwaitsTheTestCasesRequest(t *testing.T) {
	for _, on := range []string{"1", "0"} {
		t.Run("KEPLOY_PROVEN="+on, func(t *testing.T) {
			t.Setenv(provenswitch.Env, on)
			p := &requestProxy{}
			a := newReplayAgent()
			a.Proxy = p
			if err := a.BeginTestErrorCapture(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := a.ContinueTestErrorCapture(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := "await, await"
			if on != "1" {
				want = ""
			}
			if got := strings.Join(p.events, ", "); got != want {
				t.Fatalf("events %q; want %q", got, want)
			}
			// A proxy that carries a window's misses into the next one
			// (keploy test's path for every test after a set's first)
			// awaits the request too, after the window opens.
			c := &continuingRequestProxy{}
			a.Proxy = c
			if err := a.ContinueTestErrorCapture(context.Background()); err != nil {
				t.Fatal(err)
			}
			want = "continue, await"
			if on != "1" {
				want = "continue"
			}
			if got := strings.Join(c.events, ", "); got != want {
				t.Fatalf("continued: events %q; want %q", got, want)
			}
		})
	}
}

// continuingRequestProxy is a requestProxy whose capture window carries the
// misses of the previous one in (ContinueTestErrorCapture).
type continuingRequestProxy struct{ requestProxy }

func (p *continuingRequestProxy) ContinueTestErrorCapture() { p.log("continue") }

// The agent says its OpenTestCase hands the learn step a test case's request
// (models.TestRequestReadHeader) only under the switch, with a proxy that
// takes one.
func TestTheAgentSaysItReadsATestCasesRequestOnlyWhereItDoes(t *testing.T) {
	for _, tc := range []struct {
		on    string
		proxy coreAgent.Proxy
		want  bool
	}{
		{"1", &requestProxy{}, true},
		{"0", &requestProxy{}, false},
		{"1", &openerProxy{}, false},
	} {
		t.Setenv(provenswitch.Env, tc.on)
		a := newReplayAgent()
		a.Proxy = tc.proxy
		if got := a.ReadsTestRequest(); got != tc.want {
			t.Errorf("KEPLOY_PROVEN=%s, %T: %v; want %v", tc.on, tc.proxy, got, tc.want)
		}
	}
}
