package replay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap/zapcore"

	"go.keploy.io/server/v3/pkg/models"
	setfile "go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// setEndRecorder is the run's own hooks with SetEndHook: what a hook that
// judges the run (enterprise's keploy test verdict) is told of each set.
type setEndRecorder struct {
	TestHooks
	mu   sync.Mutex
	ends []setEndCall
}

type setEndCall struct {
	set string
	end models.SetEnd
}

func (h *setEndRecorder) SetEnd(testSetID string, end models.SetEnd) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ends = append(h.ends, setEndCall{testSetID, end})
}

func (h *setEndRecorder) told() []setEndCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.ends)
}

// recordSetEnds puts a SetEndHook over the run's hooks.
func recordSetEnds(run *bpRun) *setEndRecorder {
	h := &setEndRecorder{TestHooks: run.replayer.GetTestHooks()}
	run.replayer.SetTestHooks(h)
	return h
}

// A hook that judges the run from its tests' results alone cannot see how
// keploy ended a set: a set that stopped part-way after some tests passed
// (keploy's INTERNAL_ERR here; APP_HALTED when the app exits) has only
// passing results, and was read as passed. SetEndHook tells it, once per run
// of each set, the status the set's report has and why: the report's own
// reason.
func TestEachTestSetsEndIsToldToTheHooks(t *testing.T) {
	t.Run("passed", func(t *testing.T) {
		app := newBPApp(t)
		run := newBasePathRun(t, app.URL, "test-1", "test-2")
		h := recordSetEnds(run)

		if _, err := run.start(t); err != nil {
			t.Fatalf("Start: %v", err)
		}
		want := []setEndCall{{"test-set-0", models.SetEnd{Status: models.TestSetStatusPassed}}}
		if got := h.told(); !slices.Equal(got, want) {
			t.Fatalf("told %+v, want %+v", got, want)
		}
	})

	t.Run("failed", func(t *testing.T) {
		app := newBPApp(t)
		app.wrong["/test-2"] = true
		run := newBasePathRun(t, app.URL, "test-1", "test-2")
		h := recordSetEnds(run)

		if _, err := run.start(t); err != nil {
			t.Fatalf("Start: %v", err)
		}
		rep := run.report.report
		want := []setEndCall{{"test-set-0", models.SetEnd{Status: models.TestSetStatusFailed, Why: rep.FailureReason}}}
		if got := h.told(); !slices.Equal(got, want) {
			t.Fatalf("told %+v, want %+v", got, want)
		}
	})

	t.Run("stopped part-way after a pass", func(t *testing.T) {
		app := newBPApp(t)
		app.hold["/test-2"] = true
		run := newBasePathRun(t, app.URL, "test-1", "test-2", "test-3")
		h := recordSetEnds(run)
		root, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		go func() {
			<-app.held
			cancel(errors.New("the CI job was cancelled"))
		}()

		code, _ := run.startIn(t, root)

		rep := run.report.report
		if rep == nil || rep.Status != string(models.TestSetStatusInternalErr) || rep.Success != 1 {
			t.Fatalf("report %+v: want INTERNAL_ERR with 1 passed", rep)
		}
		if code != 1 {
			t.Fatalf("exited %d, want 1", code)
		}
		got := h.told()
		want := []setEndCall{{"test-set-0", models.SetEnd{Status: models.TestSetStatusInternalErr, Why: rep.FailureReason}}}
		if !slices.Equal(got, want) || got[0].end.Why == "" {
			t.Fatalf("told %+v, want %+v with the report's reason", got, want)
		}
	})

	t.Run("each run of a set it runs again", func(t *testing.T) {
		app := newBPApp(t)
		run := newBasePathRun(t, app.URL, "test-1")
		run.replayer.config.Test.MaxFlakyChecks = 2
		h := recordSetEnds(run)

		if _, err := run.start(t); err != nil {
			t.Fatalf("Start: %v", err)
		}
		passed := setEndCall{"test-set-0", models.SetEnd{Status: models.TestSetStatusPassed}}
		if got, want := h.told(), []setEndCall{passed, passed}; !slices.Equal(got, want) {
			t.Fatalf("told %+v, want %+v", got, want)
		}
	})

	t.Run("a set refused when it loaded", func(t *testing.T) {
		app := newBPApp(t)
		run := newBasePathRun(t, app.URL, "test-1")
		run.replayer.testDB = interruptedOnce{bpTestDB: run.tests, calls: new(int)}
		h := recordSetEnds(run)

		if _, err := run.start(t); err != nil {
			t.Fatalf("Start: %v", err)
		}
		rep := run.report.report
		if rep == nil || rep.FailureReason == "" {
			t.Fatalf("report %+v: want the refused set's report, with its reason", rep)
		}
		want := []setEndCall{{"test-set-0", models.SetEnd{Status: models.TestSetStatusFailed, Why: rep.FailureReason}}}
		if got := h.told(); !slices.Equal(got, want) {
			t.Fatalf("told %+v, want %+v", got, want)
		}
	})

	t.Run("a run of a set is never told the reason of the run before it", func(t *testing.T) {
		// The first run of the set is refused, with its reason; the second
		// fails, with the same status, before any report: its own error.
		app := newBPApp(t)
		run := newBasePathRun(t, app.URL, "test-1")
		run.replayer.config.Test.MaxFlakyChecks = 2
		run.replayer.testDB = interruptedOnce{bpTestDB: run.tests, calls: new(int)}
		h := recordSetEnds(run)

		_, _ = run.start(t)

		got := h.told()
		if len(got) != 2 {
			t.Fatalf("told %+v, want both runs", got)
		}
		if got[0].end.Status != models.TestSetStatusFailed || !strings.Contains(got[0].end.Why, "templatize") {
			t.Fatalf("first run told %+v, want FAILED with the refusal", got[0].end)
		}
		if got[1].end.Status != models.TestSetStatusFailed || !strings.Contains(got[1].end.Why, "permission denied") {
			t.Fatalf("second run told %+v, want FAILED with its own error", got[1].end)
		}
	})

	t.Run("a set whose last report cannot be written", func(t *testing.T) {
		app := newBPApp(t)
		run := newBasePathRun(t, app.URL, "test-1")
		run.replayer.reportDB = &refuseSecondReport{bpReportDB: run.report}
		h := recordSetEnds(run)

		_, _ = run.start(t)

		want := []setEndCall{{"test-set-0", models.SetEnd{Status: models.TestSetStatusInternalErr, Why: "failed to insert report"}}}
		if got := h.told(); !slices.Equal(got, want) {
			t.Fatalf("told %+v, want %+v: the report it could not write said PASSED", got, want)
		}
	})

	t.Run("a set that ends on an error before its report", func(t *testing.T) {
		app := newBPApp(t)
		run := newBasePathRun(t, app.URL, "test-1")
		run.report.refuseReport = true
		h := recordSetEnds(run)

		if _, err := run.start(t); err == nil {
			t.Fatal("Start returned nil for a set that could not run")
		}
		got := h.told()
		if len(got) != 1 || got[0].set != "test-set-0" || got[0].end.Status == models.TestSetStatusPassed || got[0].end.Why == "" {
			t.Fatalf("told %+v, want one end that is not PASSED and says why", got)
		}
	})
}

