package replay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

// namedSetsDB is the harness's test DB over the sets ids, with the set empty
// holding no test case.
type namedSetsDB struct {
	*prTestDB
	ids   []string
	empty string
}

func (d namedSetsDB) GetAllTestSetIDs(context.Context) ([]string, error) { return d.ids, nil }

func (d namedSetsDB) GetTestCases(ctx context.Context, set string) ([]*models.TestCase, error) {
	if set == d.empty {
		return nil, nil
	}
	return d.prTestDB.GetTestCases(ctx, set)
}

// lastSetShape is a run's sets, those of them wholly ignored, and the one with
// no test case; want is the run's last set that runs.
type lastSetShape struct {
	name    string
	ids     []string
	ignored map[string][]string
	empty   string
	want    string
}

// lastSetShapes are runs whose last set that runs is the last by its place,
// and runs whose last set by its place, or a set in the middle, is wholly
// ignored or has no test case.
var lastSetShapes = []lastSetShape{
	{name: "every set runs", ids: []string{"test-set-0", "test-set-1", "test-set-2"}, want: "test-set-2"},
	{name: "the last set wholly ignored", ids: []string{"test-set-0", "test-set-1"}, ignored: map[string][]string{"test-set-1": {}}, want: "test-set-0"},
	{name: "the last set empty", ids: []string{"test-set-0", "test-set-1"}, empty: "test-set-1", want: "test-set-0"},
	{name: "a set between wholly ignored", ids: []string{"test-set-0", "test-set-1", "test-set-2"}, ignored: map[string][]string{"test-set-1": {}}, want: "test-set-2"},
	{name: "a set between empty", ids: []string{"test-set-0", "test-set-1", "test-set-2"}, empty: "test-set-1", want: "test-set-2"},
	{name: "the last two run nothing", ids: []string{"test-set-0", "test-set-1", "test-set-2"}, ignored: map[string][]string{"test-set-2": {}}, empty: "test-set-1", want: "test-set-0"},
}

// apply sets h's run to the shape's sets.
func (s lastSetShape) apply(h *prRun) {
	h.replayer.testDB = namedSetsDB{prTestDB: &prTestDB{cases: h.cases}, ids: s.ids, empty: s.empty}
	h.replayer.config.Test.IgnoredTests = s.ignored
}

// runs is whether set runs a test case in the shape's run.
func (s lastSetShape) runs(set string) bool {
	_, ignored := s.ignored[set]
	return !ignored && set != s.empty
}

// lastCaseHooks is the harness's hooks noting, in order, each test case sent
// with KEPLOY-LAST-TESTCASE (models.TestCase.IsLast) as "set/test", and each
// run-level AfterTestRun as the set of the request sent last before it, with
// " (no agent)" when its run's agent had gone (withoutAgent). A test case
// recorded as a stream is answered with the recorded answer, as one.
type lastCaseHooks struct {
	prHooks
	mu        sync.Mutex
	last      []string
	sentIn    string
	afterRuns []string
}

func (h *lastCaseHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, set string) (interface{}, error) {
	h.mu.Lock()
	h.sentIn = set
	if tc.IsLast {
		h.last = append(h.last, set+"/"+tc.Name)
	}
	h.mu.Unlock()
	if deferredStreaming(tc) {
		return &pkg.StreamingHTTPResponse{StatusCode: tc.HTTPResp.StatusCode, StatusMessage: http.StatusText(tc.HTTPResp.StatusCode),
			Header: tc.HTTPResp.Header, Reader: io.NopCloser(strings.NewReader(tc.HTTPResp.Body)),
			StreamConfig: pkg.HTTPStreamConfig{Mode: pkg.HTTPStreamModeNone}}, nil
	}
	return h.prHooks.SimulateRequest(ctx, tc, set)
}

func (h *lastCaseHooks) AfterTestRun(ctx context.Context, testRunID string, testSetIDs []string, coverage models.TestCoverage) error {
	h.mu.Lock()
	at := h.sentIn
	if agentGone(ctx) {
		at += " (no agent)"
	}
	h.afterRuns = append(h.afterRuns, at)
	h.mu.Unlock()
	return h.prHooks.AfterTestRun(ctx, testRunID, testSetIDs, coverage)
}

// seen is what h noted: the test cases sent as the run's last, and the
// run-level AfterTestRun calls.
func (h *lastCaseHooks) seen() (last, afterRuns []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.last), slices.Clone(h.afterRuns)
}

