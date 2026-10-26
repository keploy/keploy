package mapdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/pathsafe"
	"go.uber.org/zap"
)

type MappingDb struct {
	logger      *zap.Logger
	path        string
	MapFileName string
	Format      yaml.Format
}

func New(logger *zap.Logger, path string, mapFileName string) *MappingDb {
	return NewWithFormat(logger, path, mapFileName, yaml.FormatYAML)
}

func NewWithFormat(logger *zap.Logger, path string, mapFileName string, format yaml.Format) *MappingDb {
	return &MappingDb{
		logger:      logger,
		path:        path,
		MapFileName: mapFileName,
		Format:      format,
	}
}

func (db *MappingDb) Insert(ctx context.Context, mapping *models.Mapping) error {
	testSetID := mapping.TestSetID
	mappingPath := filepath.Join(db.path, testSetID)
	fileName := db.MapFileName
	if fileName == "" {
		fileName = "mappings"
	}

	finalMappings := make(map[string][]models.MockEntry)
	var existingStartup, existingWorker, existingShared []models.MockEntry
	var existingGroups []models.ConcurrencyGroup
	var existingSource string
	var existingRecordedWith []string
	var old []byte
	var existing *models.Mapping

	// Detect whether a mappings file already exists in either format,
	// and remember which one so we write back in the same format.
	exists, detected, err := yaml.FileExistsAny(ctx, db.logger, mappingPath, fileName, db.Format)
	if err != nil {
		utils.LogError(db.logger, err, "failed to check if mapping file exists", zap.String("path", mappingPath))
		return err
	}

	effFormat := db.Format
	if exists {
		effFormat = detected
		data, err := os.ReadFile(filepath.Join(mappingPath, fileName+"."+effFormat.FileExtension()))
		if err != nil {
			utils.LogError(db.logger, err, "failed to read existing mapping file", zap.String("path", mappingPath))
			return err
		}
		old = data

		var existingConfig models.Mapping
		if err := yaml.UnmarshalGeneric(effFormat, data, &existingConfig); err != nil {
			utils.LogError(db.logger, err, "failed to unmarshal existing mappings", zap.String("path", mappingPath))
			return err
		}

		for _, t := range existingConfig.TestCases {
			finalMappings[t.ID] = t.Mocks
		}
		existingStartup = existingConfig.Startup
		existingWorker = existingConfig.Worker
		existingGroups, existingShared = existingConfig.Groups, existingConfig.Shared
		existingSource = existingConfig.Source
		existingRecordedWith = existingConfig.RecordedWith
		existing = &existingConfig
	}

	for _, t := range mapping.TestCases {
		finalMappings[t.ID] = t.Mocks
	}

	newMapping := CreateMappingStructure(testSetID, finalMappings, db.logger)

	// Carry the startup section across the flatten-and-rebuild above.
	//
	// finalMappings is a per-test map, so everything that is NOT a test entry
	// is invisible to it and CreateMappingStructure cannot reconstruct what it
	// never saw. Without this the section is written once by the caller and
	// silently dropped on the very next Insert — the same class of bug as the
	// mock_entries/mocks mismatch on the api-server boundary.
	//
	// Replace-or-keep, mirroring how test entries merge: a caller that supplies
	// startup mocks is the fresh authority for this test set (replay captures
	// them anew on every run), while a caller that supplies none must not wipe
	// what is already on disk.
	//
	// UpsertBatch needs no equivalent — it decodes into *models.Mapping and
	// mutates TestCases in place, so the section survives untouched.
	//
	// KNOWN LIMITATION: this can never CLEAR a stale section. After a re-record
	// where the app no longer makes some boot call, a run capturing zero startup
	// mocks takes the second branch and the old names persist indefinitely.
	// Benign — DiskMocks.LoadByNames silently skips names it cannot find — but it
	// means the section only ever grows stale, never shrinks. Clearing it would
	// need a way to distinguish "captured nothing" from "did not look", which
	// this signature does not carry.
	switch {
	case len(mapping.Startup) > 0:
		newMapping.Startup = mapping.Startup
	case len(existingStartup) > 0:
		newMapping.Startup = existingStartup
	}
	// The worker scope too, replace-or-keep as the startup section: it is the
	// set's, not a test's, and CreateMappingStructure cannot see it.
	switch {
	case len(mapping.Worker) > 0:
		newMapping.Worker = mapping.Worker
	case len(existingWorker) > 0:
		newMapping.Worker = existingWorker
	}
	// The concurrency groups and shared calls (models.Mapping.Groups,
	// Shared), replace-or-keep as the worker scope: the entries carry the
	// group ids they name, which would otherwise name nothing.
	switch {
	case len(mapping.Groups) > 0 || len(mapping.Shared) > 0:
		newMapping.Groups, newMapping.Shared = mapping.Groups, mapping.Shared
	default:
		newMapping.Groups, newMapping.Shared = existingGroups, existingShared
	}
	// Who wrote the test entries: a writer that names itself, else the
	// file's (models.Mapping.Source).
	newMapping.Source = existingSource
	if mapping.Source != "" {
		newMapping.Source = mapping.Source
	}
	// What keploy record recorded the set with (models.Mapping.RecordedWith):
	// the set's, as the worker scope is, so replace-or-keep, whatever the
	// switch. keploy test's whole-file write names none, and keeps it.
	newMapping.RecordedWith = existingRecordedWith
	if len(mapping.RecordedWith) > 0 {
		newMapping.RecordedWith = mapping.RecordedWith
	}

	// The fields a hand edit added that this keploy does not know, at the
	// top of the file and on each test's entry, which the rebuild above
	// cannot see: the file's, then the caller's for a test it names.
	newMapping.KeepExtra(existing)
	newMapping.KeepTestExtra(existing)
	newMapping.KeepTestExtra(mapping)

	encodedData, err := db.encodeOver(newMapping, effFormat, old, true, testSetID)
	if err != nil {
		utils.LogError(db.logger, err, "failed to encode mapping", zap.String("testSetID", testSetID))
		return err
	}
	err = yaml.WriteFileF(ctx, db.logger, mappingPath, fileName, encodedData, false, effFormat)
	if err != nil {
		utils.LogError(db.logger, err, "failed to write mapping file", zap.String("path", mappingPath))
		return err
	}

	db.logger.Info("Successfully merged and saved test-mock mappings",
		zap.String("testSetID", testSetID),
		zap.Int("totalTests", len(finalMappings)))

	return nil
}

