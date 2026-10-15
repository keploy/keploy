package replay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// startTestDB is a TestDB that knows only how to list test sets. Every other
// method panics through the nil embedded interface: the runs below end before
// Start reads a single test.
type startTestDB struct {
	TestDB
	list func(ctx context.Context) ([]string, error)
}

func (db startTestDB) GetAllTestSetIDs(ctx context.Context) ([]string, error) { return db.list(ctx) }

// startInstrumentation answers only the graceful-shutdown notice Start's
// teardown sends on every way out.
type startInstrumentation struct{ Instrumentation }

func (startInstrumentation) NotifyGracefulShutdown(context.Context) error { return nil }

type startTelemetry struct{ Telemetry }

func (startTelemetry) TestRunAborted(string) {}

// startExitCode runs Start against list and returns the exit code the process
// would end with.
func startExitCode(t *testing.T, ctx context.Context, list func(ctx context.Context) ([]string, error)) int {
	t.Helper()
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	r := NewReplayer(zap.NewNop(), startTestDB{list: list}, nil, nil, nil, nil, startTelemetry{}, startInstrumentation{}, nil, &config.Config{})
	if err := r.Start(ctx); err == nil {
		t.Fatal("Start returned nil for a run that never reached a test set")
	}
	return utils.ErrCode
}

// A `keploy test` that never ran a test has to exit non-zero: `keploy test`
// drops Start's error on purpose, so the exit code is the only thing that can
// tell a CI job its tests were never recorded or never read.
//
// It exited 0. The defer that turns Start's error into the exit code asked
// whether the ROOT context was cancelled -- a user's Ctrl+C -- but read a ctx
// that `g, ctx := errgroup.WithContext(ctx)` had since replaced in the same
// scope, and Start's own teardown defer cancels that one before this defer
// runs. So every error Start returned read as an interrupt and was dropped.
func TestStartThatRunsNoTestExitsNonZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		list func(context.Context) ([]string, error)
	}{
		{"no test sets recorded", func(context.Context) ([]string, error) { return nil, nil }},
		{"test sets unreadable", func(context.Context) ([]string, error) {
			return nil, errors.New("open keploy: permission denied")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if got := startExitCode(t, ctx, tc.list); got != 1 {
				t.Fatalf("exit code %d, want 1", got)
			}
		})
	}
}

// The other half: a run the USER stopped is still not a failure. The fix above
// must read the context Start was handed, not stop reading contexts: a Ctrl+C
// that lands while the test sets are being listed makes that read fail with the
// cancellation, and that has to keep exiting 0.
func TestStartInterruptedBeforeAnyTestExitsZero(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interrupt := func(ctx context.Context) ([]string, error) {
		cancel()
		return nil, ctx.Err()
	}
	if got := startExitCode(t, ctx, interrupt); got != 0 {
		t.Fatalf("an interrupted run exited %d, want 0", got)
	}
}

// A deadline is not a user's stop. A run whose context times out before any
// test has a verdict did not verify anything, and exits non-zero: it exited 0,
// read as a Ctrl+C.
func TestStartWhoseDeadlinePassesBeforeAnyTestExitsNonZero(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	timedOut := func(ctx context.Context) ([]string, error) { return nil, ctx.Err() }
	if got := startExitCode(t, ctx, timedOut); got != 1 {
		t.Fatalf("a run that timed out exited %d, want 1", got)
	}
}

// setupFailsInstrumentation is an agent that never comes up: its Setup fails
// as AgentClient.Setup does.
type setupFailsInstrumentation struct {
	startInstrumentation
	err error
}

func (s setupFailsInstrumentation) Setup(context.Context, string, models.SetupOptions) error {
	return s.err
}

// Instrument keeps an agent's failure, tag and all: Start reads the tag from
// it to arm the run's code before it flattens the error it returns.
func TestInstrumentNamesWhyTheAgentCouldNotStart(t *testing.T) {
	cause := fmt.Errorf("%w: the keploy agent could not start (exit status 3)", utils.ErrPrivilegeRequired)
	r := NewReplayer(zap.NewNop(), nil, nil, nil, nil, nil, startTelemetry{}, setupFailsInstrumentation{err: cause}, nil, &config.Config{Command: "python3 app.py"}).(*Replayer)
	_, err := r.Instrument(context.Background())
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("Instrument returned %v; want it to carry the agent's failure (%v)", err, cause)
	}
	if got := utils.ExitCodeFor(err); got != utils.ExitPrivilegeRequired {
		t.Fatalf("the error maps to exit code %d, want %d", got, utils.ExitPrivilegeRequired)
	}
}

// startReportDB answers the one read Start makes of the reports before it
// instruments: the run ids, to name the next run.
type startReportDB struct{ ReportDB }

func (startReportDB) GetAllTestRunIDs(context.Context) ([]string, error) { return nil, nil }

// A `keploy test` whose setup failed for a tagged reason no layer armed
// exits with that reason's code, and its error names the cause. The run's
// error is flat (#4792), so the tag is read where Start still has it; such a
// run used to be armed a bare 1.
func TestStartWhoseAgentCannotStartExitsWithItsCode(t *testing.T) {
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })
	cause := fmt.Errorf("%w: the keploy agent could not start (exit status 3)", utils.ErrPrivilegeRequired)
	sets := func(context.Context) ([]string, error) { return []string{"test-set-0"}, nil }
	r := NewReplayer(zap.NewNop(), startTestDB{list: sets}, nil, startReportDB{}, nil, nil, startTelemetry{}, setupFailsInstrumentation{err: cause}, nil, &config.Config{Command: "true"})
	err := r.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("Start returned %v; want it to name the agent's failure (%v)", err, cause)
	}
	if utils.ErrCode != utils.ExitPrivilegeRequired {
		t.Fatalf("exit code %d, want %d", utils.ErrCode, utils.ExitPrivilegeRequired)
	}
}
