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
			h := newPartialRunHarness(t, 3, 0)    // every test passes
			h.replayer.config.Test.Mocking = true // the check is for a run that answers from the recording
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
	h.replayer.config.Test.Mocking = true // the check is for a run that answers from the recording
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
		h := newPartialRunHarness(t, 3, 0)    // every test passes
		h.replayer.config.Test.Mocking = true // the check is for a run that answers from the recording
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
		h.replayer.config.Test.Mocking = true // the check is for a run that answers from the recording
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
			h := newPartialRunHarness(t, 4, 0)    // every test passes
			h.replayer.config.Test.Mocking = true // the check is for a run that answers from the recording
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
	h.replayer.config.Test.Mocking = true // the check is for a run that answers from the recording
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
			h.replayer.config.Test.Mocking = true // the check is for a run that answers from the recording
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
			h := newPartialRunHarness(t, 4, 0)    // every test passes
			h.replayer.config.Test.Mocking = true // the check is for a run that answers from the recording
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

// With --mocking=false, or a global passthrough, every call goes to the real
// dependency by design: a process the agent could not track loses nothing, so
// no test set is failed or held for it, and the agent is not even asked.
func TestARunThatDoesNotMockIsNotCheckedForUntrackedProcesses(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(h *prRun)
	}{
		{"--mocking=false", func(h *prRun) { h.replayer.config.Test.Mocking = false }},
		{"a global passthrough", func(h *prRun) { h.replayer.config.Record.GlobalPassthrough = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 3, 0)
			h.replayer.config.Test.Mocking = true
			tc.set(h)
			instr := &prPIDMapInstr{prInstr: h.instr, answers: []models.PIDMapRefusals{{}, {Target: 2, LastTargetTGID: 77}}}
			h.replayer.instrumentation = instr
			h.replayer.markPIDMaps(context.Background())
			if status := h.run(t); status != models.TestSetStatusPassed {
				t.Fatalf("status %q, want PASSED: nothing was to be mocked, so nothing was lost", status)
			}
			if instr.readCount != 0 {
				t.Fatalf("the agent's refusals were read %d times, want none", instr.readCount)
			}
			if cv := h.report.report.CaptureVerification; cv != nil {
				t.Fatalf("capture_verification %+v, want none", cv)
			}
		})
	}
}

// A compose stack reused across test sets is marked by the first test set
// that reaches its agent, not only by the run's first test set, which can end
// before it does (an empty set, a pre-script that failed): the check is not
// switched off for the rest of the run. Once a test set has marked it, the
// next one counts from the mark the last one left, not from a new one.
func TestAReusedComposeStackIsMarkedByTheFirstTestSetToReachItsAgent(t *testing.T) {
	reused := func(t *testing.T, answers []models.PIDMapRefusals) (*prRun, *prPIDMapInstr) {
		h := newPartialRunHarness(t, 3, 0)
		h.replayer.config.Test.Mocking = true
		instr := &prPIDMapInstr{prInstr: h.instr, answers: answers}
		h.replayer.instrumentation = instr
		h.replayer.config.Command = "docker compose up"
		h.replayer.config.CommandType = string(utils.DockerCompose)
		agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
		t.Cleanup(agent.Close)
		h.replayer.config.Agent.AgentURI = agent.URL
		// Not the run's first test set.
		h.replayer.isFirstTestSet = false
		return h, instr
	}

	// No test set has marked yet: this one marks.
	h, instr := reused(t, []models.PIDMapRefusals{{}, {Target: 2, LastTargetTGID: 77}})
	h.replayer.pidMarked = false
	if status := h.runServing(t); status != models.TestSetStatusFailed {
		t.Fatalf("status %q, want FAILED: a process went untracked during this test set", status)
	}
	if instr.readCount != 2 {
		t.Fatalf("the agent's refusals were read %d times, want at the mark and at the end of the test set", instr.readCount)
	}

	// The next test set on the same stack counts from the mark the last one
	// left at its end: a process refused between the two test sets is the
	// next one's, not lost to a new mark taken as it starts.
	h, instr = reused(t, []models.PIDMapRefusals{{}, {}, {Target: 2, LastTargetTGID: 77}})
	h.replayer.pidMarked = false
	if status := h.runServing(t); status != models.TestSetStatusPassed {
		t.Fatalf("first test set: status %q, want PASSED", status)
	}
	if status := h.runServing(t); status != models.TestSetStatusFailed {
		t.Fatalf("second test set: status %q, want FAILED: counted from the first one's end, a process went untracked", status)
	}
	if instr.readCount != 3 {
		t.Fatalf("the agent's refusals were read %d times, want 3: the first set's mark and end, and the second's end only", instr.readCount)
	}
}

