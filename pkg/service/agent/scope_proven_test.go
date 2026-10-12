//go:build linux

package agent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// The tests' agent is this test process, which also claims as the test
// process: the agent's refusal of its own claims is tested by itself.
func init() { agentPID = -1 }

// wireProxy is the part of the proxy the proven wire reads: the misses, how
// often each mock answered, the per-test capture window and the tree root.
type wireProxy struct {
	coreAgent.Proxy
	mu        sync.Mutex
	misses    []models.UnmatchedCall
	missErr   error
	served    map[string]uint64
	continues int
	// filtered is the per-test pool the last SetMocks installed; setErr
	// fails SetMocks.
	filtered []*models.Mock
	setErr   error
}

func (p *wireProxy) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.missErr != nil {
		return nil, p.missErr
	}
	out := p.misses
	p.misses = nil
	return out, nil
}

func (p *wireProxy) ServedCounts() map[string]uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]uint64, len(p.served))
	for k, v := range p.served {
		out[k] = v
	}
	return out
}

func (p *wireProxy) ContinueTestErrorCapture() { p.mu.Lock(); p.continues++; p.mu.Unlock() }

// SetMocks takes the pool a begin narrows to its test's mocks.
func (p *wireProxy) SetMocks(_ context.Context, filtered, _ []*models.Mock) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.setErr != nil {
		return p.setErr
	}
	p.filtered = filtered
	return nil
}

// countlessProxy cannot count which mocks answered.
type countlessProxy struct{ coreAgent.Proxy }

func (countlessProxy) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) { return nil, nil }

// captureless counts answers but cannot keep each test's misses apart.
type captureless struct{ coreAgent.Proxy }

func (captureless) ServedCounts() map[string]uint64 { return nil }

// wireAgent is an agent whose CLI (its registered client) is this test
// process's parent, so the test process and its children are the run's.
func wireAgent(mode models.Mode) (*Agent, *wireProxy) {
	p := &wireProxy{served: map[string]uint64{}}
	cfg := &config.Config{}
	cfg.Agent.Mode = mode
	cfg.Agent.ClientNSPID = uint32(os.Getppid())
	return &Agent{logger: zap.NewNop(), Proxy: p, config: cfg}, p
}

func begin(name string) models.ScopeReq { return models.ScopeReq{Name: name, CanSkip: true} }

// claimSelf claims the run for this test process with key "o", so its
// tests' IDs are o::<name>.
func claimSelf(t *testing.T, a *Agent) {
	t.Helper()
	_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: "o"}, os.Getpid())
	require.Nil(t, serr)
}

func windowOf(t *testing.T, a *Agent, id string) models.ScopeWindow {
	t.Helper()
	ws, err := a.FinalScopeWindows(context.Background())
	require.NoError(t, err)
	for _, w := range ws {
		if w.Name == id {
			return w
		}
	}
	t.Fatalf("no window %q in %+v", id, ws)
	return models.ScopeWindow{}
}

// child starts a process that runs until killed: another test process.
func child(t *testing.T) *exec.Cmd {
	t.Helper()
	c := exec.Command("sleep", "3600")
	require.NoError(t, c.Start())
	t.Cleanup(func() {
		_ = c.Process.Kill()
		_, _ = c.Process.Wait()
	})
	return c
}

func kill(t *testing.T, c *exec.Cmd) {
	t.Helper()
	require.NoError(t, c.Process.Kill())
	_, _ = c.Process.Wait() // reaped: no zombie left to look alive
}

// One test process at a time: a claim while another claimer runs is
// refused, whether or not a test is open.
func TestAClaimWhileAnotherClaimerRunsIsRefused(t *testing.T) {
	a, _ := wireAgent(models.MODE_RECORD)
	ctx := context.Background()
	other := child(t)
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "a"}, other.Process.Pid)
	require.Nil(t, serr)
	_, serr = a.ClaimInstance(ctx, models.InstanceReq{Key: "b"}, os.Getpid())
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrOneTestProcess, serr.Code)
	require.Equal(t, http.StatusUnprocessableEntity, serr.Status)
	require.Contains(t, serr.Remedy, "go test -p 1")
}