// keploy test, over a whole run: the run's last test case, the one a request
// carries KEPLOY-LAST-TESTCASE on (an SDK finalizes on it, its coverage among
// it), is the last test case of the last set that runs, not of the last set by
// its place. A later set wholly ignored, or with no test case, returns before
// it sends a request, so with the header keyed on the last set by its place no
// request carried it. Only that one test case carries it, in both modes, for
// a test case sent in the set's first phase and for one recorded as a stream,
// which the set sends after the others, last.
func TestOnlyTheLastSetThatRunsSendsTheLastTestCaseHeader(t *testing.T) {
	for _, on := range []bool{true, false} {
		for _, shape := range lastSetShapes {
			for _, streaming := range []bool{false, true} {
				name := shape.name
				if streaming {
					name += "/its last test case a stream"
				}
				if !on {
					name += "/switch off"
				}
				t.Run(name, func(t *testing.T) {
					provenOn(t, on)
					h := newPartialRunHarness(t, 2, 0)
					if streaming {
						h.streaming("test-2")
					}
					reports := &setsReportDB{prReportDB: h.report}
					h.replayer.reportDB = reports
					shape.apply(h)
					h.replayer.config.Command = "./app"
					h.replayer.config.CommandType = string(utils.Native)
					h.replayer.config.Test.MaxFlakyChecks = 1
					h.replayer.config.DisableANSI = true
					hooks := &lastCaseHooks{}
					h.replayer.hookImpl = hooks

					_, statuses := startEarlierSetRun(t, h, reports)
					last, _ := hooks.seen()
					if want := []string{shape.want + "/test-2"}; !slices.Equal(last, want) {
						t.Fatalf("the test cases sent with KEPLOY-LAST-TESTCASE: %v, want %v: the last test case of the last set that runs, alone", last, want)
					}
					for _, set := range shape.ids {
						if shape.runs(set) && statuses[set] != string(models.TestSetStatusPassed) {
							t.Fatalf("%s is %q (all %v), want PASSED", set, statuses[set], statuses)
						}
					}
				})
			}
		}
	}
}

// Under docker compose with a stack per set (keploy mocks nothing), keploy's
// agent is part of each set's stack and goes down with it, so the run's
// AfterTestRun is called once, in the run's last set by its place when that
// set runs, at its end while its agent is up. When that set is wholly ignored,
// or has no test case, it brings up no stack, and the hook is called once
// after the run, its call to the agent left out: never in the last set that
// runs, before a later set has written its report.
func TestAComposeRunCallsAfterTestRunInTheLastSetByItsPlace(t *testing.T) {
	for _, on := range []bool{true, false} {
		for _, shape := range lastSetShapes {
			name := shape.name
			if !on {
				name += "/switch off"
			}
			t.Run(name, func(t *testing.T) {
				provenOn(t, on)
				h := newPartialRunHarness(t, 2, 0)
				reports := &setsReportDB{prReportDB: h.report}
				h.replayer.reportDB = reports
				shape.apply(h)
				h.replayer.config.Command = "docker compose up"
				h.replayer.config.CommandType = string(utils.DockerCompose)
				h.replayer.config.Test.Mocking = false
				h.replayer.config.Test.MaxFlakyChecks = 1
				h.replayer.config.DisableANSI = true
				// A set that brings its stack up waits for the agent in it to
				// answer /health first.
				agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
				t.Cleanup(agent.Close)
				h.replayer.config.Agent.AgentURI = agent.URL
				hooks := &lastCaseHooks{}
				h.replayer.hookImpl = hooks

				_, statuses := startEarlierSetRun(t, h, reports)
				_, afterRuns := hooks.seen()
				want := []string{shape.want + " (no agent)"}
				if last := shape.ids[len(shape.ids)-1]; shape.runs(last) {
					want = []string{last}
				}
				if !slices.Equal(afterRuns, want) {
					t.Fatalf("the run's AfterTestRun calls, each by the set it came in: %v, want %v: once, in the last set by its place, its agent up, or after the run when that set runs nothing", afterRuns, want)
				}
				for _, set := range shape.ids {
					if shape.runs(set) && statuses[set] != string(models.TestSetStatusPassed) {
						t.Fatalf("%s is %q (all %v), want PASSED", set, statuses[set], statuses)
					}
				}
			})
		}
	}
}

// composeStopsInstr is a docker compose stack's agent (composeInstr) noting,
// at each stop of the app's container, the set whose session was open then,
// and whether test-set-0's end had written its report by then (from reports:
// a status other than the RUNNING its start inserts).
type composeStopsInstr struct {
	*composeInstr
	reports *setsReportDB

	smu            sync.Mutex
	open           string
	stoppedIn      []string
	reportedAtStop bool
}

func (c *composeStopsInstr) MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error {
	c.smu.Lock()
	c.open = models.TestSetSessionOf(ctx).Set
	c.smu.Unlock()
	return c.composeInstr.MockOutgoing(ctx, opts)
}

