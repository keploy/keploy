package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	httpparser "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// The scope API lets a user's own test runner mark per-test boundaries so
// Keploy can attribute captured mocks to a test (record) and serve only that
// test's mocks (replay). A pytest fixture / go-test helper / jest hook / curl
// script calls, using the KEPLOY_MOCK_AGENT URL Keploy exports into the wrapped
// command:
//
//	POST {url}/agent/scope/begin  {"name":"<test>"}
//	POST {url}/agent/scope/end    {"name":"<test>"}
//
// The control plane is authenticated, so both calls must carry the token Keploy
// exports next to the URL:
//
//	Authorization: Bearer $KEPLOY_MOCK_AGENT_TOKEN
//
// Treat it as absent-able: a runner driven by an older Keploy will not see the
// variable, and an agent started without a token accepts either way. Send the
// header when the variable is set and omit it when it is not.
//
// A harness that knows the test's verdict sends it with the end, so a replay
// can tell the tests that pass with every dependency mocked from the rest:
//
//	POST {url}/agent/scope/end    {"name":"<test>","outcome":"passed|failed|skipped"}
//
// A replay may run only some tests (see SetScopeGate). The begin of a test it
// gated out is answered {"status":"ok","action":"skip","reason":"..."}; the
// harness then skips the test and sends no end for it. Only a harness that
// says it can skip is told to:
//
//	POST {url}/agent/scope/begin  {"name":"<test>","canSkip":true}
//
// Any other answer is {"status":"ok"}: run the test.
//
// Scoping is entirely optional: with no scope calls the set records/replays
// suite-level, which is still correct.

// scopeKey identifies an open record-mode scope by (reporting worker PID, test
// name). Keying by BOTH — not PID alone — keeps two things correct at once:
// parallel workers don't collide (distinct PIDs), and a single worker's nested
// or overlapping named scopes (or the legacy pid==0 path) don't clobber each
// other the way a PID-only key would.
type scopeKey struct {
	pid  uint32
	name string
}

// BeginScope opens a per-test scope. In record mode it stamps the begin time so
// captured mocks can later be bucketed to this test. In test mode it restricts
// the served pool to this test's mocks when the CLI supplied a mapping table.
//
// pid is the calling worker's PID (ScopeReq.Pid). When > 0 the scope is keyed to
// that worker so PARALLEL workers each get their own served view without
// stomping each other (Design A); pid == 0 falls back to the single global
// scope (sequential single-worker runs, and the suite-level default).
func (a *Agent) BeginScopeAt(ctx context.Context, name string, pid int, at time.Time) error {
	if name == "" {
		return nil
	}
	if a.config != nil && a.config.Agent.Mode == models.MODE_TEST {
		took, lost, err := a.beginTestClock(ctx, name, pid)
		if lost.key.name != "" {
			a.failLostScope(ctx, lost, at)
		}
		if err != nil {
			// The test fails whatever its harness does with the refusal: it
			// cannot run on its recording's clock.
			a.noteScopeFailed(name, pid, at)
			return fmt.Errorf("keploy could not move the clock to when %s began in its recording: %w", name, err)
		}
		a.openWindow(name, pid, at)
		// Under the proven switch a test is the scope of the variables
		// keploy mock replay binds: each test begins with none bound. The
		// proven wire refuses a begin while a test is open (ProvenBegin), so
		// one test is one scope, and the agent keeps one table. The scope
		// begins once the test's pool is in, at each return of a begin that
		// succeeded, so a call checked against the previous pool binds
		// nothing in it; a begin that fails narrowed nothing and begins none.
		beginVariables := func() {
			if provenswitch.On() {
				a.ResetVariables()
			}
		}
		if pid == 0 && provenswitch.On() {
			// The proven wire serves a test exactly its own mocks
			// (exact_proven.go): one with no mapping, or none in it, owns
			// none, so any data-plane call of its own is a miss.
			if err := a.serveTestExactly(ctx, name); err != nil {
				return a.refuseBegin(ctx, name, pid, at, took, err)
			}
			beginVariables()
			return nil
		}
		a.scopeMu.Lock()
		names, ok := a.scopeTable[name]
		a.scopeMu.Unlock()
		var params models.MockFilterParams
		// asScope: the begin is staged as a test window of its mapping
		// (stageScope), not through params.
		asScope := false
		switch {
		case pid == 0 && ok && len(names) == 0 && !provenswitch.On():
			// Recorded with no call, with the switch off: a test window over
			// no per-test mock of its own (stageScope), so a call it makes is
			// answered by the reusable tier or misses, never by another
			// test's recording.
			a.logger.Debug("scope begin: a test recorded with no call is served the reusable tier only, in a test window", zap.String("test", name))
			asScope = true
		case !ok || len(names) == 0:
			// No per-test mapping for this test — leave the whole pool armed.
			beginVariables()
			return nil
		case pid > 0:
			// Parallel-safe: narrow ONLY this worker's served view, keyed by its
			// PID, so concurrent workers never overwrite one shared filter.
			a.logger.Debug("scope begin: worker-scoped pool", zap.String("test", name), zap.Int("worker", pid), zap.Int("mocks", len(names)))
			a.SetWorkerScope(uint32(pid), names)
			beginVariables()
			return nil
		case !provenswitch.On():
			// No worker PID reported, with the switch off: restrict the
			// single global pool to the test, in a test window of its own
			// (stageScope).
			a.logger.Debug("scope begin: restricting served pool to test, in a test window", zap.String("test", name), zap.Int("mocks", len(names)))
			asScope = true
		default:
			// Only a negative pid under the switch, which the proven wire
			// refuses (oldWireFields), so only an in-process caller passes one:
			// pid 0 under the switch was served exactly above, and with the
			// switch off case !provenswitch.On() staged it. Restrict the single
			// global pool, the pre-Design-A behavior.
			a.logger.Debug("scope begin: restricting served pool to test", zap.String("test", name), zap.Int("mocks", len(names)))
			params = models.MockFilterParams{
				MockMapping:     names,
				UseMappingBased: true,
			}
		}
		stage := func() error { return a.stagePool(ctx, params) }
		if asScope {
			stage = func() error { return a.stageScope(ctx, names, true) }
		}
		if err := stage(); err != nil {
			return a.refuseBegin(ctx, name, pid, at, took, err)
		}
		beginVariables()
		return nil
	}

	a.openWindow(name, pid, at)
	a.logger.Debug("scope begin (record)", zap.String("test", name), zap.Int("worker", pid))
	return nil
}

