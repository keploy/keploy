package record

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

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
	// lost is how many windows of test cases the recording failed to persist
	// tests holds (see addLostTest).
	lost int
	// mocks holds only the test mocks: a protocol-phase or async-lane mock
	// is no test's, and a recording can make millions of them.
	mocks []models.WindowedMock
	// stopAt is when keploy record began stopping the application (a stop
	// signal, the record timer), zero while it has not: a worker-scope mock
	// recorded from then on is the application's shutdown's
	// (MockEntry.AtStop).
	stopAt time.Time
}

// noteStop records that keploy record began stopping the application at at;
// the earliest such moment is kept.
func (w *windowMappings) noteStop(at time.Time) {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.stopAt.IsZero() || at.Before(w.stopAt) {
		w.stopAt = at
	}
	w.mu.Unlock()
}

// markAtStop marks, of the worker scope, each mock recorded once keploy
// record had begun stopping the application (stopAt): its shutdown's calls,
// which a replay requires only once it has stopped the application.
func markAtStop(worker []models.MockEntry, stopAt time.Time) {
	if stopAt.IsZero() {
		return
	}
	for i := range worker {
		if at, err := models.ParseMockTimestamp(worker[i].ReqTimestampMock); err == nil && !at.IsZero() && !at.Before(stopAt) {
			worker[i].AtStop = true
		}
	}
}

// lostTestPrefix starts the window name of a test case the recording failed
// to persist. No test case is named so: a test case's name is a file name.
const lostTestPrefix = "\x00lost-"

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
// name its insert gave it). One that came back without a name keeps its window
// and gets no entry, as a test case the recording failed to persist does: an
// entry needs the test's name.
func (w *windowMappings) addTest(tc *models.TestCase) {
	if w == nil {
		return
	}
	if tc == nil || tc.Name == "" {
		w.addLostTest(tc)
		return
	}
	win := models.RecordedWindow(tc)
	w.mu.Lock()
	w.tests = append(w.tests, win)
	w.mu.Unlock()
}

// addLostTest records the window of a test case the recording failed to
// persist. It owns what was recorded inside it, as a test case the finalize
// deletes does, so those mocks go to no test rather than to one that ran
// beside it; it gets no entry.
func (w *windowMappings) addLostTest(tc *models.TestCase) {
	if w == nil {
		return
	}
	win := models.RecordedWindow(tc)
	w.mu.Lock()
	w.lost++
	win.TestCase = fmt.Sprintf("%s%d", lostTestPrefix, w.lost)
	w.tests = append(w.tests, win)
	w.mu.Unlock()
}