// Under the proven switch, a set whose PID-map count could not be read has no
// verdict: neither PASSED nor FAILED, the reason in the report, and the run
// goes on to the next test set rather than ending after this one. With the
// switch off the set keeps its status, and the prune it holds names the lost
// count, not an INTERNAL_ERR of keploy's own; under the switch a run prunes
// nothing at all (removeUnusedMocks is ignored, S12).
func TestUnderTheSwitchASetWhoseCountIsLostIsNotJudged(t *testing.T) {
	session := []models.MockState{{Name: "mock-session", Kind: models.HTTP, Lifetime: models.LifetimeSession}}
	// A set that FAILED on its own is not judged either: its failures rest
	// on a capture that cannot be told complete.
	t.Run("switch true, a failing test", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		h := newPartialRunHarness(t, 3, 0)
		h.replayer.hookImpl = prHooks{wrongBody: map[string]bool{"test-2": true}} // test-2 fails
		h.replayer.config.Test.Mocking = true
		h.replayer.instrumentation = &prPIDMapInstr{prInstr: h.instr, answers: []models.PIDMapRefusals{{}}, errAt: map[int]error{2: errors.New("connection reset")}}
		h.replayer.markPIDMaps(context.Background())
		if status := h.run(t); status != models.TestSetStatusInternalErr || !h.replayer.notJudged("test-set-0") {
			t.Fatalf("status %q, not judged %v; want %q and not judged", status, h.replayer.notJudged("test-set-0"), models.TestSetStatusInternalErr)
		}
	})
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprint("switch ", on), func(t *testing.T) {
			if on {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			core, logs := observer.New(zapcore.WarnLevel)
			h := newPartialRunHarness(t, 3, 0)
			h.replayer.config.Test.Mocking = true
			h.replayer.logger = zap.New(core)
			h.replayer.hookImpl = prHooks{consumed: session}
			h.replayer.config.Test.RemoveUnusedMocks = true
			// Read at the mark, lost at the set's end.
			instr := &prPIDMapInstr{prInstr: h.instr, answers: []models.PIDMapRefusals{{}}, errAt: map[int]error{2: errors.New("connection reset")}}
			h.replayer.instrumentation = instr
			h.replayer.markPIDMaps(context.Background())
			status := h.run(t)
			judged := !h.replayer.notJudged("test-set-0")
			skipped := logs.FilterMessageSnippet("skipping mock pruning").All()
			if on {
				if len(skipped) != 0 || h.mocks.pruneCalls() != 0 {
					t.Fatalf("under the switch the run prunes nothing: %d prune calls, %+v", h.mocks.pruneCalls(), skipped)
				}
			} else if len(skipped) != 1 || skipped[0].ContextMap()["reason"] != "capture_unverified" {
				t.Fatalf("want one \"skipping mock pruning\" line with reason capture_unverified, got %+v", skipped)
			}
			if !on {
				if status != models.TestSetStatusPassed || !judged {
					t.Fatalf("switch off: status %q, judged %v; want PASSED, judged", status, judged)
				}
				return
			}
			if status != models.TestSetStatusInternalErr || judged {
				t.Fatalf("status %q, judged %v; want %q and not judged", status, judged, models.TestSetStatusInternalErr)
			}
			if abortAfterTestSet(status, utils.Native, !judged) {
				t.Fatal("the run would end after this test set: a set without a verdict goes on to the next one")
			}
			if rep := h.report.report; rep == nil || !strings.Contains(rep.FailureReason, "could not read whether") {
				t.Fatalf("report %+v does not say the count could not be read", rep)
			}
		})
	}
}

// prLossInstr is the RunTestSet harness's agent, counting capture losses: each
// read answers the next of answers, the last one repeating, or errAt[n] for
// the nth read (from 1).
type prLossInstr struct {
	*prInstr

	lmu     sync.Mutex
	answers []models.CaptureLosses
	errAt   map[int]error
	reads   int
}

func (f *prLossInstr) GetCaptureLosses(context.Context) (models.CaptureLosses, error) {
	f.lmu.Lock()
	defer f.lmu.Unlock()
	f.reads++
	if err := f.errAt[f.reads]; err != nil {
		return nil, err
	}
	i := f.reads - 1
	if i >= len(f.answers) {
		i = len(f.answers) - 1
	}
	return f.answers[i], nil
}

