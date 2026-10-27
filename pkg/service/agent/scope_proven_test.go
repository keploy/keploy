//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// The tests' agent is this test process, which also claims as the test
// process: the agent's refusal of its own claims is tested by itself.
func init() { agentPID = -1 }

// wireProxy is the part of the proxy the proven wire reads: the misses, the
// per-test capture window, the staging, and exact serving, whose consumption
// a test drives by counting answers in served.
type wireProxy struct {
	coreAgent.Proxy
	// Every call is in hand and answered when a test settles.
	settledProxy
	mu        sync.Mutex
	misses    []models.UnmatchedCall
	missErr   error
	served    map[string]uint64
	continues int
	// withdraws counts the windows taken back; order is what the capture
	// window and exact serving were told, in order: "continue", "withdraw",
	// "serve <test>".
	withdraws int
	order     []string
	// exactTest and exactNames are what exact serving was last told to
	// serve: an open test and its test mocks, or ("", the pool slice);
	// atServe is served when the test opened. setErr fails serving.
	exactTest  string
	exactNames []string
	// lastTest and lastNames are the latest test opened, open or ended: its
	// outcome is read after it stops being served.
	lastTest  string
	lastNames []string
	atServe   map[string]uint64
	setErr    error
	// poolNames are the names the agent last handed the proxy as the pool's
	// (SetPoolNames), nil until it did.
	poolNames []string
	// poolOwners are the owners handed with them (the key of each test-listed
	// mock's process), nil until they were.
	poolOwners map[string]string
	// failPools fails that many pool serves, then serves again.
	failPools int
	// staged counts the stagings (SetMocks).
	staged int
	// appClock is the app clock the agent last handed the proxy
	// (SetAppClock), nil until it did.
	appClock func(time.Time) (time.Time, bool)
}

func (p *wireProxy) SetAppClock(f func(time.Time) (time.Time, bool)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.appClock = f
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

func (p *wireProxy) ContinueTestErrorCapture() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.continues++
	p.order = append(p.order, "continue")
}

func (p *wireProxy) WithdrawTestErrorCapture() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.withdraws++
	p.order = append(p.order, "withdraw")
}

// SetMocks takes a staging of the replay's mocks.
func (p *wireProxy) SetMocks(_ context.Context, _, _ []*models.Mock) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.staged++
	return nil
}

func (p *wireProxy) ServeExactTest(test string, owned []*models.Mock) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.setErr != nil {
		return p.setErr
	}
	p.exactTest, p.exactNames = test, nil
	p.order = append(p.order, "serve "+test)
	for _, m := range owned {
		p.exactNames = append(p.exactNames, m.Name)
	}
	p.lastTest, p.lastNames = test, append([]string(nil), p.exactNames...)
	p.atServe = make(map[string]uint64, len(p.served))
	for k, v := range p.served {
		p.atServe[k] = v
	}
	return nil
}

func (p *wireProxy) SetPoolNames(names []string, owners map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.poolNames = append([]string{}, names...)
	sort.Strings(p.poolNames)
	p.poolOwners = owners
	return nil
}

func (p *wireProxy) ServeExactPool(_ string, names []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.setErr != nil {
		return p.setErr
	}
	if p.failPools > 0 {
		p.failPools--
		return errors.New("the proxy refused the pool")
	}
	p.exactTest, p.exactNames = "", append([]string(nil), names...)
	return nil
}

// ExactTestOutcome: of the latest test opened, a test mock answered when
// served moved since that test opened.
func (p *wireProxy) ExactTestOutcome() (string, []string, []string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastTest == "" {
		return "", nil, nil, true
	}
	var consumed, unconsumed []string
	for _, n := range p.lastNames {
		if p.served[n] > p.atServe[n] {
			consumed = append(consumed, n)
		} else {
			unconsumed = append(unconsumed, n)
		}
	}
	return p.lastTest, consumed, unconsumed, true
}

// exactless cannot serve each test exactly.
type exactless struct{ coreAgent.Proxy }

func (exactless) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) { return nil, nil }

// captureless serves exactly but cannot keep each test's misses apart.
type captureless struct{ coreAgent.Proxy }

func (captureless) ServeExactTest(string, []*models.Mock) error { return nil }
func (captureless) ServeExactPool(string, []string) error       { return nil }
func (captureless) ExactTestOutcome() (string, []string, []string, bool) {
	return "", nil, nil, true
}

// wireAgent is an agent whose CLI (its registered client) is this test
// process's parent, so the test process and its children are the run's. The
// proven switch is on: the wire is served only under it.
func wireAgent(t *testing.T, mode models.Mode) (*Agent, *wireProxy) {
	t.Setenv(provenswitch.Env, "1")
	p := &wireProxy{served: map[string]uint64{}}
	cfg := &config.Config{}
	cfg.Agent.Mode = mode
	cfg.Agent.ClientNSPID = uint32(os.Getppid())
	// Hooks that count capture losses and have lost nothing, marked as a
	// session start marks them (resetSession), and that count the
	// connections completed to the proxy (settle_proven.go).
	a := &Agent{logger: zap.NewNop(), Proxy: p, config: cfg, Hooks: &countHooks{}}
	a.proven.losses = a.markCaptureLosses(context.Background())
	return a, p
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
	a, _ := wireAgent(t, models.MODE_RECORD)
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
	a, _ := wireAgent(t, models.MODE_RECORD)
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
	a, _ := wireAgent(t, models.MODE_TEST)
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
	a, _ := wireAgent(t, models.MODE_TEST)
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
	a, _ := wireAgent(t, models.MODE_TEST)
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
	a, p := wireAgent(t, models.MODE_TEST)
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
	a, p := wireAgent(t, models.MODE_TEST)
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
	a, _ := wireAgent(t, models.MODE_TEST)
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
	a, _ := wireAgent(t, models.MODE_TEST)
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
	a, _ := wireAgent(t, models.MODE_TEST)
	for _, key := range []string{"", "a::b", ":a", "a:", "/abs", "..", "../up", "a/../../b", "x/../..", "a/b/../../..", "a\x00b", "./orders", "orders/", "a//b"} {
		_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: key}, os.Getpid())
		require.NotNil(t, serr, key)
		require.Equal(t, models.ScopeErrBadRequest, serr.Code, key)
	}
}

func TestBeginWithoutCanSkipNeedsANewIntegration(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	_, serr := a.ProvenBegin(context.Background(), models.ScopeReq{Name: "TestA"})
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrUpgradeIntegration, serr.Code)
	_, serr = a.ProvenBegin(context.Background(), models.ScopeReq{Name: "suite", CanSkip: true, Suite: true})
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrUpgradeIntegration, serr.Code)
}

func TestASecondOpenTestIsRefused(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
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
	a, p := wireAgent(t, models.MODE_TEST)
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
	a, _ := wireAgent(t, models.MODE_RECORD)
	ctx := context.Background()
	claimSelf(t, a)
	require.NoError(t, a.SetScopeSkips(ctx, map[string]string{"o::TestToken": "listed"}))
	b, serr := a.ProvenBegin(ctx, begin("TestToken"))
	require.Nil(t, serr)
	require.Empty(t, b.Action)
}

func TestAnEndWithoutAnOutcomeCountsAsFailed(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA"})
	require.Nil(t, serr)
	require.Equal(t, models.ScopeOutcomeFailed, windowOf(t, a, "o::TestA").Outcome)
}

