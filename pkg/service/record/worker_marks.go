package record

import (
	"context"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// workerMarks is a recording's marks under the user's worker-mock rules
// (config Record.WorkerMocks): each mock a rule matches, as the worker-scope
// entry it is to become (Reason models.WorkerRuleReason of the rule), waiting
// until the set's mapping file can take it.
//
// Every matching mock is marked, the ones a test consumed and the ones no test
// did (a call made outside every test's window, which no test's entry ever
// lists): a worker call's mock is often of the second kind.
//
// The entries are written by the mapping stream's consumer, right after it
// writes a batch of tests' mappings (consumeMappingsFor), and once more after
// the drain: AddToWorker rewrites the whole mapping file, so it must never run
// beside another write of it, and a set has a mapping file only once its
// first batch is written. Writing as the recording goes, not only at its end,
// matters for a Kubernetes recording, which runs for hours and is
// auto-replayed set by set before it ends.
type workerMarks struct {
	rules   []models.WorkerMockRule
	logger  *zap.Logger
	mu      sync.Mutex
	pending []models.MockEntry
	written int
	// overflowed is set once pending reached maxPendingWorkerMarks and a
	// mark was dropped: said once.
	overflowed bool
}

// maxPendingWorkerMarks bounds the marks waiting for the set's mapping file. A
// recording that never writes one (keploy record without --sync) would
// otherwise hold every mark a rule makes for as long as it runs.
const maxPendingWorkerMarks = 10000

// newWorkerMarks is the marks of a recording under rules, into db: nil,
// marking nothing, when there are none, when db cannot add to a set's worker
// scope, and under the proven switch, which places the worker scope by its
// own rules (windowMappings).
func newWorkerMarks(rules []models.WorkerMockRule, db MappingDb, logger *zap.Logger) *workerMarks {
	if len(rules) == 0 {
		return nil
	}
	if _, ok := db.(workerAdder); !ok {
		logger.Warn("worker-mock rules are not applied: this recording's mapping store cannot mark mocks in a test set's worker scope",
			zap.Int("rules", len(rules)))
		return nil
	}
	if provenswitch.On() {
		logger.Info("worker-mock rules are not applied under the proven switch: it places the set's worker scope by its own rules",
			zap.Int("rules", len(rules)))
		return nil
	}
	return &workerMarks{rules: rules, logger: logger}
}

// add marks m when a rule matches it. m is on disk: a worker entry must name a
// mock that is.
func (w *workerMarks) add(m *models.Mock) {
	if w == nil || m == nil || m.Name == "" {
		return
	}
	id, ok := models.MatchWorkerMockRules(w.rules, m)
	if !ok {
		return
	}
	e := models.MockEntry{
		Name:             m.Name,
		Kind:             string(m.GetKind()),
		Timestamp:        m.Spec.ReqTimestampMock.Unix(),
		ReqTimestampMock: models.FormatMockTimestamp(m.Spec.ReqTimestampMock),
		ResTimestampMock: models.FormatMockTimestamp(m.Spec.ResTimestampMock),
		Reason:           models.WorkerRuleReason(id),
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) >= maxPendingWorkerMarks {
		if !w.overflowed {
			w.overflowed = true
			w.logger.Warn("worker-mock rules matched more mocks than can wait for the test set's mapping file; later matches are not marked",
				zap.Int("waiting", len(w.pending)),
				zap.String("next_step", "record with --sync, so the mapping file is written as the recording goes"))
		}
		return
	}
	w.pending = append(w.pending, e)
}

// workerAdder is the mapping store's AddToWorker.
type workerAdder interface {
	AddToWorker(ctx context.Context, testSetID string, entries []models.MockEntry) ([]string, error)
}

// flush writes the marks waiting to testSetID's worker scope. A write that
// fails keeps them for the next flush; a store that cannot add to the worker
// scope, or a set whose mapping file does not exist yet, keeps them too.
// final says this is the recording's last flush: what it cannot write then
// is lost, and said so.
func (w *workerMarks) flush(ctx context.Context, db MappingDb, testSetID string, final bool) {
	if w == nil {
		return
	}
	w.mu.Lock()
	entries := w.pending
	w.pending = nil
	w.mu.Unlock()
	if len(entries) == 0 {
		return
	}
	keep := func(why string, err error) {
		if final {
			w.logger.Warn("mocks a worker-mock rule matched were not added to the test set's worker scope; a test that reaches a cold worker at replay may miss their calls",
				zap.String("testSetID", testSetID), zap.Int("mocks", len(entries)), zap.String("why", why), zap.Error(err),
				zap.String("next_step", "worker-mock rules need the test set's mappings.yaml: record with --sync (keploy record writes it only then), or mark the mocks as worker mocks on the recorded set"))
			return
		}
		w.mu.Lock()
		w.pending = append(entries, w.pending...)
		w.mu.Unlock()
	}
	adder, ok := db.(workerAdder)
	if !ok {
		// newWorkerMarks made none for such a store.
		return
	}
	if exists, err := mappingFileExists(ctx, db, testSetID); err != nil || !exists {
		keep("the test set has no mapping file yet", err)
		return
	}
	added, err := adder.AddToWorker(ctx, testSetID, entries)
	if err != nil {
		keep("writing the worker scope failed", err)
		return
	}
	w.mu.Lock()
	w.written += len(added)
	w.mu.Unlock()
	if len(added) > 0 {
		w.logger.Debug("added mocks worker-mock rules matched to the worker scope",
			zap.String("testSetID", testSetID), zap.Strings("mocks", added))
	}
}

// mappingFileExists reports whether testSetID has a mapping file, for a store
// that can say; a store that cannot is taken to have one.
func mappingFileExists(ctx context.Context, db MappingDb, testSetID string) (bool, error) {
	if e, ok := db.(interface {
		Exists(ctx context.Context, testSetID string) (bool, error)
	}); ok {
		return e.Exists(ctx, testSetID)
	}
	return true, nil
}

// lost says, once the recording ends, that the marks still waiting are not
// written, and why.
func (w *workerMarks) lost(testSetID, why string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	n := len(w.pending)
	w.pending = nil
	w.mu.Unlock()
	if n == 0 {
		return
	}
	w.logger.Warn("mocks a worker-mock rule matched were not added to the test set's worker scope; a test that reaches a cold worker at replay may miss their calls",
		zap.String("testSetID", testSetID), zap.Int("mocks", n), zap.String("why", why),
		zap.String("next_step", "re-record the test set, or mark the mocks as worker mocks on the recorded set"))
}

// count is how many marks were written to the worker scope.
func (w *workerMarks) count() int {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.written
}
