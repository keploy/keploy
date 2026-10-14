package replay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// prPIDMapInstr is the RunTestSet harness's agent, counting refused PID map
// inserts: each read answers the next of reads, the last one repeating, or err
// for every read, or errAt[n] for the nth read (from 1).
type prPIDMapInstr struct {
	*prInstr

	pmu       sync.Mutex
	answers   []models.PIDMapRefusals
	err       error
	errAt     map[int]error
	readCount int
}

func (f *prPIDMapInstr) GetPIDMapRefusals(context.Context) (models.PIDMapRefusals, error) {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	f.readCount++
	if f.err != nil {
		return models.PIDMapRefusals{}, f.err
	}
	if err := f.errAt[f.readCount]; err != nil {
		return models.PIDMapRefusals{}, err
	}
	i := f.readCount - 1
	if i >= len(f.answers) {
		i = len(f.answers) - 1
	}
	return f.answers[i], nil
}

// A test set during which the agent's PID map refused a process -- whose calls
// reached the real dependency instead of a mock -- is FAILED, every test in it
// having passed, and its report says why first. Refusals before the mark (the
// agent up, the application not yet started) are not its, and an agent that
// cannot count leaves the verdict alone.
func TestRunTestSetDuringWhichAProcessWasNotTrackedFails(t *testing.T) {
	for _, tc := range []struct {
		name       string
		answers    []models.PIDMapRefusals
		err        error
		wantStatus models.TestSetStatus
		wantReason string
	}{
		{
			name:       "refused during the test set",
			answers:    []models.PIDMapRefusals{{}, {Target: 2, LastTargetTGID: 77}},
			wantStatus: models.TestSetStatusFailed,
			wantReason: "capture incomplete: the agent's PID map target_namespace_pids refused 2 insert(s)",
		},
		{
			name:       "refused only before the mark",
			answers:    []models.PIDMapRefusals{{Target: 9}, {Target: 9}},
			wantStatus: models.TestSetStatusPassed,
		},
		{
			name:       "an agent that cannot count",
			err:        fmt.Errorf("%w: agent returned 404", models.ErrPIDMapRefusalsUnsupported),
			wantStatus: models.TestSetStatusPassed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 3, 0) // every test passes
			instr := &prPIDMapInstr{prInstr: h.instr, answers: tc.answers, err: tc.err}
			h.replayer.instrumentation = instr
			// As Start does once the agent is up and before the app starts.
			h.replayer.markPIDMaps(context.Background())

			status := h.run(t)
			if status != tc.wantStatus {
				t.Fatalf("test set status %q, want %q", status, tc.wantStatus)
			}
			if got := len(h.report.results); got != 3 {
				t.Fatalf("recorded %d test results, want 3: the loop did not run every test", got)
			}
			if tc.err == nil && instr.readCount != 2 {
				t.Fatalf("the agent's refusals were read %d times, want at the mark and at the end of the test set", instr.readCount)
			}
			reason := h.report.report.FailureReason
			cv := h.report.report.CaptureVerification
			if tc.wantReason == "" {
				if strings.Contains(reason, "capture incomplete") {
					t.Fatalf("the report says %q of a test set nothing was refused in", reason)
				}
				if cv != nil {
					t.Fatalf("the report carries capture_verification %+v of a test set nothing was refused in", cv)
				}
				return
			}
			if !strings.HasPrefix(reason, tc.wantReason) {
				t.Fatalf("the report's failure reason is %q, want it to start with %q", reason, tc.wantReason)
			}
			if h.report.report.Status != string(models.TestSetStatusFailed) {
				t.Fatalf("the report's status is %q, want FAILED", h.report.report.Status)
			}
			// The report carries the capture-verification status (B2), not only
			// the failure reason.
			if cv == nil || cv.Status != models.CaptureIncompleteStatus || !strings.HasPrefix(cv.Reason, "capture incomplete") {
				t.Fatalf("report capture_verification %+v, want status %q with a 'capture incomplete' reason", cv, models.CaptureIncompleteStatus)
			}
		})
	}
}

