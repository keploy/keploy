package manager

import (
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
const defaultMockBufferCapacity = 100

// maxRecentWindows bounds the recently-resolved-window ring (see
// SyncMockManager.recentWindows). The ring is also time-pruned to the
// 7 s staleness horizon, so this cap only matters under a burst of very
// short test windows; 256 covers far more than the ~7 s of history the
// staleness cutoff keeps reachable, while staying O(1) memory.
const maxRecentWindows = 256

// maxPressureRanges bounds SyncMockManager.pressureRanges by COUNT, not by
// wall-clock age. An earlier version time-pruned closed ranges 7 s after they
// ended, on the assumption that nothing older could still be queried. That is
// wrong for the orphan-TC suppression in routes/record.go: the test-case stream
// lags the recorder — a backed-up channel plus a slow CLI drain routinely puts
// it MORE than 7 s behind — so a range that caused a mock drop is reaped before
// the TC whose window overlaps it is ever checked, and the orphan is persisted
// (replay then fails match_phase=no_mocks). Retention therefore must be
// independent of how far record.go lags. Keep the newest maxPressureRanges
// intervals and evict the oldest: memoryguard opens at most one range per
// pause/resume cycle (a few per second of sustained pressure at most), so this
// holds hundreds of recording sessions' worth of history — far more than
// record.go could ever lag behind — while staying O(1) memory and keeping the
// O(n) WasPressureActiveInWindow / pressureActiveAtLocked scans bounded.
const maxPressureRanges = 8192

// maxDroppedTCNames bounds SyncMockManager.droppedTCNames /
// droppedTCOrder. COUNT-bounded (like maxPressureRanges), not age-bounded:
// record.go can lag the recorder, so a dropped owner name must stay
// queryable until the lagging TC is checked.
const maxDroppedTCNames = 8192

// resolvedWindow is one already-resolved per-test window retained so a
// late-arriving mock (decoded after the window closed) can still be
// attributed to the test that actually owns its ReqTimestampMock.
type resolvedWindow struct {
	start    time.Time
	end      time.Time
	testName string
	mapping  bool
}

// nopLogger is the fallback when no logger has been installed via
// SetLogger. zap.L() is NOT safe here — it returns a Nop until the
// host process has called zap.ReplaceGlobals, and syncMock is a
// package-level singleton that loads before any such bootstrap.
// Using a shared Nop avoids per-call allocation on the drop path.
var nopLogger = zap.NewNop()

func generateRandomString(n int) string {
	sb := make([]byte, n)
	for i := range sb {
		sb[i] = charset[rand.Intn(len(charset))]
	}
	return string(sb)
}

type SyncMockManager struct {
	// mu guards buffer, firstReqSeen, memoryPause, mappingChan,
	// recentWindows, resolvedTestCount.
	mu           sync.Mutex
	buffer       []*models.Mock
	mappingChan  chan<- models.TestMockMapping
	firstReqSeen bool
	memoryPause  bool

	// resolvedTestCount is the number of UNIQUE recorded test cases resolved so
	// far (incremented once per kept ResolveRange — duplicates skipped by static
	// dedup resolve with keep=false and do NOT advance it). It defines the
	// startup window: while it is < models.StartupMockTestCaseWindow, every mock
	// AddMock ingests is tagged TestModeInfo.IsStartup, so the dedup reapers
	// (DeleteMocksStrictlyBefore, the ResolveRange keep=false / out-of-window
	// rescues, the memory-pressure wipe) preserve it instead of pruning. The
	// effect is that static-dedup pruning only begins from the (N+1)-th test
	// case, keeping the boot-through-Nth-test mock corpus complete. Mirrors
	// firstReqSeen's lifecycle (set forward-only within a record session).
	resolvedTestCount int

	// recentWindows is a bounded, time-pruned ring of the most recently
	// RESOLVED per-test windows. It exists to close the async-emit vs
	// window-bin race: a parser decodes/emits a mock a few ms after the
	// real wire event (the presaved ReqTimestampMock is correct, but the
	// mock lands in the buffer late). If that mock's ReqTimestampMock
	// falls inside a window whose ResolveRange already fired — the
	// classic case is a Mongo cursor getMore that the app issued WHILE
	// producing a response, but whose decode finished after the response
	// was captured and the window closed — the direct [start,end] match
	// in ResolveRange misses it, and since every FUTURE window starts
	// after the previous one ended, it can never match again and is
	// stale-dropped. By remembering recent windows we retroactively bin
	// such late arrivals into the window that actually owns them, so the
	// recorder persists them with their (correct, in-window) timestamps
	// and replay's timestamp filter picks them up for the right test.
	// Pruned to the same staleness horizon as the buffer cutoff so it
	// can't reattach ancient mocks or grow without bound.
	recentWindows []resolvedWindow

	// outChanMu guards outChan and outChanClosed together. Senders
	// RLock across the whole read+send; the closer Locks across the
	// close. This is the only lock protecting outChan — see commit
	// history of #4045 for the data race this serializes against.
	outChanMu     sync.RWMutex
	outChan       chan<- *models.Mock
	outChanClosed bool

	// unboundWarnOnce fires a single warning the first time a mock is buffered
	// while outChan was never wired (a New() manager whose owner forgot to call
	// SetOutputChannel). Without it the failure is silent: mocks pile up in the
	// buffer and are never emitted.
	unboundWarnOnce sync.Once

	// dropCount tracks send-path drops caused by outChan being full
	// past the bounded send budget. Sampled to an Error so customers
	// get a loud signal without the log-flood anti-pattern. Using
	// the typed atomic.Uint64 wrapper removes the 32-bit-alignment
	// footgun that a bare uint64 + sync/atomic.AddUint64 would carry
	// if this struct ever got embedded or reordered.
	dropCount atomic.Uint64

	// droppedMu guards droppedTCNames / droppedTCOrder. It is a DEDICATED
	// LEAF lock: it is only ever taken while (optionally) holding
	// outChanMu.RLock, and it takes no other lock while held — so it can
	// never participate in a lock-ordering cycle with m.mu or outChanMu.
	// Do NOT reuse m.mu or outChanMu here.
	//
	// droppedTCNames is the set of test-case names that OWNED a mock which
	// was dropped on the outChan capacity path (send-budget exhaustion or an
	// already-closed channel). Unlike the memory-pressure path — which
	// records pressureRanges so record.go suppresses the overlapping TC — a
	// capacity drop feeds nothing into pressureRanges, so the owning TC would
	// otherwise reach replay mock-less (match_phase=no_mocks). record.go
	// queries this set by EXACT test name (WasMockDroppedForTC) and suppresses
	// any TC in it, so suppression cannot over-suppress a concurrent TC.
	// droppedTCOrder is the FIFO insertion order used to evict the oldest name
	// once the set exceeds maxDroppedTCNames (count-bounded, see the const).
	droppedMu      sync.Mutex
	droppedTCNames map[string]struct{}
	droppedTCOrder []string

	// revokeCapable gates the deferred-orphan revoke protocol: it is set true
	// (via SetRevokeCapable) only when the CLI negotiated
	// OutgoingOptions.SupportsDroppedRevoke on the /outgoing request. When
	// false (an older CLI, or the default), recordDroppedTC queues NOTHING and
	// drainPendingRevokes sends NOTHING — so a CLI that can't divert the
	// reserved Kind=RevokedTests control frame never receives one. atomic so
	// the send path can read it without taking a lock.
	revokeCapable atomic.Bool

	// pendingRevokes is the FIFO of dropped-TC names still to be emitted to the
	// CLI as RevokedTests control frames. Appended by recordDroppedTC when a
	// NEW capacity-drop owner is recorded AND revokeCapable is set; drained by
	// drainPendingRevokes on every FlushOwnedWindows tick and at CloseOutChan.
	// Guarded by the SAME droppedMu leaf lock as droppedTCNames (it fits the
	// leaf discipline — droppedMu takes no other lock while held), so a drop
	// records the owner and queues the revoke under one lock acquisition.
	pendingRevokes []string

	// testCounter generates this session's sequential test IDs
	// ("test-1", "test-2", …). Per-instance so concurrent capture
	// sessions in one process number their testcases independently.
	// On the per-session path this replaces the package-global
	// conn.GlobalTestCounter (see NextTestID).
	testCounter atomic.Int64

	// dedupQueue is this session's private dedup FIFO. Per-instance so
	// concurrent capture sessions don't share dedup ordering. The
	// package-global instance leaves this nil, and DedupQueue() falls back
	// to the package-global queue — preserving single-session behaviour.
	dedupQueue *DedupQueue
	// pressureDropped / totalAdded track mocks dropped vs added under memory
	// pressure. Both are atomic so they can be read without holding m.mu.
	pressureDropped atomic.Int64
	totalAdded      atomic.Int64

	// outChanClosedDrops counts mocks that were already counted in
	// totalAdded (they passed the pressure gate) but were then dropped
	// because the outChan was already closed by CloseOutChan — i.e.
	// the mock arrived AFTER shutdown sealed the stream. This is a
	// real post-count drop, so without this counter totalAdded would
	// over-report deliverable mocks. Accounting identity at shutdown:
	//   totalAdded = forwarded + still_in_buffer + outChanClosedDrops
	//                + sendBudget drops (dropCount)
	outChanClosedDrops atomic.Int64

	// pressureRanges records every [start, end] interval during which memory
	// pressure was active. Appended on the false→true transition in
	// SetMemoryPressure, closed on the true→false transition. The most recent
	// entry has end == zero while pressure is still active. Guarded by mu.
	//
	// This is the join key for the Bug 0 TC-suppression fix:
	// WasPressureActiveInWindow checks whether any range overlaps a TC's
	// [HTTPReq.Timestamp, HTTPResp.Timestamp] window. Using pressure INTERVALS
	// instead of per-mock drop timestamps is what makes suppression parser-
	// agnostic: any parser (mongo in keploy/integrations, postgres, http…) that
	// drops captured bytes on memoryguard.IsRecordingPaused() drops within an
	// interval this slice records, so record.go catches the overlap without the
	// parser reporting anything. Note the ordering is NOT a strict happens-before
	// on m.memoryPause: memoryguard flips the GLOBAL recordingPaused flag (which
	// the parser reads) just BEFORE it calls SetMemoryPressure to open the range,
	// so there is a sub-microsecond gap where a parser could drop with no range
	// yet recorded. Correctness does not rely on that gap being closed — it rests
	// on record.go querying the range much later (by which time it exists), and
	// on the drop preceding the mock's HTTPResp.Timestamp by far more than the
	// open latency, so the TC window still overlaps the recorded interval.
	//
	// Bounded, not unbounded: SetMemoryPressure caps the slice at
	// maxPressureRanges by COUNT (evicting the oldest), NOT by wall-clock age.
	// Age-based pruning would reap a range before a lagging routes/record.go
	// could check the TC it orphaned; a count cap is independent of that lag
	// while still keeping continuous recording from accumulating unbounded
	// intervals and slowing every overlap scan.
	pressureRanges []pressureRange

	// orphanRanges records every [start, end] wire window over which a mock was
	// VOIDED for a reason OTHER than memory pressure — a parser marked its mock
	// incomplete (client/server reassembly overflow, a decode error on the
	// realignment tail after a memory-pressure gap, per-conn cap, short write)
	// so supervisor.emitMockCore dropped it, or the parser reported the failing
	// operation's window directly. It is the exact complement of pressureRanges:
	// memory-pressure drops happen while memoryguard.IsRecordingPaused() is true
	// and therefore fall inside a pressureRanges interval, but these voids happen
	// while recording is NOT paused, so WasPressureActiveInWindow structurally
	// cannot see them and the orphaned TC would reach replay mock-less
	// (match_phase=no_mocks). WasMockOrphanedInWindow checks overlap against a
	// TC's [HTTPReq.Timestamp, HTTPResp.Timestamp] window with the same interval
	// semantics as pressureRanges, and record.go ORs the two. Count-bounded at
	// maxPressureRanges (oldest evicted), guarded by mu — a continuous stream of
	// voids can't grow it unbounded or slow the overlap scan. Entries always
	// carry a concrete end (RecordOrphanWindow clamps a zero/degenerate end to
	// start), so no open-interval "extend to now" handling is needed here.
	orphanRanges []pressureRange

	// loggerMu guards logger so SetLogger and the drop path can run
	// concurrently without a data race. The read lock is taken only
	// on the (sampled, cold) Error path, so contention is negligible.
	loggerMu sync.RWMutex
	logger   *zap.Logger
}

// pressureRange is one [start, end] interval during which memory pressure was
// active. end is zero while the interval is still open (pressure not cleared yet).
type pressureRange struct {
	start, end time.Time
}

// ownedMock pairs a buffered mock with the name of the test case that owns it,
// so the send path can record the OWNING TC when a capacity drop occurs. owner
// is "" for mocks not owned by a specific test (session/connection/startup/
// anonymous carve-outs) — those record nothing on a drop.
type ownedMock struct {
	mock  *models.Mock
	owner string
}

// Global instance is initialized at package load time
var instance = &SyncMockManager{
	buffer:       make([]*models.Mock, 0, defaultMockBufferCapacity),
	firstReqSeen: false,
}

// Get returns the global manager.
func Get() *SyncMockManager {
	return instance
}

// New constructs an independent SyncMockManager with its own buffer, window
// ring, drop counter, and per-session dedup queue. It shares no state with the
// package global returned by Get(). Use it when a single process runs more
// than one concurrent capture session (e.g. the enterprise multi-app DaemonSet
// agent, where each app owns its own manager); Get() remains the single-session
// default and is unchanged.
//
// The returned manager has NO output channel wired: callers MUST call
// SetOutputChannel before mocks are added, otherwise AddMock buffers every mock
// and nothing is ever emitted (a one-time warning is logged if a mock arrives
// while still unwired).
//
// Per-app isolation of dedup and static-dedup is OPT-IN by the consumer. This
// manager owns a private DedupQueue() and the package exposes the
// WithStaticDeduper / StaticDeduperFromContext context seam, but OSS code paths
// do not consult them — they use the package globals. The isolation only
// materializes once a multi-app consumer threads mgr.DedupQueue() into
// ResolveJob and the per-app deduper through the parser context.
func New(logger *zap.Logger) *SyncMockManager {
	m := &SyncMockManager{
		buffer:       make([]*models.Mock, 0, defaultMockBufferCapacity),
		firstReqSeen: false,
		dedupQueue:   NewDedupQueue(),
	}
	if logger != nil {
		m.logger = logger
	}
	return m
}

// DedupQueue returns this manager's dedup queue: its own private one for
// instances built by New(), or the package-global queue for the single-session
// default instance (which leaves dedupQueue nil). It is the per-app isolation
// carrier — a multi-app consumer calls mgr.DedupQueue() and threads the result
// into ResolveJob so one app's dedup FIFO can't bleed into another's. OSS code
// paths use the package-global GetDedupQueue() and never call this, so the
// isolation only materializes once the consumer opts in.
func (m *SyncMockManager) DedupQueue() *DedupQueue {
	if m == nil || m.dedupQueue == nil {
		return globalDedupQueue
	}
	return m.dedupQueue
}

// NextTestID returns this session's next sequential test ID. Per-instance
// so two concurrent capture sessions number testcases independently
// (each starts at 1). On the single-session path it runs against the
// package-global manager, reproducing the old conn.GlobalTestCounter
// behaviour exactly.
func (m *SyncMockManager) NextTestID() int64 {
	return m.testCounter.Add(1)
}

// SetOutputChannel plugs an outgoing mock channel into the manager.
// Only resets outChanClosed when the channel pointer changes —
// re-setting the same pointer after CloseOutChan must NOT reopen
// the closed flag, otherwise a subsequent send would hit a
// post-close channel and panic. The proxy calls this once per
// accepted connection with rule.MC (same channel across the whole
// session), so idempotent same-channel calls are the hot path.
// A distinct channel pointer means a new session (re-record), and
// only then do we clear the closed flag.
func (m *SyncMockManager) SetOutputChannel(out chan<- *models.Mock) {
	m.outChanMu.Lock()
	defer m.outChanMu.Unlock()
	if out != m.outChan {
		m.outChan = out
		m.outChanClosed = false
		// New session (distinct channel = re-record): drop any revokes still
		// queued from a prior session so a name orphaned there can't be
		// delivered onto THIS session's /outgoing stream. droppedMu is a leaf;
		// taking it here under outChanMu.Lock keeps the same outChanMu→droppedMu
		// order the send path already uses (outChanMu.RLock→recordDroppedTC), so
		// there is no new lock-ordering hazard.
		m.droppedMu.Lock()
		m.pendingRevokes = nil
		m.droppedMu.Unlock()
	}
}

func (m *SyncMockManager) SetMappingChannel(ch chan<- models.TestMockMapping) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mappingChan = ch
}

