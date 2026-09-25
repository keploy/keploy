// Package record provides functionality for recording and managing test cases and mocks.
package record

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/telemetry"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const (
	// mappingDrainGrace hard-caps how long recording shutdown waits for the agent
	// to flush the test<->mock mappings it has already resolved. Bounded so a
	// wedged agent cannot hang exit, and kept under the 30s DrainErrGroup budget
	// that Start's teardown gives the group this drain runs in — overshooting that
	// would trade a lost tail for a teardown timeout, which is no better.
	mappingDrainGrace = 15 * time.Second

	// mappingFlushBatch is how many mappings accumulate before mappings.yaml is
	// rewritten. Each rewrite re-encodes the whole file, so writing per mapping is
	// quadratic (368 tests: 164us for the first, 19.45ms for the last, ~2.7s of
	// pure rewriting) — slow enough to back-pressure the agent's mapping stream,
	// which then DROPS what it cannot hand over. Batching keeps the consumer far
	// ahead of the stream.
	mappingFlushBatch = 32

	// mappingFlushInterval bounds how long a partial batch waits, so a long
	// recording still persists mappings as it goes instead of holding them all in
	// memory until the stream closes.
	mappingFlushInterval = 2 * time.Second

	// mappingIdleGrace ends the shutdown drain once the mapping stream falls idle.
	// The agent holds the stream open for the whole session, so idleness — not EOF
	// — is what signals the tail is through. Sized well above the agent's
	// per-mapping flush latency so a slow flush is not mistaken for completion.
	mappingIdleGrace = 3 * time.Second

	// afterRecordingHookTimeout bounds the end-of-recording hook (issue #1867) so a
	// wedged consumer cannot hang teardown. Matches the 30s DrainErrGroup budgets in
	// the same teardown defer. It is the hook context's only deadline (WithoutCancel
	// drops the upstream one), and consumers are expected to honor it.
	afterRecordingHookTimeout = 30 * time.Second
)

// stopTimer disarms t, draining its channel if it had already fired, so a later
// Reset starts from a clean state.
func stopTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}

// resetTimer re-arms t for d, safely draining any pending fire first.
func resetTimer(t *time.Timer, d time.Duration) {
	stopTimer(t)
	t.Reset(d)
}

// mergeMockEntries unions incoming entries into existing ones by mock name,
// preserving recorded order. Mirrors the mapping store's merge: a test's mapping
// arrives in more than one piece and the later pieces are deltas, so replacing
// would delete mocks the earlier piece recorded.
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

// consumeMappings correlates each test<->mock mapping the agent streams to its
// real mock entries and persists it to mappings.yaml. It runs until mappings is
// closed by the producer, and returns only then — ctx bounds nothing here by
// design.
//
// Mappings are the last artifact a recorded test produces (the agent resolves a
// test's mock range only once that test is done), so the tail of every endpoint
// arrives while recording is already shutting down. Writes therefore run on a
// context detached from ctx: on a cancelled one the yaml store refuses every
// write and those tests vanish from mappings.yaml, which replay then reports as
// no_mocks. Detaching here rather than relying on the caller keeps that
// guarantee local to the loop that depends on it — see the persistCtx note in
// Start for the same requirement on the test-case and mock stores.
// shortPoolByTest is required, not optional: it is how the one data-loss path
// this function deliberately does not revoke stays visible. It maps a test name
// to the number of its mocks that never correlated. Totals are computed at
// teardown rather than accumulated here, because a test can also be revoked by
// the AGENT (a RevokedTests control frame this function never sees) and a
// revoked test is deleted, not shipped with a short pool.
func (r *Recorder) consumeMappings(ctx context.Context, testSetID string, mappings <-chan models.TestMockMapping, correlationMap, asyncMockIDs *sync.Map, droppedMockIDs *droppedMockSet, revokeTest func(string), shortPoolByTest *sync.Map) error {
	persistCtx := context.WithoutCancel(ctx)

	// pending batches mappings so the file is rewritten once per batch instead of
	// once per test. flushMu guards it because the ticker below flushes too.
	pending := make(map[string][]models.MockEntry, mappingFlushBatch)
	var flushMu sync.Mutex

	// Both are touched only by this loop (the ticker goroutine below flushes
	// `pending` under flushMu and never reads these), so plain maps are safe.
	revokedHere := map[string]struct{}{}

	flush := func() {
		flushMu.Lock()
		defer flushMu.Unlock()
		if len(pending) == 0 {
			return
		}
		if err := r.mappingDb.UpsertBatch(persistCtx, testSetID, pending); err != nil {
			// Deliberately not utils.LogError: it suppresses context.Canceled,
			// which is exactly the class this write used to fail with, so a lost
			// batch left no trace at all. A mapping that goes missing here is a
			// no_mocks failure at replay — it must never be silent again.
			names := make([]string, 0, len(pending))
			for tn := range pending {
				names = append(names, tn)
			}
			sort.Strings(names)
			r.logger.Error("failed to save mappings",
				zap.Strings("tests", names),
				zap.Error(err),
				zap.String("next_step", "these tests' mocks will be missing from mappings.yaml and replay will report no_mocks for them; re-record the test set"))
		}
		clear(pending)
	}

	// A partial batch must not sit in memory until the stream closes: recording
	// can run for hours, and an operator watching mappings.yaml should see it
	// grow. The ticker bounds how long a mapping stays unpersisted without
	// putting a file rewrite on the per-mapping path.
	ticker := time.NewTicker(mappingFlushInterval)
	defer ticker.Stop()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				flush()
			}
		}
	}()

	for mapping := range mappings {
		realMockEntries, lostMock, uncorrelated := r.resolveMappingEntries(mapping, correlationMap, asyncMockIDs, droppedMockIDs)

		// Persisted anyway (see resolveMappingEntries), so record that this test
		// reaches replay with an incomplete mock set.
		//
		// Counted per DISTINCT TEST, not per mapping: the agent emits a test's
		// mocks as several delta mappings (see the merge note below), so a
		// per-mapping counter would report more "tests" than the set contains.
		// A revoked test is excluded — it is deleted at finalize, so it never
		// reaches replay with a short pool at all.
		if uncorrelated > 0 && !lostMock {
			if _, revoked := revokedHere[mapping.TestName]; !revoked {
				prev, _ := shortPoolByTest.Load(mapping.TestName)
				n, _ := prev.(int64)
				shortPoolByTest.Store(mapping.TestName, n+int64(uncorrelated))
			}
		}

		// This test owned at least one mock we could not persist. Replaying it
		// with a short pool produces a guaranteed failure that looks like a
		// product regression, so revoke it the same way the agent's own
		// capacity-drop path does (RevokedTests) and let finalize delete it.
		if lostMock {
			revokedHere[mapping.TestName] = struct{}{}
			// Drop whatever an EARLIER delta recorded for this test. The revoke
			// can arrive after a delta that saw only a correlation timeout, and a
			// deleted test never reaches replay with a short pool at all —
			// leaving the entry in place inflates the very metric whose
			// credibility is the reason the timeout path is not revoked.
			// (Teardown applies the same exclusion for the agent's own revokes,
			// keyed on tests it actually managed to DELETE.)
			shortPoolByTest.Delete(mapping.TestName)
			if revokeTest != nil {
				revokeTest(mapping.TestName)
			}
		}

		if len(realMockEntries) == 0 {
			continue
		}

		flushMu.Lock()
		// Union, don't replace: the agent emits a test's mocks when its window
		// resolves and emits more later for mocks retroactively binned into that
		// window. Those later emissions are a DELTA — overwriting would delete the
		// mocks already recorded for the test and replay it with a short pool.
		pending[mapping.TestName] = mergeMockEntries(pending[mapping.TestName], realMockEntries)
		full := len(pending) >= mappingFlushBatch
		flushMu.Unlock()

		// Flush on size only. Draining the channel is the priority: every
		// millisecond spent rewriting mappings.yaml is a millisecond the agent
		// cannot hand over its next mapping, and it DROPS what it cannot hand
		// over. The ticker flushes whatever a partial batch leaves behind.
		if full {
			flush()
		}
	}

	// The stream is closed: persist whatever the last partial batch holds.
	flush()
	return nil
}

type Recorder struct {
	logger          *zap.Logger
	testDB          TestDB
	mockDB          MockDB
	mappingDb       MappingDb
	telemetry       Telemetry
	instrumentation Instrumentation
	testSetConf     TestSetConfig
	config          *config.Config
	hooks           RecordHooks

	// cleanups is run in LIFO order from Start's defer. Downstream
	// builds and internal subsystems register shutdown work here
	// rather than each one type-asserting its own io.Closer into the
	// store interfaces. Register with RegisterCleanup.
	cleanupMu sync.Mutex
	cleanups  []func() error
}

// RegisterCleanup appends a shutdown callback that Recorder.Start's
// defer will drain in LIFO order. Thread-safe. Callbacks should be
// idempotent — Recorder may be restarted in some flows.
func (r *Recorder) RegisterCleanup(fn func() error) {
	if fn == nil {
		return
	}
	r.cleanupMu.Lock()
	r.cleanups = append(r.cleanups, fn)
	r.cleanupMu.Unlock()
}

func New(logger *zap.Logger, testDB TestDB, mockDB MockDB, mappingDB MappingDb, telemetry Telemetry, instrumentation Instrumentation, testSetConf TestSetConfig, hooks RecordHooks, config *config.Config) Service {
	if hooks == nil {
		hooks = BaseRecordHooks{}
	}
	return &Recorder{
		logger:          logger,
		testDB:          testDB,
		mockDB:          mockDB,
		mappingDb:       mappingDB,
		telemetry:       telemetry,
		instrumentation: instrumentation,
		testSetConf:     testSetConf,
		config:          config,
		hooks:           hooks,
	}
}

// afterRecordingComplete invokes the end-of-recording hook (issue #1867). It is
// best-effort: the recording is already saved, so neither an error NOR a panic
// from the hook may propagate — a panic here would otherwise unwind the teardown
// defer and skip the deferred-orphan-revoke that runs right after this call. The
// hook does data-driven work (the Basic-Auth re-key), so a panic is plausible;
// recover, log, and let teardown continue.
func (r *Recorder) afterRecordingComplete(ctx context.Context, testSetID string) {
	defer func() {
		if rec := recover(); rec != nil {
			r.logger.Error("AfterRecordingComplete hook panicked; recording is saved but the post-record pass did not finish. Check your RecordHooks implementation.",
				zap.Any("panic", rec), zap.String("testSetID", testSetID))
		}
	}()
	// The recorder ctx is already cancelled on the normal SIGINT stop of an
	// interactive recording (the teardown defer above runs under a cancelled ctx).
	// Decouple the hook from that cancellation (context.WithoutCancel) so a
	// legitimate post-record pass is not skipped — but re-impose a fresh bound so a
	// wedged hook cannot hang teardown, matching the 10s NotifyGracefulShutdown and
	// 30s drain bounds in this same defer. WithoutCancel keeps request-scoped values
	// while dropping the upstream cancellation AND deadline, so this WithTimeout is
	// the hook's only deadline; consumers are expected to honor it (the enterprise
	// re-key does — keploy/enterprise#2536).
	hookCtx, cancelHook := context.WithTimeout(context.WithoutCancel(ctx), afterRecordingHookTimeout)
	defer cancelHook()
	if hookErr := r.hooks.AfterRecordingComplete(hookCtx, &RecordingCompleteContext{
		TestSetID: testSetID,
		Path:      r.config.Path,
	}); hookErr != nil {
		r.logger.Error("AfterRecordingComplete hook failed; recording is saved but a post-record pass may be incomplete. Check your RecordHooks implementation.",
			zap.Error(hookErr), zap.String("testSetID", testSetID))
	}
}

// SetRecordHooks replaces the current hooks. Mirrors SetTestHooks on the Replayer.
func (r *Recorder) SetRecordHooks(hooks RecordHooks) {
	if hooks != nil {
		r.hooks = hooks
	}
}

// maxLiveDroppedMockIDs bounds droppedMockSet. Every dropped mock is meant to
// be consumed by the one mapping that references it, but a mock nothing ever
// maps — a background egress whose owning test never completes — has no
// consumer, so an unbounded set would grow for the life of the recording.
//
// Past the cap we stop tracking, and BOTH benefits lapse for further drops:
// their owning tests are no longer revoked, and their mappings go back to
// burning the full ~500ms correlation spin. That is the deliberate trade — a
// recording with 10k dropped mocks is systematically broken and the teardown
// Warn plus the mocks-dropped telemetry still report the true scale — but it is
// not free, so the cap is set well above any plausible healthy session.
const maxLiveDroppedMockIDs = 10000

