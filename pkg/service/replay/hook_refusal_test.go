package replay

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/k0kubun/pp/v3"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	httpclient "go.keploy.io/server/v3/pkg/platform/http"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A replay hook that refuses a mock (MockPayloadMutator: Keploy Enterprise
// refuses a sealed value its key service did not open) fails the mock's test
// set, unrun, with the hook's reason, and the run goes on to its next set.
// It stopped the whole run at that set, and the sets after it never ran.

var threeSets = []string{"test-set-0", "test-set-1", "test-set-2"}

// namedPRSetsDB is the harness's test DB with the named sets, each holding
// the same tests.
type namedPRSetsDB struct {
	*prTestDB
	sets []string
}

func (d namedPRSetsDB) GetAllTestSetIDs(context.Context) ([]string, error) { return d.sets, nil }

// reportsBySet keeps each set's last report.
type reportsBySet struct {
	*prReportDB
	mu    sync.Mutex
	bySet map[string]*models.TestReport
}

func (r *reportsBySet) InsertReport(ctx context.Context, runID, setID string, rep *models.TestReport) error {
	r.mu.Lock()
	r.bySet[setID] = rep
	r.mu.Unlock()
	return r.prReportDB.InsertReport(ctx, runID, setID, rep)
}

func (r *reportsBySet) of(setID string) *models.TestReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bySet[setID]
}

// setMockURL is the URL of the one mock each set holds: the set's own.
func setMockURL(setID string) string { return "http://vault.internal/v1/" + setID }

// perSetMockDB holds one session mock per set, held whole (not left on
// disk), so the hook is called as the set's mocks are made ready for the
// agent.
type perSetMockDB struct{ *prMockDB }

func (perSetMockDB) GetUnFilteredMocks(_ context.Context, setID string, _, _ time.Time, _, _ map[string]bool) ([]*models.Mock, error) {
	m := sessionHTTPMock(setMockURL(setID))
	m.Name = "mock-0"
	return []*models.Mock{m}, nil
}

// refusingHooks refuses, with its error, each mock whose URL it names.
type refusingHooks struct {
	prHooks
	refuse map[string]error
	// onRefuse, when set, runs as a mock is refused.
	onRefuse func()
}

func (h refusingHooks) MutateMockPayload(_ context.Context, m *models.Mock) error {
	if m.Spec.HTTPReq == nil {
		return nil
	}
	err := h.refuse[m.Spec.HTTPReq.URL]
	if err != nil && h.onRefuse != nil {
		h.onRefuse()
	}
	return err
}

// errSealed is a refusal as Keploy Enterprise words one: the place, the key
// and the key service's reason.
var errSealed = errors.New("response body.token: sealed under key k-7, which the key service did not open: permission denied")

// startSets runs Start over h's tests as sets, as keploy test does with a
// native command, and returns the sets it ran, the reasons of its "stopping
// Keploy" lines and the code the process exits with. arm is handed the
// run's cancel, which stops it as a Ctrl+C does.
func startSets(t *testing.T, h *prRun, sets []string, arm func(cancel context.CancelFunc)) ([]string, []string, int) {
	t.Helper()
	ran, reasons, code, _ := startSetsLogged(t, h, sets, arm)
	return ran, reasons, code
}

// startSetsLogged is startSets, and returns the run's log lines too.
func startSetsLogged(t *testing.T, h *prRun, sets []string, arm func(cancel context.CancelFunc)) ([]string, []string, int, *observer.ObservedLogs) {
	t.Helper()
	db, again := h.replayer.testDB.(namedPRSetsDB) // a later Start of the same replayer
	if !again {
		db = namedPRSetsDB{prTestDB: h.replayer.testDB.(*prTestDB)}
	}
	db.sets = sets
	h.replayer.testDB = db
	if h.replayer.config.CommandType != string(utils.DockerCompose) {
		h.replayer.config.Command = "./app"
		h.replayer.config.CommandType = string(utils.Native)
	}
	h.replayer.config.Test.MaxFlakyChecks = 1
	core, logs := observer.New(zapcore.DebugLevel)
	h.replayer.logger = zap.New(core)
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	if arm != nil {
		arm(cancel)
	}
	_ = h.replayer.Start(ctx)
	var reasons []string
	for _, e := range logs.FilterMessage("stopping Keploy").All() {
		reason, _ := e.ContextMap()["reason"].(string)
		reasons = append(reasons, reason)
	}
	return setsRan(logs), reasons, utils.ErrCode, logs
}