// SetLogger installs a zap.Logger for drop-path reporting. Callers
// are expected to wire this once during proxy bootstrap with a
// process-scoped logger; nil clears it back to the shared Nop.
// Safe to call concurrently with the send path.
func (m *SyncMockManager) SetLogger(l *zap.Logger) {
	if m == nil {
		return
	}
	m.loggerMu.Lock()
	defer m.loggerMu.Unlock()
	m.logger = l
}

// dropLogger returns the active logger for the drop path, falling
// back to the shared Nop so callers never have to nil-check and an
// unwired manager is still safe to report against.
func (m *SyncMockManager) dropLogger() *zap.Logger {
	m.loggerMu.RLock()
	defer m.loggerMu.RUnlock()
	if m.logger == nil {
		return nopLogger
	}
	return m.logger
}

// sendBudget is how long sendToOutChan will wait for outChan to drain
// before dropping the mock. 200 ms is sized conservatively: large
// enough to absorb a GC pause on an oversubscribed CI runner or a
// transient downstream consumer stall, small enough that shutdown
// latency (CloseOutChan grabbing the write lock) is imperceptible.
// The historical code used a non-blocking send with `default: drop`
// which silently lost pre-first-request mocks under burst —
// customers saw "some calls didn't replay" with no actionable
// signal. See the commit that introduced this budget for the
// customer-facing flake it resolves.
const sendBudget = 200 * time.Millisecond

// sendDropSampleRate controls Warn emission under sustained overflow.
// Emitting per-drop under a stuck consumer would flood the log and
// further starve the very goroutine we're trying to let catch up —
// the same anti-pattern the Windows redirector hit. Sample every Nth
// drop so operators still see "something is wrong" without the
// recorder loop being drowned by its own logging.
const sendDropSampleRate uint64 = 1024

// sendToOutChan is the single send path to outChan. Holds outChanMu
// read-lock across the whole observation + send so CloseOutChan (the
// writer-lock holder) cannot interleave a close between our
// not-closed check and the chansend.
//
// Tries a non-blocking send first (the fast, jitter-free common case
// where the consumer is keeping up). When the channel is momentarily
// full, falls through to a bounded block (sendBudget) before
// dropping. Holding the read-lock across the bounded wait only
// lengthens CloseOutChan's shutdown path by at most sendBudget —
// acceptable because every RLock holder is doing the same thing. The
// alternative (silent drop after zero wait) was the source of a
// customer-facing recording-loss flake and is strictly worse than a
// 200 ms worst-case shutdown delay.
func (m *SyncMockManager) sendToOutChan(mock *models.Mock) {
	m.sendToOutChanOwned(mock, "")
}