// A refusal fails only a test set that would have passed: a status that
// already says more about what went wrong -- here the app runner failed with
// keploy's internal error during test-4's filter call, INTERNAL_ERR, every
// test having passed -- is kept, and the incomplete capture is put first in
// its report's reason.
func TestACaptureIncompleteTestSetKeepsAStatusThatSaysMore(t *testing.T) {
	h := newPartialRunHarness(t, 4, 0)
	// As TestRunTestSetIncompleteRunDoesNotCreateMappings' "keploy hit an internal error": the
	// harness waits for the replayer to see the stop, so the status is
	// INTERNAL_ERR every time.
	h.instr.stopAppAfterNUpdates, h.instr.stopErr = 5, models.ErrInternal
	instr := &prPIDMapInstr{prInstr: h.instr, answers: []models.PIDMapRefusals{{}, {Target: 2, LastTargetTGID: 77}}}
	h.replayer.instrumentation = instr
	h.replayer.markPIDMaps(context.Background())

	status := h.run(t)
	if status != models.TestSetStatusInternalErr {
		t.Fatalf("test set status %q, want %q: a refusal must not replace a status that already says what went wrong", status, models.TestSetStatusInternalErr)
	}
	if rep := h.report.report; rep == nil || !strings.HasPrefix(rep.FailureReason, "capture incomplete:") {
		t.Fatalf("the report is %+v; want its failure reason to start with the incomplete capture", rep)
	}
}

// appStartPIDMapInstr also notes how many times the agent's refusals had been
// read when the application was started.
type appStartPIDMapInstr struct {
	*prPIDMapInstr
	readsAtAppStart int // -1 until the application is started
}

func (f *appStartPIDMapInstr) Run(ctx context.Context, opts models.RunOptions) models.AppError {
	f.pmu.Lock()
	f.readsAtAppStart = f.readCount
	f.pmu.Unlock()
	return f.prInstr.Run(ctx, opts)
}

func (f *appStartPIDMapInstr) appStartedAfter() int {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	return f.readsAtAppStart
}

// The run itself takes the mark, and takes it before the application starts:
// Start for an agent that serves the whole run, RunTestSet for a compose agent
// that comes up with the test set's project. A test set has nothing to count
// its refusals from without it, and passes whatever the agent refused; taken
// after the application started, it would leave out the refusals of the
// application's own start.
func TestTheRunMarksThePIDMapsBeforeTheAppStarts(t *testing.T) {
	refusedDuring := []models.PIDMapRefusals{{}, {Target: 2, LastTargetTGID: 77}}
	const reason = "capture incomplete: the agent's PID map target_namespace_pids refused 2 insert(s)"
	// said keeps what the run logged, for a failure to print: the harness's own
	// logger keeps nothing.
	said := func(t *testing.T, h *prRun) {
		t.Helper()
		core, logs := observer.New(zapcore.DebugLevel)
		h.replayer.logger = zap.New(core)
		t.Cleanup(func() {
			if !t.Failed() {
				return
			}
			for _, e := range logs.All() {
				t.Logf("%s %s %v", e.Level, e.Message, e.ContextMap())
			}
		})
	}
	check := func(t *testing.T, h *prRun, instr *appStartPIDMapInstr) {
		t.Helper()
		if got := instr.appStartedAfter(); got != 1 {
			t.Fatalf("the agent's refusals had been read %d time(s) when the application was started, want 1: the mark (-1: it never started)", got)
		}
		if got := len(h.report.results); got != 3 {
			t.Fatalf("recorded %d test results, want 3: the loop did not run every test", got)
		}
		if h.report.report == nil || h.report.report.Status != string(models.TestSetStatusFailed) || !strings.HasPrefix(h.report.report.FailureReason, reason) {
			t.Fatalf("the report is %+v; want FAILED, its failure reason starting with %q", h.report.report, reason)
		}
	}

	t.Run("Start, for an agent that serves the whole run", func(t *testing.T) {
		h := newPartialRunHarness(t, 3, 0) // every test passes
		instr := &appStartPIDMapInstr{prPIDMapInstr: &prPIDMapInstr{prInstr: h.instr, answers: refusedDuring}, readsAtAppStart: -1}
		h.replayer.instrumentation = instr
		said(t, h)
		h.replayer.config.Command = "./app"
		h.replayer.config.CommandType = string(utils.Native)
		// What config/default.go sets, which the harness's bare config does
		// not: Start runs each test set this many times at most.
		h.replayer.config.Test.MaxFlakyChecks = 1
		saved := utils.ErrCode
		t.Cleanup(func() { utils.ErrCode = saved })
		utils.ErrCode = 0
		ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
		defer cancel()

		if err := h.replayer.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		check(t, h, instr)
		if utils.ErrCode != 1 {
			t.Fatalf("exit code %d, want 1: a process went untracked", utils.ErrCode)
		}
	})

	t.Run("RunTestSet, for a compose agent the test set starts", func(t *testing.T) {
		h := newPartialRunHarness(t, 3, 0)
		instr := &appStartPIDMapInstr{prPIDMapInstr: &prPIDMapInstr{prInstr: h.instr, answers: refusedDuring}, readsAtAppStart: -1}
		h.replayer.instrumentation = instr
		said(t, h)
		h.replayer.config.Command = "docker compose up"
		h.replayer.config.CommandType = string(utils.DockerCompose)
		// RunTestSet waits for the compose agent's /health before it goes on.
		agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
		defer agent.Close()
		h.replayer.config.Agent.AgentURI = agent.URL

		if status := h.run(t); status != models.TestSetStatusFailed {
			t.Fatalf("test set status %q, want %q", status, models.TestSetStatusFailed)
		}
		// Under compose the project, the agent in it, is started first, and
		// the application in it stays parked until the agent is told to
		// release it: the mark comes in between.
		instr.pmu.Lock()
		reads := instr.readCount
		instr.pmu.Unlock()
		if reads != 2 {
			t.Fatalf("the agent's refusals were read %d times, want at the mark and at the end of the test set", reads)
		}
		if h.report.report == nil || h.report.report.Status != string(models.TestSetStatusFailed) || !strings.HasPrefix(h.report.report.FailureReason, reason) {
			t.Fatalf("the report is %+v; want FAILED, its failure reason starting with %q", h.report.report, reason)
		}
	})
}

