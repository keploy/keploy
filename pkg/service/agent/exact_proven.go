package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// Ownership and exact consumption under the proven switch (design v14 §5.2,
// S12): the agent drives the proxy's exact serving (coreAgent.ExactServer),
// the one path that decides what a replay's parsers may answer with.
//
//   - A test owns exactly the test mocks its mapping lists: the data-plane
//     mocks among them. A test the mapping omits, or lists with none, owns
//     none, and any data-plane call it makes is a miss. A mapped
//     protocol-phase mock (an old mapping that listed a handshake) is not
//     owned: it stays in the pool, which serves it to every test.
//   - While a test is open, each of its test mocks answers at most one
//     request, in recorded order; one more identical request is a miss
//     (models.MissExtraCall). Each begin starts the test's consumption
//     afresh, so a retry replays from its first recorded call.
//   - While no test is open, the current key's pool slice answers, with
//     reuse or cursor-saturate as recorded. The stateful cursors start afresh
//     with the session and are each key's own.
//   - At its end, each test mock the test did not consume is evidence.
//
// The replay's mocks are staged on the proxy once per session, whole; exact
// serving narrows every read to what it shows, so nothing staged widens what
// a test is served.

// exactServer is the proxy's exact serving; an agent whose proxy has none
// cannot judge a test under the switch.
func (a *Agent) exactServer() (coreAgent.ExactServer, error) {
	es, ok := a.Proxy.(coreAgent.ExactServer)
	if !ok || a.Proxy == nil {
		return nil, errors.New("this keploy agent's proxy cannot serve each test exactly its own mocks")
	}
	return es, nil
}

// servePoolExactly serves the current claim's key's pool slice (before any
// claim, models.PoolKeyNone's) while no test is open. It stages the replay's
// mocks on the proxy the first time in a session, under exact serving with
// no pool, so no staged mock is ever served outside it, and no mock still
// staged from the previous session answers under one of this session's pool
// names (names are unique only within a recording). Until the CLI sent the
// pool slices, the slice is empty: no data-plane pool mock answers.
func (a *Agent) servePoolExactly(ctx context.Context) error {
	es, err := a.exactServer()
	if err != nil {
		return err
	}
	a.provenMu.Lock()
	key, slice, staged := a.proven.key, a.proven.pool, a.proven.staged
	a.provenMu.Unlock()
	if key == "" {
		key = models.PoolKeyNone
	}
	if !staged {
		// No worker-scope mock answers before the replay's stage lands: a
		// name is unique only within a set, and one still staged from the
		// previous session would answer under it.
		if ws, ok := es.(coreAgent.ExactWorkerServer); ok {
			if err := ws.ServeExactWorker(nil); err != nil {
				return err
			}
		}
		if err := es.ServeExactPool(key, nil); err != nil {
			return err
		}
		staged, err = a.stageForExactServing(ctx)
		if err != nil {
			return err
		}
		if staged {
			a.provenMu.Lock()
			a.proven.staged = true
			a.provenMu.Unlock()
		}
	}
	if staged {
		if err := a.serveWorkerExactly(es); err != nil {
			return err
		}
	}
	return es.ServeExactPool(key, append([]string(nil), slice[key]...))
}

// stageForExactServing stages every mock of the replay on the proxy, once:
// exact serving decides what each read shows. A new session's stateful
// cursors start afresh (two rounds never share one). It reports whether it
// staged: with no recording stored yet there is nothing to stage.
func (a *Agent) stageForExactServing(ctx context.Context) (bool, error) {
	if _, stored := a.clientMocks.Load(uint64(0)); !stored {
		// Nothing to stage yet: the CLI stores the recording before it
		// sends the table, and the next serve stages it.
		return false, nil
	}
	if r, ok := a.Proxy.(coreAgent.StatefulCursorResetter); ok {
		r.ResetStatefulCursors()
	}
	if err := a.UpdateMockParams(ctx, models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: time.Now()}); err != nil {
		return false, fmt.Errorf("stage the recording's mocks: %w", err)
	}
	return true, nil
}

