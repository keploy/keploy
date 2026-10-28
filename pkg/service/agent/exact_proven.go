package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// Ownership and exact consumption under the proven switch (design v14
// §5.2): the agent drives the proxy's exact serving (coreAgent.ExactServer),
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
	// The set's learn step ends (a retry cycle of record's full round,
	// models.MockFilterParams.LearnEnded) before anything is settled or
	// served: a request a parser's learn step takes up after this is no
	// longer paired, nor learned (one it took up before keeps its decision).
	// Once ended it stays ended for the set: only the next set's staging
	// takes it back up (as a new session does, Proxy.Mock), so a later
	// filter that does not say it, from an older or another client, never
	// turns learning back on mid-set. A proxy that cannot end it refuses
	// the filter that asks to: its learn step would pair and learn in an
	// order that is not the recorded one.
	if le, ok := a.Proxy.(coreAgent.LearnEndSetter); ok {
		if params.LearnEnded || ex.Stage {
			if err := le.SetLearnEnded(params.LearnEnded); err != nil {
				return err
			}
		}
	} else if params.LearnEnded {
		return errors.New("this keploy agent cannot stop its learn step for the test set's retry cycle")
	}
	// What the set's first cycle learned is on every recording the agent
	// serves the set from here on, its retry cycles (models.MockFilterParams.
	// Learned): only with the learn step ended, which would otherwise read
	// a learned recording as the one recorded.
	if len(params.Learned) > 0 {
		if !params.LearnEnded {
			return errors.New("the test set's learning came on a filter that does not end its learn step: keploy sends it only with its retry cycles' learn step ended; run the keploy agent of this keploy version")
		}
		if err := a.loadKeptLearning(params.Learned); err != nil {
			return fmt.Errorf("serve the test set's retry cycles what its first cycle learned: %w", err)
		}
	}
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
		// Its end waits only for the requests opened from here.
		a.beginWindow()
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
		a.testBegunAt, a.testBg = nil, nil
		a.scopeMu.Unlock()
		// The previous set's background rules go too, and the set's own
		// rules come first: every view this staging installs reads the set
		// as served by the rules it was recorded under (a marked set's
		// background names no recording until its mocks are staged), so a
		// call one of them refuses is never told the set was recorded
		// without proven routing.
		if err := a.stageBackgroundExactly(es, a.stagingRules(ex)); err != nil {
			return err
		}
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
		// Record's full round pairs with the recordings of tests not begun
		// yet too, which the on-disk store keeps.
		if rk, ok := a.Proxy.(coreAgent.RecordedOfKindSetter); ok {
			if err := rk.SetRecordedOfKind(a.diskRecordedOfKind()); err != nil {
				return fmt.Errorf("set the test set's recordings of a kind: %w", err)
			}
		}
		bg := a.settleBackground(ex)
		if _, can := es.(coreAgent.ExactBackgroundServer); bg != nil && !can {
			// A proxy that cannot serve the background rules serves the set as
			// one recorded without proven routing, and the agent says so
			// (ServedBackground): the replay checks it as such a set.
			a.logger.Info("this keploy agent's proxy cannot serve a test set by the proven routing it was recorded under: its background is served as in a set recorded without proven routing, its worker scope checked at the set's end",
				zap.String("next_step", "nothing to do for this run: it is checked as a set recorded without proven routing, and its verdicts hold. This agent's proxy (a mode or a build that predates proven routing) serves no set by proven routing; replay with one that does to have the set served by it"))
			bg = nil
		}
		a.scopeMu.Lock()
		a.testPool = append([]string(nil), ex.Pool...)
		a.testWorker = append([]models.MockEntry(nil), ex.Worker...)
		a.testBegun = map[string]bool{}
		a.testBegunAt = map[string]int{}
		a.testBg = bg
		a.scopeMu.Unlock()
		// the background rules with the set's pool and whole worker scope; today's
		// with neither (they name nothing). Either settles the staging the
		// proxy opened (stageBackgroundExactly): the views from here on are
		// no staging's.
		if bg != nil {
			if err := a.serveBackgroundExactly(es, models.RecordedWithS12Routing, ex.Pool, entryNames(ex.Worker)); err != nil {
				return err
			}
		} else if err := a.serveBackgroundExactly(es, "", nil, nil); err != nil {
			return err
		}
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
		if a.testBegunAt == nil {
			a.testBegunAt = map[string]int{}
		}
		if _, again := a.testBegunAt[ex.Test]; !again {
			a.testBegunAt[ex.Test] = len(a.testBegunAt) + 1
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

// loadKeptLearning puts what the replay keeps of the set's first-cycle
// learning (states, models.MockFilterParams.Learned) on every recording the
// agent's store hands out from now on: each resident one it holds learning
// for is replaced by a merged copy (learn.KeptLearning.Onto), and each read
// from the on-disk store is merged as it is decoded (DiskMocks.SetOnLoad).
// A test's own recordings, read by name as it opens (mocksByName), are so
// served as the next keploy test reads them once the learning is persisted.
// The recordings the proxy staged at the set's start (the pool's, the worker
// scope's) carry it as the proxy hands them out (KeptLearningSetter). A
// proxy that cannot is refused before anything is loaded: the retry cycles
// would be served the staged recordings without what was learned on them.
// The next set's store (StoreMocks) comes without it, and so does the
// proxy's next staging (SetLearnEnded(false)).
func (a *Agent) loadKeptLearning(states []models.MockState) error {
	kept := learn.NewKeptLearning(states)
	if kept == nil {
		return nil
	}
	ks, ok := a.Proxy.(coreAgent.KeptLearningSetter)
	if !ok {
		return errors.New("this keploy agent's proxy cannot serve the staged recordings (the pool's, the worker scope's) what the first cycle learned: it has no kept-learning capability; run the keploy agent of this keploy version")
	}
	v, ok := a.clientMocks.Load(uint64(0))
	s, _ := v.(*ClientMockStorage)
	if !ok || s == nil {
		return errors.New("no mocks are loaded")
	}
	if err := ks.SetKeptLearning(kept); err != nil {
		return err
	}
	s.mu.Lock()
	for _, tier := range [][]*models.Mock{s.filtered, s.unfiltered} {
		for i, m := range tier {
			tier[i] = kept.Onto(m)
		}
	}
	disk := s.diskMocks
	s.mu.Unlock()
	if disk != nil {
		disk.SetOnLoad(kept.Into)
	}
	return nil
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
	disk := a.stagedDisk()
	if disk == nil {
		return nil
	}
	return disk.RangeKeys
}

// stagedDisk is the on-disk store of the staged set's per-test mocks, nil
// when it keeps none on disk.
func (a *Agent) stagedDisk() *proxyPkg.DiskMocks {
	v, ok := a.clientMocks.Load(uint64(0))
	s, _ := v.(*ClientMockStorage)
	if !ok || s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.diskMocks
}

// diskRecordedBefore is the on-disk store's reader of the staged set's
// recordings made before an instant (DiskMocks.RangeRecordedBefore), nil
// when the set keeps none on disk.
func (a *Agent) diskRecordedBefore() func(t time.Time, visit func(*models.Mock) bool) error {
	disk := a.stagedDisk()
	if disk == nil {
		return nil
	}
	return disk.RangeRecordedBefore
}

// serveWorkerExactly tells the proxy which of the set's worker-scope mocks
// (testWorker) the set's run has reached the place of: those placed at the
// set's start, and those placed after a test case that has begun since the
// staging (testBegun). It is said before each view is installed, so the view
// shows them. A proxy that has no worker scope (coreAgent.ExactWorkerServer)
// cannot serve a set that has one.
//
// Under the background rules (testBg) a worker-scope mock
// with no identical sibling among the set's background answers from the set's
// start, wherever it is placed (testBackground.noSibling), and the reachable
// ones are said in the order the view serves them in: the earlier place first
// (the set's start, then each test case's in the order they began), within
// one place a delivering answer before an idle one, then recorded order.
// Identical siblings stay gated by their places.
func (a *Agent) serveWorkerExactly(es coreAgent.ExactServer) error {
	a.scopeMu.Lock()
	reachable := reachableWorker(a.testWorker, a.testBegun, a.testBegunAt, a.testBg)
	has := len(a.testWorker) > 0
	a.scopeMu.Unlock()
	ws, ok := es.(coreAgent.ExactWorkerServer)
	if !ok {
		if has {
			return errors.New("this keploy agent's proxy cannot serve a test set's worker scope, the calls no test case owns")
		}
		return nil
	}
	return ws.ServeExactWorker(reachable)
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

// ServedBackground is the rules the staged set's background is served by
// (models.BackgroundRulesHeader): models.RecordedWithS12Routing when its
// staging settled the background rules, "" for today's.
func (a *Agent) ServedBackground() string {
	a.scopeMu.Lock()
	defer a.scopeMu.Unlock()
	if a.testBg == nil {
		return ""
	}
	return models.RecordedWithS12Routing
}

// testBackground is a set's background as its staging settled it under the
// background rules (models.RecordedWithS12Routing): which of its worker-scope mocks
// have no identical sibling among the set's background (its worker scope and
// its pool) by their kind's exact key (models.ExactKeyOf), and which answer
// idle (models.IsIdleAnswer).
type testBackground struct {
	noSibling map[string]bool
	idle      map[string]bool
	// keyErrs say, for each background recording two or more of its kind's
	// exact keys claim (models.ExactKeyErr), why it has no key: it counts as
	// having siblings. In recorded order of the names read.
	keyErrs []string
}

// settleBackground settles the staged set's background under the background rules, for
// a staging that names them (models.ExactServing.Background) under the proven
// switch; nil for today's rules. A mock it cannot read counts as having a
// sibling: it stays gated by its place.
func (a *Agent) settleBackground(ex *models.ExactServing) *testBackground {
	if a.stagingRules(ex) == "" {
		return nil
	}
	if len(ex.Worker) == 0 {
		return &testBackground{noSibling: map[string]bool{}, idle: map[string]bool{}}
	}
	names := append(append([]string(nil), ex.Pool...), entryNames(ex.Worker)...)
	byName, err := a.mocksByName(names)
	if err != nil {
		a.logger.Warn("could not read the test set's background to tell which worker-scope recordings have no identical sibling: each answers only from its place, and a call made before it misses",
			zap.Error(err),
			zap.String("next_step", "fix the error above, or record the test set again if its mocks cannot be read"))
		byName = nil
	}
	bg := backgroundOf(ex.Worker, ex.Pool, byName)
	for _, why := range bg.keyErrs {
		a.logger.Warn("a recording of the test set's background has no exact key: it answers only from its place, and a call made before it misses",
			zap.String("why", why),
			zap.String("next_step", "make each parser's exact key decline (ok false) the recordings it does not own"))
	}
	return bg
}

// backgroundOf is the background of a set whose worker scope is worker and
// whose pool is pool, its recordings by name in byName (testBackground): a
// worker-scope recording whose kind keys it (models.ExactSiblingKey: a key
// as the registration that gave it, so two parsers' equal strings are two
// keys) and that no other data-plane recording of the background shares the
// key of has no
// sibling; one byName lacks, or its kind cannot key, or two of its kind's
// keys claim (models.ExactKeyErr, in keyErrs), has (it stays gated).
func backgroundOf(worker []models.MockEntry, pool []string, byName map[string]*models.Mock) *testBackground {
	bg := &testBackground{noSibling: map[string]bool{}, idle: map[string]bool{}}
	names := append(append([]string(nil), pool...), entryNames(worker)...)
	// How many of the background's data-plane recordings share each exact
	// key, by kind.
	keys := map[string]int{}
	keyOf := func(mk *models.Mock) (string, bool) {
		if mk == nil || mk.IsProtocolPhase() {
			return "", false
		}
		k, ok := models.ExactSiblingKey(mk)
		if !ok {
			return "", false
		}
		return string(mk.Kind) + "\x00" + k, true
	}
	errSeen := map[string]bool{}
	for _, n := range names {
		mk := byName[n]
		if mk == nil || mk.IsProtocolPhase() || errSeen[n] {
			continue
		}
		errSeen[n] = true
		if err := models.ExactKeyErr(mk); err != nil {
			bg.keyErrs = append(bg.keyErrs, err.Error())
		}
	}
	// Each recording counts under every key its kind's keys give it, so one
	// two keys claim (and so keyless itself) is a sibling under either.
	seen := map[string]bool{}
	for _, n := range names {
		mk := byName[n]
		if seen[n] || mk == nil || mk.IsProtocolPhase() {
			continue
		}
		seen[n] = true
		for _, k := range models.ExactSiblingClaims(mk) {
			keys[string(mk.Kind)+"\x00"+k]++
		}
	}
	for _, e := range worker {
		mk := byName[e.Name]
		if mk == nil {
			continue
		}
		if models.IsIdleAnswer(mk) {
			bg.idle[e.Name] = true
		}
		if k, ok := keyOf(mk); ok && keys[k] == 1 {
			bg.noSibling[e.Name] = true
		}
	}
	return bg
}

// reachableWorker is the worker-scope mocks of worker (in recorded order) the
// set's run has reached: placed at the set's start, or after a test case that
// has begun (begun); and, under the background rules (bg), each with no
// identical sibling (testBackground.noSibling), wherever it is placed, in the
// view's order: by place (the set's start for one with no sibling, else the
// order its place's test case began in, begunAt), then a delivering answer
// before an idle one, then recorded order. With bg nil, in recorded order.
func reachableWorker(worker []models.MockEntry, begun map[string]bool, begunAt map[string]int, bg *testBackground) []string {
	type reached struct {
		name string
		rank int
		idle bool
		at   int
	}
	var rs []reached
	for i, e := range worker {
		early := bg != nil && bg.noSibling[e.Name]
		if e.Place != "" && !begun[e.Place] && !early {
			continue
		}
		r := reached{name: e.Name, at: i}
		if bg != nil {
			if e.Place != "" && !early {
				r.rank = begunAt[e.Place]
			}
			r.idle = bg.idle[e.Name]
		}
		rs = append(rs, r)
	}
	if bg != nil {
		sort.SliceStable(rs, func(i, j int) bool {
			if rs[i].rank != rs[j].rank {
				return rs[i].rank < rs[j].rank
			}
			if rs[i].idle != rs[j].idle {
				return !rs[i].idle
			}
			return rs[i].at < rs[j].at
		})
	}
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.name)
	}
	return out
}

