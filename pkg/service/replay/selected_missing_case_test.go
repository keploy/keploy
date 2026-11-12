package replay

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A run selects test cases by name (keploy.yml's test.selectedTests, or the
// enterprise binary's -t set:case). The set's Total counts every name
// selected, and each name selected must get a verdict: one no test case on
// disk carries used to get no row at all, and the set passed (Total 3, two
// rows, PASSED).

// selectedRun runs test-set-0, whose test cases on disk are test-1 to
// test-n, with the named test cases selected.
func selectedRun(t *testing.T, n int, selected ...string) *prRun {
	t.Helper()
	h := newPartialRunHarness(t, n, 0)
	h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": selected}
	return h
}

// rowsOf is each test's last row, by name.
func rowsOf(results []models.TestResult) map[string]models.TestResult {
	out := make(map[string]models.TestResult, len(results))
	for _, r := range results {
		out[r.TestCaseID] = r
	}
	return out
}

// addsUp fails the test unless the report's Total is the sum of its verdicts,
// and every test counted has a row.
func addsUp(t *testing.T, rep *models.TestReport) {
	t.Helper()
	if rep == nil {
		t.Fatal("no report was persisted")
	}
	if sum := rep.Success + rep.Failure + rep.Ignored + rep.Obsolete; sum != rep.Total {
		t.Fatalf("the report does not add up: Total %d, but passed %d + failed %d + ignored %d + obsolete %d = %d",
			rep.Total, rep.Success, rep.Failure, rep.Ignored, rep.Obsolete, sum)
	}
	if len(rowsOf(rep.Tests)) != rep.Total {
		t.Fatalf("the report counts %d tests in its Total but lists %d: %+v", rep.Total, len(rowsOf(rep.Tests)), rep.Tests)
	}
}

// notFound fails the test unless name's row is FAILED for not being on disk,
// and says so.
func notFound(t *testing.T, rows map[string]models.TestResult, name string) {
	t.Helper()
	row, ok := rows[name]
	if !ok {
		t.Fatalf("the selected test case %s, which is not on disk, has no row: %+v", name, rows)
	}
	if row.Status != models.TestStatusFailed {
		t.Fatalf("the selected test case %s, which is not on disk, is %s; want FAILED", name, row.Status)
	}
	want := "test case " + name + " not found in test-set-0"
	if row.FailureInfo.NotFound != want {
		t.Fatalf("the row of %s gives the reason %q; want %q", name, row.FailureInfo.NotFound, want)
	}
	if !hasCategory(row, models.TestCaseNotFound) {
		t.Fatalf("the row of %s is filed under %v; want %s", name, row.FailureInfo.Category, models.TestCaseNotFound)
	}
	if row.FailureInfo.Risk != models.High {
		t.Fatalf("the row of %s is persisted at risk %q; want %q (keploy report and JUnit grade it from it)", name, row.FailureInfo.Risk, models.High)
	}
}

// TestASelectedCaseNotOnDiskFailsTheSet: test-9 is selected beside two test
// cases on disk. The two run and pass; test-9 is FAILED, with its reason, and
// so is the set, whose Total adds up.
func TestASelectedCaseNotOnDiskFailsTheSet(t *testing.T) {
	// Each row sets the switch it runs under (the package's TestMain clears
	// KEPLOY_PROVEN): off, then on, where the counts come from the rows.
	for _, on := range []bool{false, true} {
		t.Run(switchName(on), func(t *testing.T) {
			setSwitch(t, on)
			h := selectedRun(t, 4, "test-1", "test-9", "test-2")
			status := h.run(t)

			if status != models.TestSetStatusFailed {
				t.Fatalf("a set with a selected test case not on disk reported %q; want FAILED", status)
			}
			rep := h.report.report
			addsUp(t, rep)
			if rep.Total != 3 || rep.Success != 2 || rep.Failure != 1 {
				t.Fatalf("Total %d, passed %d, failed %d; want 3, 2 and 1", rep.Total, rep.Success, rep.Failure)
			}
			if rep.Status != string(models.TestSetStatusFailed) {
				t.Fatalf("the report's status is %s; want FAILED", rep.Status)
			}
			if !strings.Contains(rep.FailureReason, "test case test-9 not found in test-set-0") {
				t.Fatalf("the set's failure reason %q does not name the test case not on disk", rep.FailureReason)
			}
			if rep.HighRisk != 1 {
				t.Fatalf("the set counts %d high-risk failures; want 1, the test case not on disk", rep.HighRisk)
			}
			rows := rowsOf(rep.Tests)
			notFound(t, rows, "test-9")
			for _, name := range []string{"test-1", "test-2"} {
				if rows[name].Status != models.TestStatusPassed {
					t.Fatalf("%s, selected and on disk, is %s; want PASSED", name, rows[name].Status)
				}
			}
			if _, ran := rows["test-3"]; ran {
				t.Fatal("test-3 was not selected, yet it has a row")
			}
		})
	}
}

// TestNoSelectedCaseOnDiskFailsTheSetUnrun: no name selected is on disk. The
// set fails with a row for each, and starts no application: there is nothing
// to send it.
func TestNoSelectedCaseOnDiskFailsTheSetUnrun(t *testing.T) {
	// Each row sets the switch it runs under (the package's TestMain clears
	// KEPLOY_PROVEN): off, then on, where the counts come from the rows.
	for _, on := range []bool{false, true} {
		t.Run(switchName(on), func(t *testing.T) {
			setSwitch(t, on)
			h := selectedRun(t, 2, "test-8", "test-9", "test-8")
			status := h.run(t)

			if status != models.TestSetStatusFailed {
				t.Fatalf("a set none of whose selected test cases is on disk reported %q; want FAILED", status)
			}
			rep := h.report.report
			addsUp(t, rep)
			if rep.Total != 2 || rep.Failure != 2 {
				t.Fatalf("Total %d, failed %d; want 2 and 2 (test-8 is selected twice, and counted once)", rep.Total, rep.Failure)
			}
			if want := "test cases test-8, test-9 not found in test-set-0"; rep.FailureReason != want {
				t.Fatalf("the set's failure reason is %q; want %q", rep.FailureReason, want)
			}
			if rep.HighRisk != 2 {
				t.Fatalf("the set counts %d high-risk failures; want 2", rep.HighRisk)
			}
			rows := rowsOf(rep.Tests)
			notFound(t, rows, "test-8")
			notFound(t, rows, "test-9")
			if got := h.instr.runCalls(); got != 0 {
				t.Fatalf("the application was started %d time(s) for a set with no selected test case on disk; want none", got)
			}
			verdict := h.replayer.completeTestReport["test-set-0"]
			if verdict.status || verdict.total != 2 || verdict.failed != 2 {
				t.Fatalf("the run's summary has the set as %+v; want it failed, with 2 of 2 failing", verdict)
			}
		})
	}
}

// TestEverySelectedCaseOnDiskRunsAsBefore is the control: a selection whose
// every name is on disk runs and passes as it did, with no row for a test it
// did not select.
func TestEverySelectedCaseOnDiskRunsAsBefore(t *testing.T) {
	// Each row sets the switch it runs under (the package's TestMain clears
	// KEPLOY_PROVEN): off, then on, where the counts come from the rows.
	for _, on := range []bool{false, true} {
		t.Run(switchName(on), func(t *testing.T) {
			setSwitch(t, on)
			h := selectedRun(t, 4, "test-2", "test-4")
			status := h.run(t)

			if status != models.TestSetStatusPassed {
				t.Fatalf("a set whose every selected test case is on disk reported %q; want PASSED", status)
			}
			rep := h.report.report
			addsUp(t, rep)
			if rep.Total != 2 || rep.Success != 2 {
				t.Fatalf("Total %d, passed %d; want 2 and 2", rep.Total, rep.Success)
			}
			if rep.FailureReason != "" {
				t.Fatalf("a passing set gives the failure reason %q", rep.FailureReason)
			}
		})
	}
}

// TestASelectedCaseNotOnDiskThatCannotBeWrittenStopsTheSet: the row of a
// selected test case not on disk cannot be written. The set fails with the
// error, before its application starts, as a set whose rows cannot be written
// does: a run that went on would leave the name counted in Total with no row.
func TestASelectedCaseNotOnDiskThatCannotBeWrittenStopsTheSet(t *testing.T) {
	h := selectedRun(t, 2, "test-1", "test-9")
	h.report.failInsertOf = map[string]error{"test-9": errors.New("report store is unwritable")}
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	status, err := h.replayer.RunTestSet(ctx, "test-set-0", "test-run-0", false)
	if err == nil || status != models.TestSetStatusFailed {
		t.Fatalf("RunTestSet = %q, %v; want FAILED and the store's error", status, err)
	}
	if got := h.instr.runCalls(); got != 0 {
		t.Fatalf("the application was started %d time(s) after a row could not be written; want none", got)
	}
}

// TestMissingSelectedListsEachNameOnce: the names selected that are not on
// disk, each once, in the order selected, and how many distinct names were
// selected. A run that selects none misses none.
func TestMissingSelectedListsEachNameOnce(t *testing.T) {
	onDisk := []*models.TestCase{{Name: "test-1"}, {Name: "test-2"}}
	missing, selected := missingSelected([]string{"test-9", "test-1", "test-8", "test-9", "test-1"}, onDisk)
	if !slices.Equal(missing, []string{"test-9", "test-8"}) || selected != 3 {
		t.Fatalf("missingSelected = %v, %d; want [test-9 test-8], 3", missing, selected)
	}
	if missing, selected := missingSelected(nil, onDisk); len(missing) != 0 || selected != 0 {
		t.Fatalf("a run that selects no test case misses %v of %d; want none of 0", missing, selected)
	}
}

// TestASetsReasonNamesEverySelectedCaseNotOnDisk: the set's failure reason
// names each selected test case not on disk, and keeps every other reason
// it fails for beside it.
func TestASetsReasonNamesEverySelectedCaseNotOnDisk(t *testing.T) {
	if got, want := notFoundSetReason("test-set-0", []string{"test-9"}), "test case test-9 not found in test-set-0"; got != want {
		t.Fatalf("one name: %q; want %q", got, want)
	}
	if got, want := notFoundSetReason("test-set-0", []string{"test-8", "test-9"}), "test cases test-8, test-9 not found in test-set-0"; got != want {
		t.Fatalf("two names: %q; want %q", got, want)
	}
	notFoundWhy := "test case test-9 not found in test-set-0"
	if got := describeTestSetFailure(models.TestSetStatusFailed, nil, runShape{notFoundWhy: notFoundWhy}); got != notFoundWhy {
		t.Fatalf("the reason alone: %q; want %q", got, notFoundWhy)
	}
	got := describeTestSetFailure(models.TestSetStatusFailed, nil, runShape{notFoundWhy: notFoundWhy, realWhy: "real", unclaimedWhy: "unclaimed", staleWhy: "stale"})
	if want := notFoundWhy + "; real; unclaimed; stale"; got != want {
		t.Fatalf("beside the other reasons: %q; want %q", got, want)
	}
	if got := describeTestSetFailure(models.TestSetStatusFailed, nil, runShape{realWhy: "real"}); got != "real" {
		t.Fatalf("a set with every selected test case on disk: %q; want %q", got, "real")
	}
	// A set that also stopped early, failed or not, and one whose
	// application halted: the reason names the test case not on disk
	// beside what the status says of the run.
	ran := []models.TestResult{{TestCaseID: "test-1", Status: models.TestStatusPassed}}
	for _, c := range []struct {
		name   string
		status models.TestSetStatus
		shape  runShape
		want   string
	}{
		{"FAILED, stopped early", models.TestSetStatusFailed, runShape{stoppedEarly: true}, partialRunReason},
		{"a partial run's APP_FAULT", models.TestSetStatusFaultUserApp, runShape{stoppedEarly: true, downgradedFromPassed: true}, partialRunReason},
		{"APP_HALTED", models.TestSetStatusAppHalted, runShape{}, "application stopped during replay - check application logs in the app_logs field for details and next steps"},
	} {
		c.shape.notFoundWhy = notFoundWhy
		if got, want := describeTestSetFailure(c.status, ran, c.shape), notFoundWhy+"; "+c.want; got != want {
			t.Fatalf("%s: %q; want %q", c.name, got, want)
		}
	}
}

// TestASetIsFailedForItsSelectedCasesNotOnDiskAlone: a set FAILED only for
// the test cases it selected that are not on disk, and nothing else.
func TestASetIsFailedForItsSelectedCasesNotOnDiskAlone(t *testing.T) {
	for _, c := range []struct {
		name                  string
		status                models.TestSetStatus
		failure, notFound     int
		realWhy, unclaimedWhy string
		want                  bool
	}{
		{"only test cases not on disk failed", models.TestSetStatusFailed, 2, 2, "", "", true},
		{"a test case on disk failed too", models.TestSetStatusFailed, 3, 2, "", "", false},
		{"none selected is missing", models.TestSetStatusFailed, 0, 0, "", "", false},
		{"mock.real failed it too", models.TestSetStatusFailed, 1, 1, "real", "", false},
		{"a call no mock answered failed it too", models.TestSetStatusFailed, 1, 1, "", "unclaimed", false},
		{"an internal error", models.TestSetStatusInternalErr, 1, 1, "", "", false},
		{"its user stopped it", models.TestSetStatusUserAbort, 1, 1, "", "", false},
		{"its application halted", models.TestSetStatusAppHalted, 1, 1, "", "", false},
	} {
		if got := failedForNotFoundAlone(c.status, c.failure, c.notFound, c.realWhy, c.unclaimedWhy); got != c.want {
			t.Errorf("%s: %v; want %v", c.name, got, c.want)
		}
	}
}

// TestAPartialRunWithASelectedCaseNotOnDiskStaysAPartialRun: the agent
// refuses the third test, so the run stops early, and test-9, selected, is
// not on disk. The set is the partial run it is without test-9: APP_FAULT,
// with its application's logs, and its reason names test-9 beside why it
// stopped, with the switch off and on (where the counts come from the
// rows, tallyFromVerdicts). test-9 is counted failed, at HIGH risk, either
// way. When test-1 also fails, the set is FAILED, and its reason names
// test-9 beside why it stopped too.
//
// Each row sets the switch it runs under: the package's TestMain clears
// KEPLOY_PROVEN, so the environment the package runs in sets none.
func TestAPartialRunWithASelectedCaseNotOnDiskStaysAPartialRun(t *testing.T) {
	all := []string{"test-1", "test-2", "test-3", "test-4", "test-9"}
	for _, c := range []struct {
		name     string
		selected []string
		// proven runs it with KEPLOY_PROVEN=1 whatever the switch; nth is
		// the agent's ask it refuses (0: the third, the sixth when proven);
		// also fails the set some other way, or is nil.
		proven bool
		nth    int
		also   func(h *prRun)
		want   models.TestSetStatus
		// stopped is what the reason says of the stop.
		stopped  string
		notFound bool
		// failed and highRisk are the report's counts; its Total is every
		// name selected.
		failed, highRisk int
	}{
		{"every test on disk passed", all, false, 0, nil, models.TestSetStatusFaultUserApp, partialRunReason, true, 1, 1},
		{"every test on disk passed, switch on", all, true, 0, nil, models.TestSetStatusFaultUserApp, partialRunReason, true, 1, 1},
		{"test-1 failed", all, false, 0, func(h *prRun) {
			h.replayer.hookImpl = prHooks{wrongBody: map[string]bool{"test-1": true}}
		}, models.TestSetStatusFailed, partialRunReason, true, 2, 1},
		{"a mock.real destination was never reached", all, true, 0, func(h *prRun) {
			h.replayer.config.Mock.Real = []string{"db:5432"}
			h.cases[0].Real = []string{"db:5432"}
			h.replayer.instrumentation = &realInstr{prInstr: h.instr}
		}, models.TestSetStatusFailed, partialRunReason, true, 1, 1},
		{"a call after the last test no mock answered", all, true, 0, func(h *prRun) {
			h.instr.mockErrorsFor = misses(nil, nil, afterLast)
		}, models.TestSetStatusFailed, partialRunReason, true, 1, 1},
		// The pool test-3 is served over is refused: the set is not
		// judged, and stays so.
		{"not judged", all, true, 5, func(h *prRun) {
			h.instr.mockErrorsFor = misses()
		}, models.TestSetStatusInternalErr, "it also stopped before every test ran", true, 1, 1},
		{"control: every selected test case on disk", all[:4], false, 0, nil, models.TestSetStatusFaultUserApp, partialRunReason, false, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			setSwitch(t, c.proven)
			// The agent refuses its nth ask, so the run stops early. With
			// mock.real, or the calls no mock answered read, an odd ask is
			// the pool a test is served over, whose refusal leaves the set
			// not judged (INTERNAL_ERR) instead: those refuse the sixth.
			nth := c.nth
			switch {
			case nth != 0:
			case c.proven:
				nth = 6
			default:
				nth = 3
			}
			h := newPartialRunHarness(t, 4, nth)
			h.instr.recentAppLogs = "app: still serving\n"
			h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": c.selected}
			if c.also != nil {
				c.also(h)
			}
			status := h.run(t)
			rep := h.report.report
			if rep == nil {
				t.Fatal("no report was persisted")
			}
			if status != c.want || rep.Status != string(c.want) {
				t.Fatalf("the set is %s, its report %s; want %s (reason %q)", status, rep.Status, c.want, rep.FailureReason)
			}
			if !strings.Contains(rep.FailureReason, c.stopped) {
				t.Fatalf("the reason %q does not say %q", rep.FailureReason, c.stopped)
			}
			const why = "test case test-9 not found in test-set-0"
			if got := strings.Contains(rep.FailureReason, why); got != c.notFound {
				t.Fatalf("the reason %q names test-9: %v; want %v", rep.FailureReason, got, c.notFound)
			}
			if c.notFound {
				notFound(t, rowsOf(rep.Tests), "test-9")
			}
			if logs, want := rep.AppLogs != "", c.want != models.TestSetStatusFailed; logs != want {
				t.Fatalf("the report carries the application's logs: %v; want %v", logs, want)
			}
			if rep.Total != len(c.selected) || rep.Failure != c.failed || rep.HighRisk != c.highRisk {
				t.Fatalf("the report counts Total %d, failed %d, HIGH risk %d; want %d, %d, %d",
					rep.Total, rep.Failure, rep.HighRisk, len(c.selected), c.failed, c.highRisk)
			}
		})
	}
}

