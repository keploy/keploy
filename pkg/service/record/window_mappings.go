package record

import (
	"context"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// windowMappings collects, while a recording made without --sync runs under
// the proven switch, what its mappings.yaml is derived from at the end: the
// window of every test case it persisted and every mock it persisted (design
// v14 row S3). Without --sync the agent maps no test to its mocks, so without
// this the set has no mappings.yaml and its first replay made one from what
// each test happened to consume: a mock a test did not consume then dropped
// out of its mapping for good, and no later replay could see it go unconsumed.
//
// The windows are derived once, at the end (models.DeriveWindowMappings): a
// mock is often handed over before the test case of the window it is in, and a
// long request's window can contain mocks handed over long before it ends.
type windowMappings struct {
	mu    sync.Mutex
	tests []models.TestWindow
	mocks []models.WindowedMock
}

// newWindowMappings is the collector of a recording that derives its
// mappings.yaml from its test windows: one made without --sync under the
// proven switch, with mapping not disabled. Any other recording gets nil,
// which collects nothing, so it writes exactly what it wrote before.
func (r *Recorder) newWindowMappings() *windowMappings {
	if !provenswitch.On() || r.config.Record.Synchronous || r.config.DisableMapping {
		return nil
	}
	return &windowMappings{}
}

// addTest records the window of a test case once it is persisted (under the
// name its insert gave it).
func (w *windowMappings) addTest(tc *models.TestCase) {
	if w == nil {
		return
	}
	win := models.RecordedWindow(tc)
	w.mu.Lock()
	w.tests = append(w.tests, win)
	w.mu.Unlock()
}

// addMock records a mock once it is persisted (under the name its insert gave
// it, with the async lane its hooks stamped on it).
func (w *windowMappings) addMock(m *models.Mock) {
	if w == nil {
		return
	}
	wm := models.NewWindowedMock(m)
	w.mu.Lock()
	w.mocks = append(w.mocks, wm)
	w.mu.Unlock()
}

// write derives the set's mappings from its test windows and writes them to
// mappings.yaml: an entry for every test case the recording kept, with the
// test mocks of its window. A test case the finalize deleted (in deleted)
// keeps its window, so the mocks recorded inside it stay its own and are not
// handed to a test that ran beside it; its entry is left out.
//
// drained says the record drain joined every persist goroutine. When it did
// not, a test case or mock may still be on its way to disk, and a mapping
// derived now could leave a test without a mock it recorded: none is written.
// A mapping that is not written, or whose write fails, fails the recording, as
// a failed mappings write refuses under the proven model (design v14 §5.2).
func (w *windowMappings) write(ctx context.Context, logger *zap.Logger, db MappingDb, testSetID string, deleted map[string]struct{}, drained bool) {
	if w == nil {
		return
	}
	if !drained {
		logger.Error("mappings.yaml was not written: the record drain timed out, so a test case or mock may still have been on its way to disk, and a mapping derived from the test windows now could leave a test without a call it recorded",
			zap.String("testSetID", testSetID),
			zap.String("next_step", "re-record this test set"))
		utils.SetExitCodeOnce(utils.ExitKeployError)
		return
	}
	w.mu.Lock()
	tests := append([]models.TestWindow(nil), w.tests...)
	mocks := append([]models.WindowedMock(nil), w.mocks...)
	w.mu.Unlock()

	derived := models.DeriveWindowMappings(tests, mocks)
	byTest := make(map[string][]models.MockEntry, len(derived.Tests))
	mapped := 0
	for _, tc := range derived.Tests {
		if _, gone := deleted[tc.ID]; gone {
			continue
		}
		byTest[tc.ID] = tc.Mocks
		mapped += len(tc.Mocks)
	}
	if len(byTest) == 0 {
		return
	}
	if err := db.UpsertBatch(ctx, testSetID, byTest); err != nil {
		logger.Error("failed to save the test-mock mappings derived from the test windows",
			zap.String("testSetID", testSetID),
			zap.Error(err),
			zap.String("next_step", "keploy test cannot tell which recorded calls each test owns without mappings.yaml; fix the error above and re-record this test set"))
		utils.SetExitCodeOnce(utils.ExitKeployError)
		return
	}
	logger.Info("Saved the test-mock mappings derived from the test windows",
		zap.String("testSetID", testSetID),
		zap.Int("numTests", len(byTest)),
		zap.Int("mappedMocks", mapped))
	if n := len(derived.Concurrent); n > 0 {
		logger.Warn("some tests were recorded while another test's request was still open: a call recorded while two tests were open is mapped to the test whose request came first, which may not be the test that made it",
			zap.String("testSetID", testSetID),
			zap.Int("count", n),
			zap.Strings("tests", derived.Concurrent[:min(n, 20)]),
			zap.String("next_step", "if one of these tests fails in keploy test with a call it did not make or a call no recorded mock answered, record them one request at a time"))
	}
}
