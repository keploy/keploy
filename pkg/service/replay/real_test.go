package replay

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// realTestInst is an agent client that reads a test case's mock.real use.
type realTestInst struct {
	Instrumentation
	got models.RealTest
	err error
}

func (f realTestInst) RealTest(context.Context) (models.RealTest, error) { return f.got, f.err }

func realReplayer(inst Instrumentation, real ...string) *Replayer {
	cfg := &config.Config{}
	cfg.Mock.Real = real
	return &Replayer{logger: zap.NewNop(), config: cfg, instrumentation: inst, mockMismatchFailures: NewTestFailureStore()}
}

// Under the proven switch with a mock.real list, a test case's read of its
// misses carries its REAL rows, and the connections keploy could not capture
// while it ran as a capture loss: it fails the test as CAPTURE_INCOMPLETE,
// with what to change, never as a miss. A read that fails leaves the test
// without a verdict, as a loss count that could not be read does. Without
// the switch or a list, nothing is read.
func TestReadRealTestChargesWhatKeployCouldNotCapture(t *testing.T) {
	ctx := context.Background()
	got := models.RealTest{
		Real:       []models.RealUse{{Dest: "db:5432", Conns: 1}},
		Incomplete: []models.CaptureIncomplete{{Class: models.CaptureIncompleteRealName, Destination: "10.0.0.7:443", Why: "list the address in mock.real"}},
	}
	t.Setenv(provenswitch.Env, "1")
	read := testMisses{asked: true}
	realReplayer(realTestInst{got: got}, "db:5432").readRealTest(ctx, &read)
	require.NoError(t, read.err)
	require.NoError(t, read.realErr)
	require.Equal(t, got.Real, read.real)
	require.Empty(t, read.calls, "a connection keploy could not capture is no call that got no recorded answer")
	lost := models.CaptureLosses{{Kind: models.CaptureLossRealUncaptured, Count: 1, What: "10.0.0.7:443: list the address in mock.real"}}
	require.Equal(t, lost, read.realLost)

	res := models.TestResult{Name: "tc", TestCaseID: "tc", Status: models.TestStatusPassed}
	realReplayer(realTestInst{got: got}, "db:5432").attachReadMockErrors(read, "set", "tc", &res)
	require.Equal(t, got.Real, res.Real)
	require.Equal(t, lost, res.FailureInfo.CaptureIncomplete)
	require.Empty(t, res.FailureInfo.UnmatchedCalls)
	require.Contains(t, res.FailureInfo.CaptureIncomplete.Text(), "keploy could neither capture nor relay as real 1 connection(s) to a mock.real destination's address (10.0.0.7:443: list the address in mock.real)")
	results := []models.TestResult{res}
	failTestsWithCaptureLoss(results)
	require.Equal(t, models.TestStatusFailed, results[0].Status, "a connection keploy could not capture fails its test")
	require.Contains(t, results[0].FailureInfo.Category, models.CaptureIncompleteCategory)
	require.NotContains(t, results[0].FailureInfo.Category, models.CallUnmatched)

	read = testMisses{asked: true}
	realReplayer(realTestInst{err: errors.New("agent gone")}, "db:5432").readRealTest(ctx, &read)
	require.ErrorContains(t, read.realErr, "agent gone")
	res = models.TestResult{Name: "tc", TestCaseID: "tc", Status: models.TestStatusPassed}
	realReplayer(nil, "db:5432").attachReadMockErrors(read, "set", "tc", &res)
	require.True(t, res.FailureInfo.CaptureUnread, "a test whose capture cannot be told has no verdict")
	read = testMisses{asked: true}
	realReplayer(struct{ Instrumentation }{}, "db:5432").readRealTest(ctx, &read)
	require.Error(t, read.err, "an agent client that cannot read it leaves the test without a verdict")

	read = testMisses{asked: true}
	realReplayer(realTestInst{got: got}).readRealTest(ctx, &read)
	require.Equal(t, testMisses{asked: true}, read, "no list, nothing read")
	t.Setenv(provenswitch.Env, "")
	read = testMisses{asked: true}
	realReplayer(realTestInst{got: got}, "db:5432").readRealTest(ctx, &read)
	require.Equal(t, testMisses{asked: true}, read, "without the switch, nothing read")
}

