package replay

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
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