// A claimer that exited with a test open: the test fails, and the next claim
// is accepted.
func TestAClaimerThatExitedFailsItsOpenTestAndFreesTheRun(t *testing.T) {
	a, _ := wireAgent(models.MODE_RECORD)
	ctx := context.Background()
	gone := child(t)
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "a"}, gone.Process.Pid)
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestCrash"))
	require.Nil(t, serr)
	kill(t, gone)

	_, serr = a.ClaimInstance(ctx, models.InstanceReq{Key: "b"}, os.Getpid())
	require.Nil(t, serr, "the run is free once its claimer exited")
	w := windowOf(t, a, "a::TestCrash")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.NotNil(t, w.Evidence)
	require.Contains(t, w.Evidence.Reason, "exited while the test was open")
}

// The run's windows, read after its test process exited mid-test, report
// the test failed.
func TestTheWindowsReportATestWhoseProcessExitedAsFailed(t *testing.T) {
	a, _ := wireAgent(models.MODE_TEST)
	ctx := context.Background()
	proc := child(t)
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "k"}, proc.Process.Pid)
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestRunning"))
	require.Nil(t, serr)
	kill(t, proc)
	w := windowOf(t, a, "k::TestRunning")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.Contains(t, w.Evidence.Reason, "exited while the test was open")
}

// The windows are read at the run's end: a test whose process outlived the
// test command and never ended it fails too, never left out of them.
func TestATestStillOpenAtTheFinalReadFails(t *testing.T) {
	a, _ := wireAgent(models.MODE_TEST)
	ctx := context.Background()
	proc := child(t)
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "k"}, proc.Process.Pid)
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestLingering"))
	require.Nil(t, serr)
	w := windowOf(t, a, "k::TestLingering")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.Contains(t, w.Evidence.Reason, "outlived the test command")
}

// A test begun before its process claimed the run is refused: keploy cannot
// tell that the caller's calls are captured or mocked.
func TestABeginWithoutAClaimIsRefused(t *testing.T) {
	a, _ := wireAgent(models.MODE_TEST)
	_, serr := a.ProvenBegin(context.Background(), begin("TestOrphan"))
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNotInRun, serr.Code)
	require.Equal(t, http.StatusUnprocessableEntity, serr.Status)
	ws, err := a.FinalScopeWindows(context.Background())
	require.NoError(t, err)
	require.Empty(t, ws, "the refused begin opened nothing")
}

// Misses made before a new test process claims the run were made by the
// run's earlier processes: they stay in the run's misses and are charged to
// none of the new process's tests.
func TestMissesMadeBeforeAClaimAreTheRunsNotATests(t *testing.T) {
	a, p := wireAgent(models.MODE_TEST)
	ctx := context.Background()
	first := child(t)
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "a"}, first.Process.Pid)
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	p.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /teardown"}}
	kill(t, first)

	_, serr = a.ClaimInstance(ctx, models.InstanceReq{Key: "b"}, os.Getpid())
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB", Outcome: "passed"})
	require.Nil(t, serr)
	require.Empty(t, e.Action, "a miss the earlier process made failed the new process's test")
	require.Equal(t, models.ScopeOutcomePassed, windowOf(t, a, "b::TestB").Outcome)
	all, err := a.GetMockErrors(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1, "the miss stays in the run's misses")
}

// A begin judged at its end against the counts it began with: a mapped mock
// that answered before the test began, and not during it, was not made by it.
func TestAnEndJudgesAgainstTheCountsAtItsBegin(t *testing.T) {
	a, p := wireAgent(models.MODE_TEST)
	ctx := context.Background()
	storeMocks(a, httpMock("mock-1", "POST", "http://127.0.0.1:18282/audit"))
	a.scopeTable = map[string][]string{"o::TestA": {"mock-1"}}
	p.served = map[string]uint64{"mock-1": 1} // answered an earlier test
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.Contains(t, e.Reason, "recorded call POST 127.0.0.1:18282/audit (mock-1) was never made")
}

// A wire operation waiting for another ends with its caller's context: the
// CLI's final read is never stuck behind an operation it cannot see.
func TestAWaitForTheWireEndsWithItsContext(t *testing.T) {
	a, _ := wireAgent(models.MODE_TEST)
	require.NoError(t, a.wire.lock(context.Background()))
	defer a.wire.unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.FinalScopeWindows(ctx)
	require.ErrorIs(t, err, context.Canceled)
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "o"}, os.Getpid())
	require.NotNil(t, serr)
	require.Equal(t, http.StatusServiceUnavailable, serr.Status)
	_, serr = a.ProvenBegin(ctx, begin("TestA"))
	require.NotNil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA"})
	require.NotNil(t, serr)
}