// mocksPerSet makes h's mock store hold one session mock per set: held whole
// in memory, or left on disk in each set's mocks.yaml, read back as it is
// sent to the agent.
func mocksPerSet(t *testing.T, h *prRun, onDisk bool) {
	t.Helper()
	if !onDisk {
		h.replayer.mockDB = perSetMockDB{h.mocks}
		return
	}
	t.Setenv("KEPLOY_MOCK_FORMAT", "") // gob would write, and read, a mocks.gob instead
	for _, set := range threeSets {
		// A store of its own for each set, so that each set's one mock is
		// named mock-0, as a recording names it.
		w := mockdb.New(zap.NewNop(), h.replayer.config.Path, "mocks")
		if err := w.InsertMock(context.Background(), sessionHTTPMock(setMockURL(set)), set); err != nil {
			t.Fatalf("InsertMock: %v", err)
		}
	}
	h.replayer.mockDB = mockdb.New(zap.NewNop(), h.replayer.config.Path, "mocks")
}

// useCompose has h replay as keploy test does a docker compose command: a
// stack for each set, with keploy's agent in it, which holds the mocks keploy
// stores on it (an agent that holds none was replaced during the bring-up,
// and keploy fails the set).
func useCompose(t *testing.T, h *prRun) {
	t.Helper()
	h.replayer.config.Command = "docker compose up"
	h.replayer.config.CommandType = string(utils.DockerCompose)
	router := chi.NewRouter()
	routes.DefaultRoutes{}.New(router, &composeAgent{}, zap.NewNop())
	agent := httptest.NewServer(router)
	t.Cleanup(agent.Close)
	h.replayer.config.Agent.AgentURI = agent.URL + "/agent"
	h.replayer.instrumentation = &composeAgentInstr{prInstr: h.instr, client: httpclient.New(zap.NewNop(), nil, h.replayer.config)}
}

func TestARefusedMockFailsItsSetAloneAndTheRunGoesOn(t *testing.T) {
	for _, c := range []struct {
		name    string
		onDisk  bool
		compose bool
		refused []string
	}{
		{"a mock held whole", false, false, []string{"test-set-1"}},
		{"a mock left on disk, refused as it is sent", true, false, []string{"test-set-1"}},
		{"docker compose", false, true, []string{"test-set-1"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 3, 0)
			mocksPerSet(t, h, c.onDisk)
			if c.compose {
				useCompose(t, h)
			}
			refuse := map[string]error{}
			for _, set := range c.refused {
				refuse[setMockURL(set)] = errSealed
			}
			h.replayer.hookImpl = refusingHooks{refuse: refuse}
			reports := &reportsBySet{prReportDB: h.report, bySet: map[string]*models.TestReport{}}
			h.replayer.reportDB = reports

			ran, reasons, code, logs := startSetsLogged(t, h, threeSets, nil)

			if strings.Join(ran, ",") != strings.Join(threeSets, ",") {
				t.Fatalf("the run ran the sets %v and stopped with %q, want all of %v: a refused mock fails its set alone", ran, reasons, threeSets)
			}
			assertRefusalSaid(t, logs, c.refused)
			if len(reasons) != 1 || reasons[0] != runCompletedReason {
				t.Errorf("the run stops with the reasons %q, want one: %q", reasons, runCompletedReason)
			}
			if code != 1 {
				t.Errorf("exit code %d, want 1: a set failed", code)
			}
			for _, set := range threeSets {
				if rep := reports.of(set); !slices.Contains(c.refused, set) && (rep == nil || rep.Status != string(models.TestSetStatusPassed)) {
					t.Errorf("%s's report is %+v, want PASSED", set, rep)
				}
			}
			for _, set := range c.refused {
				assertRefusedReport(t, reports.of(set), set)
			}
			summary := h.replayer.refusedSetsSummary()
			for _, set := range threeSets {
				if listed := strings.Contains(summary, set+": "+"keploy refused the test set"); listed != slices.Contains(c.refused, set) {
					t.Errorf("the run's summary %q lists %s: %v, want %v", summary, set, listed, !listed)
				}
			}
			if !strings.Contains(summary, errSealed.Error()) {
				t.Errorf("the run's summary %q does not give the hook's reason", summary)
			}
		})
	}
}

