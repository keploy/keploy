package mock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
)

// caseLossRunner is a record's runner whose agent counts capture losses and
// whose incoming stream ends with a marker that says ends.
type caseLossRunner struct {
	*lossRunner
	ends  models.StreamEndSummary
	taken int
}

func (c *caseLossRunner) TakeStreamEnds() models.StreamEndSummary {
	c.taken++
	return c.ends
}

// exchangeLost is what the agent streams a test case over an exchange keploy
// could not record with.
func exchangeLost(what string) models.CaptureLosses {
	return models.CaptureLosses{{Kind: models.CaptureLossExchangeUnrecorded, Count: 1, What: what}}
}

// caseIn is a test case whose request is at at, its response a millisecond
// later, streamed with lost.
func caseIn(name string, at time.Duration, lost models.CaptureLosses) *models.TestCase {
	tc := caseAt(name, runnerT0.Add(at))
	tc.HTTPResp.Timestamp = tc.HTTPReq.Timestamp.Add(time.Millisecond)
	tc.CaptureLost = lost
	return tc
}

// recordCases records, with the app's incoming requests on, two tests
// (orders::TestA at 0-10 ms, orders::TestB at 10-20 ms, one call each) and
// cases, the agent's incoming stream ending with ends. judged makes it a
// build that judges the record. It returns the record's error, the per-test
// file it left (nil when none), the set's folder, the runner, and the
// record's ERROR lines.
func recordCases(t *testing.T, judged bool, ends models.StreamEndSummary, cases ...*models.TestCase) (error, *TestsReceipt, string, *caseLossRunner, *observer.ObservedLogs) {
	t.Helper()
	if judged {
		judgedBuild(t)
	}
	dir := t.TempDir()
	a := mark("orders::TestA", 0, 10*time.Millisecond)
	a.Outcome, a.Dir = models.ScopeOutcomePassed, "orders"
	b := mark("orders::TestB", 10*time.Millisecond, 20*time.Millisecond)
	b.Outcome, b.Dir = models.ScopeOutcomePassed, "orders"
	runner := &runnerInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped}),
		windows: []models.ScopeWindow{a, b}, incoming: cases}
	runner.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(5*time.Millisecond)), mockAt("mock-1", runnerT0.Add(15*time.Millisecond))}
	instr := &caseLossRunner{lossRunner: &lossRunner{runnerInstr: runner, lossCounter: &lossCounter{}}, ends: ends}
	cfg := instrConfig(runner.composeInstr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path = dir
	withRequests(cfg)
	set := (&mockService{config: cfg}).setName()
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	core, logs := observer.New(zapcore.ErrorLevel)
	svc := New(zap.New(core), instr, mockdb.New(zap.NewNop(), dir, ""), mapdb.New(zap.NewNop(), dir, ""), nil, nil, cfg)
	svc.(TestDBSetter).SetTestDB(&memTestDB{})
	err := svc.Record(context.Background())
	got, rerr := ReadRecordTestsReceipt(dir, set)
	require.NoError(t, rerr)
	return err, got, filepath.Join(dir, set), instr, logs
}

// Under the proven switch keploy mock record asks the agent to stream a test
// case over an exchange keploy could not record with that loss
// (ChargesLosses), as keploy record does, in place of leaving it out unsaid;
// it asks only with an agent client that counts what it could not capture.
// With the switch off it asks as before: its filters alone.
//
// Not parallel: it sets the switch.
func TestKeployMockRecordAsksForATestCaseWithWhatItsRecordingLost(t *testing.T) {
	filters := []models.Filter{{BypassRule: models.BypassRule{Path: "/health"}}}
	counting := &mockService{instrumentation: &lossRunner{runnerInstr: newRunnerInstr(t), lossCounter: &lossCounter{}}, config: &config.Config{}}
	counting.config.Record.Filters = filters
	plain := &mockService{instrumentation: newRunnerInstr(t), config: counting.config}
	t.Setenv(provenswitch.Env, "")
	require.Equal(t, models.IncomingOptions{Filters: filters}, counting.incomingOptions(), "with the switch off the stream is asked for as before")
	t.Setenv(provenswitch.Env, "1")
	require.Equal(t, models.IncomingOptions{Filters: filters, ChargesLosses: true}, counting.incomingOptions())
	require.Equal(t, models.IncomingOptions{Filters: filters}, plain.incomingOptions(), "an instrumentation that counts nothing has no watch to charge")

	// And the record asks with it.
	_, _, _, instr, _ := recordCases(t, true, models.StreamEndSummary{}, caseIn("test-1", 5*time.Millisecond, nil))
	instr.mu.Lock()
	asked := instr.incomingOpts
	instr.mu.Unlock()
	require.True(t, asked.ChargesLosses, "the record asked for its incoming stream without ChargesLosses")
	require.Equal(t, 1, instr.taken, "the record did not take what the stream's end marker said")
}

