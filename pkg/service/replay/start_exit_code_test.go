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

// GetTestRunID names the run this Start made, and none for a Start that
// stopped before it named one: a replayer an earlier Start named test-run-1
// whose next Start fails before its first test set (its agent does not come
// up) said test-run-1 was that run, and a caller reading that run's reports
// read the earlier run's green as this one's.
func TestAStartThatNamesNoRunDoesNotNameTheOneBefore(t *testing.T) {
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })
	cause := errors.New("the keploy agent could not start")
	sets := func(context.Context) ([]string, error) { return []string{"test-set-0"}, nil }
	r := NewReplayer(zap.NewNop(), startTestDB{list: sets}, nil, startReportDB{}, nil, nil, startTelemetry{}, setupFailsInstrumentation{err: cause}, nil, &config.Config{Command: "true"}).(*Replayer)
	// What an earlier Start of this replayer named.
	r.testRunID, r.testRunTestSets = "test-run-1", []string{"test-set-0"}
	if err := r.Start(context.Background()); err == nil || !strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("Start returned %v; want it to name the agent's failure (%v)", err, cause)
	}
	if got := r.GetTestRunID(); got != "" {
		t.Fatalf("GetTestRunID = %q after a Start that named no run; want \"\"", got)
	}
	if r.testRunTestSets != nil {
		t.Fatalf("the earlier run's test sets are still this run's: %v", r.testRunTestSets)
	}
}

// A Start that its user stops before its first test set is the run of none
// of an earlier Start's tests: it exits 0, as an interrupted run that failed
// nothing does, and its totals, reports, failures and the test sets whose
// misses it could not attribute are its own -- none.
// They were reset only once the test sets were listed, so the exit code read
// an earlier Start's failed tests and exited 1, and the getters returned
// that run's.
func TestAStartStoppedBeforeItsFirstSetCarriesNoEarlierRun(t *testing.T) {
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := func(ctx context.Context) ([]string, error) {
		cancel()
		return nil, ctx.Err()
	}
	r := NewReplayer(zap.NewNop(), startTestDB{list: stop}, nil, nil, nil, nil, startTelemetry{}, startInstrumentation{}, nil, &config.Config{}).(*Replayer)
	// What an earlier Start of this replayer ran: two tests, one failed.
	r.totalTests, r.totalTestPassed, r.totalTestFailed = 2, 1, 1
	r.completeTestReport = map[string]TestReportVerdict{"test-set-0": {}}
	r.mockMismatchFailures.AddFailure("test-set-0", "test-1", []string{"mock-1"}, nil)
	r.noteUnattributed("test-set-0")
	if err := r.Start(ctx); err == nil {
		t.Fatal("Start returned nil for a run stopped before its first test set")
	}
	if utils.ErrCode != 0 {
		t.Fatalf("exit code %d for a run its user stopped before it ran a test; want 0", utils.ErrCode)
	}
	if total, passed, failed := r.GetTestRunTotals(); total != 0 || passed != 0 || failed != 0 {
		t.Fatalf("totals %d/%d/%d are an earlier run's", total, passed, failed)
	}
	if got := r.GetCompleteTestRunReport(); len(got) != 0 {
		t.Fatalf("the complete report is an earlier run's: %v", got)
	}
	if got := r.GetMockMismatchFailures(); len(got) != 0 {
		t.Fatalf("the mock mismatch failures are an earlier run's: %v", got)
	}
	r.unattributedMu.Lock()
	unattributed := len(r.unattributedSets)
	r.unattributedMu.Unlock()
	if unattributed != 0 {
		t.Fatalf("the test sets an earlier run could not attribute misses of are this run's: %v", r.unattributedSets)
	}
}

// tooOldAgentInstr is the stand-in agent of a compose stack whose /storemocks
// answers as a keploy agent older than models.MockStreamMinAgentVersion does,
// in the error the agent client returns for it.
type tooOldAgentInstr struct{ *composeInstr }

func (*tooOldAgentInstr) StoreMocks(context.Context, []*models.Mock, []*models.Mock) error {
	return fmt.Errorf("storemocks http 400: %w", models.ErrAgentPredatesMockStream)
}

// A test set that fails on its agent's mock store fails the run, and Start's
// error has to keep the store's cause, not only its text. A caller that knows
// which agent it ran tells an agent too old for the mock stream by
// errors.Is(err, models.ErrAgentPredatesMockStream): k8s-proxy names its replay
// pods' agent image and keeps a recording's tests that such a replay could
// never persist. Start rebuilt the error from its message, so the cause was
// gone and k8s-proxy could only relay "storemocks http 400".
func TestStartKeepsTheCauseOfAFailedTestSet(t *testing.T) {
	c := &composeInstr{}
	h := composeHarness(t, c)
	h.mocks.filtered = []*models.Mock{{Name: "mock-a", Kind: models.HTTP}}
	h.replayer.instrumentation = &tooOldAgentInstr{composeInstr: c}
	h.replayer.config.Test.Mocking = true
	h.replayer.config.Command = "docker compose up"
	h.replayer.config.Test.MaxFlakyChecks = 1
	saved := utils.ErrCode
	t.Cleanup(func() { utils.ErrCode = saved })
	utils.ErrCode = 0
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	err := h.replayer.Start(ctx)
	if !errors.Is(err, models.ErrAgentPredatesMockStream) {
		t.Fatalf("Start: %v; want an error that is models.ErrAgentPredatesMockStream", err)
	}
	if want := "failed to run test set: storemocks http 400: " + models.ErrAgentPredatesMockStream.Error(); err.Error() != want {
		t.Fatalf("Start: %q; want %q", err.Error(), want)
	}
}
