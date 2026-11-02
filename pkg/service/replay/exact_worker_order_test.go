package replay

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap/zapcore"
)

// keploy test hands the agent, on the set's staging, every test case of the
// set in the order its run takes them up (models.ExactServing.Order):
// recorded order, with the streaming test cases (run in a pass after the
// rest) last, and with the ones it does not run among them (a test case a
// selection leaves out, a quarantined one), so the agent reaches a
// worker-scope mock's place once the run passes its test case, skipped or
// not. A set whose worker scope places no mock after a test case is staged
// with none: the agent reads the order for nothing else.
func TestTheStagingHandsTheAgentTheRunsOrderOfEveryTestCase(t *testing.T) {
	for _, tc := range []struct {
		name   string
		worker []models.MockEntry
		want   []string
	}{
		{"a worker mock placed after a test case",
			[]models.MockEntry{{Name: "mock-boot", Kind: string(models.HTTP)}, {Name: "mock-late", Kind: string(models.HTTP), Place: "test-1"}},
			[]string{"test-1", "test-3", "test-4", "test-2"}},
		{"no worker mock placed after a test case",
			[]models.MockEntry{{Name: "mock-boot", Kind: string(models.HTTP)}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSwitch(t, true)
			fastWorkerWait(t)
			h := newPartialRunHarness(t, 4, 0)
			pinWindows(h)
			h.streaming("test-2")
			h.replayer.config.Command = "./app"
			// test-1 is quarantined, test-3 left out by a selection.
			h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": {"test-1", "test-2", "test-4"}}
			h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {"test-1"}}
			h.mocks.filtered = []*models.Mock{httpAt("mock-boot", 500), httpAt("mock-a", 1010), httpAt("mock-late", 1050)}
			s12MappingFile(t, h, nil, tc.worker...)
			h.run(t)
			ex, ok := stagedExact(h)
			if !ok {
				t.Fatal("the set was not staged")
			}
			if !reflect.DeepEqual(ex.Order, tc.want) {
				t.Fatalf("the staging handed the agent the run's order %v; want %v", ex.Order, tc.want)
			}
		})
	}
}

// passOrder keeps every test case, in recorded order, the HTTP streaming ones
// last in their own recorded order; stagedOrder hands it on only when a mock
// of the worker scope the agent stages is placed after a test case.
func TestPassOrderAndWhenTheStagingHandsItOn(t *testing.T) {
	plain := func(name string) *models.TestCase {
		return &models.TestCase{Name: name, Kind: models.HTTP, HTTPResp: models.HTTPResp{StatusCode: 200}}
	}
	stream := func(name string) *models.TestCase {
		tc := plain(name)
		tc.HTTPResp.Header = map[string]string{"Content-Type": "application/x-ndjson"}
		return tc
	}
	// A gRPC test case is never deferred, whatever its HTTP response says.
	grpc := stream("g-5")
	grpc.Kind = models.GRPC_EXPORT
	got, main := passOrder([]*models.TestCase{stream("s-1"), plain("t-2"), stream("s-3"), plain("t-4"), grpc})
	if want := []string{"t-2", "t-4", "g-5", "s-1", "s-3"}; !reflect.DeepEqual(got, want) || main != 3 {
		t.Fatalf("passOrder = %v, %d; want %v, 3", got, main, want)
	}
	order := []string{"t-1", "t-2"}
	for _, tc := range []struct {
		name string
		plan *exactPlan
		want []string
	}{
		{"no plan", nil, nil},
		{"no worker scope", &exactPlan{order: order}, nil},
		{"a worker scope at the set's start only", &exactPlan{order: order, worker: []models.MockEntry{{Name: "w"}}}, nil},
		{"a placed mock the agent does not hold", &exactPlan{order: order, worker: []models.MockEntry{{Name: "w"}, {Name: "p", Place: "t-1"}}, unheld: map[string]bool{"p": true}}, nil},
		{"a placed mock the agent holds", &exactPlan{order: order, worker: []models.MockEntry{{Name: "w"}, {Name: "p", Place: "t-1"}}}, order},
	} {
		if got := tc.plan.stagedOrder(); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: stagedOrder = %v; want %v", tc.name, got, tc.want)
		}
	}
}