func TestAnEndOfATestThatIsNotOpenIsRefused(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	_, serr := a.ProvenEnd(context.Background(), models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNotOpen, serr.Code)
	require.Equal(t, http.StatusConflict, serr.Status)
	_, serr = a.ProvenEnd(context.Background(), models.ScopeReq{})
	require.NotNil(t, serr)
	require.Equal(t, http.StatusBadRequest, serr.Status)
	require.Empty(t, refusalCodes(a), "an end with no test open refuses only that call")

	// With another test open, the harness ended a test it did not begin:
	// the open test is failed and closed, so the next test begins, and the
	// run names the cause, once.
	claimSelf(t, a)
	_, serr = a.ProvenBegin(context.Background(), begin("TestB"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(context.Background(), models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNotOpen, serr.Code)
	require.Contains(t, serr.Message, "o::TestA ended while o::TestB was the open test")
	_, serr = a.ProvenBegin(context.Background(), begin("TestC"))
	require.Nil(t, serr, "TestB was closed: TestC begins")
	require.Equal(t, []string{models.ScopeErrNotOpen}, refusalCodes(a))
	w := windowOf(t, a, "o::TestB")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.Contains(t, w.Evidence.Reason, "ended while o::TestB was the open test")
}

// A miss fails the replayed test, which is recorded failed, and the run's own
// read of the misses still counts it, once.
func TestAMissFailsTheReplayedTestAndStaysInTheRunsMisses(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
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
	require.Contains(t, e.Reason, "POST /audit/{}?x={} to 127.0.0.1:18282 had no recorded answer")
	require.Contains(t, e.Reason, "a MySQL call to db:3306 had no recorded answer")
	require.Contains(t, e.Reason, "keploy mock quarantine orders::TestPlaceOrder --reason")
	require.NotContains(t, e.Reason, "secret", "no value the app sent is rendered")
	require.NotContains(t, e.Reason, "example.com", "a DNS miss is protocol-phase")

	w := windowOf(t, a, "orders::TestPlaceOrder")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome, "a test keploy fails is recorded failed")
	require.NotNil(t, w.Evidence)
	require.Len(t, w.Evidence.Misses, 2)
	require.Empty(t, w.Evidence.Misses[0].ReceivedReq, "evidence keeps summaries only")
	require.Equal(t, "POST /audit/{}?x={}", w.Evidence.Misses[0].ActualSummary, "an HTTP miss keeps its method and URL shape: its query names, never their values")
	require.Equal(t, "SELECT * FROM users WHERE token=?", w.Evidence.Misses[1].ActualSummary, "a query's literals never reach the evidence: it keeps the statement's shape")

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
	a, p := wireAgent(t, models.MODE_TEST)
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
	require.True(t, w.Evidence.Unread, "keploy could not read what the test did: the CLI's verdict is not judged, not the test's fault")
}

// A test keploy could not read is unread on its window, and that alone: it
// is not also the run's refusal (the CLI would read it as an agent failure
// and lose the test's own row).
func TestAnUnreadTestIsNotTheRunsRefusal(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	p.missErr = errors.New("the drain did not finish")
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.NotNil(t, serr)
	require.Empty(t, refusalCodes(a))
}

// A test keploy could neither read nor end: unread on its window, and its
// end's failure (its mocks or clock not put back) is the run's, once.
func TestAnUnreadTestKeployCouldNotEndIsTheRunsRefusal(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	ctx := context.Background()
	storeMocks(a, httpMock("mock-1", "GET", "http://h/x"))
	a.scopeTable = map[string][]string{"o::TestA": {"mock-1"}}
	p.served = map[string]uint64{}
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	p.mu.Lock()
	p.missErr = errors.New("the drain did not finish")
	p.setErr = errors.New("the proxy refused the pool")
	p.mu.Unlock()
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.NotNil(t, serr)
	p.mu.Lock()
	p.setErr = nil
	p.mu.Unlock()
	require.Equal(t, []string{models.ScopeErrInternal}, refusalCodes(a))
	var msg string
	for _, w := range a.claimWindows() {
		if w.Refused != nil {
			msg = w.Refused.Message
		}
	}
	require.Contains(t, msg, "keploy could not end o::TestA (its mocks or its clock may not be put back")
	w := windowOf(t, a, "o::TestA")
	require.True(t, w.Evidence.Unread)
}

// Keploy's failure to end a test is the run's whoever ends it: here the
// next claim, once the test's process exited with it open.
func TestAFailedEndAtAClaimHandoffIsTheRunsRefusal(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	ctx := context.Background()
	storeMocks(a, httpMock("mock-1", "GET", "http://h/x"))
	a.scopeTable = map[string][]string{"a::TestCrash": {"mock-1"}}
	p.served = map[string]uint64{}
	gone := child(t)
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "a"}, gone.Process.Pid)
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestCrash"))
	require.Nil(t, serr)
	kill(t, gone)
	p.mu.Lock()
	// The end serves the pool twice, and fails both: to stop serving the
	// test before it reads it, and to restore it as the test's window
	// closes. The new claim's pool is served.
	p.failPools = 2
	p.mu.Unlock()
	_, serr = a.ClaimInstance(ctx, models.InstanceReq{Key: "b"}, os.Getpid())
	require.Nil(t, serr, "the run is free once its claimer exited")
	require.Equal(t, []string{models.ScopeErrInternal}, refusalCodes(a))
}

// In a replay a proxy that cannot serve each test exactly its own mocks
// cannot judge a test, so the begin fails and leaves nothing open.
func TestABeginKeployCannotJudgeOpensNothing(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	ctx := context.Background()
	claimSelf(t, a)
	a.Proxy = exactless{}
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

// A begin serves its test exactly its test mocks: the data-plane mocks of
// its mapping, in recorded order; a mapped protocol-phase mock stays in the
// pool, and an end serves the pool slice again.
func TestABeginServesItsTestExactlyItsTestMocks(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	later := httpMock("mock-1", "GET", "http://h/later")
	later.Spec.ReqTimestampMock = t0.Add(time.Second)
	earlier := httpMock("mock-2", "GET", "http://h/earlier")
	earlier.Spec.ReqTimestampMock = t0
	handshake := httpMock("mock-3", "GET", "http://h/token")
	handshake.Spec.Metadata = map[string]string{"type": "config"}
	storeMocks(a, later, earlier, handshake, httpMock("mock-4", "GET", "http://h/pool"))
	a.scopeTable = map[string][]string{"o::TestA": {"mock-1", "mock-2", "mock-3"}}
	ctx := context.Background()
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"mock-4"}}))
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	p.mu.Lock()
	require.Equal(t, "o::TestA", p.exactTest)
	require.Equal(t, []string{"mock-2", "mock-1"}, p.exactNames, "recorded order; the handshake is no test mock")
	require.Equal(t, 1, p.staged, "the recording is staged once")
	p.served["mock-1"], p.served["mock-2"] = 1, 1
	p.mu.Unlock()
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Empty(t, e.Action, e.Reason)
	require.Equal(t, []string{"mock-4"}, p.filteredNames())
	p.mu.Lock()
	require.Equal(t, 1, p.staged, "an end stages nothing again")
	p.mu.Unlock()
}

// A test that skipped itself is not failed for the recorded calls it then
// never made; a miss still fails it.
func TestASelfSkippedTestIsNotFailedForCallsItNeverMade(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	storeMocks(a, httpMock("mock-1", "POST", "http://h/audit"))
	a.scopeTable = map[string][]string{"t": {"mock-1"}}
	ev, err := a.testEvidence(context.Background(), "t", false)
	require.NoError(t, err)
	require.True(t, ev.Empty())
}

// The same test process claiming again while its test is open is refused;
// between tests it is a claim like any other.
func TestARepeatedClaimWhileATestIsOpenIsRefused(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
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
	a, _ := wireAgent(t, models.MODE_TEST)
	a.config.Agent.ClientNSPID = 0
	_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: "a"}, os.Getpid())
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNotInRun, serr.Code)
	require.Contains(t, serr.Message, "docker compose")
}