// refuseBegin fails a replayed test's begin with err, after its window
// opened. A test that took the clock gives it back now, and is failed
// whatever its harness does with the refusal (an end it sends closes
// nothing), as a test refused the clock is.
func (a *Agent) refuseBegin(ctx context.Context, name string, pid int, at time.Time, took bool, err error) error {
	if !took {
		return err
	}
	a.withdrawWindow(name, pid)
	a.noteScopeFailed(name, pid, at)
	return errors.Join(err, a.endTestClock(ctx, name, pid))
}

// openWindow remembers when a test said it started, in either mode, so a client can read the windows later.
func (a *Agent) openWindow(name string, pid int, at time.Time) {
	k := scopeKey{pid: uint32(pid), name: name}
	var proc string
	if pid > 0 && a.config != nil && a.config.Agent.Mode == models.MODE_TEST && provenswitch.On() {
		// Which process opened it, so a lost worker's tests are told from a
		// new process's on its pid (failLostScope).
		proc = procStartTime(pid)
	}
	a.scopeMu.Lock()
	if a.workerOpen == nil {
		a.workerOpen = make(map[scopeKey]time.Time)
	}
	a.workerOpen[k] = boundaryTime(at)
	if proc != "" {
		if a.openProc == nil {
			a.openProc = make(map[scopeKey]string)
		}
		a.openProc[k] = proc
	} else {
		delete(a.openProc, k)
	}
	a.scopeMu.Unlock()
}

// failLostScope records failed a test that held the run's clock and whose
// worker exited before its end came (beginTestClock), and every other test
// that worker still had open (its subtests, a suite; not those of a new
// process on its pid): their windows close at at, the next test's begin,
// with no outcome of their harness's, since no end will ever come. The pool
// each of them narrowed is given back.
func (a *Agent) failLostScope(ctx context.Context, lost lostScope, at time.Time) {
	k := lost.key
	a.scopeMu.Lock()
	var open []scopeKey
	for o := range a.workerOpen {
		if o == k || (k.pid != 0 && o.pid == k.pid && a.openProc[o] == lost.start) {
			open = append(open, o)
		}
	}
	if a.scopeMeta == nil {
		a.scopeMeta = make(map[scopeKey]scopeMeta)
	}
	for _, o := range open {
		meta := a.scopeMeta[o]
		meta.outcome = models.ScopeOutcomeFailed
		a.scopeMeta[o] = meta
	}
	a.scopeMu.Unlock()
	for _, o := range open {
		a.closeWindow(o.name, int(o.pid), at)
		if err := a.restorePool(ctx, o.name, int(o.pid)); err != nil {
			a.logger.Debug("could not give back the pool of a test whose worker exited", zap.String("test", o.name), zap.Error(err))
		}
	}
	if len(open) == 0 {
		a.noteScopeFailed(k.name, int(k.pid), at)
	}
}