// Upsert writes a single test's mock entries, creating the file if absent. It is a one-entry UpsertBatch —
// prefer UpsertBatch when several mappings are ready at once, because each call
// rewrites the whole file (see UpsertBatch's note on cost).
func (db *MappingDb) Upsert(ctx context.Context, testSetID string, testID string, mockEntries []models.MockEntry) error {
	return db.UpsertBatch(ctx, testSetID, map[string][]models.MockEntry{testID: mockEntries})
}

// mergeMockEntries unions new entries into existing ones, keyed by mock name and
// preserving recorded order (existing first, then genuinely new arrivals).
//
// A test's mapping is built up in more than one go: the agent emits a test's
// mocks when its window resolves, and emits more later for mocks it retroactively
// bins into that already-resolved window. Those later emissions are a DELTA, not
// a replacement — overwriting on the second one silently deletes the mocks the
// first one recorded, and the test then replays with a short pool or none at all,
// which is a no_mocks failure. Union, never replace.
func mergeMockEntries(existing, incoming []models.MockEntry) []models.MockEntry {
	if len(existing) == 0 {
		return incoming
	}
	seen := make(map[string]struct{}, len(existing))
	for _, e := range existing {
		seen[e.Name] = struct{}{}
	}
	merged := existing
	for _, e := range incoming {
		if _, dup := seen[e.Name]; dup {
			continue
		}
		seen[e.Name] = struct{}{}
		merged = append(merged, e)
	}
	return merged
}