// wantSetRefused is the ERROR line keploy logs as it refuses a set.
const wantSetRefused = "keploy test refused this test set, and ran none of its test cases: " +
	"a replay hook refused one of its mocks; the run goes on to the next test set"

// assertRefusalSaid asserts that the run logged one ERROR line for each set
// in refused, as it refused it, naming the set, its mock mock-0 and the
// hook's reason, and none for any other set.
func assertRefusalSaid(t *testing.T, logs *observer.ObservedLogs, refused []string) {
	t.Helper()
	said := logs.FilterMessageSnippet("refused this test set").All()
	if len(said) != len(refused) {
		t.Fatalf("%d lines say a set was refused, want %d (%v); the log has %v", len(said), len(refused), refused, messages(logs))
	}
	for i, e := range said {
		f := e.ContextMap()
		if e.Level != zapcore.ErrorLevel || e.Message != wantSetRefused {
			t.Errorf("the refusal is the %s %q, want the ERROR %q", e.Level, e.Message, wantSetRefused)
		}
		if f["testSet"] != refused[i] || f["mock"] != "mock-0" || f["error"] != errSealed.Error() {
			t.Errorf("the refusal names the set %v, the mock %v and the error %v; want %s, mock-0 and %q", f["testSet"], f["mock"], f["error"], refused[i], errSealed.Error())
		}
	}
}

// assertRefusedReport asserts that rep is the report of a set refused for
// errSealed: FAILED, naming the mock and the hook's reason, with each of its
// three tests failed, unsent, refused.
func assertRefusedReport(t *testing.T, rep *models.TestReport, set string) {
	t.Helper()
	if rep == nil {
		t.Fatalf("%s has no report", set)
	}
	if rep.Status != string(models.TestSetStatusFailed) {
		t.Errorf("%s's report is %s, want FAILED", set, rep.Status)
	}
	for _, want := range []string{errSealed.Error(), `mock "mock-0"`} {
		if !strings.Contains(rep.FailureReason, want) {
			t.Errorf("%s's failure reason %q does not name %q", set, rep.FailureReason, want)
		}
	}
	if rep.Total != 3 || rep.Failure != 3 || len(rep.Tests) != 3 {
		t.Fatalf("%s's report counts total %d, failed %d, rows %d; want 3 of each: every test of it fails, unsent", set, rep.Total, rep.Failure, len(rep.Tests))
	}
	for _, row := range rep.Tests {
		if row.Status != models.TestStatusFailed || !refusedUnsent(row) {
			t.Errorf("%s's row %s is %s, refused unsent %v; want FAILED and refused unsent", set, row.TestCaseID, row.Status, refusedUnsent(row))
		}
		if len(row.TemplateFailures) != 1 || row.TemplateFailures[0].Where != "mock mock-0" || !strings.Contains(row.TemplateFailures[0].Why, errSealed.Error()) {
			t.Errorf("%s's row %s gives the refusal %+v, want it at mock mock-0, with the hook's reason", set, row.TestCaseID, row.TemplateFailures)
		}
		if !hasCategory(row, models.TestSetRefused) {
			t.Errorf("%s's row %s has the categories %v, want %s", set, row.TestCaseID, row.FailureInfo.Category, models.TestSetRefused)
		}
	}
}

// A hook that fails because the run was stopped refused nothing: the run
// stops, as a Ctrl+C stops it, and no set is reported refused.
func TestAHookThatFailsAsTheRunStopsRefusesNoSet(t *testing.T) {
	h := newPartialRunHarness(t, 3, 0)
	mocksPerSet(t, h, false)
	reports := &reportsBySet{prReportDB: h.report, bySet: map[string]*models.TestReport{}}
	h.replayer.reportDB = reports
	ran, _, _ := startSets(t, h, threeSets, func(cancel context.CancelFunc) {
		h.replayer.hookImpl = refusingHooks{refuse: map[string]error{setMockURL("test-set-1"): context.Canceled}, onRefuse: cancel}
	})
	if strings.Join(ran, ",") != "test-set-0,test-set-1" {
		t.Errorf("the run ran the sets %v, want it to stop at test-set-1", ran)
	}
	if rep := reports.of("test-set-1"); rep != nil && rep.Status == string(models.TestSetStatusFailed) && strings.Contains(rep.FailureReason, "refused") {
		t.Errorf("test-set-1 is reported refused (%q), want no refusal for a stopped run", rep.FailureReason)
	}
	if s := h.replayer.refusedSetsSummary(); s != "" {
		t.Errorf("the run's summary lists refused sets for a stopped run: %q", s)
	}
}

