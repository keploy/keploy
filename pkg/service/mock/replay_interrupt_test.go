package mock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// Where in a replay the user's Ctrl+C comes.
const (
	signalAtSetup   = "setup"   // while keploy starts its agent
	signalAtWindows = "windows" // after the test command exited, while keploy reads what each test did
	signalAtReal    = "real"    // after the test command exited, while keploy reads the run's mock.real
	signalAtCounts  = "counts"  // while keploy judges, reading the agent's counts of what it served
	signalAtSettle  = "settle"  // while keploy judges, waiting for the agent's end-of-run hook (proven)
)

// signalInstr is a replay's agent client that the user's Ctrl+C reaches at
// one point of the run: there it does what NewCtx's handler does (records
// SIGINT, cancels the root context), and the agent call under way ends with
// the cancel, as a real one does.
type signalInstr struct {
	*composeInstr
	at     string
	cancel context.CancelFunc
	ran    atomic.Bool
	once   sync.Once
	// stopped is set by the stop, and readAfter counts the agent's counts
	// reads made after it.
	stopped   atomic.Bool
	readAfter atomic.Int32
}

func (s *signalInstr) interrupt(ctx context.Context) error {
	s.once.Do(func() {
		utils.MarkInterrupted()
		s.stopped.Store(true)
		s.cancel()
	})
	<-ctx.Done()
	return ctx.Err()
}

func (s *signalInstr) Setup(ctx context.Context, cmd string, opts models.SetupOptions) error {
	if s.at == signalAtSetup {
		return fmt.Errorf("the keploy agent did not come up: %w", s.interrupt(ctx))
	}
	return s.composeInstr.Setup(ctx, cmd, opts)
}

func (s *signalInstr) Run(ctx context.Context, opts models.RunOptions) models.AppError {
	res := s.composeInstr.Run(ctx, opts)
	s.ran.Store(true)
	return res
}

func (s *signalInstr) GetScopeWindows(ctx context.Context) ([]models.ScopeWindow, error) {
	if s.at == signalAtWindows && s.ran.Load() {
		return nil, s.interrupt(ctx)
	}
	return s.composeInstr.GetScopeWindows(ctx)
}

// AfterTestRun is the agent's end-of-run hook (RunSettler), which a proven
// replay calls before it reads the agent's counts.
func (s *signalInstr) AfterTestRun(ctx context.Context, _ string, _ []string, _ models.TestCoverage) error {
	if s.at == signalAtSettle && s.ran.Load() {
		return s.interrupt(ctx)
	}
	return nil
}

func (s *signalInstr) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	if s.at == signalAtCounts && s.ran.Load() {
		return nil, s.interrupt(ctx)
	}
	if s.stopped.Load() {
		s.readAfter.Add(1)
	}
	return s.composeInstr.GetConsumedMocks(ctx)
}

// GetMockErrors ends with the cancel, as a real agent call does: the misses
// read right after the counts is cut short by the same Ctrl+C.
func (s *signalInstr) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.composeInstr.GetMockErrors(ctx)
}

func (s *signalInstr) PushRealDeps(context.Context, []string) error { return nil }

func (s *signalInstr) RealRun(ctx context.Context) (models.RealRun, error) {
	if s.at == signalAtReal && s.ran.Load() {
		return models.RealRun{}, s.interrupt(ctx)
	}
	return models.RealRun{}, nil
}

// A replay the user stops before keploy has judged it did not verify the
// suite, wherever the Ctrl+C comes: it exits 128+N (130 for SIGINT), never
// the test command's 0, is not keploy's failure, is not judged, and leaves
// the last receipt standing. A Ctrl+C after the test command passed, while
// keploy read what each test did or the run's mock.real, used to end the
// replay with an error the CLI took for the interrupt and dropped: no code
// was armed, and a run keploy never judged exited 0. One while the agent
// started ended it with nil, and 0, the same way; one while keploy judged
// (with the switch off, after reads that ended short) left a verdict on part
// of the run, and its receipt, and under --strict failed it (1) on a read
// the stop cut short.
func TestAReplayTheUserStopsBeforeItIsJudgedExits128PlusN(t *testing.T) {
	cases := []struct {
		name   string
		proven bool
		at     string
		strict bool
	}{
		{"while the agent starts", false, signalAtSetup, false},
		{"while the agent starts, proven", true, signalAtSetup, false},
		{"while keploy reads what each test did", false, signalAtWindows, false},
		{"while keploy reads what each test did, proven", true, signalAtWindows, false},
		{"while keploy reads the run's mock.real, proven", true, signalAtReal, false},
		{"while keploy judges", false, signalAtCounts, false},
		{"while keploy judges, --strict", false, signalAtCounts, true},
		{"while keploy judges, proven", true, signalAtCounts, false},
		{"while the agent finishes the run's end, proven", true, signalAtSettle, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.proven {
				provenOn(t)
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			utils.ClearInterrupted()
			t.Cleanup(utils.ClearInterrupted)
			defer utils.SetInterruptSignalForTest(2)() // SIGINT

			repo := t.TempDir()
			t.Chdir(repo)
			keployDir := filepath.Join(repo, "keploy")
			if err := os.MkdirAll(filepath.Join(keployDir, "set"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			writeMapping(t, keployDir)
			inner := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
			inner.consumedMocks = []models.MockState{{Name: "mock-0"}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			instr := &signalInstr{composeInstr: inner, at: tc.at, cancel: cancel}
			cfg := instrConfig(inner, utils.Native, "go test -p 1 -json ./...")
			cfg.Path = keployDir
			cfg.Mock.OnMiss = "fail"
			cfg.Mock.Strict = tc.strict
			if tc.at == signalAtReal {
				cfg.Mock.Real = []string{"localhost:5432"}
			}
			judged := false
			RegisterReplayOutcomeReporter(func(context.Context, ReplayOutcome) { judged = true })
			t.Cleanup(func() { RegisterReplayOutcomeReporter(nil) })
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })

			core, logs := observer.New(zapcore.DebugLevel)
			err := New(zap.New(core), instr, named("mock-0"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(ctx)

			if err != nil {
				t.Errorf("a replay the user stopped returned %v: it is not keploy's failure", err)
			}
			if utils.ErrCode != 130 {
				t.Errorf("a replay the user stopped (SIGINT) before keploy judged it exits %d, want 130", utils.ErrCode)
			}
			// Judging reads the counts it was stopped in; a stop before it
			// is never judged.
			if judged && tc.at != signalAtCounts {
				t.Error("a replay the user stopped before keploy read what the run did was judged")
			}
			// Stopped while it read what the run did, it reads nothing more.
			if (tc.at == signalAtWindows || tc.at == signalAtReal) && instr.readAfter.Load() != 0 {
				t.Error("a replay the user stopped while keploy read what the run did went on to read the agent's counts")
			}
			// Wherever the stop came, a read it cut short is not the agent's
			// failure: nothing tells the user to look in the agent's logs.
			if n := logs.FilterMessage("mock replay summary (incomplete: the agent did not report the whole outcome)").Len(); n != 0 {
				t.Error("a replay the user stopped said the agent did not report the whole outcome")
			}
			if n := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); n != 0 {
				t.Errorf("a replay the user stopped logged %d ERROR line(s), the first %q: the stop is not keploy's failure", n, logs.FilterLevelExact(zapcore.ErrorLevel).All()[0].Message)
			}
			if _, err := os.Stat(filepath.Join(keployDir, "set", ReceiptFile)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a replay the user stopped wrote a receipt (%v): the last run's must stand", err)
			}
		})
	}
}