// UpsertBatch merges every supplied test's mock entries into mappings.yaml in a
// SINGLE read-modify-write. Entries for a test already present are unioned with
// what is on disk, never replaced — see mergeMockEntries.
//
// Why this exists: the file is stored as one document, so persisting a mapping
// means reading, decoding, re-encoding and rewriting all of it. Doing that once
// per mapping is quadratic in the number of tests — measured at 368 tests, the
// first write costs 164us and the last 19.45ms, a 118x slowdown, ~2.7s of pure
// rewriting. That is slow enough to back-pressure the agent's mapping stream,
// and the agent DROPS mappings it cannot hand over (its send is non-blocking so
// that capture is never stalled). The lost mappings then surface at replay as
// "no_mocks" for the affected tests. Batching keeps the cost linear so the
// stream is never the bottleneck.
func (db *MappingDb) UpsertBatch(ctx context.Context, testSetID string, byTest map[string][]models.MockEntry) error {
	var fileData []byte
	if len(byTest) == 0 {
		return nil
	}

	mappingPath := filepath.Join(db.path, testSetID)
	fileName := db.MapFileName
	if fileName == "" {
		fileName = "mappings"
	}

	var mapping *models.Mapping

	exists, detected, err := yaml.FileExistsAny(ctx, db.logger, mappingPath, fileName, db.Format)
	if err != nil {
		utils.LogError(db.logger, err, "failed to check if mapping file exists",
			zap.String("path", mappingPath),
			zap.String("fileName", fileName))
		return err
	}

	effFormat := db.Format
	if exists {
		effFormat = detected
		fileData, err = yaml.ReadFileF(ctx, db.logger, mappingPath, fileName, effFormat)
		if err != nil {
			utils.LogError(db.logger, err, "failed to read mapping file for upsert",
				zap.String("testSetID", testSetID))
			return err
		}

		mapping, err = DecodeMappingF(fileData, db.logger, effFormat)
		if err != nil {
			utils.LogError(db.logger, err, "failed to decode mapping",
				zap.String("testSetID", testSetID))
			return err
		}
	} else {
		mapping = &models.Mapping{
			Version:   string(models.V1Beta1),
			Kind:      models.MappingKind,
			TestSetID: testSetID,
			TestCases: []models.MappedTestCase{},
		}
	}

	// Index the existing entries once so a batch of N tests costs one pass
	// rather than N linear scans.
	at := make(map[string]int, len(mapping.TestCases))
	for i, t := range mapping.TestCases {
		at[t.ID] = i
	}

	// Append in a stable order. Go randomises map iteration, and mappings.yaml
	// is a recorded artifact that gets diffed and reviewed — an unstable test
	// order would make every re-record look like a change.
	newIDs := make([]string, 0, len(byTest))
	for testID := range byTest {
		if i, ok := at[testID]; ok {
			mapping.TestCases[i].Mocks = mergeMockEntries(mapping.TestCases[i].Mocks, byTest[testID])
			continue
		}
		newIDs = append(newIDs, testID)
	}
	sort.Strings(newIDs)
	for _, testID := range newIDs {
		mapping.TestCases = append(mapping.TestCases, models.MappedTestCase{
			ID:    testID,
			Mocks: byTest[testID],
		})
	}

	encodedData, err := db.encodeOver(mapping, effFormat, fileData, false, testSetID)
	if err != nil {
		utils.LogError(db.logger, err, "failed to encode mapping during upsert",
			zap.String("testSetID", testSetID))
		return err
	}

	err = yaml.WriteFileF(ctx, db.logger, mappingPath, fileName, encodedData, false, effFormat)
	if err != nil {
		utils.LogError(db.logger, err, "failed to write mapping file during upsert",
			zap.String("testSetID", testSetID))
		return err
	}

	db.logger.Debug("Successfully upserted test-mock mappings",
		zap.String("testSetID", testSetID),
		zap.Int("tests", len(byTest)))

	return nil
}

// UpsertWorker writes the set's worker scope (models.Mapping.Worker), whole,
// creating the file if absent and keeping every other section as it is: the
// recording derives the scope whole from its test windows (D2), so what is
// written replaces what was there. An empty scope clears it.
func (db *MappingDb) UpsertWorker(ctx context.Context, testSetID string, worker []models.MockEntry) error {
	var fileData []byte
	mappingPath := filepath.Join(db.path, testSetID)
	fileName := db.MapFileName
	if fileName == "" {
		fileName = "mappings"
	}
	exists, effFormat, err := yaml.FileExistsAny(ctx, db.logger, mappingPath, fileName, db.Format)
	if err != nil {
		return err
	}
	if !exists {
		effFormat = db.Format
	}
	mapping := &models.Mapping{Version: string(models.V1Beta1), Kind: models.MappingKind, TestSetID: testSetID, TestCases: []models.MappedTestCase{}}
	if exists {
		fileData, err = yaml.ReadFileF(ctx, db.logger, mappingPath, fileName, effFormat)
		if err != nil {
			return err
		}
		if mapping, err = DecodeMappingF(fileData, db.logger, effFormat); err != nil {
			return err
		}
	}
	mapping.Worker = worker
	encodedData, err := db.encodeOver(mapping, effFormat, fileData, false, testSetID)
	if err != nil {
		return err
	}
	return yaml.WriteFileF(ctx, db.logger, mappingPath, fileName, encodedData, false, effFormat)
}