// restartKey starts key's calls outside every test afresh on the proxy
// (coreAgent.ExactKeyRestarter), when the proxy can: one that cannot keeps
// what they consumed.
func (a *Agent) restartKey(key string) error {
	r, ok := a.Proxy.(coreAgent.ExactKeyRestarter)
	if !ok {
		return nil
	}
	a.provenMu.Lock()
	names := append([]string(nil), a.proven.pool[key]...)
	a.provenMu.Unlock()
	return r.RestartExactKey(key, names)
}

// beginClaimVariables starts the claim key's variables, those its calls made
// outside every test bind, afresh on the proxy (coreAgent.ClaimVariables),
// and the learn step's record of those calls, under the proven switch.
func (a *Agent) beginClaimVariables(key string) {
	if !provenswitch.On() {
		return
	}
	if cv, ok := a.Proxy.(coreAgent.ClaimVariables); ok {
		cv.BeginClaimVariables(key)
	}
}

// serveTestExactly opens test id under exact serving: its test mocks, the
// data-plane mocks of its mapping, in recorded order, and its variables, with
// none bound, in the same step (openExactTest). A mapping that names a mock
// the loaded recording does not have refuses: what the test owns cannot be
// told.
func (a *Agent) serveTestExactly(ctx context.Context, id string) error {
	es, err := a.exactServer()
	if err != nil {
		return err
	}
	a.provenMu.Lock()
	staged := a.proven.staged
	a.provenMu.Unlock()
	if !staged {
		// A begin before the CLI sent its table: nothing is staged, and the
		// pool serves nothing; the test owns what its mapping (none yet)
		// lists.
		if err := a.servePoolExactly(ctx); err != nil {
			return err
		}
	}
	a.scopeMu.Lock()
	names := append([]string(nil), a.scopeTable[id]...)
	a.scopeMu.Unlock()
	var owned []*models.Mock
	if len(names) > 0 {
		mocks, err := a.mocksByName(names)
		if err != nil {
			return err
		}
		owned = a.notReal(id, testMocks(names, mocks))
	}
	// What the store refused of the test's mapping serves nothing; its end
	// counts each as a recorded call the test did not make.
	unservable := a.refusedOf(names)
	a.provenMu.Lock()
	a.proven.unservable = unservable
	a.provenMu.Unlock()
	// The worker scope's mocks placed after this test's begin are reachable
	// from its view on.
	a.scopeMu.Lock()
	if a.testBegun == nil {
		a.testBegun = map[string]bool{}
	}
	a.testBegun[id] = true
	a.scopeMu.Unlock()
	if err := a.serveWorkerExactly(es); err != nil {
		return err
	}
	// A test is the scope of the variables keploy mock replay binds: it
	// begins with none bound, as it opens.
	if err := a.openExactTest(es, id, owned, nil); err != nil {
		return err
	}
	a.provenMu.Lock()
	a.proven.servedSince = time.Now()
	a.provenMu.Unlock()
	return nil
}

// openExactTest opens test under exact serving, served exactly owned, and
// begins its variables with given bound, in one step (coreAgent.
// ExactTestOpener): no call the test's mocks answer binds before its
// variables begin. A proxy that cannot opens it only when it keeps no
// variables, and none were given (opensWithoutVariables).
func (a *Agent) openExactTest(es coreAgent.ExactServer, test string, owned []*models.Mock, given []models.VariableBinding) error {
	if o, ok := es.(coreAgent.ExactTestOpener); ok {
		return o.OpenExactTest(test, owned, given)
	}
	if err := a.opensWithoutVariables(given); err != nil {
		return err
	}
	return es.ServeExactTest(test, owned)
}

// testMocks is the test mocks among a test's mapped names: the data-plane
// mocks, each once, in recorded order (by request time; a mapping lists a
// test's mocks in recorded order, which keeps ties, and an undated mock
// before the dated ones).
func testMocks(names []string, byName map[string]*models.Mock) []*models.Mock {
	out := make([]*models.Mock, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		mk := byName[n]
		if mk == nil || seen[n] || mk.IsProtocolPhase() {
			continue
		}
		seen[n] = true
		out = append(out, mk)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ti, tj := out[i].Spec.ReqTimestampMock, out[j].Spec.ReqTimestampMock
		if ti.IsZero() || tj.IsZero() {
			return ti.IsZero() && !tj.IsZero()
		}
		return ti.Before(tj)
	})
	return out
}

