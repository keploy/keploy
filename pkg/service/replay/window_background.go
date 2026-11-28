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
	// mocks recorded outside every recorded test window (between adjacent
	// recorded test windows) when creating or healing a keploy-written
	// mappings.yaml.
	WindowDerivedWorkerReason = "recorded outside every recorded test window (window-derived background); remove this entry from worker: in mappings.yaml to stop treating matching misses as background"

	windowDerivedMaxPerShape    = 8
	windowDerivedMaxPerTier     = 256
	windowDerivedStartupReserve = 64
)

// WindowDerivedIdleAnswerReason formats the legible reason for a parser-declared
// idle answer (models.IsIdleAnswer) recorded during a test window or unwindowed
// gap, without falsely claiming it was outside every recorded test window.
func WindowDerivedIdleAnswerReason(windowName string) string {
	if windowName != "" {
		return fmt.Sprintf("an idle answer (the poll found nothing), which no test owns, recorded during test window %s; remove this entry from worker: in mappings.yaml to stop treating matching misses as background", windowName)
	}
	return "an idle answer (the poll found nothing), which no test owns; remove this entry from worker: in mappings.yaml to stop treating matching misses as background"
}

// mappingSourceCommentChecker is an optional MappingDB capability that reports
// whether the `source:` line in mappings.yaml carries a trailing YAML comment
// (such as `source: keploy test # ops: hand-curated`), indicating a user edit.
type mappingSourceCommentChecker interface {
	SourceLineCommented(ctx context.Context, testSetID string) (bool, error)
}

// windowBackgroundHealer is an optional MappingDB capability that performs an
// in-place YAML edit on an unstamped pre-PR `source: keploy test` file, adding
// `background_derived: true` and `worker:` (plus `startup:` when absent)
// without reordering keys or tests: or dropping comments.
type windowBackgroundHealer interface {
	HealWindowBackground(ctx context.Context, testSetID string, worker, startup []models.MockEntry, derivedStartupReason string) (bool, error)
}

// windowBgTracker observes mocks during RunTestSet's already-existing single
// pass over mocks.yaml (loadMocksForAgent or readMockLookupWith), keeping a
// bounded set of outside-window startup/worker candidates via the shared
// models.DerivedBgCollector so deriving once in the replayer causes ZERO extra
// decodes of the mock file and hands the exact same set to both the agent and
// mappings.yaml.
type windowBgTracker struct {
	classifier         *models.WindowClassifier
	recordedFirst      time.Time
	seen               map[string]struct{}
	startupCol         *models.DerivedBgCollector
	workerCol          *models.DerivedBgCollector
	logger             *zap.Logger
	testSetID          string
	sawPass            bool
	loggedDrops        bool
	loggedRefusal      bool
	scheduleIncomplete bool
	refusalReason      string
	refusedByScope     int
}