// TestASetFailedOnlyForASelectedCaseNotOnDiskChecksItsWorkerScope: with the
// switch on, the set's worker scope recorded mock-boot, a call no test owns,
// which the application never makes, and test-9, selected, is not on disk.
// The set is checked as it is without test-9: its worker scope fails it, said
// in the log, whether its run took up every test on disk or stopped early
// (the agent refuses its fourth ask). A set that ran every test on disk names
// its worker scope in its reason beside test-9. One that stopped early is
// FAILED, with no application logs, as it is without test-9: never the
// partial run's APP_FAULT, which would take the application's logs for why
// and, off docker compose, stop the test sets after it. A set that failed a
// test on disk as well is red whatever its worker scope did, as it is
// without test-9: nothing waits for its worker scope, and nothing names it.
// One whose end cannot read its agent as it waits for the worker scope is
// not judged (INTERNAL_ERR), as it is without test-9.
func TestASetFailedOnlyForASelectedCaseNotOnDiskChecksItsWorkerScope(t *testing.T) {
	const worker = "the application never made 1 of the calls the test set's worker scope recorded (the calls no test owns: mock-boot)"
	const why = "test case test-9 not found in test-set-0"
	for _, c := range []struct {
		name     string
		nth      int // the agent's ask it refuses; 0: none
		selected []string
		// test1Fails: test-1's answer differs from its recording.
		test1Fails bool
		reason     []string // what the set's reason says
		notFound   int
	}{
		{"every test on disk ran", 0, []string{"test-1", "test-2", "test-9"}, false, []string{why, worker}, 1},
		{"control: every test on disk ran, every name on disk", 0, []string{"test-1", "test-2"}, false, []string{worker}, 0},
		{"stopped early", 4, []string{"test-1", "test-2", "test-9"}, false, []string{why, partialRunReason}, 1},
		{"control: stopped early, every name on disk", 4, []string{"test-1", "test-2"}, false, []string{partialRunReason}, 0},
		{"test-1 failed too", 0, []string{"test-1", "test-2", "test-9"}, true, []string{why}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			setSwitch(t, true)
			fastWorkerWait(t)
			h := newPartialRunHarness(t, 2, c.nth)
			core, logs := observer.New(zapcore.ErrorLevel)
			h.replayer.logger = zap.New(core)
			pinWindows(h)
			h.replayer.config.Command = "./app"
			h.instr.recentAppLogs = "app: still serving\n"
			h.mocks.filtered = []*models.Mock{httpAt("mock-boot", 500), httpAt("mock-a", 1010), httpAt("mock-b", 2010)}
			mappingFile(t, h, nil)
			hooks := prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{
				"test-1": {mongoAt("mock-a", 1010)},
				"test-2": {mongoAt("mock-b", 2010)},
			})}
			failed := c.notFound
			if c.test1Fails {
				hooks.wrongBody = map[string]bool{"test-1": true}
				failed++
			}
			h.replayer.hookImpl = hooks
			h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": c.selected}
			status := h.run(t)
			rep := h.report.report
			if rep == nil {
				t.Fatal("no report was persisted")
			}
			if status != models.TestSetStatusFailed || rep.Status != string(models.TestSetStatusFailed) {
				t.Fatalf("the set is %s, its report %s; want FAILED (reason %q)", status, rep.Status, rep.FailureReason)
			}
			for _, w := range c.reason {
				if !strings.Contains(rep.FailureReason, w) {
					t.Fatalf("the reason %q does not say %q", rep.FailureReason, w)
				}
			}
			if got := strings.Contains(rep.FailureReason, why); got != (c.notFound > 0) {
				t.Fatalf("the reason %q names test-9: %v; want %v", rep.FailureReason, got, c.notFound > 0)
			}
			if rep.AppLogs != "" {
				t.Fatalf("the FAILED set's report carries the application's logs %q", rep.AppLogs)
			}
			if named := strings.Contains(rep.FailureReason, worker); named == c.test1Fails && c.nth == 0 {
				t.Fatalf("the reason %q names the worker scope: %v; want %v", rep.FailureReason, named, !c.test1Fails)
			}
			if logged := logs.FilterMessageSnippet("test-set failed: "+worker).Len() > 0; logged == c.test1Fails {
				t.Fatalf("a log says the worker scope failed the set: %v; want %v; errors logged: %v", logged, !c.test1Fails, messagesOf(logs))
			}
			if rep.Total != len(c.selected) || rep.Failure != failed {
				t.Fatalf("the report counts Total %d, failed %d; want %d, %d", rep.Total, rep.Failure, len(c.selected), failed)
			}
		})
	}
	// keploy cannot read its agent as the set's end waits for the worker
	// scope: what the worker scope answered is not known, so the set is not
	// judged (INTERNAL_ERR), with or without test-9, and its reason names
	// that read, after test-9 when test-9 is selected. The read fails once
	// (the set's end reads the agent again after the wait, and is answered),
	// or every read from the wait on fails.
	const unread = "keploy could not read what the test set's worker scope (the calls no test owns) answered: " + agentUnreachable
	for _, c := range []struct {
		name string
		once bool
	}{
		{"the first read in the wait fails", true},
		{"every read from the wait on fails", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			setSwitch(t, true)
			fastWorkerWait(t)
			run := func(selected ...string) (models.TestSetStatus, *models.TestReport, bool) {
				w := newWorkerScopeSet(t, selected...)
				w.h.replayer.hookImpl = readFailsInTheWait{TestHooks: w.h.replayer.hookImpl, inWait: w.inWait, once: c.once, failed: &atomic.Bool{}}
				status := w.h.run(t)
				rep := w.h.report.report
				if rep == nil {
					t.Fatal("no report was persisted")
				}
				if rep.Status != string(status) {
					t.Fatalf("the set is %s, its report %s", status, rep.Status)
				}
				return status, rep, w.inWait.Load()
			}
			ctlStatus, ctl, ctlWaited := run("test-1", "test-2")
			if ctlStatus != models.TestSetStatusInternalErr || !ctlWaited || !strings.Contains(ctl.FailureReason, unread) {
				t.Fatalf("precondition: without test-9 the set is %s, its end waited for its worker scope: %v, its reason %q; the case needs INTERNAL_ERR after the wait, its reason naming the read", ctlStatus, ctlWaited, ctl.FailureReason)
			}
			status, rep, waited := run("test-1", "test-2", "test-9")
			if status != models.TestSetStatusInternalErr {
				t.Fatalf("with test-9 the set is %s (reason %q); want INTERNAL_ERR, as without it: what its worker scope answered is not known", status, rep.FailureReason)
			}
			if !waited {
				t.Fatal("with test-9 the set's end did not wait for its worker scope; it waits as it does without test-9")
			}
			if want := why + "; " + ctl.FailureReason; rep.FailureReason != want {
				t.Fatalf("with test-9 the reason is %q; want %q: test-9, then why the set is not judged, as without it", rep.FailureReason, want)
			}
			if rep.Total != 3 || rep.Failure != ctl.Failure+1 || rep.HighRisk != ctl.HighRisk+1 {
				t.Fatalf("with test-9 the report counts Total %d, failed %d, HIGH risk %d; want 3, %d, %d", rep.Total, rep.Failure, rep.HighRisk, ctl.Failure+1, ctl.HighRisk+1)
			}
		})
	}
}