// maxLiveBenignMockIDs bounds the BENIGN half of droppedMockSet separately, and
// that separation is the point. Benign entries come from the highest-frequency
// path in the recorder — a poll lane collapses most of its cycles by design —
// while data-loss entries are rare. On a shared budget the benign traffic would
// fill the set within minutes and `add` would then fail closed for real drops,
// silently disabling the revoke this whole change exists to provide. Benign
// entries are also not reliably consumed: several classes of mock reach the
// recorder with no mapping referencing them at all, so nothing ever takes them.
// Package-level var, not a const, only so a test can shrink it to prove that
// the correctness guarantee (asyncMockIDs) holds when this best-effort cache
// declines an entry. Never reassigned in production code.
var maxLiveBenignMockIDs int64 = 10000

// droppedMockSet is the set of mock tempIDs that will never reach
// correlationMap, handed to the mapping consumer so it can skip the correlation
// spin for them instead of burning ~500ms each.
//
// Entries carry whether the absence is DATA LOSS or intentional, because the
// two demand opposite handling. A mock we failed to persist means its owning
// test must be revoked. A mock we chose not to persist — a hook-collapsed async
// poll cycle — is normal operation: revoking there would delete healthy tests
// and counting it would fire a data-loss alarm on every async-poll recording.
//
// take() removes the ID as it reports it: entries are consumed exactly once,
// which is what keeps the set from growing across a multi-day recording.
type droppedMockSet struct {
	ids sync.Map // map[string]bool — value: true = data loss, false = intentional
	// Separate budgets: see maxLiveBenignMockIDs.
	liveLost   atomic.Int64
	liveBenign atomic.Int64
}

func (d *droppedMockSet) store(tempID string, lost bool) bool {
	counter, limit := &d.liveBenign, maxLiveBenignMockIDs
	if lost {
		counter, limit = &d.liveLost, int64(maxLiveDroppedMockIDs)
	}
	if counter.Load() >= limit {
		return false
	}
	if _, loaded := d.ids.LoadOrStore(tempID, lost); !loaded {
		counter.Add(1)
	}
	return true
}

// add records a mock the recorder FAILED to persist. It reports whether the ID
// was tracked; false means the set is at capacity and the owning test will NOT
// be revoked.
func (d *droppedMockSet) add(tempID string) bool { return d.store(tempID, true) }

// addBenign records a mock the recorder intentionally did not persist. The
// owning test is left alone — only the correlation spin is short-circuited.
func (d *droppedMockSet) addBenign(tempID string) bool { return d.store(tempID, false) }

// take reports whether tempID is in the set and, if so, whether its absence is
// data loss. It removes the entry.
func (d *droppedMockSet) take(tempID string) (present, lost bool) {
	v, ok := d.ids.LoadAndDelete(tempID)
	if !ok {
		return false, false
	}
	if v.(bool) {
		d.liveLost.Add(-1)
	} else {
		d.liveBenign.Add(-1)
	}
	return true, v.(bool)
}

// skippedMockMarker tags an asyncMockIDs entry that exists ONLY because a
// RecordHook asked us not to persist the mock (a collapsed async poll cycle).
// Ordinary async-egress mocks are stored as struct{}{}.
//
// The distinction exists so the entry can be freed. asyncMockIDs is otherwise
// never pruned, and skips are the highest-frequency event in a poll-heavy
// recording — at ~105 bytes per entry, a single 100ms lane would retain
// hundreds of MB for the life of the session, which is an OOM risk in the
// DaemonSet embedding where Start is re-entered per session.
//
// The release is best-effort, not a bound: it happens when a mapping consumes
// the tempID, so a run with mappings disabled (--disable-mapping), or a skipped
// mock no mapping ever references, retains its entry for the whole session.
// Those runs are back to the pre-marker growth; the marker helps the common
// case, it does not cap the worst one.
type skippedMock struct{}

var skippedMockMarker = skippedMock{}

// markSkippedMock records a mock a RecordHook asked us not to persist, in both
// places the mapping consumer looks.
//
// Extracted so the marker choice is directly assertable. It is the load-bearing
// line: storing a plain struct{}{} here instead of skippedMockMarker still
// satisfies every behavioural test — the entry is simply never released again,
// and the leak returns silently.
func markSkippedMock(asyncMockIDs *sync.Map, dropped *droppedMockSet, tempID string) {
	// CORRECTNESS, unconditional: Skip means "never persisted", so this tempID
	// must never reach a per-test mapping and must never read as loss. It cannot
	// be gated on mock.IsAsync() — AsyncRecorder sets Skip and returns BEFORE
	// assigning Spec.Async, so a collapsed cycle reports IsAsync()==false and the
	// guard would be a no-op.
	asyncMockIDs.Store(tempID, skippedMockMarker)
	// SPEED, best-effort: short-circuits the ~500ms correlation spin. Capped, so
	// it eventually declines entries; that costs latency, never correctness.
	dropped.addBenign(tempID)
}

// releaseSkippedMock frees a SKIP-marked asyncMockIDs entry once a mapping has
// consumed it. Ordinary async entries are deliberately left in place.
//
// The asymmetry is not fussiness. A real async mock IS in correlationMap, so if
// a later mapping referenced a pruned entry it would resolve and be appended —
// leaking a background egress into a test's mock pool. A skipped mock was never
// persisted, so a later mapping referencing a pruned entry simply fails to
// correlate: no revoke, no leak, at worst one spin and one counter tick.
func releaseSkippedMock(asyncMockIDs *sync.Map, tempID string, v any) {
	if _, skipped := v.(skippedMock); skipped {
		asyncMockIDs.Delete(tempID)
	}
}