// caseRefusingHooks fails BeforeTestCaseRun for the test cases it names, and
// its SimulateRequest (prHooks.simErr) refuses them, as Keploy Enterprise's
// does for a test case whose sealed values it did not open.
type caseRefusingHooks struct {
	prHooks
	refuse map[string]error
}

func (h caseRefusingHooks) BeforeTestCaseRun(_ context.Context, tc *models.TestCase, _ string) error {
	return h.refuse[tc.Name]
}

// wantBeforeTestCaseRunFailed is the line a failed BeforeTestCaseRun logs,
// written out: a row that read it from beforeTestCaseRunFailed would pass
// whatever the line said.
const wantBeforeTestCaseRunFailed = "the BeforeTestCaseRun hook failed on this test case (the error says why): " +
	"the test case goes to the SimulateRequest hook as the failed hook left it, and is sent only if that hook allows it; " +
	"if it does not, the test case fails with that hook's error and the test set goes on"

// A BeforeTestCaseRun that fails says what comes next: the test case goes to
// the SimulateRequest hook, which sends it only if it allows it, and the
// failing hook's error is named. A refused test case fails, and the set goes
// on. The line said the replay "continues with test case in current state",
// and the test case then failed. Both places the hook runs: the first phase,
// and the streaming one after it.
func TestABeforeTestCaseRunFailureSaysWhatHappensNext(t *testing.T) {
	for _, c := range []struct {
		name      string
		streaming bool
	}{
		{"the first phase", false},
		{"the streaming phase", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 3, 0)
			if c.streaming {
				h.streaming("test-2")
			}
			refusal := errors.New("refused: " + errSealed.Error())
			h.replayer.hookImpl = caseRefusingHooks{
				prHooks: prHooks{simErr: map[string]error{"test-2": refusal}},
				refuse:  map[string]error{"test-2": errSealed},
			}
			core, logs := observer.New(zapcore.DebugLevel)
			h.replayer.logger = zap.New(core)

			if status := h.run(t); status != models.TestSetStatusFailed {
				t.Errorf("the set ended %s, want FAILED", status)
			}
			said := logs.FilterMessageSnippet("BeforeTestCaseRun hook failed").All()
			if len(said) != 1 {
				t.Fatalf("%d lines say the BeforeTestCaseRun hook failed, want 1; the log has %v", len(said), messages(logs))
			}
			if said[0].Level != zapcore.ErrorLevel || said[0].Message != wantBeforeTestCaseRunFailed {
				t.Errorf("the line is the %s %q, want the ERROR %q: it says what happens to the test case next", said[0].Level, said[0].Message, wantBeforeTestCaseRunFailed)
			}
			fields := said[0].ContextMap()
			if fields["testcase"] != "test-2" {
				t.Errorf("the line names the test case %v, want test-2", fields["testcase"])
			}
			if got, _ := fields["error"].(string); got != errSealed.Error() {
				t.Errorf("the line gives the hook's error %q, want %q", got, errSealed.Error())
			}
			if n := len(logs.FilterMessageSnippet("continues with test case in current state").All()); n != 0 {
				t.Errorf("%d lines still say the replay continues with the test case in its current state", n)
			}
			got := map[string]models.TestStatus{}
			for _, res := range h.report.results {
				got[res.TestCaseID] = res.Status
			}
			want := map[string]models.TestStatus{"test-1": models.TestStatusPassed, "test-2": models.TestStatusFailed, "test-3": models.TestStatusPassed}
			for name, status := range want {
				if got[name] != status {
					t.Errorf("%s is %q, want %q: the refused test case fails and the set goes on (results %v)", name, got[name], status, got)
				}
			}
		})
	}
}

