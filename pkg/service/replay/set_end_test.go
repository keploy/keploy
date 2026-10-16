package replay

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	setfile "go.keploy.io/server/v3/pkg/platform/yaml"
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