// sendToOutChanOwned is sendToOutChan with the owning test name threaded
// through so a capacity drop can be attributed to the TC that owns the mock.
// When owner != "" and the mock is genuinely undeliverable — the outChan is
// closed/nil, or the bounded send budget is exhausted — the owner is recorded
// via recordDroppedTC so record.go suppresses (rather than streams) that TC at
// replay. owner == "" (session/connection/startup/anonymous) records nothing.
// See sendToOutChan's doc comment for the locking rationale.
func (m *SyncMockManager) sendToOutChanOwned(mock *models.Mock, owner string) {
	m.outChanMu.RLock()
	defer m.outChanMu.RUnlock()
	if m.outChanClosed || m.outChan == nil {
		// Genuinely undeliverable → a real drop. Record the owner (under the
		// outChanMu.RLock already held; droppedMu is a leaf lock) so the
		// orphaned TC is suppressed instead of reaching replay mock-less.
		if owner != "" {
			m.recordDroppedTC(owner)
		}
		return
	}
	select {
	case m.outChan <- mock:
		return
	default:
	}
	// Fast path full. Bounded block so normal scheduling jitter
	// doesn't cost us a mock.
	timer := time.NewTimer(sendBudget)
	select {
	case m.outChan <- mock:
		timer.Stop()
	case <-timer.C:
		n := m.dropCount.Add(1)
		if owner != "" {
			m.recordDroppedTC(owner)
		}
		// The existing per-1024 sampled Error fires at n==1 AND every
		// subsequent 1024th drop. Per-Copilot review on #4176, the
		// "your recording is now lossy" wording lives on the same n==1
		// branch rather than as a separate Warn so operators see one
		// clear signal at the moment capture goes lossy, instead of
		// two separate lines that may interleave with other logs.
		// Subsequent sampled emissions stay terse to avoid drowning a
		// stuck consumer's goroutine in its own logging.
		if n == 1 || n%sendDropSampleRate == 0 {
			msg := "syncMock outChan overflow; mock dropped — consumer can't keep up with mock production"
			if n == 1 {
				msg = "syncMock outChan overflow on FIRST drop — mock recording is now LOSSY for this session; subsequent overflow drops are silent except for the per-1024 sampled line. Reduce concurrent test load, upgrade to a release with a larger outChan capacity, or investigate consumer-side stalls (slow disk / network to k8s-proxy) before re-running for a clean recording."
			}
			m.dropLogger().Error(msg,
				zap.Uint64("dropsSoFar", n),
				zap.Int("outChanCap", cap(m.outChan)),
				zap.Duration("budget", sendBudget),
			)
		}
	}
}

// trySendControlFrame attempts a NON-blocking send of a reserved-Kind control
// frame (a revoke) on outChan. Returns true iff delivered. Unlike
// sendToOutChanOwned it does NOT bump dropCount and does NOT record a drop or
// fire the "recording is lossy" log — a control frame is not a mock. If the
// channel is closed/nil or full, it returns false and the caller re-queues.
func (m *SyncMockManager) trySendControlFrame(mock *models.Mock) bool {
	m.outChanMu.RLock()
	defer m.outChanMu.RUnlock()
	if m.outChanClosed || m.outChan == nil {
		return false
	}
	select {
	case m.outChan <- mock:
		return true
	default:
		return false
	}
}

// drainPendingRevokes emits a revoke control frame for every test case queued
// by recordDroppedTC. It SNAPSHOTS the queue under droppedMu, releases the
// lock, then sends — holding droppedMu across trySendControlFrame (which takes
// outChanMu.RLock) would invert the leaf-lock order and can 3-way deadlock
// against CloseOutChan's outChanMu.Lock under load. Undelivered names are
// re-queued for the next tick (eventual delivery while the stream is open).
func (m *SyncMockManager) drainPendingRevokes() {
	if m == nil || !m.revokeCapable.Load() {
		return
	}
	m.droppedMu.Lock()
	if len(m.pendingRevokes) == 0 {
		m.droppedMu.Unlock()
		return
	}
	batch := m.pendingRevokes
	m.pendingRevokes = nil
	m.droppedMu.Unlock()

	frame := &models.Mock{
		Kind: models.RevokedTests,
		Spec: models.MockSpec{Metadata: map[string]string{"revoked_tests": strings.Join(batch, ",")}},
	}
	if !m.trySendControlFrame(frame) {
		// Re-queue the whole batch under a fresh lock (new drops may have
		// appended meanwhile — keep the retries ahead of them).
		m.droppedMu.Lock()
		m.pendingRevokes = append(batch, m.pendingRevokes...)
		m.droppedMu.Unlock()
	}
}

// DropCount exposes the cumulative drop counter for tests and
// external observability. The value is monotonically increasing;
// readers that need a delta should snapshot and diff.
func (m *SyncMockManager) DropCount() uint64 {
	if m == nil {
		return 0
	}
	return m.dropCount.Load()
}

// recordDroppedTC remembers that a mock owned by test `name` was dropped on the
// outChan capacity path. Idempotent per name (the set dedups) and count-bounded
// FIFO at maxDroppedTCNames: past the cap the oldest name is evicted so a long
// recording can't leak unbounded. Takes only droppedMu (a leaf lock).
func (m *SyncMockManager) recordDroppedTC(name string) {
	if m == nil || name == "" {
		return
	}
	m.droppedMu.Lock()
	defer m.droppedMu.Unlock()
	if m.droppedTCNames == nil {
		m.droppedTCNames = make(map[string]struct{})
	}
	if _, ok := m.droppedTCNames[name]; ok {
		return
	}
	m.droppedTCNames[name] = struct{}{}
	m.droppedTCOrder = append(m.droppedTCOrder, name)
	if len(m.droppedTCOrder) > maxDroppedTCNames {
		delete(m.droppedTCNames, m.droppedTCOrder[0])
		k := copy(m.droppedTCOrder, m.droppedTCOrder[1:])
		m.droppedTCOrder = m.droppedTCOrder[:k]
	}
	// Deferred-orphan revoke: if the TC already streamed to the CLI, a drop now
	// is undetectable by stream-time suppression, so queue the name for LIVE
	// delivery as a RevokedTests control frame. Only when the CLI negotiated the
	// capability (revokeCapable) — an older CLI would mis-persist the frame.
	// Queued under the already-held droppedMu; the name is NEW here (the dedup
	// return above guarantees it), so pendingRevokes never accumulates dupes.
	if m.revokeCapable.Load() {
		m.pendingRevokes = append(m.pendingRevokes, name)
	}
}

// SetRevokeCapable enables (or disables) emission of RevokedTests control
// frames for the deferred-orphan revoke protocol. The agent service calls it
// from GetOutgoing with OutgoingOptions.SupportsDroppedRevoke, so emission is
// gated strictly on what the connecting CLI negotiated — default false means an
// older CLI never triggers a revoke frame.
func (m *SyncMockManager) SetRevokeCapable(v bool) {
	if m == nil {
		return
	}
	m.revokeCapable.Store(v)
}

// WasMockDroppedForTC reports whether a mock owned by test `name` was dropped on
// the outChan capacity path. record.go calls this by EXACT test name so a
// suppression can never spill onto a concurrent TC that kept all its mocks.
func (m *SyncMockManager) WasMockDroppedForTC(name string) bool {
	if m == nil || name == "" {
		return false
	}
	m.droppedMu.Lock()
	defer m.droppedMu.Unlock()
	_, ok := m.droppedTCNames[name]
	return ok
}

// DroppedTCCount returns the number of distinct test cases that lost a mock to
// a capacity drop. Exposed for the session-summary log in routes/record.go.
func (m *SyncMockManager) DroppedTCCount() int {
	if m == nil {
		return 0
	}
	m.droppedMu.Lock()
	defer m.droppedMu.Unlock()
	return len(m.droppedTCNames)
}