// MoveToWorker bakes recordings into the set's worker scope
// (models.Mapping.Worker), as record's judging does with a call it saw made
// outside the window that recorded it (design v14, a2): each of moves
// names a mock and the place it is to answer from (MockEntry.Place). A mock a
// test case lists leaves that list and joins the scope at that place, with
// its entry's kind and recorded times; one the scope lists already takes that
// place. A mock neither lists is left alone. The scope stays in recorded
// order, and every other section is kept as it is. It returns the names it
// moved or placed, in moves' order; a set with no mapping file moves none.
//
// The caller decides each place: judging only ever moves a place earlier
// than the one recorded, never later.
//
// Each move's Reason (MockEntry.Reason, under the proven switch: why judging
// moved it, and its undo) is written on the entry as given: the entry's line
// is the last placement's. So is its Drift (MockEntry.Drift: the move rests on
// its owner's quarantine). Under the switch a mock taken out of a test's list
// names that test as its Owner (MockEntry.Owner), unless the move names one;
// a worker entry placed again keeps its owner unless the move names one.
// Without the switch no owner is written, so a move writes what it wrote
// before entries had one.
func (db *MappingDb) MoveToWorker(ctx context.Context, testSetID string, moves []models.MockEntry) ([]string, error) {
	if len(moves) == 0 {
		return nil, nil
	}
	mappingPath := filepath.Join(db.path, testSetID)
	fileName := db.MapFileName
	if fileName == "" {
		fileName = "mappings"
	}
	exists, effFormat, err := yaml.FileExistsAny(ctx, db.logger, mappingPath, fileName, db.Format)
	if err != nil || !exists {
		return nil, err
	}
	fileData, err := yaml.ReadFileF(ctx, db.logger, mappingPath, fileName, effFormat)
	if err != nil {
		return nil, err
	}
	mapping, err := DecodeMappingF(fileData, db.logger, effFormat)
	if err != nil {
		return nil, err
	}
	var moved []string
	for _, mv := range moves {
		if placeWorker(mapping, mv) {
			moved = append(moved, mv.Name)
		}
	}
	if len(moved) == 0 {
		return nil, nil
	}
	sort.SliceStable(mapping.Worker, func(i, j int) bool {
		return workerAt(mapping.Worker[i]).Before(workerAt(mapping.Worker[j]))
	})
	encodedData, err := db.encodeOver(mapping, effFormat, fileData, false, testSetID)
	if err != nil {
		return nil, err
	}
	if err := yaml.WriteFileF(ctx, db.logger, mappingPath, fileName, encodedData, false, effFormat); err != nil {
		return nil, err
	}
	return moved, nil
}

// placeWorker applies one of MoveToWorker's moves to mapping, reporting
// whether it found the mock. A mock several test cases list leaves every one
// of their lists: listed by one, it would still be that test's at replay.
func placeWorker(mapping *models.Mapping, mv models.MockEntry) bool {
	for i := range mapping.Worker {
		if mapping.Worker[i].Name == mv.Name {
			mapping.Worker[i].Place = mv.Place
			mapping.Worker[i].Reason = mv.Reason
			mapping.Worker[i].Drift = mv.Drift
			if mv.Owner != "" {
				mapping.Worker[i].Owner = mv.Owner
			}
			return true
		}
	}
	var entry *models.MockEntry
	owner := ""
	for i := range mapping.TestCases {
		kept := mapping.TestCases[i].Mocks[:0:0]
		for _, e := range mapping.TestCases[i].Mocks {
			if e.Name != mv.Name {
				kept = append(kept, e)
				continue
			}
			if entry == nil {
				e := e
				entry = &e
				owner = mapping.TestCases[i].ID
			}
		}
		if len(kept) != len(mapping.TestCases[i].Mocks) {
			mapping.TestCases[i].Mocks = kept
		}
	}
	if entry == nil {
		return false
	}
	entry.Place = mv.Place
	entry.Reason = mv.Reason
	entry.Drift = mv.Drift
	switch {
	case mv.Owner != "":
		entry.Owner = mv.Owner
	case provenswitch.On():
		entry.Owner = owner
	}
	mapping.Worker = append(mapping.Worker, *entry)
	return true
}