// A claim from a process outside the run's own processes is refused: keploy
// does not intercept it.
func TestAClaimFromOutsideTheRunIsRefused(t *testing.T) {
	a, _ := wireAgent(models.MODE_TEST)
	a.config.Agent.ClientNSPID = uint32(os.Getpid()) // the run is this process's own children
	stranger := os.Getppid()
	_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: "a"}, stranger)
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNotInRun, serr.Code)
	require.Contains(t, serr.Remedy, "keploy mock replay -c")
	_, serr = a.ClaimInstance(context.Background(), models.InstanceReq{Key: "a"}, child(t).Process.Pid)
	require.Nil(t, serr, "a child of the run's root is the run's")
}

func TestAClaimKeyWithTheIDSeparatorIsRefused(t *testing.T) {
	a, _ := wireAgent(models.MODE_TEST)
	for _, key := range []string{"", "a::b", ":a", "a:", "/abs", "..", "../up", "a/../../b", "x/../..", "a/b/../../..", "a\x00b", "./orders", "orders/", "a//b"} {
		_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: key}, os.Getpid())
		require.NotNil(t, serr, key)
		require.Equal(t, models.ScopeErrBadRequest, serr.Code, key)
	}
}

func TestBeginWithoutCanSkipNeedsANewIntegration(t *testing.T) {
	a, _ := wireAgent(models.MODE_TEST)
	_, serr := a.ProvenBegin(context.Background(), models.ScopeReq{Name: "TestA"})
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrUpgradeIntegration, serr.Code)
	_, serr = a.ProvenBegin(context.Background(), models.ScopeReq{Name: "suite", CanSkip: true, Suite: true})
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrUpgradeIntegration, serr.Code)
}

func TestASecondOpenTestIsRefused(t *testing.T) {
	a, _ := wireAgent(models.MODE_TEST)
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrOneTestAtATime, serr.Code)
	require.Contains(t, serr.Message, "o::TestB began while o::TestA was still open")
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.NotNil(t, serr, "a test ends once")
	require.Equal(t, models.ScopeErrNotOpen, serr.Code)
}

func TestAReplayedListedTestIsSkippedWithItsReason(t *testing.T) {
	a, p := wireAgent(models.MODE_TEST)
	ctx := context.Background()
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "orders"}, os.Getpid())
	require.Nil(t, serr)
	require.NoError(t, a.SetScopeSkips(ctx, map[string]string{"orders::TestToken": "flaky: replay_only"}))

	b, serr := a.ProvenBegin(ctx, begin("TestToken"))
	require.Nil(t, serr)
	require.Equal(t, models.ScopeBeginResp{V: 1, Status: "ok", Action: models.ScopeActionSkip, Reason: "flaky: replay_only"}, b)
	require.Zero(t, p.continues, "a skipped test opens nothing")

	// Deny list: a test it does not name runs, and nothing is open after the skip.
	b, serr = a.ProvenBegin(ctx, begin("TestOther"))
	require.Nil(t, serr)
	require.Empty(t, b.Action)
	require.Equal(t, models.ScopeOutcomeGated, windowOf(t, a, "orders::TestToken").Outcome)
}

func TestARecordNeverSkips(t *testing.T) {
	a, _ := wireAgent(models.MODE_RECORD)
	ctx := context.Background()
	claimSelf(t, a)
	require.NoError(t, a.SetScopeSkips(ctx, map[string]string{"o::TestToken": "listed"}))
	b, serr := a.ProvenBegin(ctx, begin("TestToken"))
	require.Nil(t, serr)
	require.Empty(t, b.Action)
}

func TestAnEndWithoutAnOutcomeCountsAsFailed(t *testing.T) {
	a, _ := wireAgent(models.MODE_RECORD)
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA"})
	require.Nil(t, serr)
	require.Equal(t, models.ScopeOutcomeFailed, windowOf(t, a, "o::TestA").Outcome)
}

func TestAnEndOfATestThatIsNotOpenIsRefused(t *testing.T) {
	a, _ := wireAgent(models.MODE_TEST)
	_, serr := a.ProvenEnd(context.Background(), models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNotOpen, serr.Code)
	require.Equal(t, http.StatusConflict, serr.Status)
	_, serr = a.ProvenEnd(context.Background(), models.ScopeReq{})
	require.NotNil(t, serr)
	require.Equal(t, http.StatusBadRequest, serr.Status)
}

