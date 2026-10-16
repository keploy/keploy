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
	return a.beginTestSet(ctx, begin)
}

func (a *Agent) beginTestSet(ctx context.Context, begin models.TestSetBegin) error {
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
	filtered, unfiltered, err := a.poolMocks(ctx, filter, begin.FirstRecordedTestStart, begin.StartupMockNames)
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
// names (names are unique only within a set). The set's pool is served once
// its mocks are in.
func (a *Agent) beginTestSetExactly(ctx context.Context, begin models.TestSetBegin) error {
	es, err := a.exactServer()
	if err != nil {
		return err
	}
	if err := es.ServeExactPool(models.PoolKeyNone, nil); err != nil {
		return err
	}
	pool := append([]string(nil), begin.Exact.Pool...)
	begin.Exact = nil
	if err := a.beginTestSet(ctx, begin); err != nil {
		return fmt.Errorf("stage the test set's mocks: %w", err)
	}
	a.scopeMu.Lock()
	a.testPool = pool
	a.scopeMu.Unlock()
	return es.ServeExactPool(models.PoolKeyNone, append([]string(nil), pool...))
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
	filtered, unfiltered, err := a.poolMocks(ctx, filter, time.Time{}, nil)
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

// poolMocks is a pool served outside every test, filtered as filter asks
// (its mapping, the consumed history): what a set begins with, or what keploy
// mock replay serves between its tests.
//
// What it reads of the per-test mocks on disk: the mocks filter's mapping
// names (usesMapping); else, under the strict mock window and with the set's
// first recorded test (cutoff), what was recorded before that test, the
// application's startup traffic, and the startup names, each test reading its
// own as it begins; else every mock.
//
// A set used to begin with every mock of its recording, read back from disk
// only to be filed as app-startup traffic and refiled by the set's first
// test, and a long recording OOM-killed the agent before its application
// started.
func (a *Agent) poolMocks(ctx context.Context, filter models.MockFilterParams, cutoff time.Time, startup []string) ([]*models.Mock, []*models.Mock, error) {
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

	loaded, mode, err := a.loadPoolMocks(disk, filter, strict, cutoff, startup)
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
	return a.withoutConsumed(filteredMocks, filter.AgentOwnsConsumed, filter.TotalConsumedMocks), unfilteredMocks, nil
}

// The modes a pool reads its per-test mocks from disk in (poolMocks).
const (
	loadModePoolStartup = "pool-startup"
	loadModePoolAll     = "pool-all"
)

// loadPoolMocks reads a pool's per-test mocks from disk (see poolMocks), and
// says how it read them.
func (a *Agent) loadPoolMocks(disk *proxyPkg.DiskMocks, filter models.MockFilterParams, strict bool, cutoff time.Time, startup []string) ([]*models.Mock, string, error) {
	if disk == nil {
		return nil, "", nil
	}
	switch {
	case usesMapping(filter):
		loaded, err := disk.LoadByNames(filter.MockMapping)
		return loaded, loadModeMapping, err
	case strict && !cutoff.IsZero():
		before, err := disk.LoadBefore(cutoff)
		if err != nil {
			return nil, "", err
		}
		named, err := disk.LoadByNames(startup)
		if err != nil {
			return nil, "", err
		}
		loaded := dedupByName(before, named)
		a.logger.Info("staged the set's app-startup mocks from the on-disk store; each test reads its own when it runs",
			zap.Int("startupMocks", len(loaded)),
			zap.Int("onDisk", disk.Len()),
			zap.String("firstRecordedTest", cutoff.UTC().Format(time.RFC3339Nano)))
		return loaded, loadModePoolStartup, nil
	}
	loaded, err := disk.LoadAll()
	return loaded, loadModePoolAll, err
}