// stagingRules is the rules a staging of ex is served by, before its
// background is settled: the background rules exactly when settleBackground
// settles one (a staging that names them, under the proven switch), else
// today's ("").
func (a *Agent) stagingRules(ex *models.ExactServing) string {
	if ex == nil || ex.Background != models.RecordedWithS12Routing || !provenswitch.On() {
		return ""
	}
	return models.RecordedWithS12Routing
}

// stageBackgroundExactly opens a set's staging in the proxy under rules
// (coreAgent.ExactBackgroundServer.StageExactBackground), before the staging
// installs any view: the previous set's background goes, and every view of
// the staging reads the set's rules. A proxy that cannot serve the background
// rules serves every set by today's, and needs nothing.
func (a *Agent) stageBackgroundExactly(es coreAgent.ExactServer, rules string) error {
	bs, ok := es.(coreAgent.ExactBackgroundServer)
	if !ok {
		return nil
	}
	return bs.StageExactBackground(rules)
}

// serveBackgroundExactly tells the proxy the rules the staged set's background
// is served by (coreAgent.ExactBackgroundServer): the background rules when rules is
// models.RecordedWithS12Routing, with the set's pool and its whole worker
// scope, else today's. A proxy that cannot serve the background rules serves every set
// by today's, which need nothing of it: the staging settles no background
// for it (testBg nil), so the agent says it serves today's and the replay
// keeps today's checks.
func (a *Agent) serveBackgroundExactly(es coreAgent.ExactServer, rules string, pool, worker []string) error {
	bs, ok := es.(coreAgent.ExactBackgroundServer)
	if !ok {
		return nil
	}
	return bs.ServeExactBackground(rules, pool, worker)
}

// diskRecordedOfKind is the on-disk store's reader of the staged set's
// recordings of a kind (DiskMocks.RangeKind), nil when the set keeps none on
// disk.
func (a *Agent) diskRecordedOfKind() func(kind models.Kind, visit func(name string, load func() (*models.Mock, error)) bool) error {
	disk := a.stagedDisk()
	if disk == nil {
		return nil
	}
	return disk.RangeKind
}
