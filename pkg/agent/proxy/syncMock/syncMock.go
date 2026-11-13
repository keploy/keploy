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

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
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
	// recording is the manager's count of ended recordings as it was
	// resolved (SyncMockManager.recording): a mock or a mapping that goes
	// back to it once its recording has ended is let go.
	recording uint32
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
	// mappingsOwed is what a stop waits for of the mapping stream
	// (mappings_owed.go): its own lock, a leaf.
	mappingsOwed mappingsOwed

	// mappingOverflow holds mappings the recorder was not ready to take. The
	// capture path must never block, but a mapping must never be dropped either
	// (a lost one becomes a no_mocks failure at replay), so overflow is queued
	// here and handed over by a single drainer goroutine. Guarded by
	// mappingOverflowMu: never take m.mu while holding it (only
	// mappingsOwed.mu nests inside it).
	mappingOverflowMu sync.Mutex
	mappingOverflow   []owedMapping
	mappingDraining   bool
	mappingStreamCtx  context.Context
	// mappingStreamCh is the stream mappingStreamCtx is for: a mapping read
	// for another (replaced) one is not queued behind it (sendMapping).
	mappingStreamCh chan<- models.TestMockMapping
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
	// firstReqSeen's lifecycle (set forward-only within a record session,
	// and cleared as the next one starts: StartRecordSession). A manager
	// whose mocks go to several recordings counts them per recording too
	// (destTCs).
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

	// sentClaimed is the startup mocks the rescues sent with no owner while a
	// request still in flight may take them (startupClaimedLocked), in request
	// order: the resolve of the kept request that takes one adds it to its
	// test's mapping (takeSentClaimedLocked), as it would have, had the mock
	// waited for it, and one its claimer ends without goes back to the
	// resolved kept window that left it (sentClaim.owner), as a held mock is
	// owed back. A drop of one by its send is told to the test that takes it
	// (sentClaimDone). They are not kept back: what is in the buffer at a stop
	// is not counted as pending (BacklogBefore), and a mock kept there was lost
	// when the stream closed first. Pruned once no request in flight can take
	// one (pruneSentClaimedLocked); at most maxSentClaimed.
	sentClaimed []sentClaim
	// sentClaimedEvicted counts the notes forgotten at the bound
	// (evictSentClaimedLocked).
	sentClaimedEvicted atomic.Uint64
	// evicted is what evictSentClaimedLocked forgot that settleLetGo has not
	// said yet: it counts under mu, and the line is written once mu is let
	// go. Guarded by mu.
	evicted evictedNotes

	// open is the windows of requests read but not decided yet (OpenWindow),
	// a min-heap on start: the earliest, the one every reaper asks for, is
	// open[0]. A per-test mock requested at or after its start may be one of
	// theirs, so no reaper drops it (see held). Guarded by mu.
	open windowHeap
	// lastWindowEnd is when the latest window ended, givenUpInFlight how
	// many windows were given up while their requests are still in flight
	// (noteGivenUpInFlightLocked), and windowlessIngress that this agent's
	// ingress also takes requests that open no window
	// (stampOutsideRequestsLocked). Guarded by mu.
	lastWindowEnd     time.Time
	givenUpInFlight   int
	windowlessIngress bool
	// stopFound is the windows of the requests in flight that began by the
	// instant a recording stops at (NoteStop), less those that ended since:
	// the requests its stop found in flight. They stay the stopping
	// recording's, which may still take one whose response comes as it
	// drains, until it has ended (EndStoppedRecording), which gives up those
	// still open (LostToStop). Guarded by mu.
	stopFound map[*Window]struct{}
	// stopAt is the latest instant a stop not yet ended was noted at
	// (NoteStop), zero when none is; endedStopAt is the last ended stop's
	// (EndStoppedRecording). recording counts the recordings that have
	// ended: each resolved window keeps the count it was resolved at
	// (resolvedWindow.recording), and what goes back to one of an ended
	// recording's is let go. Guarded by mu.
	stopAt, endedStopAt time.Time
	recording           uint32
	// endedLaneEpoch and endedMappingGen are the output lane's epoch
	// (revokeEpoch) and the mapping stream's generation (mappingGen) as the
	// last recording ended: while both are still the ones bound, that
	// recording's streams are, and what goes back to its test cases goes to
	// them (goneLocked). Guarded by mu.
	endedLaneEpoch  uint64
	endedMappingGen uint64
	// leftovers is the request times, in Unix nanoseconds, of the requests
	// the stops of recordings that ended found in flight, until the next
	// incoming stream takes them (TakeLeftovers). Guarded by mu.
	leftovers map[int64]struct{}
	// testSpans is, under the proven switch, the spans of the last
	// maxRecentWindows windows whose request became a test case, as its test
	// case states them (noteTestSpanLocked); testSpansNext is the one the next
	// replaces once it is full. spanWatch is the channels TestSpanAt handed
	// out for open windows, each closed and dropped as its window leaves the
	// open ones (windowLeftLocked). Guarded by mu.
	testSpans     []testSpan
	testSpansNext int
	spanWatch     map[*Window]chan struct{}
	// scopes is, under the proven switch, the test scopes open now
	// (BeginTestScope): tests whose recording opens no window. Guarded by mu.
	scopes []*TestScope
	// exchangeHolds is the exchanges parsers hold while they cannot yet tell
	// what they are (HoldExchanges), each by its earliest request time in
	// Unix nanoseconds; exchangeHoldsNext numbers them. holdsStopping is
	// closed (holdsStopped) as a recording's stop first asks what is pending
	// (BacklogBefore), and a new one made for the next recording
	// (SetOutputChannel). Guarded by holdsMu, a leaf.
	holdsMu           sync.Mutex
	exchangeHolds     map[uint64]int64
	exchangeHoldsNext uint64
	holdsStopping     chan struct{}
	holdsStopped      bool
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
	// heldCut is the mocks held at the stale cutoff (droppedOutsideAnyWindow)
	// that the hold let go of with no window taking them (dropHeldLocked),
	// each with the recording it was let go in: a request in flight with no
	// window open for it (gRPC, the proxyless capture) may still be decided
	// with one inside its window. settleLetGo leaves out its test cases as the
	// cutoff's own drops do (cutDropped), once mu is let go; letGo says some
	// wait. Guarded by mu.
	heldCut []cutHeld
	// letGo is set, under mu, while heldCut or evicted holds something for
	// settleLetGo: one atomic load where nothing does.
	letGo atomic.Bool
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
	// owedLetGo counts the mocks owed back to a test case of a recording that
	// had ended, its streams no longer bound, let go rather than handed to a
	// later recording's stream (handOwedLocked); goneLetGo, its late mocks
	// let go so (goneLocked).
	owedLetGo, goneLetGo atomic.Uint64
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

	// dropCount tracks capacity drops (dropOut): the output closed or never
	// set (send, and AddMock once it is closed), the out queue's byte bound
	// reached, or its consumer stalled (send). Sampled (sayDrop) so customers
	// get a loud signal without the log-flood anti-pattern. Using
	// the typed atomic.Uint64 wrapper removes the 32-bit-alignment
	// footgun that a bare uint64 + sync/atomic.AddUint64 would carry
	// if this struct ever got embedded or reordered.
	dropCount atomic.Uint64
	// droppedTCsRecorded counts the names droppedTCNames took, over the
	// manager's life. The set's size stops growing at its bound, so what a
	// session lost is read from this (DropsSince).
	droppedTCsRecorded atomic.Int64

	// out is the current channel's lane (outLane): the mocks a full outChan
	// has not taken yet, in order, and its drainer. outOld holds the lanes of
	// channels SetOutputChannel replaced while their drainer still ran (it had
	// mocks for them, or was telling the drops of a give-up): each goes on
	// delivering to its own channel, whose consumer (the session it was) may
	// still read, until its queue is empty or that consumer stops taking
	// (outChanStall), and its drops are told. Guarded by outQueueMu, taken
	// (when both are) after outChanMu; droppedMu, sendingMu and the drop
	// hook's locks are never taken under it.
	outQueueMu sync.Mutex
	out        *outLane
	outOld     map[*outLane]struct{}
	// outClock, outStall and outCloseStall measure the drainer's stall
	// (outChanStall, outCloseStall): the process's own runnable clock and
	// those bounds unless a test sets them.
	outClock      stall.Clock
	outStall      time.Duration
	outCloseStall time.Duration
	// handedOn counts the mocks the out path decided: delivered or dropped
	// (HandOnProgress).
	handedOn atomic.Uint64

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
	// Each name is kept with the revokeEpoch of the drop that lost it: a
	// name is lost only for the recording of that channel
	// (WasMockDroppedForTC), so a test case of a later recording that carries
	// the same name, or one a replaced channel's lane drops for, is not left
	// out for it.
	droppedMu      sync.Mutex
	droppedTCNames map[string]uint64
	droppedTCOrder []droppedName

	// revokeCapable gates the deferred-orphan revoke protocol: it is set true
	// (via SetRevokeCapable) only when the CLI negotiated
	// OutgoingOptions.SupportsDroppedRevoke on the /outgoing request. When
	// false (an older CLI, or the default), recordDroppedTC queues NOTHING and
	// drainPendingRevokes sends NOTHING — so a CLI that can't divert the
	// reserved Kind=RevokedTests control frame never receives one. atomic so
	// the send path can read it without taking a lock.
	revokeCapable atomic.Bool
	// revokeByRequest is set (SetRevokeByRequest) when the client also finds
	// a revoked test case sent with no name by its request time
	// (OutgoingOptions.SupportsRevokeByRequest): the stream then notes such
	// a test case sent under the name a revoke gives it (sentName).
	revokeByRequest atomic.Bool

	// pendingRevokes is the FIFO of dropped-TC names still to be emitted to the
	// CLI as RevokedTests control frames. Appended by recordDroppedTC when a
	// NEW capacity-drop owner is recorded AND revokeCapable is set; drained by
	// drainPendingRevokes on every FlushOwnedWindows tick and at CloseOutChan.
	// Guarded by the SAME droppedMu leaf lock as droppedTCNames (it fits the
	// leaf discipline — droppedMu takes no other lock while held), so a drop
	// records the owner and queues the revoke under one lock acquisition.
	pendingRevokes []string

	// revokeEpoch is the current channel's epoch: SetOutputChannel moves it
	// on with the channel (under outChanMu's writer lock and droppedMu, as it
	// clears pendingRevokes), and each lane carries the epoch it was the
	// current channel's in (outLane.epoch). A drop queues its owner's revoke
	// only in the epoch it happened in (recordDroppedTCFor): a replaced lane's
	// drop, or one decided before a switch and recorded after it (a lane's
	// give-up, a claimed startup mock's note), is revoked on no stream, least
	// of all the new session's, whose tests may carry the same names. Stored
	// under droppedMu; read atomically.
	revokeEpoch atomic.Uint64

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
	// totalAdded is the mocks taken (AddMock, SendConfigMock), less those
	// dropped since for pressure or for the output (SessionDrops.Added).
	totalAdded atomic.Int64
	// notRecorded counts the requests an ingress did not record because the
	// memory guard had paused recording (NoteNotRecordedUnderPressure).
	notRecorded notRecordedUnderPressure

	// leftOut counts the mocks a session routed here left out
	// because a parser could not record them (NoteMockLeftOut): their capture
	// was incomplete, they could not be decoded or served, their call did not
	// complete (an HTTP/2 stream reset), or they are the answer to a request
	// sent before the capture of their connection began
	// (supervisor.Session.NextRequest). It is for the recording's summary:
	// their warnings are rate-limited, so this is the one place each is
	// counted.
	leftOut atomic.Int64
	// notHandedOn counts the requests an ingress served and was recording
	// whose test case never reached the recording's queue: the ingress
	// stopped first (NoteNotHandedOn).
	notHandedOn atomic.Uint64
	// unnamedCallerConns counts the connections to a listener of the run's
	// own process tree whose caller the agent could not name in its PID
	// namespace, so that none of the calls they carried was recorded as a
	// test case (NoteUnnamedCallerConn).
	unnamedCallerConns atomic.Uint64
	// tlsRelayedConns counts the connections to the app that opened with a
	// TLS handshake, which the ingress relayed to the app's TLS unread, so
	// that none of the calls they carried was recorded as a test case
	// (NoteTLSRelayedConn).
	tlsRelayedConns atomic.Uint64
	// tlsRelayedSaid holds the app ports a TLS connection was relayed to
	// since the recording session started (StartTLSRelayedSession): the
	// session's first to each is said at WARN as it comes. tlsRelayedMu
	// guards it, and orders each count against a session's start.
	tlsRelayedMu   sync.Mutex
	tlsRelayedSaid map[uint16]struct{}

	// outChanClosedDrops and overflowDrops split dropCount by cause, for what
	// is said of a drop (sayDrop): the mocks dropped for an output closed or
	// never set, and those dropped for a full one. Sampled apart, so a drop at
	// a recording's stop does not take the first ERROR a real overflow is
	// owed, and each from its session's opening (StartDropSession), so every
	// session is told of its first. An unread answer is in dropCount alone:
	// its loss costs no test case, and takes no sample. pressureSampled is
	// the same for memory pressure's drops, which pressureDropped counts over
	// the manager's life.
	outChanClosedDrops atomic.Uint64
	overflowDrops      atomic.Uint64
	pressureSampled    atomic.Uint64

	// dropNames names the first dropNamesMax mocks dropped since the session
	// opened (StartDropSession), for its summary (DropsSince). dropNamesMu is
	// a leaf lock: countDrop takes it under outChanMu.
	dropNamesMu sync.Mutex
	dropNames   []string

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

	// lostOver is the requests, each an instant, of the calls whose mock, no
	// test's own and not one any test may need, was dropped on its way out
	// (leaveOutOver): recorded under sharedLossMu, and checked under it by
	// SendingTestCase, so a test case whose window holds one is left out or
	// revoked, never sent between the two. The
	// session spans a mock dropped as it is added too (orphans), outside that
	// lock, and one given up later on is spanned only here.
	lostOver Spans

	// cutLost is the requests of the per-test mocks the stale cutoff dropped
	// (leaveOutCut), each an instant: recorded and checked under
	// sharedLossMu, as lostOver is, so a test case whose window holds one is
	// left out or revoked, never sent between the two.
	cutLost Spans
	// cutDrops counts the per-test mocks the stale cutoff dropped, and
	// cutLeftOut the test cases a cutLost request left out at their verdict
	// (SendingTestCase), since the session opened (StartDropSession): the
	// lines that say them name the first dropNamesMax and the sampled ones.
	cutDrops, cutLeftOut atomic.Uint64

	// sharedLoss is the span the current recording leaves its test cases out
	// over since it lost a mock that any of them may need (servedToAnyTest)
	// on the way out (leaveOutAfter): open in orphans from that mock's
	// request, the earliest such loss's, until the recording ends
	// (SetOutputChannel). sentTCs is the test cases its stream sent since
	// (SendingTestCase), by name and window, apart per destination
	// (SetDestinationOf): such a loss revokes those whose window reaches past
	// the lost mock's request, sent before it, of every destination. Guarded
	// by sharedLossMu, taken under outChanMu or alone, and under which only
	// orphans' own lock and droppedMu are taken.
	sharedLossMu sync.Mutex
	sharedLoss   sharedLoss
	sentTCs      sentByDest
	// perTestUnnamed counts the recording's losses of a test's mock no test
	// owns that may reach a test case its stream sent and no longer names
	// (leaveOutOver), for sayLeftOutAfter's sample.
	perTestUnnamed uint64
	// perTestKept counts the recording's losses of a test's mock no test owns
	// that revoked test cases its stream sent to a client that cannot delete
	// them (leaveOutOver), and perTestKeptSent those test cases, for
	// sayLeftOutAfter.
	perTestKept, perTestKeptSent uint64
	// startupLost is, per destination (mockDest), the earliest-requested
	// start-up mock (IsStartup) the current recording lost as a test's
	// (leaveOutOver), requested at or after that destination's first test
	// case then; nil when none. A revoke can move the first test case past
	// its request, or leave it unknown: it is then a start-up mock of the
	// recording as it now stands, which a replay serves to any test, and is
	// re-read as one any test may need (rereadStartupLocked). The earliest
	// alone is kept per destination: the span its re-read opens reaches every
	// later one's request.
	startupLost map[string]lostStartup

	// firstSent is, per destination (SetDestinationOf), the earliest window
	// start among the test cases the current recording's stream sent to it
	// that are still in the recording (sentList.first): not revoked, or
	// revoked and kept by a client that cannot delete them. Once that is not
	// known, it is a bound the first is no later than; a destination with
	// none known has no entry, and the map is nil while none has one. A
	// replay serves a mock requested before its destination's first from its
	// start-up tier, to any test (servedToAnyTest). Stored, copied, under
	// sharedLossMu as sent changes, read without it.
	firstSent atomic.Pointer[map[string]time.Time]

	// destOf, when set (SetDestinationOf), names the destination whose
	// recording a mock goes to, and the one the test cases of the requests
	// the manager resolves itself go to: the manager's mocks go to several
	// recordings, each with its own first test case and its own start-up
	// window. Nil for a manager whose mocks go to one recording.
	destOf atomic.Pointer[destinations]

	// destTCs is, per destination the test cases of the requests the manager
	// resolves itself go to (destinations.own, "" for a manager whose mocks
	// go to one recording and when it cannot tell), how many of them it
	// resolved kept (ResolveRange), as resolvedTestCount counts them all. A
	// manager whose mocks go to several recordings tags a mock start-up by
	// its own destination's count (startupLocked). Guarded by mu; cleared as
	// a recording starts (StartRecordSession), and one destination's as its
	// recording is let go (ForgetDestination).
	destTCs map[string]int

	// copies is, per copy key a kind's replay serves its mocks by
	// (ServedByCopy), the earliest request of a mock of it the current
	// recording's consumer took: a mock no test owns whose copy it holds is
	// needed by no test (neededByNone).
	copies heldCopies

	// watermark says when a test case's verdict is final (settle.go); nil
	// when nothing can tell.
	watermark atomic.Pointer[watermarkBox]
	// added is the watermarks AddWatermark added beside it (settle.go):
	// copy-on-write under addedMu.
	added   atomic.Pointer[[]*watermarkBox]
	addedMu sync.Mutex
	// incoming is what the ingress has yet to hand the recorder's stream:
	// test cases being built, and the queues they wait in (settle.go).
	incoming incomingBacklog
	// heldOldest is the earliest end (UnixNano) of a test case the recorder's
	// stream holds for its verdict (NoteHeld); 0 when none.
	heldOldest atomic.Int64
	// onDrop is told of each mock dropped on the way to outChan, but an
	// answer the client never read (OnDrop, toldDropped).
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
// anonymous carve-outs) — those record nothing on a drop. noted says a
// startup mock sent for a claim (noteSentClaimedLocked), whose send's end is
// told to its note (sentClaimDone): the test that takes it is its owner.
type ownedMock struct {
	mock  *models.Mock
	owner string
	noted bool
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
	m.outChanMu.RLock()
	same := out == m.outChan
	m.outChanMu.RUnlock()
	if same {
		// The hot path (once per accepted connection): no writer lock, which
		// would wait out a sender holding the read lock.
		return
	}
	m.outChanMu.Lock()
	if out == m.outChan {
		m.outChanMu.Unlock()
		return
	}
	m.outChan = out
	m.outChanClosed = false
	epoch := m.revokeEpoch.Load() + 1
	// A new channel gets a lane of its own. The old one's, if its drainer
	// still runs (it has mocks for it, or tells the drops of a give-up), goes
	// on delivering them to the old channel: its consumer, the session it
	// was, may still read (its stop drains it), and loses them only if it
	// takes nothing for outChanStall, as any consumer. Its later drops revoke
	// nothing (their epoch is past). A
	// channel set again while its replaced lane still delivers to it gets
	// that lane back: one lane per channel, so what is sent to it now waits
	// behind what was queued for it before, rather than racing it there on a
	// second drainer.
	m.outQueueMu.Lock()
	if old := m.out; old != nil && old.draining {
		if m.outOld == nil {
			m.outOld = make(map[*outLane]struct{})
		}
		m.outOld[old] = struct{}{}
	}
	var lane *outLane
	for l := range m.outOld {
		if l.ch == out {
			lane = l
			delete(m.outOld, l)
			break
		}
	}
	if lane == nil {
		lane = &outLane{ch: out, closing: make(chan struct{})}
	} else if lane.closing == nil {
		lane.closing = make(chan struct{})
	}
	lane.epoch = epoch
	m.out = lane
	m.outQueueMu.Unlock()
	// New session (distinct channel = re-record): drop any revokes still
	// queued from a prior session, so a name orphaned there can't be
	// delivered onto THIS session's /outgoing stream, and move the epoch on
	// with them, so none is queued for it later. Under outChanMu's writer
	// lock: no send of either epoch runs meanwhile. droppedMu is a leaf.
	m.droppedMu.Lock()
	m.pendingRevokes = nil
	m.revokeEpoch.Store(epoch)
	m.droppedMu.Unlock()
	// After the epoch: a drop of the last recording's that read it before
	// has opened its span already, and one that reads it now opens none.
	m.endSharedLoss()
	// The new session's holds are told of its own stop, not the last one's
	// (HoldExchanges): a hold from before keeps the channel it took.
	m.newHoldsSession()
	m.outChanMu.Unlock()
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
	// The stream and its generation change together, under mu: a mapping
	// owed (oweMapping, under mu) is owed to the generation of the stream it
	// is sent on, so the forgiveness below never takes the debt of a mapping
	// sent on the new stream.
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mappingChan = ch

	m.mappingOverflowMu.Lock()
	m.mappingStreamCtx = streamCtx
	m.mappingStreamCh = ch
	// Anything the previous stream never handed over belongs to a recording that
	// has already ended: no stop waits for it any more.
	m.mappingOverflow = nil
	m.mappingGen++
	m.forgiveMappingsBefore(m.mappingGen)
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
//
// paid, when set, is the mapping's debt to a recording's stop (oweMapping):
// paid once the mapping is on ch, or once it is lost (and said to be).
func (m *SyncMockManager) sendMapping(ch chan<- models.TestMockMapping, entry models.TestMockMapping, paid func()) {
	pay := func() {
		if paid != nil {
			paid()
		}
	}
	queued := owedMapping{entry: entry, paid: paid}
	m.mappingOverflowMu.Lock()
	if m.forReplacedStreamLocked(ch) {
		m.mappingOverflowMu.Unlock()
		pay()
		return
	}
	if m.mappingDraining {
		// A drainer is live — it may be mid-send with overflow momentarily empty,
		// so gate on the drainer, not on the queue length, or this would overtake.
		if len(m.mappingOverflow) >= mappingOverflowCap {
			m.mappingOverflowMu.Unlock()
			m.reportMappingOverflowFull()
			pay()
			return
		}
		m.mappingOverflow = append(m.mappingOverflow, queued)
		m.mappingOverflowMu.Unlock()
		return
	}
	m.mappingOverflowMu.Unlock()

	select {
	case ch <- entry:
		pay()
		return
	default:
	}

	m.mappingOverflowMu.Lock()
	if m.forReplacedStreamLocked(ch) {
		m.mappingOverflowMu.Unlock()
		pay()
		return
	}
	if m.mappingDraining {
		// Another caller started a drainer while we were trying the fast path.
		m.mappingOverflow = append(m.mappingOverflow, queued)
		m.mappingOverflowMu.Unlock()
		return
	}
	m.mappingOverflow = append(m.mappingOverflow, queued)
	m.mappingDraining = true
	gen := m.mappingGen
	streamCtx := m.mappingStreamCtx
	m.mappingOverflowMu.Unlock()

	go m.drainMappingOverflow(ch, streamCtx, gen)
}

// forReplacedStreamLocked reports whether ch is a mapping stream since
// replaced (SetMappingChannel came between a sender's read of the stream and
// its send): the mapping went with its recording, and its debt with it
// (forgiveMappingsBefore). Queued, it would hold every mapping of the new
// stream behind a send nobody takes. The caller holds mappingOverflowMu.
func (m *SyncMockManager) forReplacedStreamLocked(ch chan<- models.TestMockMapping) bool {
	return ch != m.mappingStreamCh
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
		queued := m.mappingOverflow[0]
		m.mappingOverflow = m.mappingOverflow[1:]
		m.mappingOverflowMu.Unlock()

		select {
		case ch <- queued.entry:
			if queued.paid != nil {
				queued.paid()
			}
		case <-done:
			// The recorder's stream is gone, so this can never be delivered. That
			// is real data loss — say so loudly; it was silent before.
			m.mappingOverflowMu.Lock()
			if m.mappingGen != gen {
				// A new stream took over while we blocked; its queue is not ours
				// to clear and its drainer slot is not ours to release. The
				// mapping in hand is lost with the old stream, and owes no stop
				// (the new stream forgave the old ones' debts, this one's too).
				m.mappingOverflowMu.Unlock()
				if queued.paid != nil {
					queued.paid()
				}
				return
			}
			lost := len(m.mappingOverflow) + 1
			for _, o := range append(m.mappingOverflow, queued) {
				if o.paid != nil {
					o.paid()
				}
			}
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

// outQueueMaxBytes bounds the mocks queued for full outChans (outQueue), by
// every manager of the process together (processOutQueued): a mock that would
// take them past this is dropped. Bytes decide a loss on the way out, not a
// time: the queue holds as much again as the CLI's outChan itself (its 16,384
// slots of 4 KiB, pkg/service/agent), on top of it. One bound for the process,
// as the hold's (MaxHeldBytes): a DaemonSet agent runs a manager per app, all
// of whose channels one consumer (the k8s-proxy upload) takes, and a bound
// per manager would queue it once per app when that consumer stalls.
const outQueueMaxBytes int64 = 64 << 20

// outQueueSharedRoom is the part of outQueueMaxBytes that only a mock no test
// owns and a replay serves to any test (servedToAnyTest) may fill: a test's
// own mock is dropped once the queue holds outQueueMaxBytes less this, and
// its test left out by name. So a session's or a connection's mock (a
// handshake, a config read) queued behind a backlog of tests' mocks still
// finds room: losing it would cost every test recorded after it, losing one
// of theirs costs one test. The process's bound is the same either way.
const outQueueSharedRoom int64 = 16 << 20

// servedToAnyTest reports whether mock, sent for owner, is one no test owns
// and a replay may serve to any test of its recording: a session's or a
// connection's (attributedReusable), one with no request time, which no
// test's window can be shown to hold, or one of the start-up window's
// (IsStartup) requested before the recording's first test case
// (beforeFirstTestCase). A replay serves those from its start-up tier, the
// mocks requested before the earliest test case it holds, to every test. A
// test that needs it may be any recorded after its request, so its loss
// cannot be left out by name. IsStartup alone does not make one: AddMock
// tags every mock so while no request was signalled, which the async
// capture never signals, and while the first test cases are recorded; such
// a mock no test owns, requested after the first test case sent, is the
// test's whose window it was requested in, until a revoke moves the first
// test case past its request: its loss is then re-read as one any test may
// need (rereadStartupLocked). The first is that of the recording the mock
// goes to (mockDest): one whose recording the manager cannot tell may be
// any destination's, a new one's too, so it counts as before the first. An
// answer the client never read serves no test.
func (m *SyncMockManager) servedToAnyTest(mock *models.Mock, owner string) bool {
	if owner != "" || mock.IsUndelivered() {
		return false
	}
	req := mock.Spec.ReqTimestampMock
	if req.IsZero() || attributedReusable(mock) {
		return true
	}
	if !mock.TestModeInfo.IsStartup {
		return false
	}
	dest, known := m.mockDest(mock)
	return !known || m.beforeFirstTestCase(req, dest)
}

// beforeFirstTestCase reports whether req is before the earliest window
// start among the test cases the recording's stream sent to dest that are
// still in the recording (firstSent), or may be: it knows none, or req is
// before the bound it knows the first by (sentList.first). A window that
// became no test case (an exchange keploy's filters or its sampling left
// out, or a test case left out) does not count: the replay's start-up tier
// reaches the first test case the recording holds, so until one is sent
// every start-up mock may be in it.
func (m *SyncMockManager) beforeFirstTestCase(req time.Time, dest string) bool {
	first := m.firstSentOf(dest)
	return first == nil || req.Before(*first)
}

// firstSentOf returns the first test case sent to dest that it knows
// (firstSent), nil when it knows none.
func (m *SyncMockManager) firstSentOf(dest string) *time.Time {
	firsts := m.firstSent.Load()
	if firsts == nil {
		return nil
	}
	first, ok := (*firsts)[dest]
	if !ok {
		return nil
	}
	return &first
}

// destinations is what a manager whose mocks go to several recordings is
// told of them (SetDestinationOf).
type destinations struct {
	// of names the recording a mock goes to, "" when it cannot tell.
	of func(*models.Mock) string
	// own names the recording the test cases of the requests the manager
	// resolves itself (ResolveRange) go to, "" when it cannot tell; nil
	// names none.
	own func() string
}

// SetDestinationOf tells the manager that its mocks go to several
// recordings, as a DaemonSet node's default manager's go to the app each
// connection belongs to, how to name the one a mock goes to (of), and the
// one the test cases of the requests it resolves itself go to (own): each
// returns "" when it cannot tell, and a nil own names none. The manager
// then knows each destination's first test case apart (SendTestCaseOf
// names a test case's), so a start-up mock lost before its own recording's
// first test case counts as one any test may need, though another
// recording's test cases came first, and one whose recording it cannot tell
// counts so too. It counts the test cases it resolves per destination too
// (destTCs), as each recording has its own start-up window: a mock is
// tagged start-up (AddMock) while its own recording's count is under it,
// and always when the manager cannot tell its recording, or resolves none
// of that recording's requests itself (startupLocked). A manager whose own
// requests and mocks all go to one recording so tags them as a manager
// without SetDestinationOf does, and its reapers (static-dedup pruning, the
// stale cutoff, the memory-pressure wipe) take its mocks as that one's do.
// Set before the recording starts; a nil of goes back to one recording.
func (m *SyncMockManager) SetDestinationOf(of func(*models.Mock) string, own func() string) {
	if of == nil {
		m.destOf.Store(nil)
		return
	}
	m.destOf.Store(&destinations{of: of, own: own})
}

// ForgetDestination lets go what the manager keeps of dest's recording: the
// test cases sent to it (SendTestCaseOf), its first, the start-up mock it
// lost that a revoke may re-read, and the count of its test cases the
// manager resolved (destTCs). Call it once dest's recording is over, when
// no test case will be sent to it again, so a manager whose mocks go to many
// recordings over its life (SetDestinationOf) holds only the live ones'. A
// mock that still names dest finds no first test case, and counts as one any
// test may need (servedToAnyTest), and is tagged start-up again: fail
// closed. Test cases already sent to dest are no longer revoked by a later
// loss: its stream is over.
func (m *SyncMockManager) ForgetDestination(dest string) {
	m.mu.Lock()
	delete(m.destTCs, dest)
	m.mu.Unlock()
	m.sharedLossMu.Lock()
	defer m.sharedLossMu.Unlock()
	delete(m.sentTCs.lists, dest)
	delete(m.startupLost, dest)
	m.noteFirstSentLocked()
}

// mockDest names the destination whose recording mock goes to (destOf), ""
// for a manager whose mocks go to one, with known false when the manager
// cannot tell it.
func (m *SyncMockManager) mockDest(mock *models.Mock) (dest string, known bool) {
	d := m.destOf.Load()
	if d == nil {
		return "", true
	}
	dest = d.of(mock)
	return dest, dest != ""
}

// ownDest names the destination the test cases of the requests the manager
// resolves itself go to (destinations.own): "" for a manager whose mocks go
// to one recording, and when it cannot tell.
func (m *SyncMockManager) ownDest() string {
	d := m.destOf.Load()
	if d == nil || d.own == nil {
		return ""
	}
	return d.own()
}

// startupLocked reports whether AddMock tags a mock that goes to dest
// (mockDest, known false when the manager cannot tell it) a start-up one:
// it is taken before the first request, or while fewer than
// models.StartupMockTestCaseWindow test cases were resolved. A manager whose
// mocks go to several recordings (SetDestinationOf) reads the count of the
// mock's own recording (destTCs): its count and first request are those of
// the requests it resolves itself, which may be another recording's. A mock
// whose recording it cannot tell is tagged: it may be any recording's, one
// whose window is still open too. One of a recording whose requests it
// resolves none of keeps a count of 0, and is tagged so too: its start-up
// window is not known to be past. Under mu.
func (m *SyncMockManager) startupLocked(dest string, known bool) bool {
	if !m.firstReqSeen || m.resolvedTestCount < models.StartupMockTestCaseWindow {
		return true
	}
	if m.destOf.Load() == nil {
		return false
	}
	return !known || m.destTCs[dest] < models.StartupMockTestCaseWindow
}

// destKey is the key the manager keeps a test case sent to dest under: dest
// in a manager whose mocks go to several recordings (SetDestinationOf), ""
// in any other.
func (m *SyncMockManager) destKey(dest string) string {
	if m.destOf.Load() == nil {
		return ""
	}
	return dest
}

// queueBound is the bytes the out queue may hold with mock, sent for owner,
// queued on the channel of epoch: outQueueMaxBytes for a mock any test may
// need, less outQueueSharedRoom for any other, and for one the recording's
// consumer took a copy of already (neededByNone): no test needs it.
func (m *SyncMockManager) queueBound(mock *models.Mock, owner string, epoch uint64) int64 {
	if m.servedToAnyTest(mock, owner) && !m.neededByNone(mock, epoch) {
		return outQueueMaxBytes
	}
	return outQueueMaxBytes - outQueueSharedRoom
}

// sharedLoss is a recording's span of left-out test cases since it lost a
// mock any of them may need (leaveOutAfter): from that mock's request until
// close ends it.
type sharedLoss struct {
	from  time.Time
	close func()
}

// beforeAnyRequest is where the loss of a mock any test may need with no
// request time leaves test cases out from (leaveOutAfter): before every
// window, so every test case of the recording is left out, those sent
// already revoked. Not the zero time, which a span ignores (Spans.Open).
var beforeAnyRequest = time.Unix(0, 1)

// leftOut is what a loss of a mock any test may need left out
// (leaveOutAfter), for sayLeftOutAfter; the zero value when it left out
// none. from is the span's start (the lost mock's request), revoked the
// test cases the stream had sent before the loss whose window reaches past
// from, revoked by name, and unnamedThrough, when not zero, the latest
// window end of one it let go (sentList) that reaches past from: those it
// cannot name, so they may stay recorded without the mock.
type leftOut struct {
	from           time.Time
	revoked        int
	unnamedThrough time.Time
	// perTest is the place of a test's mock no test owns (leaveOutOver)
	// among the recording's such losses that may reach a test case it cannot
	// name (unnamedThrough), 0 for a mock any test may need: said for the
	// first and the sampled ones.
	perTest uint64
	// kept is the place of a test's mock no test owns (leaveOutOver) among
	// the recording's such losses that revoked test cases its stream sent
	// (revoked) to a client that cannot delete them, and keptSent how many
	// such test cases the recording kept so: said for the first and the
	// sampled ones, the rest at Debug.
	kept, keptSent uint64
	// reread is a start-up mock lost earlier as a test's, re-read as one any
	// test may need (rereadStartupLocked) as this loss or revoke moved the
	// recording's first test case past its request, and what that left out:
	// said at ERROR, naming it. Nil when none was.
	reread *rereadLoss
}

// lostStartup is a start-up mock (IsStartup) the current recording lost as a
// test's (leaveOutOver): its request, and what a line says of it
// (lostFields), not the mock, whose body need not stay in memory.
type lostStartup struct {
	req    time.Time
	fields []zap.Field
}

// rereadLoss is a lostStartup re-read as one any test may need, the
// recording's first test case that moved past its request (firstNow, zero
// when none is known; firstBound when it is known only to be no later than
// firstNow: sentList.first), and what the re-read left out
// (openSharedLocked).
type rereadLoss struct {
	fields     []zap.Field
	firstNow   time.Time
	firstBound bool
	left       leftOut
}

// processOutQueued is what every manager of the process holds queued
// (outQueueBytes, summed): moved under each manager's outQueueMu with its own.
var processOutQueued atomic.Int64

// outChanStall is how long the queue's drainer waits for outChan's consumer
// to take a mock, in the time the process could run (utils/stall), before it
// gives up on every mock queued: a consumer that took nothing for that long,
// however busy the host, has stopped (the CLI went away, the k8s-proxy
// upload wedged). One that takes anything starts the wait again, so a slow
// consumer loses nothing to it.
const outChanStall = 30 * time.Second

// outCloseStall is outChanStall once CloseOutChan waits on the drainer. A
// stop's teardown takes no step while it waits, and the CLI that stops the
// agent kills a teardown that takes none for utils.TeardownStallChecks of the
// agent's beats (about as many seconds of its time), cutting short what comes
// after the close: the nsswitch reset, the hooks' detach, the CA's removal.
// The consumer is often gone by then (a recording's client ends its streams
// before it stops the agent); one still reading takes a mock far more often
// than this.
const outCloseStall = 5 * time.Second

// sendDropSampleRate controls how often a drop is said aloud (sampled) under
// sustained overflow.
// Emitting per-drop under a stuck consumer would flood the log and
// further starve the very goroutine we're trying to let catch up —
// the same anti-pattern the Windows redirector hit. Sample every Nth
// drop so operators still see "something is wrong" without the
// recorder loop being drowned by its own logging.
const sendDropSampleRate uint64 = 1024

// queuedMock is a mock waiting in a lane's queue for its channel.
type queuedMock struct {
	mock  *models.Mock
	owner string
	size  int64
	done  func(delivered bool, epoch uint64)
}

// outLane is one output channel's way out: the mocks queued for it, in
// order (bytes their size), and its one drainer (drainOutQueue: draining,
// drained closed when it ends). draining is set by the send that queues a
// mock on a lane with no drainer, and cleared by that drainer alone, once
// every mock handed to the lane has been delivered or dropped and told (its
// owner recorded, OnDrop and its done told). So once it is false, what reads
// it (OutQueued, OutQueuedTo, CloseOutChan) finds each mock that a send which
// has returned handed to the lane delivered, or dropped and told.
// inHand says the drainer has taken a mock out of the queue and not yet come
// back for the next (it is delivering it, or telling its done): with the
// queue, it is what a mock sent now waits behind. A drainer telling the drops
// of a give-up has nothing in hand. closing is closed by CloseOutChan, so the
// drainer gives up on a consumer that stopped sooner (outCloseStall).
// Guarded by the manager's outQueueMu.
type outLane struct {
	ch       chan<- *models.Mock
	queue    []queuedMock
	bytes    int64
	draining bool
	inHand   bool
	drained  chan struct{}
	// closing is nil once CloseOutChan has closed it.
	closing chan struct{}
	// epoch is the revokeEpoch it was last the current channel's in.
	epoch uint64
}

// HandOnProgress is a count that moves whenever the manager hands a mock on
// to its stream, or drops one on the way: a test case held for a mock of its
// window (PendingIn) can tell a queue moving from one that is not.
func (m *SyncMockManager) HandOnProgress() uint64 {
	if m == nil {
		return 0
	}
	return m.handedOn.Load()
}

// sendToOutChan hands mock to outChan (send), with no owner and nothing to
// tell.
func (m *SyncMockManager) sendToOutChan(mock *models.Mock) {
	m.send(mock, "", nil)
}

// send hands mock to outChan without ever blocking its caller (a parser, the
// ingress's ResolveRange, the flush ticker): at once when the channel has
// room and the lane has no drainer (no mock queued or in its hand, and no
// give-up's drops still being told), else queued (outQueue) for the one
// drainer (drainOutQueue), which delivers the queue in order as the consumer
// takes it. done, when set, is told once the mock is delivered (true) or
// dropped (false), after a drop is recorded; owner, when set, is the test the
// mock belongs to, recorded on a drop (recordDroppedTC) so that test is left
// out rather than replayed without it. done is told on the drainer for a
// queued mock, while its lane still reads queued (OutQueued): like OnDrop's
// fn, it must not wait for that to change, nor call CloseOutChan.
//
// A mock is dropped only when it cannot be delivered: outChan is closed or
// was never set, the queue holds outQueueMaxBytes already, or the consumer
// took nothing for outChanStall of runnable time (every queued mock then).
// It used to be dropped once outChan stayed full for 200 ms of wall time:
// a consumer behind by a full channel lost what came next to any pause
// longer than that, a GC on a starved host or a disk flush, and its test
// cases were left out of a recording that otherwise succeeded.
//
// It returns why it dropped mock when it dropped it at once (the channel
// closed or never set, the byte bound), or MockTaken: delivered, or queued, to
// be delivered or dropped on the drainer (done).
func (m *SyncMockManager) send(mock *models.Mock, owner string, done func(bool)) MockDrop {
	var d func(bool, uint64)
	if done != nil {
		d = func(delivered bool, _ uint64) { done(delivered) }
	}
	return m.sendFor(mock, owner, d)
}

// sendFor is send, telling done the revokeEpoch of a drop too (outLane.epoch,
// recordDroppedTCFor; 0 for a delivery): what a done records for the drop is
// revoked only if that is still the current channel's.
func (m *SyncMockManager) sendFor(mock *models.Mock, owner string, done func(delivered bool, epoch uint64)) MockDrop {
	if mock.IsUndelivered() {
		// An answer the client never read is no test's to lose: no replay
		// serves it and no mapping lists it (mapsTo). So its drop, on any
		// path below or from the queue, records no owner, and OnDrop is not
		// told of it (toldDropped): it suppresses no test case, by name or
		// by the span it was requested in.
		owner = ""
	}
	tell := func(delivered bool, epoch uint64) {
		m.handedOn.Add(1)
		if done != nil {
			done(delivered, epoch)
		}
	}
	m.outChanMu.RLock()
	if m.outChanClosed || m.outChan == nil {
		// Genuinely undeliverable → a real drop, counted and said as every
		// capacity drop is (dropOut), with its owner recorded (under the
		// outChanMu.RLock already held; droppedMu is a leaf lock) so the
		// orphaned TC is suppressed instead of reaching replay mock-less.
		// Before, this drop was neither counted nor said.
		epoch := m.revokeEpoch.Load() // the read lock holds it
		n, _ := m.countDrop(mock, owner, epoch, string(MockDroppedOutputClosed))
		m.outChanMu.RUnlock()
		m.sayDrop(mock, string(MockDroppedOutputClosed), n)
		m.toldDropped(mock)
		tell(false, epoch)
		return MockDroppedOutputClosed
	}
	// trySend sends at once when the lane has no drainer (nothing queued or in
	// its hand, and no give-up's drops still being told). Under outQueueMu.
	// The lane is the current channel's (outChanMu's read lock, held, keeps
	// the channel as it is).
	trySend := func() bool {
		if m.out.draining {
			return false
		}
		select {
		case m.outChan <- mock:
			return true
		default:
			return false
		}
	}
	// sent ends a send that trySend delivered, under outQueueMu and
	// outChanMu's read lock, which it lets go: the recording's consumer took
	// mock (noteDelivered).
	sent := func() MockDrop {
		epoch := m.out.epoch
		m.outQueueMu.Unlock()
		m.outChanMu.RUnlock()
		m.noteDelivered(mock, epoch)
		tell(true, 0)
		return MockTaken
	}
	m.outQueueMu.Lock()
	if trySend() {
		return sent()
	}
	m.outQueueMu.Unlock()
	// Sized outside outQueueMu: the walk is up to mockSizeNodes values for a
	// large database mock, and the drainer and every other send wait on that
	// lock. outChanMu's read lock, still held, keeps the channel as it is.
	size := mockSize(mock)
	m.outQueueMu.Lock()
	if trySend() { // the drainer ended, or the consumer took, meanwhile
		return sent()
	}
	// The bound is on what waits behind another: a mock with nothing before
	// it (no mock queued or in the drainer's hand: inHand) is queued whatever
	// its size (the parser holds it already), else a mock past the bound, or
	// one sized at it (mockSize's cap), could never wait at all. A drainer's
	// mock in hand is not counted: it is out of the queue.
	// A test's own mock leaves the last outQueueSharedRoom of the bound to
	// the mocks any test may need (queueBound).
	lane := m.out
	if (len(lane.queue) > 0 || lane.inHand) && processOutQueued.Load()+size > m.queueBound(mock, owner, lane.epoch) {
		epoch := lane.epoch
		m.outQueueMu.Unlock()
		n, left := m.countDrop(mock, owner, epoch, string(MockDroppedOutputFull))
		m.outChanMu.RUnlock()
		m.sayDrop(mock, string(MockDroppedOutputFull), n)
		m.sayLeftOutAfter(mock, string(MockDroppedOutputFull), n, left)
		m.toldDropped(mock)
		tell(false, epoch)
		return MockDroppedOutputFull
	}
	lane.queue = append(lane.queue, queuedMock{mock: mock, owner: owner, size: size, done: done})
	lane.bytes += size
	processOutQueued.Add(size)
	if !lane.draining {
		lane.draining = true
		lane.drained = make(chan struct{})
		go m.drainOutQueue(lane, lane.closing)
	}
	m.outQueueMu.Unlock()
	m.outChanMu.RUnlock()
	return MockTaken
}

// drainOutQueue delivers lane's queue to its channel, in order, as its
// consumer takes it, and ends once the queue is empty (closing drained). It
// takes each mock out of the queue before it sends it, so the mock in hand is
// its alone, and sends without outChanMu: CloseOutChan does not close the
// channel before drained is closed, and a channel replaced (SetOutputChannel)
// is never closed by the manager. It gives the mock in hand and every queued
// one up once the consumer has taken nothing for outChanStall of runnable
// time (outCloseStall once the channel is closing), and tells each drop
// before it looks at the queue again: only finding it empty ends the lane's
// draining, so no reader of it sees the lane done with a drop untold. What
// was queued meanwhile waits a stall bound of its own, as it would with a
// drainer started for it.
//
// closing is lane.closing, read where the drainer was started (under
// outQueueMu).
func (m *SyncMockManager) drainOutQueue(lane *outLane, closing <-chan struct{}) {
	defer close(lane.drained)
	meter := stall.NewMeter(m.outClock, m.outStallBound())
	defer func() { meter.Stop() }() // the meter deliverQueued left
	for {
		m.outQueueMu.Lock()
		lane.inHand = false
		if len(lane.queue) == 0 {
			lane.queue, lane.draining = nil, false // do not keep a burst's backing array
			delete(m.outOld, lane)
			m.outQueueMu.Unlock()
			return
		}
		q := lane.queue[0]
		lane.queue[0] = queuedMock{}
		lane.queue = lane.queue[1:]
		lane.bytes -= q.size
		lane.inHand = true
		processOutQueued.Add(-q.size)
		epoch := lane.epoch
		m.outQueueMu.Unlock()

		if !m.deliverQueued(lane.ch, q.mock, &meter, &closing) {
			m.giveUpOutQueue(lane, q, "the consumer took no mock for the stall bound of the time the process could run")
			meter.Reset() // what was queued since waits from now
			continue
		}
		meter.Reset() // the consumer took one
		m.noteDelivered(q.mock, epoch)
		m.handedOn.Add(1)
		if q.done != nil {
			q.done(true, 0)
		}
	}
}

// deliverQueued sends mock on out, and reports false once out's consumer
// has taken nothing for *meter's bound. Once the channel is closing (*closing
// fires), the bound is outCloseStall from then on: *meter is replaced, and
// *closing is set nil. A look is armed only while the consumer is behind,
// and once per look, not once per mock.
func (m *SyncMockManager) deliverQueued(out chan<- *models.Mock, mock *models.Mock, meter **stall.Meter, closing *<-chan struct{}) bool {
	select {
	case out <- mock:
		return true
	default:
	}
	wait := (*meter).Wait()
	for {
		select {
		case out <- mock:
			return true
		case <-*closing:
			*closing = nil
			(*meter).Stop()
			*meter = stall.NewMeter(m.outClock, m.outCloseStallBound())
			wait = (*meter).Wait()
		case <-wait:
			if (*meter).Expired() {
				return false
			}
			wait = (*meter).Wait()
		}
	}
}

// giveUpOutQueue drops hand, the mock in hand, and every mock queued in lane
// (its drainer gave up on its consumer), and tells each. The lane stays
// draining, and a replaced lane in outOld, until its drainer finds the queue
// empty after: a reader that waits for the lane to be done (OutQueued,
// OutQueuedTo, CloseOutChan's choice to wait on drained) finds every drop
// already recorded and told.
// The drops are of the lane's epoch: a lane no longer the current channel's,
// or replaced while they are told, revokes nothing (recordDroppedTCFor): its
// session's stream is not the one the revoke would go on.
func (m *SyncMockManager) giveUpOutQueue(lane *outLane, hand queuedMock, why string) {
	m.outQueueMu.Lock()
	lost := append([]queuedMock{hand}, lane.queue...)
	processOutQueued.Add(-lane.bytes)
	lane.queue, lane.bytes, lane.inHand = nil, 0, false
	epoch := lane.epoch
	m.outQueueMu.Unlock()
	m.dropQueued(lost, epoch, why)
}

// dropQueued drops mocks taken out of a lane's queue, of revokeEpoch epoch,
// and tells each.
func (m *SyncMockManager) dropQueued(lost []queuedMock, epoch uint64, why string) {
	for _, q := range lost {
		m.dropOut(q.mock, q.owner, epoch, why)
		m.toldDropped(q.mock)
		m.handedOn.Add(1)
		if q.done != nil {
			q.done(false, epoch)
		}
	}
}

// OutQueued reports whether mocks a send handed over are still on their way
// to outChan: queued behind a full channel, or in its drainer's hand, or
// given up by a drainer that has not told every drop of them yet. A flush
// (FlushOwnedWindows) returns once its mocks are sent, not once they are on
// the channel: a consumer that ends its read when a flush has returned and the
// channel is empty ends it only once this is false too. Once it is false,
// every mock a send that has returned handed over was delivered, or dropped
// with its owner recorded (WasMockDroppedForTC) and its drop told (OnDrop,
// the send's done).
//
// Of every channel this manager has had: OutQueuedTo asks about one.
func (m *SyncMockManager) OutQueued() bool {
	if m == nil {
		return false
	}
	m.outQueueMu.Lock()
	defer m.outQueueMu.Unlock()
	return (m.out != nil && m.out.draining) || len(m.outOld) > 0
}

// OutQueuedTo is OutQueued for the mocks on their way to ch alone: the
// current channel's, or a replaced one's whose lane still delivers. A
// consumer of ch waits on it, not on another session's channel.
func (m *SyncMockManager) OutQueuedTo(ch chan<- *models.Mock) bool {
	if m == nil || ch == nil {
		return false
	}
	m.outQueueMu.Lock()
	defer m.outQueueMu.Unlock()
	if m.out != nil && m.out.ch == ch && m.out.draining {
		return true
	}
	for l := range m.outOld {
		if l.ch == ch {
			return true
		}
	}
	return false
}

// outStallBound is the drainer's stall bound: outChanStall unless a test set
// outStall.
func (m *SyncMockManager) outStallBound() time.Duration {
	if m.outStall > 0 {
		return m.outStall
	}
	return outChanStall
}

// outCloseStallBound is the drainer's stall bound once the channel is closing:
// outCloseStall unless a test set outCloseStall, and never past
// outStallBound.
func (m *SyncMockManager) outCloseStallBound() time.Duration {
	if m.outCloseStall > 0 {
		return m.outCloseStall
	}
	return min(outCloseStall, m.outStallBound())
}

// toldDropped tells OnDrop's fn of a dropped mock, but not of an answer the
// client never read (models.Mock.IsUndelivered): no replay serves it and no
// test requires it, so its loss leaves out no test case. Called with no lock
// held: the hook may take locks of its own.
func (m *SyncMockManager) toldDropped(mock *models.Mock) {
	if mock.IsUndelivered() {
		return
	}
	if fn := m.onDrop.Load(); fn != nil {
		(*fn)(mock)
	}
}

// dropOut counts and says a mock dropped on its way to a consumer that could
// not take it (why), and records its owner (recordDroppedTCFor): with a revoke
// only if epoch, the drop's, is still the current channel's. Called with no
// lock held: a caller under outChanMu counts under it and says after it.
func (m *SyncMockManager) dropOut(mock *models.Mock, owner string, epoch uint64, why string) {
	n, left := m.countDrop(mock, owner, epoch, why)
	m.sayDrop(mock, why, n)
	m.sayLeftOutAfter(mock, why, n, left)
}

// countDrop counts a drop and records its owner (revokeTC), or, for a mock
// with none, leaves out the test cases that may need it (leaveOutAfter); it
// returns either's report, for sayLeftOutAfter. It returns the drop's place
// among the drops of its cause, for sayDrop: 0 for an unread answer, whose drop takes
// no sample (outChanClosedDrops). Every mock on its way out was counted added
// (AddMock, SendConfigMock), and is no longer: the report does not say a mock
// it dropped was added (SessionDrops).
func (m *SyncMockManager) countDrop(mock *models.Mock, owner string, epoch uint64, why string) (n uint64, left leftOut) {
	m.dropCount.Add(1)
	m.totalAdded.Add(-1)
	m.nameDrop(mock, why)
	if owner != "" {
		left = m.revokeTC(owner, epoch)
	} else {
		left = m.leaveOutAfter(mock, owner, epoch, why)
	}
	switch {
	case mock.IsUndelivered():
		return 0, left
	case why == string(MockDroppedOutputClosed):
		return m.outChanClosedDrops.Add(1), left
	}
	return m.overflowDrops.Add(1), left
}

// leaveOutAfter leaves out every test case of the current recording whose
// window reaches past mock's request, once mock, which any of them may need
// and none owns (servedToAnyTest), was dropped on its way out: a replay of one
// that needs it would find no mock, a false failure no one can act on. Those
// that came before it did not use it. It opens an orphan span
// (WasMockOrphanedInWindow) from that request, or from before any window
// for a mock with no request time (beforeAnyRequest), which the recording's
// end closes (SetOutputChannel); an
// earlier loss's span covers a later one. The span reaches the test cases
// checked from now on; those the stream sent already (SendingTestCase) whose
// window reaches past the request it revokes by name (recordDroppedTCFor): a
// client that reads revokes deletes them. Opened and revoked under the lock
// SendingTestCase checks the span and notes a test case under, so none is
// sent between the two. A drop for an older recording's
// channel (epoch, the drop's, is no longer the current) leaves out none of
// this one's, and a closed output's none: no test is recorded after it. Nor
// does a mock no test needs (neededByNone): its kind's replay answers its call
// with no mock, or from a copy the recording's consumer took already. It
// reports what it left out when this drop opened the span, or moved it
// earlier, else the zero leftOut. Any other mock no test owns, lost so, was
// the test's whose window it was requested in: leaveOutOver leaves out those
// whose window holds its request instead.
func (m *SyncMockManager) leaveOutAfter(mock *models.Mock, owner string, epoch uint64, why string) leftOut {
	if why == string(MockDroppedOutputClosed) || owner != "" || mock.IsUndelivered() || m.neededByNone(mock, epoch) {
		return leftOut{}
	}
	if !m.servedToAnyTest(mock, owner) {
		return m.leaveOutOver(mock, epoch, why)
	}
	from := mock.Spec.ReqTimestampMock
	if from.IsZero() {
		// No request time: any test case of the recording may need it, so
		// all are left out, from the earliest instant a span holds.
		from = beforeAnyRequest
	}
	m.sharedLossMu.Lock()
	defer m.sharedLossMu.Unlock()
	if epoch != m.revokeEpoch.Load() {
		return leftOut{}
	}
	return m.openSharedLocked(from, epoch)
}

// openSharedLocked opens, or moves earlier, the span the current recording
// (epoch) leaves its test cases out over from from, the request of a mock any
// of them may need that it lost, and revokes by name those its stream sent
// whose window reaches past from. It reports what it left out, the zero
// leftOut when the span reached from already. Under sharedLossMu.
func (m *SyncMockManager) openSharedLocked(from time.Time, epoch uint64) leftOut {
	if m.sharedLoss.close != nil && !from.Before(m.sharedLoss.from) {
		return leftOut{}
	}
	old := m.sharedLoss.close
	m.sharedLoss = sharedLoss{from: from, close: m.orphans.Open(from)}
	if old != nil {
		old()
	}
	// What is kept ended before from: a later loss moves the span only
	// earlier, so it may still need it.
	left := leftOut{from: from}
	left.revoked = m.sentTCs.revokeReaching(from, time.Time{}, m.revokeCapable.Load(), func(name string) { m.recordDroppedTCFor(name, epoch) })
	left.unnamedThrough = m.sentTCs.unnamedReaching(from)
	m.noteFirstSentLocked()
	return left
}

// rereadStartupLocked re-reads the current recording's (epoch) earliest
// start-up mock lost as a test's (startupLost) as one any test may need once
// its request is before the recording's first test case
// (beforeFirstTestCase): a revoke moved the first past it, or left the first
// unknown. A replay of the recording as it now stands serves it from its
// start-up tier to any test, so its loss leaves out every test case whose
// window reaches past its request (openSharedLocked): from then on the span
// reaches its request, and a re-read of it again leaves out none. It
// reports what that left out, nil when it was not re-read or the span reached
// its request already. Under sharedLossMu, after a revoke that may move the
// first: a test's loss's (leaveOutOver) or one by name (revokeTC). A shared
// loss's (openSharedLocked) cannot move it past the kept one's request r
// before the span's start: a test case that started by r and was sent
// either ended before r, so before the span, or overlaps r's call, so its
// loss revoked it or left it out (one a client that cannot delete keeps
// still counts toward the first), or it was let go, which a loss's revokes
// do not take out.
//
// It re-reads the earliest of those whose destination's first moved past
// their request: the span it opens reaches every later one's.
func (m *SyncMockManager) rereadStartupLocked(epoch uint64) *rereadLoss {
	var s lostStartup
	dest := ""
	for d, l := range m.startupLost {
		if l.req.IsZero() || !m.beforeFirstTestCase(l.req, d) {
			continue
		}
		if s.req.IsZero() || l.req.Before(s.req) {
			s, dest = l, d
		}
	}
	if s.req.IsZero() {
		return nil
	}
	first, _, exact := m.sentTCs.first(dest)
	left := m.openSharedLocked(s.req, epoch)
	if left.from.IsZero() {
		return nil
	}
	return &rereadLoss{fields: s.fields, firstNow: first, firstBound: !exact, left: left}
}

// leaveOutOver leaves out every test case of the current recording whose
// window holds mock's request, once mock, which no test owns and a replay
// serves only to the test whose window it was requested in (servedToAnyTest
// is false: the async capture sends each mock as it is captured, with no
// owner), was dropped on its way out: that test's replay would find no
// mock. The request alone, not the call's span, as the stale cutoff's
// leave-out (leaveOutCut) reads it: a test case that started after the
// request, over only the call's answer, did not make the call, and its
// replay does not look for the mock. It records the request (lostOver),
// which SendingTestCase finds for the test cases checked from now on, and
// revokes by name those the stream sent already (SendingTestCase) whose
// window holds it, under the lock SendingTestCase checks the spans and
// notes a test case under, so none is sent between the two. A drop for an
// older recording's channel leaves out none of this one's. It reports,
// with perTest set, the latest window end of a test case sent and let go
// (sentList) that reaches past the request, which it cannot name
// (unnamedThrough), and its place among such losses (perTest), and the zero
// leftOut when there is none. With kept set, it reports how many of those it
// revoked (revoked) a client that cannot delete them keeps
// (SetRevokeCapable), and its place among such losses. A start-up mock
// (IsStartup) so lost is kept, the earliest-requested one, to be re-read
// as one any test may need once a revoke moves the recording's first test
// case past its request (rereadStartupLocked); this loss's own revokes may
// (reread). why is the drop's, for the line that re-read says. Its request
// time is never zero: a mock with none is one any test may need
// (servedToAnyTest).
func (m *SyncMockManager) leaveOutOver(mock *models.Mock, epoch uint64, why string) leftOut {
	at := mock.Spec.ReqTimestampMock
	return m.leaveOutSpan(mock, at, at, &m.lostOver, epoch, why, func(string) {})
}

// staleCutoffWhy is why a drop at the stale cutoff was made (leaveOutCut).
const staleCutoffWhy = "the buffer's stale cutoff: a per-test mock more than 7 s past its request that no window, resolved or open, held"

// leaveOutCut leaves out, once mock, a per-test mock that no window resolved
// or open held, was dropped at the stale cutoff (resolveRange) while the
// current recording (epoch) was open, the test cases whose window holds its
// request: those whose resolve would have taken it, as a window takes the
// per-test mocks requested inside it. A request still in flight for which no
// window was opened (OpenWindow: an ingress that opens none, as the
// proxyless capture's and gRPC's do) is among them: it is left out at its
// verdict, never saved without the mock. It records the request (cutLost),
// which SendingTestCase finds for the test cases checked from now on, and
// revokes by name those the stream sent already whose window holds it,
// telling named of each, as leaveOutOver does for a capacity drop. The
// request alone, not the call's span: a test case that started after the
// request did not take the mock, and where every request opens a window,
// none still in flight holds a request the cutoff drops (it would be held),
// so the cutoff leaves out none there: its drops are a duplicate's
// leftovers and calls no request made. A mock any test may need (one with
// no request time, servedToAnyTest) is left out as leaveOutAfter does, and
// an unread answer, or one no test needs (neededByNone), leaves out none.
// shared says it was one any test may need.
func (m *SyncMockManager) leaveOutCut(mock *models.Mock, epoch uint64, named func(string)) (left leftOut, shared bool) {
	if mock.IsUndelivered() || m.neededByNone(mock, epoch) {
		return leftOut{}, false
	}
	if m.servedToAnyTest(mock, "") {
		return m.leaveOutAfter(mock, "", epoch, staleCutoffWhy), true
	}
	at := mock.Spec.ReqTimestampMock
	return m.leaveOutSpan(mock, at, at, &m.cutLost, epoch, staleCutoffWhy, named), false
}

// leaveOutSpan is leaveOutOver over [from, to], recorded in spans, telling
// named of each test case it revokes.
func (m *SyncMockManager) leaveOutSpan(mock *models.Mock, from, to time.Time, spans *Spans, epoch uint64, why string, named func(string)) leftOut {
	m.sharedLossMu.Lock()
	defer m.sharedLossMu.Unlock()
	if epoch != m.revokeEpoch.Load() {
		return leftOut{}
	}
	spans.Record(from, to)
	deletes := m.revokeCapable.Load()
	revoked := m.sentTCs.revokeReaching(from, to, deletes, func(name string) {
		m.recordDroppedTCFor(name, epoch)
		named(name)
	})
	m.noteFirstSentLocked()
	if mock.TestModeInfo.IsStartup {
		// Kept per destination, re-read against its first. One whose
		// destination the manager cannot tell is one any test may need
		// (servedToAnyTest), and gets here only if the manager lost track
		// of it since: kept under "", it is re-read below unless a test
		// case sent with no destination named came before it (fail closed).
		dest, _ := m.mockDest(mock)
		if s, ok := m.startupLost[dest]; !ok || from.Before(s.req) {
			if m.startupLost == nil {
				m.startupLost = make(map[string]lostStartup)
			}
			m.startupLost[dest] = lostStartup{req: from, fields: lostFields(mock, why)}
		}
	}
	var left leftOut
	if unnamed := m.sentTCs.unnamedReaching(from); !unnamed.IsZero() {
		m.perTestUnnamed++
		left = leftOut{from: from, unnamedThrough: unnamed, perTest: m.perTestUnnamed}
	}
	if revoked > 0 && !deletes {
		m.perTestKept++
		m.perTestKeptSent += uint64(revoked)
		left.from, left.revoked, left.kept, left.keptSent = from, revoked, m.perTestKept, m.perTestKeptSent
	}
	left.reread = m.rereadStartupLocked(epoch)
	return left
}

// SendingTestCase reports whether the recording's stream may send the test
// case named name, whose window is [start, end], and if so notes it sent:
// the earliest start among those sent and not revoked is the recording's
// first test case (beforeFirstTestCase), a loss of a mock any test may
// need, requested before end, revokes it by name (leaveOutAfter), as does a
// test's mock no test owns, lost and requested inside its window
// (leaveOutOver); one with no name is noted under the name a revoke finds it
// by (sentName). It reports false once such a loss's span reaches end, or
// the window holds such a mock's request: the stream leaves the test case
// out. The stream
// asks last, once no other check leaves it out, so that only test cases it
// sends are noted. A zero end is read as now, the latest the window can have
// ended, and a zero start as end. Nil-safe: a nil manager lets every test
// case through.
func (m *SyncMockManager) SendingTestCase(name string, start, end time.Time) bool {
	if m == nil {
		return true
	}
	sent := m.sentName(name, start)
	start, end = sendingWindow(start, end)
	m.sharedLossMu.Lock()
	out, cut := m.leavesOutLocked(start, end)
	if !out {
		m.noteSentLocked("", sent, start, end)
	}
	m.sharedLossMu.Unlock()
	if cut {
		m.sayCutLeftOut(name, start, end)
	}
	return !out
}

// sendingTestCaseOfMu is held by SendTestCaseOf while it holds its
// managers' sharedLossMu: every other path takes one manager's alone, so no
// two callers can each hold one the other waits for.
var sendingTestCaseOfMu sync.Mutex

// SendingTestCaseOf is SendingTestCase for a test case whose mocks may come
// from any of mgrs, as a DaemonSet runtime's channel takes its own manager's
// and the node-default manager's: the stream may send it only if each of
// them may, and then it is noted sent in each. It holds all their locks at
// once, so a test case one of them leaves out is noted sent by none (a
// test case noted sent but never sent would stand as the recording's first,
// and a start-up mock lost after it would not count as one any test may
// need). Nil and repeated managers are skipped; with none left, it lets the
// test case through. A stream that may refuse the test case once asked (a
// full queue), or one of several destinations, sends it through
// SendTestCaseOf instead.
func SendingTestCaseOf(mgrs []*SyncMockManager, name string, start, end time.Time) bool {
	return SendTestCaseOf(mgrs, "", name, start, end, nil)
}

// SendTestCaseOf is SendingTestCaseOf for a test case sent to the recording
// of dest, by a stream that may refuse it: when each of mgrs may send it, it
// calls send, under their locks, and notes the test case sent in each only
// when send reports it queued, so a test case its stream refused is noted by
// none, and no loss checked between the two finds it. send must not block,
// nor call into these managers. A manager whose mocks go to several
// recordings (SetDestinationOf) notes it as dest's, named as its mocks' are
// (mockDest); any other notes it as its one recording's. It reports whether
// the test case was sent, false when one of mgrs left it out (send is then
// not called) or send refused it. A nil send queues it.
func SendTestCaseOf(mgrs []*SyncMockManager, dest, name string, start, end time.Time, send func() bool) bool {
	ms := make([]*SyncMockManager, 0, len(mgrs))
	for _, m := range mgrs {
		if m != nil && !slices.Contains(ms, m) {
			ms = append(ms, m)
		}
	}
	rawStart := start
	start, end = sendingWindow(start, end)
	sent, cutBy := sendTestCaseOfLocked(ms, dest, name, rawStart, start, end, send)
	if cutBy != nil {
		cutBy.sayCutLeftOut(name, start, end)
	}
	return sent
}

// sendTestCaseOfLocked is SendTestCaseOf under its managers' locks, which it
// takes and lets go of. cutBy is the manager whose stale cutoff's loss
// (cutLost) left the test case out, for the line that names it once the
// locks are let go of; nil when none did.
func sendTestCaseOfLocked(ms []*SyncMockManager, dest, name string, rawStart, start, end time.Time, send func() bool) (sent bool, cutBy *SyncMockManager) {
	sendingTestCaseOfMu.Lock()
	defer sendingTestCaseOfMu.Unlock()
	for _, m := range ms {
		m.sharedLossMu.Lock()
	}
	defer func() {
		for _, m := range ms {
			m.sharedLossMu.Unlock()
		}
	}()
	for _, m := range ms {
		if out, cut := m.leavesOutLocked(start, end); out {
			if cut {
				cutBy = m
			}
			return false, cutBy
		}
	}
	if send != nil && !send() {
		return false, nil
	}
	for _, m := range ms {
		m.noteSentLocked(m.destKey(dest), m.sentName(name, rawStart), start, end)
	}
	return true, nil
}

// sendingWindow is a test case's window as SendingTestCase reads it: a zero
// end as now, the latest the window can have ended, and a zero start, or one
// after end, as end.
func sendingWindow(start, end time.Time) (time.Time, time.Time) {
	if end.IsZero() {
		end = time.Now()
	}
	if start.IsZero() || start.After(end) {
		start = end
	}
	return start, end
}

// leavesOutLocked reports whether the recording leaves out a test case whose
// window is [start, end]: a loss of a mock any test may need, requested no
// later than end, or the request of a lost test's mock no test owns inside
// the window, or the request of a test's mock the stale cutoff dropped inside
// the window (SendingTestCase): cut says it was the last alone. Called under
// sharedLossMu.
func (m *SyncMockManager) leavesOutLocked(start, end time.Time) (out, cut bool) {
	if m.sharedLoss.close != nil && !end.Before(m.sharedLoss.from) {
		return true, false
	}
	if over, _ := m.lostOver.Overlaps(start, end); over {
		return true, false
	}
	over, _ := m.cutLost.Overlaps(start, end)
	return over, over
}

// noteSentLocked notes the test case named name, whose window is [start,
// end], sent by the recording's stream to dest (destKey) (SendingTestCase).
// Called under sharedLossMu.
func (m *SyncMockManager) noteSentLocked(dest, name string, start, end time.Time) {
	m.sentTCs.of(dest).add(name, start, end)
	m.noteFirstSentLocked()
}

// noteFirstSentLocked stores each destination's first test case sent and
// not revoked (sentList.first) in firstSent, with no entry for one it knows
// none of, if one moved. Called under sharedLossMu, as sent changes.
func (m *SyncMockManager) noteFirstSentLocked() {
	cur := m.firstSent.Load()
	n := 0
	moved := false
	for dest, l := range m.sentTCs.lists {
		first, ok, _ := l.first()
		if !ok {
			continue
		}
		n++
		if cur == nil {
			moved = true
			break
		}
		if was, had := (*cur)[dest]; !had || !was.Equal(first) {
			moved = true
			break
		}
	}
	// An equal set of firsts is not stored again: a test case sent costs no
	// allocation unless it moves a first.
	if !moved && (cur == nil || len(*cur) == n) {
		return
	}
	next := make(map[string]time.Time, n)
	for dest, l := range m.sentTCs.lists {
		if first, ok, _ := l.first(); ok {
			next[dest] = first
		}
	}
	m.firstSent.Store(&next)
}

// endSharedLoss ends the span of test cases a recording leaves out for a mock
// it lost (leaveOutAfter), and lets go the test cases its stream sent, the
// losses it counts and the one it may re-read (startupLost): a new
// recording's are neither left out nor revoked for it.
func (m *SyncMockManager) endSharedLoss() {
	m.sharedLossMu.Lock()
	defer m.sharedLossMu.Unlock()
	if m.sharedLoss.close != nil {
		m.sharedLoss.close()
	}
	m.sharedLoss = sharedLoss{}
	m.sentTCs, m.perTestUnnamed = sentByDest{}, 0
	m.perTestKept, m.perTestKeptSent, m.startupLost = 0, 0, nil
	m.firstSent.Store(nil)
}

// sharedLossMsg opens the ERROR that says a loss of a mock any test of the
// recording may need (sayLeftOutAfter); what it says of the test cases sent
// already follows.
const sharedLossMsg = "syncMock: a mock that any test of the recording may need (a session's, a connection's or a start-up one) (or one with no request time) was dropped before the recording held it (why says where): the recording leaves out every test case whose window reaches past its request that its stream has not sent"

// sayLeftOutAfter says, at ERROR, that the recording leaves out every test
// case from left.from on for mock, the n-th drop of why (leaveOutAfter), and
// how many its stream had sent it revoked; nothing for the zero leftOut.
// For a test's mock no test owns (left.perTest, left.kept) it says, at WARN,
// the first and the sampled of those that may reach test cases it cannot
// name, and of those whose revoked test cases a client that cannot delete
// them keeps (the rest at Debug). A start-up mock re-read as one any test
// may need (left.reread) it says at ERROR first, naming it. mock is nil for
// a revoke by name, whose report holds a re-read alone. Never called under
// outChanMu.
func (m *SyncMockManager) sayLeftOutAfter(mock *models.Mock, why string, n uint64, left leftOut) {
	if r := left.reread; r != nil {
		first := "none known"
		if !r.firstNow.IsZero() {
			first = r.firstNow.Format(time.RFC3339Nano)
			if r.firstBound {
				first = "no later than " + first
			}
		}
		// Clipped: the kept fields' spare room is not written.
		m.saySharedLoss(append(slices.Clip(r.fields),
			zap.String("first_test_case_now", first),
			zap.String("reread", "a start-up mock lost earlier as a test's: revokes the client applies since moved, or may have moved, the recording's first test case (first_test_case_now; 'no later than' when only a bound is known) past its request, so a replay of the recording as it stands would serve it to any test from its start-up tier")), r.left)
	}
	if left.from.IsZero() {
		return
	}
	if left.perTest == 0 && left.kept == 0 {
		m.saySharedLoss(dropFields(mock, n, why), left)
		return
	}
	if sampled(left.perTest) {
		m.dropLogger().Warn("syncMock: a test's mock was dropped on its way out of the agent: the recording leaves out the test cases whose window holds its request, but its stream sent some it cannot name (past the latest it names by window start, between sent_named_min and sent_named_max of them, or sent with no name to a client that cannot find one by its request time), whose window ended after the mock's request and by unnamed_sent_through: they may stay recorded without it, and fail at replay with no matching mock",
			append(dropFields(mock, n, why),
				zap.String("unnamed_sent_through", left.unnamedThrough.Format(time.RFC3339Nano)),
				zap.Int("sent_named_min", maxSentTCs/2),
				zap.Int("sent_named_max", maxSentTCs),
				zap.Uint64("such_losses_so_far", left.perTest),
				zap.String("next_step", lossNextStep))...)
	}
	if left.kept == 0 {
		return
	}
	level := zapcore.DebugLevel
	if sampled(left.kept) {
		level = zapcore.WarnLevel
	}
	if ce := m.dropLogger().Check(level, keptSentMsg); ce != nil {
		ce.Write(append(dropFields(mock, n, why),
			zap.Int("revoked_sent", left.revoked),
			zap.Bool("client_deletes_revoked", false),
			zap.Uint64("kept_sent_so_far", left.keptSent),
			zap.Uint64("such_losses_so_far", left.kept),
			zap.String("next_step", lossNextStep))...)
	}
}

// keptSentMsg is the WARN that says a test's mock no test owns, lost, revoked
// test cases its stream sent to a client that cannot delete them
// (sayLeftOutAfter).
const keptSentMsg = "syncMock: a test's mock was dropped on its way out of the agent: the recording leaves out the test cases whose window holds its request that its stream has not sent, but those it sent already (revoked_sent; kept_sent_so_far in the recording) stay recorded without it, as the client that reads the stream cannot delete them: they may fail at replay with no matching mock"

// lossNextStep is what a line of a loss that leaves test cases out says to do.
const lossNextStep = "re-record once the client that reads the agent's mocks keeps up with the app's traffic: its CPU, its disk, its network to the agent"

// cutNextStep is what a line of a stale cutoff's loss says to do.
const cutNextStep = "a request decided more than 7 s after its calls were made, with no window open for it, loses them: re-record once the agent's verdicts keep up with the app (its CPU, a stalled app), or record that ingress through one that opens a window per request"

// sayCutLeftOut says that the test case named name, whose window is [start,
// end], was left out at its verdict for the request of a test's mock the
// stale cutoff dropped inside its window (cutLost): at WARN for the first
// dropNamesMax of the session and the sampled ones, the rest at Debug. Never
// called under a lock of the manager's.
func (m *SyncMockManager) sayCutLeftOut(name string, start, end time.Time) {
	n := m.cutLeftOut.Add(1)
	level := zapcore.DebugLevel
	if n <= dropNamesMax || sampled(n) {
		level = zapcore.WarnLevel
	}
	ce := m.dropLogger().Check(level, "syncMock: a test case was left out: a mock requested inside its window was dropped at the stale cutoff, more than 7 s past its request, while no window was open for the request, so a replay of it would find no matching mock")
	if ce == nil {
		return
	}
	ce.Write(zap.String("test_case", name),
		zap.String("window_start", start.Format(time.RFC3339Nano)),
		zap.String("window_end", end.Format(time.RFC3339Nano)),
		zap.Uint64("left_out_so_far", n),
		zap.String("next_step", cutNextStep))
}

// cutReport is what one resolve's stale cutoff dropped and left out
// (cutDropped), for the line that says it.
type cutReport struct {
	dropped        int
	oldest         time.Time
	revoked        int
	names          []string
	unnamedThrough time.Time
}

// name notes a test case the cutoff's loss revoked, while the line has room.
func (r *cutReport) name(n string) {
	r.revoked++
	if len(r.names) < dropNamesMax {
		r.names = append(r.names, n)
	}
}

// cutDropped counts, tells OnDrop of, and leaves out the test cases for, the
// per-test mocks a resolve (resolving) dropped at the stale cutoff, after it
// let go of mu: under it, the walk only collects them, as a line or a hook
// written there would hold up every AddMock and resolve of the manager. With
// the output bound (a recording open, epoch its channel's), each leaves out
// the test cases whose window holds its request (leaveOutCut); one with none
// leaves out nothing, as a drop of the closed output does. It says one line
// for them all: at WARN when they revoked test cases the stream sent, named,
// else at Debug, as a test case still to be decided that one leaves out is
// named at its verdict (sayCutLeftOut). A mock any test may need, or a
// start-up mock re-read as one, is said as leaveOutAfter's are. held says the
// mocks were held for the requests in flight and let go when the windows that
// may have owned them ended without taking them (settleLetGo): the line says
// so.
func (m *SyncMockManager) cutDropped(cut []*models.Mock, epoch uint64, bound bool, resolving string, held bool) {
	if len(cut) == 0 {
		return
	}
	r := cutReport{dropped: len(cut)}
	for _, mk := range cut {
		if at := mk.Spec.ReqTimestampMock; r.oldest.IsZero() || at.Before(r.oldest) {
			r.oldest = at
		}
		if bound {
			left, shared := m.leaveOutCut(mk, epoch, r.name)
			if shared {
				// One any test may need (leaveOutAfter): said as
				// its loss on the way out is.
				m.sayLeftOutAfter(mk, staleCutoffWhy, 0, left)
			} else {
				m.sayLeftOutAfter(nil, "", 0, leftOut{reread: left.reread})
				if left.unnamedThrough.After(r.unnamedThrough) {
					r.unnamedThrough = left.unnamedThrough
				}
			}
		}
		m.toldDropped(mk)
	}
	total := m.cutDrops.Add(uint64(len(cut)))
	level, msg := zapcore.DebugLevel, "syncMock: per-test mocks that no window, resolved or open, held were dropped at the stale cutoff, more than 7 s past their request: a test case decided from now on whose window holds one's request is left out at its verdict, and named then"
	if r.revoked > 0 || !r.unnamedThrough.IsZero() {
		level = zapcore.WarnLevel
		msg = "syncMock: per-test mocks that no window, resolved or open, held were dropped at the stale cutoff, more than 7 s past their request: the recording revokes the test cases its stream sent whose window holds one's request (revoked_sent_names; the client deletes them when client_deletes_revoked, else they stay recorded without the mock and may fail at replay with no matching mock), and leaves out those decided from now on"
	}
	ce := m.dropLogger().Check(level, msg)
	if ce == nil {
		return
	}
	fields := []zap.Field{
		zap.Int("dropped", r.dropped),
		zap.Uint64("dropped_so_far", total),
		zap.String("oldest_request", r.oldest.Format(time.RFC3339Nano)),
		zap.Duration("oldest_request_age", time.Since(r.oldest)),
		zap.String("resolving_test", resolving),
		zap.Bool("recording_open", bound),
		zap.Int("revoked_sent", r.revoked),
		zap.Strings("revoked_sent_names", r.names),
		zap.Bool("client_deletes_revoked", m.revokeCapable.Load()),
	}
	if held {
		fields = append(fields, zap.String("held_then_let_go", "held past the cutoff for the requests in flight whose window opened before them, and let go when those windows ended without taking them"))
	}
	if !r.unnamedThrough.IsZero() {
		fields = append(fields, zap.String("unnamed_sent_through", r.unnamedThrough.Format(time.RFC3339Nano)),
			zap.String("unnamed_sent", "test cases the stream sent and cannot name, whose window ended after a dropped mock's request and by unnamed_sent_through, are not revoked and may stay recorded without it"))
	}
	ce.Write(append(fields, zap.String("next_step", cutNextStep))...)
}

// saySharedLoss says, at ERROR, what the loss of a mock any test may need
// left out (left), with fields naming it.
func (m *SyncMockManager) saySharedLoss(fields []zap.Field, left leftOut) {
	deletes := m.revokeCapable.Load()
	fields = append(fields,
		zap.String("left_out_from", left.from.Format(time.RFC3339Nano)),
		zap.Int("revoked_sent", left.revoked),
		zap.Bool("client_deletes_revoked", deletes))
	if left.from.Equal(beforeAnyRequest) {
		fields = append(fields, zap.String("no_request_time", "the mock has no request time, so any test case of the recording may need it: every one is left out"))
	}
	if !left.unnamedThrough.IsZero() {
		fields = append(fields, zap.String("unnamed_sent_through", left.unnamedThrough.Format(time.RFC3339Nano)),
			zap.Int("sent_named_min", maxSentTCs/2),
			zap.Int("sent_named_max", maxSentTCs),
			zap.String("unnamed_sent", "test cases the stream sent and cannot name (past the latest it names by window start, between sent_named_min and sent_named_max of them, or sent with no name to a client that cannot find one by its request time), whose window ended after left_out_from and by unnamed_sent_through, are not revoked and may stay recorded without the mock"))
	}
	// The test cases the stream sent already are the client's to delete: one
	// that did not negotiate the revoke (an older CLI, or a consumer with no
	// revoke frame) is sent none, and keeps them (SetRevokeCapable).
	msg := sharedLossMsg + ", and revokes those its stream sent already (revoked_sent), as a replay of one that needs it would find no mock"
	if !deletes {
		msg = sharedLossMsg + ", but those its stream sent already (revoked_sent) stay recorded without it, as the client that reads the stream cannot delete them: they may fail at replay with no matching mock"
	}
	m.dropLogger().Error(msg, append(fields, zap.String("next_step", lossNextStep))...)
}

// sayDrop says the n-th drop of its cause, naming the mock and why: the first
// and every sendDropSampleRate-th aloud, the rest at Debug, an unread answer's
// always at Debug, as it costs no test case. Stays terse so a stuck consumer's
// goroutine does not drown in its own logging: a drainer can give up a whole
// queue at once, so the fields are built only for a line that is logged. A
// closed output is said at WARN, which a recording's stop is free to log, where
// an overflow is an ERROR. Never called under outChanMu: a log write blocked on
// I/O would hold up every path through it.
func (m *SyncMockManager) sayDrop(mock *models.Mock, why string, n uint64) {
	const closedMsg = "syncMock: a mock was handed on after the recording's mock output had closed, or while none was set, and dropped: the recording does not hold it"
	closed := why == string(MockDroppedOutputClosed)
	level, msg := zapcore.DebugLevel, "syncMock: mock dropped on its way out of the agent"
	switch {
	case !sampled(n):
	case closed && n == 1:
		level, msg = zapcore.WarnLevel, closedMsg+"; the session's first such drop: later ones are said at Debug except for the per-1024 sampled line"
	case closed:
		level, msg = zapcore.WarnLevel, closedMsg
	case n == 1:
		level, msg = zapcore.ErrorLevel, "syncMock outChan overflow on FIRST drop — mock recording is now LOSSY for this session; subsequent overflow drops are silent except for the per-1024 sampled line. Reduce concurrent test load, or investigate consumer-side stalls (slow disk / network to k8s-proxy) before re-running for a clean recording."
	default:
		level, msg = zapcore.ErrorLevel, "syncMock outChan overflow; mock dropped — consumer can't keep up with mock production"
	}
	ce := m.dropLogger().Check(level, msg)
	if ce == nil {
		return
	}
	ce.Write(append(dropFields(mock, n, why),
		zap.Duration("stall_bound", m.outStallBound()),
		zap.Int64("queue_bound_bytes", outQueueMaxBytes),
	)...)
}

// sampled reports whether the n-th drop of a kind in a session is said aloud:
// the first and every sendDropSampleRate-th. 0, an unread answer's, never is.
func sampled(n uint64) bool {
	return n != 0 && (n == 1 || n%sendDropSampleRate == 0)
}

// dropFields are what a line of a drop says: which mock, and why. Its request
// time is a string, as the summary names it (nameDrop): the agent's logger
// encodes a zap.Time with its banner.
func dropFields(mock *models.Mock, n uint64, why string) []zap.Field {
	return append([]zap.Field{zap.Uint64("dropsSoFar", n)}, lostFields(mock, why)...)
}

// lostFields are dropFields but the drop's place: which mock, and why.
func lostFields(mock *models.Mock, why string) []zap.Field {
	fields := []zap.Field{
		zap.String("why", why),
		zap.String("mock_kind", string(mock.Kind)),
		zap.String("connID", mock.ConnectionID),
		zap.String("lifetime", mock.TestModeInfo.Lifetime.String()),
		zap.String("mock_req_ts", mock.Spec.ReqTimestampMock.Format(time.RFC3339Nano)),
	}
	if mock.Spec.DNSReq != nil {
		fields = append(fields, zap.String("dns_question", dnsQuestion(mock)))
	}
	return fields
}

// OnDrop has fn told of every mock dropped on its way to outChan: on the
// capacity path (the channel closed or never set, the queue's byte bound
// reached, or its consumer stalled: send), by AddMock once the channel is
// closed (CloseOutChan), and of a per-test mock the stale cutoff drops, at
// once or held for the requests in flight and let go when their windows end
// without it (cutDropped, settleLetGo). The test cases that used such a
// mock lack it: a capture whose test cases are not named (proxyless, a
// DaemonSet) cannot find them by owner, as WasMockDroppedForTC does, and
// leaves out those that ran while the mock was requested instead. A dropped
// answer the client never read (models.Mock.IsUndelivered) is not told, on
// any of these paths (toldDropped): no test case used it, so none is left
// out for it. fn must not
// block. A drop of a given-up queue is told on its drainer, whose lane reads
// queued until it has told every drop: fn must not wait for OutQueued or
// OutQueuedTo to read false, nor call CloseOutChan, which waits for that
// drainer. nil: nothing is told.
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

// trySendControlFrame attempts a NON-blocking send of a reserved-Kind control
// frame (a revoke) on outChan. Returns true iff delivered. Unlike
// send it does NOT bump dropCount and does NOT record a drop or
// fire the "recording is lossy" log — a control frame is not a mock. If the
// channel is closed/nil or full, it returns false and the caller re-queues.
func (m *SyncMockManager) trySendControlFrame(mock *models.Mock) bool {
	return m.trySendControlFrameFor(mock, m.revokeEpoch.Load())
}

// trySendControlFrameFor is trySendControlFrame for a frame of revokeEpoch
// epoch: refused once the channel is another epoch's.
func (m *SyncMockManager) trySendControlFrameFor(mock *models.Mock, epoch uint64) bool {
	m.outChanMu.RLock()
	defer m.outChanMu.RUnlock()
	if m.outChanClosed || m.outChan == nil || epoch != m.revokeEpoch.Load() {
		return false
	}
	m.outQueueMu.Lock()
	defer m.outQueueMu.Unlock()
	if m.out != nil && m.out.draining {
		// Mocks are queued before it, or its drainer is still telling a
		// give-up's drops (what is sent meanwhile queues behind it): it waits
		// for the next tick rather than overtake them.
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
// sent once its send has finished (or dropped it): to its note too, for a
// startup mock sent for a claim (ownedMock.noted), before it stops being
// pending, so the test that takes it is told of a drop before it is let go.
func (m *SyncMockManager) sendTaken(batch []ownedMock) {
	for _, om := range batch {
		// Read before the send: once delivered, the mock is the consumer's.
		name, at, noted, mk := om.mock.Name, om.mock.Spec.ReqTimestampMock, om.noted, om.mock
		m.sendFor(om.mock, om.owner, func(delivered bool, epoch uint64) {
			if noted {
				m.sentClaimDoneFor(name, at, delivered, epoch)
			}
			m.sent(mk)
		})
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
// a request that claimed it then (dropHeldLocked). A startup mock sent for
// such a claim counts the same way (sentClaim.owner), until a request takes
// it or the pass that gives it back to that window has its name for the
// window's mapping (pruneSentClaimedLocked), which that pass sends after its
// mocks, as it does an owed mock's: the mock is sent already, and without the
// hold the window's test case went out while nothing would ever name it (with
// no mapping, once it is delivered nothing is left to hand it). One a request
// took counts until its send has finished (sentClaimDone): a drop is then
// told to that request's test before its test case goes out.
//
// No other held mock can be a mock of a test case whose window is resolved,
// which is the only kind that settles: a kept resolve takes every held mock inside its window,
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
	lo, hi := m.sentClaimRangeLocked(start, end)
	for _, c := range m.sentClaimed[lo:hi] {
		if c.takenBy != "" || c.owner != nil && (c.owner.mapping || c.sent != claimDelivered) {
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
	epoch := m.revokeEpoch.Load()
	m.pendingRevokes = nil
	m.droppedMu.Unlock()

	frame := &models.Mock{
		Kind: models.RevokedTests,
		Spec: models.MockSpec{Metadata: map[string]string{"revoked_tests": strings.Join(batch, ",")}},
	}
	// The batch is its epoch's: not sent on, nor kept for, a channel set
	// since (SetOutputChannel cleared what was queued for the old one).
	if !m.trySendControlFrameFor(frame, epoch) {
		// Re-queue the whole batch under a fresh lock (new drops may have
		// appended meanwhile — keep the retries ahead of them).
		m.droppedMu.Lock()
		if m.revokeEpoch.Load() == epoch {
			m.pendingRevokes = append(batch, m.pendingRevokes...)
		}
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
	m.recordDroppedTCFor(name, m.revokeEpoch.Load())
}

// droppedName is a name droppedTCOrder holds, with the epoch it was lost in.
type droppedName struct {
	name  string
	epoch uint64
}

// recordDroppedTCFor is recordDroppedTC for a drop of revokeEpoch epoch,
// queuing the name's revoke only if that is still the current channel's: a
// drop of a replaced channel's lane, or one decided before a switch and
// recorded after it, is revoked on no stream, and leaves out no test case
// of the current one (WasMockDroppedForTC).
func (m *SyncMockManager) recordDroppedTCFor(name string, epoch uint64) {
	if m == nil || name == "" {
		return
	}
	m.droppedMu.Lock()
	defer m.droppedMu.Unlock()
	if m.droppedTCNames == nil {
		m.droppedTCNames = make(map[string]uint64)
	}
	if e, ok := m.droppedTCNames[name]; ok && e >= epoch {
		return
	}
	m.droppedTCNames[name] = epoch
	m.droppedTCsRecorded.Add(1)
	m.droppedTCOrder = append(m.droppedTCOrder, droppedName{name: name, epoch: epoch})
	if len(m.droppedTCOrder) > maxDroppedTCNames {
		// A name lost again in a later epoch is kept for that one.
		if old := m.droppedTCOrder[0]; m.droppedTCNames[old.name] == old.epoch {
			delete(m.droppedTCNames, old.name)
		}
		k := copy(m.droppedTCOrder, m.droppedTCOrder[1:])
		m.droppedTCOrder = m.droppedTCOrder[:k]
	}
	// Deferred-orphan revoke: if the TC already streamed to the CLI, a drop now
	// is undetectable by stream-time suppression, so queue the name for LIVE
	// delivery as a RevokedTests control frame. Only when the CLI negotiated the
	// capability (revokeCapable) — an older CLI would mis-persist the frame.
	// Queued under the already-held droppedMu; the name is NEW here (the dedup
	// return above guarantees it), so pendingRevokes never accumulates dupes.
	if m.revokeCapable.Load() && epoch == m.revokeEpoch.Load() {
		m.pendingRevokes = append(m.pendingRevokes, name)
	}
}

// RevokeTestCase leaves the test case named name out of the recording after
// it was handed on: the synchronous capture hands a test case on before it
// claims its request's window (Window.LeftOut), and when the window was given
// up meanwhile, mocks the test case may own were let go. The deferred-orphan
// path a capacity drop takes leaves it out: the recording's stream suppresses
// it if it has not sent it yet (WasMockDroppedForTC), and a CLI that
// negotiated the revoke deletes it at the end if it has (RevokedTests). A CLI
// that did not (OutgoingOptions.SupportsDroppedRevoke false) keeps a test
// case the stream had sent already, without those mocks. Nil-safe.
func (m *SyncMockManager) RevokeTestCase(name string) {
	if m == nil {
		return
	}
	m.sayLeftOutAfter(nil, "", 0, m.revokeTC(name, m.revokeEpoch.Load()))
}

// revokeTC leaves the test case named name out for a cause of its own, a
// mock it owns dropped or its window given up (recordDroppedTCFor), and,
// for a drop of the current recording's (epoch), takes it out of those its
// stream sent (sentList.forget): a revoked test case is not the recording's
// first. It reports, as reread, a start-up mock lost earlier as a test's
// that this revoke made one any test may need (rereadStartupLocked), for
// sayLeftOutAfter. Never called under sharedLossMu.
func (m *SyncMockManager) revokeTC(name string, epoch uint64) leftOut {
	m.recordDroppedTCFor(name, epoch)
	m.sharedLossMu.Lock()
	defer m.sharedLossMu.Unlock()
	if epoch != m.revokeEpoch.Load() {
		return leftOut{}
	}
	m.sentTCs.forget(name, m.revokeCapable.Load())
	m.noteFirstSentLocked()
	return leftOut{reread: m.rereadStartupLocked(epoch)}
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

// SetRevokeByRequest says whether the client finds a revoked test case the
// stream sent with no name by its request time
// (OutgoingOptions.SupportsRevokeByRequest), as GetOutgoing reads it beside
// SetRevokeCapable. Without it, a loss over such a test case's window says
// that it may stay recorded without the mock.
func (m *SyncMockManager) SetRevokeByRequest(v bool) {
	if m == nil {
		return
	}
	m.revokeByRequest.Store(v)
}

// RevokesByRequest reports whether the client finds a revoked test case the
// stream sent with no name by its request time (SetRevokeByRequest).
// Nil-safe.
func (m *SyncMockManager) RevokesByRequest() bool {
	return m != nil && m.revokeByRequest.Load()
}

// sentName is the name the stream's test case named name, whose request came
// at start as the stream gives it (the test case's own request stamp, which
// the client reads from the test case it saves), is noted sent under: its
// own, or, for one with none (the async capture leaves naming to the client),
// the name a revoke finds it by, that request time
// (models.RevokeNameByRequest), when the client reads such a name
// (SetRevokeByRequest) and the start is set; a response stamped before the
// request, or none, does not change it. Else none, and a loss over its window
// cannot revoke it (sentList.add).
func (m *SyncMockManager) sentName(name string, start time.Time) string {
	if name != "" || start.IsZero() || !m.revokeByRequest.Load() {
		return name
	}
	return models.RevokeNameByRequest(start)
}

// WasMockDroppedForTC reports whether a mock owned by test `name` was dropped on
// the outChan capacity path. record.go calls this by EXACT test name so a
// suppression can never spill onto a concurrent TC that kept all its mocks.
// Only the current channel's recording's losses count: a name lost in an
// earlier one does not leave out a test case of this one that carries it.
func (m *SyncMockManager) WasMockDroppedForTC(name string) bool {
	if m == nil || name == "" {
		return false
	}
	m.droppedMu.Lock()
	defer m.droppedMu.Unlock()
	e, ok := m.droppedTCNames[name]
	return ok && e == m.revokeEpoch.Load()
}

// DroppedTCCount returns how many test cases the set of those lost by name
// holds: the manager's, bounded at maxDroppedTCNames. What a session lost is
// DropsSince's.
func (m *SyncMockManager) DroppedTCCount() int {
	if m == nil {
		return 0
	}
	m.droppedMu.Lock()
	defer m.droppedMu.Unlock()
	return len(m.droppedTCNames)
}

// MockDrop is what AddMock did with a mock handed to it: MockTaken, or why it
// dropped it, as it was handed over. A mock taken is buffered for the test
// window that owns it, or on its way to the output: taken says no more than
// that. What befalls it after is decided once its parser has handed it over,
// and cannot be told back to it, so a parser's claim to have kept a mock holds
// as of the hand-over:
//   - a drop on its way out (its consumer stalled, the queue's byte bound at a
//     flush, the output closed as it is sent) is counted in DropCount and said
//     (dropOut), its owner recorded when its send names one
//     (WasMockDroppedForTC), and OnDrop told, but of an unread answer, which
//     costs no test case;
//   - memory pressure letting go of a buffered or held mock is counted in
//     GetDropStats' pressureDropped and said (reportPressureDrop);
//   - a reaper's cleanup (a duplicate test's mocks, a mock outside any window)
//     is no loss to the recording, and is tallied apart (dropTally);
//   - a mock let go with a window given up (LostToHoldBound), or owed to a test
//     case of a recording that has ended (owedLetGo, goneLetGo), is tallied
//     apart too: it costs a test case only through a kept verdict on its
//     request, which Window.Keep warns of and counts as that test case.
//
// Said is the first and every sendDropSampleRate-th of each of the first two
// kinds in a session, naming the mock and why, and the rest at Debug. The
// session's report (DropsSince) counts them all, and names the first
// dropNamesMax. The last two kinds stay among the mocks it reports as added.
//
// Limits of that report. The agent's stop closes the output after a draining
// client's stream has ended, so a mock dropped for the closed output then is
// said, and reaches no report: the report is already written. A mock AddMock
// drops for its output has no owner, so no test case is left out by name. A
// session spans it (supervisor.Session's EmitMock and EmitMockOutcome), which
// reaches a test case only while one overlaps it: AddMock drops one only before
// the first request or after the close, so a test case that needs a session's
// mock dropped before the first request is recorded without it, and what tells
// is the drop's ERROR and the report. A parser that calls AddMock itself spans
// nothing, as a span would reach the same few test cases, and OnDrop tells a
// build that sets one. The span is the session's, not the manager's, because a
// session can send its spans elsewhere (Session.Orphans).
type MockDrop string

const (
	// MockTaken is a mock the manager took.
	MockTaken MockDrop = ""
	// MockDroppedUnderPressure is a per-test mock whose request was made
	// while the memory guard paused recording (lostToPressureLocked): no test
	// case of the recording owns it, as each that overlaps the pause is left
	// out (WasPressureActiveInWindow).
	MockDroppedUnderPressure MockDrop = "its request was made while the memory guard had paused recording; the recording leaves out the test cases that overlap the pause"
	// MockDroppedOutputClosed is a mock handed over once the recording's mock
	// output was closed (CloseOutChan: the recording had ended), or, sent at
	// once, before one was set (AddMock buffers a mock until one is).
	MockDroppedOutputClosed MockDrop = "the recording's mock output was closed (the recording had ended) or not set"
	// MockDroppedOutputFull is a mock sent at once (by AddMock before the
	// first request, or by SendConfigMock at any time) that found the mocks
	// queued for the consumer at their byte bound.
	MockDroppedOutputFull MockDrop = "the mocks queued for the consumer already hold their byte bound"
)

// Dropped reports whether d is a drop, not MockTaken.
func (d MockDrop) Dropped() bool { return d != MockTaken }

// heldLetGo is why memory pressure drops a mock held for a request in flight
// (giveUpForPressureLocked). Its request may predate the pause, so the pause's
// spans need not cover it: the window that may own it is given up instead.
const heldLetGo = "the agent was short of memory and let go of the mocks it held for the requests in flight; a kept request among them is left out"

// reportPressureDrop counts that memory pressure dropped mock, for why, and
// says it: at WARN for the session's first and every sendDropSampleRate-th, as
// dropOut says its drops, at Debug for the rest, and an unread answer's always
// at Debug, taking no sample, as it costs no test case. Each names the mock and
// why. Called with no lock held.
func (m *SyncMockManager) reportPressureDrop(mock *models.Mock, why string) {
	m.pressureDropped.Add(1)
	m.nameDrop(mock, why)
	var n uint64
	if !mock.IsUndelivered() {
		n = m.pressureSampled.Add(1)
	}
	level := zapcore.DebugLevel
	if sampled(n) {
		level = zapcore.WarnLevel
	}
	// Checked first: a pressure tick can let go of a whole buffer, while the
	// agent is short of memory, and most of it is not logged.
	ce := m.dropLogger().Check(level, "syncMock: memory pressure dropped a mock: the recording does not hold it")
	if ce == nil {
		return
	}
	ce.Write(dropFields(mock, n, why)...)
}

// dropNamesMax bounds the mocks a recording's summary names: it is one log
// line, and a pressure tick can let go of a whole buffer. Its counts have them
// all.
const dropNamesMax = 16

// nameDrop names mock, dropped for why, for its session's summary, while the
// summary has room. Every drop passes here, an unread answer's too: its parser
// may have said it kept it, and this is the report of what became of it.
func (m *SyncMockManager) nameDrop(mock *models.Mock, why string) {
	m.dropNamesMu.Lock()
	defer m.dropNamesMu.Unlock()
	if len(m.dropNames) >= dropNamesMax {
		return
	}
	what := string(mock.Kind) + " mock"
	if q := mock.Spec.DNSReq; q != nil {
		what += " of question " + dnsQuestion(mock)
	}
	if mock.ConnectionID != "" {
		what += " of connection " + mock.ConnectionID
	}
	m.dropNames = append(m.dropNames, what+", asked at "+mock.Spec.ReqTimestampMock.Format(time.RFC3339Nano)+": "+why)
}

// dnsQuestion is the question a DNS mock answers, its name and type: a DNS
// mock has no connection to name it by.
func dnsQuestion(mock *models.Mock) string {
	return mock.Spec.DNSReq.Name + " " + mock.Spec.Metadata["qtype"]
}

// DropMark is where a manager's drop counts stood as a recording session
// opened (StartDropSession).
type DropMark struct {
	added, pressure, testCases int64
	capacity                   uint64
}

// SessionDrops is what a recording session's summary reports of the mocks its
// manager took and dropped (DropsSince).
type SessionDrops struct {
	// Added is the mocks taken and not dropped since, for pressure or the
	// output: a mock is not reported both added and dropped, but for a drop
	// landing as these are read, each on its own. MockDrop says what else
	// stays in it.
	Added int64
	// Pressure is the mocks dropped for memory pressure, as they were handed
	// over or after.
	Pressure int64
	// Capacity is the mocks dropped for the output: closed or never set, at
	// its byte bound, or its consumer stalled.
	Capacity uint64
	// TestCases is the test cases lost by name (WasMockDroppedForTC): the
	// owner of a Capacity drop, or one revoked (RevokeTestCase). A name
	// already in the set, from this session or an earlier one, is not counted
	// again.
	TestCases int64
	// Named is the first dropNamesMax of those dropped, each with why.
	Named []string
}

// StartDropSession marks where the drop counts stand, forgets the mocks named
// so far, and starts their sampling over: the counts are of the manager's life
// and it outlives a session, so a session reports what they grow by while it
// is open, and is told of its first drop of each kind. Called as a recording's
// incoming stream opens (routes/record.go). The names and the sampling are the
// manager's, not a stream's: a stream opened while another is open takes them
// over, and the first reports its counts with fewer names. So does a drop on a
// replaced lane (SetOutputChannel), which lands in the session open when it
// gives up.
func (m *SyncMockManager) StartDropSession() DropMark {
	m.dropNamesMu.Lock()
	m.dropNames = nil
	m.dropNamesMu.Unlock()
	m.outChanClosedDrops.Store(0)
	m.overflowDrops.Store(0)
	m.pressureSampled.Store(0)
	m.cutDrops.Store(0)
	m.cutLeftOut.Store(0)
	return DropMark{
		added:     m.totalAdded.Load(),
		pressure:  m.pressureDropped.Load(),
		capacity:  m.dropCount.Load(),
		testCases: m.droppedTCsRecorded.Load(),
	}
}

// DropsSince reports what the counts grew by since mark, and the mocks named
// since. Added stops at 0: this session can drop mocks an earlier one added.
func (m *SyncMockManager) DropsSince(mark DropMark) SessionDrops {
	d := SessionDrops{
		Added:     max(m.totalAdded.Load()-mark.added, 0),
		Pressure:  m.pressureDropped.Load() - mark.pressure,
		Capacity:  m.dropCount.Load() - mark.capacity,
		TestCases: m.droppedTCsRecorded.Load() - mark.testCases,
	}
	m.dropNamesMu.Lock()
	defer m.dropNamesMu.Unlock()
	d.Named = append(d.Named, m.dropNames...)
	return d
}

// AddMock takes mock from its parser: buffered for the test window that owns
// it, or sent on to the output (before the first request), or dropped, which
// it returns (MockDrop) with why, having counted and said it: for memory
// pressure (MockDroppedUnderPressure), the output closed
// (MockDroppedOutputClosed) or at its byte bound (MockDroppedOutputFull). A
// parser that says it kept a mock must ask this, through supervisor.Session's
// EmitMockOutcome: EmitMock returns nil for a mock dropped here.
func (m *SyncMockManager) AddMock(mock *models.Mock) MockDrop {
	// Unification (Phase 1): resolve the live mock's Lifetime immediately
	// on entry so the buffered mock carries a correctly-typed
	// TestModeInfo.Lifetime into whichever downstream consumer drains
	// syncMock next (persistence writer, downstream agent via outChan,
	// etc.). Cheap — single map probe — and removes the need for
	// downstream code to call DeriveLifetime defensively.
	if mock != nil {
		mock.DeriveLifetime()
		// The proven model's container key of the connection the mock came
		// from (containerkey). Stamped here, as the parser hands the mock
		// over, while its connection is still open.
		if provenswitch.On() {
			containerkey.Default.Stamp(mock)
			// And the process its call came from, and its test worker.
			starts.Default.StampOrigin(mock)
			starts.Default.StampInstance(mock)
		}
		// The address its connection reached, in every mode, stamped while
		// its connection is open (starts.Registry.StampDest).
		starts.Default.StampDest(mock)
	}
	// The recording it goes to, named before mu is taken: the namer is the
	// caller's (SetDestinationOf).
	var dest string
	known := true
	if mock != nil {
		dest, known = m.mockDest(mock)
	}
	m.mu.Lock()
	if m.lostToPressureLocked(mock) {
		m.mu.Unlock()
		m.reportPressureDrop(mock, string(MockDroppedUnderPressure))
		return MockDroppedUnderPressure
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
	//
	// A manager whose mocks go to several recordings (SetDestinationOf) reads
	// the count of the recording the mock goes to (startupLocked): one whose
	// recording it cannot tell, or whose recording's requests it does not
	// resolve itself, stays tagged. Whether one is served to any test is then
	// the first test case of its own recording's to say (servedToAnyTest).
	if mock != nil && m.startupLocked(dest, known) {
		mock.TestModeInfo.IsStartup = true
	}
	m.stampOutsideRequestsLocked(mock)

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
		// Counted and said as every capacity drop is (dropOut: the
		// summary's mocks_dropped_capacity), naming the mock. It usually
		// fires only
		// during shutdown, but in CI a poorly-ordered teardown can race
		// the recorder's final emit and lose mocks captured in the last
		// few milliseconds of the run: before, only a Debug line said so.
		// It has no owner yet (the buffer would have given it one).
		m.dropOut(mock, "", 0, string(MockDroppedOutputClosed))
		m.toldDropped(mock)
		return MockDroppedOutputClosed
	case bound && !m.firstReqSeen:
		m.noteSending(mock) // under mu: it leaves no gap for PendingIn
		m.mu.Unlock()
		// Dropped at once (the output closed since outChanStatus, or the
		// byte bound) or taken: delivered, or queued for the drainer, whose
		// drop of it later is said there (dropOut), not here.
		return m.send(mock, "", func(bool) { m.sent(mock) })
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
		return MockTaken
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
// DNS sends also stay safe against proxy shutdown. It returns the drop, as
// AddMock does, for a mock dropped at once (the output closed, or at its
// byte bound), counted and said: a caller that holds its question as
// recorded lets go of it (DNS: dnsRecordDedupe.giveBack). A nil manager has
// no output, and drops it (MockDroppedOutputClosed).
func (m *SyncMockManager) SendConfigMock(mock *models.Mock) MockDrop {
	if m == nil {
		return MockDroppedOutputClosed
	}
	m.totalAdded.Add(1)
	m.noteSending(mock)
	return m.send(mock, "", func(bool) { m.sent(mock) })
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

	// Closed to senders first: what comes after is dropped as closed. What
	// is queued already goes on to a consumer that still reads (an older
	// client's stop drains the stream until it closes): the channel is closed
	// once the drainer has delivered it, or given it up on a consumer that
	// took nothing for outCloseStall (a client that ended its streams, or is
	// gone) and told each drop: a lane giving up is draining until it has, so
	// a close that comes meanwhile waits for it too.
	m.outChanMu.Lock()
	if m.outChanClosed {
		m.outChanMu.Unlock()
		return
	}
	m.outChanClosed = true
	ch := m.outChan
	m.outQueueMu.Lock()
	var drained chan struct{}
	if lane := m.out; lane != nil {
		if lane.draining {
			drained = lane.drained
		}
		if lane.closing != nil {
			close(lane.closing)
			lane.closing = nil
		}
	}
	m.outQueueMu.Unlock()
	m.outChanMu.Unlock()
	if drained != nil {
		<-drained
	}

	// ch, not m.outChan: a new session's channel set meanwhile
	// (SetOutputChannel) is open, and its senders send on it. Nothing sends
	// on ch any more: its drainer has ended, and a send reaches only the
	// channel set now, if it is open.
	m.outChanMu.Lock()
	defer m.outChanMu.Unlock()
	if ch != nil {
		close(ch)
	}
}

// flushTake is what FlushOwnedWindows does with a buffered mock
// (flushTakeLocked): every take but flushKeep moves it out of the buffer.
type flushTake int

const (
	// flushKeep: no window can take it yet; it stays buffered for one.
	flushKeep flushTake = iota
	// flushReusable: a session or connection mock, sent as it is.
	flushReusable
	// flushOwned: in a resolved kept window, sent for that window's test.
	flushOwned
	// flushDuplicates: in a resolved static-dedup duplicate's window: a
	// startup mock is sent, any other held for a request in flight or dropped.
	flushDuplicates
	// flushStartup: a startup mock no window owns, sent.
	flushStartup
	// flushClaimed: in a resolved kept window's part after its yield, which a
	// request in flight claims: held for that request.
	flushClaimed
	// flushGone: in a window of a recording that has ended, whose streams
	// are no longer bound (goneLocked): let go.
	flushGone
)

// attributedReusable reports whether mock is attributed as a reusable mock,
// owned by no test window: a session or connection mock, but for one reusable
// only by its recorder's statement (a MySQL SET, BEGIN or COMMIT;
// models.Mock.ReusableByStatementOnly), and, under the proven switch, a test
// mock (models.Mock.IsProtocolPhase, not async-egress). Serving gives some
// test mocks the session tier: a Generic exchange its recorder stated
// per-test, a Postgres v1/v2 data mock the lax kind fallback promotes
// (models.Mock.DeriveLifetime). Under the switch each is owned by its window,
// as a recording made without --sync maps it from its window, so the replay
// serves it to its test: an unowned test mock is the pool's, which serves no
// data-plane mock while a test is open.
func attributedReusable(mock *models.Mock) bool {
	lt := mock.TestModeInfo.Lifetime
	if lt != models.LifetimeSession && lt != models.LifetimeConnection {
		return false
	}
	if mock.ReusableByStatementOnly() {
		return false
	}
	return !(provenswitch.On() && !mock.IsProtocolPhase() && !mock.IsAsync())
}

// mapsTo reports whether a test's mapping lists mock once the test's window
// owns it: every mock but an answer the client never read
// (models.Mock.IsUndelivered), which no replay serves, so no test makes the
// call it recorded, under either switch. That one is owned, named, sent and
// pruned as its window's other mocks are; only the mappings leave it out
// (withMapped, withLate, noteSentClaimedLocked), and its drop, by its send or
// by AddMock after the close, records no owner and is not told to OnDrop, so
// it suppresses no test case (sendFor, toldDropped).
func mapsTo(mock *models.Mock) bool {
	return !mock.IsUndelivered()
}

// withMapped is ids, a test's mapping, with mock's name added when the
// mapping lists mock (mapsTo).
func withMapped(ids []string, mock *models.Mock) []string {
	if !mapsTo(mock) {
		return ids
	}
	return append(ids, mock.Name)
}

// withLate is late, the late mappings by test, with mock's name added to
// test's when the mapping lists mock (mapsTo); late is made when nil.
func withLate(late map[string][]string, test string, mock *models.Mock) map[string][]string {
	if !mapsTo(mock) {
		return late
	}
	if late == nil {
		late = make(map[string][]string)
	}
	late[test] = append(late[test], mock.Name)
	return late
}

// flushTakeLocked is what the next FlushOwnedWindows does with mock, and the
// windows it does it by (ownerOrSharedLocked). BacklogBefore asks it too: what
// a flush will take is still to be handed on. Caller holds m.mu.
func (m *SyncMockManager) flushTakeLocked(mock *models.Mock) (take flushTake, w resolvedWindow, shared resolvedWindow, behind bool) {
	// A mock reusable only by its recorder's statement (a MySQL SET, BEGIN or
	// COMMIT; models.Mock.ReusableByStatementOnly) is attributed as a per-test
	// mock is: owned by its window, pruned with a duplicate's.
	if attributedReusable(mock) {
		return flushReusable, w, shared, false
	}
	w, ok, shared, behind := m.ownerOrSharedLocked(mock.Spec.ReqTimestampMock)
	switch {
	case ok && m.goneLocked(w.recording):
		return flushGone, w, shared, behind
	case ok && w.keep:
		return flushOwned, w, shared, behind
	case ok:
		return flushDuplicates, w, shared, behind
	case isStartupMock(mock):
		return flushStartup, w, shared, behind
	case behind:
		return flushClaimed, w, shared, behind
	}
	return flushKeep, w, shared, behind
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
	mocksToSend, lateMappings, gaveUp := m.flushLocked(outChanBound, &tally)
	hold := m.holdStateLocked()
	m.noteTaken(mocksToSend)
	// Owed before the mocks go: a stop asked between the two sends waits for
	// the mappings too (mappings_owed.go).
	paidLate := m.oweLateMappingsLocked(mappingChan != nil, lateMappings, mocksToSend)
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

	// Send AFTER releasing m.mu — the send takes outChanMu and the out
	// queue's lock, and tells each mock's outcome under locks of its own.
	m.sendTaken(mocksToSend)
	if mappingChan != nil {
		for tn, ids := range lateMappings {
			if len(ids) == 0 {
				continue
			}
			m.sendMapping(mappingChan, models.TestMockMapping{TestName: tn, MockIDs: ids}, paidLate[tn])
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

// flushLocked is FlushOwnedWindows' pass over the buffer, the startup mocks
// sent for a claim and what is owed, with the output bound (outChanBound): what
// to send, the late mappings to send with it, and the windows the hold's bound
// gave up. Counted in tally. Caller holds mu, and sends after letting it go.
func (m *SyncMockManager) flushLocked(outChanBound bool, tally *dropTally) (mocksToSend []ownedMock, lateMappings map[string][]string, gaveUp []*Window) {
	openFrom, open := m.openFromLocked()
	var refs ownerRefs

	keepIdx := 0
	for i := 0; i < len(m.buffer); i++ {
		mock := m.buffer[i]
		if mock == nil {
			continue
		}
		take, w, shared, behind := m.flushTakeLocked(mock)
		switch take {
		case flushGone:
			// A late mock of a test case of a recording that has ended,
			// whose streams are no longer bound: no stream of its own
			// takes it, and the next recording's is no place for it.
			m.goneLetGo.Add(1)
			continue
		case flushReusable:
			// Reusable across tests; flush verbatim (never renamed),
			// matching ResolveRange's lifetime carve-out. Owned by no
			// specific test → owner "" (a capacity drop records nothing).
			mocksToSend = append(mocksToSend, ownedMock{mock: mock})
			continue
		case flushDuplicates:
			// A static-dedup duplicate's window (sync mode): its late mocks
			// go the way its in-time ones went in ResolveRange — pruned,
			// except a startup-window mock, which is rescued with no owner
			// and no mapping (a duplicate's testName is synthetic), and one
			// a request still in flight may own, which is held for it.
			if isStartupMock(mock) {
				mock.Name = "mock-" + generateRandomString(8)
				noted := m.noteSentClaimedLocked(mock, nil, refs.of(shared, behind))
				mocksToSend = append(mocksToSend, ownedMock{mock: mock, noted: noted})
			} else {
				m.holdOrDropLocked(mock, droppedDuplicateLeftover, openFrom, open, tally, refs.of(shared, behind))
			}
			continue
		case flushOwned:
			mock.Name = "mock-" + generateRandomString(8)
			if w.mapping {
				lateMappings = withLate(lateMappings, w.testName, mock)
			}
			// Owned by the matched window's test → tag it so a capacity
			// drop suppresses that TC.
			mocksToSend = append(mocksToSend, ownedMock{mock: mock, owner: w.testName})
			continue
		case flushStartup:
			// STARTUP RESCUE: a startup-window mock the ownerWindowLocked check above
			// didn't claim (boot traffic owns no window; an early-test mock whose
			// window hasn't resolved yet on this ticker tick). Flush it to disk
			// proactively rather than leaving it parked in the buffer where a dedup
			// cleanup, stale-cutoff, or memory-pressure wipe could reap it before it
			// is ever persisted. Owns no specific test → owner "".
			mock.Name = "mock-" + generateRandomString(8)
			noted := m.noteSentClaimedLocked(mock, nil, refs.of(shared, behind))
			mocksToSend = append(mocksToSend, ownedMock{mock: mock, noted: noted})
			continue
		case flushClaimed:
			// In a resolved kept window's part after its yield, and a request
			// in flight claims it: held for that request now, with the window
			// it goes back to if that request ends without it.
			m.holdForClaimLocked(mock, tally, refs.of(shared, true))
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
	m.pruneSentClaimedLocked(&lateMappings)
	gaveUp = m.fitHoldLocked(tally)
	m.handOwedLocked(outChanBound, &mocksToSend, &lateMappings)
	return mocksToSend, lateMappings, gaveUp
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
	// kept, givenUp, inFlight, cause, told, toldEpoch and takeBack are
	// guarded by m.mu. kept: Claim (or Keep) claimed it, so it is never given
	// up. givenUp: it was given up, for cause (fitHoldLocked,
	// giveUpForPressureLocked). inFlight: given up, its request has not ended
	// yet (noteGivenUpInFlightLocked). told:
	// Keep told its loss, and Replaced has not taken it back; toldEpoch is
	// the manager's lossEpoch then (the loss is on the tallies open then,
	// which are those still open whose epoch is no later), and takeBack is
	// what lost returned. told sits with the other flags: the window stays
	// one 112 B allocation.
	kept      bool
	givenUp   bool
	inFlight  bool
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
	// LostToStop: the request was in flight across its recording's stop, and
	// that recording has ended (EndStoppedRecording). It is no request of the
	// next, and the one it began in ended without it: Keep reports false, and
	// tells no loss.
	LostToStop
)

func (c LossCause) String() string {
	switch c {
	case LostToHoldBound:
		return "hold_bound"
	case LostToMemoryPressure:
		return "memory_pressure"
	case LostToStop:
		return "recording_stopped"
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
//     counted and told. A caller that hands its test case on to a queue that
//     can be full (the synchronous capture, conn.Capture) asks LeftOut
//     first, hands the test case on with the window still unclaimed, and
//     only then Keeps it: a Keep false then means the test case went out
//     without mocks it may own, and RevokeTestCase leaves it out.
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
// A request in flight across its recording's stop is the stopping
// recording's while that drains, and is given up once it has ended
// (EndStoppedRecording): it is no request of the next one (LostToStop).
//
// Only the incoming proxy's HTTP/1 ingress opens windows. gRPC capture
// (CaptureGRPC) opens none and resolves
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
// (LossTally), and the window's lost is told the cause. A request in flight
// across its recording's stop, given up once that recording ended
// (LostToStop), is no loss of either recording, and tells none. A nil window
// reports true.
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
		if cause == LostToStop {
			// No recording's loss: the one it began in ended without it, as
			// a stop ends without the requests whose response has not come,
			// and the one that started since never had it.
			w.lost = nil
			if ce := w.m.dropLogger().Check(zap.DebugLevel, "diag/syncMock: left out a request that was in flight across its recording's stop: the recording that started since does not record it"); ce != nil {
				ce.Write(zap.Time("in_flight_since", w.start))
			}
			return
		}
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

// LeftAtStop reports whether the window was given up because its request was
// in flight across its recording's stop (LostToStop): its test case is no
// recording's. The capture without --sync, which resolves nothing, leaves such
// a test case out by it, as Keep does. False for a nil window.
func (w *Window) LeftAtStop() bool {
	if w == nil {
		return false
	}
	w.m.mu.Lock()
	defer w.m.mu.Unlock()
	return w.givenUp && w.cause == LostToStop
}

// LeftOut reports whether the window was given up already, telling its loss
// as Keep does when it was. Unlike Keep it claims nothing: a window it finds
// open stays open and unclaimed, so the bound and memory pressure can still
// give it up. The synchronous capture asks it before it hands its test case
// on, and claims the window (Keep) only once the test case is in the
// recording's queue: a claimed window pins the hold, and the queue can be
// full for as long as the recording's stream is slow. False for a nil window.
func (w *Window) LeftOut() bool {
	if w == nil {
		return false
	}
	w.m.mu.Lock()
	givenUp := w.givenUp
	w.m.mu.Unlock()
	// Given up, the window is never claimed again: Keep reports false and
	// tells the loss, once.
	return givenUp && !w.Keep()
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
		m.settleLetGo()
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
	switch {
	case w.idx >= 0:
		heap.Remove(&m.open, w.idx)
		m.noteOldestLocked()
		m.windowLeftLocked(w)
		m.lastWindowEnd = time.Now()
		if givenUp {
			m.noteGivenUpInFlightLocked(w)
		}
	case w.inFlight:
		m.givenUpEndedLocked(w)
	}
	if _, found := m.stopFound[w]; found && !givenUp {
		// Ended: no request a stop found in flight any more. One given up
		// stays, for its loss to be told as none of either recording's
		// (endStopFoundLocked). Its test case may still come after the
		// stopping recording's stream stopped reading (a response that came
		// after the stop): the next session leaves it out (TakeLeftovers).
		delete(m.stopFound, w)
		if m.leftovers == nil {
			m.leftovers = make(map[int64]struct{})
		}
		m.leftovers[w.start.UnixNano()] = struct{}{}
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
// it, the loss a kept verdict on it would reveal. One held at the stale
// cutoff is the cutoff's drop, made late: a request in flight with no window
// open for it may still be decided with it inside its window, so it goes to
// heldCut, for settleLetGo to leave that request's test case out once mu is
// let go (cutDropped). A duplicate's leftover does not: its own window says
// it is that duplicate's. Returns the mocks dropped. Caller holds mu.
func (m *SyncMockManager) dropHeldLocked(n int, t *dropTally, givenUp bool) (dropped []*models.Mock) {
	epoch := m.revokeEpoch.Load()
	for _, h := range m.held[:n] {
		if h.owner != nil {
			m.owed = append(m.owed, owedMock{mock: h.mock, to: h.owner})
			continue
		}
		if h.why == droppedOutsideAnyWindow {
			m.heldCut = append(m.heldCut, cutHeld{mock: h.mock, epoch: epoch})
			m.letGo.Store(true)
		}
		dropped = append(dropped, h.mock)
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
	n := 0
	for _, o := range m.owed {
		if m.goneLocked(o.to.recording) {
			// Owed to a test case of a recording that has ended, whose
			// streams are no longer bound: the next recording's is no
			// place for it.
			m.owedLetGo.Add(1)
			continue
		}
		n++
		o.mock.Name = "mock-" + generateRandomString(8)
		if o.to.mapping {
			*late = withLate(*late, o.to.testName, o.mock)
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
// kept for it. Returns the mocks dropped and the windows given up. Caller holds
// mu.
func (m *SyncMockManager) giveUpForPressureLocked() (dropped []*models.Mock, gaveUp []*Window) {
	n := len(m.held)
	if n == 0 {
		return nil, nil
	}
	for _, w := range m.open {
		if w.kept {
			n = min(n, sort.Search(len(m.held), func(i int) bool { return !m.held[i].mock.Spec.ReqTimestampMock.Before(w.start) }))
		}
	}
	if n == 0 {
		return nil, nil
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
		m.windowLeftLocked(w)
		m.noteGivenUpInFlightLocked(w)
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

// StartRecordSession starts a recording: until its own first request
// (SetFirstRequestSignaled), what the application calls is a startup call,
// sent on as it is captured, and its first test cases are the startup window
// again (resolvedTestCount). The manager is the process's, and outlives a
// recording: a long-lived agent's next one, or keploy's own next session,
// started with the earlier one's first request seen, and a call made before
// its own first request waited, as a test's, for a test case's window that
// never came. The last recording has ended, if nothing said so before
// (EndStoppedRecording). Called as a record session starts (the proxy's
// Record).
func (m *SyncMockManager) StartRecordSession() {
	m.mu.Lock()
	m.firstReqSeen = false
	m.resolvedTestCount = 0
	m.destTCs = nil
	m.mu.Unlock()
	m.EndStoppedRecording()
}

// NoteStop notes that the recording stops at the instant at: the requests in
// flight that began by then are the ones its stop found (stopFound). Its stop
// says so at each of its asks of what is pending (BacklogBefore, with the
// stop's instant), and at the agent's graceful shutdown (the proxy's, at the
// shutdown's own time: for keploy record, the stop's; for keploy mock, which
// sends it once its drain is done, later). Each ask with the stop's instant
// notes the same requests, and none that began after it, whenever it comes:
// an ask late in a long drain takes nothing of what began during it. An ask
// of a stop whose recording has ended (EndStoppedRecording) notes nothing: it
// would take the next recording's requests. A zero at notes nothing.
func (m *SyncMockManager) NoteStop(at time.Time) {
	if m == nil || at.IsZero() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !at.After(m.endedStopAt) {
		return
	}
	if at.After(m.stopAt) {
		m.stopAt = at
	}
	for _, w := range m.open {
		if w.start.After(at) {
			continue
		}
		if m.stopFound == nil {
			m.stopFound = make(map[*Window]struct{}, len(m.open))
		}
		m.stopFound[w] = struct{}{}
	}
}

// TakeLeftovers returns the request times (Unix nanoseconds) of the requests
// the last recordings that ended found in flight at their stops
// (EndStoppedRecording), and forgets them; nil when none since the last take.
// Each is no request of a later recording. A test case of one can still be
// built after its recording ended: one claimed for its resolve as it ended,
// which is left to the resolve, or one captured without --sync as its window
// was given up, past its look (Window.LeftAtStop). It arrives at the next
// recording's incoming stream, the first since that end, which takes these at
// its attach and leaves such a test case out.
func (m *SyncMockManager) TakeLeftovers() map[int64]struct{} {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.leftovers
	m.leftovers = nil
	return l
}

// EndStoppedRecording says the recording a stop was noted for (NoteStop) has
// ended: its incoming stream ended, the next recording's attached, or the next
// recording started (StartRecordSession), whichever comes first; the others
// find it ended already. The requests its stop found that are still in flight
// are given up (endStopFoundLocked): each is no request of the next
// recording, whose loss accounting has not begun at any of those points. What
// is owed back to its test cases (mocks held for a request in flight beside
// them, which that request did not take) goes to its mock stream now, in the
// same step that ends it, while that stream is still the one bound, and is
// let go if none is. From then on the manager is in the next recording: what
// goes back later to one of the ended recording's test cases (a mock owed
// back, a late mock, a startup mock's mapping) goes to its streams while they
// are still the ones bound, and is let go once the next recording has set its
// own (goneLocked): never handed to the next recording's streams, where its
// test's name may be another test's. A no-op when no stop was noted.
func (m *SyncMockManager) EndStoppedRecording() {
	if m == nil {
		return
	}
	var t dropTally
	var send []ownedMock
	var late map[string][]string
	m.mu.Lock()
	if m.stopAt.IsZero() {
		m.mu.Unlock()
		return
	}
	gaveUp := m.endStopFoundLocked(&t)
	outChanBound, _ := m.outChanStatus()
	mappingChan := m.mappingChan
	var heldGaveUp []*Window
	if outChanBound {
		// What goes to the ended recording's test cases now (its buffered
		// late mocks, what is owed back, its startup mocks' mappings) goes
		// to its streams, in the step that ends it.
		send, late, heldGaveUp = m.flushLocked(outChanBound, &t)
	}
	letGo := len(m.owed) // no stream of the ended recording is bound to take them
	m.owed = nil
	m.recording++
	m.endedLaneEpoch = m.revokeEpoch.Load()
	m.endedMappingGen = m.mappingGen // its writer holds mu
	m.endedStopAt, m.stopAt = m.stopAt, time.Time{}
	m.publishHoldLocked()
	hold := m.holdStateLocked()
	m.noteTaken(send)
	paidLate := m.oweLateMappingsLocked(mappingChan != nil, late, send)
	m.mu.Unlock()
	m.reportGivenUp(heldGaveUp, LostToHoldBound)
	m.settlePool()
	m.sendTaken(send)
	if mappingChan != nil {
		for tn, ids := range late {
			if len(ids) > 0 {
				m.sendMapping(mappingChan, models.TestMockMapping{TestName: tn, MockIDs: ids}, paidLate[tn])
			}
		}
	}
	if ce := m.dropLogger().Check(zap.DebugLevel, "diag/syncMock: a stopped recording ended: gave up the requests its stop found in flight, which are no requests of the next one"); ce != nil {
		ce.Write(append(append([]zap.Field{zap.Int("given_up", gaveUp), zap.Int("owed_handed_on", len(send)), zap.Int("owed_let_go", letGo)}, t.fields()...), hold.fields()...)...)
	}
}

// goneLocked reports whether a resolved window of recording (its count of
// ended recordings, resolvedWindow.recording) belongs to a recording that has
// ended and whose streams are no longer bound: one older than the last to end,
// or the last to end once another output lane (SetOutputChannel) or another
// mapping stream (SetMappingChannel) has been set: the next recording's. The
// next recording's mapping stream is set as it starts, its output lane only at
// its application's first outgoing connection, so either is the sign. What
// goes back to such a window's test case (a late mock, one owed back, a
// startup mock's mapping) is let go, mock and mapping alike: the next
// recording's streams are no place for it. Until then the ended recording's
// streams are still the ones bound, and take it. Caller holds mu.
func (m *SyncMockManager) goneLocked(recording uint32) bool {
	if recording == m.recording {
		return false
	}
	return recording+1 != m.recording || m.revokeEpoch.Load() != m.endedLaneEpoch || m.mappingGen != m.endedMappingGen
}

// endStopFoundLocked gives up the requests a stopped recording's stop found in
// flight that still are (stopFound), and returns how many. Each began in that
// recording, which has ended, and is no request of the next (LostToStop).
// Mocks are attributed by time alone: open, it held every leftover of the next
// recording's requests (a duplicate's, a call past the stale horizon) until
// the hold's budget gave it up, and kept there it was recorded there with
// them, the calls it made before the stop gone to the last recording or let
// go. A claimed one is being resolved this moment (Claim, ResolveKept) and is
// left to its resolve. One the hold's budget or memory pressure gave up
// meanwhile has its loss, not told yet, told as none of either recording's.
// Caller holds mu.
func (m *SyncMockManager) endStopFoundLocked(t *dropTally) (gaveUp int) {
	for w := range m.stopFound {
		if m.leftovers == nil {
			m.leftovers = make(map[int64]struct{}, len(m.stopFound))
		}
		m.leftovers[w.start.UnixNano()] = struct{}{}
		switch {
		case w.idx >= 0 && !w.kept:
			w.givenUp, w.cause = true, LostToStop
			m.endWindowLocked(w, t, true)
			gaveUp++
		case w.givenUp && !w.told:
			w.cause = LostToStop
		}
	}
	m.stopFound = nil
	t.windowsGivenUp += gaveUp
	return gaveUp
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
// like the drop counters (GetDropStats). The count only grows, so what it grew
// by over a recording session is that session's: routes/record.go's summary
// reports that.
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
	m.lostOver.CheckedBefore(t)
	m.cutLost.CheckedBefore(t)
}

// pressureActiveAtLocked reports whether instant t fell inside any recorded
// pressure span. Caller holds m.mu (the AddMock / SetMemoryPressure paths); the
// spans have their own lock. A still-open span extends to now.
func (m *SyncMockManager) pressureActiveAtLocked(t time.Time) bool {
	ok, _ := m.pressure.Overlaps(t, t)
	return ok
}

// lostToPressureLocked reports whether AddMock drops mock for memory pressure:
// a per-test mock whose request happened while the memory guard paused
// recording. Decided by the request's time, not by whether the pause is on
// now, both ways round:
//   - a mock decoded during a pause for a request made before it belongs to a
//     test case the ingress captured, and dropping it would orphan that test;
//   - a mock decoded after a pause for a request made during it belongs to no
//     test case (the ingress did not capture one there, and the recording
//     suppresses any that overlap the pause), so keeping it would leave a mock
//     no test owns. The relay hands parsers the whole stream through a pause
//     so that the connection's capture survives it, which is what lets a
//     parser decode such a mock after the pause has ended.
//
// A mock of a connection's or the session's lifetime (a handshake, a prepared
// statement) is kept whenever its request was made: the tests the connection
// serves after the pause are recorded, and they need it. Nil and an unknown
// request time keep the mock. Caller holds m.mu.
func (m *SyncMockManager) lostToPressureLocked(mock *models.Mock) bool {
	if mock == nil || mock.TestModeInfo.Lifetime != models.LifetimePerTest {
		return false
	}
	return m.pressureActiveAtLocked(mock.Spec.ReqTimestampMock)
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
	// What pressure lets go, counted and said once mu is released: each was
	// taken by AddMock, so nothing else tells of it.
	var wiped, heldGone []*models.Mock
	if enabled {
		if !wasEnabled {
			// false→true transition: open a new pressure span.
			// memoryguard fires SetMemoryPressure(true) once per 500ms tick,
			// but only the first call (when wasEnabled is false) is a real
			// transition; subsequent ticks while pressure is held are no-ops
			// for span tracking — they would otherwise spam the spans.
			m.closePressure = m.pressure.Open(now)
		}
		// Don't wipe the whole buffer. Three classes of mock must survive:
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
		//   3. Mocks of a connection's or the session's lifetime, whenever
		//      their request was made (lostToPressureLocked).
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
			if !m.lostToPressureLocked(mk) {
				keep = append(keep, mk)
				continue
			}
			wiped = append(wiped, mk)
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
		heldGone, pressureGaveUp = m.giveUpForPressureLocked()
		heldDropped = len(heldGone)
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
	// A held mock it let go of that was held at the stale cutoff leaves out
	// the test case of a request in flight with no window whose window holds
	// it, as the cutoff's own drops do: giving up the open windows does not
	// reach that request.
	m.settleLetGo()
	// Dropped, so no longer added (SessionDrops).
	m.totalAdded.Add(-int64(len(wiped) + len(heldGone)))
	for _, mk := range wiped {
		m.reportPressureDrop(mk, string(MockDroppedUnderPressure))
	}
	for _, mk := range heldGone {
		m.reportPressureDrop(mk, heldLetGo)
	}
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

// sentClaim is a startup mock sent with no owner that a request in flight may
// take (sentClaimed): its name, when it was requested, and how its send went.
type sentClaim struct {
	name string
	at   time.Time
	// owner, if not nil, is the resolved kept window whose part after its
	// yield covers it: that window's resolve left it, or it was sent after
	// that resolve, for a request that claimed it then. If no request in
	// flight takes it, it goes back to owner (pruneSentClaimedLocked), as a
	// held mock is owed back to its window (heldMock.owner).
	owner *resolvedWindow
	// takenBy is the test whose kept resolve took it while its send had not
	// finished: a drop then costs that test its mock, and is told to it
	// (sentClaimDone). Nothing else takes it.
	takenBy string
	// sent is how its send went: claimSending until it has finished.
	sent claimSend
	// dropEpoch is the revokeEpoch of its drop (claimDropped): the test it is
	// told to later is revoked only in that epoch (recordDroppedTCFor).
	dropEpoch uint64
}

// claimSend is how the send of a startup mock noted for a claim went.
type claimSend uint8

const (
	claimSending claimSend = iota
	claimDelivered
	claimDropped
)

// maxSentClaimed bounds sentClaimed. Past it the oldest eighth is forgotten at
// once (so a note at the bound costs a copy of the list once per eighth of it,
// not at every note), and said (evictSentClaimedLocked): their mocks were
// sent, and are left out of the mapping of the request that takes them, or of
// the window they were to go back to, and a drop of one is told to no test.
const maxSentClaimed = 1 << 16

// startupClaimedLocked reports whether a request still in flight, other than
// self's, may take a startup mock no resolved window owns: it started at or
// before the mock was requested. The resolve of a request that claims it
// (openClaimLocked) takes it, or, past a request's yield, that request's own,
// which takes what no other request claims: a streaming response yields at
// its headers, and the mocks its body's handler requests are still its.
// Caller holds m.mu.
func (m *SyncMockManager) startupClaimedLocked(mk *models.Mock, self *Window) bool {
	at := mk.Spec.ReqTimestampMock
	if at.IsZero() {
		return false
	}
	for _, o := range m.open {
		if o != self && !o.start.After(at) {
			return true
		}
	}
	return false
}

// noteSentClaimedLocked is told of a startup mock a rescue sends with no owner,
// named. The startup rescues send it at once, so no reaper drops it first and
// the stop's drain does not wait on a buffer it cannot see. A request in
// flight (other than self's) may take it: noted (sentClaimed), its resolve
// adds it to its test's mapping. Mapped by no request, it reached the
// recording, and its test case reached replay without it in its mapping
// (mock-mismatch: the flush ticker fired between the parser decoding a test's
// outgoing call and the test's resolve). owner, if not nil, is the resolved
// kept window whose part after its yield covers it (sentClaim.owner). Reports
// whether it was noted: its send's end must then be told (sentClaimDone).
// An answer the client never read is never noted (mapsTo): no mapping lists
// it, and its drop costs no test a call. Caller holds m.mu.
func (m *SyncMockManager) noteSentClaimedLocked(mk *models.Mock, self *Window, owner *resolvedWindow) bool {
	if !mapsTo(mk) || !m.startupClaimedLocked(mk, self) {
		return false
	}
	if len(m.sentClaimed) >= maxSentClaimed {
		m.evictSentClaimedLocked()
	}
	at := mk.Spec.ReqTimestampMock
	i := sort.Search(len(m.sentClaimed), func(i int) bool { return at.Before(m.sentClaimed[i].at) })
	m.sentClaimed = append(m.sentClaimed, sentClaim{})
	copy(m.sentClaimed[i+1:], m.sentClaimed[i:])
	m.sentClaimed[i] = sentClaim{name: mk.Name, at: at, owner: owner}
	return true
}

// evictSentClaimedLocked forgets the oldest eighth of the notes at the bound
// (maxSentClaimed), and counts them for settleLetGo to say once mu is let go
// (sayEvicted): those a request took, or that were to go back to a window,
// are no longer followed, so a drop of one is told to no test, and one going
// back to a window is in no mapping. Caller holds m.mu.
func (m *SyncMockManager) evictSentClaimedLocked() {
	evict := m.sentClaimed[:maxSentClaimed/8]
	for _, c := range evict {
		if c.takenBy != "" || c.owner != nil {
			m.evicted.followed++
		}
	}
	n := copy(m.sentClaimed, m.sentClaimed[maxSentClaimed/8:])
	clear(m.sentClaimed[n:])
	m.sentClaimed = m.sentClaimed[:n]
	m.sentClaimedEvicted.Add(uint64(len(evict)))
	m.evicted.forgotten += len(evict)
	m.letGo.Store(true)
}

// evictedNotes is what evictSentClaimedLocked forgot since it was last said.
type evictedNotes struct{ forgotten, followed int }

// sayEvicted says e, the notes forgotten at the bound, at WARN. Never called
// under mu.
func (m *SyncMockManager) sayEvicted(e evictedNotes) {
	if e.forgotten == 0 {
		return
	}
	ce := m.dropLogger().Check(zapcore.WarnLevel, "startup mocks sent while requests that may take them were in flight are past the bound kept for them; the oldest are forgotten, and a test case that takes one of them may be saved without it in its mapping")
	if ce == nil {
		return
	}
	ce.Write(zap.Int("forgotten", e.forgotten), zap.Int("forgottenFollowed", e.followed),
		zap.Uint64("forgottenSoFar", m.sentClaimedEvicted.Load()),
		zap.Int("bound", maxSentClaimed),
		zap.String("next_step", "a request in flight since the recording's start (a long stream) keeps every startup mock after it claimable; end it, or record past the startup window, and re-record the test cases this run saved"))
}

// cutHeld is a mock the hold let go of that it held at the stale cutoff
// (heldCut), with the revokeEpoch it was let go in.
type cutHeld struct {
	mock  *models.Mock
	epoch uint64
}

// settleLetGo does, once mu is let go, what a call under it left for after:
// it leaves out the test cases whose window holds a mock the hold let go of
// that it held at the stale cutoff (heldCut), as the cutoff's own drops do
// (cutDropped), and says the startup notes forgotten at their bound
// (evictSentClaimedLocked). Under mu neither a line nor the hook is written:
// each would hold up every AddMock and resolve of the manager. Every reaper
// that may let go of the hold or forget notes calls it after its Unlock
// (settlePool, Window.Close, SetMemoryPressure, the pool's give-up report).
// One atomic load when nothing waits. Caller does not hold mu.
func (m *SyncMockManager) settleLetGo() {
	if !m.letGo.Load() {
		return
	}
	m.mu.Lock()
	held, evicted := m.heldCut, m.evicted
	m.heldCut, m.evicted = nil, evictedNotes{}
	m.letGo.Store(false)
	m.mu.Unlock()
	m.sayEvicted(evicted)
	if len(held) == 0 {
		return
	}
	bound, _ := m.outChanStatus()
	// In runs of one recording: a later one's leaves out none of an
	// earlier's test cases (leaveOutSpan checks the epoch).
	for len(held) > 0 {
		epoch, n := held[0].epoch, 1
		for n < len(held) && held[n].epoch == epoch {
			n++
		}
		cut := make([]*models.Mock, n)
		for i := range cut {
			cut[i] = held[i].mock
		}
		m.cutDropped(cut, epoch, bound && epoch == m.revokeEpoch.Load(), "", true)
		held = held[n:]
	}
}

// sentClaimRangeLocked returns the notes requested within [start, end]:
// sentClaimed[lo:hi]. Caller holds m.mu.
func (m *SyncMockManager) sentClaimRangeLocked(start, end time.Time) (lo, hi int) {
	lo = sort.Search(len(m.sentClaimed), func(i int) bool { return !m.sentClaimed[i].at.Before(start) })
	hi = sort.Search(len(m.sentClaimed), func(i int) bool { return m.sentClaimed[i].at.After(end) })
	return lo, hi
}

// takeSentClaimedLocked is a kept resolve of [start, end], for testName,
// taking the startup mocks sent for it (sentClaimed): those it would have
// taken had they waited in the buffer, as it takes held mocks
// (takeHeldLocked). A window that yielded (sharedFrom not zero) leaves those
// requested after its yield that another request claims (others), recorded as
// going back to self if that request does not take them, and those already
// going back to a window decided before it (sentClaim.owner). The names of
// those it takes are added to ids, its test's mapping, but for one whose send
// dropped it: the test is told its mock was dropped (recordDroppedTC), as for
// any mock of its own, and the name, which the recording never got, is left
// out. One still being sent is told to the test if it drops it
// (sentClaimDone). Caller holds m.mu.
func (m *SyncMockManager) takeSentClaimedLocked(start, end, sharedFrom time.Time, others claims, testName string, self func() *resolvedWindow, ids []string) []string {
	lo, hi := m.sentClaimRangeLocked(start, end)
	stay := lo
	for i := lo; i < hi; i++ {
		c := m.sentClaimed[i]
		switch {
		case c.takenBy != "":
		case !sharedFrom.IsZero() && c.at.After(sharedFrom) && (c.owner != nil || others.covers(c.at)):
			if c.owner == nil { // left in this window's part after its yield
				c.owner = self()
			}
		case c.sent == claimDropped:
			m.recordDroppedTCFor(testName, c.dropEpoch)
			continue
		default:
			ids = append(ids, c.name)
			if c.sent == claimDelivered {
				continue
			}
			c.takenBy = testName
		}
		m.sentClaimed[stay] = c
		stay++
	}
	k := stay + copy(m.sentClaimed[stay:], m.sentClaimed[hi:])
	clear(m.sentClaimed[k:])
	m.sentClaimed = m.sentClaimed[:k]
	return ids
}

// sentClaimDone is told that the send of a startup mock noted for a claim
// (noteSentClaimedLocked), named name and requested at at, has finished:
// delivered, or dropped. A test that took it already is told of a drop
// (recordDroppedTC), as for any mock of its own: its mapping names a mock the
// recording never got. Otherwise the note keeps how it went, for the test
// that takes it or the window it goes back to.
func (m *SyncMockManager) sentClaimDone(name string, at time.Time, delivered bool) {
	m.sentClaimDoneFor(name, at, delivered, m.revokeEpoch.Load())
}

// sentClaimDoneFor is sentClaimDone for a send that dropped its mock in
// revokeEpoch epoch: the drop is told to its test with a revoke only in that
// epoch.
func (m *SyncMockManager) sentClaimDoneFor(name string, at time.Time, delivered bool, epoch uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lo, hi := m.sentClaimRangeLocked(at, at)
	for i := lo; i < hi; i++ {
		c := &m.sentClaimed[i]
		if c.name != name {
			continue
		}
		if c.takenBy == "" {
			c.sent = claimDropped
			if delivered {
				c.sent = claimDelivered
			}
			c.dropEpoch = epoch
			return
		}
		if !delivered {
			m.recordDroppedTCFor(c.takenBy, epoch)
		}
		n := i + copy(m.sentClaimed[i:], m.sentClaimed[i+1:])
		clear(m.sentClaimed[n:])
		m.sentClaimed = m.sentClaimed[:n]
		return
	}
}

// pruneSentClaimedLocked forgets the startup mocks sent for a claim that no
// request in flight can take any more: none started at or before them, or,
// for one that goes back to a resolved kept window (sentClaim.owner), none
// claims it (openClaimLocked). Such a one goes back to that window once its
// send has finished: into its test's mapping, by late (as handOwedLocked hands
// a held mock back), or, dropped, told to its test (recordDroppedTC). One a
// test took is kept until its send has finished (sentClaimDone). Caller holds
// m.mu.
func (m *SyncMockManager) pruneSentClaimedLocked(late *map[string][]string) {
	k := 0
	for _, c := range m.sentClaimed {
		stays := c.takenBy != ""
		switch {
		case stays:
		case c.owner == nil:
			stays = m.openClaimsStartupLocked(c.at)
		case m.openClaimLocked(c.at, nil) || c.sent == claimSending:
			stays = true
		case m.goneLocked(c.owner.recording):
			// It would go back to a test case of a recording that has
			// ended, whose streams are no longer bound: into a mapping, or
			// told dropped, on the next recording's streams, where that
			// test's name may be another test's. Forgotten.
		case c.sent == claimDropped:
			m.recordDroppedTCFor(c.owner.testName, c.dropEpoch)
		case c.owner.mapping:
			if *late == nil {
				*late = make(map[string][]string)
			}
			(*late)[c.owner.testName] = append((*late)[c.owner.testName], c.name)
		}
		if stays {
			m.sentClaimed[k] = c
			k++
		}
	}
	clear(m.sentClaimed[k:])
	m.sentClaimed = m.sentClaimed[:k]
}

// openClaimsStartupLocked reports whether a request in flight started at or
// before at: the earliest open one did. Caller holds m.mu.
func (m *SyncMockManager) openClaimsStartupLocked(at time.Time) bool {
	w := m.earliestOpenLocked()
	return w != nil && !w.start.After(at)
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

	// The destination a kept window's test case goes to, named before mu is
	// taken: the namer is the caller's (SetDestinationOf).
	var own string
	if keep {
		own = m.ownDest()
	}

	m.mu.Lock()
	// Snapshot the outChan wiring status under outChanMu (NOT m.mu)
	// so we don't race SetOutputChannel / CloseOutChan. Only the
	// bound boolean is needed — the actual send later goes through
	// sendToOutChan which reacquires the RLock and will skip the
	// send itself when outChanClosed is true.
	outChanBound, _ := m.outChanStatus()
	// The recording whose test cases a stale cutoff's drop leaves out
	// (cutDropped): the output's as bound now.
	cutEpoch := m.revokeEpoch.Load()
	var cut []*models.Mock
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
			selfW = &resolvedWindow{start: start, end: end, testName: testName, mapping: mapping, keep: true, sharedFrom: unixNanoOrZero(sharedFrom), recording: m.recording}
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
	// key off that per-mock tag, not the live counter. It counts toward its
	// destination's too (destTCs), which a manager whose mocks go to several
	// recordings tags by.
	if keep {
		m.resolvedTestCount++
		if m.destTCs == nil {
			m.destTCs = make(map[string]int)
		}
		m.destTCs[own]++
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
		//
		// A mock reusable only by its recorder's statement (a MySQL SET,
		// BEGIN or COMMIT; models.Mock.ReusableByStatementOnly) is matched
		// as a per-test mock is: owned by its window and mapped to its test,
		// and pruned with a duplicate's window, as before lifetimes were
		// stated. The statement decides how a replay serves it.
		if attributedReusable(mock) {
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
				associatedMockIDs = withMapped(associatedMockIDs, mock)
				// Owned by THIS window's test → tag it so a capacity
				// drop suppresses testName rather than orphaning it.
				mocksToSend = append(mocksToSend, ownedMock{mock: mock, owner: testName})
			} else if ownerW, ok, shared, behind := m.ownerOrSharedLocked(mockTime); ok && m.goneLocked(ownerW.recording) {
				// A late mock of a test case of a recording that has ended,
				// whose streams are no longer bound: let go.
				m.goneLetGo.Add(1)
			} else if ok && ownerW.keep {
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
					lateMappings = withLate(lateMappings, ownerW.testName, mock)
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
				noted := m.noteSentClaimedLocked(mock, w, refs.of(shared, behind))
				// Duplicate's synthetic testName owns no real mapping →
				// owner "" (records nothing on a capacity drop).
				mocksToSend = append(mocksToSend, ownedMock{mock: mock, noted: noted})
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
		if ownerOK && m.goneLocked(ownerW.recording) {
			// A late mock of a test case of a recording that has ended,
			// whose streams are no longer bound: let go.
			m.goneLetGo.Add(1)
			continue
		}
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
					noted := m.noteSentClaimedLocked(mock, w, refs.of(shared, behind))
					mocksToSend = append(mocksToSend, ownedMock{mock: mock, noted: noted})
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
				lateMappings = withLate(lateMappings, ownerW.testName, mock)
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
			noted := m.noteSentClaimedLocked(mock, w, refs.of(shared, behind))
			// Boot/startup traffic owns no specific test → owner "".
			mocksToSend = append(mocksToSend, ownedMock{mock: mock, noted: noted})
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
			// Collected, and said, told and left out for after mu
			// (cutDropped): a request in flight with no window open
			// for it may still be decided with this mock inside its
			// window, and a line written here would hold mu once per
			// mock.
			cut = append(cut, mock)
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
				associatedMockIDs = withMapped(associatedMockIDs, mock)
				fromHold = append(fromHold, ownedMock{mock: mock, owner: testName})
			}
			heldTaken = len(fromHold)
			mocksToSend = mergeByRequestTime(mocksToSend, fromHold)
		} else {
			m.buffer = append(m.buffer, taken...)
		}
		// The request is decided and has what it owns: its window ends here,
		// not after the sends below. It is a test case: its span is noted
		// before its window leaves the open ones (TestSpanAt).
		if w != nil && w.m == m {
			if w.idx >= 0 {
				m.noteTestSpanLocked(start, end)
			}
			m.endWindowLocked(w, &tally, false)
		}
	}
	gaveUp := m.fitHoldLocked(&tally)
	lateBinned += m.handOwedLocked(outChanBound, &mocksToSend, &lateMappings)
	hold := m.holdStateLocked()

	// The startup mocks sent while this request was in flight, which it
	// takes: in its mapping too.
	if keep {
		associatedMockIDs = m.takeSentClaimedLocked(start, end, sharedFrom, others, testName, self, associatedMockIDs)
	}
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
		recording:  m.recording,
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
	// Owed before the mocks go: a stop asked between the two sends waits for
	// the mappings too (mappings_owed.go). The test's own is timed by its
	// window's start.
	var paidEntry func()
	if mappingEntry != nil {
		paidEntry = m.oweMapping(start)
	}
	paidLate := m.oweLateMappingsLocked(mappingChan != nil, lateMappings, mocksToSend)

	m.mu.Unlock()
	m.cutDropped(cut, cutEpoch, outChanBound, testName, false)
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

	// Route mock sends through send (sendTaken) so the close-vs-send
	// race is serialized the same way AddMock does it, and a capacity
	// drop is attributed to the owning TC. Mapping channel is never
	// closed by the shutdown path today — if that ever changes, lift the
	// mapping send under an equivalent guard.
	m.sendTaken(mocksToSend)
	if mappingEntry != nil && mappingChan != nil {
		m.sendMapping(mappingChan, *mappingEntry, paidEntry)
	} else if paidEntry != nil {
		paidEntry()
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
			m.sendMapping(mappingChan, models.TestMockMapping{TestName: tn, MockIDs: ids}, paidLate[tn])
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
		// owner "". One reusable only by its recorder's statement is the
		// duplicate's debris like its per-test mocks (ReusableByStatementOnly).
		if attributedReusable(mock) {
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
		if ok && m.goneLocked(w.recording) {
			// A late mock of a test case of a recording that has ended,
			// whose streams are no longer bound: let go.
			m.goneLetGo.Add(1)
			continue
		}
		if ok && w.keep {
			if !outChanBound {
				// Can't deliver yet; retain so a later flush sends it.
				m.buffer[keepIdx] = mock
				keepIdx++
				continue
			}
			mock.Name = "mock-" + generateRandomString(8)
			if w.mapping {
				lateMappings = withLate(lateMappings, w.testName, mock)
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
			noted := m.noteSentClaimedLocked(mock, nil, refs.of(shared, behind))
			// Startup/boot traffic owns no specific test → owner "".
			mocksToSend = append(mocksToSend, ownedMock{mock: mock, noted: noted})
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
	// Owed before the mocks go (mappings_owed.go).
	paidLate := m.oweLateMappingsLocked(mappingChan != nil, lateMappings, mocksToSend)
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

	// Send AFTER releasing m.mu — the send takes outChanMu and the out
	// queue's lock, and tells each mock's outcome under locks of its own.
	m.sendTaken(mocksToSend)
	if mappingChan != nil {
		for tn, ids := range lateMappings {
			if len(ids) == 0 {
				continue
			}
			m.sendMapping(mappingChan, models.TestMockMapping{TestName: tn, MockIDs: ids}, paidLate[tn])
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