// interruptedOnce is a base-path run's test store whose first check of the
// set finds what an interrupted keploy templatize left (the set is refused,
// with that as its reason), and whose later checks cannot read the set.
type interruptedOnce struct {
	*bpTestDB
	calls *int
}

func (s interruptedOnce) Interrupted(context.Context, string) error {
	*s.calls++
	if *s.calls == 1 {
		return fmt.Errorf("%w: test-set-0 holds test-1.yaml.tmp: finish or undo the keploy templatize that wrote it", setfile.ErrReplaceInterrupted)
	}
	return errors.New("open keploy/test-set-0: permission denied")
}

// refuseSecondReport is a base-path run's report store that cannot write the
// second report of a run: the set's last, after its first, RUNNING one.
type refuseSecondReport struct {
	*bpReportDB
	n int
}

func (f *refuseSecondReport) InsertReport(ctx context.Context, runID, setID string, rep *models.TestReport) error {
	f.n++
	if f.n == 2 {
		return errors.New("disk full")
	}
	return f.bpReportDB.InsertReport(ctx, runID, setID, rep)
}

// uncheckedHooks is the run's own hooks with UncheckedHook: a hook that could
// not read what a set's tests read (enterprise's decryption of a set's
// sealed values, when its key service stopped answering) says why. It takes
// SetEndHook too, to see what the hooks are told of the set's end.
type uncheckedHooks struct {
	TestHooks
	why  string
	asks []string
	ends []models.SetEnd
}