// serveKeployTestExactly is a keploy test filter under the proven switch
// (models.ExactServing): the set's staging call stages every mock once and
// serves the set's pool; a test case's call serves it exactly its own mocks,
// and with open begins its variables as it opens (OpenTestCase); any other
// call serves the pool again, between test cases. A keploy test run has one
// key, models.PoolKeyNone.
func (a *Agent) serveKeployTestExactly(ctx context.Context, params models.MockFilterParams, open *opening) error {
	es, err := a.exactServer()
	if err != nil {
		return err
	}
	ex := params.Exact
	// A test case opens, or ends, once the calls made before are settled
	// (settle_proven.go): they are the pool's, or the test case's own. A
	// connection whose held requests cannot be told does not stop either:
	// a test case opens all the same, and one that ends is served the pool
	// and told it is not verified (its consumption read fails).
	var unknown error
	switch {
	case ex.Stage:
	case ex.Test != "":
		// A test case opens all the same with a connection whose held
		// requests cannot be told: its end finds it. The listener's drops
		// read now are what its end compares.
		drops, err := a.settle(ctx, true)
		if err != nil && !errors.Is(err, errHoldsUnknown) {
			return err
		}
		a.markDrops(drops)
	default:
		// A test case that ends is served the pool whatever its settling
		// found, and told it is not verified when keploy cannot tell its
		// calls settled: the calls after it are the pool's either way.
		unknown = a.settledSince(ctx, true, "the test case")
	}
	switch {
	case ex.Stage:
		staging := params
		staging.Exact = nil
		// Exact serving goes on, with no pool, before the stage lands: no
		// staged mock is ever served outside it, and none still staged from
		// the previous set answers under one of this set's pool names
		// (names are unique only within a set). The set's pool is served
		// once its mocks are in. The previous set's worker scope goes for
		// the same reason, before that view is installed.
		a.scopeMu.Lock()
		a.testWorker, a.testBegun = nil, nil
		a.scopeMu.Unlock()
		if err := a.serveWorkerExactly(es); err != nil {
			return err
		}
		if err := es.ServeExactPool(models.PoolKeyNone, nil); err != nil {
			return err
		}
		if r, ok := a.Proxy.(coreAgent.StatefulCursorResetter); ok {
			r.ResetStatefulCursors()
		}
		// The pool's per-test mocks are staged with the set's startup
		// traffic: the staging call reads, of the on-disk store, only what
		// was recorded before the set's first test (stagingStartupCutoff),
		// and each test reads its own as it opens (ServeExactTest). A call
		// recorded while no test was open, between tests or after the last
		// one, is the pool's, served by name from the staged mocks; were it
		// not staged, its replay would find no mock. The pool is what no
		// test owns, never the whole recording.
		//
		// The set's worker scope (models.Mapping.Worker) is staged by name
		// with the pool: its mocks answer from their places on.
		if err := a.stageMocks(ctx, staging, append(append([]string(nil), ex.Pool...), entryNames(ex.Worker)...), nil); err != nil {
			return fmt.Errorf("stage the test set's mocks: %w", err)
		}
		// A miss on a call that is the recording of a test not begun yet
		// (a worker's call made early) is told from the on-disk store: a
		// test's own are staged only as it opens.
		if rs, ok := a.Proxy.(coreAgent.RecordingSourceSetter); ok {
			if err := rs.SetRecordingSource(a.diskRecordingSource()); err != nil {
				return fmt.Errorf("set the test set's recording source: %w", err)
			}
		}
		// The learn step's earlier places of a pool recording are every
		// recording made before it, a test's own on disk too.
		if rb, ok := a.Proxy.(coreAgent.RecordedBeforeSetter); ok {
			if err := rb.SetRecordedBefore(a.diskRecordedBefore()); err != nil {
				return fmt.Errorf("set the test set's recordings before an instant: %w", err)
			}
		}
		a.scopeMu.Lock()
		a.testPool = append([]string(nil), ex.Pool...)
		a.testWorker = append([]models.MockEntry(nil), ex.Worker...)
		a.testBegun = map[string]bool{}
		a.scopeMu.Unlock()
		if err := a.serveWorkerExactly(es); err != nil {
			return err
		}
		return es.ServeExactPool(models.PoolKeyNone, append([]string(nil), ex.Pool...))
	case ex.Test != "":
		var owned []*models.Mock
		if len(ex.Owned) > 0 {
			mocks, err := a.mocksByName(ex.Owned)
			if err != nil {
				return fmt.Errorf("serve %s its own mocks: %w", ex.Test, err)
			}
			owned = testMocks(ex.Owned, mocks)
		}
		a.scopeMu.Lock()
		if a.testBegun == nil {
			a.testBegun = map[string]bool{}
		}
		a.testBegun[ex.Test] = true
		a.scopeMu.Unlock()
		if err := a.serveWorkerExactly(es); err != nil {
			return err
		}
		if len(ex.Concurrent) > 0 {
			return a.serveTestConcurrently(es, ex, owned, open)
		}
		if open != nil {
			return a.openExactTest(es, ex.Test, owned, open.given)
		}
		return es.ServeExactTest(ex.Test, owned)
	default:
		a.scopeMu.Lock()
		pool := append([]string(nil), a.testPool...)
		a.scopeMu.Unlock()
		if err := a.serveWorkerExactly(es); err != nil {
			return err
		}
		if err := a.servePoolAhead(es, pool, ex.Ahead); err != nil {
			return err
		}
		return unknown
	}
}

