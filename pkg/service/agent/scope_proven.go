package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/realdeps"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// The proven model's single-worker integration wire (design v14 §6.1 and §5,
// cut to one test process per run for the first release). It is served only
// under the proven switch; the routes call it in place of the old scope
// handlers then.
//
// One run has at most one test process at a time and at most one open test.
// The run's test command runs inside keploy (keploy mock record|replay -c),
// so its processes are the ones keploy intercepts. Each test process's keploy
// integration claims the run before its first test (ClaimInstance): the
// agent reads the caller's process from the connection, refuses a caller
// outside the run's own processes, and keeps the claim's key, the path every
// test ID of that process carries. The integration then opens and closes each
// test (ProvenBegin, ProvenEnd); a test no process claimed the run for is
// refused. In a replay, a test the gate lists is answered
// skip with its reason, and a test that missed a call or left a recorded call
// of its mapping unmade is answered fail with keploy's text. Each test's
// window carries its outcome and that evidence, for the CLI's verdict.
//
// Nothing here waits on a clock. A test whose process went away without
// ending it is failed when the next claim comes, or when the run's windows
// are read at its end.

// provenWire is the wire's state, guarded by Agent.provenMu. Every wire
// operation (a claim, a begin, an end, the final reap) also holds Agent.wire
// from start to finish, so they never interleave: one test at a time is the
// design, and an operation half done is never seen. Lock order: wire, then
// provenMu, then scopeMu; provenMu is never held while taking scopeMu or
// calling into the proxy.
type provenWire struct {
	// key is the current claim's key, and dir its test process's working
	// directory.
	key string
	dir string
	// open is the open test's ID, "" when none is.
	open string
	// skip is the gate: test ID -> the reason its harness skips it with.
	skip map[string]string
	// charged holds the misses the ends drained from the proxy, until the
	// CLI's own end-of-run read (GetMockErrors) collects them, so charging a
	// miss to a test never hides it from the run's totals.
	charged []models.UnmatchedCall
	// lossesCharged is what the tests' ends charged of what the agent could
	// not capture since the run began (chargeCaptureLosses), kind by kind:
	// what the run lost beyond it was charged to no test
	// (UnchargedCaptureLosses).
	lossesCharged models.CaptureLosses
	// claim is the live claim; zero when none.
	claim claimant
	// claims is every claim of the session, in order: from each one's time
	// on, until the next, a call made while no test is open belongs to the
	// pool slice of its key (design v14 §5.2). Record reads them with the
	// windows to tag each pool mock with its key.
	claims []models.ScopeWindow
	// pool is, in a replay, each key's pool slice (the CLI's scope table,
	// models.ScopeTableReq.Pool); nil until the CLI sent one.
	pool map[string][]string
	// staged: the replay's mocks are staged on the proxy, which serves them
	// exactly (exact_proven.go). Once per session.
	staged bool
	// servedSince is when exact serving began serving the open test
	// (serveTestExactly): a miss made before it is no call of the test's.
	servedSince time.Time
	// realSaid names the recorded mocks already said to go to a mock.real
	// destination (notReal).
	realSaid map[string]bool
	// unservable are the open test's mapped mocks the agent's store refused
	// (variables this keploy cannot follow): nothing serves them, and its end
	// counts each as a recorded call it did not make.
	unservable []string
	// drops are the proxy listener's drops read when the window now open
	// began (a test's begin, or the previous end); dropsKnown says they were
	// read (settle).
	drops      uint64
	dropsKnown bool
	// owned is the work the last window's end cut (settledSince), nil when
	// it cut none (its proxy cannot, or the switch is off): all a begin
	// waits for (settleBegin).
	owned coreAgent.InFlightScope
	// refusals are what the wire refused, and what an integration refused
	// and told the agent, in order, as windows (models.ScopeWindow.Refused).
	refusals []models.ScopeWindow
	// closed: the run's windows were read at its end; nothing more is
	// claimed or begun until the next session resets the wire.
	closed bool
	// topology is where the run's calls to local listeners went (LIVE and
	// mocked-local rows, NoteLive and NoteMocked).
	topology topologyRows
	// losses is the run's capture-loss counts at its start, which each
	// test's end counts from (capture_loss.go); testLosses the counts as the
	// previous test ended, which a test's own kinds (models.PerTest) count
	// from (nil before the first test's end: the run's mark).
	losses     captureLossMark
	testLosses models.CaptureLosses
	// scopeTests is, by the id of the keyer's scope each test was
	// (containerkey's Scope), the test's attempt: a mock stamped late, its
	// container's read the daemon did not answer, is charged to the attempt
	// its request was sent in (unreadMock). unreadOutside is what such
	// mocks of the time outside tests, or of an attempt whose window is
	// gone, charged no test with: the run's (UnchargedCaptureLosses).
	scopeTests    map[uint64]scopeTest
	unreadOutside models.CaptureLosses
}

// scopeTest is one attempt of a test: its ID, and its window's start, which
// tells it from another attempt of the same test (a retry).
type scopeTest struct {
	id    string
	start time.Time
}

// claimant is the process that claimed the run: a pid, and its start time,
// so a reused pid is never taken for it.
type claimant struct {
	pid   int
	start uint64
}

// missCapturer is implemented by a proxy that keeps each test's misses
// apart, carrying those made between tests into the next test. Continue opens
// a test's window, where the proxy's learn tracker begins the test; Withdraw
// takes back one opened for a test whose begin then failed, so what it holds
// is carried into the next test's as if it had never opened.
type missCapturer interface {
	ContinueTestErrorCapture()
	WithdrawTestErrorCapture()
}

func scopeErr(status int, code, msg, remedy string) *models.ScopeError {
	return &models.ScopeError{Status: status, Code: code, Message: msg, Remedy: remedy}
}

// wireLock serialises the wire's operations. A wait for it ends with the
// waiter's context, so a caller that went away (an interrupted CLI, a test
// process killed mid-request) leaves no waiter behind, and the CLI's final
// read is never stuck behind an operation it cannot see. The zero value is
// ready to use.
type wireLock struct {
	once sync.Once
	ch   chan struct{}
}