// Under the proven switch a capture loss taints the test running when it
// happened and every later test of the set (design v14 §3.2): each is FAILED
// with CAPTURE_INCOMPLETE and what was lost, and the test before it is not.
func TestUnderTheSwitchACaptureLossFailsTheTestRunningAndEveryLaterOne(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newPartialRunHarness(t, 3, 0) // every test passes
	h.replayer.config.Test.Mocking = true
	tls := models.CaptureLosses{{Kind: models.CaptureLossParserStall, Count: 2, What: "to 10.0.0.9:5432"}}
	// The mark, then one read as each test ends, then the set end's re-mark.
	instr := &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{nil, nil, tls}}
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
		if len(r.FailureInfo.CaptureIncomplete) != 1 || r.FailureInfo.CaptureIncomplete[0] != tls[0] {
			t.Fatalf("%s: capture evidence %+v, want %+v", id, r.FailureInfo.CaptureIncomplete, tls)
		}
		found := false
		for _, c := range r.FailureInfo.Category {
			found = found || c == models.CaptureIncompleteCategory
		}
		if !found {
			t.Fatalf("%s: categories %v lack %s", id, r.FailureInfo.Category, models.CaptureIncompleteCategory)
		}
	}
	// The next test set counts from the end of this one.
	if instr.reads != 5 || len(h.replayer.lossMark.at) != 1 {
		t.Fatalf("%d reads, mark %+v; want the mark, three tests and the end's re-mark, the mark at the loss", instr.reads, h.replayer.lossMark)
	}
}

// Under the proven switch a client-side TLS handshake that failed lost that
// one call: the test it happened in is FAILED with CAPTURE_INCOMPLETE, and the
// tests after it, whose calls were captured, are judged on their own. Every
// failed handshake used to taint every later test of the set: a false red.
func TestUnderTheSwitchATLSRejectionFailsOnlyTheTestItHappenedIn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newPartialRunHarness(t, 3, 0) // every test passes
	h.replayer.config.Test.Mocking = true
	tls := models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: 1, What: "to 10.0.0.9:443"}}
	// The mark, then one read as each test ends (the last repeating for
	// test-3 and the set end's re-mark): rejected while test-2 ran.
	instr := &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{nil, nil, tls}}
	h.replayer.instrumentation = instr
	h.replayer.markPIDMaps(context.Background())

	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("test set status %q, want FAILED: test-2 lost a call", status)
	}
	byID := map[string]models.TestResult{}
	for _, r := range h.report.report.Tests {
		byID[r.TestCaseID] = r
	}
	for _, id := range []string{"test-1", "test-3"} {
		if r := byID[id]; r.Status != models.TestStatusPassed || len(r.FailureInfo.CaptureIncomplete) != 0 {
			t.Fatalf("%s: %s %+v, want PASSED with no capture evidence: the handshake failed while test-2 ran", id, r.Status, r.FailureInfo.CaptureIncomplete)
		}
	}
	r := byID["test-2"]
	if r.Status != models.TestStatusFailed || len(r.FailureInfo.CaptureIncomplete) != 1 || r.FailureInfo.CaptureIncomplete[0] != tls[0] {
		t.Fatalf("test-2: %s %+v, want FAILED with %+v", r.Status, r.FailureInfo.CaptureIncomplete, tls)
	}
	if cv := h.report.report.CaptureVerification; cv != nil {
		t.Fatalf("capture_verification %+v: the set's tail lost nothing; test-2's handshake is test-2's", cv)
	}
	if instr.reads != 5 {
		t.Fatalf("%d reads; want the mark, three tests and the end's re-mark", instr.reads)
	}
}