// addMock records a mock once it is persisted (under the name its insert gave
// it, with the async lane its hooks stamped on it), when a test may own it.
func (w *windowMappings) addMock(m *models.Mock) {
	if w == nil {
		return
	}
	wm := models.NewWindowedMock(m)
	if !wm.TestMock || wm.At.IsZero() {
		return
	}
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
// streamed is how many mappings the agent handed over. An agent that maps a
// recording made without --sync itself (enterprise's static dedup resolves its
// windows in the agent) has written them already, and entries derived here as
// well could list one call under two tests: none is derived.
//
// drained says the record drain joined every persist goroutine. When it did
// not, a test case or mock may still be on its way to disk, and a mapping
// derived now could leave a test without a mock it recorded: none is written.
// A mapping that is not written, or whose write fails, fails the recording, as
// a failed mappings write refuses under the proven model (design v14 §5.2).
func (w *windowMappings) write(ctx context.Context, logger *zap.Logger, db MappingDb, testSetID string, deleted map[string]struct{}, drained bool, streamed uint64) {
	w.writeFor(ctx, logger, db, testSetID, deleted, drained, streamed, nil)
}

// writeFor is write, telling mapped, when set, each kept test case's mocks as
// its window derives them, as consumeMappingsFor tells a streamed mapping's:
// the capture watch charges a test case with a mock of its whose container
// the Docker daemon did not answer the read of (captureWatch.mapped).
func (w *windowMappings) writeFor(ctx context.Context, logger *zap.Logger, db MappingDb, testSetID string, deleted map[string]struct{}, drained bool, streamed uint64, mapped func(test string, entries []models.MockEntry)) {
	if w == nil {
		return
	}
	if streamed > 0 {
		logger.Info("the keploy agent mapped this recording's tests to their mocks itself, so no mappings are derived from the test windows",
			zap.String("testSetID", testSetID),
			zap.Uint64("mappingsFromTheAgent", streamed))
		return
	}
	if !drained {
		logger.Error("mappings.yaml was not written: the record drain timed out, so a test case or mock may still have been on its way to disk, and a mapping derived from the test windows now could leave a test without a call it recorded",
			zap.String("testSetID", testSetID),
			zap.String("next_step", "re-record this test set"))
		utils.SetExitCodeOnce(utils.ExitKeployError)
		return
	}
	// The drain joined every goroutine that adds to the collector: derive from
	// it as it is, without a copy.
	w.mu.Lock()
	derived := models.DeriveWindowMappings(w.tests, w.mocks)
	stopAt := w.stopAt
	w.mu.Unlock()
	kept := func(id string) bool {
		_, gone := deleted[id]
		return !gone && !strings.HasPrefix(id, lostTestPrefix)
	}
	byTest := make(map[string][]models.MockEntry, len(derived.Tests))
	mappedMocks := 0
	for _, tc := range derived.Tests {
		if !kept(tc.ID) {
			continue
		}
		byTest[tc.ID] = tc.Mocks
		mappedMocks += len(tc.Mocks)
		if mapped != nil && len(tc.Mocks) > 0 {
			mapped(tc.ID, tc.Mocks)
		}
	}
	var concurrent []string
	for _, id := range derived.Concurrent {
		if kept(id) {
			concurrent = append(concurrent, id)
		}
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
	// The set's worker scope (D2): what no test owns, each at its place. A
	// place whose test case the recording did not keep moves back to the
	// latest kept test case before it (the set's start before every one), so
	// a replay of the kept test cases reaches it.
	worker := keptPlaces(derived, kept)
	markAtStop(worker, stopAt)
	if err := db.UpsertWorker(ctx, testSetID, worker); err != nil {
		logger.Error("failed to save the test set's worker scope, the calls no test made, derived from the test windows",
			zap.String("testSetID", testSetID),
			zap.Error(err),
			zap.String("next_step", "keploy test cannot tell which recorded calls are the set's own without mappings.yaml's worker scope; fix the error above and re-record this test set"))
		utils.SetExitCodeOnce(utils.ExitKeployError)
		return
	}
	logger.Info("Saved the test-mock mappings derived from the test windows",
		zap.String("testSetID", testSetID),
		zap.Int("numTests", len(byTest)),
		zap.Int("mappedMocks", mappedMocks),
		zap.Int("workerMocks", len(worker)))
	if n := len(concurrent); n > 0 {
		logger.Warn("some tests were recorded while another test's request was still open: a call recorded while two tests were open is mapped to the test whose request came first, which may not be the test that made it",
			zap.String("testSetID", testSetID),
			zap.Int("count", n),
			zap.Strings("tests", concurrent[:min(n, 20)]),
			zap.String("next_step", "if one of these tests fails in keploy test with a call it did not make or a call no recorded mock answered, record them one request at a time"))
	}
}

// keptPlaces is derived's worker scope with each place a test case kept
// (kept) holds: a place whose test case is not kept moves back to the latest
// kept test case whose window began before it, in window order, or to the
// set's start before every one.
func keptPlaces(derived models.WindowMappings, kept func(id string) bool) []models.MockEntry {
	if len(derived.Worker) == 0 {
		return nil
	}
	// latestKept is, by test case ID, the latest kept test case at or before
	// it in window order.
	latestKept := make(map[string]string, len(derived.Tests))
	last := ""
	for _, tc := range derived.Tests {
		if kept(tc.ID) {
			last = tc.ID
		}
		latestKept[tc.ID] = last
	}
	out := make([]models.MockEntry, 0, len(derived.Worker))
	for _, e := range derived.Worker {
		if e.Place != "" {
			e.Place = latestKept[e.Place]
		}
		out = append(out, e)
	}
	return out
}