// A CLI in another pid namespace than the agent's (a CI step container's,
// beside a docker agent's own, which the app's containers join) said a pid
// that names another process here: a claimer that descends from that process
// is not the run's, and the claim says why. The CLI said in the agent's own
// namespace, or by an older CLI that says none, keeps its claim.
func TestAClaimUnderACLIOfAnotherPIDNamespaceIsRefused(t *testing.T) {
	own := utils.OwnPIDNamespace()
	ino, boot, ok := strings.Cut(own, "@")
	if !ok {
		t.Skipf("this process cannot say its pid namespace with a boot id (%q)", own)
	}
	n, err := strconv.ParseUint(ino, 10, 64)
	require.NoError(t, err)
	other := strconv.FormatUint(n+1, 10) + "@" + boot
	for _, c := range []struct {
		name, ns string
		taken    bool
	}{
		{"another namespace on this kernel", other, false},
		{"the agent's namespace on another kernel", ino + "@another-boot", false},
		{"a CLI in no Linux namespace", utils.ClientPIDNSNone, false},
		{"the agent's own namespace", own, true},
		{"an older CLI that says none", "", true},
	} {
		a, _ := wireAgent(t, models.MODE_TEST)
		a.config.Agent.ClientPIDNS = c.ns
		_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: "a"}, os.Getpid())
		if c.taken {
			require.Nil(t, serr, c.name)
			continue
		}
		require.NotNil(t, serr, "%s: the claim of a descendant of the process at the CLI's pid", c.name)
		require.Equal(t, models.ScopeErrNotInRun, serr.Code, c.name)
		require.Contains(t, serr.Message, "cannot tell its CLI's process", c.name)
		require.NotContains(t, serr.Message, "docker compose", c.name)
	}
}

func TestABeginRefusesAnIDSeparatorInItsFile(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
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
	a, p := wireAgent(t, models.MODE_RECORD)
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

// A test mock that did not answer while the test was open is an unmade
// call; one that answered during it, though it had answered before it, was
// made; a protocol-phase mock is no test mock.
func TestAnUnmadeRecordedCallIsEvidence(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	audit := httpMock("mock-1", "POST", "http://127.0.0.1:18282/audit")
	user := httpMock("mock-2", "GET", "http://127.0.0.1:18282/users/1")
	dns := &models.Mock{Name: "mock-3", Kind: models.DNS}
	storeMocks(a, audit, user, dns)
	a.scopeTable = map[string][]string{"orders::TestPlaceOrder": {"mock-1", "mock-2", "mock-3"}}
	p.served = map[string]uint64{"mock-1": 1, "mock-2": 1}
	require.NoError(t, a.serveTestExactly(context.Background(), "orders::TestPlaceOrder"))
	p.served["mock-2"]++

	ev, err := a.testEvidence(context.Background(), "orders::TestPlaceOrder", true)
	require.NoError(t, err)
	require.Equal(t, []string{"mock-1"}, ev.Unconsumed, "the audit call was not made; DNS is protocol-phase")

	reason := evidenceReason("orders::TestPlaceOrder", ev, a.mocksFound(ev.Unconsumed), time.Time{})
	require.Contains(t, reason, "recorded call POST 127.0.0.1:18282/audit (mock-1) was never made")
}

// A mapping that names a mock the loaded recording lacks cannot be served
// exactly: the begin refuses, and opens nothing.
func TestAMappedMockThatIsNotLoadedIsAnError(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	storeMocks(a, httpMock("mock-1", "GET", "http://h/x"))
	a.scopeTable = map[string][]string{"o::TestA": {"mock-1", "mock-9"}}
	claimSelf(t, a)
	_, serr := a.ProvenBegin(context.Background(), begin("TestA"))
	require.NotNil(t, serr)
	require.Contains(t, serr.Message, "mock-9")
	a.provenMu.Lock()
	defer a.provenMu.Unlock()
	require.Empty(t, a.proven.open)
}

func TestTheWireResetsWithTheSession(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
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
	a, _ := wireAgent(t, models.MODE_TEST)
	require.NoError(t, a.wire.lock(context.Background()))
	defer a.wire.unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.resetSession(ctx) }()
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

// Evidence keeps a miss's shape and the matcher's advice, never a value:
// an HTTP path's IDs and query, a SQL statement's literals.
func TestSanitizedMissKeepsTheShapeAndTheAdvice(t *testing.T) {
	m := sanitizedMiss(models.UnmatchedCall{
		Protocol: "MySQL", Destination: "db:3306", MatchPhase: "body_mismatch",
		ActualSummary: "COM_QUERY SELECT * FROM users WHERE email = 'a@b.c' AND id = 42 AND t = \"x\"",
		ClosestMock:   "mock-7", NextSteps: "Re-record mocks if the SQL query has changed.",
		Diff: "actual: secret", ClosestMockReq: "secret", ReceivedReq: "secret",
	})
	require.Equal(t, "COM_QUERY SELECT * FROM users WHERE email = ? AND id = ? AND t = ?", m.ActualSummary)
	require.Equal(t, "mock-7", m.ClosestMock)
	require.Equal(t, "Re-record mocks if the SQL query has changed.", m.NextSteps)
	require.Empty(t, m.Diff)
	require.Empty(t, m.ClosestMockReq)
	require.Empty(t, m.ReceivedReq)

	// The matcher's next step names the fields that differed; a map's keys
	// can be values, so a path keeps its shape.
	drift := sanitizedMiss(models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /x",
		NextSteps:  "Only values drifted (body.users.alice@example.com.score, body.items[3].sku_7c01d2e9). Re-record.",
		FieldDiffs: []models.MockFieldDiff{{Path: "body.users.alice@example.com.score"}, {Path: "body.items[3].sku_7c01d2e9"}}})
	require.Equal(t, "2 field(s) of the call differ from the recording; if the change is expected, re-record the mock set with 'keploy mock record'", drift.NextSteps)
	fixed := sanitizedMiss(models.UnmatchedCall{Protocol: "MySQL", NextSteps: "Re-record mocks if the SQL query has changed."})
	require.Equal(t, "Re-record mocks if the SQL query has changed.", fixed.NextSteps, "fixed advice is kept")
	pg := sanitizedMiss(models.UnmatchedCall{Protocol: "PostgresV2", ActualSummary: "SELECT 1"})
	require.Equal(t, "SELECT ?", pg.ActualSummary)
	redis := sanitizedMiss(models.UnmatchedCall{Protocol: "Redis", ActualSummary: "GET session:abc"})
	require.Empty(t, redis.ActualSummary, "a protocol keploy has no shape for keeps no call text")

	// A repeat note names recordings, a test and times, never a value the app
	// sent: it is kept, on a copy, so judging can name the earlier test.
	note := &models.RepeatNote{Mock: "mock-54", Test: "get-by-id-1", ReplayGapSeconds: 8, ReplayGapFromStart: true, RecordGapSeconds: 2.95, RecordGapKnown: true, RecordedLast: "mock-54"}
	rep := sanitizedMiss(models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /bp/202", ClosestMock: "mock-54", Repeat: note})
	require.NotNil(t, rep.Repeat, "the repeat note was dropped")
	require.Equal(t, *note, *rep.Repeat)
	require.NotSame(t, note, rep.Repeat, "the note is the miss's own: written on a copy")
}

// A miss, its reason text and a recorded mock's name keep an HTTP call's
// method and its URL's shape (models.PathShape): no value in the URL that
// its look, or a test variable's place, marks.
func TestEvidenceKeepsAnHTTPCallsShape(t *testing.T) {
	miss := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /users/alice@example.com/orders/ord_9f8e7d?token=s3cret", Destination: "api.internal:8080"}
	require.Equal(t, "GET /users/{}/orders/{}?token={}", sanitizedMiss(miss).ActualSummary)
	require.Equal(t, "GET /users/{}/orders/{}?token={} to api.internal:8080", missText(miss))
	mock := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "DELETE", URL: "http://pay.internal:8443/carts/507f1f77bcf86cd799439011?force=true"}}}
	require.Equal(t, "DELETE pay.internal:8443/carts/{}?force={} (mock-3)", mockText("mock-3", mock))
	grpc := models.UnmatchedCall{Protocol: "gRPC", ActualSummary: "/orders.v1.OrderService/PlaceOrder", Destination: "orders:9090"}
	require.Equal(t, "/orders.v1.OrderService/PlaceOrder to orders:9090", missText(sanitizedMiss(grpc)), "a gRPC miss names its RPC")
	// A test variable's place names a path segment that holds the app's
	// value: it is {}, whatever its look.
	variable := sanitizedMiss(models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /orders/kqzxwv", MatchPhase: models.MatchPhaseVariable, ClosestMock: "mock-1",
		FieldDiffs: []models.MockFieldDiff{{Path: "url.path[1]", Expected: "kqzxwv", Actual: "kqzxwv"}}})
	require.Equal(t, "GET /orders/{}", variable.ActualSummary)
	require.Equal(t, "GET /orders/{}", missText(variable))
	kept := sanitizedMiss(models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /orders/kqzxwv", MatchPhase: models.MatchPhaseVariable,
		VariablePlaces: []string{"url.path[1]", "body.users.alice@example.com"}})
	require.Equal(t, "GET /orders/{}", kept.ActualSummary)
	require.Equal(t, []string{"url.path[1]"}, kept.VariablePlaces, "a body place can name a JSON key that is a value: evidence keeps path places only")
	declared := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "GET", URL: "http://store/orders/kqzxwv"},
		Variables: models.MockVariables{"orderId": {Recorded: "kqzxwv", Bind: "url.path[1]"}}}}
	require.Equal(t, "GET store/orders/{} (mock-1)", mockText("mock-1", declared), "a mock's declared variable place holds a recorded value")
	r := evidenceReason("orders::TestA", &models.ScopeEvidence{Misses: []models.UnmatchedCall{sanitizedMiss(miss)}, Unconsumed: []string{"mock-3"}}, map[string]*models.Mock{"mock-3": mock}, time.Time{})
	for _, v := range []string{"alice@example.com", "ord_9f8e7d", "s3cret", "507f1f77bcf86cd799439011", "true"} {
		require.NotContains(t, r, v)
	}
}