// A test set during which a process went untracked consumed less than it
// used: that process's calls reached the real dependency, so the mocks recorded
// for them look unused, and are in no test's entry. Pruning against that
// (--remove-unused-mocks) deletes them from the recording, and mappings.yaml
// written from it leaves them out of their tests' entries: for good by
// default, as no default run rewrites a file that exists, and in place of
// complete entries with --update-test-mapping. Neither is done, whatever the
// flags; both are, as before, for a test set nothing was refused in.
func TestACaptureIncompleteTestSetDoesNotPruneOrWriteMappings(t *testing.T) {
	session := []models.MockState{{Name: "mock-session", Kind: models.HTTP, Lifetime: models.LifetimeSession}}
	refused := []models.PIDMapRefusals{{}, {Target: 2, LastTargetTGID: 77}}
	for _, tc := range []struct {
		name          string
		answers       []models.PIDMapRefusals
		updateMapping bool
		fileExists    bool
		want          int // UpdateMocks calls, and mappings.yaml writes
	}{
		{name: "a process was refused", answers: refused},
		{name: "a process was refused, with --update-test-mapping", answers: refused, updateMapping: true, fileExists: true},
		{name: "only excluded_pids refused", answers: []models.PIDMapRefusals{{}, {Excluded: 1}}},
		// Controls: the prune and the write stay as they were.
		{name: "nothing refused", answers: []models.PIDMapRefusals{{}, {}}, want: 1},
		{name: "nothing refused, with --update-test-mapping", answers: []models.PIDMapRefusals{{}, {}}, updateMapping: true, fileExists: true, want: 1},
		{name: "refused only before the mark", answers: []models.PIDMapRefusals{{Target: 9}, {Target: 9}}, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.WarnLevel)
			h := newPartialRunHarness(t, 4, 0) // every test passes
			h.replayer.logger = zap.New(core)
			h.replayer.hookImpl = prHooks{consumed: session}
			h.replayer.config.Test.RemoveUnusedMocks = true
			h.replayer.config.Test.UpdateTestMapping = tc.updateMapping
			h.mappings.exists = tc.fileExists
			h.replayer.instrumentation = &prPIDMapInstr{prInstr: h.instr, answers: tc.answers}
			h.replayer.markPIDMaps(context.Background())

			_ = h.run(t)

			rep := h.report.report
			if rep == nil || rep.Success != 4 || rep.Failure != 0 || rep.Obsolete != 0 {
				t.Fatalf("precondition: want all four tests to pass, got %+v", rep)
			}
			if got := h.mocks.pruneCalls(); got != tc.want {
				t.Fatalf("the run made %d UpdateMocks calls, want %d", got, tc.want)
			}
			if got := h.mappings.insertCalls(); got != tc.want {
				t.Fatalf("the run wrote mappings.yaml %d times, want %d", got, tc.want)
			}
			if tc.want != 0 {
				return
			}
			// Both are said, with the reason, where the user asked for them.
			skipped := logs.FilterMessageSnippet("skipping mock pruning").All()
			if len(skipped) != 1 || skipped[0].ContextMap()["reason"] != "capture_incomplete" {
				t.Fatalf("want one \"skipping mock pruning\" line with reason capture_incomplete, got %+v", skipped)
			}
			held := logs.FilterMessage("not writing mappings.yaml").All()
			if len(held) != 1 || !strings.Contains(fmt.Sprint(held[0].ContextMap()["reason"]), "could not track every process") {
				t.Fatalf("want one \"not writing mappings.yaml\" line saying the agent could not track every process, got %+v", held)
			}
		})
	}
}