// A set's run fails on mock.real when a destination the recordings of its
// test cases that ran list was never reached (rule 4). A test case that did
// not run asks for nothing. What keploy could not capture outside every test
// case (rule 2) is the set's capture verdict, said once per destination and
// reason.
func TestRealSetFailure(t *testing.T) {
	cases := []*models.TestCase{{Name: "a", Real: []string{"db:5432"}}, {Name: "b", Real: []string{"auth:443", "db:5432"}}, {Name: "skipped", Real: []string{"cache:6379"}}}
	results := []models.TestResult{{TestCaseID: "a", Status: models.TestStatusPassed}, {TestCaseID: "b", Status: models.TestStatusFailed}, {TestCaseID: "skipped", Status: models.TestStatusIgnored}}
	listed := realListedBy(cases, results)
	require.Equal(t, []string{"auth:443", "db:5432"}, listed)

	require.Empty(t, realSetFailure(&models.RealRun{Reached: []string{"auth:443", "db:5432"}}, listed, nil))
	why := realSetFailure(&models.RealRun{Reached: []string{"db:5432"}}, listed, nil)
	require.Contains(t, why, "auth:443")
	require.Contains(t, why, "never reached")
	require.NotContains(t, why, "db:5432,")

	inc := models.CaptureIncomplete{Class: models.CaptureIncompleteRealName, Destination: "10.0.0.7:443", Why: "list the address"}
	require.Empty(t, realSetFailure(&models.RealRun{Reached: listed, Incomplete: []models.CaptureIncomplete{inc, inc}}, listed, nil), "rule 2 is the capture verdict's")
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossRealUncaptured, Count: 2, What: "10.0.0.7:443: list the address"}},
		models.RealUncapturedLosses([]models.CaptureIncomplete{inc, inc}), "once per destination and reason, every connection counted")

	// A destination the set's cases used that never answered is told apart.
	used := realUsedBy([]models.TestResult{{Real: []models.RealUse{{Dest: "auth:443", Conns: 1}}}, {Real: []models.RealUse{{Dest: "db:5432", Failed: 1}}}})
	require.Equal(t, []string{"auth:443"}, used)
	why = realSetFailure(&models.RealRun{}, listed, used)
	require.Contains(t, why, "auth:443, which its test cases' recordings list, was used but never answered")
	require.Contains(t, why, "db:5432, which its test cases' recordings list, was never reached")
}

// A set's test case is pushed mock.real before its mocks, under the switch,
// and nothing is read from a replayer without a config.
func TestMockRealNeedsTheSwitchAndAConfig(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	require.Nil(t, (&Replayer{}).mockReal())
	require.Equal(t, []string{"db:5432"}, realReplayer(nil, "db:5432").mockReal())
	t.Setenv(provenswitch.Env, "")
	require.Nil(t, realReplayer(nil, "db:5432").mockReal())
}

// pushInst is an agent client that notes the run's pushes of mock.real.
type pushInst struct {
	Instrumentation
	calls []string
}

func (p *pushInst) PushRealDeps(_ context.Context, real []string) error {
	p.calls = append(p.calls, "push")
	return nil
}
func (p *pushInst) KeepRealDeps(_ context.Context, real []string) error {
	p.calls = append(p.calls, "keep")
	return nil
}
func (p *pushInst) RealRun(context.Context) (models.RealRun, error) { return models.RealRun{}, nil }
func (p *pushInst) MockOutgoing(context.Context, models.OutgoingOptions) error {
	p.calls = append(p.calls, "mock")
	return nil
}

// A run's first set pushes mock.real afresh, and every later set keeps what
// the run saw; a run with no agent (a base path) pushes nothing.
func TestARunPushesItsListOnceAndKeepsItAfter(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	inst := &pushInst{}
	r := realReplayer(inst, "db:5432")
	for range 3 {
		require.NoError(t, r.mockOutgoingForTestSet(context.Background(), models.OutgoingOptions{}))
	}
	require.Equal(t, []string{"push", "mock", "keep", "mock", "keep", "mock"}, inst.calls)

	inst = &pushInst{}
	r = realReplayer(inst, "db:5432")
	r.noAgent = true
	require.NoError(t, r.mockOutgoingForTestSet(context.Background(), models.OutgoingOptions{}))
	require.Equal(t, []string{"mock"}, inst.calls)
}

// A test case's REAL rows, a connection keploy could neither capture nor relay
// as real, and what the agent lost while it ran compose on its result: each
// is kept, and neither hides the other.
func TestATestCasesRealRowsAndACaptureLossCompose(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	inc := models.CaptureIncomplete{Class: models.CaptureIncompleteRealName, Destination: "10.0.0.7:443", Why: "list the address in mock.real"}
	lost := models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: 1, What: "to db:5432"}}
	read := testMisses{asked: true, real: []models.RealUse{{Dest: "db:5432", Conns: 1}}, realLost: models.RealUncapturedLosses([]models.CaptureIncomplete{inc}), lost: lost}
	r := realReplayer(nil, "db:5432")
	result := &models.TestResult{}
	r.attachReadMockErrors(read, "test-set-0", "test-1", result)
	require.Equal(t, []models.RealUse{{Dest: "db:5432", Conns: 1}}, result.Real)
	require.Equal(t, append(lost, models.CaptureLoss{Kind: models.CaptureLossRealUncaptured, Count: 1, What: "10.0.0.7:443: list the address in mock.real"}), result.FailureInfo.CaptureIncomplete)
	require.Empty(t, result.FailureInfo.UnmatchedCalls)
	text := result.FailureInfo.CaptureIncomplete.Text()
	require.Contains(t, text, "1 TLS handshake(s)")
	require.Contains(t, text, "10.0.0.7:443: list the address in mock.real")

	// A read with no result to attach to is dropped, never a panic.
	r.attachReadMockErrors(read, "test-set-0", "test-1", nil)
}
