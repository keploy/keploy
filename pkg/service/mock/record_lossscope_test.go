package mock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
)

// lossRunner is a record's runner whose agent counts capture losses (the
// first read before, every later one after) and says what of them no test
// was charged with (uncharged, or unchargedErr).
type lossRunner struct {
	*runnerInstr
	*lossCounter
	uncharged    models.CaptureLosses
	unchargedErr error
}

func (l *lossRunner) GetUnchargedCaptureLosses(context.Context) (models.CaptureLosses, error) {
	return l.uncharged, l.unchargedErr
}

// stall is n parser stalls.
func stall(n uint64) models.CaptureLosses {
	return models.CaptureLosses{{Kind: models.CaptureLossParserStall, Count: n, What: "to db:3306"}}
}

// recordWithLosses records, under the switch in a build that judges the
// record, two tests (orders::TestA at 0-10 ms, orders::TestB at 10-20 ms,
// one call each) with the given per-test evidence and agent counts, over a
// stale last-record-tests.yaml. It returns the record's error and the
// per-test file it left.
// afterErr fails the run's own count after the test command (the base read
// answers); pid, when set, is the agent's PID-map count.
func recordWithLosses(t *testing.T, evA, evB *models.ScopeEvidence, after models.CaptureLosses, afterErr error, uncharged models.CaptureLosses, unchargedErr error, pid *pidMapCounter) (error, *TestsReceipt, string) {
	t.Helper()
	return recordWithLossesLogged(t, zap.NewNop(), evA, evB, after, afterErr, uncharged, unchargedErr, pid)
}

// recordWithLossesLogged is recordWithLosses whose record logs to logger.
func recordWithLossesLogged(t *testing.T, logger *zap.Logger, evA, evB *models.ScopeEvidence, after models.CaptureLosses, afterErr error, uncharged models.CaptureLosses, unchargedErr error, pid *pidMapCounter) (error, *TestsReceipt, string) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	judgedBuild(t)
	dir := t.TempDir()
	a := mark("orders::TestA", 0, 10*time.Millisecond)
	a.Outcome, a.Dir, a.Evidence = models.ScopeOutcomePassed, "orders", evA
	b := mark("orders::TestB", 10*time.Millisecond, 20*time.Millisecond)
	b.Outcome, b.Dir, b.Evidence = models.ScopeOutcomePassed, "orders", evB
	runner := &runnerInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped}),
		windows: []models.ScopeWindow{a, b}}
	runner.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(5*time.Millisecond)), mockAt("mock-1", runnerT0.Add(15*time.Millisecond))}
	counter := &lossCounter{after: after}
	if afterErr != nil {
		counter.errAt = map[int]error{2: afterErr}
	}
	instr := &lossRunner{runnerInstr: runner, lossCounter: counter, uncharged: uncharged, unchargedErr: unchargedErr}
	var in Instrumentation = instr
	if pid != nil {
		in = &pidLossRunner{instr, pid}
	}
	cfg := instrConfig(runner.composeInstr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path = dir
	set := (&mockService{config: cfg}).setName()
	// An earlier record's per-test results.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, set), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, set, RecordTestsReceiptFile), []byte("set: "+set+"\nat: 2020-01-01T00:00:00Z\nmocksDigest: \"\"\ntests:\n  - name: orders::TestStale\n    outcome: passed\n"), 0o644))
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	svc := New(logger, in, mockdb.New(zap.NewNop(), dir, ""), mapdb.New(zap.NewNop(), dir, ""), nil, nil, cfg)
	err := svc.Record(context.Background())
	got, rerr := ReadRecordTestsReceipt(dir, set)
	require.NoError(t, rerr)
	return err, got, filepath.Join(dir, set)
}

func receiptTest(r *TestsReceipt, name string) *TestReceipt {
	for i := range r.Tests {
		if r.Tests[i].Name == name {
			return &r.Tests[i]
		}
	}
	return nil
}