// Under the proven switch a per-test read that fails leaves that test without
// a verdict, and its set unjudged, like a miss list not read whole; an agent
// that cannot count leaves the tests as they were.
func TestUnderTheSwitchAPerTestCaptureReadThatFailsLeavesTheSetUnjudged(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newPartialRunHarness(t, 3, 0)
	h.replayer.config.Test.Mocking = true
	instr := &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{nil}, errAt: map[int]error{3: errors.New("connection reset")}}
	h.replayer.instrumentation = instr
	h.replayer.markPIDMaps(context.Background())
	status := h.run(t)
	if status == models.TestSetStatusPassed || status == models.TestSetStatusFailed {
		t.Fatalf("status %s: a set with a test whose capture could not be told is neither PASSED nor FAILED", status)
	}
	var unread []string
	for _, r := range h.report.report.Tests {
		if r.FailureInfo.CaptureUnread {
			unread = append(unread, r.TestCaseID)
		}
		if r.FailureInfo.UnmatchedCallsUnread {
			t.Fatalf("%s is marked as having unread misses: its misses were read; its capture count was not", r.TestCaseID)
		}
	}
	if len(unread) != 1 || unread[0] != "test-2" {
		t.Fatalf("tests whose capture could not be told %v, want [test-2] (the read as it ended failed)", unread)
	}
	if reason := h.report.report.FailureReason; !strings.Contains(reason, "could not read whether its agent captured every call") {
		t.Fatalf("the set's reason %q does not say its capture could not be told", reason)
	}

	h = newPartialRunHarness(t, 3, 0)
	h.replayer.config.Test.Mocking = true
	instr = &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{nil}, errAt: map[int]error{1: fmt.Errorf("%w: agent returned 404", models.ErrCaptureLossesUnsupported)}}
	h.replayer.instrumentation = instr
	h.replayer.markPIDMaps(context.Background())
	if status := h.run(t); status != models.TestSetStatusPassed || instr.reads != 2 {
		t.Fatalf("an agent that cannot count: %s after %d reads; want PASSED, with only the mark and the end's re-mark read", status, instr.reads)
	}
}

// A loss before the test set's mark is not the set's: its tests pass. And a
// run that started no application (no -c, an agent already up) takes its mark
// as its first test set begins, not leaving every test unjudged.
func TestUnderTheSwitchTheTestSetCountsLossesFromItsMark(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	before := models.CaptureLosses{{Kind: models.CaptureLossParserStall, Count: 4, What: "to 10.0.0.1:5432"}}

	h := newPartialRunHarness(t, 3, 0)
	h.replayer.config.Test.Mocking = true
	h.replayer.instrumentation = &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{before}}
	h.replayer.markPIDMaps(context.Background())
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %q, want PASSED: the losses predate the set's mark", status)
	}

	// No mark taken before the set (keploy test without -c).
	h = newPartialRunHarness(t, 3, 0)
	h.replayer.config.Test.Mocking = true
	instr := &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{before, before, append(models.CaptureLosses(nil), models.CaptureLoss{Kind: models.CaptureLossParserStall, Count: 5, What: "to 10.0.0.1:5432"})}}
	h.replayer.instrumentation = instr
	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("no -c: status %q, want FAILED (test-2 and test-3 tainted), not unjudged", status)
	}
	for _, r := range h.report.report.Tests {
		if r.FailureInfo.CaptureUnread {
			t.Fatalf("no -c: %s left without a verdict: the set must take its own mark", r.TestCaseID)
		}
	}
}

// Under the proven switch, a loss after the set's last per-test read is the
// set's own: charged to none of its tests, it fails the set, and the report
// says what was lost. A set-end read that fails leaves the set unjudged, and
// the next set without a mark to count from.
func TestUnderTheSwitchALossAfterTheLastTestFailsTheSet(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	late := models.CaptureLosses{{Kind: models.CaptureLossParserStall, Count: 1, What: "to 10.0.0.7:5432"}}

	h := newPartialRunHarness(t, 3, 0)
	h.replayer.config.Test.Mocking = true
	// The mark, three per-test reads, then the set end's read.
	instr := &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{nil, nil, nil, nil, late}}
	h.replayer.instrumentation = instr
	h.replayer.markPIDMaps(context.Background())
	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("status %q, want FAILED: the set lost a call after its last test", status)
	}
	for _, r := range h.report.report.Tests {
		if r.Status != models.TestStatusPassed || len(r.FailureInfo.CaptureIncomplete) != 0 {
			t.Fatalf("%s: %s %+v; want PASSED with no capture evidence: the loss came after every test ended", r.TestCaseID, r.Status, r.FailureInfo.CaptureIncomplete)
		}
	}
	cv := h.report.report.CaptureVerification
	if cv == nil || cv.Status != models.CaptureIncompleteStatus || !strings.Contains(cv.Reason, late.Text()) {
		t.Fatalf("capture_verification %+v, want incomplete naming %q", cv, late.Text())
	}
	if instr.reads != 5 || !h.replayer.lossMark.known || len(h.replayer.lossMark.at) != 1 {
		t.Fatalf("%d reads, mark %+v; want five, the next set counting from the end's read", instr.reads, h.replayer.lossMark)
	}

	// The set end's read fails: what its tail lost cannot be told.
	h = newPartialRunHarness(t, 3, 0)
	h.replayer.config.Test.Mocking = true
	instr = &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{nil}, errAt: map[int]error{5: errors.New("connection reset")}}
	h.replayer.instrumentation = instr
	h.replayer.markPIDMaps(context.Background())
	status := h.run(t)
	if status == models.TestSetStatusPassed || status == models.TestSetStatusFailed {
		t.Fatalf("status %s, want neither PASSED nor FAILED: the set's tail could not be read", status)
	}
	if !h.replayer.notJudged("test-set-0") {
		t.Fatal("the set is not marked not judged")
	}
	if cv := h.report.report.CaptureVerification; cv == nil || cv.Status != models.CaptureUnverifiedStatus {
		t.Fatalf("capture_verification %+v, want unverified", cv)
	}
	if h.replayer.lossMark.known || h.replayer.lossMark.err == nil {
		t.Fatalf("mark %+v after a failed end read; want unknown, with the error, for the next set", h.replayer.lossMark)
	}
}