// A miss fails the replayed test, which is recorded failed, and the run's own
// read of the misses still counts it, once.
func TestAMissFailsTheReplayedTestAndStaysInTheRunsMisses(t *testing.T) {
	a, p := wireAgent(models.MODE_TEST)
	ctx := context.Background()
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "orders"}, os.Getpid())
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestPlaceOrder"))
	require.Nil(t, serr)
	require.Equal(t, 1, p.continues, "begin opens the test's capture window, carrying the misses made since the last test")
	p.misses = []models.UnmatchedCall{
		{Protocol: "HTTP", ActualSummary: "POST /audit/42?x=secret", Destination: "127.0.0.1:18282", ReceivedReq: "body with a value"},
		{Protocol: "MySQL", ActualSummary: "SELECT * FROM users WHERE token='secret'", Destination: "db:3306"},
		{Protocol: "DNS", ActualSummary: "A example.com"},
	}
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestPlaceOrder", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.True(t, strings.HasPrefix(e.Reason, "keploy: "), e.Reason)
	require.Contains(t, e.Reason, "POST /audit/{} to 127.0.0.1:18282 had no recorded answer")
	require.Contains(t, e.Reason, "a MySQL call to db:3306 had no recorded answer")
	require.Contains(t, e.Reason, "keploy mock quarantine orders::TestPlaceOrder --reason")
	require.NotContains(t, e.Reason, "secret", "no value the app sent is rendered")
	require.NotContains(t, e.Reason, "example.com", "a DNS miss is protocol-phase")

	w := windowOf(t, a, "orders::TestPlaceOrder")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome, "a test keploy fails is recorded failed")
	require.NotNil(t, w.Evidence)
	require.Len(t, w.Evidence.Misses, 2)
	require.Empty(t, w.Evidence.Misses[0].ReceivedReq, "evidence keeps summaries only")
	require.Equal(t, "POST /audit/{}", w.Evidence.Misses[0].ActualSummary, "an HTTP miss keeps its method and path shape")
	require.Empty(t, w.Evidence.Misses[1].ActualSummary, "a query's literals never reach the evidence")

	all, err := a.GetMockErrors(ctx)
	require.NoError(t, err)
	require.Len(t, all, 3)
	again, err := a.GetMockErrors(ctx)
	require.NoError(t, err)
	require.Empty(t, again, "charged misses are returned once")
}

// A test keploy cannot judge never passes: a failed read of its misses fails
// the end, and the test is recorded failed.
func TestAnUnreadableMissListFailsTheTest(t *testing.T) {
	a, p := wireAgent(models.MODE_TEST)
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	p.missErr = errors.New("the drain did not finish")
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.NotNil(t, serr)
	require.Equal(t, http.StatusInternalServerError, serr.Status)
	w := windowOf(t, a, "o::TestA")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.Contains(t, w.Evidence.Reason, "the drain did not finish")
}

// In a replay a proxy that cannot count answers cannot judge a test, so the
// begin fails and leaves nothing open.
func TestABeginKeployCannotJudgeOpensNothing(t *testing.T) {
	a, _ := wireAgent(models.MODE_TEST)
	a.Proxy = countlessProxy{}
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.NotNil(t, serr)
	require.Equal(t, http.StatusInternalServerError, serr.Status)
	a.scopeMu.Lock()
	require.Empty(t, a.workerOpen, "the failed begin opened a window")
	a.scopeMu.Unlock()
	a.Proxy = captureless{}
	_, serr = a.ProvenBegin(ctx, begin("TestA"))
	require.NotNil(t, serr)
	require.Contains(t, serr.Message, "keep each test's misses apart")
	a.Proxy = &wireProxy{served: map[string]uint64{}}
	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr, "the failed begins left no open test behind")
}

// A begin keeps how often each mock had answered, which its end judges
// against.
func TestABeginKeepsTheCountsItBeganWith(t *testing.T) {
	a, p := wireAgent(models.MODE_TEST)
	p.served = map[string]uint64{"mock-1": 3}
	claimSelf(t, a)
	_, serr := a.ProvenBegin(context.Background(), begin("TestA"))
	require.Nil(t, serr)
	a.provenMu.Lock()
	defer a.provenMu.Unlock()
	require.Equal(t, map[string]uint64{"mock-1": 3}, a.proven.atBegin)
}