// Each test set's refusals are its own: the end of one moves the mark, so the
// next counts from there.
func TestPIDMapMarkMovesToTheEndOfEachTestSet(t *testing.T) {
	h := newPartialRunHarness(t, 1, 0)
	instr := &prPIDMapInstr{prInstr: h.instr, answers: []models.PIDMapRefusals{{}, {Target: 1}, {Target: 1}, {Target: 3}}}
	h.replayer.instrumentation = instr
	r := h.replayer
	ctx := context.Background()
	r.markPIDMaps(ctx)
	if got := r.pidMapCaptureIncomplete(ctx, "test-set-0"); !strings.Contains(got.incomplete, "refused 1 insert(s)") || got.unverified != "" {
		t.Fatalf("test-set-0: %+v, want 1 refused", got)
	}
	if got := r.pidMapCaptureIncomplete(ctx, "test-set-1"); got != (pidMapVerdict{}) {
		t.Fatalf("test-set-1, nothing refused in it: %+v", got)
	}
	if got := r.pidMapCaptureIncomplete(ctx, "test-set-2"); !strings.Contains(got.incomplete, "refused 2 insert(s)") {
		t.Fatalf("test-set-2: %+v, want the 2 refused since test-set-1 ended", got)
	}
}

// A count the agent keeps but that could not be read is neither a refusal nor
// "none": the test sets it ends and starts are unverified -- each said at WARN
// -- and the next read that succeeds is the next mark, so a lost read does not
// cost the rest of the run its verdicts. An agent that cannot count at all
// leaves every test set as it was, with nothing said.
func TestALostPIDMapCountLeavesOneTestSetUnverified(t *testing.T) {
	lost := errors.New("agent returned 500: read pid_map_insert_failures[target_namespace_pids]: bad file descriptor")
	type want struct{ incomplete, unverified bool }
	for _, tc := range []struct {
		name    string
		answers []models.PIDMapRefusals
		errAt   map[int]error
		err     error
		want    []want // test-set-0, test-set-1, ...
	}{
		{
			name:    "lost at a test set's end: that test set, then counted again",
			answers: []models.PIDMapRefusals{{}, {}, {}, {Target: 2}},
			errAt:   map[int]error{2: lost},
			want:    []want{{unverified: true}, {unverified: true}, {incomplete: true}},
		},
		{
			name:    "lost at the mark: the first test set, then counted",
			answers: []models.PIDMapRefusals{{}, {Target: 1}, {Target: 1}, {Target: 4}},
			errAt:   map[int]error{1: lost},
			want:    []want{{unverified: true}, {}, {incomplete: true}},
		},
		{
			name: "an agent that cannot count: nothing to verify, nothing said",
			err:  fmt.Errorf("%w: agent returned 501", models.ErrPIDMapRefusalsUnsupported),
			want: []want{{}, {}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.WarnLevel)
			h := newPartialRunHarness(t, 1, 0)
			r := h.replayer
			r.logger = zap.New(core)
			r.instrumentation = &prPIDMapInstr{prInstr: h.instr, answers: tc.answers, errAt: tc.errAt, err: tc.err}
			ctx := context.Background()
			r.markPIDMaps(ctx)
			unverified := 0
			for i, w := range tc.want {
				got := r.pidMapCaptureIncomplete(ctx, fmt.Sprintf("test-set-%d", i))
				if (got.incomplete != "") != w.incomplete || (got.unverified != "") != w.unverified {
					t.Fatalf("test-set-%d: %+v, want incomplete %v, unverified %v", i, got, w.incomplete, w.unverified)
				}
				if w.unverified {
					unverified++
					if !strings.HasPrefix(got.unverified, "capture unverified") || !strings.Contains(got.unverified, "bad file descriptor") {
						t.Fatalf("test-set-%d: unverified reason %q does not say what was lost and why", i, got.unverified)
					}
				}
			}
			said := logs.FilterMessageSnippet("this test set is not verified").All()
			if len(said) != unverified {
				t.Fatalf("logged %d \"not verified\" WARN line(s), want one for each of the %d unverified test set(s): %+v", len(said), unverified, logs.All())
			}
			if tc.err != nil && logs.Len() != 0 {
				t.Fatalf("an agent that cannot count: logged %+v at WARN, want nothing", logs.All())
			}
		})
	}
}