// serveTestConcurrently opens ex.Test with owned and the recordings it shares
// with the tests that ran beside it at record time (ex.Concurrent, keploy
// record's full round), when the proxy serves them
// (coreAgent.ExactConcurrentServer), and with open begins its variables in
// the same step (OpenExactTestConcurrent), as openExactTest does. The CLI
// sends them only to an agent that said it does (ServesExactConcurrent); a
// proxy that does not is refused, so a test is never served less than its
// plan says without a word.
func (a *Agent) serveTestConcurrently(es coreAgent.ExactServer, ex *models.ExactServing, owned []*models.Mock, open *opening) error {
	cs, ok := es.(coreAgent.ExactConcurrentServer)
	if !ok {
		return fmt.Errorf("this keploy agent's proxy cannot serve %s the recordings it shares with the tests that ran beside it", ex.Test)
	}
	mocks, err := a.mocksByName(ex.Concurrent)
	if err != nil {
		return fmt.Errorf("serve %s the recordings it shares: %w", ex.Test, err)
	}
	cands := testMocks(ex.Concurrent, mocks)
	if open != nil {
		return cs.OpenExactTestConcurrent(ex.Test, owned, cands, open.given)
	}
	return cs.ServeExactTestConcurrent(ex.Test, owned, cands)
}

// ServesExactConcurrent reports whether this agent serves a test the
// recordings it shares with the tests that ran beside it at record time
// (coreAgent.ExactConcurrentServer): what /storemocks says under the proven
// switch (models.ExactConcurrentHeader).
func (a *Agent) ServesExactConcurrent() bool {
	_, ok := a.Proxy.(coreAgent.ExactConcurrentServer)
	return ok && a.Proxy != nil
}

// servePoolAhead serves the set's pool between test cases and, beside it, the
// next test case's mocks that were in flight when it began (ahead,
// models.ExactServing.Ahead), each answering once as that test case's, when
// the proxy can (coreAgent.ExactAheadServer). A proxy that cannot serves the
// pool alone, as before: those calls answer only once their test case opens.
func (a *Agent) servePoolAhead(es coreAgent.ExactServer, pool, ahead []string) error {
	as, ok := es.(coreAgent.ExactAheadServer)
	if len(ahead) == 0 || !ok {
		return es.ServeExactPool(models.PoolKeyNone, pool)
	}
	mocks, err := a.mocksByName(ahead)
	if err != nil {
		// The pool is served all the same: the test case that ended is
		// over, and its consumption is read next. The next test case's
		// calls in flight answer once it opens.
		utils.LogError(a.logger, err, "could not read the next test case's calls in flight when it begins; they answer only once it opens")
		return es.ServeExactPool(models.PoolKeyNone, pool)
	}
	return as.ServeExactPoolAhead(models.PoolKeyNone, pool, testMocks(ahead, mocks))
}