func (c *composeStopsInstr) StopComposeApp(ctx context.Context) (string, error) {
	c.reports.mu.Lock()
	status, ok := c.reports.statuses["test-set-0"]
	c.reports.mu.Unlock()
	c.smu.Lock()
	c.stoppedIn = append(c.stoppedIn, c.open)
	if len(c.stoppedIn) == 1 {
		c.reportedAtStop = ok && status != string(models.TestStatusRunning)
	}
	c.smu.Unlock()
	return c.composeInstr.StopComposeApp(ctx)
}

// keploy test over a whole run of a docker compose stack kept across its sets
// (keploy mocks the dependencies, so the stack is reused): test-set-0 runs,
// and test-set-1, the run's last by its place, is wholly ignored or has no
// test case. keploy stops the app's container once, in test-set-0, the last
// set that runs, before test-set-0's report: with the switch off after its
// post-script, so the report's read takes the calls the app made as it
// stopped, and under the switch at its end, before its read. Keyed on the
// last set by its place, with the switch off, no set stopped it: the last set
// returned before it, and the app stopped only with the run, after every
// read.
func TestAReusedComposeStackStopsItsAppOnceAtTheLastSetThatRuns(t *testing.T) {
	for _, on := range []bool{false, true} {
		for _, last := range []string{"ignored", "empty"} {
			name := "the last set " + last
			if on {
				name += "/switch on"
			}
			t.Run(name, func(t *testing.T) {
				provenOn(t, on)
				h := newPartialRunHarness(t, 2, 0)
				reports := &setsReportDB{prReportDB: h.report}
				h.replayer.reportDB = reports
				shape := lastSetShape{ids: []string{"test-set-0", "test-set-1"}}
				want := models.TestSetStatusNoTestsToRun
				if last == "ignored" {
					shape.ignored = map[string][]string{"test-set-1": {}}
					want = models.TestSetStatusIgnored
				} else {
					shape.empty = "test-set-1"
				}
				shape.apply(h)
				h.replayer.config.Command = "docker compose up"
				h.replayer.config.CommandType = string(utils.DockerCompose)
				h.replayer.config.Test.Mocking = true
				h.replayer.config.Test.MaxFlakyChecks = 1
				h.replayer.config.DisableANSI = true
				agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
				t.Cleanup(agent.Close)
				h.replayer.config.Agent.AgentURI = agent.URL
				c := &composeStopsInstr{composeInstr: &composeInstr{prInstr: h.instr}, reports: reports}
				h.replayer.instrumentation = c

				_, statuses := startEarlierSetRun(t, h, reports)
				c.smu.Lock()
				stoppedIn, reportedAtStop := slices.Clone(c.stoppedIn), c.reportedAtStop
				c.smu.Unlock()
				if !slices.Equal(stoppedIn, []string{"test-set-0"}) || reportedAtStop {
					t.Fatalf("the app's container was stopped in the sessions %v, test-set-0's report written first: %v; want it stopped once, in test-set-0, before its report", stoppedIn, reportedAtStop)
				}
				if statuses["test-set-0"] != string(models.TestSetStatusPassed) || statuses["test-set-1"] != string(want) {
					t.Fatalf("statuses %v; want test-set-0 PASSED and test-set-1 %s", statuses, want)
				}
			})
		}
	}
}

// reportsAtAfterRunHooks is the harness's hooks noting, at each run-level
// AfterTestRun, the sets of its testSetIDs with no final report yet (none, or
// only the RUNNING one a set's start inserts). Enterprise's AfterTestRun
// uploads the report of every set it is handed, so a set without one fails
// that upload.
type reportsAtAfterRunHooks struct {
	prHooks
	reports *setsReportDB

	mu      sync.Mutex
	calls   int
	missing []string
}

func (h *reportsAtAfterRunHooks) AfterTestRun(ctx context.Context, testRunID string, testSetIDs []string, coverage models.TestCoverage) error {
	h.reports.mu.Lock()
	var missing []string
	for _, s := range testSetIDs {
		if st, ok := h.reports.statuses[s]; !ok || st == string(models.TestStatusRunning) {
			missing = append(missing, s)
		}
	}
	h.reports.mu.Unlock()
	h.mu.Lock()
	h.calls++
	h.missing = append(h.missing, missing...)
	h.mu.Unlock()
	return h.prHooks.AfterTestRun(ctx, testRunID, testSetIDs, coverage)
}