// MarkRecordedWith adds feature to what the set's mappings.yaml says keploy
// record recorded it with (models.Mapping.RecordedWith), keeping every other
// section as it is: keploy record marks a set a proven-routing record wrote
// (models.RecordedWithS12Routing). A mark the file carries already writes
// nothing. A set with no mapping file marks none, and says so: a recording
// that wrote none has nothing to serve by the mark.
func (db *MappingDb) MarkRecordedWith(ctx context.Context, testSetID, feature string) error {
	if feature == "" {
		return fmt.Errorf("mark what %s was recorded with: an empty mark", testSetID)
	}
	mappingPath := filepath.Join(db.path, testSetID)
	fileName := db.MapFileName
	if fileName == "" {
		fileName = "mappings"
	}
	exists, effFormat, err := yaml.FileExistsAny(ctx, db.logger, mappingPath, fileName, db.Format)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("mark %s as recorded with %s: the set has no mappings file", testSetID, feature)
	}
	fileData, err := yaml.ReadFileF(ctx, db.logger, mappingPath, fileName, effFormat)
	if err != nil {
		return err
	}
	mapping, err := DecodeMappingF(fileData, db.logger, effFormat)
	if err != nil {
		return err
	}
	if mapping.WasRecordedWith(feature) {
		return nil
	}
	mapping.RecordedWith = append(mapping.RecordedWith, feature)
	encodedData, err := db.encodeOver(mapping, effFormat, fileData, false, testSetID)
	if err != nil {
		return err
	}
	return yaml.WriteFileF(ctx, db.logger, mappingPath, fileName, encodedData, false, effFormat)
}

// workerAt is when a worker-scope entry's request was recorded, for its
// order (models.MockEntry.RecordedAt).
func workerAt(e models.MockEntry) time.Time { return e.RecordedAt() }

func (db *MappingDb) UpsertBoots(ctx context.Context, testSetID string, boots []models.BootSpec) error {
	var fileData []byte
	mappingPath := filepath.Join(db.path, testSetID)
	fileName := db.MapFileName
	if fileName == "" {
		fileName = "mappings"
	}
	exists, effFormat, err := yaml.FileExistsAny(ctx, db.logger, mappingPath, fileName, db.Format)
	if err != nil {
		return err
	}
	mapping := &models.Mapping{Version: string(models.V1Beta1), Kind: models.MappingKind, TestSetID: testSetID, TestCases: []models.MappedTestCase{}}
	if exists {
		fileData, err = yaml.ReadFileF(ctx, db.logger, mappingPath, fileName, effFormat)
		if err != nil {
			return err
		}
		if mapping, err = DecodeMappingF(fileData, db.logger, effFormat); err != nil {
			return err
		}
	}
	mapping.Boots = boots
	mapping.Startup = nil
	encodedData, err := db.encodeOver(mapping, effFormat, fileData, false, testSetID)
	if err != nil {
		return err
	}
	return yaml.WriteFileF(ctx, db.logger, mappingPath, fileName, encodedData, false, effFormat)
}