func (m *SyncMockManager) AddMock(mock *models.Mock) {
	// Unification (Phase 1): resolve the live mock's Lifetime immediately
	// on entry so the buffered mock carries a correctly-typed
	// TestModeInfo.Lifetime into whichever downstream consumer drains
	// syncMock next (persistence writer, downstream agent via outChan,
	// etc.). Cheap — single map probe — and removes the need for
	// downstream code to call DeriveLifetime defensively.
	if mock != nil {
		mock.DeriveLifetime()
	}
	m.mu.Lock()
	if m.memoryPause {
		// Pressure is on at decode time, but this mock may have been decoded
		// late for a request that happened during calm — its TC was captured
		// at the ingress, so dropping the mock would orphan it. Decide by the
		// request time, not now: drop only if the request ITSELF happened
		// during pressure (the ingress never captured it, so there is no TC).
		if m.pressureActiveAtLocked(mock.Spec.ReqTimestampMock) {
			m.mu.Unlock()
			m.pressureDropped.Add(1)
			return
		}
		// Request was during calm → its TC was captured → keep this mock;
		// fall through to the normal buffer/forward path.
	}
	// Mock is being kept — count it as successfully added.
	m.totalAdded.Add(1)

	// Tag startup-window traffic. A mock is "startup" when it is captured
	// either (a) before the first inbound request — classic app-bootstrap
	// traffic (e.g. an AWS Secret Manager fetch at boot) that ran before any
	// test window exists — or (b) while we are still inside the startup window,
	// i.e. fewer than models.StartupMockTestCaseWindow unique test cases have
	// been recorded. Case (b) widens the old "before firstReqSeen" rule so the
	// boot-through-Nth-test mock corpus is preserved wholesale: the IsStartup
	// tag is the single signal every reaper below keys off (dedup DeleteMocks-
	// StrictlyBefore, the ResolveRange keep=false / out-of-window / stale-cutoff
	// rescues, FlushOwnedWindows, and the memory-pressure wipe), so tagging here
	// makes static-dedup pruning a no-op until the (N+1)-th test case. (firstReqSeen
	// is subsumed by the count test — count is 0 before the first request — but
	// is kept explicit so the boot case still holds if the window is ever 0.)
	if mock != nil && (!m.firstReqSeen || m.resolvedTestCount < models.StartupMockTestCaseWindow) {
		mock.TestModeInfo.IsStartup = true
	}

	// Decide forward vs buffer vs drop under a single snapshot of
	// (outChan, outChanClosed). The trio has three legal outcomes:
	//
	//   closed          → drop (shutdown in progress, buffer would leak)
	//   unbound (nil)   → buffer (SetOutputChannel hasn't fired yet;
	//                     ResolveRange will emit once bound)
	//   bound + open    → forward via sendToOutChan, unless we're
	//                     past firstReqSeen in which case the mock
	//                     belongs in the dedup buffer for windowing
	//
	// Session- and connection-scoped mocks (mongo handshake/heartbeat,
	// postgres v3 startup, mysql HikariCP COM_PING) follow the same
	// branching here — they ride the buffer when firstReqSeen has
	// fired so they keep their FIFO position relative to per-test
	// mocks captured on the same connection. ResolveRange's lifetime
	// carve-out drains them to outChan without subjecting them to
	// the per-test window match (so they're not dropped by the 7 s
	// cutoff for being out-of-window) but preserves arrival order.
	// An earlier attempt forwarded session mocks to outChan straight
	// from AddMock; that broke run_fuzzer_linux / Mongo Fuzzer
	// (record_build_replay_build) because the bypass hoisted handshake
	// mocks ahead of the per-test mocks emitted at end-of-test from
	// the buffer. Replay's connection-keyed FIFO matcher then saw
	// handshakes interleaved out of order with the operations they
	// preceded and stalled on the last batch of ops. Keeping the
	// FIFO-via-buffer route fixes Mongo Fuzzer, and the carve-out in
	// ResolveRange still saves session/connection mocks from the
	// stale-cutoff drop that bit gin-mongo Windows on #4122.
	bound, closed := m.outChanStatus()
	switch {
	case closed:
		m.mu.Unlock()
		// Count this post-totalAdded drop so the accounting identity
		// holds and we can see exactly how many mocks were lost to the
		// "arrived after outChan closed" race.
		closedDrops := m.outChanClosedDrops.Add(1)
		// Per-mock diagnostic: visible signal when AddMock drops a
		// mock because the outChan has already been closed by
		// CloseOutChan. This usually only fires during shutdown but
		// in CI a poorly-ordered teardown can race the recorder's
		// final emit and silently lose mocks captured in the last
		// few milliseconds of the run. The dropLogger is the right
		// receiver — it ALWAYS resolves to a non-nil logger and is
		// safe under the m.mu unlock.
		if logger := m.dropLogger(); logger != nil {
			logger.Debug("diag/AddMock: outChan already closed, mock dropped",
				zap.String("mock_kind", string(mock.Kind)),
				zap.String("connID", mock.ConnectionID),
				zap.String("lifetime", mock.TestModeInfo.Lifetime.String()),
				zap.Time("mock_req_ts", mock.Spec.ReqTimestampMock),
				zap.Int64("outchan_closed_drops_total", closedDrops),
			)
		}
		return
	case bound && !m.firstReqSeen:
		m.mu.Unlock()
		m.sendToOutChan(mock)
		return
	default:
		m.buffer = append(m.buffer, mock)
		m.mu.Unlock()
		// !bound here means outChan was never wired (closed was handled
		// above). For the package-global manager the proxy binds outChan
		// before any AddMock, so this only trips a New() manager whose owner
		// forgot SetOutputChannel — surface it once instead of silently
		// buffering forever.
		if !bound {
			m.unboundWarnOnce.Do(func() {
				if logger := m.dropLogger(); logger != nil {
					logger.Warn("syncMock: mock buffered before SetOutputChannel was wired; if this manager's output channel is never set, buffered mocks will not be emitted — call SetOutputChannel after New()")
				}
			})
		}
	}
}

// outChanStatus snapshots (bound, closed) under outChanMu.RLock so
// AddMock's fork decision sees a consistent pair.
func (m *SyncMockManager) outChanStatus() (bound, closed bool) {
	m.outChanMu.RLock()
	defer m.outChanMu.RUnlock()
	return m.outChan != nil && !m.outChanClosed, m.outChanClosed
}

// SendConfigMock forwards a config mock directly to the outgoing
// channel, bypassing the firstReqSeen buffering that AddMock does.
// DNS is the only caller today: it recognizes queries by a
// (name, qtype) dedupe key and wants every unique query mocked the
// first time it's seen, regardless of whether the first app request
// has already fired. Shares the same outChanMu guard as AddMock so
// DNS sends also stay safe against proxy shutdown.
func (m *SyncMockManager) SendConfigMock(mock *models.Mock) {
	if m == nil {
		return
	}
	m.sendToOutChan(mock)
}

// CloseOutChan flushes any still-attributable buffered mocks and then
// closes the outgoing mock channel under the writer lock so an in-flight
// sendToOutChan cannot race the close. Idempotent; safe to call with
// outChan still nil.
//
// The final FlushOwnedWindows is the shutdown twin of the periodic flush
// ticker the proxy runs while recording is live (see proxy.go): that
// ticker stops one step earlier in the shutdown sequence (its
// clientConnCancel), so a mock that finished decoding after the ticker's
// last tick — the classic teardown-phase late mock, a DB response still
// being decoded when recording is asked to stop — would otherwise sit in
// the buffer and be discarded here, orphaning its already-recorded test
// case at replay. Flushing first persists every mock the buffer can still
// attribute (session/connection mocks and per-test mocks whose
// ReqTimestampMock falls inside an already-resolved window).
//
// FlushOwnedWindows takes outChanMu.RLock (via sendToOutChan); it runs to
// completion and releases that lock BEFORE we take the write lock below,
// so the two never deadlock. The proxy calls CloseOutChan only after all
// connection handlers have drained, so no new mocks enter the buffer
// between the flush and the close.
func (m *SyncMockManager) CloseOutChan() {
	if m == nil {
		return
	}
	// Graceful-stop drain: flush every still-attributable buffered mock
	// before sealing the channel. The periodic flush ticker stops one step
	// earlier in shutdown, so a late-decoded teardown mock would otherwise be
	// discarded here and orphan its test case.
	m.FlushOwnedWindows()

	m.outChanMu.Lock()
	defer m.outChanMu.Unlock()
	if m.outChanClosed {
		return
	}
	if m.outChan != nil {
		close(m.outChan)
	}
	m.outChanClosed = true
}

// FlushOwnedWindows forwards every buffered mock that can be attributed
// right now — session/connection mocks (reusable, never window-bound) and
// per-test mocks whose ReqTimestampMock falls inside an already-resolved
// window — leaving only not-yet-attributable per-test mocks in the buffer
// for a future window match. It is the request-independent twin of
// ResolveRange's flush branches: the proxy calls it on a ticker for the
// life of a recording session (see proxy.go) so a mock that lands AFTER
// its HTTP window resolved (a multi-MB Mongo document still decoding when
// the response was captured) is persisted WHILE recording is live. The
// only other drains are request-driven, so after the final request such a
// mock would otherwise wait until shutdown — by which point the recorder
// ctx is cancelled and the relay, consumer, and InsertMock all drop it.
// Order within the buffer is preserved for the mocks left behind.
func (m *SyncMockManager) FlushOwnedWindows() {
	if m == nil {
		return
	}

	var mocksToSend []ownedMock
	var lateMappings map[string][]string

	m.mu.Lock()
	outChanBound, _ := m.outChanStatus()
	if !outChanBound {
		m.mu.Unlock()
		// Still drain pending revokes: an unbound outChan means
		// trySendControlFrame can't deliver, so the batch is re-queued for a
		// later tick — but the drain must be REACHED on every tick regardless
		// of buffer/channel state so the tail can't be starved.
		m.drainPendingRevokes()
		return
	}
	mappingChan := m.mappingChan

	ownerWindow := func(t time.Time) (resolvedWindow, bool) {
		for _, w := range m.recentWindows {
			if !t.Before(w.start) && !t.After(w.end) {
				return w, true
			}
		}
		return resolvedWindow{}, false
	}

	keepIdx := 0
	for i := 0; i < len(m.buffer); i++ {
		mock := m.buffer[i]
		if mock == nil {
			continue
		}
		if lt := mock.TestModeInfo.Lifetime; lt == models.LifetimeSession || lt == models.LifetimeConnection {
			// Reusable across tests; flush verbatim (never renamed),
			// matching ResolveRange's lifetime carve-out. Owned by no
			// specific test → owner "" (a capacity drop records nothing).
			mocksToSend = append(mocksToSend, ownedMock{mock: mock})
			continue
		}
		if w, ok := ownerWindow(mock.Spec.ReqTimestampMock); ok {
			mock.Name = "mock-" + generateRandomString(8)
			if w.mapping {
				if lateMappings == nil {
					lateMappings = make(map[string][]string)
				}
				lateMappings[w.testName] = append(lateMappings[w.testName], mock.Name)
			}
			// Owned by the matched window's test → tag it so a capacity
			// drop suppresses that TC.
			mocksToSend = append(mocksToSend, ownedMock{mock: mock, owner: w.testName})
			continue
		}
		// STARTUP RESCUE: a startup-window mock the ownerWindow check above
		// didn't claim (boot traffic owns no window; an early-test mock whose
		// window hasn't resolved yet on this ticker tick). Flush it to disk
		// proactively rather than leaving it parked in the buffer where a dedup
		// cleanup, stale-cutoff, or memory-pressure wipe could reap it before it
		// is ever persisted. Owns no specific test → owner "".
		if isStartupMock(mock) {
			mock.Name = "mock-" + generateRandomString(8)
			mocksToSend = append(mocksToSend, ownedMock{mock: mock})
			continue
		}
		// Not attributable yet — a future (possibly out-of-order) request
		// may still claim it. Keep it buffered in place.
		m.buffer[keepIdx] = mock
		keepIdx++
	}
	for i := keepIdx; i < len(m.buffer); i++ {
		m.buffer[i] = nil
	}
	m.buffer = m.buffer[:keepIdx]
	m.mu.Unlock()

	// Send AFTER releasing m.mu — sendToOutChan takes outChanMu and may
	// block up to sendBudget; holding m.mu across it would wedge AddMock.
	for _, om := range mocksToSend {
		m.sendToOutChanOwned(om.mock, om.owner)
	}
	if mappingChan != nil {
		for tn, ids := range lateMappings {
			if len(ids) == 0 {
				continue
			}
			select {
			case mappingChan <- models.TestMockMapping{TestName: tn, MockIDs: ids}:
			default:
			}
		}
	}
	// Deliver any queued deferred-orphan revokes on the same open stream. Runs
	// on EVERY tick (the proxy invokes FlushOwnedWindows periodically and
	// CloseOutChan calls it once at shutdown) so a capacity-dropped TC that
	// already streamed is signalled to the CLI while the /outgoing stream is
	// still open. Snapshot-then-send inside drainPendingRevokes keeps droppedMu
	// off the send path — do NOT hoist it under m.mu.
	m.drainPendingRevokes()
}