// keploy test under docker compose, a stack per set or one reused across
// sets: the run's AfterTestRun, called once, finds every set's report
// written, in both modes. A trailing set wholly ignored, or with no test case,
// writes its report as it returns, after the set before it: called in the
// last set that runs, the hook found none for it.
func TestAComposeRunsAfterTestRunFindsEverySetsReport(t *testing.T) {
	for _, on := range []bool{false, true} {
		for _, mocking := range []bool{false, true} {
			for _, last := range []string{"runs", "ignored", "empty"} {
				name := "a stack per set"
				if mocking {
					name = "a reused stack"
				}
				name += "/the last set " + last
				if on {
					name += "/switch on"
				}
				t.Run(name, func(t *testing.T) {
					provenOn(t, on)
					h := newPartialRunHarness(t, 2, 0)
					reports := &setsReportDB{prReportDB: h.report}
					h.replayer.reportDB = reports
					shape := lastSetShape{ids: []string{"test-set-0", "test-set-1"}}
					switch last {
					case "ignored":
						shape.ignored = map[string][]string{"test-set-1": {}}
					case "empty":
						shape.empty = "test-set-1"
					}
					shape.apply(h)
					h.replayer.config.Command = "docker compose up"
					h.replayer.config.CommandType = string(utils.DockerCompose)
					h.replayer.config.Test.Mocking = mocking
					h.replayer.config.Test.MaxFlakyChecks = 1
					h.replayer.config.DisableANSI = true
					agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
					t.Cleanup(agent.Close)
					h.replayer.config.Agent.AgentURI = agent.URL
					if mocking {
						h.replayer.instrumentation = &composeInstr{prInstr: h.instr}
					}
					hooks := &reportsAtAfterRunHooks{reports: reports}
					h.replayer.hookImpl = hooks

					_, statuses := startEarlierSetRun(t, h, reports)
					hooks.mu.Lock()
					calls, missing := hooks.calls, slices.Clone(hooks.missing)
					hooks.mu.Unlock()
					if calls != 1 || len(missing) != 0 {
						t.Fatalf("AfterTestRun called %d time(s), the sets with no final report at it: [%s] (statuses after the run %v); want one call, every set's report written", calls, strings.Join(missing, ","), statuses)
					}
				})
			}
		}
	}
}

// keploy test -t test-set-0 over a recording of test-set-0 and test-set-1:
// the last set that runs is looked for among the sets the run selected, not
// every recorded one. test-set-0's last test case alone carries
// KEPLOY-LAST-TESTCASE, and a reused docker compose stack's app container
// stops once, in test-set-0, before its report, in both modes; test-set-1
// writes no report. Looked for among every recorded set, test-set-1 would be
// the last that runs, and no set the run runs would do either.
func TestARunOfSelectedSetsEndsAtTheLastSelectedSetThatRuns(t *testing.T) {
	for _, on := range []bool{false, true} {
		for _, compose := range []bool{false, true} {
			name := "native"
			if compose {
				name = "a reused docker compose stack"
			}
			if on {
				name += "/switch on"
			}
			t.Run(name, func(t *testing.T) {
				provenOn(t, on)
				h := newPartialRunHarness(t, 2, 0)
				reports := &setsReportDB{prReportDB: h.report}
				h.replayer.reportDB = reports
				lastSetShape{ids: []string{"test-set-0", "test-set-1"}}.apply(h)
				h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": {}}
				h.replayer.config.Command = "./app"
				h.replayer.config.CommandType = string(utils.Native)
				h.replayer.config.Test.MaxFlakyChecks = 1
				h.replayer.config.DisableANSI = true
				var c *composeStopsInstr
				if compose {
					h.replayer.config.Command = "docker compose up"
					h.replayer.config.CommandType = string(utils.DockerCompose)
					h.replayer.config.Test.Mocking = true
					agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
					t.Cleanup(agent.Close)
					h.replayer.config.Agent.AgentURI = agent.URL
					c = &composeStopsInstr{composeInstr: &composeInstr{prInstr: h.instr}, reports: reports}
					h.replayer.instrumentation = c
				}
				hooks := &lastCaseHooks{}
				h.replayer.hookImpl = hooks

				_, statuses := startEarlierSetRun(t, h, reports)
				if last, _ := hooks.seen(); !slices.Equal(last, []string{"test-set-0/test-2"}) {
					t.Fatalf("the test cases sent with KEPLOY-LAST-TESTCASE: %v, want [test-set-0/test-2]: the last test case of the last selected set that runs", last)
				}
				if _, ran := statuses["test-set-1"]; ran || statuses["test-set-0"] != string(models.TestSetStatusPassed) {
					t.Fatalf("statuses %v; want test-set-0 PASSED and no report of test-set-1, which the run did not select", statuses)
				}
				if c == nil {
					return
				}
				c.smu.Lock()
				stoppedIn, reportedAtStop := slices.Clone(c.stoppedIn), c.reportedAtStop
				c.smu.Unlock()
				if !slices.Equal(stoppedIn, []string{"test-set-0"}) || reportedAtStop {
					t.Fatalf("the app's container was stopped in the sessions %v, test-set-0's report written first: %v; want it stopped once, in test-set-0, before its report", stoppedIn, reportedAtStop)
				}
			})
		}
	}
}