func (h *uncheckedHooks) Unchecked(testSetID string) string {
	h.asks = append(h.asks, testSetID)
	return h.why
}

func (h *uncheckedHooks) SetEnd(_ string, end models.SetEnd) {
	h.ends = append(h.ends, end)
}

// uncheckedWhy is what the hooks of the tests below say.
const uncheckedWhy = "the key service at https://kms.example stopped answering; 3 sealed values were served sealed"

// A hook that changes what a set reads could not tell keploy that some of the
// set's tests could not be checked: its sealed values were served sealed, a
// test that read one failed as a regression, and one that did not passed, so
// a set the run could not check read as the application's fault, or green.
// Under the proven switch the set is not judged with the hook's reason, and
// the round goes on (markNotJudged); with it off, the report says the reason
// first and keeps the run's status, and the run's log says so. The hooks are
// told the report's reason. A set run again is asked again, once per run.
func TestASetAHookCouldNotCheckSaysWhy(t *testing.T) {
	const why = uncheckedWhy
	for _, c := range []struct {
		name     string
		proven   bool
		wrong    bool
		runs     int
		status   models.TestSetStatus
		notJudge bool
	}{
		{"proven, the tests passed", true, false, 1, models.TestSetStatusInternalErr, true},
		{"proven, a test failed", true, true, 1, models.TestSetStatusInternalErr, true},
		{"proven, run again", true, false, 2, models.TestSetStatusInternalErr, true},
		{"switch off, the tests passed", false, false, 1, models.TestSetStatusPassed, false},
		{"switch off, a test failed", false, true, 1, models.TestSetStatusFailed, false},
		{"switch off, run again", false, false, 2, models.TestSetStatusPassed, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.proven {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			app := newBPApp(t)
			if c.wrong {
				app.wrong["/test-2"] = true
			}
			run := newBasePathRun(t, app.URL, "test-1", "test-2")
			run.replayer.config.Test.MaxFlakyChecks = uint32(c.runs)
			h := &uncheckedHooks{TestHooks: run.replayer.GetTestHooks(), why: why}
			run.replayer.SetTestHooks(h)

			_, _ = run.start(t)

			rep := run.report.report
			if rep == nil {
				t.Fatal("no report")
			}
			if got := models.TestSetStatus(rep.Status); got != c.status {
				t.Errorf("status %s, want %s", got, c.status)
			}
			if !strings.Contains(rep.FailureReason, why) {
				t.Errorf("failure reason %q, want it to say %q", rep.FailureReason, why)
			}
			if c.notJudge && !strings.Contains(rep.FailureReason, "not judged: "+why) {
				t.Errorf("failure reason %q, want the set not judged for it", rep.FailureReason)
			}
			if !c.notJudge && !strings.HasPrefix(rep.FailureReason, "keploy could not check every test: "+why) {
				t.Errorf("failure reason %q, want it to give the reason first", rep.FailureReason)
			}
			if got := run.replayer.notJudged("test-set-0"); got != c.notJudge {
				t.Errorf("marked not judged %v, want %v: a set not judged goes on to the next set, any other INTERNAL_ERR ends the round", got, c.notJudge)
			}
			if want := slices.Repeat([]string{"test-set-0"}, c.runs); !slices.Equal(h.asks, want) {
				t.Errorf("asked %v, want %v: once per run of the set", h.asks, want)
			}
			if len(h.ends) != c.runs || h.ends[len(h.ends)-1].Why != rep.FailureReason {
				t.Errorf("hooks told %+v, want %d ends, the last with the report's reason", h.ends, c.runs)
			}
			level, msg := zapcore.WarnLevel, "keploy could not check every test of the test-set"
			if c.proven {
				level, msg = zapcore.ErrorLevel, "test-set is not judged: keploy could not read all its tests read"
			}
			if said := run.said(level, msg); len(said) != c.runs || said[0].ContextMap()["reason"] != why {
				t.Errorf("the run said %+v at %s, want %d line(s) %q with the reason", said, level, c.runs, msg)
			}
		})
	}
}

