package mock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The replay outcome is read from the agent AFTER the runner exits. Under
// docker compose the runner exiting is what stops the whole project, agent
// included, so those reads routinely fail — and a summary that reported the
// failure as "0 consumed, 0 missed" read as a clean replay and, under --strict,
// passed a run that could have missed every call.
func TestReplayOutcomeReporting(t *testing.T) {
	twoConsumed := []models.MockState{{Name: "mock-0"}, {Name: "mock-1"}}
	oneMiss := []models.UnmatchedCall{{Protocol: "http", Destination: "dep:9411"}}

	// The two reads are tracked apart, so each needs its own case: with one
	// flag for both, a half-answer is indistinguishable from no answer and a
	// run whose misses were never read is reported as clean.
	for _, tc := range []struct {
		name          string
		consumedMocks []models.MockState
		consumedErr   error
		mockErrors    []models.UnmatchedCall
		errorsErr     error
		wantExit      int
		wantErr       bool // Replay returns keploy's own failure, not just an exit code
		wantLogs      []string
		unwantLogs    []string
		wantFields    map[string]any
		wantMetered   *ReplayOutcome // nil: this run must not be metered at all
	}{
		{
			name:          "both reads land: the real counts, and --strict applies to them",
			consumedMocks: twoConsumed,
			wantExit:      0,
			wantLogs:      []string{"mock replay summary"},
			unwantLogs:    []string{"incomplete", "could not be proven"},
			wantFields:    map[string]any{"consumed": int64(2), "missed": int64(0)},
			wantMetered:   &ReplayOutcome{SetName: "set", Loaded: 0, Consumed: 2, Missed: 0},
		},
		{
			name:        "neither read lands: both counts unknown, and --strict refuses to pass what it could not verify",
			consumedErr: errors.New("connection refused"),
			errorsErr:   errors.New("connection refused"),
			wantExit:    1,
			wantErr:     true,
			wantLogs:    []string{"incomplete", "could not be proven"},
			wantFields:  map[string]any{"consumed": "unknown", "missed": "unknown"},
		},
		{
			// Only the consumed read failed. The miss list IS readable and
			// empty, so --strict was genuinely satisfied and must not fail the
			// run — but the summary is still incomplete.
			name:        "only the consumed read fails: --strict still applies",
			consumedErr: errors.New("connection refused"),
			wantExit:    0,
			wantLogs:    []string{"incomplete"},
			unwantLogs:  []string{"could not be proven"},
			wantFields:  map[string]any{"consumed": "unknown", "missed": int64(0)},
		},
		{
			// The mirror case, and the one a single flag hides: consumed reads
			// fine, misses do not. Reporting missed=0 here would pass --strict
			// on a run whose misses were never read.
			name:          "only the miss read fails: --strict refuses to pass",
			consumedMocks: twoConsumed,
			errorsErr:     errors.New("connection refused"),
			wantExit:      1,
			wantErr:       true,
			wantLogs:      []string{"incomplete", "could not be proven"},
			wantFields:    map[string]any{"consumed": int64(2), "missed": "unknown"},
		},
		{
			// A miss that WAS read still fails the run, whatever else went
			// unread — and the summary must report it as the number it is, not
			// as "unknown" alongside an error that names it.
			name:        "a read miss fails --strict even when the consumed read did not land",
			consumedErr: errors.New("connection refused"),
			mockErrors:  oneMiss,
			wantExit:    1,
			wantLogs:    []string{"incomplete", "replay failed under --strict"},
			unwantLogs:  []string{"could not be proven"},
			wantFields:  map[string]any{"consumed": "unknown", "missed": int64(1)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)

			instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
			instr.consumedMocks = tc.consumedMocks
			instr.consumedErr = tc.consumedErr
			instr.mockErrors = tc.mockErrors
			instr.mockErrorsErr = tc.errorsErr

			cfg := instrConfig(instr, utils.Native, "pytest -q")
			cfg.Mock.Strict = true

			// utils.ErrCode is process-global and only ever raised, so reset it
			// around each case rather than leaking one case's exit into the next.
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })

			// Metering is the half nobody sees. Under compose the outcome read
			// fails on essentially every run, so a reporter that fires anyway
			// records every one of them as zero mocks consumed — the same
			// false-clean as the summary, only silent and permanent.
			var metered []ReplayOutcome
			RegisterReplayOutcomeReporter(func(_ context.Context, o ReplayOutcome) {
				metered = append(metered, o)
			})
			t.Cleanup(func() { RegisterReplayOutcomeReporter(nil) })

			svc := New(zap.New(core), instr, stubMockDB{}, nil, nil, nil, cfg)
			// A run --strict could not verify is keploy's failure, and is
			// returned as one: a bare exit 1 is what a failing suite exits
			// with too, and a caller could not tell the two apart.
			err := svc.Replay(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("Replay returned %v, want an error: %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "--strict could not verify") {
				t.Fatalf("Replay returned %q, which does not say what failed", err)
			}

			if utils.ErrCode != tc.wantExit {
				t.Errorf("exit code %d, want %d", utils.ErrCode, tc.wantExit)
			}

			var printed []string
			var summary map[string]any
			for _, e := range logs.All() {
				printed = append(printed, e.Message)
				if strings.HasPrefix(e.Message, "mock replay summary") {
					summary = e.ContextMap()
				}
			}
			joined := strings.Join(printed, "\n")

			// The message alone is not enough: the whole point of the change is
			// which VALUES the summary carries.
			for field, want := range tc.wantFields {
				if summary == nil {
					t.Fatalf("no replay summary was logged at all; got:\n%s", joined)
				}
				if got := summary[field]; got != want {
					t.Errorf("summary %s = %v (%T), want %v (%T)", field, got, got, want, want)
				}
			}
			for _, want := range tc.wantLogs {
				if !strings.Contains(joined, want) {
					t.Errorf("no log mentioning %q; got:\n%s", want, joined)
				}
			}
			switch {
			case tc.wantMetered == nil && len(metered) != 0:
				t.Errorf("metered %+v on a run whose outcome was only partly read; it must not be counted at all", metered[0])
			case tc.wantMetered != nil && len(metered) != 1:
				t.Errorf("metered %d time(s), want exactly 1", len(metered))
			case tc.wantMetered != nil && metered[0] != *tc.wantMetered:
				t.Errorf("metered %+v, want %+v", metered[0], *tc.wantMetered)
			}

			for _, unwanted := range tc.unwantLogs {
				if strings.Contains(joined, unwanted) {
					t.Errorf("logged %q, which is not true of this run; got:\n%s", unwanted, joined)
				}
			}
		})
	}
}