func (m *SyncMockManager) SetFirstRequestSignaled() {
	m.mu.Lock()
	m.firstReqSeen = true
	m.mu.Unlock()
}

func (m *SyncMockManager) GetFirstReqSeen() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.firstReqSeen
}

// GetDropStats returns a snapshot of the current pressure state and drop counters.
func (m *SyncMockManager) GetDropStats() (pressureActive bool, pressureDropped int64, totalAdded int64, bufferSize int) {
	m.mu.Lock()
	pressureActive = m.memoryPause
	bufferSize = len(m.buffer)
	m.mu.Unlock()
	pressureDropped = m.pressureDropped.Load()
	totalAdded = m.totalAdded.Load()
	return
}

// WasPressureActiveInWindow returns (true, overlapCount) if memory pressure
// was active at any moment during [start, end].
//
// Called by the TC-send path in routes/record.go right before forwarding a
// TC to the CLI: if it returns true, the TC is suppressed and never reaches
// disk, so replay cannot encounter a missing-mock EOF for it.
//
// Why this is race-free unlike a per-mock-drop ledger:
//   - memoryguard calls SetMemoryPressure(true) and the range is appended
//     under mu in the SAME critical section that flips m.memoryPause = true.
//   - Any mock-parser goroutine that subsequently sees memoryPause==true
//     (and therefore drops its mock) does so BECAUSE the range was already
//     committed. The "open" event happens-before any drop it causes.
//   - The TC's HTTP window [HTTPReq.Timestamp, HTTPResp.Timestamp] is
//     bounded by wall-clock time; if any pressure range overlaps that
//     window, the TC was at risk of losing a mock to pressure regardless
//     of when AddMock actually fires for that mock.
//
// Two intervals [a, b] and [c, d] overlap iff a <= d AND c <= b. An open
// (still-active) range's end is treated as time.Now().
func (m *SyncMockManager) WasPressureActiveInWindow(start, end time.Time) (bool, int) {
	if m == nil {
		return false, 0
	}
	// Defensive: a zero start or end would either match every range or none
	// depending on direction. Refuse to make a claim on degenerate inputs —
	// the caller should fall back to "send the TC" rather than over-suppress.
	if start.IsZero() || end.IsZero() {
		return false, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	now := time.Now()
	for _, r := range m.pressureRanges {
		rEnd := r.end
		if rEnd.IsZero() {
			rEnd = now
		}
		// Standard interval-overlap test: [r.start, rEnd] vs [start, end]
		if !r.start.After(end) && !rEnd.Before(start) {
			count++
		}
	}
	return count > 0, count
}

// pressureActiveAtLocked reports whether instant t fell inside any recorded
// pressure interval. Caller MUST hold m.mu (this is the unlocked twin of
// WasPressureActiveInWindow, used on the AddMock / SetMemoryPressure paths
// that already hold the lock). A still-open interval extends to now.
func (m *SyncMockManager) pressureActiveAtLocked(t time.Time) bool {
	now := time.Now()
	for _, r := range m.pressureRanges {
		end := r.end
		if end.IsZero() {
			end = now
		}
		if !t.Before(r.start) && !t.After(end) {
			return true
		}
	}
	return false
}

// PressureRangeCount returns the total number of pressure intervals recorded
// so far. Exposed for the session-summary log in routes/record.go.
func (m *SyncMockManager) PressureRangeCount() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pressureRanges)
}

// RecordOrphanWindow records that a mock owned by some test case was voided
// over the wire window [start, end] for a NON-pressure reason (a parser marked
// it incomplete, or emitMockCore dropped it under the mockIncomplete flag).
// record.go suppresses any TC whose HTTP [req, resp] window overlaps, so the
// orphaned TC is not streamed mock-less. A degenerate window (start == end, a
// single operation instant) is valid — the overlap test treats it as a
// zero-width interval. Bounded by count like pressureRanges (oldest evicted).
//
// A zero start is IGNORED: a caller with no reliable wire timestamp must not
// poison the suppressor into over-claiming (which would silently drop healthy
// TCs). A zero or inverted end is clamped to start so the entry is a valid
// point interval rather than an empty or reversed one.
func (m *SyncMockManager) RecordOrphanWindow(start, end time.Time) {
	if m == nil || start.IsZero() {
		return
	}
	if end.IsZero() || end.Before(start) {
		end = start
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.orphanRanges = append(m.orphanRanges, pressureRange{start: start, end: end})
	// Orphan windows are recorded per voided mock — far more frequently than
	// pressure ranges (a few per second) — so avoid the reallocate-and-copy on
	// EVERY call past the cap that SetMemoryPressure can afford. Let the slice
	// grow to 2×cap, then compact IN PLACE (reuse the backing array, non-
	// overlapping src/dst) back to the newest cap. Amortized O(1) per call, zero
	// allocation once warmed, scan bounded at 2×cap, still evicting oldest-first.
	if n := len(m.orphanRanges); n > 2*maxPressureRanges {
		m.orphanRanges = append(m.orphanRanges[:0], m.orphanRanges[n-maxPressureRanges:]...)
	}
}

// WasMockOrphanedInWindow returns (true, count) if a non-pressure mock void was
// recorded overlapping [start, end] — the orphan-window twin of
// WasPressureActiveInWindow. record.go ORs the two so a TC is suppressed
// whether its mock was lost to memory pressure OR to a parser-side void. Every
// orphanRanges entry has a concrete end (RecordOrphanWindow guarantees it), so
// no open-interval handling is needed. Degenerate TC inputs are refused (return
// false) rather than over-suppressing, matching WasPressureActiveInWindow.
func (m *SyncMockManager) WasMockOrphanedInWindow(start, end time.Time) (bool, int) {
	if m == nil || start.IsZero() || end.IsZero() {
		return false, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, r := range m.orphanRanges {
		// Standard interval-overlap test: [r.start, r.end] vs [start, end].
		if !r.start.After(end) && !r.end.Before(start) {
			count++
		}
	}
	return count > 0, count
}

// OrphanRangeCount returns the number of non-pressure mock-void windows
// recorded so far. Exposed for the session-summary log in routes/record.go.
func (m *SyncMockManager) OrphanRangeCount() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.orphanRanges)
}