// Under the proven switch a set whose application never got ready is not
// judged, saying so first: a hook's reason is one more reason it is not
// judged, after that one, never put ahead of it as a set with a status of
// its own has it.
func TestASetNeverReadyAHookCouldNotCheckIsNotJudgedForBoth(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	wake := manualWake(t)
	stallLimit(t, 1)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	app := newBPApp(t)
	run := newBasePathRun(t, app.URL, "test-1")
	run.replayer.config.Test.HealthURL = down.URL + "/up"
	run.replayer.SetTestHooks(&uncheckedHooks{TestHooks: run.replayer.GetTestHooks(), why: uncheckedWhy})
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case wake <- struct{}{}:
			case <-done:
				return
			}
		}
	}()

	_, _ = run.start(t)

	rep := run.report.report
	if rep == nil || rep.Status != string(models.TestSetStatusInternalErr) || !strings.HasPrefix(rep.FailureReason, "not judged: the application never answered") {
		t.Fatalf("report %+v, want INTERNAL_ERR saying first that the application never got ready", rep)
	}
	if !strings.Contains(rep.FailureReason, "; not judged: "+uncheckedWhy) || strings.Contains(rep.FailureReason, "keploy could not check every test") {
		t.Fatalf("failure reason %q, want the hook's reason as one more reason the set is not judged", rep.FailureReason)
	}
}

// uncheckedPRHooks is the RunTestSet harness's hooks with UncheckedHook.
type uncheckedPRHooks struct {
	prHooks
	why string
}

func (h uncheckedPRHooks) Unchecked(string) string { return h.why }

// A run a hook could not check is no basis for writing anything back to the
// recording, with the switch on or off: a mock it served sealed matched no
// call, so it is consumed by no test. The prune would delete it
// (--remove-unused-mocks, PreserveFailedMocks off: what k8s-proxy
// auto-replay sets), a new mappings.yaml would leave it out of its test's
// entry for good, and record's full round would keep what it learned on it.
func TestASetAHookCouldNotCheckWritesNothingBack(t *testing.T) {
	session := []models.MockState{{Name: "mock-session", Kind: models.HTTP, Lifetime: models.LifetimeSession}}
	t.Run("no prune", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "")
		h := newPartialRunHarness(t, 4, 0)
		h.replayer.config.Test.RemoveUnusedMocks = true
		h.replayer.hookImpl = uncheckedPRHooks{why: uncheckedWhy}
		if status := h.run(t); status != models.TestSetStatusPassed {
			t.Fatalf("status %s, want PASSED: with the switch off the set keeps its status", status)
		}
		if got := h.mocks.pruneCalls(); got != 0 {
			t.Fatalf("UpdateMocks calls = %d, want 0", got)
		}
	})
	t.Run("no mappings.yaml", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "")
		for _, flag := range []bool{false, true} {
			h := newPartialRunHarness(t, 4, 0)
			h.replayer.config.Test.UpdateTestMapping = flag
			h.replayer.hookImpl = uncheckedPRHooks{prHooks: prHooks{consumed: session}, why: uncheckedWhy}
			_ = h.run(t)
			if got := h.mappings.insertCalls(); got != 0 {
				t.Fatalf("--update-test-mapping=%v: mappings.yaml writes = %d, want 0", flag, got)
			}
		}
	})
	t.Run("nothing learned kept", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		h := newPartialRunHarness(t, 2, 0)
		h.replayer.config.Test.Learn = true
		store := &learnedStore{prMockDB: h.mocks}
		h.replayer.mockDB = store
		h.replayer.hookImpl = uncheckedPRHooks{prHooks: prHooks{consumed: session}, why: uncheckedWhy}
		if status := h.run(t); status != models.TestSetStatusInternalErr {
			t.Fatalf("status %s, want INTERNAL_ERR: not judged", status)
		}
		if store.persisted != nil {
			t.Fatalf("the learn step kept %v from a round that could not read its recordings", store.persisted)
		}
	})
}