func TestTheReasonQuotesAnIDTheShellWouldSplit(t *testing.T) {
	r := evidenceReason("tests/a.py::test_x[1-2] y", &models.ScopeEvidence{Unconsumed: []string{"m"}}, nil, time.Time{})
	require.Contains(t, r, "keploy mock quarantine 'tests/a.py::test_x[1-2] y' --reason")
	require.Equal(t, "orders::TestA", shellQuote("orders::TestA"))
}

// A claim whose process exited is no claim: a begin under it is refused, so
// a test is never given another process's key.
func TestABeginUnderADeadClaimIsRefused(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
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
	a, p := wireAgent(t, models.MODE_TEST)
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
	a, _ := wireAgent(t, models.MODE_TEST)
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
// its whatever else could not be told. Here the proxy was not serving the
// test exactly at its end.
func TestAFailedConsumptionReadKeepsTheTestsMisses(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	storeMocks(a, httpMock("mock-1", "GET", "http://h/x"))
	a.scopeTable = map[string][]string{"o::TestA": {"mock-1"}}
	p.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /audit"}}
	ev, err := a.testEvidence(context.Background(), "o::TestA", true)
	require.ErrorContains(t, err, "was not serving o::TestA exactly")
	require.NotNil(t, ev)
	require.Len(t, ev.Misses, 1)
}

// A test recorded with no call, or one the mapping omits, owns no test mock:
// no other test's mock answers it.
func TestATestRecordedWithNoCallIsServedNoPerTestMock(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	storeMocks(a, httpMock("mock-1", "GET", "http://h/other-tests-call"))
	a.scopeTable = map[string][]string{"o::TestPure": {}, "o::TestOther": {"mock-1"}}
	claimSelf(t, a)
	ctx := context.Background()
	for _, name := range []string{"TestPure", "TestUnmapped"} {
		_, serr := a.ProvenBegin(ctx, begin(name))
		require.Nil(t, serr)
		p.mu.Lock()
		require.Equal(t, "o::"+name, p.exactTest)
		require.Empty(t, p.exactNames, "another test's mock was served to %s", name)
		p.mu.Unlock()
		e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: name, Outcome: "passed"})
		require.Nil(t, serr)
		require.Empty(t, e.Action, "a test that owns nothing and called nothing passes")
	}
}

// A test whose end could not restore the pool is counted failed, in its
// window as in its answer.
func TestATestWhoseEndFailedIsCountedFailed(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
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
	refusals := refusalCodes(a)
	w := windowOf(t, a, "o::TestA")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.Contains(t, w.Evidence.Reason, "could not end o::TestA")
	require.False(t, w.Evidence.Unread, "keploy read what the test did")
	require.Equal(t, []string{models.ScopeErrInternal}, refusals, "keploy could not end the test: the run's, never the test's evidence")
}

// refusalCodes are the codes of the refusals the run has kept.
func refusalCodes(a *Agent) []string {
	var out []string
	for _, w := range a.claimWindows() {
		if w.Refused != nil {
			out = append(out, w.Refused.Code)
		}
	}
	return out
}

// A test whose name holds "::" (a subtest named after an IPv6 address) runs;
// its ID escapes the name, so the ID's last "::" still ends its path.
func TestATestNamedWithTheIDSeparatorRuns(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
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
	a, _ := wireAgent(t, models.MODE_TEST)
	_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: "o"}, os.Getpid())
	require.NotNil(t, serr)
	require.Contains(t, serr.Message, "read itself as the caller")
}

// A read that returns misses together with an error keeps them: they fail
// the test, and stay in the run's misses.
func TestMissesReadWithAnErrorAreKept(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
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

// A read that could not vouch for every miss (ErrUnmatchedCallsUnread) is
// answered with what it read, the misses already charged to tests included:
// the run's last read may be one, and nothing reads after it.
func TestAnUnreadReadKeepsTheChargedMisses(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	p := &partialProxy{wireProxy: &wireProxy{served: map[string]uint64{}}}
	a.Proxy = p
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	p.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /audit"}}
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	p.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /late"}}
	p.err = fmt.Errorf("%w: the drain stopped", models.ErrUnmatchedCallsUnread)
	all, err := a.GetMockErrors(ctx)
	require.ErrorIs(t, err, models.ErrUnmatchedCallsUnread)
	require.Len(t, all, 2, "the charged miss and the partial read")
}

// filteredNames is what exact serving was last told to serve: the open
// test's test mocks, or the pool slice between tests.
func (p *wireProxy) filteredNames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.exactNames...)
}

// Between tests the agent serves the pool slice of the current claim's key
// (design v14 §5.2, U21): before any claim the keyless slice, after a claim
// its key's, during a test the test's own mocks, and after it the key's
// slice again. Another key's pool mock never answers.
func TestBetweenTestsTheCurrentKeysPoolSliceIsServed(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	storeMocks(a,
		httpMock("mock-1", "GET", "http://h/orders-setup"),
		httpMock("mock-2", "GET", "http://h/before-any-claim"),
		httpMock("mock-3", "GET", "http://h/test-a"),
		httpMock("mock-4", "GET", "http://h/users-setup"))
	ctx := context.Background()
	a.scopeTable = map[string][]string{"o::TestA": {"mock-3"}}
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"mock-1"}, models.PoolKeyNone: {"mock-2"}, "users": {"mock-4"}}))
	require.Equal(t, []string{"mock-2"}, p.filteredNames(), "before any claim: the keyless slice")
	claimSelf(t, a)
	require.Equal(t, []string{"mock-1"}, p.filteredNames(), "after the claim: its key's slice")
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	require.Equal(t, []string{"mock-3"}, p.filteredNames(), "during the test: its own mocks")
	p.served["mock-3"] = 1
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, []string{"mock-1"}, p.filteredNames(), "after the test: the key's slice again")
}