// A refused set fails only the test cases the run would have sent: those it
// selects in the set, less those its ignore list skips, after the selected
// test cases not on disk. Each set refused is in the run's summary.
func TestARefusedSetFailsTheTestCasesTheRunWouldSend(t *testing.T) {
	h := newPartialRunHarness(t, 3, 0)
	mocksPerSet(t, h, false)
	h.replayer.hookImpl = refusingHooks{refuse: map[string]error{setMockURL("test-set-0"): errSealed, setMockURL("test-set-1"): errSealed}}
	// A run that selects test cases runs only the sets it names: an empty
	// list runs all of a set.
	h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": {}, "test-set-1": {"test-1", "test-3", "test-9"}, "test-set-2": {}}
	h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-1": {"test-3"}}
	reports := &reportsBySet{prReportDB: h.report, bySet: map[string]*models.TestReport{}}
	h.replayer.reportDB = reports

	ran, _, code := startSets(t, h, threeSets, nil)

	if len(ran) != 3 || code != 1 {
		t.Fatalf("the run ran the sets %v and exits %d, want all three and 1", ran, code)
	}
	rep := reports.of("test-set-1")
	if rep == nil || rep.Total != 2 || len(rep.Tests) != 2 || rep.Tests[0].TestCaseID != "test-9" || !notFoundUnsent(rep.Tests[0]) || rep.Tests[1].TestCaseID != "test-1" || !refusedUnsent(rep.Tests[1]) {
		t.Fatalf("test-set-1's report is %+v, want two rows: test-9's, not on disk, then test-1's, refused; test-2 is not selected, and test-3 is ignored", rep)
	}
	if rep := reports.of("test-set-0"); rep == nil || rep.Total != 3 {
		t.Errorf("test-set-0's report is %+v, want 3 rows: it selects and ignores nothing", rep)
	}
	summary := h.replayer.refusedSetsSummary()
	for _, set := range []string{"test-set-0", "test-set-1"} {
		if !strings.Contains(summary, set+": ") {
			t.Errorf("the run's summary %q does not list the refused %s", summary, set)
		}
	}
}

// A refused set whose rows cannot be written stops the run, as a report
// store that cannot be written stops it anywhere.
func TestARefusedSetWhoseRowsCannotBeWrittenStopsTheRun(t *testing.T) {
	h := newPartialRunHarness(t, 3, 0)
	mocksPerSet(t, h, false)
	h.replayer.hookImpl = refusingHooks{refuse: map[string]error{setMockURL("test-set-0"): errSealed}}
	h.report.failInsertOf = map[string]error{"test-2": errors.New("report store is unwritable")}

	ran, reasons, _ := startSets(t, h, threeSets, nil)

	if strings.Join(ran, ",") != "test-set-0" {
		t.Errorf("the run ran the sets %v, want it to stop at test-set-0", ran)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "report store is unwritable") {
		t.Errorf("the run stops with the reasons %q, want the store's error", reasons)
	}
}

// The run's printed summary, with ANSI and without, lists each set the run
// refused, with the hook's reason, under its own heading. A later Start of
// the same replayer that refuses nothing lists no refused set: each run's
// summary names only the sets that run refused.
func TestTheRunSummaryListsTheSetsThisRunRefused(t *testing.T) {
	const heading = "\tTest sets refused, none of their test cases sent: 1\n"
	line := "\t\ttest-set-1: keploy refused the test set and sent none of its test cases: the mock payload hook failed on mock \"mock-0\": " + errSealed.Error() + "\n"
	for _, noANSI := range []bool{false, true} {
		t.Run(fmt.Sprintf("DisableANSI=%v", noANSI), func(t *testing.T) {
			h := newPartialRunHarness(t, 3, 0)
			mocksPerSet(t, h, false)
			h.replayer.config.DisableANSI = noANSI
			h.replayer.hookImpl = refusingHooks{refuse: map[string]error{setMockURL("test-set-1"): errSealed}}

			ran, code, out := startSetsPrinted(t, h, threeSets)
			if len(ran) != 3 || code != 1 {
				t.Fatalf("the run ran the sets %v and exits %d, want all three and 1", ran, code)
			}
			if !strings.Contains(out, heading) || !strings.Contains(out, line) {
				t.Fatalf("the printed summary does not list test-set-1 under %q with the hook's reason (%q):\n%s", heading, line, out)
			}
			for _, set := range []string{"test-set-0", "test-set-2"} {
				if strings.Contains(out, "\t\t"+set+": ") {
					t.Errorf("the printed summary lists %s, which passed, as refused:\n%s", set, out)
				}
			}

			h.replayer.hookImpl = refusingHooks{}
			ran, code, out = startSetsPrinted(t, h, threeSets)
			if len(ran) != 3 || code != 0 {
				t.Fatalf("the second run ran the sets %v and exits %d, want all three and 0", ran, code)
			}
			if !strings.Contains(out, "COMPLETE TESTRUN SUMMARY.") {
				t.Fatalf("the second run printed no summary:\n%s", out)
			}
			if strings.Contains(out, "Test sets refused") || strings.Contains(out, "test-set-1: ") {
				t.Errorf("the second run, which refused nothing, lists the first run's refused set:\n%s", out)
			}
		})
	}
}