// Under the proven switch a run neither prunes mocks nor writes mappings.yaml
// (removeUnusedMocks is ignored and only record writes mappings, S12), lost
// calls or not: a call keploy lost while a test ran, answered by no mock,
// can never cut a recording down.
func TestUnderTheSwitchALossWhileATestRanHoldsThePruneAndMappings(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	session := []models.MockState{{Name: "mock-session", Kind: models.HTTP, Lifetime: models.LifetimeSession}}
	tls := models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: 1, What: "to 10.0.0.9:443"}}
	for _, tc := range []struct {
		name    string
		answers []models.CaptureLosses
	}{
		// The mark, then one read as each test ends: lost while test-2 ran.
		{name: "lost while a test ran", answers: []models.CaptureLosses{nil, nil, tls}},
		{name: "nothing lost", answers: []models.CaptureLosses{nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 4, 0)
			h.replayer.config.Test.Mocking = true
			h.replayer.hookImpl = prHooks{consumed: session}
			h.replayer.config.Test.RemoveUnusedMocks = true
			h.replayer.instrumentation = &prLossInstr{prInstr: h.instr, answers: tc.answers}
			h.replayer.markPIDMaps(context.Background())

			_ = h.run(t)

			if got := h.mocks.pruneCalls(); got != 0 {
				t.Fatalf("the run made %d UpdateMocks calls, want none", got)
			}
			if got := h.mappings.insertCalls(); got != 0 {
				t.Fatalf("the run wrote mappings.yaml %d times, want none", got)
			}
		})
	}
}

// A run with no agent to ask (--base-path) captures nothing of its own: under
// the switch its tests are not counted against a capture-loss count it never
// had, and its sets are judged as before.
func TestUnderTheSwitchABasePathRunReadsNoCaptureLosses(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newPartialRunHarness(t, 3, 0)
	h.replayer.config.Test.Mocking = true
	h.replayer.noAgent = true
	instr := &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{nil}, errAt: map[int]error{1: errors.New("connection refused"), 2: errors.New("connection refused")}}
	h.replayer.instrumentation = instr
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("a base-path run: status %q, want PASSED", status)
	}
	if instr.reads != 0 {
		t.Fatalf("a base-path run read the agent's capture losses %d time(s), want none", instr.reads)
	}
}

// The set's tail charges what was lost after its last test, but not the
// PID-map refusals: the set's PID-map check reports those, once.
func TestTheSetsTailLeavesPIDMapRefusalsToThePIDMapCheck(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newPartialRunHarness(t, 1, 0)
	h.replayer.config.Test.Mocking = true
	h.replayer.instrumentation = &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{{
		{Kind: models.CaptureLossPIDMap, Count: 2}, {Kind: models.CaptureLossParserStall, Count: 1},
	}}}
	h.replayer.lossMark, h.replayer.lossMarked = captureLossMark{known: true}, true
	late, err := h.replayer.endCaptureLosses(context.Background())
	if err != nil || len(late) != 1 || late[0].Kind != models.CaptureLossParserStall {
		t.Fatalf("the tail's losses %+v (%v), want the parser stall only", late, err)
	}
}