// The proxy is told every name the table's pool slices hold, of every key:
// what the learn steps read as the pool's when the replay sends no recorded
// windows. An empty table tells it no mock is the pool's.
func TestTheScopePoolTellsTheProxyWhatIsThePools(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	storeMocks(a, httpMock("mock-1", "GET", "http://h/a"), httpMock("mock-2", "GET", "http://h/b"), httpMock("mock-4", "GET", "http://h/c"))
	ctx := context.Background()
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"mock-1"}, models.PoolKeyNone: {"mock-2"}, "users": {"mock-4"}}))
	require.Equal(t, []string{"mock-1", "mock-2", "mock-4"}, p.poolNames)
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{}))
	require.NotNil(t, p.poolNames)
	require.Empty(t, p.poolNames)
}

// With the pool names, the proxy is told the key of the test process whose
// tests list each mock (teardownOwners), which bounds each process's
// teardown: a test's key is its ID's path where that path is a pool slice's
// key. A test whose ID names its test file, a test of a key with no pool
// slice (or the keyless one), and a mock tests of two keys list give "":
// such a mock bounds every process's teardown.
func TestTheScopePoolTellsTheProxyWhoseTestsListEachMock(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	storeMocks(a, httpMock("mock-1", "GET", "http://h/a"), httpMock("mock-2", "GET", "http://h/b"))
	a.scopeTable = map[string][]string{
		"a::TestA1":             {"m-a1", "m-shared"},
		"proj::a::TestA2":       {"m-a2"},
		"b::TestB":              {"m-b", "m-shared"},
		"a/a_test.go::TestFile": {"m-file"},
		"c::TestC":              {"m-c"},
		"-::TestNone":           {"m-none"},
		"TestBare":              {"m-bare"},
		"b::TestB/[%3A%3A1]":    {"m-b2"},
	}
	require.NoError(t, a.SetScopePool(context.Background(), map[string][]string{"a": {"mock-1"}, "b": {"mock-2"}, models.PoolKeyNone: {}}))
	require.Equal(t, map[string]string{
		"m-a1": "a", "m-a2": "a", "m-b": "b", "m-b2": "b",
		"m-shared": "", "m-file": "", "m-c": "", "m-none": "", "m-bare": "",
	}, p.poolOwners)
}

// Without the slices (a CLI that sent none) no data-plane pool mock answers
// between tests: the whole pool is never armed.
func TestWithoutPoolSlicesNoPoolMockIsServed(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	storeMocks(a, httpMock("mock-1", "GET", "http://h/x"), httpMock("mock-3", "GET", "http://h/test-a"))
	ctx := context.Background()
	a.scopeTable = map[string][]string{"o::TestA": {"mock-3"}}
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	p.served["mock-3"] = 1
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Empty(t, p.filteredNames())
	p.mu.Lock()
	defer p.mu.Unlock()
	require.Empty(t, p.exactTest)
}

// Each claim is in the run's windows, in order, so a record tags each pool
// mock with the key it was made under; a re-claim by the claimer with the
// same key adds none.
func TestTheClaimsAreInTheWindows(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
	claimSelf(t, a)
	claimSelf(t, a)
	ws, err := a.GetScopeWindows(context.Background())
	require.NoError(t, err)
	var claims []string
	for _, w := range ws {
		if w.Claim != "" {
			claims = append(claims, w.Claim)
			require.False(t, w.Start.IsZero())
		}
	}
	require.Equal(t, []string{"o"}, claims)
	require.NoError(t, a.resetSession(context.Background()))
	ws, err = a.GetScopeWindows(context.Background())
	require.NoError(t, err)
	for _, w := range ws {
		require.Empty(t, w.Claim, "the claims belong to the session that made them")
	}
}

// An empty pool (a recording with no start-up mock) is a pool: between
// tests no pool mock answers, and no test's own mock.
func TestAnEmptyPoolServesNoPoolMock(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	storeMocks(a, httpMock("mock-1", "GET", "http://h/test-a"), httpMock("mock-2", "GET", "http://h/test-b"))
	ctx := context.Background()
	a.scopeTable = map[string][]string{"o::TestA": {"mock-1"}, "o::TestB": {"mock-2"}}
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{}))
	require.Empty(t, p.filteredNames(), "before any claim: armed with no pool mock")
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	p.served["mock-1"] = 1
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Empty(t, p.filteredNames(), "between tests no test's mock answers")
}

// A refusal is in the run's windows, once when repeated, and the session
// keeps a bounded number of them.
func TestRefusalsAreInTheWindows(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	r := models.RunRefusal{Code: models.ScopeErrOneTestProcess, Message: "a second test process"}
	a.NoteRefusal(r)
	a.NoteRefusal(r)
	a.NoteRefusal(models.RunRefusal{Code: "go_test_parallel", Message: "go test -p 2"})
	ws, err := a.GetScopeWindows(context.Background())
	require.NoError(t, err)
	var got []string
	for _, w := range ws {
		if w.Refused != nil {
			got = append(got, w.Refused.Code)
		}
	}
	require.Equal(t, []string{models.ScopeErrOneTestProcess, "go_test_parallel"}, got)
	for i := 0; i < 2*maxRefusals; i++ {
		a.NoteRefusal(models.RunRefusal{Code: "c", Message: fmt.Sprint(i)})
	}
	require.Len(t, a.claimWindows(), maxRefusals)
	require.NoError(t, a.resetSession(context.Background()))
	require.Empty(t, a.claimWindows(), "the refusals belong to their session")
}

// A variable that did not hold keeps, as evidence, the place it did not hold
// at, never a value.
func TestSanitizedMissKeepsTheVariablesPlaceNotItsValues(t *testing.T) {
	m := sanitizedMiss(models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /orders/o9abc", ClosestMock: "mock-2",
		MatchPhase: models.MatchPhaseVariable, Diff: `variable orderId is "o1abc" ...`,
		NextSteps:  "the application sent another value at url.path[1] than the one it made in this test",
		FieldDiffs: []models.MockFieldDiff{{Path: "url.path[1]", Kind: models.DiffKindVariable, Expected: "o1abc", Actual: "o9abc"}}})
	if len(m.FieldDiffs) != 1 || m.FieldDiffs[0].Path != "url.path[1]" || m.FieldDiffs[0].Expected != "" || m.FieldDiffs[0].Actual != "" {
		t.Fatalf("field diffs %+v", m.FieldDiffs)
	}
	if m.Diff != "" || strings.Contains(m.NextSteps, "o1abc") || strings.Contains(m.NextSteps, "o9abc") || !strings.Contains(m.NextSteps, "mock-2") {
		t.Fatalf("evidence %+v", m)
	}
	// A body place keeps only that it is in the body: a JSON key can be a
	// value the app sent.
	b := sanitizedMiss(models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /x", ClosestMock: "mock-3", MatchPhase: models.MatchPhaseVariable,
		FieldDiffs: []models.MockFieldDiff{
			{Path: `body.users["alice@x.com"].id`, Kind: models.DiffKindTypeChanged, Expected: `"1"`, Actual: "1"},
			{Path: `body[0]["bob@x.com"]`, Kind: models.DiffKindVariable, Expected: "a", Actual: "b"},
		}})
	if len(b.FieldDiffs) != 2 || b.FieldDiffs[0].Path != "body" || b.FieldDiffs[0].Kind != models.DiffKindTypeChanged || b.FieldDiffs[0].Expected != "" || b.FieldDiffs[1].Path != "body" {
		t.Fatalf("body places kept as %+v", b.FieldDiffs)
	}
}

