package mock

import (
	"context"
	"errors"
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
			if err := svc.Replay(context.Background()); err != nil {
				t.Fatalf("Replay: %v", err)
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