// waitingForWorkerScope is what the set's end logs as it starts to wait for
// its worker scope (awaitWorkerScope).
const waitingForWorkerScope = "waiting for the test set's worker scope"

// workerScopeSet is test-set-0, with test-1 and test-2 on disk and the named
// test cases selected, whose worker scope recorded mock-boot, a call no test
// owns, which the application never makes. inWait is set once its end waits
// for its worker scope; reads counts how many times keploy asked its agent
// what the application consumed.
type workerScopeSet struct {
	h      *prRun
	logs   *observer.ObservedLogs
	inWait *atomic.Bool
	reads  *atomic.Int32
}

func newWorkerScopeSet(t *testing.T, selected ...string) workerScopeSet {
	h := newPartialRunHarness(t, 2, 0)
	w := workerScopeSet{h: h, inWait: &atomic.Bool{}, reads: &atomic.Int32{}}
	core, logs := observer.New(zapcore.InfoLevel)
	w.logs = logs
	h.replayer.logger = zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
		if strings.Contains(e.Message, waitingForWorkerScope) {
			w.inWait.Store(true)
		}
		return nil
	}))
	pinWindows(h)
	h.replayer.config.Command = "./app"
	h.instr.recentAppLogs = "app: still serving\n"
	h.mocks.filtered = []*models.Mock{httpAt("mock-boot", 500), httpAt("mock-a", 1010), httpAt("mock-b", 2010)}
	mappingFile(t, h, nil)
	byWindow := consumedByWindow(h, map[string][]models.MockState{
		"test-1": {mongoAt("mock-a", 1010)},
		"test-2": {mongoAt("mock-b", 2010)},
	})
	h.replayer.hookImpl = prHooks{consumedFor: func() []models.MockState {
		w.reads.Add(1)
		return byWindow()
	}}
	h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": selected}
	return w
}