// A test set whose count was lost is not failed for it -- keploy test has no
// --strict -- but what it consumed is no basis for deleting recorded mocks or
// for mappings.yaml: nothing shows an untracked process did not make calls the
// recording has mocks for. Neither is done, with the reason said; an agent
// that cannot count prunes and writes as before.
func TestAnUnverifiedTestSetDoesNotPruneOrWriteMappings(t *testing.T) {
	session := []models.MockState{{Name: "mock-session", Kind: models.HTTP, Lifetime: models.LifetimeSession}}
	lost := errors.New("agent returned 500: bad file descriptor")
	for _, tc := range []struct {
		name          string
		errAt         map[int]error
		err           error
		updateMapping bool
		want          int // UpdateMocks calls, and mappings.yaml writes
	}{
		{name: "lost at the end", errAt: map[int]error{2: lost}},
		{name: "lost at the mark", errAt: map[int]error{1: lost}},
		{name: "lost at the end, with --update-test-mapping", errAt: map[int]error{2: lost}, updateMapping: true},
		// Control: an agent that cannot count is not one whose count was lost.
		{name: "an agent that cannot count", err: fmt.Errorf("%w: agent returned 404", models.ErrPIDMapRefusalsUnsupported), want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.WarnLevel)
			h := newPartialRunHarness(t, 4, 0) // every test passes
			h.replayer.logger = zap.New(core)
			h.replayer.hookImpl = prHooks{consumed: session}
			h.replayer.config.Test.RemoveUnusedMocks = true
			h.replayer.config.Test.UpdateTestMapping = tc.updateMapping
			h.mappings.exists = tc.updateMapping
			h.replayer.instrumentation = &prPIDMapInstr{prInstr: h.instr, answers: []models.PIDMapRefusals{{}}, errAt: tc.errAt, err: tc.err}
			h.replayer.markPIDMaps(context.Background())

			status := h.run(t)

			rep := h.report.report
			if rep == nil || rep.Success != 4 || rep.Failure != 0 || rep.Obsolete != 0 {
				t.Fatalf("precondition: want all four tests to pass, got %+v", rep)
			}
			if status != models.TestSetStatusPassed || strings.Contains(rep.FailureReason, "capture") {
				t.Fatalf("status %q, reason %q: a lost count fails nothing, and is no refusal", status, rep.FailureReason)
			}
			// The founder rule: capture is never silent. A lost count leaves a
			// trace in the report (not only a WARN log); an agent that cannot
			// count at all leaves none, because there was nothing to verify.
			if tc.want == 0 {
				cv := rep.CaptureVerification
				if cv == nil || cv.Status != models.CaptureUnverifiedStatus || !strings.HasPrefix(cv.Reason, "capture unverified") {
					t.Fatalf("report capture_verification %+v, want status %q with a 'capture unverified' reason", cv, models.CaptureUnverifiedStatus)
				}
			} else if rep.CaptureVerification != nil {
				t.Fatalf("report carries capture_verification %+v for an agent that cannot count, want none", rep.CaptureVerification)
			}
			if got := h.mocks.pruneCalls(); got != tc.want {
				t.Fatalf("the run made %d UpdateMocks calls, want %d", got, tc.want)
			}
			if got := h.mappings.insertCalls(); got != tc.want {
				t.Fatalf("the run wrote mappings.yaml %d times, want %d", got, tc.want)
			}
			if tc.want != 0 {
				return
			}
			skipped := logs.FilterMessageSnippet("skipping mock pruning").All()
			if len(skipped) != 1 || skipped[0].ContextMap()["reason"] != "capture_unverified" {
				t.Fatalf("want one \"skipping mock pruning\" line with reason capture_unverified, got %+v", skipped)
			}
			if tc.updateMapping {
				held := logs.FilterMessage("not writing mappings.yaml").All()
				if len(held) != 1 || !strings.Contains(fmt.Sprint(held[0].ContextMap()["reason"]), "could not read whether the agent tracked every process") {
					t.Fatalf("want one \"not writing mappings.yaml\" line saying the count could not be read, got %+v", held)
				}
			}
		})
	}
}