// startSetsPrinted is startSets over sets, and returns what the run
// printed, its summary as the user reads it: pp's output too, which the ANSI
// summary is printed with.
func startSetsPrinted(t *testing.T, h *prRun, sets []string) (ran []string, code int, out string) {
	t.Helper()
	out = captureStdout(t, func() {
		prev := pp.GetDefaultOutput()
		pp.SetDefaultOutput(os.Stdout)
		defer pp.SetDefaultOutput(prev)
		ran, _, code = startSets(t, h, sets, nil)
	})
	return ran, code, out
}

// A run that counts no test case prints no table, but a set it refused is
// still in its summary, with the hook's reason: here the run's only set,
// each of whose test cases it ignores. A run that counts no test case and
// refuses no set (its one set holds none) prints no summary, as before.
func TestARunThatCountsNoTestCaseStillListsTheSetItRefused(t *testing.T) {
	want := "\n <=========================================> \n  COMPLETE TESTRUN SUMMARY. \n\tTotal tests: 0" +
		"\n\n\tTest sets refused, none of their test cases sent: 1\n" +
		"\t\ttest-set-0: keploy refused the test set and sent none of its test cases: the mock payload hook failed on mock \"mock-0\": " + errSealed.Error() + "\n" +
		"\n<=========================================> \n\n"
	for _, noANSI := range []bool{false, true} {
		t.Run(fmt.Sprintf("DisableANSI=%v", noANSI), func(t *testing.T) {
			h := newPartialRunHarness(t, 3, 0)
			mocksPerSet(t, h, false)
			h.replayer.config.DisableANSI = noANSI
			h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {"test-1", "test-2", "test-3"}}
			h.replayer.hookImpl = refusingHooks{refuse: map[string]error{setMockURL("test-set-0"): errSealed}}

			ran, code, out := startSetsPrinted(t, h, []string{"test-set-0"})
			if len(ran) != 1 || code != 1 {
				t.Fatalf("the run ran the sets %v and exits %d, want test-set-0 and 1", ran, code)
			}
			if !strings.Contains(out, want) {
				t.Fatalf("the printed summary is not %q; the run printed:\n%s", want, out)
			}

			// An ignored test case is counted, so the second run's set holds
			// none.
			h.replayer.hookImpl = refusingHooks{}
			h.replayer.testDB.(namedPRSetsDB).cases = nil
			_, _, out = startSetsPrinted(t, h, []string{"test-set-0"})
			if strings.Contains(out, "COMPLETE TESTRUN SUMMARY.") {
				t.Errorf("the second run counted no test case and refused no set, and printed a summary:\n%s", out)
			}
		})
	}
}

// A refused set is FAILED, and RunTestSet returns no error, so the run goes
// on to its next set. FAILED is what fails the run for it: a refused set
// whose every test case the run ignores has no test case to fail, and the
// run still exits 1.
func TestARefusedSetIsFailedWithNoError(t *testing.T) {
	for _, compose := range []bool{false, true} {
		t.Run(fmt.Sprintf("RunTestSet, docker compose %v", compose), func(t *testing.T) {
			h := newPartialRunHarness(t, 3, 0)
			mocksPerSet(t, h, false)
			h.replayer.config.Command = "./app"
			h.replayer.config.CommandType = string(utils.Native)
			if compose {
				useCompose(t, h)
			}
			h.replayer.hookImpl = refusingHooks{refuse: map[string]error{setMockURL("test-set-1"): errSealed}}
			reports := &reportsBySet{prReportDB: h.report, bySet: map[string]*models.TestReport{}}
			h.replayer.reportDB = reports
			h.set = "test-set-1"
			// run fails the row if RunTestSet returns an error.
			if status := h.run(t); status != models.TestSetStatusFailed {
				t.Errorf("RunTestSet returned %s for the refused set, want FAILED", status)
			}
			assertRefusedReport(t, reports.of("test-set-1"), "test-set-1")
		})
	}
	t.Run("every test case of it ignored", func(t *testing.T) {
		h := newPartialRunHarness(t, 3, 0)
		mocksPerSet(t, h, false)
		h.replayer.hookImpl = refusingHooks{refuse: map[string]error{setMockURL("test-set-1"): errSealed}}
		h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-1": {"test-1", "test-2", "test-3"}}
		reports := &reportsBySet{prReportDB: h.report, bySet: map[string]*models.TestReport{}}
		h.replayer.reportDB = reports

		ran, reasons, code := startSets(t, h, threeSets, nil)

		if len(ran) != 3 {
			t.Fatalf("the run ran the sets %v and stopped with %q, want all three", ran, reasons)
		}
		rep := reports.of("test-set-1")
		if rep == nil || rep.Status != string(models.TestSetStatusFailed) || rep.Total != 0 || len(rep.Tests) != 0 || !strings.Contains(rep.FailureReason, errSealed.Error()) {
			t.Fatalf("test-set-1's report is %+v, want FAILED with the hook's reason and no row: the run ignores each of its test cases", rep)
		}
		if code != 1 {
			t.Errorf("exit code %d, want 1: the refused set failed, though no test case of it did", code)
		}
	})
}