// agentUnreachable is why readFailsInTheWait's reads fail.
const agentUnreachable = "agent unreachable as the set's end waits for its worker scope"

// readFailsInTheWait answers as the hooks it wraps, but asking it what the
// application consumed fails once the set's end waits for its worker scope
// (inWait): the first time only (once), or every time from then on.
type readFailsInTheWait struct {
	TestHooks
	inWait *atomic.Bool
	once   bool
	failed *atomic.Bool
}

func (h readFailsInTheWait) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	if h.inWait.Load() && (!h.once || !h.failed.Load()) {
		h.failed.Store(true)
		return nil, errors.New(agentUnreachable)
	}
	return h.TestHooks.GetConsumedMocks(ctx)
}

// TestASetWhoseSelectedCasesOnDiskAreAllIgnoredWaitsForNoWorkerScope: with
// the switch on, test-1 and test-2, the set's test cases on disk, are both
// selected and both in test.ignoredTests, and test-9, selected too, is not
// on disk. The set is FAILED for test-9 alone, and none of its tests opened
// its capture, as without test-9 (the set is then IGNORED): its end reads
// nothing of what the application did, so nothing waits for its worker
// scope (mock-boot, which the application never makes), nothing names it,
// and keploy asks its agent what the application consumed as many times as
// without test-9.
func TestASetWhoseSelectedCasesOnDiskAreAllIgnoredWaitsForNoWorkerScope(t *testing.T) {
	setSwitch(t, true)
	fastWorkerWait(t)
	run := func(selected ...string) (models.TestSetStatus, *models.TestReport, bool, int32) {
		w := newWorkerScopeSet(t, selected...)
		w.h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {"test-1", "test-2"}}
		status := w.h.run(t)
		rep := w.h.report.report
		if rep == nil {
			t.Fatal("no report was persisted")
		}
		if rep.Status != string(status) {
			t.Fatalf("the set is %s, its report %s", status, rep.Status)
		}
		return status, rep, w.logs.FilterMessageSnippet(waitingForWorkerScope).Len() > 0, w.reads.Load()
	}
	ctlStatus, _, ctlWaited, ctlReads := run("test-1", "test-2")
	if ctlStatus != models.TestSetStatusIgnored || ctlWaited {
		t.Fatalf("precondition: without test-9 the set is %s, and its end waited for its worker scope: %v; the case needs IGNORED, no wait", ctlStatus, ctlWaited)
	}
	status, rep, waited, reads := run("test-1", "test-2", "test-9")
	const why = "test case test-9 not found in test-set-0"
	if status != models.TestSetStatusFailed || rep.FailureReason != why {
		t.Fatalf("with test-9 the set is %s, its reason %q; want FAILED, for test-9 alone (%q)", status, rep.FailureReason, why)
	}
	if waited {
		t.Fatal("with test-9 the set's end waited for its worker scope; no test of it opened its capture, so its end reads nothing, as without test-9")
	}
	if reads != ctlReads {
		t.Fatalf("with test-9 keploy asked its agent %d times what the application consumed, %d without it; want as many", reads, ctlReads)
	}
	if rep.Total != 3 || rep.Failure != 1 || rep.Ignored != 2 || rep.Success != 0 {
		t.Fatalf("with test-9 the report counts Total %d, failed %d, ignored %d, passed %d; want 3, 1, 2, 0", rep.Total, rep.Failure, rep.Ignored, rep.Success)
	}
}