// passTrace is, in order, the test cases the run opened on the agent and the
// ends of its passes it told the agent of (models.ExactServing.Passed, as
// "passed N").
func passTrace(h *prRun) []string {
	h.instr.mu.Lock()
	defer h.instr.mu.Unlock()
	var out []string
	for _, p := range h.instr.allParams {
		switch ex := p.Exact; {
		case ex == nil || ex.Stage:
		case ex.Test != "":
			out = append(out, ex.Test)
		case ex.Passed != 0:
			out = append(out, fmt.Sprintf("passed %d", ex.Passed))
		}
	}
	return out
}

// As each pass of the set's run ends, keploy test tells the agent how many
// test cases of the run's order it has passed (models.ExactServing.Passed),
// after the pass's last test case: the main loop's (those before the
// streaming test cases) when the streaming pass runs after it, else every
// test case, and every test case at the streaming pass's end. So a test case
// skipped after the last one the run opened (a quarantined last test case, a
// --testcase selection that ends before the set does, a quarantined one
// before the streaming pass) reaches the worker-scope mocks placed after it.
// A retry cycle's end tells the agent nothing more, and a set whose staging
// handed the agent no order is told nothing. A pass that ends on a stop
// tells it nothing either: the application stopping part way, a result the
// report store refuses, the compose stack halting (keploy's agent with it)
// as the last test case ends, the agent refusing a test case's opening, or
// the agent beginning no streaming test case. A last test case that fails
// unsent ends its pass as any other. A pass end the agent does not take is
// said once, as one that may leave those places unreached, and the next
// pass end (a retry cycle's) tells the agent again; a pool call between
// test cases it does not take is said once, as before.
func TestTheEndOfEachPassTellsTheAgentWhatTheRunPassed(t *testing.T) {
	placed := []models.MockEntry{{Name: "mock-boot", Kind: string(models.HTTP)}, {Name: "mock-late", Kind: string(models.HTTP), Place: "test-4"}}
	for _, tc := range []struct {
		name      string
		worker    []models.MockEntry
		streaming []string
		selected  []string
		ignored   []string
		retry     bool
		stopNth   int    // the application exits at this filter call
		unsaved   string // a test case whose result the report store refuses
		// unsendable is a test case whose URL has no origin for the run's
		// base path: it fails unsent.
		unsendable string
		// compose runs the application under docker compose: the set is
		// served, its agent holding the set's mocks as the stack's bring-up
		// left them.
		compose bool
		// refusePool is the pool call (1 for the set's first) the agent
		// does not take: under compose, the stack's halt, keploy's agent
		// going down with it.
		refusePool int
		refuseOpen int    // the agent opens no capture window at this one (1 for the set's first)
		refuseTest string // the agent refuses this test case's opening
		want       []string
		// poolSaid and passEndSaid are how many times the run says that the
		// agent did not take a pool call between test cases, and a pass end.
		poolSaid, passEndSaid int
	}{
		{name: "a quarantined last test case", worker: placed, ignored: []string{"test-4"},
			want: []string{"test-1", "test-2", "test-3", "passed 4"}},
		{name: "a selection that ends before the set does", worker: placed, selected: []string{"test-1", "test-2"},
			want: []string{"test-1", "test-2", "passed 4"}},
		{name: "a quarantined test case before the streaming pass", worker: placed, streaming: []string{"test-4"}, ignored: []string{"test-3"},
			want: []string{"test-1", "test-2", "passed 3", "test-4", "passed 4"}},
		{name: "a quarantined streaming test case, so no streaming pass", worker: placed, streaming: []string{"test-4"}, ignored: []string{"test-4"},
			want: []string{"test-1", "test-2", "test-3", "passed 4"}},
		{name: "a retry cycle after the first", worker: placed, ignored: []string{"test-4"}, retry: true,
			want: []string{"test-1", "test-2", "test-3", "passed 4", "test-2", "test-3"}},
		{name: "the application stops part way", worker: placed, ignored: []string{"test-4"}, stopNth: 3,
			want: []string{"test-1"}},
		{name: "the application stops in the last test case", worker: placed, ignored: []string{"test-4"}, stopNth: 6,
			want: []string{"test-1", "test-2", "test-3"}},
		{name: "the application stops in the streaming pass", worker: placed, streaming: []string{"test-4"}, ignored: []string{"test-3"}, stopNth: 7,
			want: []string{"test-1", "test-2", "passed 3", "test-4"}},
		{name: "the streaming test case's result cannot be saved", worker: placed, streaming: []string{"test-4"}, ignored: []string{"test-3"}, unsaved: "test-4",
			want: []string{"test-1", "test-2", "passed 3", "test-4"}},
		{name: "the last test case cannot be sent", worker: placed, unsendable: "test-4",
			want: []string{"test-1", "test-2", "test-3", "passed 4"}},
		{name: "the last test case cannot be sent and its result cannot be saved", worker: placed, unsendable: "test-4", unsaved: "test-4",
			want: []string{"test-1", "test-2", "test-3"}},
		{name: "under docker compose", worker: placed, ignored: []string{"test-4"}, compose: true,
			want: []string{"test-1", "test-2", "test-3", "passed 4"}},
		{name: "the compose stack halts as the last test case ends", worker: placed, ignored: []string{"test-4"}, compose: true, refusePool: 3,
			want: []string{"test-1", "test-2", "test-3"}, poolSaid: 1},
		{name: "the agent refuses a test case's opening", worker: placed, refuseTest: "test-2",
			want: []string{"test-1"}},
		{name: "the agent refuses the last test case's opening", worker: placed, refuseTest: "test-4",
			want: []string{"test-1", "test-2", "test-3"}},
		{name: "the agent does not take the first cycle's pass end", worker: placed, ignored: []string{"test-4"}, retry: true, refusePool: 4,
			want: []string{"test-1", "test-2", "test-3", "test-2", "test-3", "passed 4"}, passEndSaid: 1},
		{name: "the agent does not begin the streaming test case", worker: placed, streaming: []string{"test-4"}, ignored: []string{"test-3"}, refuseOpen: 3,
			want: []string{"test-1", "test-2", "passed 3"}},
		{name: "no worker mock placed after a test case", worker: placed[:1], ignored: []string{"test-4"},
			want: []string{"test-1", "test-2", "test-3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSwitch(t, true)
			fastWorkerWait(t)
			h := newPartialRunHarness(t, 4, 0)
			pinWindows(h)
			h.streaming(tc.streaming...)
			h.replayer.config.Command = "./app"
			if tc.selected != nil {
				h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": tc.selected}
			}
			// A set listed with no test cases is quarantined whole.
			if tc.ignored != nil {
				h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": tc.ignored}
			}
			// test-1 fails (the harness's application consumes none of
			// its mock-a), so the passing ones run again.
			h.replayer.config.RetryPassing = tc.retry
			h.instr.stopAppAfterNUpdates = tc.stopNth
			if tc.unsaved != "" {
				h.report.failInsertOf = map[string]error{tc.unsaved: errors.New("report store is unwritable")}
			}
			if tc.unsendable != "" {
				h.replayer.config.Test.BasePath = strings.TrimSuffix(h.cases[0].HTTPReq.URL, "/test-1")
				for _, c := range h.cases {
					if c.Name == tc.unsendable {
						c.HTTPReq.URL = "/" + c.Name
					}
				}
			}
			h.mocks.filtered = []*models.Mock{httpAt("mock-boot", 500), httpAt("mock-a", 1010), httpAt("mock-late", 1050)}
			s12MappingFile(t, h, nil, tc.worker...)
			if tc.compose || tc.refusePool != 0 || tc.refuseOpen != 0 || tc.refuseTest != "" {
				vi := &viewInstr{prInstr: h.instr,
					failPool:   func(n int) bool { return n == tc.refusePool },
					poolErr:    errors.New("dial tcp 172.18.0.3:8086: connect: connection refused"),
					failOpen:   func(n int) bool { return n == tc.refuseOpen },
					refuseTest: tc.refuseTest}
				h.replayer.instrumentation = vi
				if tc.compose {
					h.replayer.config.CommandType = string(utils.DockerCompose)
					vi.loaded = len(h.mocks.filtered)
				}
			}
			logs := observeLog(h)
			if tc.compose {
				h.runServing(t)
			} else {
				h.run(t)
			}
			if got := passTrace(h); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("the run opened and passed %v; want %v", got, tc.want)
			}
			pool := logs.FilterMessage("failed to serve the test set's pool between test cases").Len()
			passEnd := logs.FilterLevelExact(zapcore.WarnLevel).FilterMessage(passEndUnconfirmedMsg).Len()
			if pool != tc.poolSaid || passEnd != tc.passEndSaid {
				t.Fatalf("the run said %d times that the agent did not take a pool call between test cases and %d times a pass end; want %d and %d",
					pool, passEnd, tc.poolSaid, tc.passEndSaid)
			}
		})
	}
}