func TestWithCaptureIncomplete(t *testing.T) {
	for _, tc := range []struct{ ci, reason, want string }{
		{"", "", ""},
		{"", "r", "r"},
		{"c", "", "c"},
		{"c", "r", "c; r"},
	} {
		if got := withCaptureIncomplete(tc.ci, tc.reason); got != tc.want {
			t.Errorf("withCaptureIncomplete(%q, %q) = %q, want %q", tc.ci, tc.reason, got, tc.want)
		}
	}
}

// Under the proven switch a process the PID map refused taints the test
// running when it happened and every later test of the set (design v14 §3.2):
// each is FAILED with CAPTURE_INCOMPLETE and what was lost, and the test
// before it is not. With the switch off the per-test reads are not made.
func TestUnderTheSwitchACaptureLossFailsTheTestRunningAndEveryLaterOne(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newPartialRunHarness(t, 3, 0) // every test passes
	// The mark, then one read as each test ends, then the set's own read.
	instr := &prPIDMapInstr{prInstr: h.instr, answers: []models.PIDMapRefusals{
		{}, {}, {Target: 2, LastTargetErr: -7}, {Target: 2, LastTargetErr: -7}, {Target: 2, LastTargetErr: -7},
	}}
	h.replayer.instrumentation = instr
	h.replayer.markPIDMaps(context.Background())

	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("test set status %q, want FAILED", status)
	}
	byID := map[string]models.TestResult{}
	for _, r := range h.report.report.Tests {
		byID[r.TestCaseID] = r
	}
	if r := byID["test-1"]; r.Status != models.TestStatusPassed || len(r.FailureInfo.CaptureIncomplete) != 0 {
		t.Fatalf("test-1 (ended before the loss): %s %+v, want PASSED with no capture evidence", r.Status, r.FailureInfo.CaptureIncomplete)
	}
	for _, id := range []string{"test-2", "test-3"} {
		r := byID[id]
		if r.Status != models.TestStatusFailed {
			t.Fatalf("%s: %s, want FAILED: the loss taints the test running and every later one", id, r.Status)
		}
		if len(r.FailureInfo.CaptureIncomplete) != 1 || r.FailureInfo.CaptureIncomplete[0].Count != 2 ||
			!strings.Contains(r.FailureInfo.CaptureIncomplete[0].What, "target_namespace_pids") {
			t.Fatalf("%s: capture evidence %+v, want 2 refused inserts into target_namespace_pids", id, r.FailureInfo.CaptureIncomplete)
		}
		found := false
		for _, c := range r.FailureInfo.Category {
			found = found || c == models.CaptureIncompleteCategory
		}
		if !found {
			t.Fatalf("%s: categories %v lack %s", id, r.FailureInfo.Category, models.CaptureIncompleteCategory)
		}
	}
}

// Under the proven switch a per-test read that fails leaves that test without
// a verdict, and its set unjudged, like a miss list not read whole.
func TestUnderTheSwitchAPerTestCaptureReadThatFailsLeavesTheSetUnjudged(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newPartialRunHarness(t, 3, 0)
	instr := &prPIDMapInstr{prInstr: h.instr, answers: []models.PIDMapRefusals{{}}, errAt: map[int]error{3: errors.New("connection reset")}}
	h.replayer.instrumentation = instr
	h.replayer.markPIDMaps(context.Background())
	status := h.run(t)
	if status == models.TestSetStatusPassed || status == models.TestSetStatusFailed {
		t.Fatalf("status %s: a set with a test whose capture could not be told is neither PASSED nor FAILED", status)
	}
	var unread []string
	for _, r := range h.report.report.Tests {
		if r.FailureInfo.UnmatchedCallsUnread {
			unread = append(unread, r.TestCaseID)
		}
	}
	if len(unread) != 1 || unread[0] != "test-2" {
		t.Fatalf("tests without a verdict %v, want [test-2] (the read as it ended failed)", unread)
	}
}