// UpsertCases records which test cases each test produced, adding an entry for a test that has no mocks yet.
func (db *MappingDb) UpsertCases(ctx context.Context, testSetID string, byTest map[string]models.MappedTestCase, startup []models.MockEntry, suites []models.SuiteSpan) error {
	var fileData []byte
	if len(byTest) == 0 && len(startup) == 0 && len(suites) == 0 {
		return nil
	}
	mappingPath := filepath.Join(db.path, testSetID)
	fileName := db.MapFileName
	if fileName == "" {
		fileName = "mappings"
	}
	exists, effFormat, err := yaml.FileExistsAny(ctx, db.logger, mappingPath, fileName, db.Format)
	if err != nil {
		return err
	}
	mapping := &models.Mapping{Version: string(models.V1Beta1), Kind: models.MappingKind, TestSetID: testSetID, TestCases: []models.MappedTestCase{}}
	if exists {
		fileData, err = yaml.ReadFileF(ctx, db.logger, mappingPath, fileName, effFormat)
		if err != nil {
			return err
		}
		if mapping, err = DecodeMappingF(fileData, db.logger, effFormat); err != nil {
			return err
		}
	}
	at := make(map[string]int, len(mapping.TestCases))
	for i, t := range mapping.TestCases {
		at[t.ID] = i
	}
	testIDs := make([]string, 0, len(byTest))
	for testID := range byTest {
		testIDs = append(testIDs, testID)
	}
	sort.Strings(testIDs)
	for _, testID := range testIDs {
		in := byTest[testID]
		i, ok := at[testID]
		if !ok {
			mapping.TestCases = append(mapping.TestCases, models.MappedTestCase{ID: testID})
			i = len(mapping.TestCases) - 1
		}
		tc := &mapping.TestCases[i]
		tc.Cases = mergeNames(tc.Cases, in.Cases)
		tc.CaseMocks = mergeMaps(tc.CaseMocks, in.CaseMocks)
		tc.CaseSteps = mergeMaps(tc.CaseSteps, in.CaseSteps)
		if in.Dir != "" {
			tc.Dir = in.Dir
		}
		if in.Starts > 0 {
			tc.Starts = in.Starts
		}
		if !in.At.IsZero() {
			tc.At = in.At
		}
		// The mock.real destinations this record's run of the test used
		// (proven switch only): they replace what an earlier record wrote.
		if len(in.Real) > 0 {
			tc.Real = in.Real
		}
	}
	if len(suites) > 0 {
		mapping.Suites = suites
	}
	if len(startup) > 0 {
		mapping.Startup = startup
	}
	encodedData, err := db.encodeOver(mapping, effFormat, fileData, false, testSetID)
	if err != nil {
		return err
	}
	return yaml.WriteFileF(ctx, db.logger, mappingPath, fileName, encodedData, false, effFormat)
}

func mergeMaps[V any](existing, incoming map[string]V) map[string]V {
	if len(incoming) == 0 {
		return existing
	}
	if existing == nil {
		existing = make(map[string]V, len(incoming))
	}
	for k, v := range incoming {
		existing[k] = v
	}
	return existing
}

// mergeNames unions incoming names into existing ones, keeping order and dropping repeats.
func mergeNames(existing, incoming []string) []string {
	seen := make(map[string]struct{}, len(existing))
	for _, n := range existing {
		seen[n] = struct{}{}
	}
	merged := existing
	for _, n := range incoming {
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		merged = append(merged, n)
	}
	return merged
}

// Exists reports whether the given test-set has a mappings file on disk, in
// either format, as Get, GetStartup, Insert and UpsertBatch find it. Used by
// the test-mode create-if-not-present write path — distinct from Get's second
// return (which is "has at least one non-empty test entry"). A file with only
// empty entries should still count as "exists" so we don't overwrite the
// operator's intentional empty mapping.
func (db *MappingDb) Exists(ctx context.Context, testSetID string) (bool, error) {
	mappingPath := filepath.Join(db.path, testSetID)
	fileName := db.MapFileName
	if fileName == "" {
		fileName = "mappings"
	}
	exists, _, err := yaml.FileExistsAny(ctx, db.logger, mappingPath, fileName, db.Format)
	return exists, err
}

// Delete removes the set's mappings file in every format; a missing file is not an error.
func (db *MappingDb) Delete(_ context.Context, testSetID string) error {
	if err := pathsafe.ValidateSingleSegment(testSetID, false); err != nil {
		return fmt.Errorf("rejecting Delete: testSetID %q must be a single-segment name under the mappings directory: %w", testSetID, err)
	}
	fileName := db.MapFileName
	if fileName == "" {
		fileName = "mappings"
	}
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		path, err := yaml.ValidatePath(filepath.Join(db.path, testSetID, fileName+"."+format.FileExtension()))
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			utils.LogError(db.logger, err, "failed to delete the mapping file", zap.String("path", path))
			return err
		}
	}
	return nil
}

