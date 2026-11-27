package replay

import (
	"context"
	"fmt"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

const (
	// WindowDerivedStartupReason is written on startup: entries derived from
	// mocks recorded strictly before the earliest recorded test window when
	// creating or healing a keploy-written mappings.yaml.
	WindowDerivedStartupReason = "recorded before the first recorded test window (window-derived startup); remove this entry from startup: in mappings.yaml to stop treating matching misses as background"

	// WindowDerivedWorkerReason is written on worker: entries derived from
	// mocks recorded outside every recorded test window (between or after test
	// windows) when creating or healing a keploy-written mappings.yaml.
	WindowDerivedWorkerReason = "recorded outside every recorded test window (window-derived background); remove this entry from worker: in mappings.yaml to stop treating matching misses as background"

	windowDerivedMaxPerShape    = 8
	windowDerivedMaxPerTier     = 256
	windowDerivedStartupReserve = 64
)

// WindowDerivedIdleAnswerReason formats the legible reason for a parser-declared
// idle answer (rule D4, models.IsIdleAnswer) recorded during a test window or
// removed-test gap, without falsely claiming it was outside every test window.
func WindowDerivedIdleAnswerReason(windowName string) string {
	if windowName != "" {
		return fmt.Sprintf("idle background answer (rule D4) recorded during test window %s; remove this entry from worker: in mappings.yaml to stop treating matching misses as background", windowName)
	}
	return "idle background answer (rule D4); remove this entry from worker: in mappings.yaml to stop treating matching misses as background"
}

// windowBgTracker observes mocks during RunTestSet's already-existing single
// pass over mocks.yaml (loadMocksForAgent or readMockLookup), keeping a bounded
// set of outside-window startup/worker candidates via the shared
// models.DerivedBgCollector so persisting them causes ZERO extra decodes of the
// mock file and matches the agent's Pass 1 collector semantics.
type windowBgTracker struct {
	classifier    *models.WindowClassifier
	recordedFirst time.Time
	seen          map[string]struct{}
	startupCol    *models.DerivedBgCollector
	workerCol     *models.DerivedBgCollector
	logger        *zap.Logger
	testSetID     string
	sawPass       bool
	loggedDrops   bool
}

func newWindowBgTracker(testCases []*models.TestCase, recordedFirst time.Time, logger *zap.Logger, testSetID string) *windowBgTracker {
	windows := recordedTestWindows(testCases)
	if len(windows) == 0 {
		return nil
	}
	minSamples := learn.MinEpochReads + 1
	return &windowBgTracker{
		classifier:    models.NewWindowClassifier(windows),
		recordedFirst: recordedFirst,
		seen:          make(map[string]struct{}),
		startupCol: models.NewDerivedBgCollector(models.DerivedBgCollectorConfig{
			MaxEntries:  windowDerivedMaxPerTier,
			MaxPerShape: windowDerivedMaxPerShape,
			MinPerShape: minSamples,
			HeadReserve: windowDerivedStartupReserve,
		}),
		workerCol: models.NewDerivedBgCollector(models.DerivedBgCollectorConfig{
			MaxEntries:  windowDerivedMaxPerTier,
			MaxPerShape: windowDerivedMaxPerShape,
			MinPerShape: minSamples,
			HeadReserve: 0,
		}),
		logger:    logger,
		testSetID: testSetID,
	}
}

func (t *windowBgTracker) markPass() {
	if t != nil {
		t.sawPass = true
	}
}

func (t *windowBgTracker) observe(m *models.Mock) {
	if t == nil || m == nil || m.Name == "" {
		return
	}
	if _, dup := t.seen[m.Name]; dup {
		return
	}
	t.seen[m.Name] = struct{}{}

	kind, winName := t.classifier.ClassifyForBackground(m, t.recordedFirst)
	if kind == models.WindowBgNone {
		return
	}

	at := m.Spec.ReqTimestampMock
	var reason string
	var col *models.DerivedBgCollector
	switch kind {
	case models.WindowBgStartup:
		reason = WindowDerivedStartupReason
		col = t.startupCol
	case models.WindowBgOutsideWindow:
		reason = WindowDerivedWorkerReason
		col = t.workerCol
	case models.WindowBgIdleInWindow:
		reason = WindowDerivedIdleAnswerReason(winName)
		col = t.workerCol
	default:
		return
	}

	entry := models.MockEntry{
		Name:             m.Name,
		Kind:             string(m.Kind),
		Timestamp:        at.Unix(),
		ReqTimestampMock: at.Format(time.RFC3339Nano),
		Reason:           reason,
	}
	if !m.Spec.ResTimestampMock.IsZero() {
		entry.ResTimestampMock = m.Spec.ResTimestampMock.Format(time.RFC3339Nano)
	}
	col.Add(models.DerivedBgItem{
		Entry:  entry,
		Reason: reason,
		Shape:  models.BackgroundShapeKey(m),
		At:     at,
	})
}

func (t *windowBgTracker) observeSlice(mocks []*models.Mock) {
	if t == nil {
		return
	}
	t.sawPass = true
	for _, m := range mocks {
		t.observe(m)
	}
}

// keepNames returns all derived startup and worker mock names not owned by any
// test case in docs so RemoveUnusedMocks retains them before pruning runs.
func (t *windowBgTracker) keepNames(docs ...*models.Mapping) []string {
	if t == nil {
		return nil
	}
	owned := ownedMockNames(docs...)
	var out []string
	for _, e := range t.startupCol.Entries() {
		if e.Name != "" && !owned[e.Name] {
			out = append(out, e.Name)
		}
	}
	for _, e := range t.workerCol.Entries() {
		if e.Name != "" && !owned[e.Name] {
			out = append(out, e.Name)
		}
	}
	return out
}

func ownedMockNames(docs ...*models.Mapping) map[string]bool {
	exclude := make(map[string]bool)
	for _, doc := range docs {
		if doc == nil {
			continue
		}
		for _, tc := range doc.TestCases {
			for _, m := range tc.Mocks {
				if m.Name != "" {
					exclude[m.Name] = true
				}
			}
		}
	}
	return exclude
}

// apply merges the observed outside-window startup/worker entries into
// mappingDoc, excluding any mock owned by a test case in mappingDoc, actual, or
// extraExclude and any mock already listed in mappingDoc.Startup or
// mappingDoc.Worker. Logs at Info whenever a tier cap dropped recordings.
func (t *windowBgTracker) apply(mappingDoc, actual *models.Mapping, extraExclude ...*models.Mapping) (addedStartup, addedWorker int) {
	if t == nil || mappingDoc == nil {
		return 0, 0
	}
	allDocs := make([]*models.Mapping, 0, 2+len(extraExclude))
	allDocs = append(allDocs, mappingDoc, actual)
	allDocs = append(allDocs, extraExclude...)
	exclude := ownedMockNames(allDocs...)
	for _, m := range mappingDoc.Startup {
		if m.Name != "" {
			exclude[m.Name] = true
		}
	}
	for _, m := range mappingDoc.Worker {
		if m.Name != "" {
			exclude[m.Name] = true
		}
	}

	if added := filterExcludedEntries(t.startupCol.Entries(), exclude); len(added) > 0 {
		mappingDoc.Startup = append(mappingDoc.Startup, added...)
		sortByRecordedTime(mappingDoc.Startup)
		addedStartup = len(added)
	}
	if added := filterExcludedEntries(t.workerCol.Entries(), exclude); len(added) > 0 {
		mappingDoc.Worker = append(mappingDoc.Worker, added...)
		sortByRecordedTime(mappingDoc.Worker)
		addedWorker = len(added)
	}
	t.logCapDrops()
	return addedStartup, addedWorker
}

func (t *windowBgTracker) logCapDrops() {
	if t == nil || t.loggedDrops || t.logger == nil {
		return
	}
	t.loggedDrops = true
	logTier := func(tier string, st models.DerivedBgStats) {
		if st.DroppedByCap == 0 {
			return
		}
		t.logger.Info("window-derived background collector reached tier cap; dropped oldest shapes by recency while preserving active pollers",
			zap.String("testSetID", t.testSetID),
			zap.String("tier", tier),
			zap.Int("kept", st.Kept),
			zap.Int("distinctShapes", st.DistinctShapes),
			zap.Int("droppedByCap", st.DroppedByCap),
			zap.Int("droppedShapes", st.DroppedShapes),
			zap.Int("maxPerTier", windowDerivedMaxPerTier))
	}
	logTier("startup", t.startupCol.Stats())
	logTier("worker", t.workerCol.Stats())
}

func filterExcludedEntries(entries []models.MockEntry, exclude map[string]bool) []models.MockEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]models.MockEntry, 0, len(entries))
	for _, e := range entries {
		if !exclude[e.Name] {
			out = append(out, e)
		}
	}
	return out
}