// resolveMappingEntries correlates a test's mapping tempIDs to the persisted
// MockEntries in correlationMap, dropping each consumed tempID as it goes. It
// EXCLUDES async-egress mocks (those in asyncMockIDs): they are served at replay
// by the async engine from the complete corpus, so they must never be recorded
// as a testcase's per-test mock — even when a background egress's timestamp
// happened to fall inside the testcase's request window (the agent's mapper bins
// purely by timestamp and has no async awareness). The Mock loop stores the
// async marker before it publishes the tempID to correlationMap, so a resolved
// tempID always has its async status already decided.
func (r *Recorder) resolveMappingEntries(mapping models.TestMockMapping, correlationMap, asyncMockIDs *sync.Map, droppedMockIDs *droppedMockSet) ([]models.MockEntry, bool, int) {
	var realMockEntries []models.MockEntry
	droppedOwner := false
	uncorrelated := 0
	for _, tempID := range mapping.MockIDs {
		// Async-egress mocks are deliberately excluded from per-test mappings
		var realEntry models.MockEntry
		found := false
		dropped := false
		lostMock := false
		// Simple retry loop (fast spin) to wait for the Mock Loop.
		for i := 0; i < 50; i++ { // Wait up to ~500ms
			if val, ok := correlationMap.Load(tempID); ok {
				realEntry = val.(models.MockEntry)
				found = true
				break
			}
			// A mock we gave up persisting will NEVER reach correlationMap, so
			// without this the spin burns its full ~500ms per dropped mock —
			// serialized in the single mapping consumer, which the agent cannot
			// outrun: it DROPS mappings it cannot hand over, so the stall would
			// corrupt unrelated healthy tests.
			//
			// The check MUST be inside the spin. Mocks and mappings arrive on
			// two independent channels drained by two independent goroutines —
			// that race is the whole reason the spin exists — so a mapping
			// routinely arrives BEFORE its mock is even attempted. Checking
			// once up front loses to that ordering every time: the test is
			// silently persisted with a short mock pool AND the full stall is
			// still paid.
			if present, lost := droppedMockIDs.take(tempID); present {
				// A benign skip (hook-collapsed poll cycle) short-circuits the
				// spin but is NOT loss: it must not revoke and must not be
				// counted as a short pool.
				dropped = true
				lostMock = lost
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if dropped {
			// Async status is decided HERE, not earlier. The mock loop stores
			// the async marker before it attempts the insert, so by the time a
			// drop is recorded the marker is already published — but a mapping
			// routinely reaches this loop before either happens, so a check made
			// up front would simply miss. Getting this wrong DELETES healthy
			// tests: async-egress mocks are excluded from per-test mappings by
			// design (see the doc above), so a dropped one is never a short pool
			// for anybody, yet without this it would set droppedOwner and revoke
			// the test that merely overlapped it in time.
			if v, isAsync := asyncMockIDs.Load(tempID); isAsync {
				releaseSkippedMock(asyncMockIDs, tempID, v)
				continue
			}
			if lostMock {
				droppedOwner = true
			}
			continue
		}
		if !found {
			// Deliberately NOT treated as a drop. A drop is a fact — we know the
			// mock was abandoned. A correlation timeout is an inference: the mock
			// may well have been persisted and simply not observed inside 500ms,
			// and revoking on that guess would convert a latency event into
			// deterministic deletion of a customer's recorded test. So the test
			// is still persisted, with a short pool, exactly as before.
			//
			// It is counted, though. Persisting a test whose mock set is
			// incomplete is real data loss whatever the cause, and it was
			// previously visible only as a per-occurrence log line — no total, so
			// nobody could say how often it fires. The count is reported at
			// teardown and in telemetry so the frequency is measurable BEFORE
			// anyone argues about changing the semantics.
			// Third and last settle point. If the async marker landed at any
			// point during the spin, this tempID's absence is by design and must
			// not be reported as an incomplete mock set — async-egress mocks are
			// never part of a per-test mapping. Only a tempID we still cannot
			// classify after the full wait is real, unexplained loss.
			if v, isAsync := asyncMockIDs.Load(tempID); isAsync {
				releaseSkippedMock(asyncMockIDs, tempID, v)
				continue
			}
			uncorrelated++
			r.logger.Error("Failed to correlate mock mapping",
				zap.String("test", mapping.TestName),
				zap.String("tempMockID", tempID),
				zap.String("next_step", "ensure mapping store is enabled, avoid high parallelism, or re-record if mappings are inconsistent"))
			continue
		}
		correlationMap.Delete(tempID)
		// The other settle point. The doc's invariant — the mock loop stores the
		// async marker before it publishes the tempID to correlationMap — makes
		// this the moment async status is knowable for a RESOLVED mock. Checking
		// only before the spin would let an async mock whose marker landed mid-
		// spin resolve normally and leak into the per-test mapping, which is
		// exactly what this exclusion exists to prevent.
		if v, isAsync := asyncMockIDs.Load(tempID); isAsync {
			releaseSkippedMock(asyncMockIDs, tempID, v)
			continue
		}
		realMockEntries = append(realMockEntries, realEntry)
	}
	return realMockEntries, droppedOwner, uncorrelated
}

// GetRecordHooks returns the current hooks.
func (r *Recorder) GetRecordHooks() RecordHooks {
	return r.hooks
}

func (r *Recorder) Start(ctx context.Context) error {

	r.logger.Debug("Starting Keploy recording... Please wait.")

	sessionStart := time.Now()

	// Auto-register mockDB.Close if it implements io.Closer. MockYaml
	// implements Close unconditionally: in gob mode it drains the
	// async writer and flushes the file; in yaml mode it is a no-op
	// (gobStop is nil, so Close returns nil immediately). Registered
	// via RegisterCleanup so it drains in LIFO order alongside any
	// other subsystems the caller has registered (telemetry flush,
	// mapping-DB sync, etc.).
	if closer, ok := r.mockDB.(io.Closer); ok {
		r.RegisterCleanup(closer.Close)
	}

	// Drain all registered cleanups in LIFO order on return. Errors
	// are logged but do not abort subsequent cleanups — each subsystem
	// gets its flush regardless of what came before.
	defer func() {
		r.cleanupMu.Lock()
		cleanups := r.cleanups
		r.cleanups = nil
		r.cleanupMu.Unlock()
		for i := len(cleanups) - 1; i >= 0; i-- {
			if err := cleanups[i](); err != nil {
				// A cleanup error usually means a mock-file flush or
				// telemetry-drain returned non-nil — the session data
				// is still on disk (the writer buffers drain before
				// Close returns err on inner failures), but the tail
				// batch may be incomplete. Log at Error so the
				// operator sees it on exit summary; include cleanup
				// index so `record cleanup N failed` is actionable
				// against the known RegisterCleanup call sites.
				r.logger.Error("record cleanup returned an error; inspect the mock file for a truncated tail and check disk space/permissions at the recording output directory",
					zap.Int("cleanupIndex", i),
					zap.Error(err))
			}
		}
	}()

	// creating error group to manage proper shutdown of all the go routines and to propagate the error to the caller
	errGrp, _ := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, errGrp)

	// persistCtx is the context the test-case and mock stores are written on,
	// detached from ctx on purpose. (consumeMappings makes the same guarantee for
	// mappings.yaml itself.)
	//
	// Capture and persistence are driven by different contexts, and the gap
	// between them is where recorded data used to disappear. The agent streams on
	// reqCtx (see below), which is WithoutCancel'd and so survives SIGINT — it
	// stops only at reqCtxCancel() in teardown, AFTER the user app has been
	// stopped and drained. The consumer goroutines below, however, are children of
	// ctx, which cancels the instant SIGINT lands. So for the whole teardown
	// window (a NotifyGracefulShutdown of up to 10s, then an app drain of up to
	// 30s) the agent kept handing us test cases, mocks and mappings while every
	// store refused to write them: the yaml path returns ctx.Err() on a cancelled
	// ctx, and the callers below treated that as "shutting down, skip" — silently.
	//
	// The damage was worst for mappings, which are emitted LAST (the agent can
	// only resolve a test's mock range once that test is done), so the tail of
	// every endpoint landed squarely in that window: mappings.yaml lost those
	// tests and replay reported "no_mocks" (candidates: 0) for exactly them. A
	// dropped mock insert compounded it by also skipping its correlationMap entry,
	// stranding the mapping that referenced it even when the mapping did arrive.
	//
	// Writes must therefore outlive cancellation. This does NOT risk a hung
	// teardown: the consumers are `range` loops that end when the agent closes its
	// streams (at reqCtxCancel), each write is a local file op, and Start's
	// teardown still bounds the whole group with DrainErrGroup.
	persistCtx := context.WithoutCancel(ctx)

	/*
	 * THE PORTS INGRESS WAS OBSERVED ON, for the UI join annotation.
	 *
	 * Declared here rather than inside the consumer loop because the
	 * annotation is written from the stop defer below, which needs the
	 * whole session's observations — and because the loop runs in its
	 * own goroutine, so the collector is the thing that carries them
	 * across. It is safe for concurrent use because it holds a mutex;
	 * the goroutine is the reason it NEEDS to be safe, not the reason it
	 * is.
	 *
	 * Costs an ordinary recording AT MOST one mutex and one map insert
	 * per test case -- observe() consults nothing about whether a join
	 * was requested, but it does return before the mutex when AppPort is
	 * 0 -- plus one sorted() at stop, which takes the mutex and, on a
	 * recording that saw ingress, allocates and sorts, and one
	 * context.WithTimeout plus its cancel. The defer then returns
	 * immediately because no join was requested.
	 */
	uiJoinObserved := newUIJoinPorts()

	runAppErrGrp, _ := errgroup.WithContext(ctx)
	runAppCtx := context.WithoutCancel(ctx)
	runAppCtx, runAppCtxCancel := context.WithCancel(runAppCtx)

	setupErrGrp, _ := errgroup.WithContext(ctx)
	setupCtx := context.WithoutCancel(ctx)
	setupCtx, setupCtxCancel := context.WithCancel(setupCtx)
	setupCtx = context.WithValue(setupCtx, models.ErrGroupKey, setupErrGrp)

	// Propagate parent context cancellation to setupCtx
	// This ensures that when Ctrl+C is pressed, setupCtx is cancelled immediately
	go func() {
		<-ctx.Done()
		setupCtxCancel()
	}()

	reqErrGrp, _ := errgroup.WithContext(ctx)
	reqCtx := context.WithoutCancel(ctx)
	reqCtx, reqCtxCancel := context.WithCancel(reqCtx)
	reqCtx = context.WithValue(reqCtx, models.ErrGroupKey, reqErrGrp)

	var stopReason string
	// defining all the channels and variables required for the record
	var runAppError models.AppError
	var appErrChan = make(chan models.AppError, 1)
	var insertTestErrChan = make(chan error, 10)
	var insertMockErrChan = make(chan error, 10)
	var newTestSetID string
	var err error
	var testCount = 0
	var mockCountMap = make(map[string]int)
	// droppedMockCount / droppedMockKinds are the missing half of mockCountMap,
	// which only ever counts successes. A mock whose own payload cannot be
	// encoded is now skipped rather than fatal (see the ErrMockEncode branch in
	// the consumer below), and a skipped mock must never be silent — these feed
	// the teardown log and the RecordedTestSuite telemetry.
	var droppedMockCount = 0
	// shortPoolByTest maps a test name to how many of its mocks never
	// correlated, i.e. tests persisted with an INCOMPLETE mock set (see the
	// !found branch in resolveMappingEntries). Unlike a dropped mock this does
	// not revoke the test, so without a total the loss shows up only as
	// scattered per-occurrence log lines. Kept per-test rather than
	// pre-aggregated so the teardown block can subtract EVERY revoke path — the
	// agent's RevokedTests control frame revokes tests consumeMappings never
	// sees, and a revoked test is deleted rather than shipped short.
	var shortPoolByTest sync.Map
	var droppedMockKinds = make(map[string]int)
	// mockCountMap is written by the mock-consumer goroutine and read at
	// teardown for telemetry. Guard both with a mutex so that if teardown
	// ever runs while the consumer is still draining (e.g. a force-shutdown),
	// it can't trigger a concurrent map read/write. correlationMap beside it
	// is already a sync.Map.
	var mockCountMapMu sync.Mutex
	domainSet := telemetry.NewDomainSet()
	var recordingStarted bool
	/*
	 * WHEN CAPTURE ACTUALLY BEGAN, which is not when Start was entered.
	 *
	 * This is T0WallMs for the UI join annotation, which
	 * pkg/models/uijoin.go asks to be good enough "for skew estimation
	 * only" -- the part of that field doc that makes startup time an
	 * ERROR rather than a matter of taste. Deliberately not quoting the
	 * rest of it. `sessionStart` is stamped at the
	 * top of Start(), before instrumentation setup and agent bring-up
	 * (AgentReadyTimeout defaults to 330s) — tens of seconds in a
	 * docker-compose recording, where the user's app boot falls inside
	 * that window too. (For every other command type instrumentation.Run
	 * starts the app AFTER this stamp, so app boot is not excluded;
	 * pkg/models/uijoin.go's field doc says so.) Handing `sessionStart`
	 * to a skew estimate puts the whole of startup into the skew, in a
	 * model that spends twenty-four lines refusing a float32 BECAUSE it
	 * introduces 24 seconds of error. The recorder knows the better
	 * value; this is it.
	 *
	 * `sessionStart` keeps its own job (telemetry duration, which really
	 * is the whole session).
	 */
	var captureStart time.Time

	// Deferred-orphan revoke bookkeeping. revokedNames collects TC names the
	// agent signalled via Kind=RevokedTests control frames (their owned mock
	// was capacity-dropped AFTER the TC streamed); insertedNames records which
	// TCs actually persisted. At finalize we delete the intersection so a
	// revoke that raced ahead of its TC's persistence is still caught. Both are
	// written only by the mock/TC consumer goroutines and read at teardown
	// AFTER those goroutines drain, but the mutexes are cheap insurance against
	// a force-shutdown teardown racing an in-flight consumer.
	revokedNames := map[string]struct{}{}
	var revokedMu sync.Mutex
	insertedNames := map[string]struct{}{}
	var insertedMu sync.Mutex

	// defering the stop function to stop keploy in case of any error in record or in case of context cancellation
	defer func() {
		select {
		case <-ctx.Done():
		default:
			err := utils.Stop(r.logger, stopReason)
			if err != nil {
				utils.LogError(r.logger, err, "failed to stop recording")
			}
		}

		r.logger.Info("Stopping Keploy recording...")

		// Notify the agent that we are shutting down gracefully
		// This will cause connection errors to be logged as debug instead of error.
		// Bound it: an up-but-unresponsive agent (still booting under contention)
		// must not block teardown — the path SIGINT takes — indefinitely.
		notifyCtx, notifyCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := r.instrumentation.NotifyGracefulShutdown(notifyCtx); err != nil {
			r.logger.Debug("failed to notify agent of graceful shutdown", zap.Error(err))
		}
		notifyCancel()
		// Pcap + keylog flow over long-lived HTTP streams started in
		// Start() right after the agent's broadcaster came up. The
		// streams unwind on their own when the agent closes the
		// response or the recorder context is cancelled — there is
		// no on-disk file on the agent and no fetch step here.

		// Bounded drains: a goroutine wedged under contention that ignores its
		// cancel() must not hang teardown forever (which would swallow SIGINT and
		// keep the process alive until an external SIGKILL). See utils.DrainErrGroup.
		runAppCtxCancel()
		if err := utils.DrainErrGroup(r.logger, "record-app", runAppErrGrp, 30*time.Second); err != nil {
			utils.LogError(r.logger, err, "failed to stop application")
		}

		reqCtxCancel()
		if err := utils.DrainErrGroup(r.logger, "record-req", reqErrGrp, 30*time.Second); err != nil {
			utils.LogError(r.logger, err, "failed to stop request processing")
		}

		setupCtxCancel()
		if err := utils.DrainErrGroup(r.logger, "record-setup", setupErrGrp, 30*time.Second); err != nil {
			utils.LogError(r.logger, err, "failed to stop setup execution, that covers init container")
		}

		// recordDrainedCleanly gates the end-of-recording hook below. It stays true
		// only if this drain actually joined every persist goroutine; on the
		// timeout path a mock/test-case writer may still be in flight (see below).
		recordDrainedCleanly := true
		if err, timedOut := utils.DrainErrGroupStatus(r.logger, "record", errGrp, 30*time.Second); err != nil {
			utils.LogError(r.logger, err, "failed to stop recording")
		} else if timedOut {
			recordDrainedCleanly = false
		}

		// End-of-recording hook (issue #1867 Basic-Auth re-key): every test case
		// and mock is now drained to disk, so a cross-artifact pass can correlate
		// them. Best-effort — a failure never invalidates the recording.
		//
		// Gated on a CLEAN drain. DrainErrGroupStatus reports timedOut when a
		// persist goroutine ignored cancellation and may still be appending to the
		// test-set files (the insert goroutines write on persistCtx =
		// WithoutCancel, so they do NOT stop on the teardown cancel). The hook's
		// consumer rewrites a whole mock file (read → rewrite → atomic rename), so
		// a mock appended between its read and its rename would be silently dropped
		// into a valid-but-short file. Skipping the pass on the timeout path costs
		// one recording's post-record work; running it risks a truncated mock set.
		if recordingStarted && newTestSetID != "" {
			if recordDrainedCleanly {
				r.afterRecordingComplete(ctx, newTestSetID)
			} else {
				r.logger.Warn("skipping the end-of-recording hook: the record drain timed out, so a mock or test-case write may still be in flight; the post-record pass is skipped to avoid racing an unfinished write. The recording itself is saved.",
					zap.String("testSetID", newTestSetID))
			}
		}

		// Deferred-orphan revoke: delete TCs whose owned mock was capacity-dropped
		// AFTER the TC streamed (the agent signalled them live via RevokedTests
		// control frames). Applied here — after all inserts drained — so a revoke
		// that raced ahead of its TC's persistence is still caught by the intersect.
		// Snapshot both sets under their OWN locks before intersecting. On the
		// DrainErrGroup TIMEOUT path a wedged consumer goroutine can still be
		// writing these maps (DrainErrGroup returns after its timeout WITHOUT
		// joining a goroutine that ignores cancellation — see utils/drain.go),
		// so an unguarded read here could hit a fatal "concurrent map read and
		// map write". The two maps are never locked together elsewhere, so
		// snapshot each independently (lock, copy, unlock) — no nesting, no
		// ordering hazard.
		revokedMu.Lock()
		revokedSnap := make([]string, 0, len(revokedNames))
		for n := range revokedNames {
			revokedSnap = append(revokedSnap, n)
		}
		revokedMu.Unlock()

		// Names whose delete actually SUCCEEDED. A revoke is not always applied:
		// DeleteTests is reached by runtime assertion because it is deliberately
		// not on the record TestDB interface, and an individual delete can fail and
		// warn. A test that stays in the set IS shipped with a short mock pool, so
		// only a genuinely deleted one may be excluded from the totals below.
		deletedSet := map[string]struct{}{}

		var toDelete []string
		insertedMu.Lock()
		for _, n := range revokedSnap {
			if _, ok := insertedNames[n]; ok {
				toDelete = append(toDelete, n)
			}
		}
		insertedMu.Unlock()
		if len(toDelete) > 0 {
			// DeleteTests is on the concrete *testdb.TestYaml but NOT on the record
			// TestDB interface (enterprise implements that interface; don't break it).
			// Reach it by runtime assertion; degrade to a warning if unavailable.
			if td, ok := r.testDB.(interface {
				DeleteTests(ctx context.Context, testSetID string, testCaseIDs []string) error
			}); ok {
				deleted := 0
				for _, n := range toDelete {
					if err := td.DeleteTests(ctx, newTestSetID, []string{n}); err != nil {
						r.logger.Warn("deferred-orphan revoke: failed to delete a capacity-orphaned test case; leaving it in the set",
							zap.String("testSetID", newTestSetID), zap.String("testCase", n), zap.Error(err))
						continue
					}
					deletedSet[n] = struct{}{}
					deleted++
				}
				if deleted > 0 {
					// Guard under insertedMu so this decrement can't race the
					// (possibly still-live, on the drain-timeout path) testCount++
					// in the TC-insert loop, which now runs under the same lock.
					insertedMu.Lock()
					testCount -= deleted
					insertedMu.Unlock()
					r.logger.Info("deferred-orphan revoke: removed capacity-orphaned test cases whose mock was dropped after streaming",
						zap.Int("revoked", deleted), zap.String("testSetID", newTestSetID))
				}
			} else {
				r.logger.Warn("deferred-orphan revoke: testDB does not support DeleteTests; leaving orphaned test cases in the set",
					zap.Int("count", len(toDelete)))
			}
		}

		/*
		 * testCount, READ ONCE AND UNDER THE LOCK, for every consumer in
		 * this defer.
		 *
		 * There were three readers and only the annotation's was guarded
		 * -- the two telemetry calls below took it bare. On the
		 * DRAIN-TIMEOUT path DrainErrGroup returns without joining a
		 * wedged consumer, so the `testCount++` in the TC-insert loop can
		 * still be live while this defer runs: exactly the hazard the
		 * revokedNames/insertedNames note above documents, on a plain int
		 * instead of a map, where the race detector has nothing to trip
		 * on until it does.
		 *
		 * Reading once also makes the three consumers agree. Three
		 * separate reads of a value another goroutine may still be
		 * incrementing can report three different totals for one
		 * recording -- telemetry saying N, the session summary N+1, and
		 * the annotation refusing at 0.
		 *
		 * THE POSITION IS WHAT MAKES IT CORRECT. It has to sit BELOW the
		 * deferred-orphan revoke's `testCount -= deleted` and above every
		 * reader: above the decrement, telemetry over-reports revoked
		 * tests, and on an all-revoked recording the `testCount <= 0` arm
		 * stops firing and writes an annotation onto a test-set with no
		 * test cases -- the phantom that arm exists to prevent.
		 *
		 * PINNED by TestStart_AgentRevokedTestIsNotCountedAsShortPool,
		 * which records one test, has the agent revoke it, and asserts
		 * BOTH telemetry counts are 0. Measured: moving this read above
		 * the decrement makes both report 1. It was unpinned until
		 * recTelemetry stopped discarding the count argument -- a fake
		 * that throws a value away makes every test using it blind to
		 * that value.
		 *
		 * It is also STALER than the three bare reads it replaced, on
		 * one path: on the drain-timeout path a still-live `testCount++`
		 * between here and the annotation is now excluded from all
		 * three. That is the trade -- consistency over freshness, on a
		 * path where the count is arbitrary anyway -- and it is a cost,
		 * not a free win.
		 */
		insertedMu.Lock()
		testCountSnapshot := testCount
		insertedMu.Unlock()

		totalMocks := 0
		if recordingStarted {
			mockCountMapMu.Lock()
			mockCountSnapshot := make(map[string]int, len(mockCountMap))
			for k, v := range mockCountMap {
				mockCountSnapshot[k] = v
			}
			// Snapshot the drop counters under the same lock that guards them.
			droppedSnapshot := droppedMockCount
			droppedKindSnapshot := make(map[string]int, len(droppedMockKinds))
			for k, v := range droppedMockKinds {
				droppedKindSnapshot[k] = v
			}
			mockCountMapMu.Unlock()
			// Say it on the way out too, so an operator sees the holes even with
			// telemetry disabled.
			// Totalled here, not accumulated as we went, so every revoke path is
			// accounted for: revokedNames holds both consumeMappings' own revokes
			// and the agent's RevokedTests frames. Subtract on DELETED, not
			// merely revoked — a revoke that could not be applied leaves the test
			// in the set, still short, so it must stay in the count.
			shortPoolTests, shortPoolMocks := 0, 0
			shortPoolByTest.Range(func(k, v any) bool {
				name, _ := k.(string)
				if _, gone := deletedSet[name]; gone {
					return true
				}
				shortPoolTests++
				n, _ := v.(int64)
				shortPoolMocks += int(n)
				return true
			})
			if shortPoolTests > 0 {
				r.logger.Warn("recording finished with test cases whose mock set is INCOMPLETE: some mocks never correlated to their mapping, and unlike a dropped mock this does not revoke the test — these will fail at replay",
					zap.Int("testsWithShortMockPool", shortPoolTests),
					zap.Int("mocksUncorrelated", shortPoolMocks),
					zap.String("testSetID", newTestSetID),
					zap.String("next_step", "ensure the mapping store is enabled and reduce recording parallelism; re-record the test set if this count is non-trivial"))
			}
			if droppedSnapshot > 0 {
				r.logger.Warn("recording finished with mocks dropped; the tests that owned them were revoked rather than recorded with a short mock pool",
					zap.Int("mocksDropped", droppedSnapshot),
					zap.Any("byKind", droppedKindSnapshot),
					zap.String("testSetID", newTestSetID),
					zap.String("next_step", "check byKind: an unencodable payload is a parser/encoder bug worth reporting; drops during shutdown are usually I/O and re-recording recovers them"))
			}
			suiteMeta := map[string]interface{}{
				"host-domains": domainSet.ToSlice(),
			}
			// Report skipped mocks alongside the recorded ones. mockCountSnapshot
			// counts only successes, so without this a recording with holes is
			// indistinguishable from a clean one in telemetry.
			if droppedSnapshot > 0 {
				suiteMeta["mocks-dropped"] = droppedSnapshot
				suiteMeta["mocks-dropped-by-kind"] = droppedKindSnapshot
			}
			// Gated separately from the drops. The motivating case for these two
			// is a correlation timeout under parallelism with a perfectly
			// healthy encoder — zero drops — which is exactly the recording the
			// drop gate excludes, so folding them in there would leave the
			// frequency invisible in the one case they were added to measure.
			if shortPoolTests > 0 {
				suiteMeta["tests-short-pool"] = shortPoolTests
				suiteMeta["mocks-uncorrelated"] = shortPoolMocks
			}
			r.telemetry.RecordedTestSuite(newTestSetID, testCountSnapshot, mockCountSnapshot, suiteMeta)
			for _, c := range mockCountSnapshot {
				totalMocks += c
			}
		}
		// Emit the session summary on every graceful stop: either recording
		// actually started, OR an error path set a stopReason before frames
		// were established (setup/agent-not-ready/testset-lookup failures that
		// previously emitted nothing — the highest-signal "stuck" cases).
		// "completed" for a clean exit / user Ctrl+C, "aborted" when a
		// stopReason was set; stop_reason carries the categorized cause.
		if recordingStarted || stopReason != "" {
			status := "completed"
			if stopReason != "" {
				status = "aborted"
			}
			r.telemetry.RecordSessionCompleted(int64(testCountSnapshot), int64(totalMocks), time.Since(sessionStart).Milliseconds(), status, stopReason)
		}
		/*
		 * THE UI JOIN ANNOTATION, written once, at stop.
		 *
		 * Here and not at start, for two reasons that both come from
		 * pkg/models/uijoin.go. T1WallMs is the session's end and is not
		 * knowable before it; and validateStructure refuses an empty
		 * IngressPorts list, so at start — with no ingress observed yet —
		 * there is no storable annotation to write.
		 *
		 * ON persistCtx, NOT ctx. This defer runs during teardown, so on
		 * Ctrl+C `ctx` is already cancelled and the write would fail for
		 * the one reason that has nothing to do with the data. persistCtx
		 * is the same detached context the test-case and mock stores use,
		 * and for the same reason.
		 *
		 * NOT GUARDED ON recordingStarted, deliberately. Such a guard
		 * would be redundant rather than wrong: a session that never
		 * began has nothing to annotate, and that is already handled one
		 * layer down — `recordingStarted = true` is set before the
		 * consumer goroutine that calls observe() is ever spawned, so
		 * recordingStarted == false implies both that sorted() is nil
		 * and that nothing was persisted.
		 *
		 * WHICH ARM FIRES DEPENDS ON WHY THE RUN STOPPED, and an earlier
		 * version of this note said it was always the testCount one and
		 * always Debug. That is no longer true and was the justification
		 * for deleting the guard, so it is worth restating exactly. If
		 * GetNextTestSetID failed, newTestSetID is "" and the
		 * `testSetID == ""` arm fires -- at ERROR, because a join that
		 * was REQUESTED and cannot be attempted must not return
		 * silently. Only when a test-set id exists and nothing was
		 * persisted does the testCount arm fire, at Debug.
		 *
		 * The guard stays deleted anyway, and on a better reason than
		 * the one it had: what it would suppress is a truthful Error
		 * about a join the operator asked for and did not get. It could
		 * be deleted with the whole package green, and inverting it to
		 * `!recordingStarted` DID turn the harness test red -- so the
		 * direction was pinned and only the removal survived.
		 */
		/*
		 * A DEADLINE, AND EXACTLY AS MUCH AS THAT BUYS.
		 *
		 * persistCtx is context.WithoutCancel(ctx), which strips the
		 * deadline along with the cancellation -- that is what it is for,
		 * so a write survives SIGINT. This call then sits after every
		 * DrainErrGroup budget, on the path SIGINT takes, and does a
		 * read-modify-write of config.yaml with no bound of its own.
		 *
		 * WHAT THE 10s ACTUALLY REACHES, traced rather than assumed:
		 * ctxReader.Read selects on ctx.Done() between chunks, and
		 * CreateFileF checks ctx.Err() in its not-exist branch. That is
		 * all. ctxWriter.Write (pkg/platform/yaml/yaml.go) is a plain
		 * retry loop that never looks at the context; MkdirAll, OpenFile,
		 * Sync, Chmod and Rename are blocking syscalls a Go context
		 * cannot interrupt; and warnIfDroppingFields re-reads with
		 * context.WithoutCancel, deliberately discarding this deadline
		 * inside the very write it is meant to bound.
		 *
		 * SO THIS DOES NOT MAKE TEARDOWN SAFE ON A WEDGED MOUNT. An
		 * NFS/overlay volume in D-state still hangs here, deadline or
		 * not, and it is not "bounded like every other operation in this
		 * defer": the drains bound because DrainErrGroup returns WITHOUT
		 * its goroutine, which a deadline over synchronous file I/O has
		 * no equivalent for. Nor is it the only unbounded one -- the
		 * per-name DeleteTests loop earlier in this defer is equally
		 * unbounded.
		 *
		 * What it does buy is the slow-but-responsive disk: the read
		 * returns DeadlineExceeded, persistUIJoinAnnotation propagates
		 * it, and the operator gets an Error line instead of an
		 * indefinite wait. The write is temp-file + Rename, so a
		 * cut-off write cannot corrupt config.yaml. Losing the
		 * annotation there is the right trade -- the recording is
		 * already on disk.
		 *
		 * AND NO TEST PINS THE BOUND. Measured: removing it entirely
		 * survives the package 10 runs out of 10 (shrinking it to 1ns is
		 * caught, so the context is genuinely propagated -- the duration
		 * is not). Said here rather than left to be found.
		 */
		annotateCtx, annotateCancel := context.WithTimeout(persistCtx, 10*time.Second)
		r.recordUIJoinAnnotation(
			annotateCtx,
			newTestSetID,
			// One argument where there were two interchangeable int64s,
			// and T1 is stamped inside newUIJoinSpan -- see uiJoinSpan.
			// NOT a compile-time guarantee: these two are both time.Time
			// and swapping them still builds. It is caught by
			// TestStart_TakesT0FromTheCaptureStartNotTheProcessStart,
			// measured 10 kills out of 10.
			newUIJoinSpan(captureStart, sessionStart),
			uiJoinObserved.sorted(),
			// The one guarded snapshot, taken at the top of this defer
			// and shared by all three consumers -- this annotation and
			// the two telemetry calls beside it, which read
			// testCountSnapshot rather than the live counter for the
			// same reason. Reading the counter once is what makes the
			// three agree; an inline read here could race the
			// still-live consumer on the drain-timeout path and size the
			// annotation from a different number than the telemetry.
			//
			// NOT COVERED, said rather than left to be found: replacing
			// this with a bare `testCount` read is a MEASURED survivor.
			// The snapshot's POSITION is pinned (moving it above the
			// revoke block is caught); the snapshot itself is not.
			testCountSnapshot,
		)
		annotateCancel()
		if s, ok := r.telemetry.(interface{ Shutdown() }); ok {
			s.Shutdown()
		}
	}()

	// appErrChan is intentionally NOT closed by Start. The app-runner goroutine
	// spawned in runAppErrGrp (the DockerCompose branch at ~308 and the
	// non-compose branch at ~518) sends the app's exit error on it and can
	// still be running when Start returns during shutdown: on SIGTERM the app
	// exits with "signal: terminated" (not ErrCtxCanceled), so the goroutine
	// reaches its `appErrChan <- runAppError` send. Closing the channel here
	// raced that send and panicked with "send on closed channel" (seen in the
	// pulsar-basetopic teardown). The sole consumer is a single receive in the
	// select below that does not depend on a close, and the size-1 buffer
	// absorbs the lone send (the two sender branches are mutually exclusive),
	// so leaving the channel open to be GC'd is correct and race-free.
	// insertTestErrChan / insertMockErrChan are NOT closed either, for exactly
	// the reason spelled out for appErrChan above. Their producer is the
	// consumer goroutine spawned below, which can still be mid-send when Start
	// returns during teardown; closing here raced that send and panicked with
	// "send on closed channel", taking down the whole control plane in the
	// DaemonSet embedding. The sole consumer is a single receive in the select
	// below that does not depend on a close, and the size-10 buffers absorb any
	// in-flight send, so leaving them open to be GC'd is correct and race-free.

	newTestSetID, err = r.GetNextTestSetID(ctx)
	if err != nil {
		stopReason = "failed to get new test-set id"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf("%s", stopReason)
	}

	// Create config.yaml if metadata is provided
	if r.config.Record.Metadata != "" && r.testSetConf != nil {
		r.createConfigWithMetadata(ctx, newTestSetID)
	}

	//checking for context cancellation as we don't want to start the instrumentation if the context is cancelled
	select {
	case <-ctx.Done():
		return nil
	default:
	}

	passPortsUint := config.GetByPassPorts(r.config)
	passPortsUint32 := make([]uint32, len(passPortsUint)) // slice type of uint32
	for i, port := range passPortsUint {
		passPortsUint32[i] = uint32(port)
	}

	memoryLimit := uint64(0)
	if r.config.CommandType == string(utils.DockerRun) || r.config.CommandType == string(utils.DockerCompose) {
		memoryLimit = r.config.Record.MemoryLimit
	}

	// Instrument will setup the environment and start the hooks and proxy
	setupOpts := models.SetupOptions{Container: r.config.ContainerName, DockerDelay: r.config.BuildDelay, Mode: models.MODE_RECORD, CommandType: r.config.CommandType, EnableTesting: false, GlobalPassthrough: r.config.Record.GlobalPassthrough, CapturePackets: r.config.Record.CapturePackets, OpportunisticTLSIntercept: r.config.Record.OpportunisticTLSIntercept, ChannelBindingShim: r.config.Record.ChannelBindingShim, UpstreamTLSVerify: r.config.Record.UpstreamTLS.Verify, UpstreamTLSCACert: r.config.Record.UpstreamTLS.CACert, BuildDelay: r.config.BuildDelay, PassThroughPorts: passPortsUint, MemoryLimit: memoryLimit, ConfigPath: r.config.ConfigPath, EnableSampling: r.config.Record.EnableSampling, RecordBufferMaxMemoryPerConn: r.config.Record.RecordBuffer.MaxMemoryPerConnection, RecordBufferQueueSize: r.config.Record.RecordBuffer.QueueSize, RecordBufferConsumerStallGrace: r.config.Record.RecordBuffer.ConsumerStallGrace, RecordBufferHalfCloseGrace: r.config.Record.RecordBuffer.HalfCloseGrace}
	// Retry only a stalled agent bring-up (pkg.ErrAgentNotReady) with a fresh agent.
	err = pkg.RetryAgentSetup(setupCtx, r.logger, func(c context.Context, attempt int) error {
		o := setupOpts
		if attempt > 1 {
			o.AgentReadyTimeout = 120 * time.Second
		}
		return r.instrumentation.Setup(c, r.config.Command, o)
	})

	if err != nil {
		// If context was cancelled (user pressed Ctrl+C), return gracefully without error
		if ctx.Err() != nil {
			return nil
		}
		stopReason = "failed setting up the environment"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf("%s", stopReason)
	}

	r.logger.Debug("Command type:", zap.String("commandType", r.config.CommandType))

	if r.config.CommandType == string(utils.DockerCompose) {

		r.logger.Info("Waiting for keploy-agent to be ready for docker compose...", zap.String("Agent-uri", r.config.Agent.AgentURI))

		runAppErrGrp.Go(func() error {
			runAppError = r.instrumentation.Run(runAppCtx, models.RunOptions{})
			if (runAppError.AppErrorType == models.ErrCtxCanceled || runAppError == models.AppError{}) {
				return nil
			}
			appErrChan <- runAppError
			return nil
		})

		// Aligned with the agent's own healthcheck budget; a fixed 120s wait gave
		// up while the agent container was still starting under CI daemon
		// contention. See pkg.AgentReadyTimeout (KEPLOY_AGENT_READY_TIMEOUT).
		agentCtx, cancel := context.WithTimeout(ctx, pkg.AgentReadyTimeout())
		defer cancel()

		agentReadyCh := make(chan bool, 1)
		go pkg.AgentHealthTicker(agentCtx, r.logger, r.config.Agent.AgentURI, agentReadyCh, 1*time.Second)

		// agentCtx is ctx's child, so its Done covers a stop as well as the
		// budget running out, and the ticker closes agentReadyCh (ready ==
		// false) on either. Which it was is decided below, in order, not by
		// whichever case the select happens to pick when several are ready
		// at once -- as they all are when the stop landed during setup,
		// which App.SetupCompose, taking no context, reports as success.
		var ready bool
		select {
		case <-agentCtx.Done():
		case ready = <-agentReadyCh:
		}
		if ctx.Err() != nil {
			// Stopped (Ctrl+C, SIGTERM), not failed, whatever else was
			// ready. Every stop returns nil -- see the final select.
			return nil
		}
		if !ready {
			return fmt.Errorf("keploy-agent did not become ready in time")
		}
	}

	r.logger.Debug("Agent is ready. Starting to fetch test cases and mocks...")

	var correlationMap sync.Map
	// asyncMockIDs holds tempIDs of async mocks (Spec.Async set); resolveMappingEntries uses it
	// to keep async-egress mocks out of the per-test mappings (see its doc).
	var asyncMockIDs sync.Map
	// droppedMockIDs holds tempIDs of mocks the recorder gave up persisting.
	// The mapping consumer checks it so it never burns its ~500ms correlation
	// spin on a mock that will never arrive, and so it can revoke the owning
	// test rather than replay it with a short mock pool.
	var droppedMockIDs droppedMockSet
	// fetching test cases and mocks from the application and inserting them into the database
	frames, err := r.GetTestAndMockChans(reqCtx)
	if err != nil {
		stopReason = "failed to get data frames"
		utils.LogError(r.logger, err, stopReason)
		if ctx.Err() == context.Canceled {
			return nil
		}
		return fmt.Errorf("%s", stopReason)
	}
	recordingStarted = true
	// Stamped HERE, beside the flag, because they mean the same thing:
	// GetTestAndMockChans has returned, so the agent is capturing.
	captureStart = time.Now()

	/*
	 * THE FORWARDERS NOW OUTLIVE THIS FUNCTION, so every path out of it has
	 * to tell them whether a consumer is coming.
	 *
	 * GetTestAndMockChans has spawned UP TO three forwarders on reqCtx
	 * (WithoutCancel), and they keep pulling from the agent regardless of
	 * what happens to ctx.
	 *
	 * "Up to", because it has two `err == nil` returns of its own, both on
	 * the shutdown path: the one taken when GetIncoming fails with ctx
	 * already done has spawned NONE and closes all three channels, and
	 * the GetOutgoing equivalent has spawned ONE (the incoming forwarder)
	 * and sets handedOff itself. This defer is correct on all three
	 * counts -- it abandons whatever exists -- but an earlier version of
	 * this note said "three" flatly, which is only true of a full
	 * success, and someone reasoning from it about the shutdown paths
	 * would be reasoning about goroutines that were never started.
	 *
	 * The consumers that drain them are spawned three
	 * hundred lines below, and between here and there is exactly ONE
	 * return: the ctx.Err() gate on the next line. On that path the
	 * forwarders are alive with nobody reading, and each one ends by
	 * handing over the item it has already taken from the agent. That
	 * hand-over used to park forever: a 30s DrainErrGroup timeout on Ctrl+C
	 * and a goroutine held for the process lifetime, re-leaked per session
	 * in the DaemonSet embedding. MEASURED deterministically on all three.
	 *
	 * A defer rather than a call beside each return, because the failure
	 * mode of getting this wrong is a hang rather than a compile error, and
	 * a new error return added later would reintroduce it silently. The flag
	 * is set only once every consumer is running, so it says exactly what it
	 * is named for: is anyone going to read these channels.
	 */
	consumersStarted := false
	defer func() {
		if consumersStarted {
			return
		}
		/*
		 * NIL-GUARDED, because a missing Abandon must degrade to the leak
		 * this defer exists to prevent -- not to a panic in teardown.
		 *
		 * GetTestAndMockChans sets Abandon on every err == nil return, so
		 * this is unreachable today, and the suite cannot reach it either
		 * -- MEASURED: removing this nil guard AND `Abandon:` from an
		 * early return leaves the package green. The guard is kept for
		 * the cost of being wrong about a construction site, not because
		 * a test defends it; saying so is more useful than a measurement
		 * that was never taken.
		 */
		if frames.Abandon != nil {
			frames.Abandon()
		}
	}()

	if ctx.Err() != nil {
		return nil
	}

	// Kick off the pcap + keylog streams. GetTestAndMockChans above
	// has already triggered Proxy.Record() on the agent, which
	// installs the broadcaster — so the stream subscribes against a
	// live capture. The goroutine ends when ctx is cancelled
	// (recording stop) or when the agent's HTTP server closes the
	// response. Any error is logged but never fails the recording.
	if r.config.Record.CapturePackets {
		destDir := filepath.Join(r.config.Path, newTestSetID)
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			r.logger.Warn("failed to create test-set dir for pcap stream; continuing without pcap",
				zap.String("destDir", destDir), zap.Error(err))
		} else {
			errGrp, _ := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
			if errGrp != nil {
				errGrp.Go(func() error {
					if err := r.instrumentation.StreamPcapArtifacts(ctx, destDir); err != nil {
						utils.LogError(r.logger, err, "pcap stream ended with error",
							zap.String("destDir", destDir))
					}
					return nil
				})
			}
		}
	}

	if r.config.CommandType == string(utils.DockerCompose) {

		r.logger.Debug("Making keploy-agent ready for docker compose...")

		err := r.instrumentation.MakeAgentReadyForDockerCompose(ctx)
		if err != nil {
			utils.LogError(r.logger, err, "Failed to make the request to make agent ready for the docker compose")
		}
	}

	r.logger.Info("Keploy agent is ready to record test cases and mocks.")

	r.mockDB.ResetCounterID() // Reset mock ID counter for each recording session
	errGrp.Go(func() error {
		for testCase := range frames.Incoming {
			// Skip curl generation for either form data requests or large body (>1MB)
			if len(testCase.HTTPReq.Body) <= 1*1024*1024 && len(testCase.HTTPReq.Form) == 0 {
				testCase.Curl = pkg.MakeCurlCommand(testCase.HTTPReq)
			}
			domainSet.AddAll(telemetry.ExtractDomainsFromTestCase(testCase))
			if hookErr := r.hooks.BeforeTestCaseInsert(ctx, &TestCaseContext{
				TestCase: testCase, TestSetID: newTestSetID,
			}); hookErr != nil {
				r.logger.Error("BeforeTestCaseInsert hook failed; recording will continue but hook side-effects may be missing. Check your RecordHooks implementation.",
					zap.Error(hookErr),
					zap.String("testSetID", newTestSetID),
					zap.String("testCaseName", testCase.Name))
			}
			/*
			 * OBSERVED HERE, at the one point ingress is known to have
			 * ARRIVED -- which is BEFORE the insert, not after it.
			 *
			 * IngressPorts exists so the offline joiner can say
			 * NO_INGRESS_OBSERVED for an exchange on a port nothing was
			 * listening on, rather than failing to join with no reason.
			 * That answer is only honest if the list records ports
			 * ingress ACTUALLY arrived on — a configured value would
			 * describe intent, and could claim a port this recording
			 * never saw.
			 *
			 * Before the insert, deliberately: a test case that fails to
			 * persist still tells us which port ingress came in on, and
			 * the port list is about what the recorder observed, not
			 * about what the store accepted.
			 *
			 * PINNED BOTH WAYS, BY TWO TESTS -- and it takes two.
			 *
			 * Moving this call into the `err == nil` branch below is the
			 * disk-full shape, where every insert errors and the mutant
			 * loses the join entirely:
			 * TestAPortIsObservedEvenWhenItsTestCaseFailsToPersist fails
			 * on it.
			 *
			 * Moving it after the whole if/else is only visible when the
			 * error branch takes its `continue`, which needs ctx already
			 * cancelled. That test runs on context.Background(), so
			 * execution falls through past the if/else either way and the
			 * mutant SURVIVED it 10 runs out of 10.
			 * TestAPortIsObservedWhenTheInsertFailsDuringShutdown is the
			 * missing half: it holds the insert open, cancels, then lets
			 * it fail, so the `continue` really is taken.
			 */
			uiJoinObserved.observe(testCase.AppPort)
			err := r.testDB.InsertTestCase(persistCtx, testCase, newTestSetID, true)
			if err != nil {
				if ctx.Err() != nil {
					// Once shutdown has begun nothing reads insertTestErrChan:
					// Start's select has already returned, and this loop keeps
					// draining for the whole app-drain window. Log instead of
					// dropping the error on the floor: with persistCtx the write
					// no longer fails merely because we are shutting down, so an
					// error here is real and the operator needs it.
					utils.LogError(r.logger, err, "failed to record test case during shutdown",
						zap.String("testCaseName", testCase.Name),
						zap.String("testSetID", newTestSetID))
					continue
				}
				// Non-blocking for the same reason as the mock loop's sibling
				// send below: on a disk-full EVERY insert errors here, and this
				// loop keeps draining long after Start's one-shot select has
				// taken the first error. A blocking send would park forever once
				// the buffer filled, wedging this goroutine and the forwarder
				// feeding frames.Incoming, timing out DrainErrGroup and leaking
				// both past teardown. The buffer guarantees the first error —
				// the only one acted on — still lands.
				select {
				case insertTestErrChan <- err:
				default:
				}
			} else {
				// testCount and insertedNames are both TC-insert state; write
				// them under one insertedMu section so the finalize revoke block
				// (which reads insertedNames and adjusts testCount, possibly
				// while this goroutine is still live on the drain-timeout path)
				// can't race them.
				insertedMu.Lock()
				testCount++
				insertedNames[testCase.Name] = struct{}{}
				insertedMu.Unlock()
				r.telemetry.RecordedTestAndMocks()
				if hookErr := r.hooks.AfterTestCaseInsert(ctx, &TestCaseContext{
					TestCase: testCase, TestSetID: newTestSetID,
				}); hookErr != nil {
					r.logger.Error("AfterTestCaseInsert hook failed; test case was recorded successfully but post-insert hook side-effects may be missing. Check your RecordHooks implementation.",
						zap.Error(hookErr),
						zap.String("testSetID", newTestSetID),
						zap.String("testCaseName", testCase.Name))
				}
			}
		}
		return nil
	})

	errGrp.Go(func() error {
		for mock := range frames.Outgoing {
			// Deferred-orphan revoke: a reserved-Kind control frame, NOT a mock.
			// Divert it into the revoke set (applied at finalize) BEFORE any
			// domain-extraction / hook / InsertMock / correlation work — it
			// carries no traffic and must never be persisted.
			if mock.GetKind() == string(models.RevokedTests) {
				if mock.Spec.Metadata != nil {
					for _, n := range strings.Split(mock.Spec.Metadata["revoked_tests"], ",") {
						if n = strings.TrimSpace(n); n != "" {
							revokedMu.Lock()
							revokedNames[n] = struct{}{}
							revokedMu.Unlock()
						}
					}
				}
				continue
			}
			domainSet.AddAll(telemetry.ExtractDomainsFromMock(mock))
			tempID := mock.Name
			mockCtx := &MockContext{Mock: mock, TestSetID: newTestSetID}
			if hookErr := r.hooks.BeforeMockInsert(ctx, mockCtx); hookErr != nil {
				r.logger.Error("BeforeMockInsert hook failed; recording will continue but hook side-effects may be missing. Check your RecordHooks implementation.",
					zap.Error(hookErr),
					zap.String("testSetID", newTestSetID),
					zap.String("mockName", mock.Name),
					zap.String("mockKind", mock.GetKind()))
			}
			if mockCtx.Skip {
				// A hook asked to drop this mock (collapsed async poll no-change
				// cycle). Do not persist, map, or correlate it.
				//
				// The agent does not know we skipped it, so its tempID is still
				// in the mapping it streams, and two separate things must be
				// true for that not to hurt:
				//
				//  1. asyncMockIDs — CORRECTNESS, and unconditional. Skip means
				//     "never persisted", so this tempID must never appear in a
				//     per-test mapping and must never be read as loss. Note it
				//     cannot be gated on mock.IsAsync(): AsyncRecorder sets
				//     info.Skip and returns BEFORE it assigns Spec.Async, so a
				//     collapsed cycle reports IsAsync()==false. Gating on it
				//     would make this whole guard a no-op.
				//  2. droppedMockIDs — SPEED, and best-effort. It short-circuits
				//     the ~500ms correlation spin, but it is capped, so on a very
				//     long poll-heavy recording it eventually declines entries.
				//     That costs latency, never correctness, because (1) is not
				//     capped.
				markSkippedMock(&asyncMockIDs, &droppedMockIDs, tempID)
				continue
			}
			// The AsyncRecorder hook sets Spec.Async in BeforeMockInsert above;
			// remember it so the mapping goroutine below never per-test maps it.
			if mock.IsAsync() {
				asyncMockIDs.Store(tempID, struct{}{})
			}
			err := r.mockDB.InsertMock(persistCtx, mock, newTestSetID)
			if err != nil {
				if ctx.Err() != nil {
					// See the sibling note on the test-case insert: insertMockErrChan
					// is no longer safe to report through once teardown has begun. A
					// skipped mock also skips its correlationMap entry below, which
					// strands the mapping that references it — so this must be loud.
					//
					// Bookkeep EVERY error class here, not just ErrMockEncode: past
					// this point the mock is lost whatever the cause, and the
					// consequences are identical — the owning test would otherwise be
					// persisted with a short pool after its mapping burned the full
					// correlation spin, invisible to both the teardown Warn and the
					// telemetry. Nothing is being made non-fatal: teardown is already
					// under way.
					mockCountMapMu.Lock()
					droppedMockCount++
					droppedMockKinds[mock.GetKind()]++
					mockCountMapMu.Unlock()
					droppedMockIDs.add(tempID)
					utils.LogError(r.logger, err, "failed to record mock during shutdown",
						zap.String("mockName", mock.Name),
						zap.String("mockKind", mock.GetKind()),
						zap.String("testSetID", newTestSetID))
					continue
				}
				if errors.Is(err, models.ErrMockEncode) {
					// THIS mock's payload cannot be encoded — an unsupported
					// kind or a value the encoder cannot represent. Unlike a
					// disk-full or storage-gone error it says nothing about the
					// next mock, so killing the session throws away everything
					// recorded so far: one gzip response body ended a 46-hour
					// production recording exactly this way. Drop the one mock
					// and keep recording.
					mockCountMapMu.Lock()
					droppedMockCount++
					droppedMockKinds[mock.GetKind()]++
					dropped := droppedMockCount
					mockCountMapMu.Unlock()
					// Tell the mapping consumer so it revokes the owning test
					// instead of spinning on an ID that will never arrive.
					tracked := droppedMockIDs.add(tempID)
					// Loud on the first, then sampled: a recording that quietly
					// grows holes is worse than one that dies, so this must stay
					// visible without drowning the log on a systematic failure.
					if dropped == 1 || dropped%100 == 0 {
						utils.LogError(r.logger, err, "dropping ONE unencodable mock and CONTINUING to record; the test that owns it is revoked, not recorded short",
							zap.String("mockName", mock.Name),
							zap.String("mockKind", mock.GetKind()),
							zap.String("testSetID", newTestSetID),
							zap.Int("mocksDroppedSoFar", dropped),
							zap.Bool("ownerRevocable", tracked))
					}
					continue
				}
				// Environmental (I/O, storage gone): every later mock fails too,
				// so stopping is still correct — but stopping is not instant.
				// Start's select takes the first error and teardown then runs
				// for the whole app-drain window with this loop still draining,
				// so errors 2..N land here too. Bookkeep them exactly like the
				// shutdown branch: the mock is lost whatever the cause, and
				// without this the teardown Warn and the mocks-dropped telemetry
				// undercount while every owning test is persisted with a short
				// pool instead of revoked.
				mockCountMapMu.Lock()
				droppedMockCount++
				droppedMockKinds[mock.GetKind()]++
				dropped := droppedMockCount
				mockCountMapMu.Unlock()
				droppedMockIDs.add(tempID)
				if dropped == 1 || dropped%100 == 0 {
					utils.LogError(r.logger, err, "failed to record mock (environmental); stopping the session",
						zap.String("mockName", mock.Name),
						zap.String("mockKind", mock.GetKind()),
						zap.String("testSetID", newTestSetID),
						zap.Int("mocksDroppedSoFar", dropped))
				}
				// The send must not block: once the buffer filled, a blocking
				// send would wedge this goroutine forever — which in turn wedges
				// the forwarder feeding frames.Outgoing, times out both
				// DrainErrGroup budgets, and leaks the pair past teardown (it
				// matters in the DaemonSet embedding, where Start is re-entered
				// per session). Only the FIRST error is acted on anyway; the
				// buffer guarantees it lands.
				select {
				case insertMockErrChan <- err:
				default:
				}
			} else {
				if hookErr := r.hooks.AfterMockInsert(ctx, &MockContext{
					Mock: mock, TestSetID: newTestSetID,
				}); hookErr != nil {
					r.logger.Error("AfterMockInsert hook failed; mock was inserted successfully but post-insert hook side-effects may be missing. Check your RecordHooks implementation.",
						zap.Error(hookErr),
						zap.String("testSetID", newTestSetID),
						zap.String("mockName", mock.Name),
						zap.String("mockKind", mock.GetKind()))
				}
				if tempID != "" && mock.Name != "" {
					correlationMap.Store(tempID, models.MockEntry{
						Name:             mock.Name,
						Kind:             string(mock.GetKind()),
						Timestamp:        mock.Spec.ReqTimestampMock.Unix(),
						ReqTimestampMock: models.FormatMockTimestamp(mock.Spec.ReqTimestampMock),
						ResTimestampMock: models.FormatMockTimestamp(mock.Spec.ResTimestampMock),
					})
				}
				mockCountMapMu.Lock()
				mockCountMap[mock.GetKind()]++
				mockCountMapMu.Unlock()
				r.telemetry.RecordedTestCaseMock(mock.GetKind())
			}
		}
		return nil
	})

	errGrp.Go(func() error {
		return r.consumeMappings(ctx, newTestSetID, frames.Mappings, &correlationMap, &asyncMockIDs, &droppedMockIDs,
			func(testName string) {
				// Reuse the deferred-orphan revoke path the agent's own
				// capacity-drop uses: finalize deletes these test cases instead
				// of letting them reach replay without their mocks.
				revokedMu.Lock()
				revokedNames[testName] = struct{}{}
				revokedMu.Unlock()
			}, &shortPoolByTest)
	})

	// Every frame channel now has a reader, so the shutdown hand-over can no
	// longer be abandoned. Nothing may return between GetTestAndMockChans and
	// this line without the defer above firing.
	consumersStarted = true

	if r.config.CommandType != string(utils.DockerCompose) {
		runAppErrGrp.Go(func() error {
			runAppError = r.instrumentation.Run(runAppCtx, models.RunOptions{})
			if runAppError.AppErrorType == models.ErrCtxCanceled {
				return nil
			}
			appErrChan <- runAppError
			return nil
		})
	}

	// setting a timer for recording
	if r.config.Record.RecordTimer != 0 {
		errGrp.Go(func() error {
			r.logger.Info("Setting a timer of " + r.config.Record.RecordTimer.String() + " for recording")
			timer := time.After(r.config.Record.RecordTimer)
			select {
			case <-timer:
				r.logger.Info("Time up! Stopping keploy")
				err := utils.Stop(r.logger, "Time up! Stopping keploy")
				if err != nil {
					utils.LogError(r.logger, err, "failed to stop recording")
					return errors.New("failed to stop recording")
				}
			case <-ctx.Done():
				return nil
			}
			return nil
		})
	}

	// Waiting for the error to occur in any of the go routines.
	//
	// A recording that was STOPPED -- a signal, --record-timer, anything that
	// cancels ctx -- returns nil from here and from every earlier stop check,
	// and one that FAILED returns an error, so the caller can tell the two
	// apart by the error alone. `keploy record` does: it exits non-zero on
	// any error (cli/record.go), and cannot ask ctx instead, because the stop
	// defer above cancels the root context on every other way out.
	var stoppedBy *models.AppError
	select {
	case appErr := <-appErrChan:
		stoppedBy = &appErr
		switch appErr.AppErrorType {
		case models.ErrCommandError:
			stopReason = "error in running the user application, hence stopping keploy"
		case models.ErrUnExpected:
			stopReason = "user application terminated unexpectedly hence stopping keploy, please check application logs if this behaviour is not expected"
		case models.ErrInternal:
			stopReason = "internal error occurred while hooking into the application, hence stopping keploy"
		case models.ErrAppStopped:
			stopReason = "user application terminated unexpectedly hence stopping keploy, please check application logs if this behaviour is not expected"
			r.logger.Info(stopReason, zap.Error(appErr))
			return nil
		case models.ErrCtxCanceled:
			return nil
		case models.ErrTestBinStopped:
			stopReason = "keploy test mode binary stopped, hence stopping keploy"
			return nil
		default:
			stopReason = "unknown error received from application, hence stopping keploy"
		}

	case err = <-insertTestErrChan:
		stopReason = "error while inserting test case into db, hence stopping keploy"
	case err = <-insertMockErrChan:
		stopReason = "error while inserting mock into db, hence stopping keploy"
	case <-ctx.Done():
		return nil
	}
	utils.LogError(r.logger, err, stopReason)
	if stoppedBy != nil {
		return &appStopError{reason: stopReason, app: *stoppedBy}
	}
	return fmt.Errorf("%s", stopReason)
}