// diskRecordingSource visits, of the set staged, the on-disk mocks of a kind
// under request keys (DiskMocks.RangeKeys); nil when the set's mocks are all
// resident, and so all staged in the proxy's trees.
func (a *Agent) diskRecordingSource() func(kind models.Kind, keys []string, visit func(name string, load func() (*models.Mock, error)) bool) error {
	v, ok := a.clientMocks.Load(uint64(0))
	s, _ := v.(*ClientMockStorage)
	if !ok || s == nil {
		return nil
	}
	s.mu.RLock()
	disk := s.diskMocks
	s.mu.RUnlock()
	if disk == nil {
		return nil
	}
	return disk.RangeKeys
}

// diskRecordedBefore is the on-disk store's reader of the staged set's
// recordings made before an instant (DiskMocks.RangeRecordedBefore), nil
// when the set keeps none on disk.
func (a *Agent) diskRecordedBefore() func(t time.Time, visit func(*models.Mock) bool) error {
	v, ok := a.clientMocks.Load(uint64(0))
	s, _ := v.(*ClientMockStorage)
	if !ok || s == nil {
		return nil
	}
	s.mu.RLock()
	disk := s.diskMocks
	s.mu.RUnlock()
	if disk == nil {
		return nil
	}
	return disk.RangeRecordedBefore
}

// serveWorkerExactly tells the proxy which of the set's worker-scope mocks
// (testWorker) the set's run has reached the place of: those placed at the
// set's start, and those placed after a test case that has begun since the
// staging (testBegun). It is said before each view is installed, so the view
// shows them, each of a timer's only within its stretch of writes
// (coreAgent.ExactStretchServer). A proxy that has no worker scope
// (coreAgent.ExactWorkerServer) cannot serve a set that has one, nor one
// without stretches a set whose worker scope has them.
func (a *Agent) serveWorkerExactly(es coreAgent.ExactServer) error {
	a.scopeMu.Lock()
	var reachable []string
	stretches := map[string]models.Stretch{}
	for _, e := range a.testWorker {
		if e.Place == "" || a.testBegun[e.Place] {
			reachable = append(reachable, e.Name)
		}
		if e.Stretch != nil {
			stretches[e.Name] = *e.Stretch
		}
	}
	// A test's own read that waits for a write (SetScopeBounds): the proxy
	// shows it, among its test's, only once the run has passed the write.
	for _, e := range a.testBounds {
		if e.Stretch != nil {
			stretches[e.Name] = models.Stretch{After: e.Stretch.After}
		}
	}
	// The run is past every write recorded before the latest recorded begin
	// of the tests begun since the staging: a write of a test that did not
	// run (or that the run made elsewhere) holds no stretch shut or open.
	var reached time.Time
	for id := range a.testBegun {
		if at := a.testBegins[id]; at.After(reached) {
			reached = at
		}
	}
	var passed []string
	if !reached.IsZero() {
		for write, at := range a.testEdgeAt {
			if at.Before(reached) {
				passed = append(passed, write)
			}
		}
	}
	has := len(a.testWorker) > 0
	bounds := len(a.testBounds) > 0
	a.scopeMu.Unlock()
	ws, ok := es.(coreAgent.ExactWorkerServer)
	if !ok {
		if bounds {
			return errors.New("this keploy agent's proxy cannot hold a test's reads back until a write")
		}
		if has {
			return errors.New("this keploy agent's proxy cannot serve a test set's worker scope, the calls no test case owns")
		}
		return nil
	}
	if ss, ok := es.(coreAgent.ExactStretchServer); ok {
		if err := ss.ServeExactStretches(stretches, passed); err != nil {
			return err
		}
	} else if len(stretches) > 0 {
		return errors.New("this keploy agent's proxy cannot serve a timer's recordings within their stretch of writes")
	}
	return ws.ServeExactWorker(reachable)
}