// Decision 20 under the proven switch: a call keploy cannot capture at all (a
// Unix socket) leaves the test open at the time, and every later one, without
// a verdict, never FAILED; the set is not judged. Opened at the
// application's startup, it leaves every test unjudged; opened mid-run, the
// tests before it keep their verdicts.
func TestUnderTheSwitchACallKeployCannotCaptureLeavesTestsNotJudged(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	db := models.CaptureLosses{{Kind: models.CaptureLossUnixSocket, Count: 1, What: "/var/run/postgresql/.s.PGSQL.5432"}}
	for _, tc := range []struct {
		name    string
		answers []models.CaptureLosses // the mark, then a read as each test ends, then the set's end
		judged  []string
	}{
		{name: "at startup", answers: []models.CaptureLosses{nil, db}, judged: nil},
		{name: "mid-run", answers: []models.CaptureLosses{nil, nil, db}, judged: []string{"test-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 3, 0)
			h.replayer.config.Test.Mocking = true
			h.replayer.instrumentation = &prLossInstr{prInstr: h.instr, answers: tc.answers}
			h.replayer.markPIDMaps(context.Background())
			if status := h.run(t); status != models.TestSetStatusInternalErr || !h.replayer.notJudged("test-set-0") {
				t.Fatalf("status %q, not judged %v; want %q and not judged", status, h.replayer.notJudged("test-set-0"), models.TestSetStatusInternalErr)
			}
			var judged []string
			for _, r := range h.report.report.Tests {
				if r.Status == models.TestStatusFailed {
					t.Fatalf("%s FAILED: a call keploy cannot capture never fails a test", r.TestCaseID)
				}
				if len(r.FailureInfo.CannotCapture) == 0 {
					judged = append(judged, r.TestCaseID)
				} else if r.FailureInfo.CannotCapture[0].What != db[0].What || len(r.FailureInfo.CaptureIncomplete) != 0 {
					t.Fatalf("%s: not judged %+v, capture incomplete %+v", r.TestCaseID, r.FailureInfo.CannotCapture, r.FailureInfo.CaptureIncomplete)
				}
			}
			if fmt.Sprint(judged) != fmt.Sprint(tc.judged) {
				t.Fatalf("judged tests %v, want %v", judged, tc.judged)
			}
			if reason := h.report.report.FailureReason; !strings.Contains(reason, "keploy cannot capture") || !strings.Contains(reason, "/var/run/postgresql/.s.PGSQL.5432") {
				t.Fatalf("the set's reason %q does not say what keploy cannot capture", reason)
			}
		})
	}

	// A later test set on the same agent counts it from the run's first
	// mark: the connection outlives the set it was opened in.
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.config.Test.Mocking = true
	h.replayer.instrumentation = &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{db}}
	h.replayer.lossRunBase, h.replayer.lossRunBaseSet = nil, true // the run began before the connect
	h.replayer.markPIDMaps(context.Background())                  // this set's mark is after it
	if status := h.run(t); status != models.TestSetStatusInternalErr {
		t.Fatalf("a later test set: status %q, want not judged: the connect before its mark still serves its tests", status)
	}
}

// An agent already up (no -c): the application is up before the run, so a
// Unix-socket connection it opened at startup is in the counts already when
// the run's first test set takes the run's first mark. What keploy cannot
// capture at all is counted from the hooks' load, as the agent counts it, so
// every test is not judged; counted from that mark, every test would be
// judged on calls keploy never saw.
func TestAnAgentUpRunCountsAUnixConnectionOpenedBeforeIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	db := models.CaptureLosses{{Kind: models.CaptureLossUnixSocket, Count: 1, What: "/var/run/postgresql/.s.PGSQL.5432"}}
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.config.Test.Mocking = true
	h.replayer.instrumentation = &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{db}}
	// No mark before the set: RunTestSet takes the run's first.
	if status := h.run(t); status != models.TestSetStatusInternalErr || !h.replayer.notJudged("test-set-0") {
		t.Fatalf("status %q, not judged %v; want %q and not judged: the connection the application opened before the run serves its tests", status, h.replayer.notJudged("test-set-0"), models.TestSetStatusInternalErr)
	}
	for _, r := range h.report.report.Tests {
		if r.Status == models.TestStatusFailed || len(r.FailureInfo.CannotCapture) == 0 {
			t.Fatalf("%s: status %s, not judged %+v; want not judged, never failed", r.TestCaseID, r.Status, r.FailureInfo.CannotCapture)
		}
	}
}