// A test that skipped itself is not failed for the recorded calls it then
// never made; a miss still fails it.
func TestASelfSkippedTestIsNotFailedForCallsItNeverMade(t *testing.T) {
	a, _ := wireAgent(models.MODE_TEST)
	storeMocks(a, httpMock("mock-1", "POST", "http://h/audit"))
	a.scopeTable = map[string][]string{"t": {"mock-1"}}
	ev, err := a.testEvidence(context.Background(), "t", nil, false)
	require.NoError(t, err)
	require.True(t, ev.Empty())
}

// The same test process claiming again while its test is open is refused;
// between tests it is a claim like any other.
func TestARepeatedClaimWhileATestIsOpenIsRefused(t *testing.T) {
	a, _ := wireAgent(models.MODE_RECORD)
	ctx := context.Background()
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "a"}, os.Getpid())
	require.Nil(t, serr)
	_, serr = a.ClaimInstance(ctx, models.InstanceReq{Key: "a"}, os.Getpid())
	require.Nil(t, serr, "between tests")
	_, serr = a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	_, serr = a.ClaimInstance(ctx, models.InstanceReq{Key: "b"}, os.Getpid())
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrOneTestAtATime, serr.Code)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr, "the open test still ends under its own key")
}

// A run keploy did not start natively (docker compose: no client) cannot
// take a claim, and says so.
func TestAClaimInARunWithoutAClientSaysWhy(t *testing.T) {
	a, _ := wireAgent(models.MODE_TEST)
	a.config.Agent.ClientNSPID = 0
	_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: "a"}, os.Getpid())
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNotInRun, serr.Code)
	require.Contains(t, serr.Message, "docker compose")
}

func TestABeginRefusesAnIDSeparatorInItsFile(t *testing.T) {
	a, _ := wireAgent(models.MODE_TEST)
	for _, req := range []models.ScopeReq{
		{Name: "t", File: "a::b", CanSkip: true},
		{Name: "t", File: ":a", CanSkip: true},
		{Name: "t", File: "a:", CanSkip: true},
		{Name: "t", Project: "p:", CanSkip: true},
		{Name: "t", Project: ":p", CanSkip: true},
	} {
		_, serr := a.ProvenBegin(context.Background(), req)
		require.NotNil(t, serr, "%+v", req)
		require.Equal(t, models.ScopeErrBadRequest, serr.Code, "%+v", req)
	}
}

func TestARecordedTestNeverFails(t *testing.T) {
	a, p := wireAgent(models.MODE_RECORD)
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	p.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /x"}}
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Empty(t, e.Action)
	require.Nil(t, windowOf(t, a, "o::TestA").Evidence)
}

func storeMocks(a *Agent, mocks ...*models.Mock) {
	a.clientMocks.Store(uint64(0), &ClientMockStorage{filtered: mocks})
}

func httpMock(name, method, url string) *models.Mock {
	return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: models.Method(method), URL: url}}}
}

// A mapped data-plane mock that did not answer while the test was open is
// an unmade call; one that answered again during the test, though it had
// answered before it, was made; a protocol-phase mock is never checked.
func TestAnUnmadeRecordedCallIsEvidence(t *testing.T) {
	a, p := wireAgent(models.MODE_TEST)
	audit := httpMock("mock-1", "POST", "http://127.0.0.1:18282/audit")
	user := httpMock("mock-2", "GET", "http://127.0.0.1:18282/users/1")
	dns := &models.Mock{Name: "mock-3", Kind: models.DNS}
	storeMocks(a, audit, user, dns)
	a.scopeTable = map[string][]string{"orders::TestPlaceOrder": {"mock-1", "mock-2", "mock-3"}}

	atBegin := map[string]uint64{"mock-1": 1, "mock-2": 1}
	p.served = map[string]uint64{"mock-1": 1, "mock-2": 2}
	ev, err := a.testEvidence(context.Background(), "orders::TestPlaceOrder", atBegin, true)
	require.NoError(t, err)
	require.Equal(t, []string{"mock-1"}, ev.Unconsumed, "the audit call was not made; DNS is protocol-phase")

	reason := evidenceReason("orders::TestPlaceOrder", ev, a.mocksFound(ev.Unconsumed))
	require.Contains(t, reason, "recorded call POST 127.0.0.1:18282/audit (mock-1) was never made")
}

