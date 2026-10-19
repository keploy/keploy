package agent

import (
	"context"
	"fmt"
	"time"

	"go.keploy.io/server/v3/pkg"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// BeginTestSet begins a test set (models.TestSetBegin), before the
// application starts and before any of the set's tests: the set's stateful
// cursors start over, and the proxy starts the set's state afresh (its startup
// cutoff, its recorded windows, whether a test has fired) with the set's pool
// staged, the mocks the application is served until the set's first test
// begins (UpdateMockParams). The pool is read from the mocks the client stored
// for the set (StoreMocks).
//
// Under the proven switch, keploy test's exact serving (begin.Exact) stages
// the set's mocks once, then serves the set's pool between test cases.
func (a *Agent) BeginTestSet(ctx context.Context, begin models.TestSetBegin) error {
	if begin.Exact != nil && provenswitch.On() {
		return a.beginTestSetExactly(ctx, begin)
	}
	return a.beginTestSet(ctx, begin, nil)
}

// beginTestSet begins the set without exact serving. alsoLoad names per-test
// mocks to read from the on-disk store as well, beside the set's startup
// traffic: under the proven switch, the set's exact pool and worker scope
// (beginTestSetExactly).
func (a *Agent) beginTestSet(ctx context.Context, begin models.TestSetBegin, alsoLoad []string) error {
	// Each test set replays its stateful sequences (1, 2, 3…) from the first
	// recorded response, rather than carrying a cursor across sets; a test's
	// filter leaves them alone, so a sequence spans the whole set. Optional
	// capability: a proxy without stateful cursors has nothing to reset.
	if resetter, ok := a.Proxy.(coreAgent.StatefulCursorResetter); ok {
		resetter.ResetStatefulCursors()
	}
	filter := models.MockFilterParams{
		UseMappingBased:    begin.UseMappingBased,
		StrictMockWindow:   begin.StrictMockWindow,
		AgentOwnsConsumed:  begin.AgentOwnsConsumed,
		TotalConsumedMocks: begin.TotalConsumedMocks,
	}
	filtered, unfiltered, err := a.poolMocks(ctx, filter, poolRead{
		cutoff:  begin.FirstRecordedTestStart,
		windows: begin.RecordedWindows,
		startup: begin.StartupMockNames,
		also:    alsoLoad,
	})
	if err != nil {
		return err
	}
	stager, ok := a.Proxy.(coreAgent.TestSetStager)
	if !ok {
		if err := a.Proxy.SetMocks(ctx, filtered, unfiltered); err != nil {
			utils.LogError(a.logger, err, "failed to set mocks on proxy")
			return err
		}
		return nil
	}
	if err := stager.BeginTestSet(ctx, begin.FirstRecordedTestStart, begin.RecordedWindows, filtered, unfiltered); err != nil {
		utils.LogError(a.logger, err, "failed to begin the test set on the proxy", zap.String("testSetID", begin.TestSetID))
		return err
	}
	a.logger.Debug("began the test set",
		zap.String("testSetID", begin.TestSetID),
		zap.Time("firstRecordedTest", begin.FirstRecordedTestStart),
		zap.Int("recordedWindows", len(begin.RecordedWindows)),
		zap.Int("filtered", len(filtered)),
		zap.Int("unfiltered", len(unfiltered)))
	return nil
}

// beginTestSetExactly is a test set's begin under keploy test's exact serving
// (proven switch): exact serving goes on, with no pool, before the set's
// mocks are staged, so no staged mock is ever served outside it and none
// still staged from the previous set answers under one of this set's pool
// names (names are unique only within a set). The previous set's worker scope
// goes for the same reason, before that view is installed. The set's pool is
// served once its mocks are in.
func (a *Agent) beginTestSetExactly(ctx context.Context, begin models.TestSetBegin) error {
	es, err := a.exactServer()
	if err != nil {
		return err
	}
	ex := begin.Exact
	begin.Exact = nil
	a.scopeMu.Lock()
	a.testWorker, a.testBegun = nil, nil
	a.scopeMu.Unlock()
	if err := a.serveWorkerExactly(es); err != nil {
		return err
	}
	if err := es.ServeExactPool(models.PoolKeyNone, nil); err != nil {
		return err
	}
	// The pool's per-test mocks are staged with the set's startup traffic:
	// the begin reads, of the on-disk store, only what was recorded before
	// the set's first test (loadPoolMocks), and each test reads its own as it
	// opens (ServeExactTest). A call recorded while no test was open, between
	// tests or after the last one, is the pool's, served by name from the
	// staged mocks; were it not staged, its replay would find no mock. The
	// pool is what no test owns, never the whole recording.
	//
	// The set's worker scope (models.Mapping.Worker) is staged by name with
	// the pool: its mocks answer from their places on.
	if err := a.beginTestSet(ctx, begin, append(append([]string(nil), ex.Pool...), entryNames(ex.Worker)...)); err != nil {
		return fmt.Errorf("stage the test set's mocks: %w", err)
	}
	// A miss on a call that is the recording of a test not begun yet (a
	// worker's call made early) is told from the on-disk store: a test's own
	// are staged only as it opens.
	if rs, ok := a.Proxy.(coreAgent.RecordingSourceSetter); ok {
		if err := rs.SetRecordingSource(a.diskRecordingSource()); err != nil {
			return fmt.Errorf("set the test set's recording source: %w", err)
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
}

// EndTestSet ends a test set: its tests are over, and the agent lets go of the
// mocks the client stored for it (StoreMocks), the on-disk store's file
// included. What the proxy serves stays staged until the next set begins, so
// an application kept alive across the boundary is answered as before; a
// test's filter needs the set's mocks stored again.
func (a *Agent) EndTestSet(_ context.Context, testSetID string) error {
	v, ok := a.clientMocks.LoadAndDelete(uint64(0))
	a.scopeMu.Lock()
	a.loadedMocks = 0
	a.scopeMu.Unlock()
	if !ok {
		return nil
	}
	storage, _ := v.(*ClientMockStorage)
	if storage == nil {
		return nil
	}
	storage.mu.Lock()
	disk := storage.diskMocks
	stored := len(storage.filtered) + len(storage.unfiltered)
	storage.mu.Unlock()
	if disk != nil {
		stored += disk.Len()
		if err := disk.Close(); err != nil {
			utils.LogError(a.logger, err, "failed to remove the ended test set's on-disk mock store; its temp file stays until the agent exits",
				zap.String("testSetID", testSetID))
		}
	}
	a.logger.Info("the test set ended: let go of the mocks stored for it",
		zap.String("testSetID", testSetID), zap.Int("storedMocks", stored))
	return nil
}

// stagePool stages, between tests of the current set, the pool filter names:
// the mocks its mapping names (usesMapping), or every stored mock. It opens no
// test window, and leaves the set's state as it is (TestSetStager.StagePool).
// keploy mock replay stages it as a test of its ends.
func (a *Agent) stagePool(ctx context.Context, filter models.MockFilterParams) error {
	filtered, unfiltered, err := a.poolMocks(ctx, filter, poolRead{})
	if err != nil {
		return err
	}
	stager, ok := a.Proxy.(coreAgent.TestSetStager)
	if !ok {
		if err := a.Proxy.SetMocks(ctx, filtered, unfiltered); err != nil {
			utils.LogError(a.logger, err, "failed to set mocks on proxy")
			return err
		}
		return nil
	}
	if err := stager.StagePool(ctx, filtered, unfiltered); err != nil {
		utils.LogError(a.logger, err, "failed to stage the pool between tests on the proxy")
		return err
	}
	return nil
}

// poolWindow is the window a pool's mocks are filtered by: every recorded
// time. A pool is served outside every test, so none of its mocks is left out
// for when it was recorded; the filter routes each by its lifetime only, as a
// test's filter does (pkg.FilterPerTestAndLaxPromotedTierAware).
func poolWindow() (time.Time, time.Time) {
	return models.BaseTime, time.Now()
}

// poolRead is what a pool reads of the per-test mocks on disk beside its
// mapping's (poolMocks). The zero value reads every mock.
type poolRead struct {
	// cutoff is the request time of the set's earliest recorded test
	// (models.TestSetBegin.FirstRecordedTestStart): under the strict mock
	// window what was recorded before it is the application's startup
	// traffic, the pool's.
	cutoff time.Time
	// windows are the set's recorded test windows: with them, the carry-over
	// mocks the application's startup reaches are read too
	// (proxyPkg.StartupCarryOverRange).
	windows []models.TestWindow
	// startup names the mapping's startup mocks.
	startup []string
	// also names per-test mocks read as well: under the proven switch, the
	// set's exact pool and worker scope.
	also []string
}

// poolMocks is a pool served outside every test, filtered as filter asks
// (its mapping, the consumed history): what a set begins with, or what keploy
// mock replay serves between its tests.
//
// What it reads of the per-test mocks on disk: the mocks filter's mapping
// names (usesMapping); else, under the strict mock window and with the set's
// first recorded test (read.cutoff), what was recorded before that test, the
// application's startup traffic, with the startup names, the mocks a parser
// declares it needs staged (models.StagingWithStartup), the carry-over mocks
// the startup reaches and read.also, each test reading its own as it begins;
// else every mock.
//
// A set used to begin with every mock of its recording, read back from disk
// only to be filed as app-startup traffic and refiled by the set's first
// test, and a long recording OOM-killed the agent before its application
// started.
func (a *Agent) poolMocks(ctx context.Context, filter models.MockFilterParams, read poolRead) ([]*models.Mock, []*models.Mock, error) {
	strict := a.noteStrictWindow(filter.StrictMockWindow)
	storage, err := a.storedMocks()
	if err != nil {
		return nil, nil, err
	}
	storage.mu.RLock()
	resident := append([]*models.Mock(nil), storage.filtered...)
	unfiltered := append([]*models.Mock(nil), storage.unfiltered...)
	disk := storage.diskMocks
	storage.mu.RUnlock()

	loaded, mode, err := a.loadPoolMocks(disk, resident, filter, strict, read)
	var carry []*models.Mock
	if err == nil && mode == loadModePoolStartup {
		carry, err = a.loadStartupCarryOver(disk, read)
	}
	if err != nil {
		utils.LogError(a.logger, err, "failed to read the pool's mocks from the agent's on-disk store; the temp file may be unreadable or the pool was superseded")
		return nil, nil, err
	}
	if disk != nil {
		a.logger.Debug("agent mock residency: loaded per-test set from disk",
			zap.String("mode", mode),
			zap.Int("loadedFromDisk", len(loaded)),
			zap.Int("residentIneligible", len(resident)))
	}
	perTest := dedupByName(resident, loaded)

	var filteredMocks, unfilteredMocks []*models.Mock
	if usesMapping(filter) {
		filteredMocks = pkg.FilterTcsMocksMapping(ctx, a.logger, perTest, filter.MockMapping)
		unfilteredMocks = pkg.FilterConfigMocksMapping(ctx, a.logger, unfiltered, filter.MockMapping)
	} else {
		from, to := poolWindow()
		var promoted []*models.Mock
		filteredMocks, promoted = pkg.FilterPerTestAndLaxPromotedTierAware(ctx, a.logger, perTest, from, to, false, time.Time{})
		unfilteredMocks = append(pkg.FilterConfigMocksTierAware(ctx, a.logger, unfiltered, from, to, false, time.Time{}), promoted...)
	}
	// The carry-over mocks the startup reaches go to the proxy as the
	// set's own, which files them with its startup traffic; the consumed
	// history drops the ones already consumed.
	filteredMocks = appendCarryOver(filteredMocks, carry)
	return a.withoutConsumed(filteredMocks, filter.AgentOwnsConsumed, filter.TotalConsumedMocks), unfilteredMocks, nil
}

// The modes a pool reads its per-test mocks from disk in (poolMocks).
const (
	loadModePoolStartup = "pool-startup"
	loadModePoolAll     = "pool-all"
)

// loadPoolMocks reads a pool's per-test mocks from disk (see poolMocks), and
// says how it read them. resident is what the store holds in memory, which is
// not read again.
func (a *Agent) loadPoolMocks(disk *proxyPkg.DiskMocks, resident []*models.Mock, filter models.MockFilterParams, strict bool, read poolRead) ([]*models.Mock, string, error) {
	if disk == nil {
		return nil, "", nil
	}
	switch {
	case usesMapping(filter):
		loaded, err := disk.LoadByNames(filter.MockMapping)
		if err != nil {
			return nil, "", err
		}
		also, err := loadNamesNotHeld(disk, read.also, resident, loaded)
		return append(loaded, also...), loadModeMapping, err
	case strict && !read.cutoff.IsZero():
		loaded, err := disk.LoadBefore(read.cutoff)
		if err != nil {
			return nil, "", err
		}
		named, err := loadNamesNotHeld(disk, read.startup, resident, loaded)
		if err != nil {
			return nil, "", err
		}
		loaded = append(loaded, named...)
		startup := len(loaded)
		// Beside the startup band, the mocks a parser declares it needs
		// staged with it (models.StagingWithStartup): a server push it files
		// as the set begins and releases at its own test window. They are
		// read from the band's cutoff on, so the two never overlap.
		declared := 0
		if models.StagedAtStagingRegistered() {
			_, to := poolWindow()
			staged, err := disk.LoadStaged(read.cutoff, to)
			if err != nil {
				return nil, "", err
			}
			loaded = append(loaded, staged...)
			declared = len(staged)
		}
		// And read.also (the exact pool, the worker scope), what the band
		// and the declared mocks have not read already.
		also, err := loadNamesNotHeld(disk, read.also, resident, loaded)
		if err != nil {
			return nil, "", err
		}
		loaded = append(loaded, also...)
		a.logger.Info("staged the set's app-startup mocks from the on-disk store; each test reads its own when it runs",
			zap.Int("startupMocks", startup),
			zap.Int("declaredMocks", declared),
			zap.Int("poolMocks", len(also)),
			zap.Int("onDisk", disk.Len()),
			zap.String("firstRecordedTest", read.cutoff.UTC().Format(time.RFC3339Nano)))
		return loaded, loadModePoolStartup, nil
	}
	loaded, err := disk.LoadAll()
	return loaded, loadModePoolAll, err
}

// loadNamesNotHeld reads, from disk, the mocks names lists that no slice of
// held holds already (by name), so none is decoded twice.
func loadNamesNotHeld(disk *proxyPkg.DiskMocks, names []string, held ...[]*models.Mock) ([]*models.Mock, error) {
	if len(names) == 0 {
		return nil, nil
	}
	have := make(map[string]struct{})
	for _, list := range held {
		for _, m := range list {
			if m != nil {
				have[m.Name] = struct{}{}
			}
		}
	}
	want := make([]string, 0, len(names))
	for _, n := range names {
		if _, ok := have[n]; !ok {
			want = append(want, n)
		}
	}
	if len(want) == 0 {
		return nil, nil
	}
	return disk.LoadByNames(want)
}

// loadStartupCarryOver reads, as a set begins with its startup band, the
// carry-over mocks (models.IsCarryOver) the application's startup reaches
// (proxyPkg.StartupCarryOverRange): what each test's lookahead load
// (loadCarryOverLookahead) reads for its window, for the startup. Nothing is
// read with no disk store, when the proxy plans no carry-over
// (coreAgent.CarryOverPlanner), or when the range is empty.
func (a *Agent) loadStartupCarryOver(disk *proxyPkg.DiskMocks, read poolRead) ([]*models.Mock, error) {
	if disk == nil {
		return nil, nil
	}
	if _, ok := a.Proxy.(coreAgent.CarryOverPlanner); !ok {
		return nil, nil
	}
	from, to, ok := proxyPkg.StartupCarryOverRange(read.cutoff, read.windows)
	if !ok {
		return nil, nil
	}
	carry, err := disk.LoadCarryOver(from, to)
	if err != nil {
		return nil, err
	}
	a.logger.Debug("agent mock residency: loaded the carry-over mocks the app's startup reaches",
		zap.Time("from", from), zap.Time("to", to), zap.Int("count", len(carry)))
	return carry, nil
}