func newWindowBgTracker(testCases []*models.TestCase, opts models.BackgroundWindowOptions, logger *zap.Logger, testSetID string) *windowBgTracker {
	windows := recordedTestWindows(testCases)
	if len(windows) == 0 {
		return nil
	}
	minSamples := learn.MinEpochReads + 1
	clf := models.NewWindowClassifierForBackground(windows, opts)
	refusalReason := ""
	switch {
	case opts.ScheduleIncomplete:
		refusalReason = "could not read the test set's full recorded schedule"
	case !clf.HasFullSchedule():
		refusalReason = "no recorded test schedule metadata (recorded_windows) is present for this set"
	case opts.RecordedStartUnreadable:
		refusalReason = "could not read the earliest recorded test start"
	}
	return &windowBgTracker{
		classifier:         clf,
		recordedFirst:      opts.RecordedFirstStart,
		seen:               make(map[string]struct{}),
		scheduleIncomplete: opts.ScheduleIncomplete,
		refusalReason:      refusalReason,
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

func (t *windowBgTracker) hasFullProof() bool {
	return t != nil && t.classifier != nil && t.classifier.HasFullSchedule()
}

func (t *windowBgTracker) logWholesaleRefusalOnce() {
	if t == nil || t.loggedRefusal || t.hasFullProof() || t.logger == nil {
		return
	}
	t.loggedRefusal = true
	reason := t.refusalReason
	if reason == "" {
		reason = "no recorded test schedule metadata (recorded_windows) is present for this set"
	}
	t.logger.Info("skipping persisted window-derived background derivation and keeping legacy Pass 1 behaviour; re-record the test set to persist recorded_windows",
		zap.String("testSetID", t.testSetID),
		zap.String("reason", reason))
}

func (r *Replayer) backgroundWindowOptions(ctx context.Context, testSetID string, testCases []*models.TestCase) models.BackgroundWindowOptions {
	var opts models.BackgroundWindowOptions
	if r == nil {
		return opts
	}
	if rwr, ok := r.testDB.(RecordedWindowsReader); ok {
		allWindows, err := rwr.RecordedTestWindows(ctx, testSetID)
		if err != nil {
			if r.logger != nil {
				r.logger.Warn("could not read the test set's full recorded test schedule; refusing window-derived background promotion (fail closed)",
					zap.String("testSetID", testSetID), zap.Error(err))
			}
			opts.ScheduleIncomplete = true
			return opts
		}
		opts.RecordedWindows = allWindows
	}
	if len(opts.RecordedWindows) == 0 && r.testSetConf != nil {
		tsConf, err := r.testSetConf.ReadForUpdate(ctx, testSetID)
		if err != nil {
			if r.logger != nil {
				r.logger.Warn("could not read test-set config for recorded_windows; refusing window-derived background promotion (fail closed)",
					zap.String("testSetID", testSetID), zap.Error(err))
			}
			opts.ScheduleIncomplete = true
			return opts
		}
		if tsConf != nil && len(tsConf.RecordedWindows) > 0 {
			opts.RecordedWindows = tsConf.RecordedWindows
		}
	}
	// --must-pass deletions ONLY add refusal zones; they never turn an
	// incomplete schedule into a complete recorded schedule.
	if deleted := r.deletedWindowsFor(testSetID); len(deleted) > 0 {
		opts.RefusalWindows = deleted
	}
	if rsr, ok := r.testDB.(RecordedStartReader); ok {
		recorded, err := rsr.RecordedFirstTestStart(ctx, testSetID)
		if err != nil {
			if r.logger != nil {
				r.logger.Warn("could not read when the test set's first test was recorded; refusing prefix window-derived startup promotion (fail closed)",
					zap.String("testSetID", testSetID), zap.Error(err))
			}
			opts.RecordedStartUnreadable = true
		} else if !recorded.IsZero() {
			opts.RecordedFirstStart = recorded
			opts.RecordedStartKnown = true
		}
	}
	return opts
}

func (t *windowBgTracker) markPass() {
	if t != nil {
		t.sawPass = true
	}
}

func (t *windowBgTracker) reset() {
	if t == nil {
		return
	}
	minSamples := learn.MinEpochReads + 1
	t.seen = make(map[string]struct{})
	t.sawPass = false
	t.loggedDrops = false
	t.refusedByScope = 0
	t.startupCol = models.NewDerivedBgCollector(models.DerivedBgCollectorConfig{
		MaxEntries:  windowDerivedMaxPerTier,
		MaxPerShape: windowDerivedMaxPerShape,
		MinPerShape: minSamples,
		HeadReserve: windowDerivedStartupReserve,
	})
	t.workerCol = models.NewDerivedBgCollector(models.DerivedBgCollectorConfig{
		MaxEntries:  windowDerivedMaxPerTier,
		MaxPerShape: windowDerivedMaxPerShape,
		MinPerShape: minSamples,
		HeadReserve: 0,
	})
}

func (t *windowBgTracker) observe(m *models.Mock) {
	if t == nil || m == nil || m.Name == "" {
		return
	}
	kind, winName := t.classifier.ClassifyForBackground(m, t.recordedFirst)
	if kind == models.WindowBgNone {
		t.refusedByScope++
		return
	}
	if _, dup := t.seen[m.Name]; dup {
		return
	}
	t.seen[m.Name] = struct{}{}

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
	return t.keepNamesExcludingExpected(nil, docs...)
}

// keepNamesExcludingExpected returns all derived startup and worker mock names
// not owned by any test case in expected or docs, ONLY when a full recorded
// schedule proof existed.
func (t *windowBgTracker) keepNamesExcludingExpected(expected map[string][]models.MockEntry, docs ...*models.Mapping) []string {
	if !t.hasFullProof() {
		return nil
	}
	owned := ownedMockNames(docs...)
	for _, list := range expected {
		for _, e := range list {
			if e.Name != "" {
				owned[e.Name] = true
			}
		}
	}
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

// backgroundShapeMatchedSet returns the set of loaded mock names not already
// in knownBgNames whose SpecificBackgroundShapeKey (including Mongo query key
// structure) matches at least one mock in knownBgNames. Used ONLY to prevent
// false OBSOLETE demotion on incidental in-window background polls — NEVER for
// --assert-dependencies, schema-noise-strict, DepResult, or unconsumedTestMocks.
func backgroundShapeMatchedSet(knownBgNames map[string]bool, mockSlices ...[]*models.Mock) map[string]bool {
	names := backgroundShapeMatchedNames(knownBgNames, mockSlices...)
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// isOnlyObsoleteShapeUnconsumed reports whether the mock-set mismatch between
// filteredConsumed and filteredExpected was caused SOLELY by unconsumed
// expected mocks in obsoleteShapeExcluded (no foreign mock consumed outside
// filteredExpected, and every unconsumed expected mock is shape-matched to a
// known background recording).
func isOnlyObsoleteShapeUnconsumed(filteredConsumed, filteredExpected []string, obsoleteShapeExcluded map[string]bool) bool {
	if len(obsoleteShapeExcluded) == 0 {
		return false
	}
	expectedSet := make(map[string]bool, len(filteredExpected))
	for _, n := range filteredExpected {
		expectedSet[n] = true
	}
	consumedSet := make(map[string]bool, len(filteredConsumed))
	for _, n := range filteredConsumed {
		if !expectedSet[n] {
			// A foreign mock outside the test's mapping was consumed (not just
			// an unconsumed background-shaped mock): keep OBSOLETE demotion.
			return false
		}
		consumedSet[n] = true
	}
	hasExcludedUnconsumed := false
	for _, n := range filteredExpected {
		if consumedSet[n] {
			continue
		}
		if !obsoleteShapeExcluded[n] {
			// A genuine per-test expected mock was left unconsumed.
			return false
		}
		hasExcludedUnconsumed = true
	}
	return hasExcludedUnconsumed
}

// streamObsoleteShapeVeto reports whether a streaming test case's
// mockSetMismatch was caused solely by unconsumed expected mocks matching a
// known background shape (vetoing OBSOLETE demotion without excusing foreign
// consumed mocks or --assert-dependencies). Kept out of RunTestSet so
// RunTestSet retains its single audited call to eligibleExpectedEntries.
func streamObsoleteShapeVeto(
	expected []models.MockEntry,
	consumed []models.MockState,
	mockKindByName map[string]models.Kind,
	reusableMockNames map[string]bool,
	obsoleteShapeExcluded map[string]bool,
) bool {
	if len(obsoleteShapeExcluded) == 0 {
		return false
	}
	var expNames, consNames []string
	for _, m := range eligibleExpectedEntries(expected, mockKindByName, reusableMockNames) {
		expNames = append(expNames, m.Name)
	}
	for _, m := range consumed {
		if !consumedOutsideAssertion(m) && !reusableMockNames[m.Name] {
			consNames = append(consNames, m.Name)
		}
	}
	return isOnlyObsoleteShapeUnconsumed(consNames, expNames, obsoleteShapeExcluded)
}

// backgroundShapeMatchedNames returns every loaded mock name not already in
// knownBgNames whose SpecificBackgroundShapeKey matches at least one mock in
// knownBgNames. Mocks without a concrete request identity (empty specific
// shape) never seed or match.
func backgroundShapeMatchedNames(knownBgNames map[string]bool, mockSlices ...[]*models.Mock) []string {
	if len(knownBgNames) == 0 {
		return nil
	}
	shapeOf := models.SpecificBackgroundShapeKey
	bgShapes := make(map[string]bool)
	for _, list := range mockSlices {
		for _, m := range list {
			if m == nil || !knownBgNames[m.Name] {
				continue
			}
			if key := shapeOf(m); key != "" {
				bgShapes[key] = true
			}
		}
	}
	if len(bgShapes) == 0 {
		return nil
	}
	var out []string
	added := make(map[string]bool)
	for _, list := range mockSlices {
		for _, m := range list {
			if m == nil || m.Name == "" || knownBgNames[m.Name] || added[m.Name] {
				continue
			}
			if key := shapeOf(m); key != "" && bgShapes[key] {
				added[m.Name] = true
				out = append(out, m.Name)
			}
		}
	}
	return out
}

// expandBackgroundExclusions widens backgroundExcluded (for legacy/auto-replay
// mode, !r.exactServing()) with proven windowBg-derived startup/worker mock
// names when the test set has a complete recorded schedule and is eligible for
// window-derived background persistence/healing. Shape-matched in-window mocks
// are NEVER added here (they apply only to OBSOLETE demotion).
func (r *Replayer) expandBackgroundExclusions(
	ctx context.Context,
	testSetID string,
	mappingExists func() (bool, error),
	windowBg *windowBgTracker,
	expectedTestMockMappings map[string][]models.MockEntry,
	base []string,
) []string {
	seen := make(map[string]bool, len(base))
	out := make([]string, 0, len(base))
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, n := range base {
		add(n)
	}
	if windowBg != nil && windowBg.hasFullProof() {
		allowBg, _, existingMap := r.windowBgEligibility(ctx, testSetID, mappingExists)
		// If a mappingReader already serves a non-nil mapping document (even
		// when Exists returned false on a synthetic store), honor its Source,
		// stamp, and Worker list rather than bypassing healability checks.
		if allowBg && existingMap == nil {
			if mr, ok := r.mappingDB.(mappingReader); ok {
				if m, err := mr.GetMapping(ctx, testSetID); err != nil {
					allowBg = false
				} else if m != nil {
					existingMap = m
					allowBg = r.isHealableKeployTestMappingDoc(ctx, testSetID, m)
				}
			}
		}
		if allowBg {
			for _, n := range windowBg.keepNamesExcludingExpected(expectedTestMockMappings, existingMap) {
				add(n)
			}
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
// mappingDoc ONLY when a full recorded schedule proof existed (hasFullProof).
// Excludes mocks already owned in extraExclude (pre-existing mappings) and
// strips promoted background entries from newly generated mappingDoc.TestCases
// so Pass 1 in RAM and the persisted mappings.yaml always agree. Stamps
// mappingDoc.BackgroundDerived = true ONLY when a full proof existed.
func (t *windowBgTracker) apply(mappingDoc, actual *models.Mapping, extraExclude ...*models.Mapping) (addedStartup, addedWorker int) {
	if t == nil || mappingDoc == nil {
		return 0, 0
	}
	if !t.hasFullProof() {
		t.logWholesaleRefusalOnce()
		return 0, 0
	}
	_ = actual
	exclude := ownedMockNames(extraExclude...)
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

	promoted := make(map[string]bool)
	if added := filterExcludedEntries(t.startupCol.Entries(), exclude); len(added) > 0 {
		mappingDoc.Startup = append(mappingDoc.Startup, added...)
		sortByRecordedTime(mappingDoc.Startup)
		addedStartup = len(added)
		for _, e := range added {
			promoted[e.Name] = true
		}
	}
	if added := filterExcludedEntries(t.workerCol.Entries(), exclude); len(added) > 0 {
		mappingDoc.Worker = append(mappingDoc.Worker, added...)
		sortByRecordedTime(mappingDoc.Worker)
		addedWorker = len(added)
		for _, e := range added {
			promoted[e.Name] = true
		}
	}
	// Keep Pass 1 and persisted mappings.yaml consistent: a mock proven outside
	// every recorded test window and handed to the agent as background in Pass 1
	// belongs in startup:/worker:, not duplicated inside a test's mocks: list.
	if len(promoted) > 0 && len(mappingDoc.TestCases) > 0 {
		for i := range mappingDoc.TestCases {
			if len(mappingDoc.TestCases[i].Mocks) == 0 {
				continue
			}
			kept := mappingDoc.TestCases[i].Mocks[:0]
			for _, m := range mappingDoc.TestCases[i].Mocks {
				if !promoted[m.Name] {
					kept = append(kept, m)
				}
			}
			mappingDoc.TestCases[i].Mocks = kept
		}
	}
	mappingDoc.BackgroundDerived = true
	t.logCapDrops()
	if t.logger != nil && t.refusedByScope > 0 {
		t.logger.Debug("window-derived background classifier left non-qualifying mocks out of startup:/worker: (inside, in-flight/edge-owned, unprovable prefix/gap, or trailing)",
			zap.String("testSetID", t.testSetID),
			zap.Int("refusedMocks", t.refusedByScope),
			zap.Int("derivedStartupMocks", addedStartup),
			zap.Int("derivedWorkerMocks", addedWorker))
	}
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
		t.logger.Info("window-derived background collector reached tier cap; dropped least-recently-seen sparse shapes first, then oldest shapes by recency",
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
//   - Existing unstamped pre-PR mappings.yaml written by keploy test
//     (Source == "keploy test", !BackgroundDerived, no derived startup:, no
//     trailing comment on source:, empty worker:) -> true, healing=true.
//   - Any stamped file (BackgroundDerived == true), user-edited file, keploy
//     record file (Source != "keploy test"), or file that already lists worker:
//     entries -> false (never overwritten or re-derived).
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
	if r.isHealableKeployTestMappingDoc(ctx, testSetID, m) {
		return true, true, m
	}
	return false, false, nil
}

// isHealableKeployTestMapping reports whether an existing on-disk mapping is an
// unstamped pre-PR file written by `keploy test` with an empty worker: list
// and no previously derived startup: entries.
func isHealableKeployTestMapping(m *models.Mapping) bool {
	if m == nil || m.Source != models.MappingSourceKeployTest || len(m.Worker) > 0 || m.BackgroundDerived {
		return false
	}
	for _, e := range m.Startup {
		if e.Reason == WindowDerivedStartupReason {
			return false
		}
	}
	return true
}

func (r *Replayer) isHealableKeployTestMappingDoc(ctx context.Context, testSetID string, m *models.Mapping) bool {
	if !isHealableKeployTestMapping(m) {
		return false
	}
	if sc, ok := r.mappingDB.(mappingSourceCommentChecker); ok {
		commented, err := sc.SourceLineCommented(ctx, testSetID)
		if err != nil || commented {
			return false
		}
	}
	return true
}

// populateWindowDerivedBackground populates mappingDoc.Startup and
// mappingDoc.Worker with bounded, legible window-derived background entries
// when creating a NEW mappings.yaml or healing an unstamped keploy-written one
// with an empty worker: list.
func (r *Replayer) populateWindowDerivedBackground(ctx context.Context, testSetID string, testCases []*models.TestCase, mappingDoc, actual *models.Mapping, extraExclude ...*models.Mapping) error {
	if r.mockDB == nil || mappingDoc == nil || len(testCases) == 0 {
		return nil
	}
	tracker := newWindowBgTracker(testCases, r.backgroundWindowOptions(ctx, testSetID, testCases), r.logger, testSetID)
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

// healKeployTestMappingWorker performs a one-time heal on an unstamped pre-PR
// `source: keploy test` mappings.yaml whose worker: list is empty, persisting
// window-derived worker: (and missing startup:) entries and stamping
// `background_derived: true` so any future user edit removing worker: entries
// is preserved permanently. Uses in-place YAML editing when supported by
// mappingDB so existing key order, tests: order, and comments stay intact.
func (r *Replayer) healKeployTestMappingWorker(ctx context.Context, testSetID string, existing, actual *models.Mapping, tracker *windowBgTracker, testCases []*models.TestCase) {
	if r.mappingDB == nil || !r.isHealableKeployTestMappingDoc(ctx, testSetID, existing) {
		return
	}
	healDoc := &models.Mapping{
		Version:           existing.Version,
		Kind:              existing.Kind,
		TestSetID:         testSetID,
		Startup:           append([]models.MockEntry(nil), existing.Startup...),
		Source:            models.MappingSourceKeployTest,
		BackgroundDerived: true,
	}
	if healDoc.Version == "" {
		healDoc.Version = "api.keploy.io/v1beta1"
	}
	if healDoc.Kind == "" {
		healDoc.Kind = "TestMockMapping"
	}
	if tracker != nil && tracker.sawPass {
		if !tracker.hasFullProof() {
			tracker.logWholesaleRefusalOnce()
			return
		}
		tracker.apply(healDoc, actual, existing)
	} else if err := r.populateWindowDerivedBackground(ctx, testSetID, testCases, healDoc, actual, existing); err != nil {
		return
	}
	if !healDoc.BackgroundDerived || len(healDoc.Worker) == 0 {
		return
	}
	// Preserve existing Startup if already present on disk.
	var derivedStartup []models.MockEntry
	if len(existing.Startup) == 0 {
		derivedStartup = healDoc.Startup
	} else {
		healDoc.Startup = nil
	}

	if healer, ok := r.mappingDB.(windowBackgroundHealer); ok {
		healed, err := healer.HealWindowBackground(ctx, testSetID, healDoc.Worker, derivedStartup, WindowDerivedStartupReason)
		if err != nil {
			if r.logger != nil {
				r.logger.Error("Error healing keploy-written mappings.yaml with window-derived worker entries",
					zap.String("testSetID", testSetID), zap.Error(err))
			}
			return
		}
		if !healed {
			return
		}
	} else {
		if err := r.mappingDB.Insert(ctx, healDoc); err != nil {
			if r.logger != nil {
				r.logger.Error("Error healing keploy-written mappings.yaml with window-derived worker entries",
					zap.String("testSetID", testSetID), zap.Error(err))
			}
			return
		}
	}
	if r.logger != nil {
		r.logger.Info("Healed unstamped keploy-written mappings.yaml (source: keploy test, empty worker:) with window-derived background entries and stamped background_derived: true so user removals of worker: entries are preserved",
			zap.String("testSetID", testSetID),
			zap.Int("derivedWorkerMocks", len(healDoc.Worker)),
			zap.Int("derivedStartupMocks", len(derivedStartup)))
	}
}