func (m *SyncMockManager) SetMemoryPressure(enabled bool) {
	if m == nil {
		return
	}

	// time.Now() OUTSIDE the lock: cheap, and avoids holding mu across the
	// syscall. Reused for both the new-range open AND the close, so the two
	// transitions can never produce a range with end < start due to clock skew.
	now := time.Now()

	m.mu.Lock()
	wasEnabled := m.memoryPause
	m.memoryPause = enabled

	var clearedFromBuffer int
	if enabled {
		if !wasEnabled {
			// false→true transition: open a new pressure interval.
			// memoryguard fires SetMemoryPressure(true) once per 500ms tick,
			// but only the first call (when wasEnabled is false) is a real
			// transition; subsequent ticks while pressure is held are no-ops
			// for range tracking — they would otherwise spam the slice.
			m.pressureRanges = append(m.pressureRanges, pressureRange{start: now})
		}
		// Don't wipe the whole buffer. Two classes of mock must survive:
		//   1. Startup-window mocks (IsStartup) — boot traffic and everything
		//      captured within the first StartupMockTestCaseWindow test cases
		//      (#4282). They own no resolved window and reach disk only via
		//      FlushOwnedWindows; wiping them is the exact loss the IsStartup
		//      tag exists to prevent.
		//   2. Mocks whose request happened during calm — they belong to an
		//      already-captured TC and must survive or replay orphans it
		//      (#4220 Bug-0). Only mocks whose request was during pressure are
		//      dropped: the ingress never captured those, so there is no TC to
		//      orphan.
		// Unknown timestamp → keep (safe default). In-place filter, nil the tail.
		before := len(m.buffer)
		keep := m.buffer[:0]
		for _, mk := range m.buffer {
			if mk == nil {
				continue
			}
			if isStartupMock(mk) {
				keep = append(keep, mk)
				continue
			}
			if !m.pressureActiveAtLocked(mk.Spec.ReqTimestampMock) {
				keep = append(keep, mk)
			}
		}
		for i := len(keep); i < before; i++ {
			m.buffer[i] = nil
		}
		m.buffer = keep
		clearedFromBuffer = before - len(keep)
	} else if wasEnabled {
		// true→false transition: close the most recent open interval.
		// The defensive len check covers the degenerate case where
		// SetMemoryPressure(false) is somehow called without a prior (true),
		// e.g. a partial state restore in tests.
		if n := len(m.pressureRanges); n > 0 && m.pressureRanges[n-1].end.IsZero() {
			m.pressureRanges[n-1].end = now
		}
	}

	// Bound the range history by count (see maxPressureRanges). Evict the
	// oldest intervals: routes/record.go consumes TCs oldest-first, so anything
	// beyond the newest maxPressureRanges was streamed and checked long ago.
	// Copy into a right-sized slice so the backing array does not pin the
	// evicted entries. Eviction is rare (only past thousands of pressure
	// cycles) so the allocation is not a hot path.
	if n := len(m.pressureRanges); n > maxPressureRanges {
		trimmed := make([]pressureRange, maxPressureRanges)
		copy(trimmed, m.pressureRanges[n-maxPressureRanges:])
		m.pressureRanges = trimmed
	}
	m.mu.Unlock() // NEVER hold mu while logging — logging inside a lock causes a deadlock under I/O pressure (see BUG 5: 70-minute CI hang)

	// Debug-level, and only on state TRANSITIONS (not on every 500ms memoryguard
	// tick): these are internal pressure-mechanism diagnostics. The operator-facing
	// "how many mocks were dropped" signal is surfaced once per session by the
	// recording-complete summary in routes/record.go, so Info here would be noise.
	if logger := m.dropLogger(); logger != nil {
		if enabled && !wasEnabled {
			// Pressure just turned ON (false→true transition)
			logger.Debug("agent: memory pressure activated — pressure-request mocks dropped, calm-captured kept",
				zap.Int("mocks_cleared_from_buffer", clearedFromBuffer),
				zap.Int64("mocks_dropped_so_far", m.pressureDropped.Load()),
				zap.Int64("mocks_added_so_far", m.totalAdded.Load()),
			)
		} else if !enabled && wasEnabled {
			logger.Debug("agent: memory pressure cleared",
				zap.Int64("mocks_dropped_total", m.pressureDropped.Load()),
				zap.Int64("mocks_added_total", m.totalAdded.Load()),
			)
		}
	}
}

// isStartupMock reports whether a buffered mock falls in the startup window
// and must be preserved by every reaper instead of pruned. A mock is tagged
// IsStartup at ingest in AddMock when it is captured before the first inbound
// request (classic app-bootstrap, e.g. an AWS Secret Manager fetch) OR while
// fewer than models.StartupMockTestCaseWindow unique test cases have been
// recorded. Such mocks must be flushed to disk so replay's startup tier can
// serve them, NOT reaped as duplicate debris or stale-cutoff. The tag — rather
// than a timestamp test — is deliberately the only signal: it captures the
// "recorded inside the startup window" intent precisely (including mocks that
// land inside a static-dedup duplicate's window), whereas a per-test mock that
// merely lands before its window once we are PAST the startup window (genuine
// stale cross-test bleed) is untagged and still reaped to bound buffer growth.
func isStartupMock(mk *models.Mock) bool {
	return mk != nil && mk.TestModeInfo.IsStartup
}