// TestASetNotJudgedInItsRunWithASelectedCaseNotOnDiskWaitsForNoWorkerScope:
// with the switch on, the set's worker scope recorded mock-boot, which the
// application never makes, and test-9, selected, is not on disk, so the only
// test that fails is test-9. The set's run already left it without a verdict
// of its own before its end: keploy could not read or write its results
// (INTERNAL_ERR), its application exited after test-1 (APP_HALTED), or the
// user stopped the run as test-2 was sent (USER_ABORT). It ends the same way
// with or without test-9: nothing waits for its worker scope, and keploy asks
// its agent what the application consumed as many times as it does without
// test-9.
func TestASetNotJudgedInItsRunWithASelectedCaseNotOnDiskWaitsForNoWorkerScope(t *testing.T) {
	const waiting = "waiting for the test set's worker scope"
	for _, c := range []struct {
		name string
		arm  func(h *prRun)
		want models.TestSetStatus
	}{
		{"its results could not be read", func(h *prRun) {
			h.report.readErr = errors.New("report store is unreadable")
		}, models.TestSetStatusInternalErr},
		{"a judged test's result could not be written", func(h *prRun) {
			h.report.failInsertOf = map[string]error{"test-1": errors.New("report store is unwritable")}
		}, models.TestSetStatusInternalErr},
		{"the application exited after test-1", func(h *prRun) {
			h.replayer.instrumentation = &haltsAfterLastTest{prInstr: h.instr, at: 1}
		}, models.TestSetStatusAppHalted},
		{"the user stopped the run as test-2 was sent", func(h *prRun) {
			h.replayer.hookImpl = stopHooks{TestHooks: h.replayer.hookImpl, send: "test-2", stop: func() { h.cancel() }}
		}, models.TestSetStatusUserAbort},
	} {
		t.Run(c.name, func(t *testing.T) {
			setSwitch(t, true)
			fastWorkerWait(t)
			// run replays the set with the names selected, and returns its
			// status, how many times keploy asked its agent what the
			// application consumed, and whether it waited for the worker
			// scope.
			run := func(selected []string) (models.TestSetStatus, int32, bool) {
				h := newPartialRunHarness(t, 3, 0)
				core, logs := observer.New(zapcore.InfoLevel)
				h.replayer.logger = zap.New(core)
				pinWindows(h)
				h.replayer.config.Command = "./app"
				h.instr.recentAppLogs = "app: still serving\n"
				h.mocks.filtered = []*models.Mock{httpAt("mock-boot", 500), httpAt("mock-a", 1010), httpAt("mock-b", 2010), httpAt("mock-c", 3010)}
				mappingFile(t, h, nil)
				byWindow := consumedByWindow(h, map[string][]models.MockState{
					"test-1": {mongoAt("mock-a", 1010)},
					"test-2": {mongoAt("mock-b", 2010)},
					"test-3": {mongoAt("mock-c", 3010)},
				})
				var reads atomic.Int32
				h.replayer.hookImpl = prHooks{consumedFor: func() []models.MockState {
					reads.Add(1)
					return byWindow()
				}}
				h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": selected}
				c.arm(h)
				status := h.run(t)
				if rep := h.report.report; rep != nil && rep.Status != string(status) {
					t.Fatalf("the set is %s, its report %s", status, rep.Status)
				}
				waited := logs.FilterMessageSnippet(waiting).Len() > 0
				t.Logf("selected %v: %s, %d reads, waited %v", selected, status, reads.Load(), waited)
				return status, reads.Load(), waited
			}
			status, reads, waited := run([]string{"test-1", "test-2", "test-3", "test-9"})
			ctlStatus, ctlReads, ctlWaited := run([]string{"test-1", "test-2", "test-3"})
			if ctlStatus != c.want || ctlWaited {
				t.Fatalf("precondition: without test-9 the set is %s and waited for its worker scope: %v; the case needs %s, no wait", ctlStatus, ctlWaited, c.want)
			}
			if status != c.want {
				t.Fatalf("with test-9 the set is %s; want %s, as without it", status, c.want)
			}
			if waited {
				t.Fatal("with test-9 the set's end waited for its worker scope; a set its run left without a verdict waits for none, as without test-9")
			}
			if reads != ctlReads {
				t.Fatalf("with test-9 keploy asked its agent %d times what the application consumed, %d without it; want as many", reads, ctlReads)
			}
		})
	}
}