// A hook that says nothing changes no report, and holds nothing back.
func TestASetAHookCheckedKeepsItsReport(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	app := newBPApp(t)
	run := newBasePathRun(t, app.URL, "test-1")
	run.replayer.SetTestHooks(&uncheckedHooks{TestHooks: run.replayer.GetTestHooks()})

	if _, err := run.start(t); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if rep := run.report.report; rep == nil || rep.Status != string(models.TestSetStatusPassed) || rep.FailureReason != "" {
		t.Fatalf("report %+v, want PASSED with no reason", rep)
	}
	if run.replayer.notJudged("test-set-0") {
		t.Fatal("the set is marked not judged")
	}

	t.Setenv(provenswitch.Env, "")
	h := newPartialRunHarness(t, 4, 0)
	h.replayer.config.Test.RemoveUnusedMocks = true
	h.replayer.hookImpl = uncheckedPRHooks{why: " \n"}
	if status := h.run(t); status != models.TestSetStatusPassed || h.report.report.FailureReason != "" {
		t.Fatalf("status %s, reason %q: want PASSED with no reason for a hook that says only blanks", status, h.report.report.FailureReason)
	}
	if got := h.mocks.pruneCalls(); got != 1 {
		t.Fatalf("UpdateMocks calls = %d, want 1: a hook that says nothing holds no prune", got)
	}
}

// setCallsRecorder is the run's own hooks with SetCallsHook: what record's
// judging is told of the calls each set's end charged to the set.
type setCallsRecorder struct {
	TestHooks
	mu   sync.Mutex
	told [][]models.UnmatchedCall
}

func (h *setCallsRecorder) SetCalls(_ string, calls []models.UnmatchedCall) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.told = append(h.told, calls)
}

func (h *setCallsRecorder) last() []models.UnmatchedCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.told[len(h.told)-1]
}

// Under the proven switch the calls the set's end charged to the set, made
// before its first test and after its last, are told to a SetCallsHook with
// the set's end, each as its parser reported it, so record's judging can read
// one that is a recording made elsewhere (a2). A set's end whose report
// does not say the status it ended with tells none.
func TestTheCallsChargedToTheSetAreToldToTheHooks(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	before := func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "DELETE /msg", Destination: "sqs:4566",
			Kind: models.MissOutsideTest, BeforeSet: true, ClosestMock: "mock-6", NotOwned: true}}, nil
	}
	h.instr.mockErrorsFor = misses(before, nil, afterLast)
	rec := &setCallsRecorder{TestHooks: h.replayer.GetTestHooks()}
	h.replayer.SetTestHooks(rec)

	status := h.run(t)
	h.replayer.tellSetEnd("test-set-0", status, nil)
	got := rec.last()
	// The delete made before the set, which reached no test, is background
	// work: listed for the set, not charged to it.
	if len(got) != 1 || got[0].ActualSummary != "POST /publish" {
		t.Fatalf("told %+v; want the publish read at the set's end alone", got)
	}
	if bg := h.report.report.BackgroundCalls; len(bg) != 1 || bg[0].ClosestMock != "mock-6" || !bg[0].NotOwned || !bg[0].BeforeSet {
		t.Fatalf("background calls %+v; want the delete made before the set, named as mock-6", bg)
	}

	h.replayer.tellSetEnd("test-set-0", models.TestSetStatusInternalErr, errors.New("the report could not be written"))
	if got := rec.last(); got != nil {
		t.Fatalf("a set's end its report does not say told %+v", got)
	}
}