// decodeMapping reads and decodes a test-set's mappings file.
//
// Shared by Get and GetStartup so the filename-defaulting, format detection and
// decode live in one place. Returns (nil, false, nil) when the file is absent —
// callers distinguish "no file" from "decoded" via the bool rather than by
// inspecting the error, so a missing mapping is never an error path.
func (db *MappingDb) decodeMapping(ctx context.Context, testSetID string) (*models.Mapping, string, bool, error) {
	mappingPath := filepath.Join(db.path, testSetID)
	fileName := db.MapFileName
	if fileName == "" {
		fileName = "mappings"
	}

	fileData, detected, err := yaml.ReadFileAny(ctx, db.logger, mappingPath, fileName, db.Format)
	if err != nil {
		if os.IsNotExist(err) {
			db.logger.Debug("Mapping file does not exist",
				zap.String("testSetID", testSetID),
				zap.String("path", mappingPath))
			return nil, "", false, nil
		}
		utils.LogError(db.logger, err, "failed to read mapping file",
			zap.String("testSetID", testSetID),
			zap.String("path", mappingPath),
			zap.String("fileName", fileName))
		return nil, "", false, err
	}

	mapping, err := DecodeMappingF(fileData, db.logger, detected)
	if err != nil {
		utils.LogError(db.logger, err, "failed to decode mapping",
			zap.String("testSetID", testSetID))
		return nil, "", false, err
	}
	return mapping, filepath.Join(mappingPath, fileName+"."+detected.FileExtension()), true, nil
}

// GetSource returns which keploy command wrote testSetID's mappings.yaml
// (models.Mapping.Source): "" for keploy record, and for no file.
func (db *MappingDb) GetSource(ctx context.Context, testSetID string) (string, error) {
	m, _, _, err := db.decodeMapping(ctx, testSetID)
	if err != nil || m == nil {
		return "", err
	}
	return m.Source, nil
}

func (db *MappingDb) GetMapping(ctx context.Context, testSetID string) (*models.Mapping, error) {
	m, _, _, err := db.decodeMapping(ctx, testSetID)
	return m, err
}

// GetCases lists the test cases each flow recorded, keyed by flow.
func (db *MappingDb) GetCases(ctx context.Context, testSetID string) (map[string][]string, error) {
	mapping, _, present, err := db.decodeMapping(ctx, testSetID)
	if err != nil || !present {
		return map[string][]string{}, err
	}
	out := make(map[string][]string, len(mapping.TestCases))
	for _, tc := range mapping.TestCases {
		if len(tc.Cases) > 0 {
			out[tc.ID] = append([]string(nil), tc.Cases...)
		}
	}
	return out, nil
}

// GetStartup reads the test-set-scoped startup section from mappings.yaml.
//
// Startup mocks are the traffic the app produced while booting — driver
// handshakes, connection-pool warm-up, config/secret fetches — recorded before
// the first test request fired. They belong to no single test, so they have no
// entry under `tests:` and Get() never returns them.
//
// This matters only on the MAPPING-BASED path. Agent.loadPerTestMocks picks its
// loader by mode:
//
//	UseMappingBased && len(MockMapping) > 0 -> disk.LoadByNames(MockMapping)
//	strict window                           -> disk.LoadWindow(...) + disk.LoadBefore(firstWindowStart)
//	otherwise                               -> disk.LoadAll()
//
// The window path loads startup mocks explicitly via LoadBefore. The mapping
// path loads ONLY the names it is handed, so a startup mock that appears in no
// test's list is absent from the pool for every test. Callers merge these names
// into the per-test list to close that gap.
//
// A missing file, or a file with no startup section, is not an error — it
// returns nil, which is the correct answer for every mapping written before the
// section existed.
func (db *MappingDb) GetStartup(ctx context.Context, testSetID string) ([]models.MockEntry, error) {
	mapping, _, present, err := db.decodeMapping(ctx, testSetID)
	if err != nil {
		return nil, err
	}
	if !present || mapping == nil || len(mapping.Startup) == 0 {
		return nil, nil
	}

	out := make([]models.MockEntry, 0, len(mapping.Startup))
	for _, e := range mapping.Startup {
		if e.Name == "" {
			continue
		}
		out = append(out, e)
	}

	db.logger.Debug("loaded test-set startup mocks from mappings",
		zap.String("testSetID", testSetID),
		zap.Int("startupMocks", len(out)))

	return out, nil
}