// The proven wire carries no pid, runner clock, folder or suite: a begin
// or an end that sends one predates the protocol, and is refused, naming
// them: a refused begin opens nothing, and a refused end fails and closes
// the test it names. A v1 body runs.
func TestTheWireRefusesThePidClockAndFolderOfTheOldWire(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	ctx := context.Background()
	claimSelf(t, a)
	for _, tc := range []struct {
		req  models.ScopeReq
		sent string
	}{
		{models.ScopeReq{Name: "TestA", CanSkip: true, Pid: 4242}, "pid"},
		{models.ScopeReq{Name: "TestA", CanSkip: true, At: time.Unix(1_800_000_000, 0)}, "at"},
		{models.ScopeReq{Name: "TestA", CanSkip: true, Dir: "/repo/o"}, "dir"},
		{models.ScopeReq{Name: "TestA", CanSkip: true, Pid: 1, Dir: "/repo/o"}, "pid, dir"},
	} {
		_, serr := a.ProvenBegin(ctx, tc.req)
		require.NotNil(t, serr, tc.sent)
		require.Equal(t, models.ScopeErrUpgradeIntegration, serr.Code, tc.sent)
		require.Contains(t, serr.Message, "sends "+tc.sent+", which keploy's scope protocol v1 does not use", tc.sent)
	}
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr, "nothing was opened by a refused begin")
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed", Pid: 4242})
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrUpgradeIntegration, serr.Code)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed", Suite: true})
	require.NotNil(t, serr)
	require.Contains(t, serr.Message, "sends suite")
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.NotNil(t, serr, "the refused end failed and closed the test it named")
	require.Equal(t, models.ScopeErrNotOpen, serr.Code)
}

// An end the wire refuses for the old wire's fields fails and closes the
// test it names, with keploy's reason: left open, the next begin would be
// refused one_test_at_a_time and the run blamed for two tests at once.
func TestAnEndWithTheOldWiresFieldsFailsTheOpenTest(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed", Pid: 4242})
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrUpgradeIntegration, serr.Code)
	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr, "the refused end closed TestA: TestB begins")
	w := windowOf(t, a, "o::TestA")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.NotNil(t, w.Evidence)
	require.Contains(t, w.Evidence.Reason, "sends pid")
}

// A harness refused at every test, by turns at its begin and its end, adds
// each refusal once, and a refusal it has not made is still kept.
func TestARefusalIsKeptOnceWhateverCameBetween(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	begin := models.RunRefusal{Code: models.ScopeErrUpgradeIntegration, Message: "predates protocol v1"}
	end := models.RunRefusal{Code: models.ScopeErrUpgradeIntegration, Message: "sends pid"}
	for i := 0; i < 2*maxRefusals; i++ {
		a.NoteRefusal(begin)
		a.NoteRefusal(end)
	}
	process := models.RunRefusal{Code: models.ScopeErrOneTestProcess, Message: "a second test process"}
	a.NoteRefusal(process)
	ws := a.claimWindows()
	var got []models.RunRefusal
	for _, w := range ws {
		if w.Refused != nil {
			got = append(got, *w.Refused)
		}
	}
	require.Equal(t, []models.RunRefusal{begin, end, process}, got)
}