// withdrawWindow forgets a started test's window: the test failed at its
// begin, and its end must not record it as having run.
func (a *Agent) withdrawWindow(name string, pid int) {
	k := scopeKey{pid: uint32(pid), name: name}
	a.scopeMu.Lock()
	delete(a.workerOpen, k)
	delete(a.openProc, k)
	delete(a.scopeMeta, k)
	a.scopeMu.Unlock()
}

// closeWindow turns a started test into a window once it says it ended, and
// reports whether there was one to close.
func (a *Agent) closeWindow(name string, pid int, at time.Time) bool {
	a.scopeMu.Lock()
	k := scopeKey{pid: uint32(pid), name: name}
	start, ok := a.workerOpen[k]
	if ok {
		delete(a.workerOpen, k)
		delete(a.openProc, k)
		meta := a.scopeMeta[k]
		a.scopeWindows = append(a.scopeWindows, models.ScopeWindow{Name: name, Start: start, End: boundaryTime(at), PID: uint32(pid), Dir: meta.dir, Suite: meta.suite, Outcome: meta.outcome, Evidence: meta.evidence, Real: meta.real})
		delete(a.scopeMeta, k)
	}
	a.scopeMu.Unlock()
	return ok
}

// BeginScope is BeginScopeAt stamped with the agent's clock.
func (a *Agent) BeginScope(ctx context.Context, name string, pid int) error {
	return a.BeginScopeAt(ctx, name, pid, time.Time{})
}

// EndScope closes a per-test scope. In record mode it records the [begin, now]
// window (tagged with the worker PID) for later correlation. In test mode it
// restores this worker's whole-pool view so a call made between tests still
// matches; with no worker PID and the switch off, the whole pool less what
// tests consumed (stageScope).
func (a *Agent) EndScopeAt(ctx context.Context, name string, pid int, at time.Time) error {
	if name == "" {
		return nil
	}
	if a.config != nil && a.config.Agent.Mode == models.MODE_TEST {
		a.noteEndOfGated(name, pid)
		closed := a.closeWindow(name, pid, at)
		// Under the switch, only the end of a begin that opened a window gives
		// the pool back: a test keploy refused or withdrew at its begin, or an
		// end sent twice, narrowed nothing, and giving the pool back would
		// widen the view of the test that holds the clock. The pool is
		// restored before the clock moves back, so a clock that cannot move
		// leaves no test's view narrowed. endTestClock moves only the clock
		// this test holds, or its own move still in force.
		var err error
		if closed || !provenswitch.On() {
			err = a.restorePool(ctx, name, pid)
		}
		if cerr := a.endTestClock(ctx, name, pid); cerr != nil {
			err = errors.Join(err, fmt.Errorf("keploy could not move the clock back after %s: %w", name, cerr))
		}
		return err
	}

	a.closeWindow(name, pid, at)
	a.logger.Debug("scope end (record)", zap.String("test", name), zap.Int("worker", pid))
	return nil
}

// restorePool gives a worker, or the whole run, back the whole pool after a
// test whose mapping narrowed it (the whole run, with the switch off, less
// what tests consumed: stageScope); under the proven wire, the current key's
// pool slice after any test.
func (a *Agent) restorePool(ctx context.Context, name string, pid int) error {
	a.scopeMu.Lock()
	_, scoped := a.scopeTable[name]
	a.scopeMu.Unlock()
	// The proven wire narrowed even a test without a mapping entry.
	if !scoped && !(pid == 0 && provenswitch.On()) {
		return nil
	}
	if pid > 0 {
		a.logger.Debug("scope end: clearing worker scope", zap.String("test", name), zap.Int("worker", pid))
		a.ClearWorkerScope(uint32(pid))
		return nil
	}
	if pid == 0 && provenswitch.On() {
		// Between tests the proven wire serves the current key's pool
		// slice (exact_proven.go).
		a.logger.Debug("scope end: serving the pool slice of the current key", zap.String("test", name))
		return a.servePoolExactly(ctx)
	}
	a.logger.Debug("scope end: restoring whole pool", zap.String("test", name))
	if !provenswitch.On() {
		// Closes the test window the begin opened, with the mocks tests
		// consumed left out (stageScope).
		return a.stageScope(ctx, nil, false)
	}
	// Only a negative pid under the switch, from an in-process caller,
	// reaches here: pid 0 under the switch served its pool slice exactly
	// above.
	return a.stagePool(ctx, models.MockFilterParams{})
}

