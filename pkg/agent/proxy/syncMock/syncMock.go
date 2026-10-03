package manager

import (
	"container/heap"
	"context"
	"math/rand"
	"slices"
	"sort"
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
// SyncMockManager.recentWindows) by COUNT — the ring is deliberately NOT
// age-pruned (ResolveRange's retro-bin rescues in-window mocks from windows far
// older than the 7 s stale-cutoff; see that function and
// TestResolveRangeRecordsLateMockInOldWindowButDropsOrphan). The cap must
// therefore hold far more than the windows that can resolve while a test's
// LATE-decoded mock (e.g. a large async Mongo aggregate response) is still in
// flight — otherwise the owning window is evicted before the mock lands,
// ownerWindowLocked() misses, and the mock is orphaned from its test's mapping →
// replay match_phase=no_mocks for that query. The previous 256 was far too
// small: heavy-load recordings burst well past 1000 resolved windows within a
// single 7 s span (go-memory-load-mongo: ~1000 windows/7 s peak), so a slow
// aggregate's window was evicted long before it could be retro-binned. The
// window only has to survive the mock's decode-lag plus one FlushOwnedWindows
// tick (~1 s) — retro-bin fires on the NEXT ResolveRange or that periodic tick,
// not at test end — so at the ~143 windows/s peak, 8192 (≈57 s of history) is a
// ~50x margin. It matches maxPressureRanges, which guards the SAME consumer-lag
// class of bug, and stays O(1) memory (~64 B/window).
const maxRecentWindows = 8192

// maxPressureRanges bounds the spans a Spans keeps (the memory-pressure spans,
// SyncMockManager.pressure, the orphan spans and a DaemonSet pod's) by COUNT,
// not by wall-clock age. An earlier version time-pruned closed pressure ranges
// 7 s after they ended, on the assumption that nothing older could still be
// queried. That is wrong for the orphan-TC suppression in routes/record.go: the
// test-case stream lags the recorder — a backed-up channel plus a slow CLI
// drain routinely puts it MORE than 7 s behind — so a range that caused a mock
// drop is reaped before the TC whose window overlaps it is ever checked, and
// the orphan is persisted (replay then fails match_phase=no_mocks). A later
// version evicted the oldest past the count, which is the same loss once
// enough spans come fast enough. Past the count spans are joined instead
// (Spans), which never uncovers one, while staying O(1) memory and
// keeping each overlap check a binary search.
const maxPressureRanges = 8192

// StaleHorizon is how old a buffered per-test mock that no window owns may get
// before a resolve drops it (ResolveRange's stale cutoff). The hold keeps what
// a request in flight may own past it, within MaxHeldBytes (holdpool.go).
const StaleHorizon = 7 * time.Second

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
	// keep is the window's own verdict: false for a static-dedup duplicate.
	// A late mock is kept or pruned by the window that OWNS it, never by the
	// resolve that happens to find it (see the retroactive bin).
	keep bool
	// sharedFrom is when its request gave up being the only one in flight
	// (Window.Yield), if it did before end, in Unix nanoseconds; 0 if not.
	// From then on it owned only what no other request claimed (see
	// exclusiveEnd). An int64, not a time.Time: the ring keeps 8192 of these.
	sharedFrom int64
}

// unixNanoOrZero is t in Unix nanoseconds, 0 for the zero time.
func unixNanoOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// exclusiveEnd is the end of the part of the window in which its request was
// the only one in flight: its yield (Window.Yield), or its end when it did not
// yield before it.
func (r resolvedWindow) exclusiveEnd() time.Time {
	if r.sharedFrom != 0 {
		if at := time.Unix(0, r.sharedFrom); at.Before(r.end) {
			return at
		}
	}
	return r.end
}

// ownerWindowLocked returns the recently-resolved window that owns a per-test
// mock timestamped t: one whose [start,end] contains t (inclusive, matching
// ResolveRange's current-window test). Sequential request windows never
// overlap, but concurrent sync-mode requests' windows can; a KEPT window then
// wins over a static-dedup duplicate, because pruning a kept test's mock fails
// its replay while keeping a duplicate's costs one extra mock. Among windows
// with the same verdict the oldest wins. Caller holds m.mu.
//
// A kept window owns the part after its request yielded (Window.Yield) only
// where no other request claims t: a kept window over t that had not yielded
// by then wins over it, and while a window still open claims t
// (openClaimLocked) it is not its, nor any resolved window's but a
// duplicate's, so a reaper holds it for that request (holdOrDropLocked).
//
// Only RESOLVED windows are here. A window still open (OpenWindow) has no end
// and no verdict yet; a mock it may own that a reaper would drop — a
// duplicate's verdict, the stale cutoff — is held for it instead (holdOrDropLocked),
// and goes to the first kept window resolved over it.
func (m *SyncMockManager) ownerWindowLocked(t time.Time) (resolvedWindow, bool) {
	owner, ok, _, _ := m.ownerOrSharedLocked(t)
	return owner, ok
}

// ownerOrSharedLocked is ownerWindowLocked, and also returns, when no window
// owns t yet because a window still open claims it, the resolved kept window
// whose part after its yield covers it (shared, with behind true): the mock
// goes to the open window's request if it takes it, else back to that one, so
// the late path that finds it holds it at once, with a record of that window
// (holdForClaimLocked, heldMock.owner). It allocates nothing. Caller holds
// m.mu.
func (m *SyncMockManager) ownerOrSharedLocked(t time.Time) (owner resolvedWindow, ok bool, shared resolvedWindow, behind bool) {
	var sharedOwner resolvedWindow
	found, foundShared := false, false
	for _, w := range m.recentWindows {
		if t.Before(w.start) || t.After(w.end) {
			continue
		}
		if w.keep {
			if !t.After(w.exclusiveEnd()) {
				return w, true, resolvedWindow{}, false
			}
			if !foundShared {
				sharedOwner, foundShared = w, true
			}
			continue
		}
		if !found {
			owner, found = w, true
		}
	}
	if foundShared {
		if !m.openClaimLocked(t, nil) {
			return sharedOwner, true, resolvedWindow{}, false
		}
		return owner, found, sharedOwner, true
	}
	return owner, found, resolvedWindow{}, false
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
	mu          sync.Mutex
	buffer      []*models.Mock
	mappingChan chan<- models.TestMockMapping

	// mappingOverflow holds mappings the recorder was not ready to take. The
	// capture path must never block, but a mapping must never be dropped either
	// (a lost one becomes a no_mocks failure at replay), so overflow is queued
	// here and handed over by a single drainer goroutine. Guarded by
	// mappingOverflowMu, which is a leaf: never take m.mu while holding it.
	mappingOverflowMu sync.Mutex
	mappingOverflow   []models.TestMockMapping
	mappingDraining   bool
	mappingStreamCtx  context.Context
	// mappingGen identifies the current stream. Bumped on every
	// SetMappingChannel so a drainer left over from a previous stream retires
	// instead of clearing the new stream's queue.
	mappingGen   uint64
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

	// recentWindows is a COUNT-bounded ring of the most recently
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
	// Bounded by COUNT (maxRecentWindows), NOT by age: it must retain
	// windows far older than the 7 s buffer stale-cutoff, because a late
	// mock legitimately belongs to a long-past window (a large async
	// response can decode tens of seconds after its window closed, and a
	// long-running test — the ~56 s mongo fuzzer /run — spans that horizon
	// by itself). Age-pruning would strand those mocks (see
	// TestResolveRangeRecordsLateMockInOldWindowButDropsOrphan); the count
	// cap bounds memory instead.
	recentWindows []resolvedWindow

	// open is the windows of requests read but not decided yet (OpenWindow),
	// a min-heap on start: the earliest, the one every reaper asks for, is
	// open[0]. A per-test mock requested at or after its start may be one of
	// theirs, so no reaper drops it (see held). Guarded by mu.
	open windowHeap
	// held is the per-test mocks a reaper would have dropped (past the stale
	// horizon, or a duplicate's leftovers) while a window still open may own
	// them: no resolved window owns them, and every one is requested at or
	// after the earliest open start. Sorted by request time, each with the
	// reason it would have been dropped for. Kept out of the buffer, so a
	// resolve looks only at the part inside its own window (takeHeldLocked)
	// instead of rescanning them all against every resolved window. A kept
	// window resolved over one takes it; ending a window drops what no open
	// window may own any more (endWindowLocked); MaxHeldBytes bounds it, with
	// the holds of the process's other managers (fitHoldLocked, settlePool),
	// and memory pressure empties it (giveUpForPressureLocked). Guarded by mu.
	held []heldMock
	// heldBytes is the size of all of held, kept as held changes (holdLocked,
	// unholdLocked), so no fit sums the hold. Guarded by mu.
	heldBytes int64
	// pool is the budget the hold shares with the process's other managers
	// (MaxHeldBytes; poolLocked). pooled is what was last published to it
	// (publishHoldLocked): heldBytes as of the end of the last change to the
	// hold, and pooledNow the same, and oldestUnclaimed when m's oldest
	// request in flight started (noteOldestLocked), for the pool to choose
	// among managers without their locks. inPool: m is one of its members.
	// Guarded by mu, but pooledNow and oldestUnclaimed.
	pool            *holdPool
	pooled          int64
	pooledNow       atomic.Int64
	oldestUnclaimed atomic.Int64
	inPool          bool
	// heldShared is how many of held have an owner (heldMock.owner): none,
	// but with the synchronous ingress, so PendingIn looks at the hold only
	// when there is one. Guarded by mu.
	heldShared int
	// owed is the held mocks let go of with no request in flight claiming
	// them that a resolved kept window gets back (heldMock.owner): handed on
	// to it, mapping included, by the next resolve, prune or flush with the
	// output wired (handOwedLocked). Guarded by mu.
	owed []owedMock
	// windowsGivenUp counts the open windows the hold's bound gave up, for the
	// sampled diagnostic in reportGivenUp. Not loss by itself: only a request
	// that is then kept loses its test case (keptLeftOut).
	windowsGivenUp atomic.Uint64
	// keptLeftOut counts the kept verdicts that found their window given up:
	// each is one test case left out of the recording. This is the loss, and
	// Keep warns of it (reportLeftOut). One a later request is recorded in the
	// place of is taken out again (Window.Replaced), so the warning is
	// sampled on leftOutTold, which only grows.
	keptLeftOut atomic.Uint64
	leftOutTold atomic.Uint64
	// lossTallies are the tallies open (OpenLossTally): each counts the
	// losses told while it is open, less those of them taken back. lossEpoch
	// is the number of tallies ever opened, the last one's epoch; a window
	// that tells its loss keeps the lossEpoch it was told in (Window.toldEpoch),
	// so its take-back lands on the tallies that counted it and on no other.
	// Guarded by mu.
	lossTallies []*LossTally
	lossEpoch   uint64

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
	// records pressure spans so record.go suppresses the overlapping TC — a
	// capacity drop feeds nothing into them, so the owning TC would
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

	// leftOut counts the mocks a session routed here left out
	// because a parser could not record them: their capture was incomplete,
	// or they could not be decoded or served (NoteMockLeftOut), for the
	// recording's summary: their warnings are rate-limited, so this is the
	// one place each is counted.
	leftOut atomic.Int64

	// outChanClosedDrops counts mocks that were already counted in
	// totalAdded (they passed the pressure gate) but were then dropped
	// because the outChan was already closed by CloseOutChan — i.e.
	// the mock arrived AFTER shutdown sealed the stream. This is a
	// real post-count drop, so without this counter totalAdded would
	// over-report deliverable mocks. Accounting identity at shutdown:
	//   totalAdded = forwarded + still_in_buffer + outChanClosedDrops
	//                + sendBudget drops (dropCount)
	outChanClosedDrops atomic.Int64

	// pressure holds every span during which memory pressure was active: one
	// opens on the false→true transition in SetMemoryPressure (closePressure
	// is its closer, guarded by mu), and closes on the true→false one. A span
	// still open extends to the moment it is queried.
	//
	// This is the join key for the Bug 0 TC-suppression fix:
	// WasPressureActiveInWindow checks whether any span overlaps a TC's
	// [HTTPReq.Timestamp, HTTPResp.Timestamp] window. Using pressure INTERVALS
	// instead of per-mock drop timestamps is what makes suppression parser-
	// agnostic: any parser (mongo in keploy/integrations, postgres, http…) that
	// drops captured bytes on memoryguard.IsRecordingPaused() drops within an
	// interval this records, so record.go catches the overlap without the
	// parser reporting anything. Note the ordering is NOT a strict happens-before
	// on m.memoryPause: memoryguard flips the GLOBAL recordingPaused flag (which
	// the parser reads) just BEFORE it calls SetMemoryPressure to open the span,
	// so there is a sub-microsecond gap where a parser could drop with no span
	// yet recorded. Correctness does not rely on that gap being closed — it rests
	// on record.go querying the span much later (by which time it exists), and
	// on the drop preceding the mock's HTTPResp.Timestamp by far more than the
	// open latency, so the TC window still overlaps the recorded interval.
	//
	// Bounded by COUNT (maxPressureRanges), not by wall-clock age, and past it
	// spans are joined, never evicted (Spans): a lagging
	// routes/record.go still finds the span of the TC it checks, however long
	// ago it closed and however many came after. Spans has its own lock, taken
	// under mu or alone.
	pressure      Spans
	closePressure func()

	// orphans are the spans over which a connection carried traffic that was
	// not recorded as a mock, whatever the cause: a hole a parser reports
	// (a mongo/v2 reassembly resync hole, where a dropped chunk desyncs the
	// framer, so a message delivered during the hole is DELIVERED, not
	// dropped, yet never turned into a mock), a mock the session's
	// incomplete-mock flag left out or its parser reported it could not
	// record, the exchange it stopped on included (supervisor.Session's
	// ReportLeftOut, ReportStoppedOn), or a connection that can no longer be
	// recorded at all (see unrecorded.go). Like the pressure spans they feed a suppressor
	// query, WasMockOrphanedInWindow, which record.go checks alongside
	// WasPressureActiveInWindow, so record.go suppresses every TC whose window
	// overlaps one rather than shipping it mock-less (replay would report
	// match_phase=no_mocks).
	//
	// Kept SEPARATE from the pressure spans, which say only when the memory
	// guard paused recording. A pause also costs connections exchanges (a
	// chunk the relay's tee gates voids the next mock the connection's parser
	// emits, and desyncs the connection's capture, which unrecorded.go then
	// follows), and those land here too: an orphan span is what a connection
	// lost, the pressure spans are when pressure was on.
	//
	// Closed spans come from RecordOrphanWindow, open ones from
	// OpenOrphanWindow: a connection that is CURRENTLY un-capturable and may
	// stay that way for the rest of the session. RecordOrphanWindow can only be
	// called once a hole's width is known, when the parser gets to it, and a
	// parser runs behind the traffic: by then test cases inside it may have
	// been streamed already. The suppressor in routes/record.go reads these
	// spans as each TC is streamed, not afterwards, so those are saved without
	// the mock. Only a capture with a watermark (settle.go) holds its test
	// cases until the spans that could overlap them are known.
	//
	// Spans bounds their number, not their width: a suppression is
	// session-global for every TC whose window overlaps a span, so keeping a
	// span narrow is the recorder's responsibility. Spans has its own lock.
	orphans Spans

	// watermark says when a test case's verdict is final (settle.go); nil
	// when nothing can tell.
	watermark atomic.Pointer[watermarkBox]
	// heldOldest is the earliest end (UnixNano) of a test case the recorder's
	// stream holds for its verdict (NoteHeld); 0 when none.
	heldOldest atomic.Int64
	// onDrop is told of each mock the outChan capacity path drops (OnDrop).
	onDrop atomic.Pointer[func(*models.Mock)]
	// sending holds the request time of each mock taken out of the buffer
	// (or handed straight on) whose send to outChan has not finished. A mock
	// leaving the buffer is added under mu, so it is always either buffered
	// or here until it is sent or dropped (PendingIn). sendingMu is a leaf:
	// taken under mu, or alone.
	sendingMu sync.Mutex
	sending   map[*models.Mock]time.Time

	// loggerMu guards logger so SetLogger and the drop path can run
	// concurrently without a data race. The read lock is taken only
	// on the (sampled, cold) Error path, so contention is negligible.
	loggerMu sync.RWMutex
	logger   *zap.Logger
}