// messagesOf is what was logged, message by message.
func messagesOf(logs *observer.ObservedLogs) []string {
	var out []string
	for _, e := range logs.All() {
		out = append(out, e.Message)
	}
	return out
}

// TestACaptureIncompleteSetWithASelectedCaseNotOnDiskIsAsWithoutIt: the
// agent's PID map refused a process during the set, so calls of its reached
// the real dependency, and test-9, selected, is not on disk. That check comes
// after the partial-run downgrade and fails only a set that would pass, so the
// set is what it is without test-9: FAILED when every test on disk ran, and
// the partial run's APP_FAULT, with the application's logs, when the agent
// refused its third ask. Its reason says first that capture was incomplete,
// and names test-9, and its report carries the capture verdict.
func TestACaptureIncompleteSetWithASelectedCaseNotOnDiskIsAsWithoutIt(t *testing.T) {
	const capture = "capture incomplete: the agent's PID map target_namespace_pids refused 2 insert(s)"
	const why = "test case test-9 not found in test-set-0"
	for _, c := range []struct {
		name     string
		nth      int // the agent's ask it refuses; 0: none
		selected []string
		want     models.TestSetStatus
	}{
		{"every test on disk ran", 0, []string{"test-1", "test-2", "test-3", "test-9"}, models.TestSetStatusFailed},
		{"control: every test on disk ran, every name on disk", 0, []string{"test-1", "test-2", "test-3"}, models.TestSetStatusFailed},
		{"stopped early", 3, []string{"test-1", "test-2", "test-3", "test-9"}, models.TestSetStatusFaultUserApp},
		{"control: stopped early, every name on disk", 3, []string{"test-1", "test-2", "test-3"}, models.TestSetStatusFaultUserApp},
	} {
		t.Run(c.name, func(t *testing.T) {
			setSwitch(t, false)
			h := newPartialRunHarness(t, 3, c.nth)
			h.instr.recentAppLogs = "app: still serving\n"
			h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": c.selected}
			h.replayer.config.Test.Mocking = true // the check is for a run that answers from the recording
			h.replayer.instrumentation = &prPIDMapInstr{prInstr: h.instr, answers: []models.PIDMapRefusals{{}, {Target: 2, LastTargetTGID: 77}}}
			// As Start does once the agent is up and before the app starts.
			h.replayer.markPIDMaps(context.Background())
			status := h.run(t)
			rep := h.report.report
			if rep == nil {
				t.Fatal("no report was persisted")
			}
			if status != c.want || rep.Status != string(c.want) {
				t.Fatalf("the set is %s, its report %s; want %s (reason %q)", status, rep.Status, c.want, rep.FailureReason)
			}
			if !strings.HasPrefix(rep.FailureReason, capture) {
				t.Fatalf("the reason %q does not start with %q", rep.FailureReason, capture)
			}
			notOnDisk := slices.Contains(c.selected, "test-9")
			if got := strings.Contains(rep.FailureReason, why); got != notOnDisk {
				t.Fatalf("the reason %q names test-9: %v; want %v", rep.FailureReason, got, notOnDisk)
			}
			if cv := rep.CaptureVerification; cv == nil || cv.Status != models.CaptureIncompleteStatus {
				t.Fatalf("the report carries capture_verification %+v; want %q", cv, models.CaptureIncompleteStatus)
			}
			if logs, want := rep.AppLogs != "", c.want == models.TestSetStatusFaultUserApp; logs != want {
				t.Fatalf("the report carries the application's logs: %v; want %v", logs, want)
			}
		})
	}
}