// EndScope is EndScopeAt stamped with the agent's clock.
func (a *Agent) EndScope(ctx context.Context, name string, pid int) error {
	return a.EndScopeAt(ctx, name, pid, time.Time{})
}

// boundaryTime is the runner's time when it gave one, else now.
func boundaryTime(at time.Time) time.Time {
	if at.IsZero() {
		return time.Now()
	}
	return at
}

// GetScopeWindows returns the per-test windows collected so far this
// session. A read at the run's end is FinalScopeWindows.
func (a *Agent) GetScopeWindows(ctx context.Context) ([]models.ScopeWindow, error) {
	return a.scopeWindowsNow(), nil
}

// FinalScopeWindows is the run's last read of its windows, once its command
// has exited: under the proven wire a test still open is failed and closed
// (so its window is among these), and the wire is closed, as nothing begun
// now would be read.
func (a *Agent) FinalScopeWindows(ctx context.Context) ([]models.ScopeWindow, error) {
	if err := a.reapOpenTest(ctx); err != nil {
		return nil, err
	}
	return a.scopeWindowsNow(), nil
}

func (a *Agent) scopeWindowsNow() []models.ScopeWindow {
	// Read before scopeMu: provenMu is never held while taking it.
	claims := a.claimWindows()
	a.scopeMu.Lock()
	defer a.scopeMu.Unlock()
	out := make([]models.ScopeWindow, len(a.scopeWindows), len(a.scopeWindows)+len(claims))
	copy(out, a.scopeWindows)
	out = append(out, claims...)
	for _, s := range starts.Default.List() {
		out = append(out, models.ScopeWindow{Name: s.Key, Start: s.First, End: s.First, Ready: s.Ready, PID: s.Root, Dir: s.Dir, App: true, Port: s.Port, Program: s.Program, Place: s.Place, N: s.N, Ref: s.Ref, Worker: s.Worker})
	}
	return out
}

// SetScopeTable installs the replay-time per-test name→mock-names table the CLI
// read from mappings.yaml.
func (a *Agent) SetScopeTable(_ context.Context, table map[string][]string) error {
	a.scopeMu.Lock()
	a.scopeTable = table
	a.scopeMu.Unlock()

	// Push the union of every test's mapped mock names to the proxy so a scoped
	// worker can tell another test's mock (hide) from a genuinely-shared,
	// unmapped recording (keep). De-duplicated across tests.
	seen := make(map[string]struct{})
	universe := make([]string, 0)
	for _, names := range table {
		for _, n := range names {
			if _, ok := seen[n]; !ok {
				seen[n] = struct{}{}
				universe = append(universe, n)
			}
		}
	}
	a.SetMappedUniverse(universe)
	return nil
}

// DrainCapturedMocks returns and clears the mocks captured on miss during a
// `--on-miss record` replay session, so the CLI can append them to the set.
func (a *Agent) DrainCapturedMocks(_ context.Context) ([]*models.Mock, error) {
	return httpparser.DrainCaptured(), nil
}

// MockStats returns a non-draining snapshot for /agent/mock/stats. Consumed and
// missed totals are surfaced in the CLI's end-of-run summary (they drain their
// capture windows), so this live endpoint reports the loaded count only.
func (a *Agent) MockStats(_ context.Context) (models.MockStats, error) {
	a.scopeMu.Lock()
	loaded := a.loadedMocks
	a.scopeMu.Unlock()
	return models.MockStats{Loaded: loaded}, nil
}

type scopeMeta struct {
	dir      string
	suite    bool
	outcome  string
	evidence *models.ScopeEvidence
	// real are the mock.real destinations the test used while open.
	real []models.RealUse
}

func (a *Agent) NoteScope(name string, pid int, dir string, suite bool) {
	if name == "" || (dir == "" && !suite) {
		return
	}
	a.scopeMu.Lock()
	if a.scopeMeta == nil {
		a.scopeMeta = make(map[scopeKey]scopeMeta)
	}
	a.scopeMeta[scopeKey{pid: uint32(pid), name: name}] = scopeMeta{dir: dir, suite: suite}
	a.scopeMu.Unlock()
}

// NoteScopeOutcome records the verdict a test's harness reported as it ended
// the scope, for the window that end closes. Called before the end itself,
// and only for a scope that is open: an end with no begin closes nothing, so
// its verdict must not wait to stamp a later run of the test.
func (a *Agent) NoteScopeOutcome(name string, pid int, outcome string) {
	if name == "" || outcome == "" {
		return
	}
	a.scopeMu.Lock()
	defer a.scopeMu.Unlock()
	k := scopeKey{pid: uint32(pid), name: name}
	if _, open := a.workerOpen[k]; !open {
		return
	}
	if a.scopeMeta == nil {
		a.scopeMeta = make(map[scopeKey]scopeMeta)
	}
	meta := a.scopeMeta[k]
	meta.outcome = outcome
	a.scopeMeta[k] = meta
}

