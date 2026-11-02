package mock

import (
	"errors"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zapcore"
)

// A test keploy refused at its begin fails the replay whatever the test
// command exits: a harness that logs the refusal and runs the test anyway
// (keploy's documented Go and JS snippets do) exits 0, and was served what
// the refusal could not narrow. The replay says so at ERROR, naming the test
// and the refusal. A failing test command keeps its own exit code, and a
// failure keploy already named keeps its name.
func TestATestKeployRefusedAtItsBeginFailsTheReplay(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	at := time.Now()
	const why = "t1 began with the pid 4242: refused"
	refused := models.ScopeWindow{Name: "t1", Start: at, End: at, PID: 4242, Outcome: models.ScopeOutcomeFailed, BeginRefused: why}
	ran := models.ScopeWindow{Name: "t2", Start: at, End: at.Add(time.Second), Outcome: models.ScopeOutcomePassed}
	for _, tc := range []struct {
		name     string
		exit     models.AppError
		keploy   bool
		code     int
		failedBy string
	}{
		{name: "the test command passes", code: 1, failedBy: FailedByBeginRefused},
		{name: "the test command fails", exit: models.AppError{AppErrorType: models.ErrCommandError, ExitCode: 3}, code: 3, failedBy: FailedByBeginRefused},
		{name: "keploy fails", exit: models.AppError{AppErrorType: models.ErrInternal, Err: errors.New("the app runner panicked")}, keploy: true, code: 1, failedBy: FailedByKeploy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, code, logs := replayIn(t, replayCase{runExit: tc.exit, keployFails: tc.keploy, windows: []models.ScopeWindow{ran, refused}})
			if code != tc.code {
				t.Fatalf("exit %d, want %d", code, tc.code)
			}
			if r := mustReceipt(t, dir); r.FailedBy != tc.failedBy {
				t.Fatalf("receipt failedBy %q, want %q", r.FailedBy, tc.failedBy)
			}
			var named int
			for _, e := range logs.FilterMessageSnippet("keploy refused a test at its begin").All() {
				f := e.ContextMap()
				if e.Level == zapcore.ErrorLevel && f["test"] == "t1" && f["reason"] == why && f["pid"] == uint32(4242) {
					named++
				}
			}
			if named != 1 {
				t.Fatalf("%d ERROR lines name t1 and its refusal, want 1", named)
			}
		})
	}
	// A test its harness reported failed, which keploy did not refuse, is
	// the test command's to fail.
	failed := models.ScopeWindow{Name: "t1", Start: at, End: at.Add(time.Second), Outcome: models.ScopeOutcomeFailed}
	dir, code, logs := replayIn(t, replayCase{windows: []models.ScopeWindow{ran, failed}})
	if code != 0 {
		t.Fatalf("a run with no refused test exited %d, want the test command's 0", code)
	}
	if r := mustReceipt(t, dir); r.FailedBy != "" {
		t.Fatalf("a run with no refused test failed by %q", r.FailedBy)
	}
	if n := logs.FilterMessageSnippet("keploy refused a test at its begin").Len(); n != 0 {
		t.Fatalf("a run with no refused test logged %d refusals", n)
	}
}