// TestMustPassKeepsASelectedCaseNotOnDisk: --must-pass removes the failed
// test cases of a set and runs it again. A selected test case not on disk
// was never sent, and there is none to remove: it is kept, and said.
func TestMustPassKeepsASelectedCaseNotOnDisk(t *testing.T) {
	rows := []models.TestResult{
		{TestCaseID: "test-1", Status: models.TestStatusFailed},
		{TestCaseID: "test-9", Status: models.TestStatusFailed, FailureInfo: models.FailureInfo{NotFound: "test case test-9 not found in test-set-0"}},
		{TestCaseID: "test-2", Status: models.TestStatusPassed},
	}
	deletable, kept := mustPassDeletions(rows)
	if !slices.Equal(deletable, []string{"test-1"}) || !slices.Equal(kept, []string{"test-9"}) {
		t.Fatalf("--must-pass removes %v and keeps %v; want [test-1] and [test-9]", deletable, kept)
	}
}

// TestASubsetRunWithASelectedCaseNotOnDiskPrunesNothing: four test cases on
// disk, and test-1 to test-3 selected with test-9, which is not on disk.
// test-4 never runs, so the run is a subset: it removes no mock and writes
// no mappings.yaml, however many names it selected that are not on disk.
// The control selects every test case on disk and test-9: that run is
// complete.
func TestASubsetRunWithASelectedCaseNotOnDiskPrunesNothing(t *testing.T) {
	session := []models.MockState{{Name: "mock-session", Kind: models.HTTP, Lifetime: models.LifetimeSession}}
	// With the switch off: under it, replay removes unused mocks only beside a
	// mappings.yaml, and never writes one (only keploy record does).
	for _, c := range []struct {
		name                  string
		selected              []string
		total, passed, prunes int // prunes: and mappings.yaml writes
	}{
		{"a subset with a name not on disk", []string{"test-1", "test-2", "test-3", "test-9"}, 4, 3, 0},
		{"every test case on disk with a name not on disk", []string{"test-1", "test-2", "test-3", "test-4", "test-9"}, 5, 4, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := selectedRun(t, 4, c.selected...)
			h.replayer.hookImpl = prHooks{consumed: session}
			h.replayer.config.Test.RemoveUnusedMocks = true
			if status := h.run(t); status != models.TestSetStatusFailed {
				t.Fatalf("the set reported %q; want FAILED", status)
			}
			rep := h.report.report
			addsUp(t, rep)
			if rep.Total != c.total || rep.Success != c.passed || rep.Failure != 1 {
				t.Fatalf("Total %d, passed %d, failed %d; want %d, %d and 1", rep.Total, rep.Success, rep.Failure, c.total, c.passed)
			}
			if got := h.mocks.pruneCalls(); got != c.prunes {
				t.Fatalf("the run removed unused mocks %d time(s); want %d", got, c.prunes)
			}
			if got := h.mappings.insertCalls(); got != c.prunes {
				t.Fatalf("the run wrote mappings.yaml %d time(s); want %d", got, c.prunes)
			}
		})
	}
}