// A loss during one test of a judged record taints that test (and every
// later one), not the recording: the record keeps what it captured and
// writes what each test did, the clean test's line without evidence and the
// tainted one's with it, so judging commits the clean test's new recording
// and keeps the tainted one's previous one (design v14 §7.5). Before, the
// whole set was dropped, clean tests too, with no per-test file written and
// an earlier record's left standing.
func TestAJudgedRecordWithALossKeepsItsCleanTests(t *testing.T) {
	err, got, setDir := recordWithLosses(t, nil, &models.ScopeEvidence{CaptureIncomplete: stall(1)}, stall(1), nil, nil, nil, nil)
	require.NoError(t, err, "a loss charged to a test does not fail a judged record: judging decides")
	require.NotNil(t, got, "no per-test file was written")
	require.Nil(t, receiptTest(got, "orders::TestStale"), "an earlier record's per-test file was left standing")
	a, b := receiptTest(got, "orders::TestA"), receiptTest(got, "orders::TestB")
	require.NotNil(t, a)
	require.NotNil(t, b)
	require.Nil(t, a.Evidence, "the clean test carries no evidence")
	require.NotNil(t, b.Evidence)
	require.Equal(t, stall(1), b.Evidence.CaptureIncomplete)
	require.Empty(t, got.CaptureLosses, "every loss was charged to a test")
	raw, rerr := os.ReadFile(filepath.Join(setDir, "mocks.yaml"))
	require.NoError(t, rerr, "the recording was not kept")
	require.Contains(t, string(raw), "mock-0")
	require.Contains(t, string(raw), "mock-1")
}

// A loss after the last test is charged to no test: no recording holds what
// it carried, so the record's per-test file says it at the run's level, for
// judging to fail the run. Before, a run whose tests already carried an
// earlier loss lost it (the file was not even written), and judging
// quarantined the tainted tests and passed.
func TestAJudgedRecordSaysALossChargedToNoTest(t *testing.T) {
	ev := &models.ScopeEvidence{CaptureIncomplete: stall(1)}
	err, got, _ := recordWithLosses(t, ev, ev, stall(2), nil, stall(1), nil, nil)
	require.NoError(t, err)
	require.NotNil(t, got, "no per-test file was written")
	require.Equal(t, stall(1), got.CaptureLosses, "the loss after the last test is the run's")

	// What no test was charged with cannot be read: every loss of the run
	// is the run's, so judging fails it rather than pass on a guess.
	err, got, _ = recordWithLosses(t, ev, ev, stall(2), nil, nil, errors.New("the agent went away"), nil)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, stall(2), got.CaptureLosses)

	// Nor the run's own count: what was lost after the last test is
	// unknown, never "none", and judging fails the run on it.
	err, got, _ = recordWithLosses(t, ev, ev, nil, errors.New("bad file descriptor"), nil, errors.New("the agent went away"), nil)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got.CaptureLosses)
	require.Contains(t, got.CaptureLossesUnread, "bad file descriptor")
}

// Why the agent's counts could not be read is its client's error, whose text
// the agent's answer can fill: the per-test file keeps it whole, for judging,
// and the line that says judging fails the run names it by its kind, unless
// KEPLOY_LOG_CONTENT is set.
func TestAJudgedRecordLogsAnUnreadLossByItsKind(t *testing.T) {
	const canary = "kp-canary-loss-unread-5c1d"
	ev := &models.ScopeEvidence{CaptureIncomplete: stall(1)}
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		err, got, _ := recordWithLossesLogged(t, logger, ev, ev, nil, errors.New("the agent answered "+canary), nil, errors.New("the agent went away"), nil)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Contains(t, got.CaptureLossesUnread, canary, "the per-test file keeps why the losses could not be read")
		logtest.Check(t, logs, canary, on, "keploy could not read whether its agent captured every call after the test command's last test, so judging fails the run ("+provenswitch.Env+"=1)")
	})
}

// pidLossRunner is lossRunner whose agent also counts PID-map refusals.
type pidLossRunner struct {
	*lossRunner
	pid *pidMapCounter
}

func (p *pidLossRunner) GetPIDMapRefusals(ctx context.Context) (models.PIDMapRefusals, error) {
	return p.pid.GetPIDMapRefusals(ctx)
}

// A PID-map refusal charged to a test does not drop a judged record either:
// the clean test keeps its new recording.
func TestAJudgedRecordKeepsItsCleanTestsOverAPIDMapRefusal(t *testing.T) {
	ev := &models.ScopeEvidence{CaptureIncomplete: models.CaptureLosses{{Kind: models.CaptureLossPIDMap, Count: 1}}}
	err, got, setDir := recordWithLosses(t, nil, ev, nil, nil, nil, nil, &pidMapCounter{after: models.PIDMapRefusals{Target: 1}})
	require.NoError(t, err, "a refusal charged to a test does not fail a judged record")
	require.NotNil(t, got, "no per-test file was written")
	require.Nil(t, receiptTest(got, "orders::TestA").Evidence)
	raw, rerr := os.ReadFile(filepath.Join(setDir, "mocks.yaml"))
	require.NoError(t, rerr, "the recording was not kept")
	require.Contains(t, string(raw), "mock-0")
}