func (l *wireLock) lock(ctx context.Context) error {
	l.once.Do(func() { l.ch = make(chan struct{}, 1) })
	// A free lock is taken whatever the context says: only a wait ends with
	// it.
	select {
	case l.ch <- struct{}{}:
		return nil
	default:
	}
	select {
	case l.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (l *wireLock) unlock() { <-l.ch }

// abandoned answers an operation whose caller went away while it waited.
func abandoned(err error) *models.ScopeError {
	return scopeErr(http.StatusServiceUnavailable, models.ScopeErrInternal,
		"the request ended before keploy's agent could take it: "+err.Error(), "send it again")
}

// clientRoot is the process keploy's CLI registered as its client, in this
// agent's pid namespace (utils.ClientPIDHere), or 0 with why it is not taken:
// a CLI in another pid namespace (a docker agent's own, which the app's
// containers join, beside a CLI in a CI step container) said a pid that names
// another process here, so a claimer that descends from that process is not
// the run's.
func (a *Agent) clientRoot() (int, string) {
	if a.config == nil {
		return 0, ""
	}
	here := utils.ClientPIDHere(a.config.Agent.ClientNSPID, a.config.Agent.ClientPIDNS)
	return int(here.PID), here.Why
}

// agentPID is this agent's process: a claim read as its own is refused.
var agentPID = os.Getpid()

// resetSession clears the per-test scope state and the wire's for a new
// record or replay session, under one hold of the wire, so no operation in
// flight writes into the new session's state.
func (a *Agent) resetSession(ctx context.Context) error {
	if err := a.wire.lock(ctx); err != nil {
		return fmt.Errorf("start the session: %w", err)
	}
	defer a.wire.unlock()
	// The clock again, under the wire: a begin of the previous session that
	// came after the caller's release (MockOutgoing, GetOutgoing) took it,
	// and the reset below forgets that test, so nothing would end it. A
	// failure here comes after the caller's own resets (the proxy's captured
	// mocks, the app starts, the IDs), unlike the caller's release.
	if err := a.releaseTestClock(ctx); err != nil {
		return fmt.Errorf("keploy could not move the run's clock back from the previous session's last test: %w", err)
	}
	a.resetScopeState()
	// What the run's tests count their capture losses from: read before
	// provenMu, which is never held while calling into the hooks.
	losses := a.markCaptureLosses(ctx)
	a.provenMu.Lock()
	a.proven = provenWire{losses: losses}
	a.provenMu.Unlock()
	// A mock its stamp could not name the container of is its own test's
	// in a record, whenever it is stamped (unreadMock).
	containerkey.Default.OnUnreadMock(a.unreadMock)
	if t := a.realTracker(); t != nil {
		// A test the last session left open is no test of this one.
		t.Interrupt()
	}
	return nil
}

// ClaimInstance claims the run for caller (a process ID in the agent's PID
// namespace, read from the connection). A caller outside the run's own
// processes is refused: keploy does not intercept it, so its tests' calls
// would reach real infrastructure. A run another live process claimed is
// refused (the first release runs one test process at a time), and so is a
// second claim of the claimer while its test is open. A claim whose process
// exited is ended first: its open test, if any, fails.
//
// In a replay, the misses made before a new process claims the run are the
// run's, not a test's: they were made by the run's earlier processes (the
// previous test binary after its last test, a setup step of the command), so
// charging them to this process's first test would blame a test that never
// made them. They stay in the run's totals, where the CLI reports them as
// calls made outside every test. Misses a claimer makes between its own tests
// are still charged to its next test.
func (a *Agent) ClaimInstance(ctx context.Context, req models.InstanceReq, caller int) (models.InstanceResp, *models.ScopeError) {
	if err := a.wire.lock(ctx); err != nil {
		return models.InstanceResp{}, abandoned(err)
	}
	defer a.wire.unlock()
	key := req.Key
	// An empty key would give its tests bare IDs, which no verdicts.yaml
	// entry of the Universal Mocker names ([project::]path::name).
	if key == "" || strings.ContainsRune(key, 0) || !models.IDPart(key) || len(key) > 4096 || !filepath.IsLocal(key) || filepath.ToSlash(filepath.Clean(key)) != key {
		return models.InstanceResp{}, scopeErr(http.StatusBadRequest, models.ScopeErrBadRequest,
			"the instance key is not a clean path inside the project", `send the package directory relative to the project directory, with slashes and nothing to clean up ("." for the project's own), without "::" and neither beginning nor ending with ":"`)
	}
	if a.wireClosed() {
		return models.InstanceResp{}, errClosed()
	}
	root, why := a.clientRoot()
	if root <= 0 {
		msg := "this keploy run cannot take a test process's claim: keploy did not start its command natively (docker compose)"
		if why != "" {
			msg = "this keploy run cannot take a test process's claim: keploy's agent cannot tell its CLI's process (" + why + ")"
		}
		return models.InstanceResp{}, scopeErr(http.StatusUnprocessableEntity, models.ScopeErrNotInRun,
			msg,
			"run the test command natively: keploy mock record -c \"<command>\" or keploy mock replay -c \"<command>\"")
	}
	if caller == agentPID {
		return models.InstanceResp{}, scopeErr(http.StatusInternalServerError, models.ScopeErrNoCaller,
			"keploy's agent read itself as the caller: the connection reached it through its own proxy (is the agent's port in the pass-through ports?)",
			"run the test command natively, and keep keploy's agent port out of the ports keploy intercepts")
	}
	start, alive := procStart(caller)
	if caller <= 0 || !alive {
		return models.InstanceResp{}, scopeErr(http.StatusInternalServerError, models.ScopeErrNoCaller,
			"keploy's agent could not tell which process called it", "run the test command natively, on the machine the agent runs on")
	}
	if !procDescends(caller, root) {
		return models.InstanceResp{}, scopeErr(http.StatusUnprocessableEntity, models.ScopeErrNotInRun,
			fmt.Sprintf("process %d is not part of this keploy run, so keploy cannot capture or mock its calls", caller),
			`run the test command through keploy: keploy mock record -c "<command>" or keploy mock replay -c "<command>"`)
	}
	me := claimant{pid: caller, start: start}
	a.provenMu.Lock()
	prev, open := a.proven.claim, a.proven.open
	switch {
	case prev == me && open != "":
		a.provenMu.Unlock()
		return models.InstanceResp{}, scopeErr(http.StatusUnprocessableEntity, models.ScopeErrOneTestAtATime,
			fmt.Sprintf("process %d claimed the run again while its test %s was open", caller, open),
			"claim once per test process, before its first test")
	case prev.pid != 0 && prev != me && procAlive(prev.pid, prev.start):
		a.provenMu.Unlock()
		return models.InstanceResp{}, scopeErr(http.StatusUnprocessableEntity, models.ScopeErrOneTestProcess,
			fmt.Sprintf("process %d asked to run tests while process %d still runs this keploy run's tests: two test processes ran at once", caller, prev.pid),
			"run one test process at a time (go test -p 1, pytest without -n, jest --runInBand); parallel test workers arrive in a later keploy release")
	}
	// The previous claimer is gone: a test it left open can no longer end,
	// and fails. (A test is begun only under a claim, so with no previous
	// claimer none is open.)
	stale := ""
	if prev != me {
		stale = a.proven.open
	}
	a.provenMu.Unlock()
	if stale != "" {
		a.failOpen(ctx, stale, "the test process exited while the test was open")
	}
	// The run's misses so far are taken before the claim is made: a claim
	// whose read failed is not made, so its retry reads them again.
	if prev != me && a.replaying() && a.Proxy != nil {
		misses, err := a.Proxy.GetMockErrors(ctx)
		// What was read is the run's whether or not the read was whole.
		a.charge(misses)
		if err != nil {
			return models.InstanceResp{}, scopeErr(http.StatusInternalServerError, models.ScopeErrInternal,
				"keploy could not read the calls made before this test process started: "+err.Error(), "check the agent's log")
		}
	}
	if prev != me {
		// A failed TLS handshake before this claim is the run's -- charged
		// to no test (UnchargedCaptureLosses), and in the run's own verdict,
		// which counts every loss while the command ran -- as its misses
		// are: not this process's first test's. A read that fails leaves
		// that test to count from the previous test's end.
		if now, err := a.CaptureLosses(ctx); err == nil {
			a.provenMu.Lock()
			a.proven.testLosses = now
			a.provenMu.Unlock()
		}
	}
	a.provenMu.Lock()
	a.proven.claim = me
	newClaim := prev != me || a.proven.key != key
	if newClaim {
		now := time.Now()
		a.proven.claims = append(a.proven.claims, models.ScopeWindow{Claim: key, Start: now, End: now})
	}
	a.proven.key, a.proven.dir = key, req.Dir
	a.provenMu.Unlock()
	if newClaim {
		// The instance is a scope of its own outside its tests: two
		// containers configured the same are told apart only within one
		// (containerkey).
		containerkey.Default.BeginInstance()
	}
	if a.replaying() {
		// A new claim of the key (a test process run again: a retried
		// package, --rerun-fails) is a new scope: its calls outside every
		// test start afresh, as a test begun again does (decision 31's
		// ruling on re-runs). A first claim has consumed nothing yet.
		if newClaim {
			if err := a.restartKey(key); err != nil {
				return models.InstanceResp{}, scopeErr(http.StatusInternalServerError, models.ScopeErrInternal,
					"keploy could not start the calls of "+key+" made outside its tests afresh: "+err.Error(), "check the agent's log")
			}
		}
		// Calls made from now while no test is open are this key's.
		if err := a.applyPool(ctx); err != nil {
			return models.InstanceResp{}, scopeErr(http.StatusInternalServerError, models.ScopeErrInternal,
				"keploy could not arm the recorded calls of "+key+" made outside its tests: "+err.Error(), "check the agent's log")
		}
	}
	// The clock record the run's processes read: the integration exports it
	// as KEPLOY_TIME_FILE to what it starts ("" when the run is not frozen).
	return models.InstanceResp{V: models.ScopeProtocolVersion, Instance: models.InstanceRun, TimeFile: TimeFile()}, nil
}

// SetScopePool installs each key's pool slice for a replay (design v14 §5.2,
// U21), and serves the current key's while no test is open.
func (a *Agent) SetScopePool(ctx context.Context, pool map[string][]string) error {
	if err := a.wire.lock(ctx); err != nil {
		return err
	}
	defer a.wire.unlock()
	cp := make(map[string][]string, len(pool))
	for k, names := range pool {
		cp[k] = append([]string{}, names...)
	}
	a.provenMu.Lock()
	a.proven.pool = cp
	a.provenMu.Unlock()
	// Whose a mock is, for the learn steps: keploy mock replay sends no
	// recorded test windows, and the table's pool slices are what says a
	// mock was recorded while no test was open.
	if ps, ok := a.Proxy.(coreAgent.PoolNamesSetter); ok {
		var names []string
		for _, slice := range cp {
			names = append(names, slice...)
		}
		if err := ps.SetPoolNames(names); err != nil {
			return err
		}
	}
	return a.applyPool(ctx)
}

// applyPool serves the current key's pool slice exactly (servePoolExactly),
// unless a test is open: its end serves it.
func (a *Agent) applyPool(ctx context.Context) error {
	a.provenMu.Lock()
	open := a.proven.open
	a.provenMu.Unlock()
	if open != "" {
		return nil
	}
	return a.servePoolExactly(ctx)
}

// claimWindows is the session's claims, refusals and topology rows, as
// windows (models.ScopeWindow.Claim, .Refused, .Live, .Mocked).
func (a *Agent) claimWindows() []models.ScopeWindow {
	a.provenMu.Lock()
	defer a.provenMu.Unlock()
	out := append([]models.ScopeWindow(nil), a.proven.claims...)
	out = append(out, a.proven.refusals...)
	return append(out, a.proven.topology.windows...)
}

// maxRefusals bounds the refusals a session keeps: past it, a harness that
// is refused at every test adds nothing new.
const maxRefusals = 64

// NoteRefusal records a refusal of the run (models.RunRefusal): one the wire
// answered a claim, a begin or an end with, or one an integration made and
// told the agent. The CLI's verdict names it. A refusal like one already
// kept is kept once: a harness refused at every test, by turns for its
// begin and its end, never crowds out a refusal it has not made.
func (a *Agent) NoteRefusal(r models.RunRefusal) {
	a.provenMu.Lock()
	defer a.provenMu.Unlock()
	for _, w := range a.proven.refusals {
		if *w.Refused == r {
			return
		}
	}
	if len(a.proven.refusals) >= maxRefusals {
		return
	}
	now := time.Now()
	a.proven.refusals = append(a.proven.refusals, models.ScopeWindow{Start: now, End: now, Refused: &r})
}

// SetScopeSkips installs the proven gate: the tests a replay skips, by ID,
// with their reasons. nil skips nothing.
func (a *Agent) SetScopeSkips(_ context.Context, skip map[string]string) error {
	cp := make(map[string]string, len(skip))
	for id, reason := range skip {
		cp[id] = reason
	}
	a.provenMu.Lock()
	a.proven.skip = cp
	a.provenMu.Unlock()
	return nil
}

func (a *Agent) replaying() bool {
	return a.config != nil && a.config.Agent.Mode == models.MODE_TEST
}

// ProvenBegin opens a test (design v14 §5.3). The run must be claimed; the
// harness must say it can skip; a second test while one is open is refused;
// in a replay a gated test is answered skip, and opens nothing.
func (a *Agent) ProvenBegin(ctx context.Context, req models.ScopeReq) (models.ScopeBeginResp, *models.ScopeError) {
	if err := a.wire.lock(ctx); err != nil {
		return models.ScopeBeginResp{}, abandoned(err)
	}
	defer a.wire.unlock()
	if req.Name == "" {
		return models.ScopeBeginResp{}, scopeErr(http.StatusBadRequest, models.ScopeErrBadRequest, "a begin names its test", "send the test's name")
	}
	if !models.IDPart(req.File) || !models.IDPart(req.Project) {
		return models.ScopeBeginResp{}, scopeErr(http.StatusBadRequest, models.ScopeErrBadRequest,
			`a test's file and project cannot hold "::", which separates the parts of its ID, or begin or end with ":"`, "send them as the framework names them")
	}
	if !req.CanSkip || req.Suite {
		return models.ScopeBeginResp{}, scopeErr(http.StatusUnprocessableEntity, models.ScopeErrUpgradeIntegration,
			"this test harness predates keploy's scope protocol v1: it cannot skip a test keploy quarantines, or it opens suite scopes",
			"upgrade the keploy integration of your test framework (go-sdk keploytest, keploy-pytest, @keploy/jest)")
	}
	if serr := oldWireFields(req); serr != nil {
		return models.ScopeBeginResp{}, serr
	}
	at := time.Now()
	a.provenMu.Lock()
	if a.proven.closed {
		a.provenMu.Unlock()
		return models.ScopeBeginResp{}, errClosed()
	}
	// A claim whose process exited is no claim: its key would name another
	// process's tests.
	if c := a.proven.claim; c.pid == 0 || !procAlive(c.pid, c.start) {
		a.provenMu.Unlock()
		return models.ScopeBeginResp{}, scopeErr(http.StatusUnprocessableEntity, models.ScopeErrNotInRun,
			req.Name+" began before its test process claimed this keploy run, so keploy cannot tell that its calls are captured or mocked",
			"upgrade the keploy integration of your test framework: it claims the run (POST /agent/instance) before its first test")
	}
	id := models.TestID(req.Project, req.File, a.proven.key, req.Name)
	if open := a.proven.open; open != "" {
		a.provenMu.Unlock()
		return models.ScopeBeginResp{}, scopeErr(http.StatusUnprocessableEntity, models.ScopeErrOneTestAtATime,
			fmt.Sprintf("%s began while %s was still open: two tests ran at once in one test process", id, open),
			"run them one at a time: start parallel tests with keploytest.Parallel and subtests with keploytest.Run (keploy mock codemod go writes them); parallel test workers arrive in a later keploy release")
	}
	reason, gated := a.proven.skip[id]
	dir := a.proven.dir
	a.provenMu.Unlock()
	if a.replaying() && gated {
		a.NoteScopeGated(id, 0, at)
		return models.ScopeBeginResp{V: models.ScopeProtocolVersion, Status: "ok", Action: models.ScopeActionSkip, Reason: reason}, nil
	}

	// What judging the test at its end needs is checked before anything is
	// opened: a begin that fails leaves nothing behind.
	var capture missCapturer
	if a.replaying() {
		if _, err := a.exactServer(); err != nil {
			return models.ScopeBeginResp{}, scopeErr(http.StatusInternalServerError, models.ScopeErrInternal,
				"keploy cannot judge "+id+": "+err.Error(), "run the keploy agent of this keploy version")
		}
		c, ok := a.Proxy.(missCapturer)
		if !ok {
			return models.ScopeBeginResp{}, scopeErr(http.StatusInternalServerError, models.ScopeErrInternal,
				"keploy cannot judge "+id+": this keploy agent's proxy cannot keep each test's misses apart", "run the keploy agent of this keploy version")
		}
		capture = c
	}
	// The previous test's calls are settled first (settleBegin): they are
	// never this test's. Calls made since its end are this test's or
	// background work, and do not hold the begin.
	// A connection whose held requests cannot be told does not stop the
	// begin: the test's end finds it, and the test is not verified.
	drops, err := a.settleBegin(ctx, a.replaying())
	if err != nil && !errors.Is(err, errHoldsUnknown) {
		return models.ScopeBeginResp{}, scopeErr(http.StatusInternalServerError, models.ScopeErrInternal,
			"keploy cannot judge "+id+": "+err.Error(), "check the agent's log")
	}
	// The test's end compares the listener's drops with these.
	a.markDrops(drops)
	a.NoteScope(id, 0, dir, false)
	// opened: the test's capture window opened (open), so a begin that
	// fails after it takes the window back.
	var open func()
	opened := false
	if capture != nil {
		open = func() {
			// The learn step bounds the test's calls by when the test began
			// in its recording, the instant its begin moved the app's clock
			// to (coreAgent.RecordedBeginTaker): given before the window
			// opens, so the test begins with it.
			if r, ok := a.Proxy.(coreAgent.RecordedBeginTaker); ok {
				a.scopeMu.Lock()
				began := a.testBegins[id]
				a.scopeMu.Unlock()
				r.TakeRecordedBegin(began)
			}
			// Misses made from here, and those made since the previous test
			// ended (or before the first test), are this test's: a miss
			// outside every test is charged to the next test that begins
			// (design v14 §5.1). Opening the window is where the proxy's
			// learn tracker begins the test, so it opens once the begin has
			// moved the app's clock (beginScopeAt): a client test's call
			// reads its window on the app's clock from the tracker's begin
			// (learn.Tracker.CallWindowFrom), and a begin stamped before the
			// move would give the test's first call a span the clock did not
			// run as one, so no window. It opens before the test's view is
			// installed, as keploy test's loops open it: a tracker that
			// began after the view was installed would file what the view
			// served before it as the previous test's, so the store's
			// account of the open test and the tracker's could not be of one
			// test.
			capture.ContinueTestErrorCapture()
			opened = true
		}
	}
	if err := a.beginScopeAt(ctx, id, 0, at, open); err != nil {
		if opened {
			// The test did not begin: what its window holds is carried
			// into the next test's, as if it had never opened.
			capture.WithdrawTestErrorCapture()
		}
		a.abandonWindow(id)
		return models.ScopeBeginResp{}, scopeErr(http.StatusInternalServerError, models.ScopeErrInternal,
			"keploy's agent could not open "+id+": "+err.Error(), "check the agent's log")
	}
	if t := a.realTracker(); t != nil {
		// The mock.real destinations its connections send bytes to from
		// here are this test's.
		t.BeginTest()
	}
	a.provenMu.Lock()
	a.proven.open = id
	a.provenMu.Unlock()
	// The test is a scope of its own from here (containerkey); closeOpen
	// goes back to the run's however the test closes.
	containerkey.Default.BeginTest()
	scope := containerkey.Default.Scope()
	a.provenMu.Lock()
	if a.proven.scopeTests == nil {
		a.proven.scopeTests = map[uint64]scopeTest{}
	}
	a.proven.scopeTests[scope] = scopeTest{id: id, start: boundaryTime(at)} // its window's start (BeginScopeAt)
	a.provenMu.Unlock()
	return models.ScopeBeginResp{V: models.ScopeProtocolVersion, Status: "ok"}, nil
}

// oldWireFields refuses a begin or an end that carries the old wire's
// fields (release1-cut §2.1: no pid on the wire). The claim names the test
// process, and the agent stamps each test's begin and end with its own
// clock, so a pid, a runner's clock, a folder or a suite scope would decide
// nothing, and a harness that sends them predates the protocol.
func oldWireFields(req models.ScopeReq) *models.ScopeError {
	var sent []string
	if req.Pid != 0 {
		sent = append(sent, "pid")
	}
	if !req.At.IsZero() {
		sent = append(sent, "at")
	}
	if req.Dir != "" {
		sent = append(sent, "dir")
	}
	if req.Suite {
		sent = append(sent, "suite")
	}
	if len(sent) == 0 {
		return nil
	}
	return scopeErr(http.StatusUnprocessableEntity, models.ScopeErrUpgradeIntegration,
		"this test harness sends "+strings.Join(sent, ", ")+", which keploy's scope protocol v1 does not use: the claim names the test process, and keploy stamps each test's begin and end itself",
		"upgrade the keploy integration of your test framework (go-sdk keploytest, keploy-pytest, @keploy/jest)")
}

// ProvenEnd closes the open test. An end without an outcome counts as
// failed. In a replay, the test's misses and the data-plane mocks of its
// mapping it did not consume fail it, with keploy's reason; a test keploy
// fails is recorded failed.
func (a *Agent) ProvenEnd(ctx context.Context, req models.ScopeReq) (models.ScopeEndResp, *models.ScopeError) {
	if err := a.wire.lock(ctx); err != nil {
		return models.ScopeEndResp{}, abandoned(err)
	}
	defer a.wire.unlock()
	if req.Name == "" {
		return models.ScopeEndResp{}, scopeErr(http.StatusBadRequest, models.ScopeErrBadRequest, "an end names its test", "send the name its begin sent")
	}
	at := time.Now()
	a.provenMu.Lock()
	id := models.TestID(req.Project, req.File, a.proven.key, req.Name)
	open := a.proven.open
	a.provenMu.Unlock()
	if serr := oldWireFields(req); serr != nil {
		// The test it names is failed and closed with keploy's reason:
		// left open, it would be blamed on the next begin
		// (one_test_at_a_time).
		if open == id {
			if _, _, err := a.closeOpen(ctx, id, models.ScopeOutcomeFailed, at, serr.Message); err != nil {
				a.logger.Warn("could not judge a test whose end keploy refused", zap.String("test", id), zap.Error(err))
			}
		}
		return models.ScopeEndResp{}, serr
	}
	if open != id {
		// A harness that ends a test it was told to skip is warned.
		a.noteEndOfGated(id, 0)
		msg := fmt.Sprintf("%s ended, but no test is open", id)
		if open != "" {
			msg = fmt.Sprintf("%s ended while %s was the open test", id, open)
		}
		serr := scopeErr(http.StatusConflict, models.ScopeErrNotOpen, msg,
			"end a test once, with the name its begin sent, and only when its begin was not answered skip")
		// With another test open, the harness ended a test it did not
		// begin: the open test is failed and closed with keploy's reason
		// (left open, every later begin would be refused as two tests at
		// once), and the run names the cause. An end with no test open
		// refuses only that call.
		if open != "" {
			if _, _, err := a.closeOpen(ctx, open, models.ScopeOutcomeFailed, at, msg); err != nil {
				a.logger.Warn("could not judge a test another test's end left open", zap.String("test", open), zap.Error(err))
			}
			a.NoteRefusal(serr.Refusal())
		}
		return models.ScopeEndResp{}, serr
	}
	outcome := models.NormalizeScopeOutcome(req.Outcome)
	if outcome == "" {
		outcome = models.ScopeOutcomeFailed
	}
	ev, outcome, err := a.closeOpen(ctx, id, outcome, at, "")
	if err != nil {
		// A test keploy could not read is unread on its window (its
		// verdict is keploy's not-judged); a failure to end it is the run's
		// refusal already (endTest).
		return models.ScopeEndResp{}, scopeErr(http.StatusInternalServerError, models.ScopeErrInternal,
			"keploy could not judge "+id+": "+err.Error(), "check the agent's log; the test is counted failed")
	}
	// Record instances never answer fail (design v14 §5.3): a record's
	// evidence (capture losses) is for judging, on the window. Nor does a
	// replayed test that is only not judged (decision 20): its window says
	// so, and its outcome stands.
	if a.replaying() && outcome == models.ScopeOutcomeFailed && ev != nil && (!ev.Empty() || ev.Reason != "") {
		return models.ScopeEndResp{V: models.ScopeProtocolVersion, Status: "ok", Action: models.ScopeActionFail, Reason: ev.Reason}, nil
	}
	return models.ScopeEndResp{V: models.ScopeProtocolVersion, Status: "ok"}, nil
}

// closeOpen ends the open test id (endTest) and clears it, whatever happens
// on the way. With the wire held.
func (a *Agent) closeOpen(ctx context.Context, id, outcome string, at time.Time, why string) (*models.ScopeEvidence, string, error) {
	defer func() {
		a.provenMu.Lock()
		closed := a.proven.open == id
		if closed {
			a.proven.open = ""
		}
		a.provenMu.Unlock()
		if closed {
			// Connections from here on are the run's again, in a scope of
			// their own until the next test (containerkey).
			containerkey.Default.EndTest()
		}
	}()
	return a.endTest(ctx, id, outcome, at, why)
}

// failOpen ends, as failed, a test whose harness can no longer end it. With
// the wire held.
func (a *Agent) failOpen(ctx context.Context, id string, why string) {
	if _, _, err := a.closeOpen(ctx, id, models.ScopeOutcomeFailed, time.Now(), why); err != nil {
		a.logger.Warn("could not judge a test whose process went away", zap.String("test", id), zap.Error(err))
	}
	a.logger.Warn("a test is counted failed: "+why, zap.String("test", id))
}

// endTest records a test's outcome and evidence and closes its window. A
// read that failed is an error, and the test is recorded failed with it: a
// test keploy could not judge never passes. A test that skipped itself is
// not failed for the recorded calls it then never made.
func (a *Agent) endTest(ctx context.Context, id, outcome string, at time.Time, why string) (*models.ScopeEvidence, string, error) {
	var ev *models.ScopeEvidence
	var readErr error
	// The test's calls are settled before it is judged (settle_proven.go):
	// a call it made that the proxy has not yet taken or answered is its
	// own, whatever the load. One keploy cannot settle leaves the test
	// unjudged, and failed.
	settleErr := a.settledSince(ctx, a.replaying(), id)
	// The mock.real destinations the test used, and the connections keploy
	// could neither capture nor relay as real while it was open: in a record
	// as in a replay (design v14 §3.2, §4).
	var uses []models.RealUse
	var incomplete []models.CaptureIncomplete
	if t := a.realTracker(); t != nil {
		uses, incomplete = t.EndTest()
	}
	// When exact serving began serving the test: a miss made before it is
	// no call of the test's (evidenceReason).
	beganAt := a.servedSince()
	if a.replaying() {
		// The test stops being served before what it did is read: a call
		// that lands after the read cannot consume one of its mocks, so the
		// outcome read is final (ExactTestOutcome reads the ended view).
		// A pool that cannot be served is keploy's failure to end the test,
		// not to read it: the end serves the pool again below (EndScopeAt)
		// and, failing again, makes it the run's refusal.
		if err := a.servePoolExactly(ctx); err != nil {
			a.logger.Warn("keploy could not stop serving the test before reading it; its end serves the pool again", zap.String("test", id), zap.Error(err))
		}
		ev, readErr = a.testEvidence(ctx, id, outcome != models.ScopeOutcomeSkipped)
	}
	if len(incomplete) > 0 {
		if ev == nil {
			ev = &models.ScopeEvidence{}
		}
		ev.Incomplete = append(ev.Incomplete, incomplete...)
	}
	switch {
	case readErr != nil:
		if ev == nil {
			ev = &models.ScopeEvidence{}
		}
		ev.Reason = "keploy: keploy could not tell what " + id + " did: " + readErr.Error()
		ev.Unread = true
		outcome = models.ScopeOutcomeFailed
	case !ev.Empty() && a.replaying():
		ev.Reason = evidenceReason(id, ev, a.mocksFound(ev.Unconsumed), beganAt)
		outcome = models.ScopeOutcomeFailed
	case !ev.Empty():
		// A record never fails a test: what keploy could not capture is
		// kept for judging, as R0's capture_incomplete (design v14 §7.4),
		// and the test's outcome is its own against real infra.
		ev.Reason = evidenceReason(id, ev, nil, beganAt)
	}
	if len(uses) > 0 {
		a.noteScopeReal(id, uses)
	}
	// Before why: why goes in front of whatever reason the evidence gives.
	ev, outcome = a.chargeCaptureLosses(ctx, id, ev, outcome, readErr == nil, beganAt)
	if settleErr != nil {
		note := "keploy could not tell that every call of " + id + " reached it: " + settleErr.Error()
		if why == "" {
			why = note
		} else {
			why += "; " + note
		}
		if readErr == nil {
			readErr = settleErr
		}
		// Unsettled, what the test did cannot all have been read: it has
		// no verdict of its own.
		if ev == nil {
			ev = &models.ScopeEvidence{}
		}
		ev.Unread = true
	}
	if why != "" {
		if ev == nil {
			ev = &models.ScopeEvidence{}
		}
		ev.Reason = strings.TrimSpace("keploy: " + why + ". " + strings.TrimPrefix(ev.Reason, "keploy: "))
		outcome = models.ScopeOutcomeFailed
	}
	a.NoteScopeOutcome(id, 0, outcome)
	if ev != nil && (ev.Reason != "" || !ev.Empty()) {
		a.noteScopeEvidence(id, ev)
	}
	if err := a.EndScopeAt(ctx, id, 0, at); err != nil {
		// The window closed with the outcome above; a test whose end failed
		// is counted failed, as its end answer says. Its mocks or its clock
		// may not be put back, which taints every test after it: the run's
		// to name, as keploy's own failure, whoever ended the test and
		// whatever its read did.
		a.failClosedWindow(id, "keploy: keploy could not end "+id+": "+err.Error())
		a.NoteRefusal(models.RunRefusal{Code: models.ScopeErrInternal,
			Message: "keploy could not end " + id + " (its mocks or its clock may not be put back, so the tests after it may have run on them): " + err.Error(),
			Remedy:  "check the agent's log, and run the tests again"})
		if readErr == nil {
			readErr = err
		}
		outcome = models.ScopeOutcomeFailed
	}
	return ev, outcome, readErr
}

// chargeCaptureLosses adds to a test's evidence what the agent could not
// capture that the test is charged (design v14 §3.2): a loss since the run
// began taints the test open when it happened and every later one, but for a
// test's own kinds (models.PerTest: a failed TLS handshake), which are the
// test's they happened in. In a replay the test then fails, with the losses
// in its reason; a record keeps the evidence on the window for
// judging and answers as before (record instances never fail a test). A read
// of the losses that failed is a test keploy cannot judge: failed in a
// replay, and said on the window in a record.
//
// fromEvidence says ev's reason, if any, is keploy's text for its evidence
// (evidenceReason, with beganAt), which is then written again with the
// losses in it; any other reason (a read that failed) keeps its text, after
// the losses'.
func (a *Agent) chargeCaptureLosses(ctx context.Context, id string, ev *models.ScopeEvidence, outcome string, fromEvidence bool, beganAt time.Time) (*models.ScopeEvidence, string) {
	a.provenMu.Lock()
	mark, test := a.proven.losses, a.proven.testLosses
	a.provenMu.Unlock()
	lost, now, err := a.captureLossesSince(ctx, mark, test)
	if !a.replaying() {
		// A record charges a container read the daemon did not answer by
		// the mock its stamp marked (unreadMock), its own test's whenever
		// it came, not by the count, which may come after its test ended.
		lost, now = lost.Without(models.CaptureLossContainerUnread), now.Without(models.CaptureLossContainerUnread)
	}
	if err == nil && now != nil {
		// The next test's own losses count from here.
		a.provenMu.Lock()
		a.proven.testLosses = now
		a.provenMu.Unlock()
	}
	if err == nil && len(lost) == 0 {
		return ev, outcome
	}
	if err == nil {
		// What the tests have been charged, kind by kind: a taint is
		// charged whole to every test from it on, so the most any test's
		// end saw since the run began (tests of parallel instances end
		// concurrently, and their reads land in either order); a test's
		// own kind (PerTest) only to the test it happened in, so the sum
		// of what each test was charged -- what a test process's claim
		// kept from its first test is charged to none.
		var taint, own models.CaptureLosses
		for _, c := range sinceRun(now, mark) {
			if !c.Kind.PerTest() {
				taint = append(taint, c)
			}
		}
		for _, c := range lost {
			if c.Kind.PerTest() {
				own = append(own, c)
			}
		}
		a.provenMu.Lock()
		a.proven.lossesCharged = a.proven.lossesCharged.Max(taint).Plus(own)
		a.provenMu.Unlock()
	}
	if ev == nil {
		ev = &models.ScopeEvidence{}
	}
	if err != nil {
		ev.Reason = strings.TrimSpace("keploy: keploy could not tell whether every call of " + id + " was captured: " + err.Error() + ". " + strings.TrimPrefix(ev.Reason, "keploy: "))
		// As a test whose evidence could not be read: no verdict of its own.
		ev.Unread = true
		if a.replaying() {
			outcome = models.ScopeOutcomeFailed
		}
		return ev, outcome
	}
	// A call keploy cannot capture at all (decision 20: over a Unix socket)
	// leaves the test without a verdict, never failed: it is on the window,
	// and in the reason, but the outcome stands.
	judged, notJudged := lost.Split()
	ev.CannotCapture = notJudged
	if len(judged) == 0 {
		if ev.Reason == "" {
			ev.Reason = "keploy: " + notJudged.NotJudgedText() + "."
		} else {
			ev.Reason = strings.TrimSpace(ev.Reason + " Also " + notJudged.NotJudgedText() + ".")
		}
		return ev, outcome
	}
	ev.CaptureIncomplete = judged
	switch {
	case fromEvidence:
		// keploy's text for the evidence, written again with the losses
		// first: the same text in a record (whose evidence is the
		// connections keploy could not capture) as in a replay.
		var mocks map[string]*models.Mock
		if a.replaying() {
			mocks = a.mocksFound(ev.Unconsumed)
		}
		ev.Reason = evidenceReason(id, ev, mocks, beganAt)
	default:
		ev.Reason = strings.TrimSpace("keploy: " + judged.Text() + ". " + strings.TrimPrefix(ev.Reason, "keploy: "))
	}
	if a.replaying() {
		outcome = models.ScopeOutcomeFailed
	}
	return ev, outcome
}

// unreadMock charges, in a record, the test a mock's request was sent in
// (scope: the keyer's Scope id then) with the mock, which its stamp could
// not name the container of: the Docker daemon did not answer keploy's read
// of it (what names it). Whenever the stamp came, the test's window says it
// is not judged: still open, as it closes; closed, now, until the CLI reads
// the windows at the run's end. A mock of the time outside tests is the
// run's (UnchargedCaptureLosses). In a replay a use's count charges it
// (chargeCaptureLosses): its call is answered in its test.
func (a *Agent) unreadMock(scope uint64, what string) {
	if a.replaying() {
		return
	}
	loss := models.CaptureLosses{{Kind: models.CaptureLossContainerUnread, Count: 1, What: what}}
	a.provenMu.Lock()
	t, inTest := a.proven.scopeTests[scope]
	a.provenMu.Unlock()
	if inTest && a.chargeAttempt(t, loss) {
		return
	}
	a.provenMu.Lock()
	a.proven.unreadOutside = a.proven.unreadOutside.Plus(loss)
	a.provenMu.Unlock()
}

// chargeAttempt charges attempt t of a test with loss: on its window, still
// open (as it closes) or closed. It reports whether t has a window: an
// attempt withdrawn at its begin has none, and another attempt of the same
// test (a retry) is not charged with it.
func (a *Agent) chargeAttempt(t scopeTest, loss models.CaptureLosses) bool {
	a.scopeMu.Lock()
	defer a.scopeMu.Unlock()
	k := scopeKey{pid: 0, name: t.id}
	if start, open := a.workerOpen[k]; open && start.Equal(t.start) {
		if a.scopeMeta == nil {
			a.scopeMeta = make(map[scopeKey]scopeMeta)
		}
		meta := a.scopeMeta[k]
		meta.unread = meta.unread.Plus(loss)
		a.scopeMeta[k] = meta
		return true
	}
	for i := len(a.scopeWindows) - 1; i >= 0; i-- {
		w := &a.scopeWindows[i]
		if w.Name == t.id && w.PID == 0 && w.Start.Equal(t.start) {
			w.Evidence = withUnread(w.Evidence, loss)
			return true
		}
	}
	return false
}

// withUnread is ev (a copy, nil for none) with loss, mocks whose container the
// Docker daemon did not answer the read of: what leaves the test not judged,
// and said in its reason.
func withUnread(ev *models.ScopeEvidence, loss models.CaptureLosses) *models.ScopeEvidence {
	out := &models.ScopeEvidence{}
	if ev != nil {
		c := *ev
		out = &c
	}
	out.CannotCapture = out.CannotCapture.Plus(loss)
	text := loss.NotJudgedText()
	switch {
	case out.Reason == "":
		out.Reason = "keploy: " + text + "."
	default:
		out.Reason = strings.TrimSpace(out.Reason + " Also " + text + ".")
	}
	return out
}

// failClosedWindow marks the latest closed window of id failed, with why.
func (a *Agent) failClosedWindow(id, why string) {
	a.scopeMu.Lock()
	defer a.scopeMu.Unlock()
	for i := len(a.scopeWindows) - 1; i >= 0; i-- {
		w := &a.scopeWindows[i]
		if w.Name != id || w.PID != 0 {
			continue
		}
		w.Outcome = models.ScopeOutcomeFailed
		ev := &models.ScopeEvidence{}
		if w.Evidence != nil {
			c := *w.Evidence
			ev = &c
		}
		ev.Reason = strings.TrimSpace(ev.Reason + "\n" + why)
		w.Evidence = ev
		return
	}
}

// testEvidence is what fails the open test id at its end: its data-plane
// misses (models.UnmatchedCall.CountsAgainstTest: DNS is protocol-phase, and
// a connection that sent nothing made no call), and the test mocks it did not
// consume while it was open (exact serving tells which). A failed read is an
// error. Every miss is still charged to the run, so its next step is said.
func (a *Agent) testEvidence(ctx context.Context, id string, unmade bool) (*models.ScopeEvidence, error) {
	if a.Proxy == nil {
		return nil, errors.New("no proxy")
	}
	ev := &models.ScopeEvidence{}
	misses, err := a.Proxy.GetMockErrors(ctx)
	// Misses a read returned with its error are this test's all the same.
	a.charge(misses)
	for _, m := range misses {
		if !m.CountsAgainstTest() {
			continue
		}
		ev.Misses = append(ev.Misses, sanitizedMiss(m))
	}
	if err != nil {
		return ev, fmt.Errorf("reading its misses: %w", err)
	}
	if !unmade {
		return ev, nil
	}
	// A read that fails from here keeps the misses already found: they are
	// this test's whatever else could not be told.
	es, err := a.exactServer()
	if err != nil {
		return ev, err
	}
	test, _, unconsumed, ok := es.ExactTestOutcome()
	if !ok || test != id {
		return ev, fmt.Errorf("keploy's proxy was not serving %s exactly at its end (it was serving %q)", id, test)
	}
	a.provenMu.Lock()
	unservable := append([]string(nil), a.proven.unservable...)
	a.provenMu.Unlock()
	ev.Unconsumed = append(unconsumed, unservable...)
	return ev, nil
}

// mocksByName finds every stored mock named, resident or on disk. A name the
// stored set does not have is an error: the mapping names a recording that
// is not loaded, so whether the test made that call cannot be told.
func (a *Agent) mocksByName(names []string) (map[string]*models.Mock, error) {
	out := make(map[string]*models.Mock, len(names))
	v, ok := a.clientMocks.Load(uint64(0))
	s, _ := v.(*ClientMockStorage)
	if !ok || s == nil {
		return nil, errors.New("no mocks are loaded")
	}
	want := make(map[string]bool, len(names))
	s.mu.RLock()
	for _, n := range names {
		// A mock the store refused was said as it was refused: it answers
		// nothing (refusedOf).
		if _, refused := s.refused[n]; !refused {
			want[n] = true
		}
	}
	for _, pool := range [][]*models.Mock{s.filtered, s.unfiltered} {
		for _, m := range pool {
			if m != nil && want[m.Name] {
				out[m.Name] = m
			}
		}
	}
	disk := s.diskMocks
	s.mu.RUnlock()
	var rest []string
	for n := range want {
		if out[n] == nil {
			rest = append(rest, n)
		}
	}
	if len(rest) > 0 && disk != nil {
		found, err := disk.LoadByNames(rest)
		if err != nil {
			return nil, fmt.Errorf("reading mocks %s: %w", strings.Join(rest, ", "), err)
		}
		for _, m := range found {
			if m != nil {
				out[m.Name] = m
			}
		}
	}
	var missing []string
	for n := range want {
		if out[n] == nil {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("its mapping names mocks the loaded recording does not have: %s; record the set again", strings.Join(missing, ", "))
	}
	return out, nil
}

// servedSince is when exact serving began serving the open test; zero
// when it does not.
func (a *Agent) servedSince() time.Time {
	a.provenMu.Lock()
	defer a.provenMu.Unlock()
	return a.proven.servedSince
}

// refusedOf is, in order, the names of names the agent's store refused that
// are test mocks: a protocol-phase mock is no test's.
func (a *Agent) refusedOf(names []string) []string {
	v, ok := a.clientMocks.Load(uint64(0))
	s, _ := v.(*ClientMockStorage)
	if !ok || s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for _, n := range names {
		if s.refused[n] {
			out = append(out, n)
		}
	}
	return out
}

// mocksFound is mocksByName for rendering only: what it cannot find is left out.
func (a *Agent) mocksFound(names []string) map[string]*models.Mock {
	m, err := a.mocksByName(names)
	if err != nil {
		return map[string]*models.Mock{}
	}
	return m
}

// noteScopeEvidence attaches ev to the window id's end closes.
func (a *Agent) noteScopeEvidence(id string, ev *models.ScopeEvidence) {
	a.scopeMu.Lock()
	defer a.scopeMu.Unlock()
	k := scopeKey{pid: 0, name: id}
	if _, open := a.workerOpen[k]; !open {
		return
	}
	if a.scopeMeta == nil {
		a.scopeMeta = make(map[scopeKey]scopeMeta)
	}
	meta := a.scopeMeta[k]
	meta.evidence = ev
	a.scopeMeta[k] = meta
}

// noteScopeReal attaches the mock.real destinations a test used to the
// window id's end closes.
func (a *Agent) noteScopeReal(id string, uses []models.RealUse) {
	a.scopeMu.Lock()
	defer a.scopeMu.Unlock()
	k := scopeKey{pid: 0, name: id}
	if _, open := a.workerOpen[k]; !open {
		return
	}
	if a.scopeMeta == nil {
		a.scopeMeta = make(map[scopeKey]scopeMeta)
	}
	meta := a.scopeMeta[k]
	meta.real = uses
	a.scopeMeta[k] = meta
}

// realTracking is implemented by a proxy that relays the run's mock.real
// destinations (design v14 §4).
type realTracking interface {
	RealTracker() *realdeps.Tracker
}

// realTracker is what the run's REAL connections did; nil for a proxy that
// relays none.
func (a *Agent) realTracker() *realdeps.Tracker {
	if a.Proxy == nil {
		return nil
	}
	p, ok := a.Proxy.(realTracking)
	if !ok {
		return nil
	}
	return p.RealTracker()
}

// abandonWindow forgets a window a failed begin opened.
func (a *Agent) abandonWindow(id string) {
	a.scopeMu.Lock()
	k := scopeKey{pid: 0, name: id}
	delete(a.workerOpen, k)
	delete(a.scopeMeta, k)
	a.scopeMu.Unlock()
}

// reapOpenTest fails, before the run's windows are read, a test that was
// never ended. The windows are read only at the run's end (the CLI's read
// once the run's command has exited, the agent's own as it stops), so a test
// still open then can no longer end: its process exited, or it outlived the
// command that started it. Either way keploy cannot judge it, and it fails;
// it is never left out of the windows. A caller that goes away while it waits
// ends the read with its context's error.
func (a *Agent) reapOpenTest(ctx context.Context) error {
	if err := a.wire.lock(ctx); err != nil {
		return err
	}
	defer a.wire.unlock()
	a.provenMu.Lock()
	// The run is over: what a process still running claims or begins now
	// would never be read.
	a.proven.closed = true
	id, c := a.proven.open, a.proven.claim
	if id == "" {
		a.provenMu.Unlock()
		return nil
	}
	a.proven.claim = claimant{}
	a.provenMu.Unlock()
	why := "the test process exited while the test was open"
	if procAlive(c.pid, c.start) {
		why = fmt.Sprintf("the test was still open when keploy read the run's results: its process %d outlived the test command", c.pid)
	}
	a.failOpen(ctx, id, why)
	return nil
}

// sanitizedMiss is a miss as evidence keeps it: the protocol, the
// destination, how far the matcher got, the closest recorded mock's name,
// the matcher's next step, when it happened, why it missed (the learn step's
// reason, in record's full round), how an answered call was answered and the
// process that made the call (models.UnmatchedCall.Answer, Source: names,
// no value), and the call's shape
// (models.MissShape): for HTTP and gRPC the method and the target's shape,
// for SQL the statement with its literals written "?". No body, no bound
// parameter, no SQL literal, no query value, and no path segment that looks
// like a value (models.PathShape says which do) or that a test variable's
// place on the call names (VariablePlaces: one the miss names, one the
// closest mock or a mock on the call's route declares, or one that holds a
// value the test bound). The next step is the matcher's own fixed advice
// (nextStepOf).
func sanitizedMiss(m models.UnmatchedCall) models.UnmatchedCall {
	out := models.UnmatchedCall{Protocol: m.Protocol, Destination: m.Destination, MatchPhase: m.MatchPhase,
		ClosestMock: m.ClosestMock, NextSteps: nextStepOf(m), At: m.At, ActualSummary: models.MissShape(m), Differs: models.FieldShapes(m.Differs),
		Kind: m.Kind, Recorded: m.Recorded, NotOwned: m.NotOwned, Reason: m.Reason, Concurrent: m.Concurrent.Clone(),
		// An answered call stays one (models.UnmatchedCall.Answer): how it was
		// answered and by which process, no value.
		Answer: m.Answer, Drift: m.Drift, Reused: m.Reused, WriteKind: m.WriteKind, Source: m.Source, RecordedIn: m.RecordedIn,
		Repeat: m.Repeat.Clone()}
	// A variable's path places name segments, not values: kept, so a shape
	// of this miss (models.MissShape) still writes them {}. Any other place
	// can name a JSON key that is a value.
	for _, p := range m.VariablePlaces {
		if models.IsPathPlace(p) {
			out.VariablePlaces = append(out.VariablePlaces, p)
		}
	}
	if m.MatchPhase == models.MatchPhaseVariable {
		// A test variable did not hold, or the closest mock's variables could
		// not be followed. The part of the call it is in is kept: a URL path
		// segment by its position, a header or query parameter by its name.
		// A body place is kept as "body" alone, since a JSON key can be a
		// value the app sent (a map keyed by an email). No value is kept.
		for _, d := range m.FieldDiffs {
			path := d.Path
			if strings.HasPrefix(path, "body.") || strings.HasPrefix(path, "body[") {
				path = "body"
			}
			out.FieldDiffs = append(out.FieldDiffs, models.MockFieldDiff{Path: path, Kind: d.Kind})
		}
		out.NextSteps = "a test variable did not hold in " + m.ClosestMock + " (the call sent another value than the one the test bound, a value of another type, or a value before the call that binds it), or that mock's variables cannot be followed: fix the application, or re-record the test with 'keploy mock record' if that is intended"
	}
	return out
}

// nextStepOf is a miss's next step as evidence keeps it. Every matcher
// writes fixed advice, but one lists the fields that differed, and a map's
// keys can be values (an email, a user ID): a next step that names any of
// the miss's field paths is replaced by fixed advice that counts them.
func nextStepOf(m models.UnmatchedCall) string {
	for _, d := range m.FieldDiffs {
		if d.Path == "" {
			continue
		}
		if strings.Contains(m.NextSteps, d.Path) || strings.Contains(m.NextSteps, strings.TrimPrefix(d.Path, "body.")) {
			return fmt.Sprintf("%d field(s) of the call differ from the recording; if the change is expected, re-record the mock set with 'keploy mock record'", len(m.FieldDiffs))
		}
	}
	return m.NextSteps
}

// charge keeps misses drained from the proxy for the CLI's end-of-run read,
// as evidence keeps them (no value the app sent): they are counted, and
// named in the run's summary, but no full request piles up across a run.
func (a *Agent) charge(misses []models.UnmatchedCall) {
	if len(misses) == 0 {
		return
	}
	kept := make([]models.UnmatchedCall, len(misses))
	for i, m := range misses {
		kept[i] = sanitizedMiss(m)
	}
	a.provenMu.Lock()
	a.proven.charged = append(a.proven.charged, kept...)
	a.provenMu.Unlock()
}

func (a *Agent) wireClosed() bool {
	a.provenMu.Lock()
	defer a.provenMu.Unlock()
	return a.proven.closed
}

func errClosed() *models.ScopeError {
	return scopeErr(http.StatusConflict, models.ScopeErrNotInRun,
		"this keploy run's results were read: the run is over, and nothing begun now would be judged", "run the tests through keploy again")
}

// takeChargedMisses returns, once, the misses the ends drained from the proxy.
func (a *Agent) takeChargedMisses() []models.UnmatchedCall {
	a.provenMu.Lock()
	defer a.provenMu.Unlock()
	out := a.proven.charged
	a.proven.charged = nil
	return out
}

// evidenceReason is keploy's text for a failed test (design v14 §5.3). It
// names calls by method, destination and URL shape (models.PathShape), never
// by a body or a query value.
// beganAt is when the test began being served: a miss filed outside every
// test whose call was made at or after it was made as the test ended (its
// last call in flight, filed once the test stopped being served); one made
// before it, before the test began, an extra call of another test's
// included.
func evidenceReason(id string, ev *models.ScopeEvidence, mocks map[string]*models.Mock, beganAt time.Time) string {
	var what []string
	for _, c := range ev.Incomplete {
		what = append(what, "keploy could not capture a connection to "+c.Destination+" ("+c.Why+")")
	}
	// A miss whose call was made before the test began being served is no
	// call of the test's, whatever kind it was filed with: one another test
	// made, still in flight as that test ended, filed once this one began.
	before := func(m models.UnmatchedCall) bool {
		return !beganAt.IsZero() && !m.At.IsZero() && m.At.Before(beganAt)
	}
	// One call made more often than recorded is said once: how often the
	// test made it, and how often it was recorded.
	extra := map[string]int{}
	for _, m := range ev.Misses {
		if m.Kind == models.MissExtraCall && !before(m) {
			extra[missText(m)]++
		}
	}
	for _, m := range ev.Misses {
		if before(m) {
			// Where it was made is not known: a miss is filed by the view
			// served when it is filed, which may be after its own test.
			what = append(what, missText(m)+", made before this one began, had no recorded answer"+differsText(models.FieldShapes(m.Differs)))
			continue
		}
		switch m.Kind {
		case models.MissExtraCall:
			call := missText(m)
			if n, ok := extra[call]; ok {
				what = append(what, fmt.Sprintf("%s was called %d times, recorded %d", call, m.Recorded+n, m.Recorded))
				delete(extra, call)
			}
		case models.MissOutsideTest:
			if !beganAt.IsZero() && !m.At.IsZero() {
				what = append(what, missText(m)+", made as this test ended, had no recorded answer"+differsText(models.FieldShapes(m.Differs)))
				continue
			}
			what = append(what, missText(m)+", made outside every test before this one began, had no recorded answer"+differsText(models.FieldShapes(m.Differs)))
		default:
			what = append(what, missText(m)+" had no recorded answer"+differsText(models.FieldShapes(m.Differs)))
		}
	}
	unmade := append([]string(nil), ev.Unconsumed...)
	sort.Strings(unmade)
	for _, n := range unmade {
		what = append(what, "recorded call "+mockText(n, mocks[n])+" was never made")
	}
	if t := ev.CaptureIncomplete.Text(); t != "" {
		if len(what) == 0 {
			// Failed only for what keploy could not capture: re-recording
			// or quarantining the test would hide keploy's own loss, so
			// neither is offered; the loss's text says what was lost and
			// why.
			return "keploy: " + t + "."
		}
		// First: it says why the rest of the evidence may be short.
		what = append([]string{t}, what...)
	}
	const most = 5
	if len(what) > most {
		what = append(what[:most], fmt.Sprintf("and %d more", len(what)-most))
	}
	if len(ev.Misses) == 0 && len(ev.Unconsumed) == 0 {
		// Only connections keploy could not capture: each says what to
		// change, and nothing the test did needs re-recording or quarantine.
		return "keploy: " + strings.Join(what, "; ") + "."
	}
	return fmt.Sprintf("keploy: %s. If this is an intended change, re-record it: keploy mock record -c \"<your command, selecting %s>\".\n"+
		"If it is flaky, not broken: keploy mock quarantine %s --reason \"<why>\".", strings.Join(what, "; "), id, shellQuote(id))
}

// shellQuote quotes s for a POSIX shell unless it is made of characters a
// shell reads as one plain word.
func shellQuote(s string) string {
	plain := s != "" && !strings.ContainsFunc(s, func(r rune) bool {
		return !(r == '/' || r == ':' || r == '.' || r == '_' || r == '-' || r == '+' || r == '=' || r == ',' || r == '@' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	})
	if plain {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// missText names a miss: an HTTP or gRPC call by its method and target
// shape (models.MissShape); any other protocol by itself and its destination
// only, as one line of the reason (a SQL statement's shape is in the
// evidence).
func missText(m models.UnmatchedCall) string {
	proto := strings.TrimSpace(m.Protocol)
	s := "a call"
	if proto != "" {
		s = "a " + proto + " call"
	}
	if models.IsPathProtocol(proto) {
		if shape := models.MissShape(m); shape != "" {
			s = shape
		}
	}
	if m.Destination != "" {
		s += " to " + m.Destination
	}
	return s
}

// differsText names, for a miss's reason, the fields in which the call
// differed from its closest recording: " (body.amount differs)", " (host and
// header.accept differ)", " (a, b, c and 2 more differ)"; "" for none.
func differsText(fields []string) string {
	const most = 3
	switch n := len(fields); {
	case n == 0:
		return ""
	case n == 1:
		return " (" + fields[0] + " differs)"
	case n <= most:
		return " (" + strings.Join(fields[:n-1], ", ") + " and " + fields[n-1] + " differ)"
	default:
		return fmt.Sprintf(" (%s and %d more differ)", strings.Join(fields[:most], ", "), n-most)
	}
}

func mockText(name string, m *models.Mock) string {
	if m == nil {
		return name
	}
	if m.Kind == models.HTTP && m.Spec.HTTPReq != nil {
		return fmt.Sprintf("%s %s (%s)", m.Spec.HTTPReq.Method, models.MockURLShape(m), name)
	}
	return fmt.Sprintf("%s mock %s", m.Kind, name)
}