// SetScopeWorker installs the set's worker scope for keploy mock replay
// (models.ScopeTableReq.Worker): no test owns its mocks, and each of a
// timer's answers only within its stretch. It reads when each write ending a
// stretch was recorded, from the replay's stored mocks, so the run can tell
// when it has gone past one (serveWorkerExactly); a write it does not hold
// ends its stretches only by being answered. It is served with the pool that
// SetScopePool serves next, once the replay's stage lands.
func (a *Agent) SetScopeWorker(ctx context.Context, worker []models.MockEntry) error {
	if err := a.wire.lock(ctx); err != nil {
		return err
	}
	defer a.wire.unlock()
	var edges []string
	seen := map[string]bool{}
	a.scopeMu.Lock()
	bounds := a.testBounds
	a.scopeMu.Unlock()
	for _, e := range append(append([]models.MockEntry(nil), worker...), bounds...) {
		if e.Stretch == nil {
			continue
		}
		for _, w := range []string{e.Stretch.After, e.Stretch.Until} {
			if w != "" && !seen[w] {
				seen[w] = true
				edges = append(edges, w)
			}
		}
	}
	edgeAt := map[string]time.Time{}
	if len(edges) > 0 {
		mocks, err := a.mocksByName(edges)
		if err != nil {
			return fmt.Errorf("read when the writes ending a timer's stretches were recorded: %w", err)
		}
		for name, mk := range mocks {
			if mk != nil && !mk.Spec.ReqTimestampMock.IsZero() {
				edgeAt[name] = mk.Spec.ReqTimestampMock
			}
		}
	}
	a.scopeMu.Lock()
	a.testWorker = append([]models.MockEntry(nil), worker...)
	a.testBegun = map[string]bool{}
	a.testEdgeAt = edgeAt
	a.scopeMu.Unlock()
	return nil
}

// SetScopeBounds installs the tests' own reads that answer only once the run
// has passed a write (models.ScopeTableReq.Bounds): each is shown among its
// test's mocks only after its Stretch.After write has answered or the run has
// gone past it (serveWorkerExactly says them to the proxy with the worker
// scope's stretches; SetScopeWorker, which comes next, reads when the writes
// were recorded). A table without them leaves none of the previous set's.
func (a *Agent) SetScopeBounds(ctx context.Context, bounds []models.MockEntry) error {
	if err := a.wire.lock(ctx); err != nil {
		return err
	}
	defer a.wire.unlock()
	a.scopeMu.Lock()
	a.testBounds = append([]models.MockEntry(nil), bounds...)
	a.scopeMu.Unlock()
	return nil
}

// entryNames is the names of entries, in order.
func entryNames(entries []models.MockEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Name != "" {
			out = append(out, e.Name)
		}
	}
	return out
}

// realDestinations is implemented by a proxy that relays the run's mock.real
// destinations: which one, if any, a recorded mock's destination is.
type realDestinations interface {
	RealDestination(m *models.Mock) (string, bool)
}

// notReal is owned less the mocks whose destination mock.real now reaches
// for real: a call to one is relayed before any matcher reads it, so the
// test is neither served the mock nor judged for not making its call (its
// REAL row says what it did there). Each is said once per session, with the
// fix.
func (a *Agent) notReal(id string, owned []*models.Mock) []*models.Mock {
	rd, ok := a.Proxy.(realDestinations)
	if !ok {
		return owned
	}
	out := owned[:0:0]
	for _, m := range owned {
		label, real := rd.RealDestination(m)
		if !real {
			out = append(out, m)
			continue
		}
		a.provenMu.Lock()
		said := a.proven.realSaid[m.Name]
		if !said {
			if a.proven.realSaid == nil {
				a.proven.realSaid = map[string]bool{}
			}
			a.proven.realSaid[m.Name] = true
		}
		a.provenMu.Unlock()
		if !said {
			a.logger.Warn("a test's recorded call goes to a destination mock.real now reaches for real: it is neither served nor judged",
				zap.String("test", id), zap.String("mock", m.Name), zap.String("real", label),
				zap.String("next_step", "record the test again, so its recording holds no call to "+label))
		}
	}
	return out
}