func (m *SyncMockManager) ResolveRange(start, end time.Time, testName string, keep bool, mapping bool) {
	// Collect mocks and mapping data under the lock, then send to the
	// outgoing channels AFTER releasing it. Holding m.mu across a
	// channel send can deadlock on ordering: a buffer-full outChan
	// would keep mu held, blocking every AddMock waiting to enqueue.
	// We have outChanMu (inside sendToOutChan) to guard the actual
	// send against close, so m.mu release here is safe.
	var mocksToSend []ownedMock
	var associatedMockIDs []string
	var mappingEntry *models.TestMockMapping
	// lateMappings accumulates mock IDs for mocks retroactively binned
	// into a PAST (already-resolved) window, keyed by that window's test
	// name. lateBinned counts them for the buffer-transition diagnostic.
	var lateMappings map[string][]string
	lateBinned := 0

	m.mu.Lock()
	// Snapshot the outChan wiring status under outChanMu (NOT m.mu)
	// so we don't race SetOutputChannel / CloseOutChan. Only the
	// bound boolean is needed — the actual send later goes through
	// sendToOutChan which reacquires the RLock and will skip the
	// send itself when outChanClosed is true.
	outChanBound, _ := m.outChanStatus()
	mappingChan := m.mappingChan

	// A kept resolve (keep==true) is one UNIQUE recorded test case; advance the
	// startup-window counter so AddMock stops tagging mocks IsStartup once we are
	// past the Nth test. Duplicates resolve with keep==false (static dedup) and
	// must NOT count — the window is measured in recorded tests, not requests.
	// Incrementing here only gates FUTURE ingests; the mocks processed in this
	// call were already tagged (or not) at AddMock time, and the rescues below
	// key off that per-mock tag, not the live counter.
	if keep {
		m.resolvedTestCount++
	}

	// Stale-buffer safety valve.
	//
	// The check exists to bound buffer growth when a stream of mocks
	// arrives that is never closed off by a corresponding test-window
	// resolve (e.g. a parser kept emitting after the test ended).
	// Without it, m.buffer would grow without bound across a long
	// recording session.
	//
	// CRITICAL ordering: cutoff must NOT pre-empt the window match.
	// A long-running test (mongo fuzzer's curl /run takes ~56 s for
	// 10 000 ops) emits per-test mocks whose ReqTimestampMock is far
	// older than 7 s by the time ResolveRange fires at request
	// completion — but those mocks ARE in-window and must be flushed
	// to the recorder, not silently dropped. The pre-c53b4906 V2 path
	// bypassed syncMock entirely so the cutoff never applied; routing
	// through AddMock (#4122) made the previous "cutoff first" ordering
	// drop the first ~49 s of a 56 s recording window, leaving replay
	// without the mongo handshake mocks → connection-pool error at
	// driver init.
	//
	// New ordering:
	//   1. In-window matches are kept/forwarded regardless of age.
	//   2. Out-of-window mocks are subject to the 7 s cutoff: kept if
	//      recent (might match a future out-of-order request), dropped
	//      otherwise (stale and unrecoverable).
	//   3. Session- and connection-scoped mocks are flushed to outChan
	//      (when bound) regardless of [start,end]; their Lifetime makes
	//      them reusable across every test window and they intentionally
	//      never window-match. Retained in the buffer when outChan is
	//      unbound so a later ResolveRange with the channel wired can
	//      drain them. AddMock now forwards these directly when outChan
	//      is bound at ingest time, so this branch only fires for mocks
	//      that landed in the buffer during the brief unbound startup
	//      window before SetOutputChannel.
	cutoffTime := time.Now().Add(-7 * time.Second)

	// Prune the recently-resolved-window ring to the same staleness
	// horizon as the buffer cutoff, so retroactive binning below can't
	// reattach a mock to an ancient window (which would defeat the
	// stale-cutoff's buffer-bound guarantee).
	if len(m.recentWindows) > 0 {
		kept := m.recentWindows[:0]
		for _, w := range m.recentWindows {
			kept = append(kept, w)
		}
		m.recentWindows = kept
	}
	// ownerWindow returns the recently-resolved window whose [start,end]
	// contains t (inclusive, matching the current-window test below).
	// Windows are non-overlapping FIFO request windows, so at most one
	// matches.
	ownerWindow := func(t time.Time) (resolvedWindow, bool) {
		for _, w := range m.recentWindows {
			if !t.Before(w.start) && !t.After(w.end) {
				return w, true
			}
		}
		return resolvedWindow{}, false
	}

	keepIdx := 0

	for i := 0; i < len(m.buffer); i++ {
		mock := m.buffer[i]
		mockTime := mock.Spec.ReqTimestampMock

		// LIFETIME CARVE-OUT: session- and connection-scoped mocks
		// are reusable across every test window and never need
		// per-test window filtering. Drain to outChan when bound so
		// the recorder writes them to disk; retain in the buffer
		// otherwise so a later ResolveRange (with outChan bound) can
		// pick them up. Skipping the per-test window match is
		// correct: session mocks aren't anchored to any test, so
		// trying to "match" them against [start,end] would either
		// drop them silently (out-of-window cutoff) or attribute
		// them to whichever test happens to ResolveRange first.
		lt := mock.TestModeInfo.Lifetime
		if lt == models.LifetimeSession || lt == models.LifetimeConnection {
			if !outChanBound {
				m.buffer[keepIdx] = mock
				keepIdx++
				continue
			}
			// Don't stamp a synthetic name on session/connection
			// mocks — Lifetime-derived parsers depend on
			// Spec.Metadata for routing at replay time and the
			// recorder writes whatever Name the mock already
			// carries. They also don't belong in the per-test
			// associatedMockIDs mapping (which is purely about
			// per-test matches). Owned by no specific test → owner "".
			mocksToSend = append(mocksToSend, ownedMock{mock: mock})
			continue
		}

		// MATCHING LOGIC: Process mocks in the requested window first
		// so a long-running test's per-test mocks aren't pre-empted by
		// the stale-buffer cutoff.
		if (mockTime.Equal(start) || mockTime.After(start)) && (mockTime.Equal(end) || mockTime.Before(end)) {
			if keep {
				// If output channel is not wired yet, keep matching
				// mocks buffered so they can be emitted later instead
				// of blocking on a nil channel. Shutdown-vs-normal
				// distinction is left to sendToOutChan (it no-ops
				// silently on outChanClosed), so we don't pre-drop
				// here — dropping in ResolveRange masked legitimate
				// mocks from the mongo fuzzer when CloseOutChan
				// fired between outChanStatus and the send (see
				// #4045 CI regression on record_build_replay_latest).
				if !outChanBound {
					m.buffer[keepIdx] = mock
					keepIdx++
					continue
				}
				mock.Name = "mock-" + generateRandomString(8)
				associatedMockIDs = append(associatedMockIDs, mock.Name)
				// Owned by THIS window's test → tag it so a capacity
				// drop suppresses testName rather than orphaning it.
				mocksToSend = append(mocksToSend, ownedMock{mock: mock, owner: testName})
			} else if isStartupMock(mock) {
				// STARTUP RESCUE (static-dedup duplicate window): keep==false
				// means the enterprise static-dedup deemed THIS test case a
				// duplicate, so its in-window mocks are normally pruned (the
				// drop-via-continue below). But a startup-window mock must
				// survive — until the Nth unique test is recorded, dedup pruning
				// is suppressed wholesale (a once-per-boot init call that fires
				// inside a duplicate's window would otherwise be lost, corrupting
				// the startup recording). Retain when outChan isn't bound yet,
				// else flush to disk. No associatedMockIDs entry: a duplicate's
				// testName is synthetic ("test-0"), so it owns no real mapping —
				// the same treatment the window-less startup rescue below gives.
				if !outChanBound {
					m.buffer[keepIdx] = mock
					keepIdx++
					continue
				}
				mock.Name = "mock-" + generateRandomString(8)
				// Duplicate's synthetic testName owns no real mapping →
				// owner "" (records nothing on a capacity drop).
				mocksToSend = append(mocksToSend, ownedMock{mock: mock})
			}
			// We successfully matched and handled this mock.
			// We discard it from the buffer so it doesn't get processed again.
			continue
		}

		// RETROACTIVE BIN: the mock missed the CURRENT window, but a
		// recently-resolved window may own it. This is the async-emit vs
		// window-close race — most visibly a Mongo cursor getMore the app
		// issued WHILE producing a response, whose decode finished only
		// after that response was captured and its window closed. The
		// mock's presaved ReqTimestampMock is correct and in that window,
		// but the direct [start,end] test above already ran for it, and no
		// FUTURE window can contain an earlier timestamp — so without this
		// it would fall to the stale-cutoff and be lost. Attribute it to
		// the owning window's test so it's persisted (and picked up by
		// replay's timestamp filter) instead of dropped. Placed BEFORE the
		// stale-cutoff so an in-window-but-old mock (long test window that
		// straddles the 7 s horizon) is rescued rather than reaped. Mirrors
		// the in-window branch's keep / outChanBound handling.
		if ownerW, ok := ownerWindow(mockTime); ok {
			if keep {
				if !outChanBound {
					m.buffer[keepIdx] = mock
					keepIdx++
					continue
				}
				mock.Name = "mock-" + generateRandomString(8)
				if ownerW.mapping {
					if lateMappings == nil {
						lateMappings = make(map[string][]string)
					}
					lateMappings[ownerW.testName] = append(lateMappings[ownerW.testName], mock.Name)
				}
				// Owned by the retro-matched window's test → tag it so a
				// capacity drop suppresses that TC.
				mocksToSend = append(mocksToSend, ownedMock{mock: mock, owner: ownerW.testName})
				lateBinned++
			}
			// Handled (flushed or retained); drop from the current buffer.
			continue
		}

		// STARTUP RESCUE (out-of-window): a startup-window mock (IsStartup) that
		// didn't match the current window — boot traffic like an AWS Secret
		// Manager fetch / DB handshake / config load, or an early-test mock
		// whose own window isn't the one resolving now. The stale-cutoff below
		// would otherwise silently reap it (the "present in some test sets,
		// missing in others" bug). Flush it to disk instead so replay's startup
		// tier can serve it. Placed BEFORE the cutoff so a slow boot (>7 s to the
		// first request) can't lose it. A per-test mock captured PAST the startup
		// window that merely lands before the current window (genuine stale
		// cross-test bleed) is NOT tagged IsStartup and still falls to the cutoff.
		if isStartupMock(mock) {
			if !outChanBound {
				m.buffer[keepIdx] = mock
				keepIdx++
				continue
			}
			mock.Name = "mock-" + generateRandomString(8)
			// Boot/startup traffic owns no specific test → owner "".
			mocksToSend = append(mocksToSend, ownedMock{mock: mock})
			continue
		}

		// SAFETY VALVE: Expire stale OUT-OF-WINDOW mocks.
		// A mock that didn't match the current window AND is older
		// than 7 s is unrecoverable — no future request can have a
		// window that includes a timestamp from before "now-7s" once
		// we're already past the head of the dedup queue, because
		// the queue is FIFO on ReqTimestamp. Drop to bound growth.
		// (Session- and connection-tier mocks are handled in the
		// lifetime carve-out at the top of the loop and never reach
		// this branch.)
		if mockTime.Before(cutoffTime) {
			// Per-mock diagnostic: a per-test mock that fell off the
			// stale-buffer cutoff almost always means the recorder
			// kept emitting after the dedup queue had advanced past
			// the matching test's window — log enough context for
			// post-hoc CI analysis. Sampled via dropLogger to honour
			// the same flood-prevention as the outChan-overflow path.
			if logger := m.dropLogger(); logger != nil {
				logger.Debug("diag/ResolveRange: stale-cutoff drop (out-of-window per-test mock older than 7s)",
					zap.String("mock_name", mock.Name),
					zap.String("mock_kind", string(mock.Kind)),
					zap.String("connID", mock.ConnectionID),
					zap.String("lifetime", mock.TestModeInfo.Lifetime.String()),
					zap.Time("mock_req_ts", mockTime),
					zap.Time("window_start", start),
					zap.Time("window_end", end),
					zap.Time("cutoff", cutoffTime),
					zap.String("test_name", testName),
				)
			}
			continue
		}

		// RETENTION: Keep the mock if it's recent (within 7s) but
		// didn't match this specific window. It might be matched
		// by a future out-of-order request.
		m.buffer[keepIdx] = mock
		keepIdx++
	}

	// Snapshot pre-truncation length so the diagnostic can report
	// "shrunk from N to M" rather than the post-truncation length.
	bufferLenBefore := len(m.buffer)

	// MEMORY CLEANUP: Nil out the deleted entries to allow GC to reclaim the memory
	for i := keepIdx; i < len(m.buffer); i++ {
		m.buffer[i] = nil
	}

	// Reslice the buffer
	m.buffer = m.buffer[:keepIdx]

	if len(associatedMockIDs) > 0 && mappingChan != nil && mapping {
		mappingEntry = &models.TestMockMapping{
			TestName: testName,
			MockIDs:  associatedMockIDs,
		}
	}

	// Record THIS window so a later ResolveRange can retroactively bin a
	// mock decoded after this window closed (see recentWindows). Appended
	// AFTER the match loop, so the current window's own mocks went through
	// the direct [start,end] path above, never the ring. Skipped for the
	// no-keep / unbound cases is unnecessary — an empty-but-recorded
	// window is harmless and pruned by age.
	m.recentWindows = append(m.recentWindows, resolvedWindow{
		start:    start,
		end:      end,
		testName: testName,
		mapping:  mapping,
	})
	if len(m.recentWindows) > maxRecentWindows {
		// Drop the oldest entries; copy down so the big backing array
		// isn't retained by the reslice.
		n := copy(m.recentWindows, m.recentWindows[len(m.recentWindows)-maxRecentWindows:])
		m.recentWindows = m.recentWindows[:n]
	}

	bufferLenAfter := len(m.buffer)
	mocksToSendLen := len(mocksToSend)

	m.mu.Unlock()

	// Per-resolve diagnostic: surface buffer-state transitions per
	// test-window resolve so a CI log can show when a per-test cohort
	// flushed zero mocks or when stale-buffer cutoff started reaping.
	// Sampled via dropLogger which is the standard observability sink
	// for buffer-flow events on this manager. Only logged when there
	// was actual state change to avoid log noise on idle resolves.
	if logger := m.dropLogger(); logger != nil && (bufferLenBefore != bufferLenAfter || mocksToSendLen > 0) {
		logger.Debug("diag/ResolveRange: buffer transition",
			zap.String("test_name", testName),
			zap.Time("window_start", start),
			zap.Time("window_end", end),
			zap.Int("buffer_len_before", bufferLenBefore),
			zap.Int("buffer_len_after", bufferLenAfter),
			zap.Int("mocks_flushed", mocksToSendLen),
			zap.Int("late_binned", lateBinned),
			zap.Int("dropped_total", bufferLenBefore-bufferLenAfter-mocksToSendLen),
			zap.Bool("outChan_bound", outChanBound),
			zap.Bool("mapping_enabled", mapping),
		)
	}

	// Route mock sends through sendToOutChanOwned so the close-vs-send
	// race is serialized the same way AddMock does it, and a capacity
	// drop is attributed to the owning TC. Mapping channel is never
	// closed by the shutdown path today — if that ever changes, lift the
	// mapping send under an equivalent guard.
	for _, om := range mocksToSend {
		m.sendToOutChanOwned(om.mock, om.owner)
	}
	if mappingEntry != nil && mappingChan != nil {
		select {
		case mappingChan <- *mappingEntry:
		default:
		}
	}
	// Retroactive mapping entries for mocks late-binned into past windows.
	// The recorder Upserts mappings by test name, so a second entry for an
	// already-resolved test merges into its existing mapping rather than
	// replacing it.
	if mappingChan != nil {
		for tn, ids := range lateMappings {
			if len(ids) == 0 {
				continue
			}
			select {
			case mappingChan <- models.TestMockMapping{TestName: tn, MockIDs: ids}:
			default:
			}
		}
	}
}