// windowBgEligibility reports whether window-derived background entries may be
// populated for testSetID:
//   - Exists errors -> FAIL CLOSED (false): never clobber an unreadable file.
//   - No mappings.yaml yet (!exists) -> true (brand-new file).
//   - Existing mappings.yaml written by keploy test (Source == "keploy test")
//     with an empty worker: list -> true, healing=true (self-heals sets whose
//     mappings.yaml predates window-derived worker persistence).
//   - Any user-edited file, keploy record file (Source != "keploy test"), or
//     file that already lists worker: entries -> false (never overwritten).
func (r *Replayer) windowBgEligibility(ctx context.Context, testSetID string, mappingExists func() (bool, error)) (allow bool, healing bool, existing *models.Mapping) {
	if r.mappingDB == nil || mappingExists == nil {
		return false, false, nil
	}
	exists, existsErr := mappingExists()
	if existsErr != nil {
		return false, false, nil
	}
	if !exists {
		return true, false, nil
	}
	mr, ok := r.mappingDB.(mappingReader)
	if !ok {
		return false, false, nil
	}
	m, err := mr.GetMapping(ctx, testSetID)
	if err != nil || m == nil {
		return false, false, nil
	}
	if m.Source == models.MappingSourceKeployTest && len(m.Worker) == 0 {
		return true, true, m
	}
	return false, false, nil
}