// A miss's reason names the fields in which the call differed from its
// closest recording (row S1: "GET /users/{} had no recorded answer
// (body.role differs)"), and never a value: a map key the app sent (an ID,
// an email address) becomes {}, as a path's segments do.
func TestAMissNamesWhatDiffersWithoutValues(t *testing.T) {
	// The matcher names fields so already (it shapes each key); the
	// evidence shapes each part again, for a field named by any other means.
	miss := sanitizedMiss(models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /charge", Destination: "pay.internal",
		Differs: []string{"body.amount", "body.users.alice@example.role", "body.orders.123456.total", "header.authorization", "body.items[].sku9X0001234[]", "header.x-b3-traceid"}})
	// A header's name is the protocol's: never written {}.
	require.Equal(t, []string{"body.amount", "body.users.{}.role", "body.orders.{}.total", "header.authorization", "body.items[].{}[]", "header.x-b3-traceid"}, miss.Differs)
	reason := evidenceReason("orders::TestCharge", &models.ScopeEvidence{Misses: []models.UnmatchedCall{miss}}, nil, time.Time{})
	require.Contains(t, reason, "POST /charge to pay.internal had no recorded answer (body.amount, body.users.{}.role, body.orders.{}.total and 3 more differ)")
	require.NotContains(t, reason, "alice")

	// The path-shape rule names both the call and its differing fields: no
	// ID, token or random key the app sent reaches the reason.
	shaped := sanitizedMiss(models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /charge/a1b2", Destination: "pay.internal",
		Differs: []string{"body.users.kqzx9w.role", "body.keys.NffrFeUfNVaHib", "query.ord_9f8e"}})
	shapedReason := evidenceReason("orders::TestCharge", &models.ScopeEvidence{Misses: []models.UnmatchedCall{shaped}}, nil, time.Time{})
	require.Contains(t, shapedReason, "POST /charge/{} to pay.internal had no recorded answer (body.users.{}.role, body.keys.{} and query.{} differ)")
	for _, v := range []string{"a1b2", "kqzx9w", "NffrFeUfNVaHib", "ord_9f8e"} {
		require.NotContains(t, shapedReason, v)
	}
	// The reason shapes what differs itself, part by part, for a miss not
	// shaped before (a backstop: the strict comparison shapes each key).
	raw := evidenceReason("t", &models.ScopeEvidence{Misses: []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /charge",
		Differs: []string{"body.users.kqzx9w.role"}}}}, nil, time.Time{})
	require.Contains(t, raw, "POST /charge had no recorded answer (body.users.{}.role differs)")

	one := evidenceReason("t", &models.ScopeEvidence{Misses: []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /charge", Differs: []string{"body.amount"}}}}, nil, time.Time{})
	require.Contains(t, one, "POST /charge had no recorded answer (body.amount differs)")
	two := evidenceReason("t", &models.ScopeEvidence{Misses: []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /x", Differs: []string{"host", "header.accept"}}}}, nil, time.Time{})
	require.Contains(t, two, "GET /x had no recorded answer (host and header.accept differ)")
	none := evidenceReason("t", &models.ScopeEvidence{Misses: []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /x"}}}, nil, time.Time{})
	require.Contains(t, none, "GET /x had no recorded answer.")
}

// A miss's reason says why a replay refused a field for a reason of its own
// (models.UnmatchedCall.FieldReasons: a learned read of the application's
// clock the call does not carry), the field and why, after the fields that
// differ, wherever the miss was made; never a value: the evidence keeps only
// a reason whose field is its own shape (an object key the app sent may be
// a value its why quotes), once.
func TestAMissSaysWhyAFieldWasRefused(t *testing.T) {
	since := "body.since was learned as a read of the application's clock, and the value this call carries is not a read of the application's clock made in this call's window: it changed"
	ttl := "body.expires was learned as clock + 30m, a read of the application's clock plus that offset, and the value this call carries is not a read of the application's clock plus that offset made in this call's window: it changed"
	miss := sanitizedMiss(models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /v1/usage", Destination: "api.internal",
		Differs:      []string{"body.since", "body.users.kqzx9w.since"},
		FieldReasons: []models.FieldReason{{Field: "body.since", Why: since}, {Field: "body.users.kqzx9w.since", Why: "body.users.kqzx9w.since was learned as a read of the application's clock"}, {Field: "body.since", Why: since}}})
	require.Equal(t, []models.FieldReason{{Field: "body.since", Why: since}}, miss.FieldReasons)
	began := time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name    string
		kind    string
		at      time.Time
		beganAt time.Time
		where   string
	}{
		{"in the test", models.MissInTest, began.Add(time.Second), began, " had no recorded answer"},
		{"before it began", models.MissInTest, began.Add(-time.Second), began, ", made before this one began, had no recorded answer"},
		{"as it ended", models.MissOutsideTest, began.Add(time.Second), began, ", made as this test ended, had no recorded answer"},
		{"outside every test", models.MissOutsideTest, time.Time{}, time.Time{}, ", made outside every test before this one began, had no recorded answer"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := miss
			m.Kind, m.At = c.kind, c.at
			reason := evidenceReason("usage::TestUsage", &models.ScopeEvidence{Misses: []models.UnmatchedCall{m}}, nil, c.beganAt)
			require.Contains(t, reason, "GET /v1/usage to api.internal"+c.where+" (body.since and body.users.{}.since differ: "+since+")")
			require.NotContains(t, reason, "kqzx9w")
		})
	}
	// Not sanitized first, the reason still drops a why its field may quote
	// a value in; with no field named as differing it gives the why alone,
	// and two are joined.
	two := evidenceReason("t", &models.ScopeEvidence{Misses: []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /x",
		FieldReasons: []models.FieldReason{{Field: "body.since", Why: since}, {Field: "body.users.kqzx9w.since", Why: "kqzx9w"}, {Field: "body.expires", Why: ttl}}}}}, nil, time.Time{})
	require.Contains(t, two, "GET /x had no recorded answer ("+since+"; "+ttl+")")
	require.NotContains(t, two, "kqzx9w")
	plain := evidenceReason("t", &models.ScopeEvidence{Misses: []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /x", Differs: []string{"body.q"}}}}, nil, time.Time{})
	require.Contains(t, plain, "GET /x had no recorded answer (body.q differs).")

	// The run's summary shows a miss's next step as evidence keeps it
	// (nextStepOf): a next step that names a field the call differs in is
	// replaced by fixed advice, and the why still reaches the summary, with
	// no value; the matcher's own advice keeps it too, once.
	advice := "2 field(s) of the call differ from the recording; if the change is expected, re-record the mock set with 'keploy mock record'"
	mongo := sanitizedMiss(models.UnmatchedCall{Protocol: "MongoDB", Destination: "127.0.0.1:27017", MatchPhase: "body_mismatch", ClosestMock: "mock-12",
		NextSteps:    "fields differ: body.since, body.users.kqzx9w.since",
		FieldDiffs:   []models.MockFieldDiff{{Path: "body.since"}, {Path: "body.users.kqzx9w.since"}},
		FieldReasons: []models.FieldReason{{Field: "body.since", Why: since}, {Field: "body.users.kqzx9w.since", Why: "kqzx9w"}}})
	require.Equal(t, advice+" ("+since+")", mongo.NextSteps)
	own := sanitizedMiss(models.UnmatchedCall{Protocol: "HTTP", NextSteps: "re-record the test",
		FieldReasons: []models.FieldReason{{Field: "body.since", Why: since}, {Field: "body.expires", Why: ttl}}})
	require.Equal(t, "re-record the test ("+since+"; "+ttl+")", own.NextSteps)
	said := sanitizedMiss(models.UnmatchedCall{Protocol: "HTTP", NextSteps: since, FieldReasons: []models.FieldReason{{Field: "body.since", Why: since}}})
	require.Equal(t, since, said.NextSteps, "a why the advice says already is not said twice")
	bare := sanitizedMiss(models.UnmatchedCall{Protocol: "HTTP", FieldReasons: []models.FieldReason{{Field: "body.since", Why: since}}})
	require.Equal(t, since, bare.NextSteps)

	// A Postgres miss's place names (bind.$N, literal.N) are positions, not
	// values: its why reaches the test's reason, with the place it names.
	bound := "bind.$1 was learned as clock, a read of the application's clock, and the call sends no value there: it changed"
	pg := sanitizedMiss(models.UnmatchedCall{Protocol: "Postgres", ActualSummary: "INSERT INTO audit (at) VALUES ($1)", Destination: "db:5432",
		Differs: []string{"bind.$1"}, FieldReasons: []models.FieldReason{{Field: "bind.$1", Why: bound}}})
	require.Equal(t, []models.FieldReason{{Field: "bind.$1", Why: bound}}, pg.FieldReasons)
	require.Equal(t, []string{"bind.$1"}, pg.Differs)
	pgReason := evidenceReason("t", &models.ScopeEvidence{Misses: []models.UnmatchedCall{pg}}, nil, time.Time{})
	require.Contains(t, pgReason, " (bind.$1 differs: "+bound+")")
}

// endOrderProxy notes when the test stops being served and when its outcome
// is read.
type endOrderProxy struct {
	*wireProxy
	events []string
}

func (p *endOrderProxy) ServeExactPool(key string, names []string) error {
	p.events = append(p.events, "pool")
	return p.wireProxy.ServeExactPool(key, names)
}

func (p *endOrderProxy) ExactTestOutcome() (string, []string, []string, bool) {
	p.events = append(p.events, "outcome")
	return p.wireProxy.ExactTestOutcome()
}

// A test's end stops serving it before it reads what the test did: a call
// that lands after the read cannot consume one of its mocks (round-2 review).
func TestAnEndStopsServingTheTestBeforeItReadsTheOutcome(t *testing.T) {
	a, wp := wireAgent(t, models.MODE_TEST)
	p := &endOrderProxy{wireProxy: wp}
	a.Proxy = p
	storeMocks(a, httpMock("mock-1", "GET", "http://h/a"))
	ctx := context.Background()
	a.scopeTable = map[string][]string{"o::TestA": {"mock-1"}}
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{}))
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	wp.served["mock-1"] = 1
	p.events = nil
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	require.Empty(t, e.Action, e.Reason)
	require.GreaterOrEqual(t, len(p.events), 2)
	require.Equal(t, []string{"pool", "outcome"}, p.events[:2], "the outcome was read while the test was still served")
}

// A connection whose client sent nothing (models.MatchPhaseNoRequest) made no
// call: it fails no test, and the run still reads it, for its next step.
func TestAConnectionThatSentNothingFailsNoTest(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	ctx := context.Background()
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "orders"}, os.Getpid())
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestPlaceOrder"))
	require.Nil(t, serr)
	p.misses = []models.UnmatchedCall{{Protocol: "MySQL", Destination: "10.9.9.9:13306", MatchPhase: models.MatchPhaseNoRequest}}
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestPlaceOrder", Outcome: "passed"})
	require.Nil(t, serr)
	require.Empty(t, e.Action, e.Reason)
	all, err := a.GetMockErrors(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1, "the run still reads it, for its next step")
}

// clockHook is the no-op hooks with an app clock that runs offset past the
// real one (AppClockReader).
type clockHook struct {
	AgentHook
	offset time.Duration
}

func (h *clockHook) AppClock(real time.Time) (time.Time, bool) { return real.Add(h.offset), true }

// otherHooks are hooks of another build that report no app clock: they may
// move it, so keploy cannot tell which clock the app reads.
type otherHooks struct{ AgentHook }

// Once the mocks are stored, the proxy reads the app's clock as the hooks
// move it. Under the proven switch this build's own hooks, which move no
// clock, hand it the real clock; hooks of another build that report none
// hand it nothing (keploy cannot tell), and nor does any hook with the switch
// off, as before it.
func TestStoredMocksHandTheProxyTheHooksAppClock(t *testing.T) {
	prev := ActiveHooks
	t.Cleanup(func() { ActiveHooks = prev })
	at := time.Date(2026, 10, 20, 15, 50, 35, 0, time.UTC)

	ActiveHooks = &AgentHook{}
	a, p := wireAgent(t, models.MODE_TEST)
	t.Setenv(provenswitch.Env, "")
	a.finalizeClientMocks(context.Background(), &ClientMockStorage{})
	require.Nil(t, p.appClock, "the switch off: no app clock, as before")

	t.Setenv(provenswitch.Env, "1")
	ActiveHooks = &otherHooks{}
	a, p = wireAgent(t, models.MODE_TEST)
	a.finalizeClientMocks(context.Background(), &ClientMockStorage{})
	require.Nil(t, p.appClock, "another build's hooks that report no app clock: keploy cannot tell it")

	ActiveHooks = &AgentHook{}
	a.finalizeClientMocks(context.Background(), &ClientMockStorage{})
	require.NotNil(t, p.appClock, "this build's own hooks move no clock: the app reads the real one")
	got, ok := p.appClock(at)
	require.True(t, ok)
	require.Equal(t, at, got)

	ActiveHooks = &clockHook{offset: 21 * time.Second}
	a.finalizeClientMocks(context.Background(), &ClientMockStorage{})
	require.NotNil(t, p.appClock)
	got, ok = p.appClock(at)
	require.True(t, ok)
	require.Equal(t, at.Add(21*time.Second), got)
}

