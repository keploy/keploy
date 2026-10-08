package agent

import (
	"context"
	"strings"
	"time"

	httpparser "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/models"
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
		a.openWindow(name, pid, at)
		a.scopeMu.Lock()
		names, ok := a.scopeTable[name]
		a.scopeMu.Unlock()
		if !ok || len(names) == 0 {
			// No per-test mapping for this test — leave the whole pool armed.
			return nil
		}
		if pid > 0 {
			// Parallel-safe: narrow ONLY this worker's served view, keyed by its
			// PID, so concurrent workers never overwrite one shared filter.
			a.logger.Debug("scope begin: worker-scoped pool", zap.String("test", name), zap.Int("worker", pid), zap.Int("mocks", len(names)))
			a.SetWorkerScope(uint32(pid), names)
			return nil
		}
		// No worker PID reported — restrict the single global pool (correct for
		// a sequential single-worker suite, the pre-Design-A behavior).
		a.logger.Debug("scope begin: restricting served pool to test", zap.String("test", name), zap.Int("mocks", len(names)))
		return a.UpdateMockParams(ctx, models.MockFilterParams{
			MockMapping:     names,
			UseMappingBased: true,
			AfterTime:       models.BaseTime,
			BeforeTime:      time.Now(),
		})
	}

	a.openWindow(name, pid, at)
	a.logger.Debug("scope begin (record)", zap.String("test", name), zap.Int("worker", pid))
	return nil
}

// openWindow remembers when a test said it started, in either mode, so a client can read the windows later.
func (a *Agent) openWindow(name string, pid int, at time.Time) {
	a.scopeMu.Lock()
	if a.workerOpen == nil {
		a.workerOpen = make(map[scopeKey]time.Time)
	}
	a.workerOpen[scopeKey{pid: uint32(pid), name: name}] = boundaryTime(at)
	a.scopeMu.Unlock()
}

// closeWindow turns a started test into a window once it says it ended.
func (a *Agent) closeWindow(name string, pid int, at time.Time) {
	a.scopeMu.Lock()
	k := scopeKey{pid: uint32(pid), name: name}
	start, ok := a.workerOpen[k]
	if ok {
		delete(a.workerOpen, k)
		meta := a.scopeMeta[k]
		a.scopeWindows = append(a.scopeWindows, models.ScopeWindow{Name: name, Start: start, End: boundaryTime(at), PID: uint32(pid), Dir: meta.dir, Suite: meta.suite, Outcome: meta.outcome})
		delete(a.scopeMeta, k)
	}
	a.scopeMu.Unlock()
}

// BeginScope is BeginScopeAt stamped with the agent's clock.
func (a *Agent) BeginScope(ctx context.Context, name string, pid int) error {
	return a.BeginScopeAt(ctx, name, pid, time.Time{})
}

// EndScope closes a per-test scope. In record mode it records the [begin, now]
// window (tagged with the worker PID) for later correlation. In test mode it
// restores this worker's whole-pool view so a call made between tests still
// matches.
func (a *Agent) EndScopeAt(ctx context.Context, name string, pid int, at time.Time) error {
	if name == "" {
		return nil
	}
	if a.config != nil && a.config.Agent.Mode == models.MODE_TEST {
		a.noteEndOfGated(name, pid)
		a.closeWindow(name, pid, at)
		a.scopeMu.Lock()
		_, scoped := a.scopeTable[name]
		a.scopeMu.Unlock()
		if !scoped {
			return nil
		}
		if pid > 0 {
			a.logger.Debug("scope end: clearing worker scope", zap.String("test", name), zap.Int("worker", pid))
			a.ClearWorkerScope(uint32(pid))
			return nil
		}
		a.logger.Debug("scope end: restoring whole pool", zap.String("test", name))
		return a.UpdateMockParams(ctx, models.MockFilterParams{
			AfterTime:  models.BaseTime,
			BeforeTime: time.Now(),
		})
	}

	a.closeWindow(name, pid, at)
	a.logger.Debug("scope end (record)", zap.String("test", name), zap.Int("worker", pid))
	return nil
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

// GetScopeWindows returns the per-test windows collected this record session,
// consumed by the CLI to build mappings.yaml.
func (a *Agent) GetScopeWindows(_ context.Context) ([]models.ScopeWindow, error) {
	a.scopeMu.Lock()
	defer a.scopeMu.Unlock()
	out := make([]models.ScopeWindow, len(a.scopeWindows))
	copy(out, a.scopeWindows)
	for _, s := range starts.Default.List() {
		out = append(out, models.ScopeWindow{Name: s.Key, Start: s.First, End: s.First, Ready: s.Ready, PID: s.Root, Dir: s.Dir, App: true, Port: s.Port, Program: s.Program, Place: s.Place, N: s.N, Ref: s.Ref, Worker: s.Worker})
	}
	return out, nil
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
	dir     string
	suite   bool
	outcome string
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