// Under docker compose the agent is a service in the project, and compose
// stops it the moment the runner exits -- so there is no agent left to ask by
// the time the replay reads its outcome, and every compose replay reported
// consumed -1 / missed -1, was never isolated, and failed --strict. The agent
// now leaves its account as it is stopped, and that account is what the
// replay is judged on.
func TestComposeReplayIsJudgedOnWhatTheStoppedAgentLeft(t *testing.T) {
	twoConsumed := []models.MockState{{Name: "mock-0"}, {Name: "mock-1"}}
	oneMiss := []models.UnmatchedCall{{Protocol: "http", Destination: "dep:9411"}}
	for _, tc := range []struct {
		name         string
		left         models.MockOutcome
		leftErr      error
		wantExit     int
		wantErr      bool
		wantIsolated bool
		wantFields   map[string]any
		wantLogs     []string
		unwantLogs   []string
	}{
		{
			name:         "the agent left its account: the run is proven",
			left:         models.MockOutcome{Consumed: twoConsumed},
			wantIsolated: true,
			wantFields:   map[string]any{"consumed": int64(2), "missed": int64(0)},
			wantLogs:     []string{"this replay ran with every dependency answered from the recording"},
			unwantLogs:   []string{"incomplete", "could not be proven"},
		},
		{
			name:       "a miss it left fails --strict as the suite's contract, not as keploy's error",
			left:       models.MockOutcome{Consumed: twoConsumed, Missed: oneMiss},
			wantExit:   1,
			wantFields: map[string]any{"consumed": int64(2), "missed": int64(1)},
			wantLogs:   []string{"recorded dependency calls were missed"},
			unwantLogs: []string{"incomplete"},
		},
		{
			name:       "no account: unknown, and why, and --strict fails as keploy's own error",
			leftErr:    errors.New("the keploy-agent container keploy-v3-test left no /tmp/keploy-mock-outcome.json"),
			wantExit:   1,
			wantErr:    true,
			wantFields: map[string]any{"consumed": "unknown", "missed": "unknown", "reason": "the keploy-agent container keploy-v3-test left no /tmp/keploy-mock-outcome.json"},
			wantLogs:   []string{"incomplete", "writes what it served and missed as compose stops it", "could not be proven"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortAgentBudget(t)
			core, logs := observer.New(zapcore.DebugLevel)
			instr := newInstr(t, agentUpAfterRun, false, models.AppError{AppErrorType: models.ErrAppStopped})
			instr.runUntilReady = true
			// The agent is gone: compose stopped it with the runner. Asking it
			// over HTTP is what used to happen, and it can never answer.
			instr.consumedErr = errors.New("connection refused")
			instr.mockErrorsErr = errors.New("connection refused")
			instr.leftOutcome, instr.leftOutcomeErr = tc.left, tc.leftErr

			cfg := instrConfig(instr, utils.DockerCompose, "docker compose up")
			cfg.Mock.Strict = true
			cfg.Mock.OnMiss = string(models.MissFail)
			cfg.Path = t.TempDir()
			if err := os.MkdirAll(filepath.Join(cfg.Path, "set"), 0o755); err != nil {
				t.Fatal(err)
			}

			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })

			err := New(zap.New(core), instr, stubMockDB{}, nil, nil, nil, cfg).Replay(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("Replay returned %v, want an error: %v", err, tc.wantErr)
			}
			if utils.ErrCode != tc.wantExit {
				t.Errorf("exit code %d, want %d", utils.ErrCode, tc.wantExit)
			}

			var printed []string
			var summary map[string]any
			for _, e := range logs.All() {
				printed = append(printed, e.Message+" "+fmt.Sprint(e.ContextMap()))
				if strings.HasPrefix(e.Message, "mock replay summary") {
					summary = e.ContextMap()
				}
			}
			joined := strings.Join(printed, "\n")
			if summary == nil {
				t.Fatalf("no replay summary was logged; got:\n%s", joined)
			}
			for field, want := range tc.wantFields {
				if got := summary[field]; got != want {
					t.Errorf("summary %s = %v (%T), want %v (%T)", field, got, got, want, want)
				}
			}
			for _, want := range tc.wantLogs {
				if !strings.Contains(joined, want) {
					t.Errorf("no log mentioning %q; got:\n%s", want, joined)
				}
			}
			for _, unwanted := range tc.unwantLogs {
				if strings.Contains(joined, unwanted) {
					t.Errorf("logged %q, which is not true of this run; got:\n%s", unwanted, joined)
				}
			}

			r, rerr := ReadReceipt(cfg.Path, "set")
			if rerr != nil || r == nil {
				t.Fatalf("no receipt: %v", rerr)
			}
			if r.Isolated != tc.wantIsolated {
				t.Errorf("receipt isolated = %v (%s), want %v", r.Isolated, r.IsolationNote, tc.wantIsolated)
			}
			wantConsumed, wantMissed := len(tc.left.Consumed), len(tc.left.Missed)
			if tc.leftErr != nil {
				wantConsumed, wantMissed = -1, -1
			}
			if r.Consumed != wantConsumed || r.Missed != wantMissed {
				t.Errorf("receipt consumed/missed = %d/%d, want %d/%d", r.Consumed, r.Missed, wantConsumed, wantMissed)
			}
		})
	}
}
