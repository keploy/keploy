package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
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
	// atBegin is how often each mock had answered when the open test began.
	atBegin map[string]uint64
	// skip is the gate: test ID -> the reason its harness skips it with.
	skip map[string]string
	// charged holds the misses the ends drained from the proxy, until the
	// CLI's own end-of-run read (GetMockErrors) collects them, so charging a
	// miss to a test never hides it from the run's totals.
	charged []models.UnmatchedCall
	// claim is the live claim; zero when none.
	claim claimant
	// closed: the run's windows were read at its end; nothing more is
	// claimed or begun until the next session resets the wire.
	closed bool
}

// claimant is the process that claimed the run: a pid, and its start time,
// so a reused pid is never taken for it.
type claimant struct {
	pid   int
	start uint64
}

// servedCounter is implemented by a proxy that counts how often each mock
// answered.
type servedCounter interface {
	ServedCounts() map[string]uint64
}

// missCapturer is implemented by a proxy that keeps each test's misses
// apart, carrying those made between tests into the next test.
type missCapturer interface {
	ContinueTestErrorCapture()
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

// clientRoot is the process keploy's CLI registered as its client.
func (a *Agent) clientRoot() int {
	if a.config == nil {
		return 0
	}
	return int(a.config.Agent.ClientNSPID)
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
	a.resetScopeState()
	a.provenMu.Lock()
	a.proven = provenWire{}
	a.provenMu.Unlock()
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
	root := a.clientRoot()
	if root <= 0 {
		return models.InstanceResp{}, scopeErr(http.StatusUnprocessableEntity, models.ScopeErrNotInRun,
			"this keploy run cannot take a test process's claim: keploy did not start its command natively (docker compose)",
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
	stale, staleBegin := "", map[string]uint64(nil)
	if prev != me {
		stale, staleBegin = a.proven.open, a.proven.atBegin
	}
	a.provenMu.Unlock()
	if stale != "" {
		a.failOpen(ctx, stale, staleBegin, "the test process exited while the test was open")
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
	a.provenMu.Lock()
	a.proven.claim = me
	a.proven.key, a.proven.dir = key, req.Dir
	a.provenMu.Unlock()
	return models.InstanceResp{V: models.ScopeProtocolVersion, Instance: models.InstanceRun}, nil
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
	at := boundaryTime(req.At)
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
	var counts map[string]uint64
	var capture missCapturer
	if a.replaying() {
		var err error
		if counts, err = a.servedCounts(); err != nil {
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
	a.NoteScope(id, 0, dir, false)
	if err := a.BeginScopeAt(ctx, id, 0, at); err != nil {
		a.abandonWindow(id)
		return models.ScopeBeginResp{}, scopeErr(http.StatusInternalServerError, models.ScopeErrInternal,
			"keploy's agent could not open "+id+": "+err.Error(), "check the agent's log")
	}
	if capture != nil {
		// Misses made from here, and those made since the previous test
		// ended (or before the first test), are this test's: a miss outside
		// every test is charged to the next test that begins (design v14
		// §5.1).
		capture.ContinueTestErrorCapture()
	}
	a.provenMu.Lock()
	a.proven.open, a.proven.atBegin = id, counts
	a.provenMu.Unlock()
	return models.ScopeBeginResp{V: models.ScopeProtocolVersion, Status: "ok"}, nil
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
	at := boundaryTime(req.At)
	a.provenMu.Lock()
	id := models.TestID(req.Project, req.File, a.proven.key, req.Name)
	open, atBegin := a.proven.open, a.proven.atBegin
	a.provenMu.Unlock()
	if open != id {
		// A harness that ends a test it was told to skip is warned.
		a.noteEndOfGated(id, 0)
		msg := fmt.Sprintf("%s ended, but it is not the open test", id)
		if open == "" {
			msg = fmt.Sprintf("%s ended, but no test is open", id)
		}
		return models.ScopeEndResp{}, scopeErr(http.StatusConflict, models.ScopeErrNotOpen, msg,
			"end a test once, with the name its begin sent, and only when its begin was not answered skip")
	}
	outcome := models.NormalizeScopeOutcome(req.Outcome)
	if outcome == "" {
		outcome = models.ScopeOutcomeFailed
	}
	ev, err := a.closeOpen(ctx, id, outcome, at, atBegin, "")
	if err != nil {
		return models.ScopeEndResp{}, scopeErr(http.StatusInternalServerError, models.ScopeErrInternal,
			"keploy could not judge "+id+": "+err.Error(), "check the agent's log; the test is counted failed")
	}
	if !ev.Empty() {
		return models.ScopeEndResp{V: models.ScopeProtocolVersion, Status: "ok", Action: models.ScopeActionFail, Reason: ev.Reason}, nil
	}
	return models.ScopeEndResp{V: models.ScopeProtocolVersion, Status: "ok"}, nil
}

// closeOpen ends the open test id (endTest) and clears it, whatever happens
// on the way. With the wire held.
func (a *Agent) closeOpen(ctx context.Context, id, outcome string, at time.Time, atBegin map[string]uint64, why string) (*models.ScopeEvidence, error) {
	defer func() {
		a.provenMu.Lock()
		if a.proven.open == id {
			a.proven.open, a.proven.atBegin = "", nil
		}
		a.provenMu.Unlock()
	}()
	return a.endTest(ctx, id, outcome, at, atBegin, why)
}

// failOpen ends, as failed, a test whose harness can no longer end it. With
// the wire held.
func (a *Agent) failOpen(ctx context.Context, id string, atBegin map[string]uint64, why string) {
	if _, err := a.closeOpen(ctx, id, models.ScopeOutcomeFailed, time.Now(), atBegin, why); err != nil {
		a.logger.Warn("could not judge a test whose process went away", zap.String("test", id), zap.Error(err))
	}
	a.logger.Warn("a test is counted failed: "+why, zap.String("test", id))
}

// endTest records a test's outcome and evidence and closes its window. A
// read that failed is an error, and the test is recorded failed with it: a
// test keploy could not judge never passes. A test that skipped itself is
// not failed for the recorded calls it then never made.
func (a *Agent) endTest(ctx context.Context, id, outcome string, at time.Time, atBegin map[string]uint64, why string) (*models.ScopeEvidence, error) {
	var ev *models.ScopeEvidence
	var readErr error
	if a.replaying() {
		ev, readErr = a.testEvidence(ctx, id, atBegin, outcome != models.ScopeOutcomeSkipped)
		switch {
		case readErr != nil:
			if ev == nil {
				ev = &models.ScopeEvidence{}
			}
			ev.Reason = "keploy: keploy could not tell what " + id + " did: " + readErr.Error()
			outcome = models.ScopeOutcomeFailed
		case !ev.Empty():
			ev.Reason = evidenceReason(id, ev, a.mocksFound(ev.Unconsumed))
			outcome = models.ScopeOutcomeFailed
		}
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
		// is counted failed, as its end answer says.
		a.failClosedWindow(id, "keploy: keploy could not end "+id+": "+err.Error())
		if readErr == nil {
			readErr = err
		}
	}
	return ev, readErr
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
// misses (DNS is protocol-phase), and the data-plane mocks of its mapping it
// did not consume while it was open. A failed read is an error.
func (a *Agent) testEvidence(ctx context.Context, id string, atBegin map[string]uint64, unmade bool) (*models.ScopeEvidence, error) {
	if a.Proxy == nil {
		return nil, errors.New("no proxy")
	}
	ev := &models.ScopeEvidence{}
	misses, err := a.Proxy.GetMockErrors(ctx)
	// Misses a read returned with its error are this test's all the same.
	a.charge(misses)
	for _, m := range misses {
		if strings.EqualFold(m.Protocol, "DNS") {
			continue
		}
		ev.Misses = append(ev.Misses, sanitizedMiss(m))
	}
	if err != nil {
		return ev, fmt.Errorf("reading its misses: %w", err)
	}
	a.scopeMu.Lock()
	names := append([]string(nil), a.scopeTable[id]...)
	a.scopeMu.Unlock()
	if len(names) == 0 || !unmade {
		return ev, nil
	}
	// A read that fails from here keeps the misses already found: they are
	// this test's whatever else could not be told.
	mocks, err := a.mocksByName(names)
	if err != nil {
		return ev, err
	}
	now, err := a.servedCounts()
	if err != nil {
		return ev, err
	}
	for _, n := range names {
		if mocks[n].IsProtocolPhase() || now[n] > atBegin[n] {
			continue
		}
		ev.Unconsumed = append(ev.Unconsumed, n)
	}
	return ev, nil
}

// servedCounts is how often each mock has answered this session.
func (a *Agent) servedCounts() (map[string]uint64, error) {
	c, ok := a.Proxy.(servedCounter)
	if !ok || a.Proxy == nil {
		return nil, errors.New("this keploy agent's proxy does not count which mocks answered")
	}
	return c.ServedCounts(), nil
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
	for _, n := range names {
		want[n] = true
	}
	s.mu.RLock()
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
	id, atBegin, c := a.proven.open, a.proven.atBegin, a.proven.claim
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
	a.failOpen(ctx, id, atBegin, why)
	return nil
}

// sanitizedMiss is a miss as evidence keeps it: the protocol, the
// destination, how far the matcher got, when it happened, and, for HTTP, the
// method and the path's shape; never a value the app sent (a query's
// literals, a path's IDs, a body).
func sanitizedMiss(m models.UnmatchedCall) models.UnmatchedCall {
	out := models.UnmatchedCall{Protocol: m.Protocol, Destination: m.Destination, MatchPhase: m.MatchPhase, At: m.At}
	if isHTTP(m.Protocol) {
		if method, path, ok := strings.Cut(strings.TrimSpace(m.ActualSummary), " "); ok && method != "" {
			out.ActualSummary = method + " " + pathShape(path)
		}
	}
	return out
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

// isHTTP reports whether a miss's protocol is HTTP, whose summary is a method
// and a path: "HTTP", "HTTP2" or "HTTP/2".
func isHTTP(proto string) bool {
	return strings.EqualFold(proto, "HTTP") || strings.EqualFold(proto, "HTTP2") || strings.EqualFold(proto, "HTTP/2")
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
// names calls by method, destination and path shape, never by a recorded or
// sent value.
func evidenceReason(id string, ev *models.ScopeEvidence, mocks map[string]*models.Mock) string {
	var what []string
	for _, m := range ev.Misses {
		what = append(what, missText(m)+" had no recorded answer")
	}
	unmade := append([]string(nil), ev.Unconsumed...)
	sort.Strings(unmade)
	for _, n := range unmade {
		what = append(what, "recorded call "+mockText(n, mocks[n])+" was never made")
	}
	const most = 5
	if len(what) > most {
		what = append(what[:most], fmt.Sprintf("and %d more", len(what)-most))
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

var (
	allDigits = regexp.MustCompile(`^[0-9]+$`)
	hasDigit  = regexp.MustCompile(`[0-9]`)
)

// pathShape keeps a path's shape and drops its values: a segment of digits,
// of eight or more characters with a digit in it (an ID, a hash, a UUID), or
// with an @ (an email address), becomes {}; the query is dropped. A value
// that looks like a word (a user name) is kept: the shape is a best effort.
func pathShape(p string) string {
	p, _, _ = strings.Cut(p, "?")
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if allDigits.MatchString(s) || (len(s) >= 8 && hasDigit.MatchString(s)) || strings.Contains(s, "@") {
			segs[i] = "{}"
		}
	}
	return strings.Join(segs, "/")
}

// missText names a miss: an HTTP request by method and path shape; any other
// protocol by itself and its destination only, since its summary can hold
// the values it sent (a query's literals).
func missText(m models.UnmatchedCall) string {
	proto := strings.TrimSpace(m.Protocol)
	s := "a call"
	if proto != "" {
		s = "a " + proto + " call"
	}
	if isHTTP(proto) {
		if method, path, ok := strings.Cut(strings.TrimSpace(m.ActualSummary), " "); ok && method != "" {
			s = method + " " + pathShape(path)
		}
	}
	if m.Destination != "" {
		s += " to " + m.Destination
	}
	return s
}

func mockText(name string, m *models.Mock) string {
	if m == nil {
		return name
	}
	if m.Kind == models.HTTP && m.Spec.HTTPReq != nil {
		where := pathShape(m.Spec.HTTPReq.URL)
		if u, err := url.Parse(m.Spec.HTTPReq.URL); err == nil && u.Host != "" {
			where = u.Host + pathShape(u.EscapedPath())
		}
		return fmt.Sprintf("%s %s (%s)", m.Spec.HTTPReq.Method, where, name)
	}
	return fmt.Sprintf("%s mock %s", m.Kind, name)
}