// Under --keep-app-alive (the proven switch off) the first test set to reach
// the wait for the application waits for it, and the sets after it skip the
// wait. A set refused before it reached the wait does not count: the set
// after it waits. The second set skipped the wait, as the set at index 1, and
// fired at an application nobody had waited for.
func TestTheFirstSetToReachTheKeptAppWaitsForIt(t *testing.T) {
	for _, c := range []struct {
		name    string
		compose bool
		refused []string
		// waited is that an earlier Start of the replayer waited for its
		// kept application.
		waited bool
		// ran is the sets the run runs, skipped those of them that skip the
		// wait.
		ran, skipped []string
	}{
		{"no set refused", false, nil, false, threeSets, []string{"test-set-1", "test-set-2"}},
		{"the first set refused", false, []string{"test-set-0"}, false, threeSets, []string{"test-set-2"}},
		{"the first two sets refused", false, []string{"test-set-0", "test-set-1"}, false, threeSets, nil},
		{"after a Start that waited", false, nil, true, threeSets, []string{"test-set-1", "test-set-2"}},
		{"docker compose, the first set refused", true, []string{"test-set-0"}, false, threeSets, []string{"test-set-2"}},
		{"docker compose, no set refused", true, nil, false, threeSets, []string{"test-set-1", "test-set-2"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", "")
			h := newPartialRunHarness(t, 3, 0)
			mocksPerSet(t, h, false)
			if c.compose {
				useCompose(t, h)
			}
			h.replayer.config.Test.KeepAppAlive = true
			refuse := map[string]error{}
			for _, set := range c.refused {
				refuse[setMockURL(set)] = errSealed
			}
			h.replayer.hookImpl = refusingHooks{refuse: refuse}
			h.replayer.keptAppWaited = c.waited

			ran, reasons, _, logs := startSetsLogged(t, h, threeSets, nil)

			if strings.Join(ran, ",") != strings.Join(c.ran, ",") {
				t.Fatalf("the run ran the sets %v and stopped with %q, want %v", ran, reasons, c.ran)
			}
			if got := setsSkippingTheAppWait(logs); strings.Join(got, ",") != strings.Join(c.skipped, ",") {
				t.Errorf("the sets %v skip the wait for the kept application, want %v: the first set to reach it waits", got, c.skipped)
			}
		})
	}
}

// setsSkippingTheAppWait is the sets that skipped the wait for the
// application --keep-app-alive keeps, each once, in the order they ran.
func setsSkippingTheAppWait(logs *observer.ObservedLogs) []string {
	var set string
	var skipped []string
	for _, e := range logs.All() {
		switch {
		case e.Message == "running":
			if s, ok := e.ContextMap()["test-set"].(string); ok {
				set = sgrCode.ReplaceAllString(s, "")
			}
		case strings.Contains(e.Message, "skipping waitForAppReady") && !slices.Contains(skipped, set):
			skipped = append(skipped, set)
		}
	}
	return skipped
}

func messages(logs *observer.ObservedLogs) []string {
	var out []string
	for _, e := range logs.All() {
		out = append(out, e.Message)
	}
	return out
}