// A mapping that names a mock the loaded recording lacks cannot be judged.
func TestAMappedMockThatIsNotLoadedIsAnError(t *testing.T) {
	a, _ := wireAgent(models.MODE_TEST)
	storeMocks(a, httpMock("mock-1", "GET", "http://h/x"))
	a.scopeTable = map[string][]string{"t": {"mock-1", "mock-9"}}
	_, err := a.testEvidence(context.Background(), "t", nil, true)
	require.ErrorContains(t, err, "mock-9")
}

func TestTheWireResetsWithTheSession(t *testing.T) {
	a, _ := wireAgent(models.MODE_TEST)
	ctx := context.Background()
	require.NoError(t, a.SetScopeSkips(ctx, map[string]string{"o::TestA": "listed"}))
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	require.NoError(t, a.resetSession(context.Background()))
	_, serr = a.ProvenBegin(ctx, begin("TestA"))
	require.NotNil(t, serr, "the claim belongs to the session that took it")
	claimSelf(t, a)
	b, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr, "nothing is open after a reset")
	require.Empty(t, b.Action, "the gate belongs to the session that installed it")
}

// A session start that waits on a wire operation ends with its caller: a
// stuck operation never holds the record or replay request forever.
func TestASessionStartEndsWithItsCaller(t *testing.T) {
	a, _ := wireAgent(models.MODE_TEST)
	require.NoError(t, a.wire.lock(context.Background()))
	defer a.wire.unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.resetSession(ctx) }()
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestPathShapeDropsValues(t *testing.T) {
	require.Equal(t, "/users/{}/orders", pathShape("/users/42/orders?token=abc"))
	require.Equal(t, "/v1/{}", pathShape("/v1/7c01d2e9"))
}

func TestTheReasonQuotesAnIDTheShellWouldSplit(t *testing.T) {
	r := evidenceReason("tests/a.py::test_x[1-2] y", &models.ScopeEvidence{Unconsumed: []string{"m"}}, nil)
	require.Contains(t, r, "keploy mock quarantine 'tests/a.py::test_x[1-2] y' --reason")
	require.Equal(t, "orders::TestA", shellQuote("orders::TestA"))
}

// A claim whose process exited is no claim: a begin under it is refused, so
// a test is never given another process's key.
func TestABeginUnderADeadClaimIsRefused(t *testing.T) {
	a, _ := wireAgent(models.MODE_TEST)
	ctx := context.Background()
	gone := child(t)
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "pkg1"}, gone.Process.Pid)
	require.Nil(t, serr)
	kill(t, gone)
	_, serr = a.ProvenBegin(ctx, begin("TestFromPkg2"))
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNotInRun, serr.Code)
}

// A claim whose read of the run's misses failed is not made: its retry
// reads them again, so they are never charged to its first test.
func TestAClaimWhoseMissReadFailedIsNotMade(t *testing.T) {
	a, p := wireAgent(models.MODE_TEST)
	ctx := context.Background()
	p.missErr = errors.New("the drain did not finish")
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "o"}, os.Getpid())
	require.NotNil(t, serr)
	a.provenMu.Lock()
	require.Zero(t, a.proven.claim.pid, "the claim was made")
	a.provenMu.Unlock()
	p.missErr = nil
	p.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /before"}}
	claimSelf(t, a)
	_, serr = a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Empty(t, e.Action, "the miss made before the claim failed the first test")
}

// Once the run's windows are read the run is over: a claim or a begin then
// is refused, as nothing it does would be read.
func TestNothingIsBegunAfterTheFinalRead(t *testing.T) {
	a, _ := wireAgent(models.MODE_TEST)
	ctx := context.Background()
	claimSelf(t, a)
	_, err := a.FinalScopeWindows(ctx)
	require.NoError(t, err)
	_, serr := a.ProvenBegin(ctx, begin("TestLate"))
	require.NotNil(t, serr)
	require.Equal(t, http.StatusConflict, serr.Status)
	_, serr = a.ClaimInstance(ctx, models.InstanceReq{Key: "o"}, os.Getpid())
	require.NotNil(t, serr)
	require.NoError(t, a.resetSession(context.Background()))
	claimSelf(t, a)
	_, serr = a.ProvenBegin(ctx, begin("TestNextSession"))
	require.Nil(t, serr, "a new session opens the wire again")
}