// recBeginProxy is a wireProxy that takes a test's recorded begin
// (coreAgent.RecordedBeginTaker), and says in which order it was given it and
// opened the test's capture window.
type recBeginProxy struct {
	*wireProxy
	order []string
	taken []time.Time
}

func (p *recBeginProxy) TakeRecordedBegin(at time.Time) {
	p.order, p.taken = append(p.order, "recorded begin"), append(p.taken, at)
}

func (p *recBeginProxy) ContinueTestErrorCapture() {
	p.order = append(p.order, "open")
	p.wireProxy.ContinueTestErrorCapture()
}

// A replayed test's begin gives the proxy's learn step the instant the test
// began at in its recording, before the test's capture window opens (the
// tracker begins the test with it): a test the recording lists no begin of
// gives none.
func TestABeginGivesTheLearnStepItsRecordedBegin(t *testing.T) {
	a, wp := wireAgent(t, models.MODE_TEST)
	p := &recBeginProxy{wireProxy: wp}
	a.Proxy = p
	ctx := context.Background()
	claimSelf(t, a)
	at := time.Date(2026, 10, 22, 11, 14, 15, 0, time.UTC)
	require.NoError(t, a.SetTestBegins(ctx, map[string]time.Time{"o::TestA": at}, nil))
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	require.Equal(t, []string{"recorded begin", "open", "recorded begin", "open"}, p.order)
	require.Equal(t, []time.Time{at, {}}, p.taken)
}

// A begin opens the test's capture window, where the proxy's learn tracker
// begins the test, before it installs the test's view: a tracker that began
// after it would file what the view served first as the previous test's. A
// begin whose view the proxy refuses takes the window back, so what it held
// is carried into the next test's, and is served nothing.
func TestABeginOpensItsCaptureWindowBeforeItsView(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	storeMocks(a, httpMock("mock-1", "GET", "http://h/a"), httpMock("mock-2", "GET", "http://h/b"), httpMock("mock-4", "GET", "http://h/pool"))
	a.scopeTable = map[string][]string{"o::TestA": {"mock-1"}, "o::TestB": {"mock-2"}}
	ctx := context.Background()
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"mock-4"}}))
	claimSelf(t, a)
	p.mu.Lock()
	p.setErr = errors.New("the proxy refused the test's view")
	p.order = nil
	p.mu.Unlock()
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.NotNil(t, serr, "a begin whose view the proxy refused")
	p.mu.Lock()
	require.Equal(t, []string{"continue", "withdraw"}, p.order, "the refused begin's window is taken back, and nothing is served")
	p.setErr = nil
	p.order = nil
	p.mu.Unlock()
	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	p.mu.Lock()
	require.Equal(t, []string{"continue", "serve o::TestB"}, p.order, "the window opens before the view")
	p.mu.Unlock()
}

// movingClock is the no-op hooks with a run's clock that a test's begin
// moves to when the test began in its recording (TestClock), read as the
// app reads it (AppClockReader): at a real instant, the offset in force then.
type movingClock struct {
	AgentHook
	mu    sync.Mutex
	moved time.Time     // the real instant of the latest move
	off   time.Duration // the offset in force from then
}

func (h *movingClock) BeginTestClock(_ context.Context, _ string, at time.Time) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.moved = time.Now()
	h.off = at.Sub(h.moved)
	return nil
}

func (h *movingClock) EndTestClock(context.Context, string) error { return nil }

func (h *movingClock) AppClock(real time.Time) (time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.moved.IsZero() || real.Before(h.moved) {
		return real, true
	}
	return real.Add(h.off), true
}

// trackerProxy is a wireProxy whose capture window begins a learn tracker's
// test, as the proxy's does (openTestErrorCapture), with the test's
// recorded begin given first (coreAgent.RecordedBeginTaker).
type trackerProxy struct {
	*wireProxy
	tracker *learn.Tracker
}

func (p *trackerProxy) TakeRecordedBegin(at time.Time) { p.tracker.RecordedBegin(at) }

func (p *trackerProxy) ContinueTestErrorCapture() {
	p.tracker.Begin()
	p.wireProxy.ContinueTestErrorCapture()
}

// A client test's begin moves the app's clock to when the test began in its
// recording, and the learn tracker begins the test on the moved clock: the
// test's first call, which no earlier call bounds, reads its window on the
// app's clock from the tracker's begin, so a test that stamps its own time
// (pg-learn-e2e's TestStampAnEvent) is read as a read of the app's clock.
// A tracker begun before the move gives that call a span the clock did not
// run as one, so no window, and keploy mock record refuses the test.
func TestAClientTestsFirstCallHasAWindowOnTheClockItsBeginMoved(t *testing.T) {
	prev := ActiveHooks
	t.Cleanup(func() { ActiveHooks = prev })
	clock := &movingClock{}
	ActiveHooks = clock
	a, wp := wireAgent(t, models.MODE_TEST)
	tr := learn.NewTracker()
	tr.SetClientTests(true)
	tr.SetAppClock(clock.AppClock)
	a.Proxy = &trackerProxy{wireProxy: wp, tracker: tr}
	ctx := context.Background()
	claimSelf(t, a)
	recBegan := time.Date(2026, 10, 22, 11, 14, 15, 0, time.UTC)
	require.NoError(t, a.SetTestBegins(ctx, map[string]time.Time{"o::TestStampAnEvent": recBegan}, nil))
	_, serr := a.ProvenBegin(ctx, begin("TestStampAnEvent"))
	require.Nil(t, serr)
	wp.mu.Lock()
	require.Equal(t, []string{"continue", "serve o::TestStampAnEvent"}, wp.order, "the window opens before the view")
	wp.mu.Unlock()

	paired := &models.Mock{Spec: models.MockSpec{ReqTimestampMock: recBegan.Add(40 * time.Millisecond)}}
	w := tr.CallWindowFrom(time.Time{}, paired, time.Now())
	require.NotNil(t, w, "the test's first call has a window on the clock its begin moved")
	require.Equal(t, recBegan, w.Rec.From, "in the recording, from the test's recorded begin")
	require.False(t, w.F.From.Before(recBegan), "in this run, from the tracker's begin on the moved clock: %s", w.F.From)
}

// A begin whose clock cannot be moved fails before the test's capture window
// opens, so it takes back no window: the previous test's window, and the
// misses carried since it closed, stay as they were, for the next test.
func TestABeginWhoseClockCannotMoveOpensNoWindow(t *testing.T) {
	prev := ActiveHooks
	t.Cleanup(func() { ActiveHooks = prev })
	ActiveHooks = &clockHooks{err: errors.New("the clock file is read-only")}
	a, p := wireAgent(t, models.MODE_TEST)
	ctx := context.Background()
	claimSelf(t, a)
	require.NoError(t, a.SetTestBegins(ctx, map[string]time.Time{"o::TestA": time.Date(2026, 10, 22, 11, 14, 15, 0, time.UTC)}, nil))
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.NotNil(t, serr, "a begin whose clock cannot move")
	p.mu.Lock()
	defer p.mu.Unlock()
	require.Empty(t, p.order, "no window opened, none taken back, nothing served")
}