// isHealableKeployTestMapping reports whether an existing on-disk mapping was
// written by `keploy test` with an empty worker: list (and thus eligible for
// window-derived background healing without touching user-edited files).
func isHealableKeployTestMapping(m *models.Mapping) bool {
	return m != nil && m.Source == models.MappingSourceKeployTest && len(m.Worker) == 0
}

// populateWindowDerivedBackground populates mappingDoc.Startup and
// mappingDoc.Worker with bounded, legible window-derived background entries
// when creating a NEW mappings.yaml or healing a keploy-written one with an
// empty worker: list.
func (r *Replayer) populateWindowDerivedBackground(ctx context.Context, testSetID string, testCases []*models.TestCase, mappingDoc, actual *models.Mapping, extraExclude ...*models.Mapping) error {
	if r.mockDB == nil || mappingDoc == nil || len(testCases) == 0 {
		return nil
	}
	tracker := newWindowBgTracker(testCases, r.recordedFirstTestStart(ctx, testSetID), r.logger, testSetID)
	if tracker == nil {
		return nil
	}
	if err := r.eachSetMock(ctx, testSetID, true, func(m *models.Mock, _ bool) {
		tracker.observe(m)
	}); err != nil {
		if r.logger != nil {
			r.logger.Warn("failed to stream mocks while deriving window background entries for mappings.yaml; worker: list was not derived",
				zap.String("testSetID", testSetID), zap.Error(err))
		}
		return err
	}
	tracker.apply(mappingDoc, actual, extraExclude...)
	return nil
}

// healKeployTestMappingWorker persists window-derived worker: (and missing
// startup:) entries into an existing keploy-written mappings.yaml whose
// worker: list is empty, without rewriting or removing any per-test entries.
func (r *Replayer) healKeployTestMappingWorker(ctx context.Context, testSetID string, existing, actual *models.Mapping, tracker *windowBgTracker, testCases []*models.TestCase) {
	if r.mappingDB == nil || !isHealableKeployTestMapping(existing) {
		return
	}
	healDoc := &models.Mapping{
		Version:   existing.Version,
		Kind:      existing.Kind,
		TestSetID: testSetID,
		Startup:   append([]models.MockEntry(nil), existing.Startup...),
		Source:    models.MappingSourceKeployTest,
	}
	if healDoc.Version == "" {
		healDoc.Version = "api.keploy.io/v1beta1"
	}
	if healDoc.Kind == "" {
		healDoc.Kind = "TestMockMapping"
	}
	if tracker != nil && tracker.sawPass {
		tracker.apply(healDoc, actual, existing)
	} else if err := r.populateWindowDerivedBackground(ctx, testSetID, testCases, healDoc, actual, existing); err != nil {
		return
	}
	if len(healDoc.Worker) == 0 {
		return
	}
	// Preserve existing Startup if already present on disk.
	if len(existing.Startup) > 0 {
		healDoc.Startup = nil
	}
	if err := r.mappingDB.Insert(ctx, healDoc); err != nil {
		if r.logger != nil {
			r.logger.Error("Error healing keploy-written mappings.yaml with window-derived worker entries",
				zap.String("testSetID", testSetID), zap.Error(err))
		}
		return
	}
	if r.logger != nil {
		r.logger.Info("Healed keploy-written mappings.yaml with window-derived background worker entries",
			zap.String("testSetID", testSetID),
			zap.Int("workerMocks", len(healDoc.Worker)),
			zap.Int("startupMocks", len(healDoc.Startup)))
	}
}