// DeleteMocksStrictlyBefore is the dedup-queue cleanup invoked when a
// DUPLICATE request is skipped: it clears buffered mocks captured before
// the duplicate's request timestamp so the recording doesn't accumulate
// the skipped duplicate's debris.
//
// It must NOT, however, delete a mock that legitimately belongs to an
// earlier KEPT (non-duplicate) test and merely arrived in the buffer
// late — the exact failure that lost every per-test Mongo mock in the
// mongo-bigmock recording: a 6 MB document decodes/emits after its
// HTTP window already resolved (so ResolveRange never matched it), and
// then the NEXT cycle's duplicate requests fire DeleteMocksStrictlyBefore
// and wipe those kept-but-late mocks before any retroactive-bin
// ResolveRange can rescue them. Duplicate cycles take this path, never
// ResolveRange, so without the rescue here the kept mocks are gone.
//
// Discriminator: a mock belongs to a kept test iff its ReqTimestampMock
// falls inside a recently-resolved window (recentWindows only ever holds
// NON-duplicate windows — duplicates resolve through here, not
// ResolveRange). So a before-the-horizon mock that owns a recent window
// is a late kept mock → flush it (rescue); one that owns none is the
// skipped duplicate's own debris → drop it. Session/connection mocks are
// reusable across tests and are never reaped by a per-test cleanup.
func (m *SyncMockManager) DeleteMocksStrictlyBefore(timestamp time.Time) {
	if m == nil {
		return
	}

	var mocksToSend []ownedMock
	var lateMappings map[string][]string

	m.mu.Lock()
	outChanBound, _ := m.outChanStatus()
	mappingChan := m.mappingChan

	ownerWindow := func(t time.Time) (resolvedWindow, bool) {
		for _, w := range m.recentWindows {
			if !t.Before(w.start) && !t.After(w.end) {
				return w, true
			}
		}
		return resolvedWindow{}, false
	}

	keepIdx := 0
	for i := 0; i < len(m.buffer); i++ {
		mock := m.buffer[i]
		if mock == nil {
			continue
		}

		// At/after the cleanup horizon: belongs to the current or a future
		// request, not the duplicate being skipped — always retain.
		if !mock.Spec.ReqTimestampMock.Before(timestamp) {
			m.buffer[keepIdx] = mock
			keepIdx++
			continue
		}

		// Session/connection mocks outlive any single test window and must
		// survive a per-test cleanup. Flush when we can persist them now,
		// otherwise retain for a later drain. Owned by no specific test →
		// owner "".
		if lt := mock.TestModeInfo.Lifetime; lt == models.LifetimeSession || lt == models.LifetimeConnection {
			if outChanBound {
				mocksToSend = append(mocksToSend, ownedMock{mock: mock})
				continue
			}
			m.buffer[keepIdx] = mock
			keepIdx++
			continue
		}

		// RESCUE: a before-the-horizon per-test mock that owns a recent
		// KEPT window is a legitimately-kept test's late arrival — flush
		// it to that test instead of deleting it as duplicate debris.
		if w, ok := ownerWindow(mock.Spec.ReqTimestampMock); ok {
			if !outChanBound {
				// Can't deliver yet; retain so a later flush sends it.
				m.buffer[keepIdx] = mock
				keepIdx++
				continue
			}
			mock.Name = "mock-" + generateRandomString(8)
			if w.mapping {
				if lateMappings == nil {
					lateMappings = make(map[string][]string)
				}
				lateMappings[w.testName] = append(lateMappings[w.testName], mock.Name)
			}
			// Owned by the rescued window's test → tag it so a capacity
			// drop suppresses that TC.
			mocksToSend = append(mocksToSend, ownedMock{mock: mock, owner: w.testName})
			continue
		}

		// STARTUP RESCUE: a startup-window mock (boot traffic, or anything
		// captured within the first StartupMockTestCaseWindow test cases) is NOT
		// the skipped duplicate's debris and must survive this cleanup. Without
		// this, a once-per-boot init call (e.g. AWS Secret Manager) is dropped
		// whenever an early request hashes as a dedup duplicate while
		// recentWindows can't yet claim it (empty at boot, or the mock landed in
		// a duplicate's own window) — the root of the flaky per-test-set capture,
		// and exactly what keeps static dedup from pruning the startup corpus.
		// Flush it instead.
		if isStartupMock(mock) {
			if !outChanBound {
				m.buffer[keepIdx] = mock
				keepIdx++
				continue
			}
			mock.Name = "mock-" + generateRandomString(8)
			// Startup/boot traffic owns no specific test → owner "".
			mocksToSend = append(mocksToSend, ownedMock{mock: mock})
			continue
		}

		// Owns no kept window and is before the horizon → the skipped
		// duplicate's own debris. Drop it (fall through without keeping).
	}

	// Memory Cleanup: Nil out the deleted entries to allow GC to reclaim the memory
	for i := keepIdx; i < len(m.buffer); i++ {
		m.buffer[i] = nil
	}
	// Reslice the buffer
	m.buffer = m.buffer[:keepIdx]
	m.mu.Unlock()

	// Send AFTER releasing m.mu — sendToOutChan takes outChanMu and may
	// block up to sendBudget; holding m.mu across it would wedge AddMock.
	for _, om := range mocksToSend {
		m.sendToOutChanOwned(om.mock, om.owner)
	}
	if mappingChan != nil {
		for tn, ids := range lateMappings {
			if len(ids) == 0 {
				continue
			}
			select {
			case mappingChan <- models.TestMockMapping{TestName: tn, MockIDs: ids}:
			default:
			}
		}
	}
}

type DedupJob struct {
	ReqTimestamp time.Time
	ResTimestamp time.Time
	// TestName is the recorder-side test identifier anchored to this
	// dedup bucket. Populated at ResolveJob time so the internal
	// ResolveRange call below forwards a non-empty testName into the
	// TestMockMapping entry when enableMapping is true. Left empty for
	// the early-exit defer path where no test identity is established.
	TestName    string
	Resolved    bool
	IsDuplicate bool
}

type DedupQueue struct {
	mu    sync.Mutex
	queue []*DedupJob
}

var globalDedupQueue = &DedupQueue{
	queue: make([]*DedupJob, 0),
}

func GetDedupQueue() *DedupQueue {
	return globalDedupQueue
}

// NewDedupQueue constructs an independent dedup queue. Pair it with a
// syncMock.New() manager when a process runs multiple concurrent capture
// sessions, so each session's strict-FIFO dedup ordering is isolated and
// one app's requests cannot mark another app's first occurrence a
// duplicate. GetDedupQueue() remains the single-session global default.
func NewDedupQueue() *DedupQueue {
	return &DedupQueue{
		queue: make([]*DedupJob, 0),
	}
}

// Enqueue adds a request to the end of the queue as soon as it's encountered.
func (dq *DedupQueue) Enqueue(reqTime time.Time) *DedupJob {
	dq.mu.Lock()
	defer dq.mu.Unlock()
	job := &DedupJob{
		ReqTimestamp: reqTime,
		Resolved:     false,
	}
	dq.queue = append(dq.queue, job)
	return job
}

// ResolveJob marks a job as resolved and attempts to process the queue from the head.
//
// testName is the caller's anchor for this specific job: under async
// capture.go the header-derived Keploy-Test-Name flows in so the
// downstream ResolveRange → TestMockMapping entry is stamped with the
// same test identity the sync branch would have synthesised. Empty
// testName is legal (early-exit defer, no-mapping callers) — it falls
// through to the unchanged historical behaviour and ResolveRange simply
// emits an anonymous mapping entry (or none, when mapping is false).
func (dq *DedupQueue) ResolveJob(job *DedupJob, isDuplicate bool, resTimestamp time.Time, testName string, enableMapping bool, mockMgr *SyncMockManager) {
	dq.mu.Lock()
	defer dq.mu.Unlock()

	job.IsDuplicate = isDuplicate
	job.Resolved = true
	job.ResTimestamp = resTimestamp
	// Stamp the anchor onto the job itself so the head-draining loop
	// below forwards the correct testName even when an earlier job at
	// the head is resolved by a LATER caller (strict-FIFO drain can
	// process several jobs under a single ResolveJob invocation, each
	// needing its own anchor).
	job.TestName = testName

	// Always process from the head to ensure strict FIFO ordering
	for len(dq.queue) > 0 {
		head := dq.queue[0]

		// If the oldest request hasn't been resolved yet, halt and wait.
		if !head.Resolved {
			break
		}

		// If it is a duplicate, perform the strict cleanup.
		if head.IsDuplicate && mockMgr != nil {
			mockMgr.DeleteMocksStrictlyBefore(head.ReqTimestamp)
		} else if head.IsDuplicate == false && mockMgr != nil {
			mockMgr.ResolveRange(head.ReqTimestamp, head.ResTimestamp, head.TestName, true, enableMapping)

		}

		dq.queue = dq.queue[1:]
	}
}