// Under the proven switch, in a build that judges the record, a test case the
// agent streamed with what its recording lost is stored, and that loss is the
// evidence of the test it was recorded in, so judging decides that test on
// it, as on a loss the agent charged it. So is a late loss the stream's end
// marker names, by the window the test case was streamed with. What no test
// holds (a test case recorded outside every test, the test cases the agent
// left out unnamed, a stream that ended without its marker) is the run's, in
// the per-test file. The record keeps what it captured: judging commits the
// clean test and keeps the charged one's previous recording.
//
// Not parallel: it sets the switch and the judging build.
func TestAJudgedMockRecordChargesATestCasesLossToItsTest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	clean := caseIn("test-1", 5*time.Millisecond, nil)
	lossy := caseIn("test-2", 12*time.Millisecond, exchangeLost("MySQL: mysql exchange not recorded"))
	late := caseIn("test-3", 14*time.Millisecond, nil)
	outside := caseIn("test-4", 50*time.Millisecond, exchangeLost("Http: http decode error"))
	ends := models.StreamEndSummary{
		LateLosses:          []models.LateLoss{{Request: late.HTTPReq.Timestamp, Response: late.HTTPResp.Timestamp, Lost: exchangeLost("MySQL: behind the traffic")}},
		TestCasesSuppressed: 1,
		Unmarked:            1,
	}
	err, got, setDir, _, logs := recordCases(t, true, ends, clean, lossy, late, outside)
	require.NoError(t, err, "a test case's loss does not fail a judged record: judging decides")
	require.NotNil(t, got, "no per-test file was written")
	a, b := receiptTest(got, "orders::TestA"), receiptTest(got, "orders::TestB")
	require.NotNil(t, a)
	require.NotNil(t, b)
	require.Nil(t, a.Evidence, "the clean test carries no evidence")
	require.NotNil(t, b.Evidence, "the test the lossy test cases were recorded in carries no evidence")
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossExchangeUnrecorded, Count: 2, What: "MySQL: mysql exchange not recorded; MySQL: behind the traffic"}}, b.Evidence.CaptureIncomplete)
	require.Contains(t, b.Evidence.Reason, "test case test-2 recorded in it")
	require.Contains(t, b.Evidence.Reason, "test case test-3 recorded in it")
	for _, want := range []string{"test case test-4, recorded outside every test", "(Http: http decode error)", "tcs_suppressed_total", "ended without their end marker"} {
		require.Contains(t, got.TestCasesLost, want)
	}
	raw, rerr := os.ReadFile(filepath.Join(setDir, "mocks.yaml"))
	require.NoError(t, rerr, "the recording was not kept")
	require.Contains(t, string(raw), "mock-1")
	var said bool
	for _, e := range logs.All() {
		if strings.HasPrefix(e.Message, "keploy could not record as mocks every call made while these test cases were recorded") {
			said = true
			require.Equal(t, []any{"test-2", "test-3", "test-4"}, e.ContextMap()["testCases"])
		}
	}
	require.True(t, said, "the record did not say which test cases lost a call: %v", logs.All())
}

// Under the proven switch, a record nothing judges is not kept over a test
// case's loss, as over any other loss of capture: it fails, naming the test
// cases and what they lost, and the previous recording is left as it was. So
// does a stream that left test cases out unnamed, or ended without its
// marker: the recording misses what it cannot say. With the switch off the
// agent streams no loss, the marker is not read, and the record is kept, as
// before.
//
// Not parallel: it sets the switch.
func TestAMockRecordNothingJudgesIsNotKeptOverATestCasesLoss(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, c := range []struct {
		name  string
		ends  models.StreamEndSummary
		cases []*models.TestCase
		want  string
	}{
		{"a test case streamed with its loss", models.StreamEndSummary{}, []*models.TestCase{caseIn("test-1", 5*time.Millisecond, exchangeLost("MySQL: mysql exchange not recorded"))}, "(MySQL: mysql exchange not recorded)"},
		{"test cases left out unnamed", models.StreamEndSummary{LeftOutInFlight: 2}, []*models.TestCase{caseIn("test-1", 5*time.Millisecond, nil)}, "tcs_left_out_in_flight"},
		{"a stream that ended without its marker", models.StreamEndSummary{Unmarked: 1}, []*models.TestCase{caseIn("test-1", 5*time.Millisecond, nil)}, "ended without their end marker"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err, _, setDir, _, logs := recordCases(t, false, c.ends, c.cases...)
			require.Error(t, err)
			require.Contains(t, err.Error(), c.want)
			require.Equal(t, int(utils.ExitKeployError), utils.ErrCode)
			_, serr := os.Stat(filepath.Join(setDir, "mocks.yaml"))
			require.True(t, errors.Is(serr, os.ErrNotExist), "the recording was kept: %v", serr)
			var said bool
			for _, e := range logs.All() {
				if strings.HasPrefix(e.Message, "the recording was not kept: keploy could not record as mocks every call made while these test cases were recorded") {
					said = strings.Contains(e.ContextMap()["reason"].(string), c.want)
				}
			}
			require.True(t, said, "the record did not say why: %v", logs.All())
		})
	}

	t.Run("switch off", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "")
		err, _, setDir, instr, _ := recordCases(t, false, models.StreamEndSummary{LeftOutInFlight: 2}, caseIn("test-1", 5*time.Millisecond, nil))
		require.NoError(t, err)
		require.Zero(t, instr.taken, "with the switch off the marker is not taken")
		_, serr := os.Stat(filepath.Join(setDir, "mocks.yaml"))
		require.NoError(t, serr, "with the switch off the recording is kept as before")
	})
}