// NoteScopeGated records a test the replay told not to run, as a window with
// no extent and the ScopeOutcomeGated verdict, so the replay's outcome and
// receipt say it was gated rather than that it never appeared.
func (a *Agent) NoteScopeGated(name string, pid int, at time.Time) {
	if name == "" {
		return
	}
	t := boundaryTime(at)
	a.scopeMu.Lock()
	a.scopeWindows = append(a.scopeWindows, models.ScopeWindow{Name: name, Start: t, End: t, PID: uint32(pid), Outcome: models.ScopeOutcomeGated})
	if a.gatedScopes == nil {
		a.gatedScopes = make(map[scopeKey]struct{})
	}
	a.gatedScopes[scopeKey{pid: uint32(pid), name: name}] = struct{}{}
	a.scopeMu.Unlock()
}

// noteScopeFailed records a test keploy failed at its begin, as a window with
// no extent and the failed verdict, so the replay's outcome and receipt count
// it failed even when its harness ignores the refusal and runs it.
func (a *Agent) noteScopeFailed(name string, pid int, at time.Time) {
	t := boundaryTime(at)
	a.scopeMu.Lock()
	a.scopeWindows = append(a.scopeWindows, models.ScopeWindow{Name: name, Start: t, End: t, PID: uint32(pid), Outcome: models.ScopeOutcomeFailed})
	a.scopeMu.Unlock()
}

// noteEndOfGated warns, once per session, when a harness ends a scope it was
// told to skip and never began: the harness said it can skip, then ran the
// test anyway. The test is recorded as gated; what it did is not counted.
func (a *Agent) noteEndOfGated(name string, pid int) {
	k := scopeKey{pid: uint32(pid), name: name}
	a.scopeMu.Lock()
	_, gated := a.gatedScopes[k]
	_, open := a.workerOpen[k]
	warn := gated && !open && !a.gatedEndWarned
	if warn {
		a.gatedEndWarned = true
	}
	a.scopeMu.Unlock()
	if warn {
		a.logger.Warn("the test runner's harness ended a test it was told to skip; it is recorded as not run, and what it did is not counted",
			zap.String("test", name),
			zap.String("next_step", `make the harness skip a test whose /agent/scope/begin answer says "action":"skip"`))
	}
}

// NoteUngatable warns, once per session, that the replay gates which tests
// run but the runner's harness cannot skip one (it did not send canSkip), so
// the gate is not applied: every test runs.
func (a *Agent) NoteUngatable(name string) {
	a.scopeMu.Lock()
	warned := a.gateWarned
	a.gateWarned = true
	a.scopeMu.Unlock()
	if warned {
		return
	}
	a.logger.Warn("this replay runs only some tests, but the test runner's harness cannot skip one, so every test runs",
		zap.String("test", name),
		zap.String("next_step", `update the harness: send "canSkip":true with /agent/scope/begin and skip a test whose answer says "action":"skip"`))
}

// SetScopeGate names the only tests that should run this replay; run == nil
// lets every test run. The replay CLI installs it every session, and a new
// session (resetScopeState) clears it, so a gate never outlives its replay.
func (a *Agent) SetScopeGate(_ context.Context, run []string, reason string) error {
	var set map[string]struct{}
	if run != nil {
		set = make(map[string]struct{}, len(run))
		for _, n := range run {
			set[n] = struct{}{}
		}
	}
	a.scopeMu.Lock()
	a.gateRun, a.gateReason = set, reason
	a.scopeMu.Unlock()
	return nil
}

// ScopeRun reports whether the test named name should run under the gate, and
// why not when it should not. Only a replay is gated; a subtest of a test the
// gate names (name "TestX/case" under "TestX") runs with it.
func (a *Agent) ScopeRun(name string) (bool, string) {
	if a.config == nil || a.config.Agent.Mode != models.MODE_TEST {
		return true, ""
	}
	a.scopeMu.Lock()
	defer a.scopeMu.Unlock()
	if a.gateRun == nil {
		return true, ""
	}
	for n := name; ; {
		if _, ok := a.gateRun[n]; ok {
			return true, ""
		}
		i := strings.LastIndex(n, "/")
		if i <= 0 {
			break
		}
		n = n[:i]
	}
	return false, a.gateReason
}