func (r *Recorder) GetTestAndMockChans(ctx context.Context) (FrameChan, error) {

	incomingOpts := models.IncomingOptions{
		Filters: r.config.Record.Filters,
	}

	// Create channels to receive incoming and outgoing data.
	//
	// THE ONE SLOT IS A CONVENIENCE, NOT A GUARANTEE. It is tempting to
	// reason that the handover fires at most once per forwarder, so one slot
	// is exactly sufficient; it does not follow. The slot can ALREADY be full
	// from an ordinary send that no consumer has taken yet, and the handover
	// below then parks on a full buffer exactly as it would on an unbuffered
	// one. Measured, with nothing consuming: mappingChan (unbuffered) wedges
	// on the first item, these two on the second.
	//
	// What actually stops the wedge is `abandoned` — see FrameChan.Abandon.
	// Nothing consuming is a reachable state rather than a hypothetical:
	// these forwarders run on reqCtx (WithoutCancel) and keep pulling from
	// the agent, while Start can return between creating them and spawning
	// the consumers. With the fix in place Abandon reaches them FIRST --
	// Start registers its abandon defer after the stop defer, so LIFO runs
	// it before reqCtxCancel(). Before the fix that cancellation was what
	// drove all three into the handover, with nobody left to receive it.
	incomingChan := make(chan *models.TestCase, 1)
	outgoingChan := make(chan *models.Mock, 1)
	mappingChan := make(chan models.TestMockMapping)

	// Closed by FrameChan.Abandon. Idempotent via sync.Once because Start
	// defers it unconditionally and the error paths below return a FrameChan
	// that carries it too.
	abandoned := make(chan struct{})
	var abandonOnce sync.Once
	abandon := func() { abandonOnce.Do(func() { close(abandoned) }) }

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return FrameChan{}, fmt.Errorf("failed to get error group from context")
	}

	// INCOMING
	incomingStream, err := r.instrumentation.GetIncoming(ctx, incomingOpts)
	if err != nil {
		if ctx.Err() != nil || utils.IsShutdownError(err) {
			r.logger.Debug("Context cancelled or shutdown error while getting incoming test cases")
			/*
			 * ALL THREE CLOSED, and mappingChan is the one that was
			 * missed.
			 *
			 * These early returns hand back a FrameChan the caller ranges
			 * over. A channel that is never closed and never written is
			 * the same as no channel at all: `for m := range mappings`
			 * blocks forever with no ctx escape. Start spawns
			 * consumeMappings unconditionally, so it parked here, and
			 * teardown waited out the whole DrainErrGroup budget before
			 * giving up on it.
			 *
			 * NOT A SHUTDOWN-ONLY PATH, which is what made it worth
			 * fixing rather than documenting: utils.IsShutdownError
			 * matches "connection refused", so an agent socket that is
			 * merely not up yet lands here with ctx perfectly live.
			 * MEASURED on that shape: Start returned in 30.0s and the
			 * mapping consumer plus its flush ticker leaked for the
			 * process lifetime.
			 */
			close(incomingChan)
			close(outgoingChan)
			close(mappingChan)
			return FrameChan{
				Incoming: incomingChan,
				Outgoing: outgoingChan,
				Mappings: mappingChan,
				Abandon:  abandon,
			}, nil
		}
		return FrameChan{}, fmt.Errorf("failed to get incoming test cases: %w", err)
	}

	g.Go(func() error {
		defer close(incomingChan)
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case tc, ok := <-incomingStream:
				if !ok {
					return nil
				}
				// This test case has ALREADY been taken from the agent — the
				// agent considers it delivered and will not re-send it. Returning
				// here on a cancelled ctx therefore drops it silently, with no
				// log and no counter, while its mocks and its mapping are still
				// persisted: the set ships with orphaned mocks and a mappings.yaml
				// entry for a test that does not exist. Since SIGINT lands while
				// the agent is still handing over the tail, that is exactly the
				// window the tail is in.
				//
				// Hand it over anyway and then stop, mirroring the outgoing
				// forwarder below. The consumer writes on persistCtx (detached
				// from ctx) precisely so a shutdown-time hand-off still reaches
				// disk.
				//
				// UNLESS NOBODY IS COMING. Without the `abandoned` arm this
				// send parked forever whenever Start returned before spawning
				// a consumer; the buffer slot does not save it, because an
				// ordinary send may already be sitting in it.
				select {
				case <-ctx.Done():
					select {
					case incomingChan <- tc:
					case <-abandoned:
						r.logger.Warn("dropped an in-flight test case on shutdown: recording stopped before any consumer started, so there is nothing left to persist it",
							zap.String("testCaseName", tc.Name),
							zap.String("next_step", "this test case was captured but not written; re-record the test set if you need it"))
					}
					return ctx.Err()
				case incomingChan <- tc:
				case <-abandoned:
					// Abandoned while ctx is still live: Start returned before
					// spawning a consumer and reqCtx has not been cancelled yet.
					// Without this arm the send parks until that cancellation
					// arrives -- for mappings, a further 15s of mappingDrainGrace
					// on top.
					//
					// NOT COVERED on the OUTER select, and that is a
					// MEASURED survivor: deleting this arm here leaves the
					// package green, because the inner ctx.Done arm still
					// catches it eventually. On this select the arm is a
					// latency guard, not a correctness one. The INNER
					// hand-over arms below ARE pinned.
					r.logger.Warn("dropped a test case: recording stopped before any consumer started, so there is nothing left to persist it",
						zap.String("testCaseName", tc.Name),
						zap.String("next_step", "this test case was captured but not written; re-record the test set if you need it"))
					return nil
				}
			}
		}
	})

	/*
	 * FROM HERE ON A FORWARDER IS ALREADY RUNNING, so no error return may
	 * leave without telling it.
	 *
	 * The incoming forwarder is spawned just above, on reqCtx
	 * (WithoutCancel). Every `return FrameChan{}, err` below therefore
	 * hands the caller a ZERO FrameChan -- nil Abandon -- and Start bails
	 * at its `if err != nil` before it can register its own abandon
	 * defer. The forwarder is then unabandonable: it takes an item, parks
	 * on the hand-over, and nothing can release it.
	 *
	 * MEASURED on the non-shutdown GetOutgoing error with ctx live:
	 * Start returned in 30.028s -- the full DrainErrGroup budget -- with
	 * the goroutine leaked for the process lifetime. Byte-for-byte the
	 * SEV-1 the rest of this file's guards exist to remove, behind a
	 * different door.
	 *
	 * A DEFER WITH A FLAG rather than a call beside each return, for the
	 * same reason Start uses one: there are two such returns today, the
	 * failure mode of missing one is a hang rather than a compile error,
	 * and a third added later would reintroduce it silently.
	 */
	handedOff := false
	defer func() {
		if !handedOff {
			abandon()
		}
	}()

	// OUTGOING
	// Create a cancelable child that we always cancel when ctx is done.
	mockCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	var tlsPrivateKey string
	if r.config.Record.TLSPrivateKeyPath != "" {
		keyBytes, err := os.ReadFile(r.config.Record.TLSPrivateKeyPath)
		if err != nil {
			r.logger.Error("failed to read tls private key", zap.Error(err))
			cancel()
			return FrameChan{}, err
		}
		tlsPrivateKey = string(keyBytes)
	}

	outgoingStream, err := r.instrumentation.GetOutgoing(mockCtx, models.OutgoingOptions{
		Rules:                     r.config.BypassRules,
		MongoPassword:             r.config.Test.MongoPassword,
		TLSPrivateKey:             tlsPrivateKey,
		CapturePackets:            r.config.Record.CapturePackets,
		OpportunisticTLSIntercept: r.config.Record.OpportunisticTLSIntercept,
		// Only the boolean travels on this request; the *x509.CertPool cannot
		// (see models.OutgoingOptions.UpstreamTLSRootCAs). The agent resolves
		// record.upstreamTls.caCert on its own filesystem — the path precedence
		// at proxy.New, the pool itself lazily on the first record session —
		// and stamps it onto these options in Proxy.Record.
		UpstreamTLSVerify:         r.config.Record.UpstreamTLS.Verify,
		MysqlPorts:                r.config.MysqlPorts,
		DisableMysqlAutoDetect:    r.config.DisableMysqlAutoDetect,
		DisableMysqlEndpointDrift: r.config.DisableMysqlEndpointDrift,
		PassThroughPorts:          r.config.Record.PassThroughPorts,
		PassThroughHosts:          r.config.Record.PassThroughHosts,
		// Advertise that this CLI understands the reserved Kind=RevokedTests
		// control frame: it diverts such a frame into a revoke set and deletes
		// those deferred-orphan test cases at finalize instead of persisting it
		// as a mock. The agent emits revoke frames ONLY when this is true.
		SupportsDroppedRevoke: true,
	})
	if err != nil {

		cancel()
		if ctx.Err() != nil || utils.IsShutdownError(err) {
			r.logger.Debug("Context cancelled or shutdown error while getting outgoing mocks")
			// Close outgoingChan to prevent callers from hanging.
			// incomingChan is closed by the forwarder started above when
			// ctx is done. mappingChan has no producer on this path at
			// all -- see the note on the incoming early return for what
			// ranging over it costs.
			close(outgoingChan)
			close(mappingChan)
			handedOff = true
			return FrameChan{
				Incoming: incomingChan,
				Outgoing: outgoingChan,
				Mappings: mappingChan,
				Abandon:  abandon,
			}, nil
		}
		return FrameChan{}, fmt.Errorf("failed to get outgoing mocks: %w", err)
	}
	g.Go(func() error {
		defer close(outgoingChan)
		defer cancel()

		// Also cancel mockCtx when parent ctx is done
		// This is done inside the goroutine to avoid goroutine leaks
		go func() {
			<-ctx.Done()
			cancel()
		}()

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case m, ok := <-outgoingStream:
				if !ok {
					return nil
				}
				select {
				case <-ctx.Done():
					// See the incoming forwarder: the `abandoned` arm is what
					// keeps this from parking forever with no consumer.
					select {
					case outgoingChan <- m:
					case <-abandoned:
						r.logger.Warn("dropped an in-flight mock on shutdown: recording stopped before any consumer started, so there is nothing left to persist it",
							zap.String("mockName", m.Name),
							zap.String("next_step", "this mock was captured but not written; re-record the test set if you need it"))
					}
					return ctx.Err()
				case outgoingChan <- m:
				case <-abandoned:
					// See the incoming forwarder -- including its note that
					// this OUTER arm is a measured, uncovered latency guard.
					r.logger.Warn("dropped a mock: recording stopped before any consumer started, so there is nothing left to persist it",
						zap.String("mockName", m.Name),
						zap.String("next_step", "this mock was captured but not written; re-record the test set if you need it"))
					return nil
				}
			}
		}
	})

	// MAPPINGS
	g.Go(func() error {
		defer close(mappingChan)

		// A mapping is the LAST artifact a recorded test produces: it is derived,
		// and the agent can only emit it once that test's mock range has resolved.
		// So when recording stops, a burst for the most recently recorded tests is
		// still queued inside the agent. Tearing the stream down the instant ctx was
		// done dropped that burst SILENTLY — the entries never reach the consumer, so
		// not even a write error is logged. mappings.yaml then omitted the tail of
		// every endpoint, and replay reported "no_mocks" (candidates: 0) for exactly
		// those tests, because resolveMockSets picks mapping-based selection per test
		// SET and has no per-test fallback for a test that is missing.
		//
		// It is tempting to assume the tail has already been flushed by now because
		// teardown stops the app first. That is only true for the NATIVE path: under
		// docker-compose keploy never runs the app at all (see the CommandType check
		// above), runAppErrGrp is empty, its drain returns instantly, and reqCtxCancel
		// therefore fires within milliseconds of SIGINT — with the agent's queue still
		// full. Docker-compose is what most e2e lanes and many users run, and it is
		// where this bug was measured: 27 of 342 tests lost, all tails.
		//
		// So on shutdown keep the stream open and keep draining. Stop as soon as the
		// tail is through — the agent closing the stream, or the stream falling idle —
		// and hard-cap the total wait so a wedged agent cannot hang exit.
		mapCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		defer cancel()

		drained := make(chan struct{})
		defer close(drained)
		go func() {
			select {
			case <-drained:
				return // finished before shutdown — nothing to wait for
			case <-ctx.Done():
			}
			select {
			case <-drained: // tail fully drained
			case <-time.After(mappingDrainGrace):
				// Hitting the cap means the agent never finished flushing, so the
				// tail is truncated here — the same damage this drain prevents.
				// Say so: silence is what made the original bug so hard to find.
				r.logger.Warn("timed out draining test-mock mappings on shutdown",
					zap.Duration("waited", mappingDrainGrace),
					zap.String("next_step", "mappings.yaml may be missing the last recorded tests and replay can report no_mocks for them; re-record the test set and report this if it recurs"))
			}
			cancel()
		}()

		// Call the new AgentClient method
		ch, err := r.instrumentation.GetMappings(mapCtx, incomingOpts)
		if err != nil {
			if ctx.Err() != nil || utils.IsShutdownError(err) {
				return nil
			}
			return fmt.Errorf("failed to get mappings: %w", err)
		}

		// idle is armed only once shutdown has begun. While recording is live, gaps
		// between mappings are normal and must never end the stream; once shutdown
		// starts, a gap long enough means the agent has nothing left to flush (it
		// holds the stream open for the whole session, so it will not close it).
		idle := time.NewTimer(mappingIdleGrace)
		defer idle.Stop()
		stopTimer(idle)

		for {
			// While recording is live, wake on ctx.Done(); once draining, wait on the
			// idle timer instead. Arming both would let an already-closed ctx.Done()
			// spin the loop.
			var idleC <-chan time.Time
			var shutdownC <-chan struct{}
			if ctx.Err() != nil {
				resetTimer(idle, mappingIdleGrace)
				idleC = idle.C
			} else {
				shutdownC = ctx.Done()
			}

			select {
			case <-shutdownC:
				// Shutdown began: re-loop to arm the idle timer and start draining.
				continue
			case <-abandoned:
				/*
				 * NOBODY IS COMING, so there is nothing to drain FOR.
				 *
				 * `abandoned` was wired only into the two SEND selects
				 * below, never here -- so an abandoned mapping forwarder
				 * ignored it and sat out the full mappingIdleGrace before
				 * noticing the stream had gone quiet. MEASURED: a fixed
				 * 3s added to every abandoned teardown, inside the
				 * record-req drain, and 8 of the wedge table's 10
				 * subtests paying it at 3s each.
				 *
				 * Returning here drops nothing a consumer could have
				 * received: an item already taken off the stream is
				 * handled by the hand-over below, which logs what it
				 * drops. Items still queued on `ch` ARE dropped, and
				 * without a log line -- where the pre-fix path logged
				 * one. That is the right trade only because this arm is
				 * reached when the consumers are already gone, so no
				 * reader exists for them; it is not "nothing".
				 */
				return nil
			case <-mapCtx.Done():
				// Hard cap reached, or the stream was torn down.
				return nil
			case <-idleC:
				// Draining and nothing for mappingIdleGrace: the tail is through.
				return nil
			case m, ok := <-ch:
				if !ok {
					return nil
				}
				select {
				case <-mapCtx.Done():
					// Hand off the mapping we already took off the stream before
					// unwinding — cancellation is not a licence to drop data we are
					// holding. Both cases are ready once mapCtx is done, so the
					// runtime picks at random and this would otherwise lose the last
					// in-flight mapping about half the time. Mirrors the outgoing
					// producer above.
					//
					// THE CONSUMER ONLY RANGES IF Start GOT FAR ENOUGH TO
					// SPAWN IT, which is not something this goroutine can
					// assume. The channel is UNBUFFERED, so with no consumer
					// this send wedged on the very FIRST mapping — measured.
					/*
					 * THIS ARM IS DEFENSIVE, AND NOT COVERED BY A TEST --
					 * said plainly rather than left for the next reviewer
					 * to measure.
					 *
					 * Reaching it needs mapCtx cancelled while `abandoned`
					 * is still open, with an item in hand. mapCtx is
					 * cancelled mappingDrainGrace (15s) AFTER ctx is done,
					 * and Abandon fires the moment Start returns -- which
					 * on the no-consumer path is immediately, at its gate.
					 * So the outer arm below wins by about fifteen
					 * seconds every time, and the wedge table cannot drive
					 * this one without sleeping out that grace period for
					 * a state the current ordering cannot produce.
					 *
					 * Kept because "the ordering makes it unreachable" is
					 * a property of two other functions, not of this one,
					 * and the cost of being wrong about that is a
					 * permanent goroutine leak. MEASURED: deleting it
					 * leaves the suite green.
					 */
					select {
					case mappingChan <- m:
					case <-abandoned:
						r.logger.Warn("dropped an in-flight test-mock mapping on shutdown: recording stopped before any consumer started",
							zap.String("testName", m.TestName),
							zap.String("next_step", "mappings.yaml may be missing this test; re-record the test set if you need it"))
					}
					return ctx.Err()
				case mappingChan <- m:
				case <-abandoned:
					// See the incoming forwarder. This one matters most: mapCtx
					// is not cancelled until mappingDrainGrace (15s) after ctx
					// is done, so without this arm an abandoned mapping
					// forwarder holds the errgroup for that whole window before
					// it even reaches the hand-over.
					r.logger.Warn("dropped a test-mock mapping: recording stopped before any consumer started",
						zap.String("testName", m.TestName),
						zap.String("next_step", "mappings.yaml may be missing this test; re-record the test set if you need it"))
					return nil
				}
			}
		}
	})

	handedOff = true
	return FrameChan{
		Incoming: incomingChan,
		Outgoing: outgoingChan,
		Mappings: mappingChan,
		Abandon:  abandon,
	}, nil

}