// A read that fails after the test's misses were found keeps them: they are
// its whatever else could not be told.
func TestAFailedMappingReadKeepsTheTestsMisses(t *testing.T) {
	a, p := wireAgent(models.MODE_TEST)
	storeMocks(a, httpMock("mock-1", "GET", "http://h/x"))
	a.scopeTable = map[string][]string{"o::TestA": {"mock-9"}}
	p.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /audit"}}
	ev, err := a.testEvidence(context.Background(), "o::TestA", nil, true)
	require.ErrorContains(t, err, "mock-9")
	require.NotNil(t, ev)
	require.Len(t, ev.Misses, 1)
}

// Under the switch a test recorded with no call is served only the
// reusable tier: no other test's per-test mock answers it.
func TestATestRecordedWithNoCallIsServedNoPerTestMock(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	a, p := wireAgent(models.MODE_TEST)
	storeMocks(a, httpMock("mock-1", "GET", "http://h/other-tests-call"))
	a.scopeTable = map[string][]string{"o::TestPure": {}}
	claimSelf(t, a)
	_, serr := a.ProvenBegin(context.Background(), begin("TestPure"))
	require.Nil(t, serr)
	p.mu.Lock()
	defer p.mu.Unlock()
	require.Empty(t, p.filtered, "another test's per-test mock was armed")
}

// A test whose end could not restore the pool is counted failed, in its
// window as in its answer.
func TestATestWhoseEndFailedIsCountedFailed(t *testing.T) {
	a, p := wireAgent(models.MODE_TEST)
	storeMocks(a, httpMock("mock-1", "GET", "http://h/x"))
	a.scopeTable = map[string][]string{"o::TestA": {"mock-1"}}
	p.served = map[string]uint64{}
	claimSelf(t, a)
	_, serr := a.ProvenBegin(context.Background(), begin("TestA"))
	require.Nil(t, serr)
	p.mu.Lock()
	p.served["mock-1"] = 1
	p.setErr = errors.New("the proxy refused the pool")
	p.mu.Unlock()
	_, serr = a.ProvenEnd(context.Background(), models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.NotNil(t, serr)
	p.mu.Lock()
	p.setErr = nil
	p.mu.Unlock()
	w := windowOf(t, a, "o::TestA")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.Contains(t, w.Evidence.Reason, "could not end o::TestA")
}

// A test whose name holds "::" (a subtest named after an IPv6 address) runs;
// its ID escapes the name, so the ID's last "::" still ends its path.
func TestATestNamedWithTheIDSeparatorRuns(t *testing.T) {
	a, _ := wireAgent(models.MODE_RECORD)
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestDial/[::1]:8080"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestDial/[::1]:8080", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, models.ScopeOutcomePassed, windowOf(t, a, "o::TestDial/[%3A%3A1]:8080").Outcome)
}

// A claim the agent reads as its own (its port went through its own proxy)
// is refused, saying why.
func TestTheAgentRefusesItsOwnClaim(t *testing.T) {
	was := agentPID
	agentPID = os.Getpid()
	t.Cleanup(func() { agentPID = was })
	a, _ := wireAgent(models.MODE_TEST)
	_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: "o"}, os.Getpid())
	require.NotNil(t, serr)
	require.Contains(t, serr.Message, "read itself as the caller")
}

// A read that returns misses together with an error keeps them: they fail
// the test, and stay in the run's misses.
func TestMissesReadWithAnErrorAreKept(t *testing.T) {
	a, _ := wireAgent(models.MODE_TEST)
	p := &partialProxy{wireProxy: &wireProxy{served: map[string]uint64{}}}
	a.Proxy = p
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	p.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /audit"}}
	p.err = errors.New("some misses were not read")
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.NotNil(t, serr)
	w := windowOf(t, a, "o::TestA")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.Len(t, w.Evidence.Misses, 1)
	_, err := a.GetMockErrors(ctx)
	require.Error(t, err, "the read still fails")
	p.err = nil
	all, err := a.GetMockErrors(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1, "the run's misses lost the test's")
}

// partialProxy returns its misses together with an error, as a proxy whose
// read was not whole does.
type partialProxy struct {
	*wireProxy
	misses []models.UnmatchedCall
	err    error
}

func (p *partialProxy) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	m, err := p.misses, p.err
	p.misses = nil
	return m, err
}