// pressureRange is one closed [start, end] span of a Spans.
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

// mergeByRequestTime merges sorted, which is in request order, into arrived,
// which keeps its own order (the buffer's), so that each of sorted goes before
// the first of arrived requested after it. A connection's mocks then still reach
// the recorder in the order they were requested when some of them were held.
func mergeByRequestTime(arrived, sorted []ownedMock) []ownedMock {
	if len(sorted) == 0 {
		return arrived
	}
	out := make([]ownedMock, 0, len(arrived)+len(sorted))
	j := 0
	for _, a := range arrived {
		for j < len(sorted) && sorted[j].mock.Spec.ReqTimestampMock.Before(a.mock.Spec.ReqTimestampMock) {
			out = append(out, sorted[j])
			j++
		}
		out = append(out, a)
	}
	return append(out, sorted[j:]...)
}

// Global instance is initialized at package load time
var instance = &SyncMockManager{
	buffer:       make([]*models.Mock, 0, defaultMockBufferCapacity),
	firstReqSeen: false,
	pool:         processHold,
}

// Get returns the global manager.
func Get() *SyncMockManager {
	return instance
}

// New constructs an independent SyncMockManager with its own buffer, window
// ring, drop counter, and per-session dedup queue. It shares no state with the
// package global returned by Get(), or with any other manager New made, but the
// process's hold budget (processHold, MaxHeldBytes): a settle for that budget
// may give up a request of any of them that holds more than its share (see
// MaxHeldBytes). Use it when a single process runs more
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
		pool:         processHold,
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

// mappingOverflowCap bounds the queue of mappings the recorder has not taken yet.
// Unbounded would be worse than the drop it replaces: the agent already carries a
// large resident footprint, and trading bounded partial loss for an OOM kill loses
// the WHOLE recording. Sized far above any real backlog — the recorder drains in
// batches, so reaching this means it is wedged, not merely slow.
const mappingOverflowCap = 10000

// SetMappingChannel installs the recorder's mapping stream. streamCtx must be the
// ctx of the HTTP request serving that stream: it is the only signal that the
// recorder has gone away, and the overflow drainer needs it to know when a
// blocking hand-off can never complete.
//
// A new stream fully supersedes the old one. mappingGen is bumped so any drainer
// still blocked on the previous stream retires instead of draining this stream's
// queue into a channel nobody reads — and so it cannot clear a queue that is no
// longer its own.
func (m *SyncMockManager) SetMappingChannel(streamCtx context.Context, ch chan<- models.TestMockMapping) {
	m.mu.Lock()
	m.mappingChan = ch
	m.mu.Unlock()

	m.mappingOverflowMu.Lock()
	m.mappingStreamCtx = streamCtx
	// Anything the previous stream never handed over belongs to a recording that
	// has already ended.
	m.mappingOverflow = nil
	m.mappingGen++
	// The previous drainer (if any) retires on its generation check, so this
	// stream starts with no drainer and the next overflow spawns a fresh one.
	m.mappingDraining = false
	m.mappingOverflowMu.Unlock()
}

// sendMapping hands a mapping to the recorder without ever blocking the capture
// path and without ever discarding the mapping.
//
// This used to be a bare non-blocking send with a `default:` that threw the
// mapping away. That is a data-loss bug, not a backpressure policy: the recorder
// only rewrites mappings.yaml so fast, and once its 100-slot buffer filled — which
// a heavy concurrent recording reliably does — the dropped mappings surfaced at
// replay as "no_mocks" for those tests, with nothing logged anywhere. Measured: a
// recorder that stalls to write received 4 of 500 mappings.
//
// The capture path still must never block (ResolveRange runs from the ingress
// hook), so the fast path stays non-blocking; anything that does not fit is queued
// and handed over by a single drainer goroutine that CAN block. Ordering: once a
// drainer is live every mapping queues behind it, so the fast path can never
// overtake an entry the drainer is mid-send on.
func (m *SyncMockManager) sendMapping(ch chan<- models.TestMockMapping, entry models.TestMockMapping) {
	m.mappingOverflowMu.Lock()
	if m.mappingDraining {
		// A drainer is live — it may be mid-send with overflow momentarily empty,
		// so gate on the drainer, not on the queue length, or this would overtake.
		if len(m.mappingOverflow) >= mappingOverflowCap {
			m.mappingOverflowMu.Unlock()
			m.reportMappingOverflowFull()
			return
		}
		m.mappingOverflow = append(m.mappingOverflow, entry)
		m.mappingOverflowMu.Unlock()
		return
	}
	m.mappingOverflowMu.Unlock()

	select {
	case ch <- entry:
		return
	default:
	}

	m.mappingOverflowMu.Lock()
	if m.mappingDraining {
		// Another caller started a drainer while we were trying the fast path.
		m.mappingOverflow = append(m.mappingOverflow, entry)
		m.mappingOverflowMu.Unlock()
		return
	}
	m.mappingOverflow = append(m.mappingOverflow, entry)
	m.mappingDraining = true
	gen := m.mappingGen
	streamCtx := m.mappingStreamCtx
	m.mappingOverflowMu.Unlock()

	go m.drainMappingOverflow(ch, streamCtx, gen)
}

// reportMappingOverflowFull logs the only case in which a mapping is still lost:
// the recorder has wedged and the queue has hit its cap. Never silent.
func (m *SyncMockManager) reportMappingOverflowFull() {
	if logger := m.dropLogger(); logger != nil {
		logger.Error("mapping overflow is full; dropping mappings",
			zap.Int("cap", mappingOverflowCap),
			zap.String("next_step", "the recorder is not draining the mapping stream; affected tests will be missing from mappings.yaml and replay will report no_mocks for them — report this with the record logs"))
	}
}

// drainMappingOverflow hands queued mappings over one at a time, blocking until
// each is taken. It runs on its own goroutine so the capture path never waits.
//
// gen pins it to the stream it was started for: if the recorder reconnects,
// SetMappingChannel bumps the generation and this drainer retires without touching
// the new stream's queue or its drainer slot.
func (m *SyncMockManager) drainMappingOverflow(ch chan<- models.TestMockMapping, streamCtx context.Context, gen uint64) {
	var done <-chan struct{}
	if streamCtx != nil {
		done = streamCtx.Done()
	}

	for {
		m.mappingOverflowMu.Lock()
		if m.mappingGen != gen {
			// Superseded: the new stream owns mappingDraining and the queue now.
			m.mappingOverflowMu.Unlock()
			return
		}
		if len(m.mappingOverflow) == 0 {
			m.mappingDraining = false
			m.mappingOverflowMu.Unlock()
			return
		}
		entry := m.mappingOverflow[0]
		m.mappingOverflow = m.mappingOverflow[1:]
		m.mappingOverflowMu.Unlock()

		select {
		case ch <- entry:
		case <-done:
			// The recorder's stream is gone, so this can never be delivered. That
			// is real data loss — say so loudly; it was silent before.
			m.mappingOverflowMu.Lock()
			if m.mappingGen != gen {
				// A new stream took over while we blocked; its queue is not ours
				// to clear and its drainer slot is not ours to release.
				m.mappingOverflowMu.Unlock()
				return
			}
			lost := len(m.mappingOverflow) + 1
			m.mappingOverflow = nil
			m.mappingDraining = false
			m.mappingOverflowMu.Unlock()
			if logger := m.dropLogger(); logger != nil {
				logger.Error("mapping stream closed with mappings still queued",
					zap.Int("lost_mappings", lost),
					zap.String("next_step", "these tests will be missing from mappings.yaml and replay will report no_mocks for them; re-record the test set"))
			}
			return
		}
	}
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
	if !m.trySendOwned(mock, owner) {
		// Told outside outChanMu: the hook may take locks of its own.
		if fn := m.onDrop.Load(); fn != nil {
			(*fn)(mock)
		}
	}
}

// OnDrop has fn told of every mock dropped on the outChan capacity path (the
// channel closed, or the send budget spent). The test cases that used such a
// mock lack it: a capture whose test cases are not named (proxyless, a
// DaemonSet) cannot find them by owner, as WasMockDroppedForTC does, and
// leaves out those that ran while the mock was requested instead. fn must not
// block. nil: nothing is told.
func (m *SyncMockManager) OnDrop(fn func(*models.Mock)) {
	if m == nil {
		return
	}
	if fn == nil {
		m.onDrop.Store(nil)
		return
	}
	m.onDrop.Store(&fn)
}

