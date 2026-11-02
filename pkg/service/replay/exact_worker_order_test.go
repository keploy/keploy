package replay

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
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
// A retry cycle's end tells the agent nothing more, a run the application
// stops part way tells it nothing, and a set whose staging handed the agent
// no order is told nothing.
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
		want      []string
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
			h.mocks.filtered = []*models.Mock{httpAt("mock-boot", 500), httpAt("mock-a", 1010), httpAt("mock-late", 1050)}
			s12MappingFile(t, h, nil, tc.worker...)
			h.run(t)
			if got := passTrace(h); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("the run opened and passed %v; want %v", got, tc.want)
			}
		})
	}
}