func (r *Recorder) RunApplication(ctx context.Context, appID uint64, opts models.RunOptions) models.AppError {
	return r.instrumentation.Run(ctx, opts)
}

func (r *Recorder) GetNextTestSetID(ctx context.Context) (string, error) {
	testSetIDs, err := r.testDB.GetAllTestSetIDs(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get test set IDs: %w", err)
	}

	if r.config.Record.Metadata == "" {
		return pkg.NextID(testSetIDs, models.TestSetPattern), nil
	}
	r.config.Record.Metadata = utils.TrimSpaces(r.config.Record.Metadata)
	meta, err := utils.ParseMetadata(r.config.Record.Metadata)
	if err != nil || meta == nil {
		return pkg.NextID(testSetIDs, models.TestSetPattern), nil
	}

	nameVal, ok := meta["name"]
	requestedName, isStr := nameVal.(string)
	if !ok || !isStr || requestedName == "" {
		return pkg.NextID(testSetIDs, models.TestSetPattern), nil
	}

	existingIDs := make(map[string]struct{}, len(testSetIDs))
	for _, id := range testSetIDs {
		existingIDs[id] = struct{}{}
	}

	if _, occupied := existingIDs[requestedName]; !occupied {
		return requestedName, nil
	}

	var highestSuffix int
	namePrefix := requestedName + "-"
	for id := range existingIDs {
		if !strings.HasPrefix(id, namePrefix) {
			continue
		}
		suffixPart := id[len(namePrefix):]
		if n, err := strconv.Atoi(suffixPart); err == nil && n > highestSuffix {
			highestSuffix = n
		}
	}

	newSuffix := highestSuffix + 1
	assignedName := fmt.Sprintf("%s-%d", requestedName, newSuffix)

	r.logger.Info(fmt.Sprintf(
		"Test set name '%s' already exists, using '%s' instead. You can change this name if you want.",
		requestedName, assignedName,
	))

	return assignedName, nil
}

func (r *Recorder) createConfigWithMetadata(ctx context.Context, testSetID string) {
	// Parse metadata from the config
	metadata, err := utils.ParseMetadata(r.config.Record.Metadata)
	if err != nil {
		utils.LogError(r.logger, err, "failed to parse metadata", zap.String("metadata", r.config.Record.Metadata))
		return
	}
	testSet := &models.TestSet{
		PreScript:  "",
		PostScript: "",
		Template:   make(map[string]interface{}),
		Metadata:   metadata,
	}

	err = r.testSetConf.Write(ctx, testSetID, testSet)
	if err != nil {
		utils.LogError(r.logger, err, "Failed to create test-set config file with metadata", zap.String("testSet", testSetID))
		return
	}

	r.logger.Info("Created test-set config file with metadata")
}