// TestMustPassTakesTheTestCasesItRemovesOutOfTheSelection: --must-pass
// removes test-3, which fails, from a set run with a selection, and runs the
// set again. test-3 leaves the selection: the next attempt runs what is left
// of it, and does not fail test-3 as selected and not on disk. When test-3
// was all the selection held, the set is not run again (an empty selection
// would run every test case on disk), and its last attempt's verdict stands.
func TestMustPassTakesTheTestCasesItRemovesOutOfTheSelection(t *testing.T) {
	// Each row sets the switch it runs under (the package's TestMain clears
	// KEPLOY_PROVEN): off, then on.
	for _, on := range []bool{false, true} {
		for _, c := range []struct {
			name     string
			selected []string
			left     []string // the selection after the run
			test1    int      // how many times test-1 was sent
			pass     bool     // the set's verdict in the run's summary
			total    int
		}{
			{"test-1 is left", []string{"test-1", "test-3"}, []string{"test-1"}, 2, true, 1},
			{"nothing is left", []string{"test-3"}, []string{"test-3"}, 0, false, 1},
		} {
			t.Run(switchName(on)+"/"+c.name, func(t *testing.T) {
				setSwitch(t, on)
				app := newBPApp(t)
				port := closedLocalPort(t)
				app.wrong["/test-3"] = true
				run := newBasePathRunOf(t, app.URL, nil, bpTestCase("test-1", port), bpTestCase("test-2", port), bpTestCase("test-3", port))
				rec := &deletionRecorder{bpTestDB: run.tests}
				run.replayer.testDB = rec
				run.replayer.config.Test.MustPass = true
				run.replayer.config.Test.MaxFailAttempts = 5
				run.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": slices.Clone(c.selected)}

				if _, err := run.start(t); err != nil {
					t.Fatalf("Start: %v", err)
				}
				if !slices.Equal(rec.deleted, []string{"test-3"}) {
					t.Fatalf("--must-pass deleted %v; want [test-3]", rec.deleted)
				}
				if got := run.replayer.config.Test.SelectedTests["test-set-0"]; !slices.Equal(got, c.left) {
					t.Fatalf("the selection is %v after the run; want %v", got, c.left)
				}
				sent := map[string]int{}
				for _, asked := range app.asked() {
					sent[asked]++
				}
				if sent["GET /test-1"] != c.test1 || sent["GET /test-2"] != 0 || sent["GET /test-3"] != 1 {
					t.Fatalf("sent test-1 %d, test-2 %d and test-3 %d time(s); want %d, 0 and 1", sent["GET /test-1"], sent["GET /test-2"], sent["GET /test-3"], c.test1)
				}
				run.report.mu.Lock()
				rep := run.report.report
				run.report.mu.Unlock()
				for _, row := range rep.Tests {
					if notFoundUnsent(row) {
						t.Fatalf("the test case --must-pass removed is failed as not found: %+v", row)
					}
				}
				if kept := run.logs.FilterMessageSnippet("--must-pass keeps the test cases refused").All(); len(kept) != 0 {
					t.Fatalf("--must-pass says it keeps %d test case(s) it removed itself", len(kept))
				}
				nothingLeft := run.logs.FilterMessageSnippet("--must-pass removed every test case selected in this test set").All()
				if want := map[bool]int{true: 0, false: 1}[c.pass]; len(nothingLeft) != want {
					t.Fatalf("said %d time(s) that the set has nothing left to run; want %d", len(nothingLeft), want)
				}
				v := run.replayer.completeTestReport["test-set-0"]
				if v.status != c.pass || v.total != c.total {
					t.Fatalf("the run's summary has the set as %+v; want passed=%v and total %d", v, c.pass, c.total)
				}
			})
		}
	}
}