// Get reads test-mock mappings from a YAML file
// Returns: testMockMappings, mappingFilePresent, error
func (db *MappingDb) Get(ctx context.Context, testSetID string) (map[string][]models.MockEntry, bool, error) {
	mapping, filePath, present, err := db.decodeMapping(ctx, testSetID)
	if err != nil {
		return nil, false, err
	}
	if !present {
		return make(map[string][]models.MockEntry), false, nil
	}

	testMockMappings := GetMappings(mapping, db.logger)

	hasMeaningfulMappings := false
	for _, mocks := range testMockMappings {
		if len(mocks) > 0 {
			hasMeaningfulMappings = true
			break
		}
	}

	db.logger.Info("Successfully loaded test-mock mappings",
		zap.String("testSetID", testSetID),
		zap.String("filePath", filePath),
		zap.Int("numTests", len(testMockMappings)),
		zap.Bool("hasMeaningfulMappings", hasMeaningfulMappings))

	return testMockMappings, hasMeaningfulMappings, nil
}

// UpsertConcurrency writes the set's concurrency groups and shared calls
// (models.Mapping.Groups, Shared), whole, creating the file if absent and
// keeping every other section as it is: the recording derives them whole from
// its test windows, so what is written replaces what was there. Empty ones
// clear them. A file whose groups and shared calls are what it would write is
// left as it is, byte for byte, and a missing one is not created for none.
func (db *MappingDb) UpsertConcurrency(ctx context.Context, testSetID string, groups []models.ConcurrencyGroup, shared []models.MockEntry) error {
	return db.rewrite(ctx, testSetID, len(groups) > 0 || len(shared) > 0, false, func(m *models.Mapping) bool {
		if len(m.Groups) == 0 && len(m.Shared) == 0 && len(groups) == 0 && len(shared) == 0 {
			return false
		}
		m.Groups, m.Shared = groups, shared
		return true
	})
}

// ApplyShares writes onto the set's mappings.yaml what keploy record's full
// round settled of the calls it shared between tests that were open together
// while they were recorded (models.Mapping.ApplyShares): pins, moves and the
// shared calls, keeping every other section and recorded order, and dropping
// the groups no entry names any more. Idempotent. A set with no mapping file
// is an error: there is nothing to pin onto. It reports whether it wrote.
func (db *MappingDb) ApplyShares(ctx context.Context, testSetID string, s models.SetShares) (bool, error) {
	wrote := false
	err := db.rewrite(ctx, testSetID, false, true, func(m *models.Mapping) bool {
		wrote = m.ApplyShares(s)
		return wrote
	})
	return wrote, err
}

// rewrite reads the set's mapping file, applies change, and writes it back
// in the format it was in when change reports a change. A missing file is
// created (as an empty mapping) when create; otherwise it is an error when
// mustExist, and left missing when not.
func (db *MappingDb) rewrite(ctx context.Context, testSetID string, create, mustExist bool, change func(*models.Mapping) bool) error {
	var fileData []byte
	mappingPath := filepath.Join(db.path, testSetID)
	fileName := db.MapFileName
	if fileName == "" {
		fileName = "mappings"
	}
	exists, effFormat, err := yaml.FileExistsAny(ctx, db.logger, mappingPath, fileName, db.Format)
	if err != nil {
		return err
	}
	if !exists && !create {
		if !mustExist {
			return nil
		}
		return fmt.Errorf("the test set %s has no mappings file", testSetID)
	}
	if !exists {
		effFormat = db.Format
	}
	mapping := &models.Mapping{Version: string(models.V1Beta1), Kind: models.MappingKind, TestSetID: testSetID, TestCases: []models.MappedTestCase{}}
	if exists {
		fileData, err = yaml.ReadFileF(ctx, db.logger, mappingPath, fileName, effFormat)
		if err != nil {
			return err
		}
		if mapping, err = DecodeMappingF(fileData, db.logger, effFormat); err != nil {
			return err
		}
	}
	if !change(mapping) {
		return nil
	}
	encodedData, err := db.encodeOver(mapping, effFormat, fileData, false, testSetID)
	if err != nil {
		return err
	}
	return yaml.WriteFileF(ctx, db.logger, mappingPath, fileName, encodedData, false, effFormat)
}