// trySendOwned is sendToOutChanOwned's send; false when the mock was dropped.
func (m *SyncMockManager) trySendOwned(mock *models.Mock, owner string) bool {
	m.outChanMu.RLock()
	defer m.outChanMu.RUnlock()
	if m.outChanClosed || m.outChan == nil {
		// Genuinely undeliverable → a real drop. Record the owner (under the
		// outChanMu.RLock already held; droppedMu is a leaf lock) so the
		// orphaned TC is suppressed instead of reaching replay mock-less.
		if owner != "" {
			m.recordDroppedTC(owner)
		}
		return false
	}
	select {
	case m.outChan <- mock:
		return true
	default:
	}
	// Fast path full. Bounded block so normal scheduling jitter
	// doesn't cost us a mock.
	timer := time.NewTimer(sendBudget)
	select {
	case m.outChan <- mock:
		timer.Stop()
		return true
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
		return false
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

// noteSending records mocks on their way to outChan (PendingIn). Callers
// that take them out of the buffer hold mu.
func (m *SyncMockManager) noteSending(mocks ...*models.Mock) {
	if len(mocks) == 0 {
		return
	}
	m.sendingMu.Lock()
	defer m.sendingMu.Unlock()
	if m.sending == nil {
		m.sending = make(map[*models.Mock]time.Time)
	}
	for _, mk := range mocks {
		if mk != nil {
			m.sending[mk] = mk.Spec.ReqTimestampMock
		}
	}
}

// sent says a mock's send has finished: delivered, or dropped and told.
func (m *SyncMockManager) sent(mk *models.Mock) {
	m.sendingMu.Lock()
	delete(m.sending, mk)
	m.sendingMu.Unlock()
}

// noteTaken is noteSending for a batch taken out of the buffer. Caller holds
// mu.
func (m *SyncMockManager) noteTaken(batch []ownedMock) {
	mocks := make([]*models.Mock, 0, len(batch))
	for _, om := range batch {
		mocks = append(mocks, om.mock)
	}
	m.noteSending(mocks...)
}

// sendTaken sends mocks taken out of the buffer (noteTaken), and says each is
// sent once its send has finished (or dropped it).
func (m *SyncMockManager) sendTaken(batch []ownedMock) {
	for _, om := range batch {
		m.sendToOutChanOwned(om.mock, om.owner)
		m.sent(om.mock)
	}
}

// PendingIn reports whether the manager still holds a mock requested within
// [start, end] that it has not handed on: buffered until a window claims it,
// or on its way to outChan. A mock it drops on the way is told to OnDrop
// before it stops being pending. So once a test case's connections' parsers
// have got past its end, PendingIn false says each of its mocks has been
// handed on or dropped, and a drop has been told (syncMock settle.go).
//
// Only mocks of the window count: a full outChan holds back the test cases
// whose own mocks wait on it, not every test case. It looks at every buffered
// mock and every mock being sent, under mu: the hold asks it once per test
// case whose other conditions are met, and the buffer holds only what is not
// yet claimed.
//
// Mocks held for requests in flight (OpenWindow) do not count, but for one
// owed back to a resolved kept window if the request that claims it does not
// take it (heldMock.owner), and one owed to it already (owed): what lies in
// the window's part after its yield (Window.Yield) and was left, or held, for
// a request that claimed it then (dropHeldLocked). No other held mock
// can be a mock of a test case whose window is resolved, which is the only
// kind that settles: a kept resolve takes every held mock inside its window,
// and a mock of its window decoded later is claimed by the resolved window,
// never held. Counted, they would hold back test cases of this and every other
// manager asked until some unrelated request in flight ended. Only the
// synchronous ingress yields, so elsewhere no held mock counts.
func (m *SyncMockManager) PendingIn(start, end time.Time) bool {
	if m == nil {
		return false
	}
	in := func(t time.Time) bool { return !t.IsZero() && !t.Before(start) && !t.After(end) }
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mk := range m.buffer {
		if mk != nil && in(mk.Spec.ReqTimestampMock) {
			return true
		}
	}
	if m.heldShared > 0 {
		lo, hi := m.heldRangeLocked(start, end)
		for _, h := range m.held[lo:hi] {
			if h.owner != nil {
				return true
			}
		}
	}
	for _, o := range m.owed {
		if in(o.mock.Spec.ReqTimestampMock) {
			return true
		}
	}
	m.sendingMu.Lock()
	defer m.sendingMu.Unlock()
	for _, t := range m.sending {
		if in(t) {
			return true
		}
	}
	return false
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
		if fn := m.onDrop.Load(); fn != nil {
			(*fn)(mock)
		}
		return
	case bound && !m.firstReqSeen:
		m.noteSending(mock) // under mu: it leaves no gap for PendingIn
		m.mu.Unlock()
		m.sendToOutChan(mock)
		m.sent(mock)
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
	m.noteSending(mock)
	defer m.sent(mock)
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
	var tally dropTally

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
	openFrom, open := m.openFromLocked()
	var refs ownerRefs

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
		w, ok, shared, behind := m.ownerOrSharedLocked(mock.Spec.ReqTimestampMock)
		if ok {
			if !w.keep {
				// A static-dedup duplicate's window (sync mode): its late mocks
				// go the way its in-time ones went in ResolveRange — pruned,
				// except a startup-window mock, which is rescued with no owner
				// and no mapping (a duplicate's testName is synthetic), and one
				// a request still in flight may own, which is held for it.
				if isStartupMock(mock) {
					mock.Name = "mock-" + generateRandomString(8)
					mocksToSend = append(mocksToSend, ownedMock{mock: mock})
				} else {
					m.holdOrDropLocked(mock, droppedDuplicateLeftover, openFrom, open, &tally, refs.of(shared, behind))
				}
				continue
			}
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
		// STARTUP RESCUE: a startup-window mock the ownerWindowLocked check above
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
		if behind {
			// In a resolved kept window's part after its yield, and a request
			// in flight claims it: held for that request now, with the window
			// it goes back to if that request ends without it.
			m.holdForClaimLocked(mock, &tally, refs.of(shared, true))
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
	gaveUp := m.fitHoldLocked(&tally)
	m.handOwedLocked(outChanBound, &mocksToSend, &lateMappings)
	hold := m.holdStateLocked()
	m.noteTaken(mocksToSend)
	m.mu.Unlock()
	m.reportGivenUp(gaveUp, LostToHoldBound)
	m.settlePool()
	if tally.changed() {
		// Check first, here and in the other reapers' diagnostics: the fields
		// are built only for a logger that writes them. A reaper runs per
		// request, and its fields are a few KiB of garbage otherwise.
		if ce := m.dropLogger().Check(zap.DebugLevel, "diag/FlushOwnedWindows: buffer transition"); ce != nil {
			fields := append([]zap.Field{zap.Int("mocks_flushed", len(mocksToSend))}, tally.fields()...)
			ce.Write(append(fields, hold.fields()...)...)
		}
	}

	// Send AFTER releasing m.mu — sendToOutChan takes outChanMu and may
	// block up to sendBudget; holding m.mu across it would wedge AddMock.
	m.sendTaken(mocksToSend)
	if mappingChan != nil {
		for tn, ids := range lateMappings {
			if len(ids) == 0 {
				continue
			}
			m.sendMapping(mappingChan, models.TestMockMapping{TestName: tn, MockIDs: ids})
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

// Window is the window of a request whose headers have been read and whose
// verdict (kept, or a static-dedup duplicate) is not known yet; see OpenWindow.
// A nil Window's methods are no-ops, and Keep reports true.
type Window struct {
	m     *SyncMockManager
	start time.Time
	once  sync.Once
	// idx is the window's place in m.open, -1 once it left it (ended or
	// given up). Guarded by m.mu.
	idx int
	// lost is told, once, by a Keep that finds the window given up, with why.
	// What it returns takes its telling back (Replaced). Touched only inside
	// lostOnce, which drops it once it has told it.
	lost     func(LossCause) (takeBack func())
	lostOnce sync.Once
	// kept, givenUp, cause, told, toldEpoch and takeBack are guarded by m.mu.
	// kept: Claim (or Keep) claimed it, so it is never given up. givenUp: it
	// was given up, for cause (fitHoldLocked, giveUpForPressureLocked). told:
	// Keep told its loss, and Replaced has not taken it back; toldEpoch is
	// the manager's lossEpoch then (the loss is on the tallies open then,
	// which are those still open whose epoch is no later), and takeBack is
	// what lost returned. told sits with the other flags: the window stays
	// one 112 B allocation.
	kept      bool
	givenUp   bool
	told      bool
	cause     LossCause
	toldEpoch uint64
	takeBack  func()
	// yieldAt is when its request stopped being the only one in flight
	// (Yield), in Unix nanoseconds; 0 while it is. Guarded by m.mu.
	yieldAt int64
}

// LossCause is why a window was given up while its request was in flight.
type LossCause int

const (
	// LostToHoldBound: the mocks the process held for its requests in flight
	// passed MaxHeldBytes, and this was the oldest request not claimed of the
	// managers holding more than their share.
	LostToHoldBound LossCause = iota + 1
	// LostToMemoryPressure: memory pressure let go of mocks it may own.
	LostToMemoryPressure
)

func (c LossCause) String() string {
	switch c {
	case LostToHoldBound:
		return "hold_bound"
	case LostToMemoryPressure:
		return "memory_pressure"
	}
	return "unknown"
}

// OpenWindow registers the window of a request whose headers have been read
// and whose verdict is not known yet. From start on, every per-test mock may be
// that request's: mocks are attributed by time alone, and its egress calls are
// buffered long before its response is complete. So while the window is open,
// no reaper drops a per-test mock requested at or after start — not the stale
// cutoff, not a duplicate's prune — unless a resolved KEPT window owns it,
// which still takes it. Such a mock is held (see held) and goes to the first
// kept window resolved over it, usually this request's own.
//
// The caller decides the request, then:
//   - kept: Keep (or Claim), then ResolveKept, which takes what was held for
//     it and ends the window in one step. Keep false means the window was
//     given up: some mocks it may own were let go, so leave the test case
//     out, and resolve nothing for it (it is no duplicate: a prune over its
//     span would drop the mocks of the calls that ran beside it); the loss is
//     counted and told.
//   - a duplicate: Close BEFORE the prune, so its own window does not hold its
//     own debris.
//   - given up on (an early exit, a failed read): Close.
//
// Ending a window drops the held mocks no window still open may own: each was
// held instead of being dropped as a duplicate's leftover or a mock outside any
// window, and that is what it still is.
//
// The hold is bounded, with the holds of the process's other managers, by
// MaxHeldBytes: past it the oldest window not claimed of the managers holding
// more than their share is given up, which costs exactly that request's test
// case if it is kept. Memory pressure gives up every unclaimed window that may
// own a held mock and drops those mocks (SetMemoryPressure). lost, if not nil,
// is told the cause, once,
// by the Keep that finds its window given up, for the caller's loss accounting;
// what it returns, if not nil, takes that back, and Replaced calls it when a
// later request is recorded in this one's place. A zero start registers
// nothing.
//
// The ingress carries the window to the capture hook in its ctx (WithWindow),
// and closes it once the hook has returned, on every path: a window left open
// keeps every later droppable mock held for it, until the bound gives it up.
//
// Only HTTP/1 ingress opens windows (the incoming proxy here, and the
// enterprise eBPF parser). gRPC capture (CaptureGRPC) opens none and resolves
// its window only in sync mode, so a gRPC call is not covered: one that outlives
// the stale horizon while other requests are decided can still lose the mocks
// it made early on.
func (m *SyncMockManager) OpenWindow(start time.Time, lost func(LossCause) (takeBack func())) *Window {
	if m == nil || start.IsZero() {
		return nil
	}
	w := &Window{m: m, start: start, lost: lost}
	m.mu.Lock()
	heap.Push(&m.open, w)
	m.noteOldestLocked()
	m.mu.Unlock()
	return w
}

// Start reports the window's start: the request time its capture stamps, from
// which on every per-test mock may be its request's. Zero for a nil window.
func (w *Window) Start() time.Time {
	if w == nil {
		return time.Time{}
	}
	return w.start
}

// OpenWindows reports how many windows are open.
func (m *SyncMockManager) OpenWindows() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.open)
}

// Keep claims the window for a kept resolve: from now on it is never given up.
// It reports false when it already was: mocks this request may own were let go,
// so its test case must not be recorded. That is the loss a given-up window
// costs, and only a kept one costs it, so it is told here, once: the manager
// counts and warns of it (KeptRequestsLeftOut), the tallies open count it
// (LossTally), and the window's lost is told the cause. A nil window reports
// true.
//
// A caller that records one test case for many requests of a kind (a static
// deduper: one per schema) tells the loss of the first of them only, and takes
// it back (Replaced) when a later request of the kind is recorded: one test
// case is at stake for the kind, however many of its requests are left out.
func (w *Window) Keep() bool {
	if w.Claim() {
		return true
	}
	w.lostOnce.Do(func() {
		w.m.mu.Lock()
		cause := w.cause
		w.m.mu.Unlock()
		w.m.reportLeftOut(w, cause)
		var takeBack func()
		if w.lost != nil {
			takeBack = w.lost(cause)
			// Told. The window stays referenced for as long as the loss
			// stands (whoever may take it back holds Replaced), and lost
			// closes over the request's connection: let go of it.
			w.lost = nil
		}
		w.m.mu.Lock()
		w.told, w.takeBack = true, takeBack
		// On the tallies open now, in the same step: a tally opened later
		// has a later epoch than this, so the take-back passes it by.
		w.toldEpoch = w.m.lossEpoch
		for _, t := range w.m.lossTallies {
			t.told++
		}
		w.m.mu.Unlock()
	})
	return false
}

// Replaced says the test case this window's request was left out of the
// recording as (Keep reported false, and told it) is in the recording after
// all: a later request was recorded in its place. The loss is taken back: out
// of the manager's count (KeptRequestsLeftOut), off the tallies that counted
// it (LossTally) and from whoever the window's lost told. A no-op on a window
// whose loss was not told, or was taken back already, and on a nil one.
//
// The request recorded in its place may be another session's: a deduper that
// outlives a session (one that runs for the agent's life) takes a loss told in
// one session back in a later one. That loss was never on the later session's
// tally, so its take-back is not either.
func (w *Window) Replaced() {
	if w == nil {
		return
	}
	w.m.mu.Lock()
	told, takeBack := w.told, w.takeBack
	w.told, w.takeBack = false, nil
	if told {
		for _, t := range w.m.lossTallies {
			if t.epoch <= w.toldEpoch {
				t.takenBack++
			}
		}
	}
	w.m.mu.Unlock()
	if !told {
		return
	}
	w.m.keptLeftOut.Add(^uint64(0))
	if takeBack != nil {
		takeBack()
	}
}

// Claim claims the window for a kept resolve as Keep does, but tells nothing:
// it reports false when the window was already given up, and leaves telling
// that loss to a Keep the caller makes afterwards. It is for a caller that
// decides under its own lock whether the request is kept at all — a static
// deduper counting the request's schema only if it can still be recorded — and
// must not run the loss reporting, or the window's lost, under that lock. A
// claimed window must be resolved (ResolveKept) at once: until then it pins
// the hold. A nil window reports true.
func (w *Window) Claim() bool {
	if w == nil {
		return true
	}
	w.m.mu.Lock()
	defer w.m.mu.Unlock()
	if w.givenUp {
		return false
	}
	w.kept = true
	w.m.noteOldestLocked()
	return true
}

// Yield says that from at on, the window's request is no longer the only one in
// flight. The synchronous ingress calls it at the response's headers when it
// has given its lock back early (for a response, or a request body, of unknown
// length: the next request then runs beside the rest of this one), and at the
// response's last byte (the request is in flight no more).
//
// Mocks are attributed by time alone. A window claims the span from its start
// to its yield, or on with no end while it is open and has not yielded. In
// synchronous mode, one whose request gave the lock back early claims up to
// its response's headers, even when that was at a request body of unknown
// length, before them (as on main, it takes what was made meanwhile, another
// stream's calls included); one whose request kept the lock claims up to when
// its response's last byte came from the app (the lock may stay held longer,
// while a slow client takes the body: a call made then is not its).
//
// From at on, the window's kept resolve (ResolveKept) leaves a mock that
// another request claims (a window still open) or was decided over first (a
// resolved kept window) to that request, and takes the rest, up to the end it
// is given. What it leaves is held for the request that claims it, and owed
// back to it, recorded with the mock, once no request in flight claims it (that
// request ended without taking it: heldMock.owner, oweUnclaimedLocked); a mock
// decoded later that lands in that part while a request still claims it is held
// the same way by the first resolve, flush or prune that looks it up
// (holdForClaimLocked). Before at it takes all of its span, as a window that
// never yields (any other ingress) does: where two such spans overlap, the
// first resolved takes what both span. The earliest yield counts. A no-op on a
// nil window, or for a zero at.
func (w *Window) Yield(at time.Time) {
	if w == nil || at.IsZero() {
		return
	}
	n := at.UnixNano()
	w.m.mu.Lock()
	if w.yieldAt == 0 || n < w.yieldAt {
		w.yieldAt = n
		if w.idx >= 0 {
			// It claims nothing after at any more: a held mock it was the
			// last to claim there goes back to its owner now, not when this
			// window ends (its yield may land after mocks made past it).
			w.m.oweUnclaimedLocked(at)
			w.m.publishHoldLocked()
		}
	}
	w.m.mu.Unlock()
}

// KeptRequestsLeftOut reports how many kept requests were left out of the
// recording because their window was given up while they were in flight
// (Window.Keep reported false), less those a later request has been recorded
// in the place of (Window.Replaced): the test cases the recording lacks for it.
func (m *SyncMockManager) KeptRequestsLeftOut() uint64 {
	if m == nil {
		return 0
	}
	return m.keptLeftOut.Load()
}

// LossTally counts one recording session's own losses on a manager: the kept
// requests left out of the recording (Window.Keep reported false) while it is
// open, less those of them taken back (Window.Replaced). A loss told before it
// opened is not on it, nor is its take-back, whenever that comes: the
// manager's count (KeptRequestsLeftOut) is shared by every session the
// manager serves, so the growth of that count over a session is not the
// session's loss. Each tally is exact while other tallies are open beside it.
type LossTally struct {
	m     *SyncMockManager
	epoch uint64
	// told and takenBack are guarded by m.mu. takenBack never passes told: a
	// window tells its loss once and has it taken back at most once, and
	// only off the tallies that counted it.
	told      uint64
	takenBack uint64
}

// OpenLossTally opens a tally of the losses told from now on (LossTally).
// Close it when the session ends: an open tally costs every loss told or taken
// back a step, and it stays referenced until then. Nil for a nil manager.
func (m *SyncMockManager) OpenLossTally() *LossTally {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lossEpoch++
	t := &LossTally{m: m, epoch: m.lossEpoch}
	m.lossTallies = append(m.lossTallies, t)
	return t
}

// LeftOut reports the losses told while the tally has been open, less those of
// them taken back: the test cases its session's recording lacks for it. 0 for a
// nil tally.
func (t *LossTally) LeftOut() uint64 {
	if t == nil {
		return 0
	}
	t.m.mu.Lock()
	defer t.m.mu.Unlock()
	return t.told - t.takenBack
}

// Close ends the tally: it counts nothing more, and LeftOut keeps reporting
// what it counted. Idempotent, and a no-op on a nil tally.
func (t *LossTally) Close() {
	if t == nil {
		return
	}
	t.m.mu.Lock()
	defer t.m.mu.Unlock()
	if i := slices.Index(t.m.lossTallies, t); i >= 0 {
		t.m.lossTallies = slices.Delete(t.m.lossTallies, i, i+1)
	}
}

// OpenLossTallies reports how many tallies are open (OpenLossTally).
func (m *SyncMockManager) OpenLossTallies() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.lossTallies)
}

// Close ends the window and drops the held mocks no window still open may own
// any more. Idempotent, and a no-op on a window ResolveKept already ended.
func (w *Window) Close() {
	if w == nil {
		return
	}
	w.once.Do(func() {
		m := w.m
		var t dropTally
		m.mu.Lock()
		m.endWindowLocked(w, &t, false)
		m.publishHoldLocked()
		hold := m.holdStateLocked()
		m.mu.Unlock()
		if t.dropped() > 0 {
			if ce := m.dropLogger().Check(zap.DebugLevel, "diag/syncMock: window ended: dropped the held mocks no request in flight may own"); ce != nil {
				ce.Write(append(t.fields(), hold.fields()...)...)
			}
		}
	})
}

// endWindowLocked takes w out of the open windows, if it still is one, and
// drops the held mocks no window still open may own, counting them in t: as
// the cleanup they were held instead of when w ended normally, as mocks let go
// with a given-up window (givenUp) when the hold gave it up. What is owed back
// to a resolved kept window and no window still open claims any more goes to
// it (oweUnclaimedLocked). Caller holds mu.
func (m *SyncMockManager) endWindowLocked(w *Window, t *dropTally, givenUp bool) {
	if w.idx >= 0 {
		heap.Remove(&m.open, w.idx)
		m.noteOldestLocked()
	}
	m.releaseHeldLocked(t, givenUp, w.start)
}

// releaseHeldLocked lets go, as a window that started at ended ends, of the
// held mocks no open window may own: those requested before the earliest open
// start, or all of them when none is open (dropHeldLocked); and those owed
// back to a resolved kept window that no open window claims any more
// (oweUnclaimedLocked). Caller holds mu.
func (m *SyncMockManager) releaseHeldLocked(t *dropTally, givenUp bool, ended time.Time) {
	n := len(m.held)
	if n == 0 {
		return // the common case: a window ends with nothing held
	}
	if from, open := m.openFromLocked(); open {
		n = sort.Search(len(m.held), func(i int) bool { return !m.held[i].mock.Spec.ReqTimestampMock.Before(from) })
	}
	if n > 0 {
		m.dropHeldLocked(n, t, givenUp)
	}
	m.oweUnclaimedLocked(ended)
}

// oweUnclaimedLocked moves to owed the held mocks that go back to a resolved
// kept window (heldMock.owner) and that no window still open claims any more
// (openClaimLocked), looking at those requested at or after from: the request
// they were held for has ended, or yielded before them, without them. Only a
// window that claims such a mock can take it: a kept resolve of one that
// yielded before it leaves it (takeHeldLocked), and one that started after it
// does not span it. So an older window still open, a long stream that gave its
// lock back before the mock, must not keep it held: it would hold the owner's
// test case back (PendingIn) for as long as it stays open, and count against
// the hold's budget.
//
// A held mock with an owner is held while a window claims it
// (holdForClaimLocked, takeHeldLocked), and a window stops claiming it only as
// it ends or yields before it, which is where this runs: as a window ends
// (endWindowLocked: a resolve, a close, the hold's bound giving it up), from
// its start; as it yields (Window.Yield), from the yield. So only the mocks
// that window could have claimed and no longer does are looked at: a long
// stream's leftovers held from before are not walked at every window end. A
// yield can land after mocks made past it: the synchronous loop stamps a
// response's headers, then yields under the manager's lock, and an earlier
// stream may be decided in between, holding such a mock for this window to go
// back to it; and every window opened after it starts after that mock. Memory
// pressure gives windows up without ending them here, but only those that may
// own what it drops, before the start of the claimed window it stops at; what
// is held after that start is looked at when that window ends, in its
// ResolveKept. Nothing is looked at while no held mock has an owner
// (heldShared), which is always the case but with the synchronous ingress.
// Caller holds mu.
func (m *SyncMockManager) oweUnclaimedLocked(from time.Time) {
	if m.heldShared == 0 {
		return
	}
	lo := sort.Search(len(m.held), func(i int) bool { return !m.held[i].mock.Spec.ReqTimestampMock.Before(from) })
	stay := lo
	for i := lo; i < len(m.held); i++ {
		h := m.held[i]
		if h.owner != nil && !m.openClaimLocked(h.mock.Spec.ReqTimestampMock, nil) {
			m.owed = append(m.owed, owedMock{mock: h.mock, to: h.owner})
			m.heldShared--
			m.heldBytes -= h.size
			continue
		}
		m.held[stay] = h
		stay++
	}
	clear(m.held[stay:])
	m.held = m.held[:stay]
	if stay == 0 && cap(m.held) > heldKeepCap {
		m.held = nil // do not keep a burst's backing array
	}
}

// dropHeldLocked takes held[:n] out of the hold, when no open window that has
// not been given up may own any of them. One that lies in the part of a
// resolved kept window after its yield (heldMock.owner) is owed to that
// window: it was left, or held, for a request that claimed it then, and with
// no request in flight claiming it now it is that window's, which the next
// resolve, prune or flush hands it to (handOwedLocked). No other resolved kept
// window owns one: one resolved over it took it (ResolveKept, ResolveRange).
// The rest is dropped and counted in t (if not nil): as the cleanup each was
// held instead of, or, when a window was given up (givenUp), as let go with
// it, the loss a kept verdict on it would reveal. Returns how many were
// dropped. Caller holds mu.
func (m *SyncMockManager) dropHeldLocked(n int, t *dropTally, givenUp bool) (dropped int) {
	for _, h := range m.held[:n] {
		if h.owner != nil {
			m.owed = append(m.owed, owedMock{mock: h.mock, to: h.owner})
			continue
		}
		dropped++
		switch {
		case t == nil:
		case givenUp:
			t.givenUpDrops++
		default:
			t.count(h.why)
		}
	}
	m.unholdLocked(0, n)
	return dropped
}

// earliestOpenLocked returns the open window that started first, nil when
// none is open. O(1). Caller holds mu.
func (m *SyncMockManager) earliestOpenLocked() *Window {
	if len(m.open) == 0 {
		return nil
	}
	return m.open[0]
}

// windowHeap is the open windows, a min-heap on start (container/heap).
type windowHeap []*Window

func (h windowHeap) Len() int           { return len(h) }
func (h windowHeap) Less(i, j int) bool { return h[i].start.Before(h[j].start) }
func (h windowHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].idx, h[j].idx = i, j
}
func (h *windowHeap) Push(x any) {
	w := x.(*Window)
	w.idx = len(*h)
	*h = append(*h, w)
}
func (h *windowHeap) Pop() any {
	old := *h
	n := len(old)
	w := old[n-1]
	old[n-1] = nil
	w.idx = -1
	*h = old[:n-1]
	return w
}

// openFromLocked returns the earliest start of a window still open: every
// per-test mock requested at or after it may be a request's that is not decided
// yet. open is false when no window is open. Caller holds mu.
func (m *SyncMockManager) openFromLocked() (from time.Time, open bool) {
	if w := m.earliestOpenLocked(); w != nil {
		return w.start, true
	}
	return time.Time{}, false
}

// openClaimLocked reports whether a window still open other than self claims
// t: it started at or before t and had not yielded (Window.Yield) by then, so
// its request was the only one in flight at t. O(open windows); only a mock
// in the part of a window after its yield is asked about. Caller holds mu.
func (m *SyncMockManager) openClaimLocked(t time.Time, self *Window) bool {
	for _, o := range m.open {
		if o == self || o.start.After(t) {
			continue
		}
		if o.yieldAt == 0 || o.yieldAt >= t.UnixNano() {
			return true
		}
	}
	return false
}

// claim is a span in which one request was the only one in flight: from its
// window's start to its yield, or on with no end (to zero) while it has not
// yielded and is still open.
type claim struct{ from, to time.Time }

// claims is the spans other requests claim, sorted by from and merged, so
// covers is a binary search.
type claims []claim

// covers reports whether t lies within one of the spans.
func (c claims) covers(t time.Time) bool {
	i := sort.Search(len(c), func(i int) bool { return c[i].from.After(t) }) // the first span starting after t
	return i > 0 && (c[i-1].to.IsZero() || !t.After(c[i-1].to))
}

// claimsLocked returns what requests other than self's have the better right to
// within (after, end]: windows still open that started by end and had not
// yielded by after, from their start to their yield (or with no end); and
// resolved kept windows, over all of their span that reaches past after. Over
// its exclusive part a resolved window holds the lock's claim; over its part
// after its yield it was decided first, and what both span is its, as the late
// paths decide (ownerOrSharedLocked): what its resolve left to a request that
// claimed it goes back to it, not to a window decided after it. A resolved
// duplicate claims nothing: a kept window may take a duplicate's leftovers, as
// everywhere else. One pass over the open windows and the resolved ones, for
// the kept resolve of a window that yielded. Caller holds mu.
func (m *SyncMockManager) claimsLocked(self *Window, after, end time.Time) claims {
	var c claims
	for _, o := range m.open {
		if o == self || o.start.After(end) || (o.yieldAt != 0 && o.yieldAt <= after.UnixNano()) {
			continue
		}
		var to time.Time
		if o.yieldAt != 0 {
			to = time.Unix(0, o.yieldAt)
		}
		c = append(c, claim{from: o.start, to: to})
	}
	for _, r := range m.recentWindows {
		if !r.keep || r.start.After(end) || !r.end.After(after) {
			continue
		}
		c = append(c, claim{from: r.start, to: r.end})
	}
	return mergeClaims(c)
}

// mergeClaims sorts c by from and merges the spans that overlap, in place.
func mergeClaims(c claims) claims {
	if len(c) < 2 {
		return c
	}
	sort.Slice(c, func(i, j int) bool { return c[i].from.Before(c[j].from) })
	merged := c[:1]
	for _, s := range c[1:] {
		last := &merged[len(merged)-1]
		if last.to.IsZero() || !s.from.After(last.to) {
			if s.to.IsZero() || (!last.to.IsZero() && s.to.After(last.to)) {
				last.to = s.to
			}
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

// dropReason is why a reaper was about to drop a per-test mock: the reason it
// is dropped for, or held for while a window still open may own it.
type dropReason int

const (
	// droppedDuplicateLeftover: inside a static-dedup duplicate's window or
	// before its prune horizon, and no kept window owns it.
	droppedDuplicateLeftover dropReason = iota
	// droppedOutsideAnyWindow: older than the stale horizon, and no resolved
	// window owns it.
	droppedOutsideAnyWindow
)

// heldKeepCap is the most entries an emptied hold keeps its backing array for:
// what a burst grew beyond that is given back.
const heldKeepCap = 1024

// heldMock is a mock the hold keeps for the requests in flight, with the reason
// it would otherwise have been dropped for.
type heldMock struct {
	mock *models.Mock
	why  dropReason
	// owner, if not nil, is the resolved kept window whose part after its
	// yield (Window.Yield) covers it: it was held, or left by that window's
	// resolve, for a request that claimed it. Let go of with no request in
	// flight claiming it, it is owed to owner (dropHeldLocked, oweUnclaimedLocked), and
	// until it is handed on it is pending for owner's test case (PendingIn).
	// Recorded here, not looked up in the ring later: the request that
	// claimed it may stay in flight while the ring's 8192 windows turn over.
	owner *resolvedWindow
	// size is about how many bytes of heap the mock takes (mockSize), taken
	// once, as it is held.
	size int64
}

// owedMock is a mock owed to a resolved kept window (dropHeldLocked).
type owedMock struct {
	mock *models.Mock
	to   *resolvedWindow
}

// handOwedLocked hands what is owed on to the windows it is owed to, as the
// retroactive bin does a late mock: into send, and into late (when the window
// maps its mocks). Not while the output is unwired: it waits. Returns how many
// it handed on. Caller holds mu.
func (m *SyncMockManager) handOwedLocked(outChanBound bool, send *[]ownedMock, late *map[string][]string) int {
	if !outChanBound || len(m.owed) == 0 {
		return 0
	}
	n := len(m.owed)
	for _, o := range m.owed {
		o.mock.Name = "mock-" + generateRandomString(8)
		if o.to.mapping {
			if *late == nil {
				*late = make(map[string][]string)
			}
			(*late)[o.to.testName] = append((*late)[o.to.testName], o.mock.Name)
		}
		*send = append(*send, ownedMock{mock: o.mock, owner: o.to.testName})
	}
	m.owed = nil
	return n
}

// holdOrDropLocked decides a per-test mock a reaper was about to drop for why:
// held when a window still open may own it (requested at or after the earliest
// open start), else dropped. owner, if not nil, is the resolved kept window
// whose part after its yield covers it (heldMock.owner). Counted in t either
// way; reports whether it was held. held stays sorted by request time. Caller
// holds mu.
func (m *SyncMockManager) holdOrDropLocked(mk *models.Mock, why dropReason, from time.Time, open bool, t *dropTally, owner *resolvedWindow) (held bool) {
	if open && mk != nil {
		if at := mk.Spec.ReqTimestampMock; !at.IsZero() && !at.Before(from) {
			m.holdLocked(heldMock{mock: mk, why: why, size: mockSize(mk), owner: owner})
			t.held++
			return true
		}
	}
	t.count(why)
	return false
}

// holdForClaimLocked holds a per-test mock that a window still open claims
// (openClaimLocked) and that goes back to owner, a resolved kept window whose
// part after its yield covers it, if that window's request ends without it:
// one a yielded resolve leaves, or one a late path finds there, young or old.
// It is held at once, not left in the buffer for the next pass to look owner
// up in the ring again: the claimant may stay in flight while the ring's 8192
// windows turn over, and the ring would then name no window, or one decided
// after owner. An open window's claim starts at or before the mock, so a
// window still open may own it, which is what the hold is for. Counted in t.
// Caller holds mu.
func (m *SyncMockManager) holdForClaimLocked(mk *models.Mock, t *dropTally, owner *resolvedWindow) {
	m.holdLocked(heldMock{mock: mk, why: droppedOutsideAnyWindow, size: mockSize(mk), owner: owner})
	t.held++
}

// ownerRefs hands a pass over the buffer the records of the windows the mocks
// it holds go back to (heldMock.owner): ownerOrSharedLocked returns a window
// by value, from the ring, which moves its entries as it turns over, so a held
// mock carries a copy of its own. Consecutive mocks behind the same window
// share one copy, so a pass makes one record per window it holds mocks for,
// not one per mock. The zero value is ready to use.
type ownerRefs struct{ last *resolvedWindow }

// of returns the record of w, nil when not ok.
func (r *ownerRefs) of(w resolvedWindow, ok bool) *resolvedWindow {
	if !ok {
		return nil
	}
	if r.last == nil || *r.last != w {
		c := w
		r.last = &c
	}
	return r.last
}

// holdLocked puts h in the hold, in request order, and counts its size.
// Caller holds mu.
func (m *SyncMockManager) holdLocked(h heldMock) {
	at := h.mock.Spec.ReqTimestampMock
	i := sort.Search(len(m.held), func(i int) bool { return at.Before(m.held[i].mock.Spec.ReqTimestampMock) })
	m.held = append(m.held, heldMock{})
	copy(m.held[i+1:], m.held[i:])
	m.held[i] = h
	m.heldBytes += h.size
	if h.owner != nil {
		m.heldShared++
	}
}

// unholdLocked takes held[lo:hi] out of the hold and its sizes out of the
// counts. Caller holds mu, and has what it needs of them already.
func (m *SyncMockManager) unholdLocked(lo, hi int) {
	for i := lo; i < hi; i++ {
		m.heldBytes -= m.held[i].size
		if m.held[i].owner != nil {
			m.heldShared--
		}
	}
	k := lo + copy(m.held[lo:], m.held[hi:])
	clear(m.held[k:])
	m.held = m.held[:k]
	if k == 0 && cap(m.held) > heldKeepCap {
		m.held = nil // do not keep a burst's backing array
	}
}

// heldRangeLocked returns the bounds [lo, hi) of the held mocks requested
// within [start, end]. Caller holds mu.
func (m *SyncMockManager) heldRangeLocked(start, end time.Time) (lo, hi int) {
	lo = sort.Search(len(m.held), func(i int) bool { return !m.held[i].mock.Spec.ReqTimestampMock.Before(start) })
	hi = sort.Search(len(m.held), func(i int) bool { return m.held[i].mock.Spec.ReqTimestampMock.After(end) })
	return lo, hi
}

// takeHeldLocked removes and returns the held mocks requested within
// [start, end], in request order. A window that yielded (sharedFrom not zero)
// leaves those requested after its yield that another request claims (others:
// see Window.Yield), which stay held for it, recorded as owed to self if that
// request does not take them; and those already owed back to a window decided
// before it (heldMock.owner), which it would otherwise take once the ring has
// let go of that window. Caller holds mu.
func (m *SyncMockManager) takeHeldLocked(start, end, sharedFrom time.Time, others claims, self func() *resolvedWindow) []*models.Mock {
	lo, hi := m.heldRangeLocked(start, end)
	if lo >= hi {
		return nil
	}
	taken := make([]*models.Mock, 0, hi-lo)
	if sharedFrom.IsZero() {
		for _, h := range m.held[lo:hi] {
			taken = append(taken, h.mock)
		}
		m.unholdLocked(lo, hi)
		return taken
	}
	// Compact held[lo:hi] down to what stays, in order, then close the gap.
	stay := lo
	for i := lo; i < hi; i++ {
		h := m.held[i]
		if at := h.mock.Spec.ReqTimestampMock; at.After(sharedFrom) && (h.owner != nil || others.covers(at)) {
			if h.owner == nil { // left in this window's part after its yield
				h.owner = self()
				m.heldShared++
			}
			m.held[stay] = h
			stay++
			continue
		}
		taken = append(taken, h.mock)
		if h.owner != nil {
			m.heldShared--
		}
		m.heldBytes -= h.size
	}
	k := stay + copy(m.held[stay:], m.held[hi:])
	clear(m.held[k:])
	m.held = m.held[:k]
	if k == 0 && cap(m.held) > heldKeepCap {
		m.held = nil
	}
	return taken
}

// fitHoldLocked ends a reaper call's change to the hold: it keeps the process
// within MaxHeldBytes where the call's own manager is the one to give a request
// up, and publishes the hold to the process's budget (publishHoldLocked).
//
// While what m holds would take the pool past its budget, and m's oldest
// request in flight is the oldest of those of the managers holding more than
// their share (holdPool.oldestOverShare), that request is given up: it stops
// holding, and the mocks only it could own are dropped. Its request loses
// mocks it may own, so if it is kept its test case is not recorded (Keep
// reports false), and that one test case is the whole cost; a duplicate's
// costs nothing, and every other request keeps what it owns. Where the oldest
// such request is another manager's, m does not take that manager's lock under
// its own: it publishes, and once it has let go of its lock settlePool gives
// that request up. It gives up only with the pool's shedMu, which it tries
// for and never waits on under its lock (a settle holding it may be waiting
// for m's lock): while a settle runs, that settle decides, so the two never
// give a request up each for the same excess.
//
// It stops at a claimed request: that one is between its claim and its
// ResolveKept, which takes what it owns and ends it with no I/O in between, so
// the hold is over the budget only for that moment. Returns the windows given
// up; the drops are counted in t. Caller holds mu, and after releasing it
// reports the windows (reportGivenUp) and settles the pool (settlePool).
func (m *SyncMockManager) fitHoldLocked(t *dropTally) (gaveUp []*Window) {
	p := m.poolLocked()
	over := func() bool { return m.heldBytes > m.pooled && p.total.Load()+m.heldBytes-m.pooled > p.limit }
	if over() && p.shedMu.TryLock() {
		for over() {
			if v, _ := p.oldestOverShare(m, m.heldBytes, m.oldestUnclaimedLocked(), nil); v != m {
				break
			}
			w := m.earliestOpenLocked()
			w.givenUp, w.cause = true, LostToHoldBound
			gaveUp = append(gaveUp, w)
			m.endWindowLocked(w, t, true)
		}
		// Published before shedMu is let go: the next settle sees what these
		// give-ups freed, and does not give up another request for an excess
		// they already took away.
		m.publishHoldLocked()
		p.unlockOwnShed()
	}
	t.windowsGivenUp += len(gaveUp)
	m.publishHoldLocked()
	return gaveUp
}

// giveUpForPressureLocked lets go of the hold under memory pressure, whatever
// its size and whatever room the process's budget has left: the agent is short
// of memory. One that
// is owed back to a resolved kept window once these requests are given up
// (heldMock.owner) is owed to it (dropHeldLocked), not dropped. Any other held
// mock's owner is a request not decided yet, whose window may end before the
// pressure span opened (its response complete, its capture still to run), so
// the pressure check on its test case's window would not catch it: every
// unclaimed window that may own a dropped mock is given up instead, and if its
// request is kept its test case is left out and counted (Keep). A claimed
// window is being resolved this moment (Claim, ResolveKept): what it may own is
// kept for it. Returns how many mocks were dropped and the windows given up.
// Caller holds mu.
func (m *SyncMockManager) giveUpForPressureLocked() (dropped int, gaveUp []*Window) {
	n := len(m.held)
	if n == 0 {
		return 0, nil
	}
	for _, w := range m.open {
		if w.kept {
			n = min(n, sort.Search(len(m.held), func(i int) bool { return !m.held[i].mock.Spec.ReqTimestampMock.Before(w.start) }))
		}
	}
	if n == 0 {
		return 0, nil
	}
	last := m.held[n-1].mock.Spec.ReqTimestampMock
	for _, w := range m.open {
		if !w.kept && !w.start.After(last) {
			gaveUp = append(gaveUp, w)
		}
	}
	for _, w := range gaveUp {
		w.givenUp, w.cause = true, LostToMemoryPressure
		heap.Remove(&m.open, w.idx)
	}
	m.noteOldestLocked()
	return m.dropHeldLocked(n, nil, true), gaveUp
}

// reportGivenUp logs the windows given up, for diagnosis. It is not a loss
// report: whether a given-up request loses anything is known only at its
// verdict — a kept one is left out, and Keep warns of it (reportLeftOut); a
// duplicate's costs nothing. Sampled like the outChan overflow line. Caller
// does not hold mu.
func (m *SyncMockManager) reportGivenUp(gaveUp []*Window, cause LossCause) {
	if len(gaveUp) == 0 {
		return
	}
	n := m.windowsGivenUp.Add(uint64(len(gaveUp)))
	if prev := n - uint64(len(gaveUp)); prev == 0 || prev/sendDropSampleRate != n/sendDropSampleRate {
		m.dropLogger().Debug("diag/syncMock: gave up request(s) still in flight; a kept one among them is left out of the recording at its verdict, a duplicate costs nothing",
			zap.Stringer("cause", cause),
			zap.Time("in_flight_since", gaveUp[0].start),
			zap.Int("given_up_now", len(gaveUp)),
			zap.Uint64("given_up_so_far", n),
			zap.Int64("hold_bound_bytes", MaxHeldBytes))
	}
}

// reportLeftOut counts and warns of a kept request left out of the recording
// because its window was given up while it was in flight: the loss a given-up
// window costs. Sampled like the outChan overflow line. Caller does not hold
// mu.
//
// The line says what happened and what the recording does about it. It asks
// nothing of whoever records: there is nothing to turn down that would not
// also change what is recorded.
func (m *SyncMockManager) reportLeftOut(w *Window, cause LossCause) {
	n := m.keptLeftOut.Add(1)
	// Sampled on the losses told so far, not on n: n falls again with every
	// loss taken back (Window.Replaced), and would make each next one a first.
	told := m.leftOutTold.Add(1)
	if prev := told - 1; prev == 0 || prev/sendDropSampleRate != told/sendDropSampleRate {
		msg, next := "syncMock: left a kept request out of the recording: it stayed in flight while the mocks held for the requests in flight passed their budget, so mocks it may have made were let go",
			"a request (a stream, a long poll, a slow endpoint) stayed in flight while the app made other calls; the agent holds those calls for whichever request in flight is kept, up to hold_bound_bytes for all the apps it records together, and past that the apps holding more than their share give up their oldest request in flight first. This request outlasted that. It is left out rather than recorded without mocks it may have made; every other request is recorded as ever, and a later request of the same kind that fits the budget is recorded with its mocks"
		if cause == LostToMemoryPressure {
			msg, next = "syncMock: left a kept request out of the recording: memory pressure let go of mocks it may have made while it was in flight",
				"the agent was short of memory while the request was in flight and let go of the mocks it held for the requests in flight. The request is left out rather than recorded without mocks it may have made; the memory guard's lines in this log say how close the agent ran to its limit (--memory-limit)"
		}
		m.dropLogger().Warn(msg,
			zap.Stringer("cause", cause),
			zap.Time("in_flight_since", w.start),
			zap.Uint64("left_out_so_far", n),
			zap.Uint64("left_out_told", told),
			zap.Int64("hold_bound_bytes", MaxHeldBytes),
			zap.String("next_step", next))
	}
}

// dropTally counts what one reaper call did with the mocks it did not hand on,
// for its diagnostic. Every drop it counts is cleanup: duplicate leftovers and
// mocks outside any window, which no request still in flight may own (a mock
// one may own is held instead, and dropped for the same reason once none may).
// Loss is not decided here: a window given up costs a test case only if its
// request is then kept (Window.Keep warns of that and counts it,
// KeptRequestsLeftOut).
type dropTally struct {
	// duplicateLeftovers: inside a static-dedup duplicate's window or before
	// its prune horizon, and no kept or open window may own it. Cleanup.
	duplicateLeftovers int
	// outsideAnyWindow: older than the stale horizon, and no window, resolved
	// or open, owns it. Cleanup.
	outsideAnyWindow int
	// held: moved to the hold this call instead of being dropped.
	held int
	// windowsGivenUp: requests in flight the hold gave up this call — a kept
	// one among them is left out at its verdict (that is the loss, warned by
	// Keep), a duplicate's costs nothing. givenUpDrops: the held mocks only
	// they could own, let go with them; not cleanup, and loss only through a
	// kept verdict on their request. A mock held and let go within one call is
	// in held as well.
	windowsGivenUp, givenUpDrops int
}

func (d *dropTally) count(why dropReason) {
	if why == droppedOutsideAnyWindow {
		d.outsideAnyWindow++
		return
	}
	d.duplicateLeftovers++
}

func (d dropTally) dropped() int { return d.duplicateLeftovers + d.outsideAnyWindow }

func (d dropTally) changed() bool {
	return d.dropped() > 0 || d.held > 0 || d.windowsGivenUp > 0 || d.givenUpDrops > 0
}

func (d dropTally) fields() []zap.Field {
	return []zap.Field{
		zap.Int("dropped", d.dropped()),
		zap.Int("dropped_duplicate_leftovers", d.duplicateLeftovers),
		zap.Int("dropped_outside_any_window", d.outsideAnyWindow),
		zap.Int("held_now", d.held),
		zap.Int("windows_given_up", d.windowsGivenUp),
		zap.Int("dropped_given_up", d.givenUpDrops),
	}
}

// holdState is the hold as a reaper call left it, for its diagnostic.
type holdState struct {
	mocks, open         int
	bytes, processBytes int64
}

// holdStateLocked snapshots the hold for a diagnostic. Caller holds mu.
func (m *SyncMockManager) holdStateLocked() holdState {
	return holdState{mocks: len(m.held), open: len(m.open), bytes: m.heldBytes, processBytes: m.poolLocked().total.Load()}
}

// fields: held_total and held_bytes are what this manager holds;
// held_bytes_in_process is what the process's managers hold together, as they
// last published it (MaxHeldBytes bounds it).
func (h holdState) fields() []zap.Field {
	return []zap.Field{
		zap.Int("held_total", h.mocks),
		zap.Int64("held_bytes", h.bytes),
		zap.Int64("held_bytes_in_process", h.processBytes),
		zap.Int("open_windows", h.open),
	}
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

// GetDropStats returns a snapshot of the current pressure state and drop
// counters. bufferSize counts every mock not handed on yet: the buffer's and
// those held for requests in flight.
func (m *SyncMockManager) GetDropStats() (pressureActive bool, pressureDropped int64, totalAdded int64, bufferSize int) {
	m.mu.Lock()
	pressureActive = m.memoryPause
	bufferSize = len(m.buffer) + len(m.held)
	m.mu.Unlock()
	pressureDropped = m.pressureDropped.Load()
	totalAdded = m.totalAdded.Load()
	return
}

// NoteMockLeftOut counts a mock that a session whose mocks go to m left out
// because a parser could not record it: for the incomplete-mock flag (a chunk
// the relay dropped, a short write, a message that does not decode), or
// reported by its parser for a reason of its own, such as an exchange the
// replayer cannot serve or the exchange the parser stopped on
// (supervisor.Session's ReportLeftOut, ReportStoppedOn). Nil-safe.
func (m *SyncMockManager) NoteMockLeftOut() {
	if m == nil {
		return
	}
	m.leftOut.Add(1)
}

// MocksLeftOut returns how many mocks NoteMockLeftOut has counted: the mocks
// left out because a parser could not record them, over the manager's life,
// like the drop counters (GetDropStats). Exposed for the recording's summary
// in routes/record.go.
func (m *SyncMockManager) MocksLeftOut() int64 {
	if m == nil {
		return 0
	}
	return m.leftOut.Load()
}

// WasPressureActiveInWindow returns (true, overlapCount) if memory pressure
// was active at any moment during [start, end].
//
// Called by the TC-send path in routes/record.go right before forwarding a
// TC to the CLI: if it returns true, the TC is suppressed and never reaches
// disk, so replay cannot encounter a missing-mock EOF for it. record.go
// queries WasMockOrphanedInWindow alongside this so a TC stranded by EITHER
// cause is suppressed; the two are kept as separate methods so each scans only
// its own slice and diagnostics attribute a suppression to its real cause.
//
// Why this is race-free unlike a per-mock-drop ledger:
//   - memoryguard calls SetMemoryPressure(true) and the span is opened under
//     mu in the SAME critical section that flips m.memoryPause = true.
//   - Any mock-parser goroutine that subsequently sees memoryPause==true
//     (and therefore drops its mock) does so BECAUSE the range was already
//     committed. The "open" event happens-before any drop it causes.
//   - The TC's HTTP window [HTTPReq.Timestamp, HTTPResp.Timestamp] is
//     bounded by wall-clock time; if any pressure range overlaps that
//     window, the TC was at risk of losing a mock to pressure regardless
//     of when AddMock actually fires for that mock.
//
// Two intervals [a, b] and [c, d] overlap iff a <= d AND c <= b. An open
// (still-active) span's end is treated as time.Now(). Spans that were joined
// count as one.
func (m *SyncMockManager) WasPressureActiveInWindow(start, end time.Time) (bool, int) {
	if m == nil {
		return false, 0
	}
	// A zero start or end would either match every span or none depending on
	// direction: Spans.Overlaps refuses to make a claim on it, and the caller
	// falls back to "send the TC" rather than over-suppress.
	return m.pressure.Overlaps(start, end)
}

// WasMockOrphanedInWindow returns (true, overlapCount) if any orphan span
// (see RecordOrphanWindow and OpenOrphanWindow) overlaps [start, end]. It is
// the orphan-window twin of WasPressureActiveInWindow: routes/record.go
// queries BOTH before forwarding a TC and suppresses it if EITHER overlaps, so
// a TC stranded by an exchange its connection did not record as a mock — a
// delivered-but-unframable op, a mock the incomplete flag left out, a
// connection that can no longer be recorded — is never shipped mock-less
// (replay would report match_phase=no_mocks).
//
// Kept as a SEPARATE method rather than folded into WasPressureActiveInWindow
// so each scans only its own slice, the two suppression causes stay distinct in
// diagnostics, and the enterprise mongo parser's orphanWindowChecker probe
// (WasMockOrphanedInWindow) resolves against it. Same degenerate-input guard
// as its twin (Spans.Overlaps).
func (m *SyncMockManager) WasMockOrphanedInWindow(start, end time.Time) (bool, int) {
	if m == nil {
		return false, 0
	}
	// Still-open spans extend to now, matching WasPressureActiveInWindow's
	// treatment of an open pressure interval. Without that a connection that
	// is un-capturable RIGHT NOW would suppress nothing, which is precisely the
	// window whose test cases must not be shipped mock-less.
	return m.orphans.Overlaps(start, end)
}

// OpenOrphanWindow marks the start of a hole whose end is not yet known and
// returns the closer that ends it. The returned func is idempotent and safe to
// call from any goroutine; not calling it leaves the hole open, which is the
// correct reading for a connection that stays un-capturable until shutdown.
//
// Use this when capture for a connection has failed in a way it cannot recover
// from — the parser has been retired and the relay is raw-forwarding — as
// opposed to [RecordOrphanWindow], which suits a parser that has already
// resynced and therefore knows the hole's width.
//
// A zero start is ignored and yields a no-op closer, mirroring
// RecordOrphanWindow's refusal to make a claim on a degenerate input.
func (m *SyncMockManager) OpenOrphanWindow(start time.Time) func() {
	if m == nil {
		return func() {}
	}
	return m.orphans.Open(start)
}

// RecordOrphanWindow records a [start,end] interval over which a connection
// carried an exchange that was not recorded as a mock: a hole a parser reports
// once it knows its width (a mongo/v2 reassembly resync hole), or a mock the
// session's incomplete-mock flag left out, whatever set the flag, memory
// pressure included, or that its parser reported it could not record, the
// exchange it stopped on included (supervisor.Session.RecordOrphanWindow
// lists them). It feeds
// WasMockOrphanedInWindow so record.go suppresses every TC whose window
// overlaps the span — the same coverage-for-stability tradeoff the
// memory-pressure suppressor makes — instead of shipping that TC mock-less
// (replay would then report match_phase=no_mocks). Only a TC checked after
// the span is recorded: one record.go streamed before is saved without the
// mock (see orphans). The keploy/integrations mongo/v2 parser reaches it
// through supervisor.Session.RecordOrphanWindow, which it calls directly:
// that method's name and signature are an API across the two repositories
// (Session.RecordOrphanWindow lists the session methods the integrations
// parsers call). A zero start is dropped (no wire ts to attribute); an end
// before start is clamped.
func (m *SyncMockManager) RecordOrphanWindow(start, end time.Time) {
	if m == nil {
		return
	}
	m.orphans.Record(start, end)
}

// CheckedBefore says that every test case of this recording still to be
// checked against its pressure and orphan spans starts at or after t
// (Spans.CheckedBefore): routes/record.go's hold holds none that starts
// earlier.
func (m *SyncMockManager) CheckedBefore(t time.Time) {
	if m == nil {
		return
	}
	m.pressure.CheckedBefore(t)
	m.orphans.CheckedBefore(t)
}

// pressureActiveAtLocked reports whether instant t fell inside any recorded
// pressure span. Caller holds m.mu (the AddMock / SetMemoryPressure paths); the
// spans have their own lock. A still-open span extends to now.
func (m *SyncMockManager) pressureActiveAtLocked(t time.Time) bool {
	ok, _ := m.pressure.Overlaps(t, t)
	return ok
}

// OrphanRangeCount returns how many orphan intervals have been recorded, and
// how many spans hold them now, split into closed and still-open. Exposed for
// the session-summary log in routes/record.go so a run with a high
// tcs_suppressed_total says WHICH suppressor fired: the spans over which the
// memory guard paused recording (PressureRangeCount), or the spans over which a
// connection lost exchanges (this). A still-open interval is the one that
// keeps suppressing to the end of the session — worth surfacing separately.
//
// The two are not exclusive causes. An orphan span is an exchange a
// connection lost, whatever lost it, and a pause loses some too: a chunk the
// relay's tee gates voids the next mock its parser emits
// (Session.LeaveOutIfIncomplete reports it here), and desyncs the
// connection's capture (unrecorded.go). So orphan spans with no pressure spans
// are loss pressure did not cause; beside pressure spans, part of them may be
// pressure's, and the WARN that reports a mock left out for the incomplete
// flag says why the flag was set.
//
// recorded is how often the suppressor fired. closed and open are after
// joining: intervals that overlap or touch are one span, and past the cap
// spans are joined (Spans), so 25,000 back-to-back intervals can be one span.
func (m *SyncMockManager) OrphanRangeCount() (recorded, closed, open int) {
	if m == nil {
		return 0, 0, 0
	}
	// A span OpenOrphanWindow returned is closed once its closer ran, so it
	// counts as closed. Reporting still-open for it would tell an operator whose
	// windows all closed cleanly that suppression ran to end of session: the
	// exact wrong diagnosis, in the field added to prevent one.
	closed, open = m.orphans.Counts()
	return m.orphans.Recorded(), closed, open
}

// PressureRangeCount returns how many memory-pressure intervals have been
// recorded, and how many spans hold them now (after joining, see
// OrphanRangeCount). Exposed for the session-summary log in routes/record.go.
func (m *SyncMockManager) PressureRangeCount() (recorded, spans int) {
	if m == nil {
		return 0, 0
	}
	closed, open := m.pressure.Counts()
	return m.pressure.Recorded(), closed + open
}

func (m *SyncMockManager) SetMemoryPressure(enabled bool) {
	if m == nil {
		return
	}

	// time.Now() OUTSIDE the lock: cheap, and avoids holding mu across the
	// syscall. The span a transition to pressure opens starts here; its closer
	// ends it no earlier (Spans).
	now := time.Now()

	m.mu.Lock()
	wasEnabled := m.memoryPause
	m.memoryPause = enabled

	var clearedFromBuffer, heldDropped int
	var pressureGaveUp []*Window
	if enabled {
		if !wasEnabled {
			// false→true transition: open a new pressure span.
			// memoryguard fires SetMemoryPressure(true) once per 500ms tick,
			// but only the first call (when wasEnabled is false) is a real
			// transition; subsequent ticks while pressure is held are no-ops
			// for span tracking — they would otherwise spam the spans.
			m.closePressure = m.pressure.Open(now)
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
		// Let go of the hold too, whatever its size and the room the budget
		// has left: the agent is short of memory. The requests that may own
		// what it drops are given up, so none is recorded without its mocks
		// (giveUpForPressureLocked).
		heldDropped, pressureGaveUp = m.giveUpForPressureLocked()
		m.publishHoldLocked()
		clearedFromBuffer += heldDropped
	} else if wasEnabled {
		// true→false transition: close the open span. The nil check covers
		// the degenerate case where SetMemoryPressure(false) is somehow called
		// without a prior (true), e.g. a partial state restore in tests.
		if m.closePressure != nil {
			m.closePressure()
			m.closePressure = nil
		}
	}
	m.mu.Unlock() // NEVER hold mu while logging — logging inside a lock causes a deadlock under I/O pressure (see BUG 5: 70-minute CI hang)
	m.reportGivenUp(pressureGaveUp, LostToMemoryPressure)
	if heldDropped > 0 {
		// On every tick that drops some, not only the transition below: the
		// hold keeps filling while pressure lasts.
		m.dropLogger().Debug("diag/syncMock: memory pressure let go of the mocks held for requests in flight",
			zap.Int("dropped_given_up", heldDropped),
			zap.Int("windows_given_up", len(pressureGaveUp)))
	}

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

// ResolveRange resolves the test window [start, end]: kept (keep), its mocks
// are handed on as testName's; a static-dedup duplicate's (keep false) are
// pruned, bar what a request still in flight may own. For a kept request with
// an open window, use ResolveKept, which also ends its window.
func (m *SyncMockManager) ResolveRange(start, end time.Time, testName string, keep bool, mapping bool) {
	m.resolveRange(start, end, testName, keep, mapping, nil)
}

// ResolveKept is the kept ResolveRange of a request with an open window w
// (OpenWindow, claimed by Keep or Claim): in the same critical section that
// takes what was held for the request, it ends w, so the window never holds
// while the resolve hands its mocks on (a send can wait on a full output
// channel) and the hold's bound never waits on it. Its late mocks are claimed
// by the resolved window, as any kept window's are. A nil w, or one of another
// manager, is just ResolveRange's; the caller still owes Close.
func (m *SyncMockManager) ResolveKept(w *Window, start, end time.Time, testName string, mapping bool) {
	m.resolveRange(start, end, testName, true, mapping, w)
}

func (m *SyncMockManager) resolveRange(start, end time.Time, testName string, keep bool, mapping bool, w *Window) {
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
	var tally dropTally

	m.mu.Lock()
	// Snapshot the outChan wiring status under outChanMu (NOT m.mu)
	// so we don't race SetOutputChannel / CloseOutChan. Only the
	// bound boolean is needed — the actual send later goes through
	// sendToOutChan which reacquires the RLock and will skip the
	// send itself when outChanClosed is true.
	outChanBound, _ := m.outChanStatus()
	mappingChan := m.mappingChan
	// Requests still in flight own what was requested since the earliest of
	// them started: a mock this call would drop is held for them instead.
	openFrom, open := m.openFromLocked()
	// A kept window whose request yielded (Window.Yield: the synchronous
	// ingress gave its lock back, and the next request ran beside the rest of
	// it) takes what was requested after its yield only where no other request
	// claims it: a mock another request claims is that request's, and goes
	// the way of a mock outside this window (it is late-binned to that request
	// once resolved, or waits, or is held, for it).
	var sharedFrom time.Time
	var others claims
	if keep && w != nil && w.m == m && w.yieldAt != 0 && w.yieldAt < end.UnixNano() {
		sharedFrom = time.Unix(0, w.yieldAt)
		others = m.claimsLocked(w, sharedFrom, end)
	}
	// self is this window as it is recorded once resolved, for what it leaves
	// to a request in flight: owed back to it if that request does not take
	// it (heldMock.owner). Made on first use.
	var selfW *resolvedWindow
	self := func() *resolvedWindow {
		if selfW == nil {
			selfW = &resolvedWindow{start: start, end: end, testName: testName, mapping: mapping, keep: true, sharedFrom: unixNanoOrZero(sharedFrom)}
		}
		return selfW
	}
	var refs ownerRefs

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
	//      otherwise (stale and unrecoverable) — unless a request still
	//      in flight may own one (OpenWindow): that one is held for it.
	//      A request's own egress call is made long before its response
	//      is complete, so a slow request's early mocks are older than
	//      the cutoff while its window is still open; dropping them
	//      recorded it without them, silently.
	//   3. Session- and connection-scoped mocks are flushed to outChan
	//      (when bound) regardless of [start,end]; their Lifetime makes
	//      them reusable across every test window and they intentionally
	//      never window-match. Retained in the buffer when outChan is
	//      unbound so a later ResolveRange with the channel wired can
	//      drain them. AddMock now forwards these directly when outChan
	//      is bound at ingest time, so this branch only fires for mocks
	//      that landed in the buffer during the brief unbound startup
	//      window before SetOutputChannel.
	now := time.Now()
	cutoffTime := now.Add(-StaleHorizon)

	// The recentWindows ring is intentionally NOT age-pruned. The retro-bin below
	// deliberately rescues an in-window mock whose owning window is FAR older than
	// the 7 s stale-cutoff: a long-running test (e.g. the mongo fuzzer's ~56 s
	// /run) emits per-test mocks across its whole window, and a large async Mongo
	// response can finish decoding tens of seconds after that window closed —
	// time-pruning would strand those mocks (see
	// TestResolveRangeRecordsLateMockInOldWindowButDropsOrphan). Retention is
	// therefore bounded by COUNT (maxRecentWindows) only, mirroring
	// maxPressureRanges, which guards the very same consumer-lag class of bug. The
	// eviction happens at the append-and-cap below; keeping the count high enough
	// (8192, was 256) is what stops a burst from evicting a window while its test's
	// late aggregate mock is still decoding — the go-memory-load-mongo no_mocks bug.
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
		// the stale-buffer cutoff. A mock another request claims after this
		// one yielded is not in this window (see sharedFrom).
		inWindow := !mockTime.Before(start) && !mockTime.After(end)
		claimedAway := false // left to a request that claims it (see sharedFrom)
		if inWindow && len(others) > 0 && mockTime.After(sharedFrom) && others.covers(mockTime) {
			inWindow, claimedAway = false, true
		}
		if inWindow {
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
			} else if ownerW, ok, shared, behind := m.ownerOrSharedLocked(mockTime); ok && ownerW.keep {
				// This duplicate's window overlaps an earlier KEPT window that
				// also contains the mock (concurrent sync-mode requests): the
				// kept test owns it, exactly as the retroactive bin below
				// decides for a mock outside the current window.
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
				mocksToSend = append(mocksToSend, ownedMock{mock: mock, owner: ownerW.testName})
				lateBinned++
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
			} else {
				// A duplicate's debris — unless a concurrent request still in
				// flight may own it (a duplicate's window says nothing about
				// whose it is): then it is held for that request.
				m.holdOrDropLocked(mock, droppedDuplicateLeftover, openFrom, open, &tally, refs.of(shared, behind))
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
		//
		// The verdict is the OWNER's (ownerW.keep), not this call's keep. With
		// static dedup most resolves are duplicates, and deciding by the finder
		// dropped a kept test's late mock whenever a duplicate's resolve found
		// it — while persisting a duplicate's late mock whenever a kept resolve
		// did. The in-time path's startup rescue applies here too, so a
		// startup-window mock of a duplicate is not lost for decoding late.
		ownerW, ownerOK, shared, behind := m.ownerOrSharedLocked(mockTime)
		if !ownerOK && claimedAway {
			// A request in flight claims it: held for that request, owed
			// back to the window decided first over it (this one, unless an
			// earlier one's part after its yield covers it too) if that
			// request does not take it. What this resolve leaves shares one
			// record of it.
			owner := self()
			if behind {
				owner = refs.of(shared, true)
			}
			m.holdForClaimLocked(mock, &tally, owner)
			continue
		}
		if ownerOK {
			if !ownerW.keep {
				if isStartupMock(mock) {
					if !outChanBound {
						m.buffer[keepIdx] = mock
						keepIdx++
						continue
					}
					mock.Name = "mock-" + generateRandomString(8)
					mocksToSend = append(mocksToSend, ownedMock{mock: mock})
				} else {
					m.holdOrDropLocked(mock, droppedDuplicateLeftover, openFrom, open, &tally, refs.of(shared, behind))
				}
				continue
			}
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
			// Handled (flushed); drop from the current buffer.
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

		// BEHIND A CLAIM: in a resolved kept window's part after its yield,
		// decoded after that window was decided, and a request in flight
		// claims it: held for that request now, young or old, with the window
		// it goes back to if that request ends without it (as this resolve
		// does what it leaves, above), not kept for the next pass to look
		// that window up in the ring again.
		if behind {
			m.holdForClaimLocked(mock, &tally, refs.of(shared, true))
			continue
		}

		// SAFETY VALVE: Expire stale OUT-OF-WINDOW mocks.
		// A mock that didn't match the current window AND is older
		// than 7 s is unrecoverable once no window can own it: no
		// resolved window does (the retroactive bin above), and no
		// request still in flight may (OpenWindow) — such a request
		// started before the mock and is decided later, so the mock is
		// held for it instead. What is left is drop-to-bound-growth
		// cleanup: a duplicate's leftovers past its prune, background
		// calls no request made. (Session- and connection-tier mocks
		// are handled in the lifetime carve-out at the top of the loop
		// and never reach this branch.)
		if mockTime.Before(cutoffTime) {
			if m.holdOrDropLocked(mock, droppedOutsideAnyWindow, openFrom, open, &tally, nil) {
				continue
			}
			// Per-mock diagnostic: a per-test mock that fell off the
			// stale-buffer cutoff almost always means the recorder
			// kept emitting after the dedup queue had advanced past
			// the matching test's window — log enough context for
			// post-hoc CI analysis. Sampled via dropLogger to honour
			// the same flood-prevention as the outChan-overflow path.
			if logger := m.dropLogger(); logger != nil {
				logger.Debug("diag/ResolveRange: stale-cutoff drop (per-test mock older than 7s that no resolved or open window owns)",
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

	// A kept window takes what was held inside it for the requests in flight,
	// this one's own early mocks first among them. A duplicate's window takes
	// nothing: every held mock is still one an open window may own (ending a
	// window drops the rest), and that request's verdict is the one to wait
	// for. With the output not wired yet, a kept window's held mocks go back to
	// the buffer with its other in-window mocks, which wait there for it as
	// a resolved kept window's (ownerWindowLocked).
	heldTaken := 0
	if keep {
		taken := m.takeHeldLocked(start, end, sharedFrom, others, self)
		if outChanBound {
			fromHold := make([]ownedMock, 0, len(taken))
			for _, mock := range taken {
				mock.Name = "mock-" + generateRandomString(8)
				associatedMockIDs = append(associatedMockIDs, mock.Name)
				fromHold = append(fromHold, ownedMock{mock: mock, owner: testName})
			}
			heldTaken = len(fromHold)
			mocksToSend = mergeByRequestTime(mocksToSend, fromHold)
		} else {
			m.buffer = append(m.buffer, taken...)
		}
		// The request is decided and has what it owns: its window ends here,
		// not after the sends below.
		if w != nil && w.m == m {
			m.endWindowLocked(w, &tally, false)
		}
	}
	gaveUp := m.fitHoldLocked(&tally)
	lateBinned += m.handOwedLocked(outChanBound, &mocksToSend, &lateMappings)
	hold := m.holdStateLocked()

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
	// window is harmless and aged out by the count cap below.
	m.recentWindows = append(m.recentWindows, resolvedWindow{
		start:      start,
		end:        end,
		testName:   testName,
		mapping:    mapping,
		keep:       keep,
		sharedFrom: unixNanoOrZero(sharedFrom),
	})

	if len(m.recentWindows) > maxRecentWindows {
		// Drop the oldest entries; copy down so the big backing array
		// isn't retained by the reslice.
		n := copy(m.recentWindows, m.recentWindows[len(m.recentWindows)-maxRecentWindows:])
		m.recentWindows = m.recentWindows[:n]
	}

	bufferLenAfter := len(m.buffer)
	mocksToSendLen := len(mocksToSend)
	m.noteTaken(mocksToSend)

	m.mu.Unlock()
	m.reportGivenUp(gaveUp, LostToHoldBound)
	m.settlePool()

	// Per-resolve diagnostic: surface buffer-state transitions per
	// test-window resolve so a CI log can show when a per-test cohort
	// flushed zero mocks or when stale-buffer cutoff started reaping.
	// Sampled via dropLogger which is the standard observability sink
	// for buffer-flow events on this manager. Only logged when there
	// was actual state change to avoid log noise on idle resolves.
	// Every count is THIS call's. dropped splits by reason, and both reasons
	// are cleanup: duplicate leftovers, and mocks outside any window, which no
	// resolved window owns and no request still in flight may. Loss is not
	// counted here: windows_given_up are requests in flight the hold had to
	// give up, and one costs its test case only if it is then kept — Window.Keep
	// warns of that ("left a kept request out") and counts it.
	if ce := m.dropLogger().Check(zap.DebugLevel, "diag/ResolveRange: buffer transition"); ce != nil && (bufferLenBefore != bufferLenAfter || mocksToSendLen > 0 || tally.changed()) {
		fields := []zap.Field{
			zap.String("test_name", testName),
			zap.Time("window_start", start),
			zap.Time("window_end", end),
			zap.Int("buffer_len_before", bufferLenBefore),
			zap.Int("buffer_len_after", bufferLenAfter),
			zap.Int("mocks_flushed", mocksToSendLen),
			zap.Int("late_binned", lateBinned),
			zap.Int("held_taken", heldTaken),
		}
		fields = append(fields, tally.fields()...)
		fields = append(fields, hold.fields()...)
		fields = append(fields,
			zap.Bool("outChan_bound", outChanBound),
			zap.Bool("mapping_enabled", mapping),
		)
		ce.Write(fields...)
	}

	// Route mock sends through sendToOutChanOwned so the close-vs-send
	// race is serialized the same way AddMock does it, and a capacity
	// drop is attributed to the owning TC. Mapping channel is never
	// closed by the shutdown path today — if that ever changes, lift the
	// mapping send under an equivalent guard.
	m.sendTaken(mocksToSend)
	if mappingEntry != nil && mappingChan != nil {
		m.sendMapping(mappingChan, *mappingEntry)
	}
	// Retroactive mapping entries for mocks late-binned into past windows. These
	// are a DELTA for an already-resolved test, not its full set, so the recorder
	// MUST union them into that test's existing mapping. It used to replace, which
	// silently deleted the mocks the original resolution recorded — see
	// mergeMockEntries in the mapping store.
	if mappingChan != nil {
		for tn, ids := range lateMappings {
			if len(ids) == 0 {
				continue
			}
			m.sendMapping(mappingChan, models.TestMockMapping{TestName: tn, MockIDs: ids})
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
// falls inside a recently-resolved KEPT window. (In async mode, the only
// caller, duplicates resolve through here rather than ResolveRange, so every
// window in recentWindows is kept; a sync-mode ResolveRange(keep=false) window
// is recorded with keep=false and never claims a mock here.) So a
// before-the-horizon mock that owns a recent kept window is a late kept mock
// → flush it (rescue); one that owns none is the skipped duplicate's own
// debris → drop it, unless a request still in flight may own it
// (OpenWindow): that one is held for it, however long the request takes.
// Session/connection mocks are reusable across tests and are never reaped by
// a per-test cleanup.
func (m *SyncMockManager) DeleteMocksStrictlyBefore(timestamp time.Time) {
	if m == nil {
		return
	}

	var mocksToSend []ownedMock
	var lateMappings map[string][]string
	var tally dropTally

	m.mu.Lock()
	outChanBound, _ := m.outChanStatus()
	mappingChan := m.mappingChan
	openFrom, open := m.openFromLocked()
	bufferLenBefore := len(m.buffer)
	var refs ownerRefs

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
		// it to that test instead of deleting it as duplicate debris. One
		// owned by a duplicate window falls through to the startup rescue
		// and is otherwise dropped, as the duplicate's own debris is.
		w, ok, shared, behind := m.ownerOrSharedLocked(mock.Spec.ReqTimestampMock)
		if ok && w.keep {
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
		// duplicate's own debris, unless a request still in flight started
		// before it: then it may be that request's, and is held for it.
		// Drop it (fall through without keeping), or hold it.
		m.holdOrDropLocked(mock, droppedDuplicateLeftover, openFrom, open, &tally, refs.of(shared, behind))
	}

	// Memory Cleanup: Nil out the deleted entries to allow GC to reclaim the memory
	for i := keepIdx; i < len(m.buffer); i++ {
		m.buffer[i] = nil
	}
	// Reslice the buffer
	m.buffer = m.buffer[:keepIdx]
	gaveUp := m.fitHoldLocked(&tally)
	m.handOwedLocked(outChanBound, &mocksToSend, &lateMappings)
	bufferLenAfter, hold := len(m.buffer), m.holdStateLocked()
	m.noteTaken(mocksToSend)
	m.mu.Unlock()
	m.reportGivenUp(gaveUp, LostToHoldBound)
	m.settlePool()

	// What this prune did, by reason (see ResolveRange's buffer transition):
	// its drops were silent before, and a loss scan could not tell a
	// duplicate's debris from a mock of a request still in flight.
	if tally.changed() {
		if ce := m.dropLogger().Check(zap.DebugLevel, "diag/DeleteMocksStrictlyBefore: buffer transition"); ce != nil {
			fields := append([]zap.Field{
				zap.Time("horizon", timestamp),
				zap.Int("buffer_len_before", bufferLenBefore),
				zap.Int("buffer_len_after", bufferLenAfter),
				zap.Int("mocks_flushed", len(mocksToSend)),
			}, tally.fields()...)
			ce.Write(append(fields, hold.fields()...)...)
		}
	}

	// Send AFTER releasing m.mu — sendToOutChan takes outChanMu and may
	// block up to sendBudget; holding m.mu across it would wedge AddMock.
	m.sendTaken(mocksToSend)
	if mappingChan != nil {
		for tn, ids := range lateMappings {
			if len(ids) == 0 {
				continue
			}
			m.sendMapping(mappingChan, models.TestMockMapping{TestName: tn, MockIDs: ids})
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
