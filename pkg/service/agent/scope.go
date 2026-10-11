package agent

import (
	"context"
	"strings"
	"time"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	httpparser "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
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
//
// attempt is the runner's attempt number (ScopeReq.Attempt): 0 for a first run,
// 1+ for a retry. On a retry this scope's own consumption is reset before the
// re-stage, so the retried test replays its tape from the start. Nothing else is
// reset — see resetConsumedForScope.
func (a *Agent) BeginScope(ctx context.Context, name string, pid, attempt int) (models.ScopeAck, error) {
	if name == "" {
		return models.ScopeAck{Reason: models.ScopeReasonEmptyName, Attempt: attempt}, nil
	}
	if a.config != nil && a.config.Agent.Mode == models.MODE_TEST {
		a.scopeMu.Lock()
		names, ok := a.scopeTable[name]
		own := names
		tableSize := len(a.scopeTable)
		universe := a.mappedUniverse
		// SPEC-SCOPED SETUP. A test's mapping covers only what it issued inside
		// its OWN scope window. Everything its spec captured before the first
		// test opened (`beforeAll`: sign-in, app list, first render) and between
		// test windows (a test's own async traffic that outlived its scope) is
		// filed under `__suite__:<spec>` instead -- 811 mocks on the atg "order"
		// tape, 349 true beforeAll and 444 intra-spec leakage.
		//
		// Those are in the universe, so FilterTcsMocksMappingWithShared reads
		// them as "owned by some test -- hide", and they were unreachable by the
		// very tests that depend on them: 1,264 mock-instances a test requested
		// AT RECORD that it does not own, 952 of them (75%) sitting in its own
		// spec's suite entry. That is the single largest source of the 96 misses.
		//
		// Appending them to THIS test's mapping restores reach without widening:
		// a test sees its own mocks plus its own spec's setup, and nothing from
		// any other spec. Measured across five tapes (f41, f41b, order, bucket-e,
		// drawer-only), suite captures never land in another spec's window --
		// cross-spec leakage is 0 -- so the spec boundary is the correct fence.
		//
		// Ordering: own mocks FIRST. FilterTcsMocksMapping preserves slice order
		// within the mapped tier, so a test's own recording still wins every tie
		// against the shared setup, exactly as before.
		if ok {
			if suiteNames, sOK := a.scopeTable[suiteScopeOf(name)]; sOK && len(suiteNames) > 0 {
				merged := make([]string, 0, len(names)+len(suiteNames))
				merged = append(merged, names...)
				merged = append(merged, suiteNames...)
				names = merged
			}
		}
		a.scopeMu.Unlock()
		// SPEC-SHARED REQUESTS (specpool.go): requests whose every copy in the
		// spec carries the same response are reachable from every scope of the
		// spec and not consumed, so a poll, refetch or extra page load cannot
		// run them dry. Must run outside scopeMu: building the plan reads the
		// scope table under that lock.
		if ok {
			names = a.applySpecPool(name, names)
		}
		// No per-test mapping for this test — leave the whole pool armed. All
		// three shapes below served the whole set silently before; they are
		// distinguished here because a runner needs to know WHICH it hit: an
		// absent mappings.yaml is a setup error, an absent NAME is a renamed
		// or never-recorded test.
		//
		// A retry (attempt > 0) that lands here has NOTHING to reset: the reset
		// is defined as "this scope's mock names", and none of these three cases
		// yields any. The ack carries attempt with retry_reset false so the
		// runner sees the request was understood and declined, rather than
		// broadening the reset to mocks that are not this scope's.
		// Strict scoping: a test with no recording of its own is narrowed to
		// NOTHING rather than left looking at the whole pool.
		//
		// The default is deliberately lenient because integration testing wants
		// it: there, a mock is a third party keeping the service alive, and one
		// test's recording answering another's request is fine. For a browser
		// suite it is not -- the Playwright spec asserts on the response, and
		// test A's body is simply not a valid answer for test B. Serving it
		// produces a PASS that means nothing, which is the failure this whole
		// per-test scheme exists to prevent.
		//
		// Mocks that no test owns stay reachable: they belong to nobody, so
		// serving them steals from no one. Everything owned -- including a
		// `__suite__` recording -- becomes invisible, so the first real
		// dependency call misses and --on-miss fail turns it red.
		if a.scopeStrict && (tableSize == 0 || !ok || len(names) == 0) {
			// Two very different situations narrow to the same thing but must NOT
			// report the same thing.
			//
			//   mapped to zero mocks -> this test WAS recorded and needed nothing.
			//                           Expected, unremarkable, no alarm.
			//   absent from the table -> this test was NEVER recorded: new, renamed,
			//                           or its recording was lost. Someone must act.
			//
			// Collapsing them is what forced every earlier attempt at refusing an
			// unmapped test to be abandoned as a false positive on the call-free
			// ones. record now writes an explicit empty entry precisely so the two
			// can be told apart here.
			recorded := ok && len(names) == 0
			reason := models.ScopeReasonStrictNoRecording
			if recorded {
				reason = models.ScopeReasonEmptyMapping
				a.logger.Debug("scope begin: strict scoping, this test was recorded and needed no mocks",
					zap.String("test", name), zap.Int("attempt", attempt))
			} else {
				a.logger.Info("scope begin: strict scoping, this test has no recording; serving only unowned mocks",
					zap.String("test", name), zap.Int("attempt", attempt))
			}
			if pid > 0 {
				// NOT an empty slice: SetWorkerScope treats that as "clear this
				// worker's scope", which serves the WHOLE pool -- the exact
				// opposite of what strict scoping means, while still acking
				// Scoped with 0 mocks. Narrow to a name no mock can have instead.
				// Mock names are "<12-hex>-<n>" or "mock-<n>" (mockdb.mintName),
				// so this sentinel can never collide with a real recording.
				a.SetWorkerScope(uint32(pid), []string{strictNoRecordingSentinel})
			} else if err := a.UpdateMockParams(ctx, models.MockFilterParams{
				MockMapping:         []string{},
				MockMappingUniverse: universe,
				UseMappingBased:     true,
				AfterTime:           models.BaseTime,
				BeforeTime:          time.Now(),
				AgentOwnsConsumed:   true,
			}); err != nil {
				return models.ScopeAck{}, err
			}
			return models.ScopeAck{Scoped: true, Mocks: 0, Reason: reason, Attempt: attempt}, nil
		}

		switch {
		case tableSize == 0:
			a.logger.Debug("scope begin: NOT scoped, no per-test mapping table installed", zap.String("test", name), zap.Int("attempt", attempt))
			return models.ScopeAck{Reason: models.ScopeReasonNoMappingTable, Attempt: attempt}, nil
		case !ok:
			a.logger.Debug("scope begin: NOT scoped, this name is absent from the mapping table", zap.String("test", name), zap.Int("mapped_tests", tableSize), zap.Int("attempt", attempt))
			// Not scoped to its own recording (there is none), but still kept
			// inside its own spec: whatever was staged before belongs to the
			// previous test or the previous spec. A spec with no suite recording
			// (28 of 56 on the atg tape) lands here for its suite scope too.
			if pid == 0 {
				if err := a.stageSpecSetup(ctx, specOfScope(name), "unmapped-scope"); err != nil {
					a.logger.Debug("could not stage the spec setup pool for an unmapped scope; the previous pool stays", zap.String("scope", name), zap.Error(err))
				}
			}
			return models.ScopeAck{Reason: models.ScopeReasonUnmappedScope, Attempt: attempt}, nil
		case len(names) == 0:
			a.logger.Debug("scope begin: NOT scoped, this name is mapped to zero mocks", zap.String("test", name), zap.Int("attempt", attempt))
			return models.ScopeAck{Reason: models.ScopeReasonEmptyMapping, Attempt: attempt}, nil
		}
		if pid > 0 {
			// Parallel-safe: narrow ONLY this worker's served view, keyed by its
			// PID, so concurrent workers never overwrite one shared filter.
			//
			// A retry is NOT reset here. This path never re-stages the pool —
			// SetWorkerScope only narrows the view over a pool that consumption
			// mutates globally — so un-consuming the ledger would restore
			// nothing while reporting that it had. retry_reset stays false.
			if attempt > 0 {
				a.logger.Debug("scope begin: retry reset skipped, worker-scoped pools are not re-staged", zap.String("test", name), zap.Int("worker", pid), zap.Int("attempt", attempt))
			}
			a.logger.Debug("scope begin: worker-scoped pool", zap.String("test", name), zap.Int("worker", pid), zap.Int("mocks", len(names)))
			a.SetWorkerScope(uint32(pid), names)
			return models.ScopeAck{Scoped: true, Mocks: len(names), Reason: models.ScopeReasonWorkerScoped, Attempt: attempt}, nil
		}
		// No worker PID reported — restrict the single global pool (correct for
		// a sequential single-worker suite, the pre-Design-A behavior).
		ack := models.ScopeAck{Scoped: true, Mocks: len(names), Reason: models.ScopeReasonPoolRestricted, Attempt: attempt}
		if attempt > 0 {
			// BEFORE consumedSoFar, so the re-stage below sees the post-reset
			// ledger and re-arms exactly this test's mocks.
			ack.RestoredMocks, ack.RetryReset = a.resetConsumedForScope(ctx, name, names)
		}
		a.logger.Debug("scope begin: restricting served pool to test", zap.String("test", name), zap.Int("mocks", len(names)))
		// Pending-write fence (fencescope.go): only a TEST has a sequence of its
		// own writes to fence; a suite scope is setup shared by every describe.
		if strings.HasPrefix(name, "__suite__:") {
			a.setFenceSetup(specOfScope(name))
		} else {
			a.setFenceScope(name, own)
		}
		if err := a.UpdateMockParams(ctx, models.MockFilterParams{
			MockMapping: names,
			// Mocks belonging to no test stay reachable as overflow, after this
			// test's own. Without it a `beforeAll` recording is invisible for the
			// whole test and a call that needs it misses with candidates: 0,
			// while the per-worker path (SetWorkerScope above) already keeps such
			// mocks visible. The two paths agreed on nothing here until now.
			MockMappingUniverse: universe,
			UseMappingBased:     true,
			AfterTime:           models.BaseTime,
			BeforeTime:          time.Now(),
			// Subtract what this session already served. Upstream's persistent
			// ledger (#4534) exists but is not consulted unless the caller asks,
			// so without this every scope boundary re-stages a pristine pool and
			// resurrects the whole suite's consumption.
			AgentOwnsConsumed: true,
		}); err != nil {
			return models.ScopeAck{}, err
		}
		return ack, nil
	}

	// Record mode: mark the window start (agent clock), keyed by worker PID so
	// overlapping windows from parallel workers stay distinguishable. We do NOT
	// touch the syncMock ingress-correlation machinery here — that is driven by
	// incoming requests, which mock mode has none of.
	a.scopeMu.Lock()
	if a.workerOpen == nil {
		a.workerOpen = make(map[scopeKey]time.Time)
	}
	k := scopeKey{pid: uint32(pid), name: name}
	// A second begin for a scope that was never ended silently discards the
	// earlier window start, so every mock captured before this call is
	// attributed to no test and vanishes from mappings.yaml. The overwrite is
	// kept (changing it is a separate fix) but it is no longer silent.
	_, alreadyOpen := a.workerOpen[k]
	a.workerOpen[k] = time.Now()
	a.scopeMu.Unlock()
	a.logger.Debug("scope begin (record)", zap.String("test", name), zap.Int("worker", pid), zap.Bool("already_open", alreadyOpen), zap.Int("attempt", attempt))
	// attempt is echoed but changes nothing at record: recording has no served
	// pool to un-consume, and a retried test simply opens a second window under
	// the same name, which correlateScopes already handles.
	if alreadyOpen {
		return models.ScopeAck{Reason: models.ScopeReasonRecordAlreadyOpen, Attempt: attempt}, nil
	}
	return models.ScopeAck{Reason: models.ScopeReasonRecordWindowOpened, Attempt: attempt}, nil
}

// EndScope closes a per-test scope. In record mode it records the [begin, now]
// window (tagged with the worker PID) for later correlation. In test mode it
// stages the spec's setup pool (stageSpecSetup) so a call made between tests
// still matches, against its own spec's recording only.
func (a *Agent) EndScope(ctx context.Context, name string, pid int) error {
	if name == "" {
		return nil
	}
	if a.config != nil && a.config.Agent.Mode == models.MODE_TEST {
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
		// A suite ends only when the runner moves to another spec or tears the
		// worker down, and the next scope begin replaces the pool either way.
		// Leave the setup pool staged rather than open a window on the whole
		// tape in between.
		if strings.HasPrefix(name, "__suite__:") {
			a.logger.Info("KPGAP suite scope ended; setup pool left staged", zap.String("scope", name))
			return nil
		}
		a.noteTestEnded(name)
		return a.stageSpecSetup(ctx, specOfScope(name), "test-end")
	}

	a.scopeMu.Lock()
	k := scopeKey{pid: uint32(pid), name: name}
	start, ok := a.workerOpen[k]
	if ok {
		delete(a.workerOpen, k)
		a.scopeWindows = append(a.scopeWindows, models.ScopeWindow{Name: name, Start: start, End: time.Now(), PID: uint32(pid)})
	}
	a.scopeMu.Unlock()
	a.logger.Debug("scope end (record)", zap.String("test", name), zap.Int("worker", pid))
	return nil
}

// stageSpecSetup stages the pool a spec's code runs against OUTSIDE its tests:
// the spec's suite recording (beforeAll / afterAll and the traffic between
// tests) plus its spec-shared representatives, and the unowned overflow.
//
// WHY. A test's end used to restore the WHOLE tape (every spec, ~7,900 mocks on
// the atg tape) until the next scope began, and every beforeAll after a spec's
// first describe runs in exactly that window: the fixture opens the suite scope
// once per spec and does not reopen it per describe. Measured in one run, that
// window cost 16 tests: a spec-shared "zero apps" MyApps from apps/page (never
// consumed, by design) answered six later specs' beforeAll hooks, and gap
// requests consumed other specs' only copies of /auth/check and
// /cluster/proxy-origins, starving them before they ran. Each affected spec's
// own suite recording held the right answer for every one of those hooks.
// Reloading 7,900 mocks per test end also cost ~2s each.
//
// A spec with nothing staged here (no suite recording, no shared requests) is
// narrowed to the unowned overflow, never widened to other specs' recordings.
func (a *Agent) stageSpecSetup(ctx context.Context, spec, why string) error {
	suite := "__suite__:" + spec
	a.scopeMu.Lock()
	names := append([]string(nil), a.scopeTable[suite]...)
	universe := a.mappedUniverse
	a.scopeMu.Unlock()
	names = a.applySpecPool(suite, names)
	mapping := names
	if len(mapping) == 0 {
		mapping = []string{strictNoRecordingSentinel}
	}
	a.setFenceSetup(spec)
	a.logger.Info("KPGAP staging spec setup pool", zap.String("spec", spec), zap.String("why", why), zap.Int("names", len(names)))
	return a.UpdateMockParams(ctx, models.MockFilterParams{
		MockMapping:         mapping,
		MockMappingUniverse: universe,
		UseMappingBased:     true,
		AfterTime:           models.BaseTime,
		BeforeTime:          time.Now(),
		AgentOwnsConsumed:   true,
	})
}

// resolveOwner names the per-test scope that owns a capture made by worker `pid`
// at request time `ts`. This is the record-time half of the mock identity
// (owner, n): it runs while the scopes are still live, so a capture is stamped
// with its owner as it leaves the agent rather than being bucketed by timestamp
// after the fact.
//
// The rule is deliberately the SAME one the CLI's post-run correlateScopes uses
// (pkg/service/mock/record.go) so the two can be cross-checked against each
// other: the innermost -- latest-started -- window containing ts wins, and a
// window recorded by this exact worker is preferred over one from another
// worker, so overlapping parallel windows never steal each other's captures.
//
// The one thing it can see that correlateScopes cannot is a window that is
// still OPEN: a.workerOpen holds a start with no end yet, and is treated as
// extending to +infinity. That is the common case here -- the mock is usually
// emitted while its own test is still running.
//
// Returns "" when no window contains ts (a boot-time handshake before the first
// scope, a runner that declares no scopes at all, or a capture whose kind never
// stamps ReqTimestampMock). "" is the unowned namespace and keeps the legacy
// mock-N naming, so every un-scoped recording is unaffected.
func (a *Agent) resolveOwner(pid uint32, ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	a.scopeMu.Lock()
	defer a.scopeMu.Unlock()

	// sameWorkerOnly restricts the scan to windows this worker opened. Scope
	// names are non-empty (BeginScope rejects ""), so "" is a safe sentinel.
	scan := func(sameWorkerOnly bool) string {
		best := ""
		var bestStart time.Time
		for _, w := range a.scopeWindows {
			if sameWorkerOnly && w.PID != pid {
				continue
			}
			if ts.Before(w.Start) || ts.After(w.End) {
				continue
			}
			if best == "" || w.Start.After(bestStart) {
				best, bestStart = w.Name, w.Start
			}
		}
		for k, start := range a.workerOpen {
			if sameWorkerOnly && k.pid != pid {
				continue
			}
			if ts.Before(start) {
				continue
			}
			if best == "" || start.After(bestStart) {
				best, bestStart = k.name, start
			}
		}
		return best
	}

	var owner string
	if pid != 0 {
		// Same rule as below: a `__suite__` hit is not a decision, because that
		// window spans the whole spec and therefore contains every inter-test
		// gap. Keep it as the fallback and let the grace try first.
		if o := scan(true); o != "" && !strings.HasPrefix(o, "__suite__:") {
			return o
		} else if o != "" {
			owner = o
		}
	}
	if owner == "" {
		owner = scan(false)
	}
	// A match that is only the spec's own `__suite__` window is NOT a decision:
	// that window spans the whole spec, so it contains every gap between tests.
	// Fall through to the trailing grace first and let a test that just closed
	// claim its own late capture; the suite stays the fallback below.
	if owner != "" && !strings.HasPrefix(owner, "__suite__:") {
		return owner
	}

	return owner
}

// resetConsumedForScope un-consumes exactly the mocks that belong to `name`, so
// a retried test replays its own tape from the start. It returns how many
// entries the ledger actually dropped, and whether the reset ran at all.
//
// `names` is this scope's mappings.yaml entry, and that is what makes the reset
// SCOPE-PRECISE. The cumulative ledger is global, so clearing it wholesale would
// re-arm every mock the suite has consumed so far — the resurrection bug the
// ledger was introduced to fix. Passing only this scope's names means the retry
// gets its own tape back and no other test's.
//
// A proxy without the optional resetter reports false, so the acknowledgement
// says the reset did not happen rather than implying it did.
func (a *Agent) resetConsumedForScope(ctx context.Context, name string, names []string) (int, bool) {
	r, ok := a.Proxy.(coreAgent.ConsumedMockResetter)
	if !ok {
		a.logger.Debug("retry reset unavailable: this proxy cannot reset consumed mocks", zap.String("test", name))
		return 0, false
	}
	restored, err := r.ResetConsumedMocks(ctx, names)
	if err != nil {
		a.logger.Debug("failed to reset this scope's consumed mocks for a retry", zap.String("test", name), zap.Error(err))
		return 0, false
	}
	a.logger.Debug("scope begin: retry, re-arming this test's own mocks", zap.String("test", name), zap.Int("scope_mocks", len(names)), zap.Int("restored", restored))
	return restored, true
}

// GetScopeWindows returns the per-test windows collected this record session,
// consumed by the CLI to build mappings.yaml.
func (a *Agent) GetScopeWindows(_ context.Context) ([]models.ScopeWindow, error) {
	a.scopeMu.Lock()
	defer a.scopeMu.Unlock()
	out := make([]models.ScopeWindow, len(a.scopeWindows))
	copy(out, a.scopeWindows)
	return out, nil
}

// SetScopeTable installs the replay-time per-test name→mock-names table the CLI
// read from mappings.yaml.
// strictNoRecordingSentinel is a mock name that cannot exist. Under strict
// scoping a per-worker view is narrowed to this rather than to an empty list,
// because an empty list means "clear the scope" and would serve everything.
const strictNoRecordingSentinel = "\x00keploy-strict-no-recording"

func (a *Agent) SetScopeTable(_ context.Context, table map[string][]string, strict bool) error {
	a.scopeMu.Lock()
	a.scopeTable = table
	a.scopeStrict = strict
	a.scopeMu.Unlock()
	// A new table means a new recording: any per-spec sharing plan built
	// against the old one is stale.
	a.resetSpecPools()

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
	// Cached for BeginScope's pid == 0 branch, which needs the same set to tell
	// "another test's mock" from "belongs to no test" when it narrows the pool.
	a.scopeMu.Lock()
	a.mappedUniverse = universe
	a.scopeMu.Unlock()
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
	stats := models.MockStats{Loaded: loaded}
	// Consumed was never populated, so every reader saw a literal 0 while the
	// end-of-run summary reported the real figure. That silently disarmed the
	// fixture's strict-scope gate in BOTH directions: `after > before` is 0 > 0,
	// and MockStats has no omitempty so the "count unknowable" branch could not
	// fire either. GetPersistentConsumed is the cumulative, NON-draining ledger
	// -- reading it here leaves the summary's drain untouched.
	if reader, ok := a.Proxy.(coreAgent.ConsumedStateReader); ok {
		stats.Consumed = len(reader.GetPersistentConsumed())
	}
	return stats, nil
}

// suiteScopeOf maps a per-test scope name to its spec's suite scope name.
// The runner names tests "<spec>.spec.ts > describe > test" and the suite
// scope "__suite__:<spec>.spec.ts" (tests/_lib/keploy-fixture.ts
// suiteScopeName), so the spec file is the prefix up to the first " > ".
func suiteScopeOf(testName string) string {
	spec := testName
	if i := strings.Index(spec, " > "); i >= 0 {
		spec = spec[:i]
	}
	return "__suite__:" + spec
}
