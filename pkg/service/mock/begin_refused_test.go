package mock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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

// With the switch off the per-test windows carry the begins keploy refused,
// so a replay that could not read them cannot know of one, and fails: an
// ERROR naming the failed read and that keploy cannot tell, a passing test
// command's exit becomes 1 (a failing one keeps its code), and the receipt
// says keploy did not complete the run. From the agent and from the account
// a docker compose agent left as it stopped alike.
func TestAReplayThatCannotReadItsWindowsFailsWithTheSwitchOff(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	const read = "keploy could not read what each test did from its agent: "
	check := func(t *testing.T, dir string, code int, logs *observer.ObservedLogs, wantCode, wantRunner int, why string) {
		t.Helper()
		if code != wantCode {
			t.Fatalf("exit %d, want %d", code, wantCode)
		}
		r := mustReceipt(t, dir)
		if r.FailedBy != FailedByKeploy || r.RunnerExitCode != wantRunner || r.Error != read+why || r.Isolated {
			t.Fatalf("receipt failedBy %q, runnerExitCode %d, error %q, isolated %v; want %q, %d, %q, false", r.FailedBy, r.RunnerExitCode, r.Error, r.Isolated, FailedByKeploy, wantRunner, read+why)
		}
		var named int
		for _, e := range logs.FilterMessage("replay failed: keploy could not read what each test did from its agent, so it cannot tell whether it refused a test at its begin").All() {
			if e.Level == zapcore.ErrorLevel && e.ContextMap()["error"] == read+why {
				named++
			}
		}
		if named != 1 {
			t.Fatalf("%d ERROR lines name the failed read, want 1", named)
		}
	}
	for _, tc := range []struct {
		name         string
		exit         models.AppError
		code, runner int
	}{
		{name: "the test command passes", code: 1},
		{name: "the test command fails", exit: models.AppError{AppErrorType: models.ErrCommandError, ExitCode: 3}, code: 3, runner: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, code, logs := replayIn(t, replayCase{runExit: tc.exit, keployFails: true, windowsErr: errors.New("connection reset by peer")})
			check(t, dir, code, logs, tc.code, tc.runner, "connection reset by peer")
		})
	}
	// When the agent died under the test command, that is why the read
	// failed, and the run fails as keploy's own on it, after it reports what
	// the agent served and missed, as it did before.
	t.Run("the agent died", func(t *testing.T) {
		died := models.AppError{AppErrorType: models.ErrInternal, Err: errors.New("the keploy agent exited: signal: killed")}
		dir, code, logs := replayIn(t, replayCase{runExit: died, keployFails: true, windowsErr: context.Canceled})
		if code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		if r := mustReceipt(t, dir); r.FailedBy != FailedByKeploy || r.Error != died.Err.Error() {
			t.Fatalf("receipt failedBy %q, error %q; want %q, %q", r.FailedBy, r.Error, FailedByKeploy, died.Err.Error())
		}
		if logs.FilterMessageSnippet("mock replay summary").Len() == 0 {
			t.Fatal("the replay did not report what the agent served and missed")
		}
		if n := logs.FilterMessageSnippet("cannot tell whether it refused a test").Len(); n != 0 {
			t.Fatalf("%d lines blame the read for a run whose agent died", n)
		}
	})
	t.Run("docker compose", func(t *testing.T) {
		const why = "keploy did not read the replay outcome the keploy-agent container left"
		core, logs := observer.New(zapcore.DebugLevel)
		instr := newInstr(t, agentUpAfterRun, false, models.AppError{AppErrorType: models.ErrAppStopped})
		instr.runUntilReady = true
		instr.consumedErr = errors.New("connection refused")
		instr.mockErrorsErr = errors.New("connection refused")
		instr.leftOutcomeErr = errors.New(why)
		cfg := instrConfig(instr, utils.DockerCompose, "docker compose up")
		cfg.Mock.OnMiss = string(models.MissFail)
		cfg.Path = t.TempDir()
		if err := os.MkdirAll(filepath.Join(cfg.Path, "set"), 0o755); err != nil {
			t.Fatal(err)
		}
		utils.ErrCode = 0
		t.Cleanup(func() { utils.ErrCode = 0 })
		err := New(zap.New(core), instr, stubMockDB{}, nil, nil, nil, cfg).Replay(context.Background())
		if err == nil || err.Error() != "keploy did not complete the run: "+read+why {
			t.Fatalf("Replay returned %v, want keploy's failure naming the read", err)
		}
		check(t, cfg.Path, utils.ErrCode, logs, 1, 0, why)
	})
}
