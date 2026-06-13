package manager

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// mockSummary returns a short human-readable description of the mock's
// operation, used in pressure-drop and teardown logs so you can identify
// which SQL query / mongo operation was lost without reading a full YAML.
//
//	MySQL  → "MySQL/mocks COM_QUERY: SELECT * FROM orders LIMIT 10"
//	Mongo  → "Mongo/mocks kind=Mongo ts=18:43:16.357"
//	Other  → "Http/mock-42"
func mockSummary(mock *models.Mock) string {
	if mock == nil {
		return "<nil>"
	}
	kind := string(mock.Kind)
	name := mock.Name
	switch mock.Kind {
	case models.MySQL:
		if len(mock.Spec.MySQLRequests) > 0 {
			req := mock.Spec.MySQLRequests[0]
			// PacketBundle.Header is *PacketInfo; PacketInfo.Type is the packet type string.
			hdrType := ""
			if req.Header != nil {
				hdrType = req.Header.Type
			}
			// Extract query text from the Message field (COM_QUERY, COM_STMT_PREPARE)
			queryStr := ""
			if m, ok := req.Message.(map[string]interface{}); ok {
				if q, ok := m["query"].(string); ok && q != "" {
					queryStr = q
				}
			}
			if queryStr == "" {
				queryStr = mock.Spec.Metadata["requestOperation"]
			}
			if len(queryStr) > 120 {
				queryStr = queryStr[:120] + "…"
			}
			if queryStr != "" {
				return fmt.Sprintf("MySQL/%s %s: %s", name, hdrType, queryStr)
			}
			return fmt.Sprintf("MySQL/%s %s", name, hdrType)
		}
		return fmt.Sprintf("MySQL/%s", name)
	case models.Mongo:
		// Mongo mocks come from the external integrations module; the Spec
		// carries MongoRequests but the operation is buried in BSON payload.
		// Log the connection ID and timestamp so the drop can be correlated
		// to a TC by timestamp even without decoding the BSON.
		ts := ""
		if !mock.Spec.ReqTimestampMock.IsZero() {
			ts = mock.Spec.ReqTimestampMock.UTC().Format("15:04:05.000")
		}
		conn := mock.ConnectionID
		if ts != "" && conn != "" {
			return fmt.Sprintf("Mongo/%s conn=%s req_ts=%s", name, conn, ts)
		}
		if ts != "" {
			return fmt.Sprintf("Mongo/%s req_ts=%s", name, ts)
		}
		return fmt.Sprintf("Mongo/%s", name)
	default:
		return fmt.Sprintf("%s/%s", kind, name)
	}
}

// pressureDropSummary builds the one-line log for a pressure-drop event.
// It includes the mock summary plus the running counters so you can see
// exactly what was lost and how bad the pressure situation is.
func pressureDropSummary(mock *models.Mock, dropped, added int64) string {
	return fmt.Sprintf("[DROP #%d/%d] %s", dropped, added, mockSummary(mock))
}

// tcCorrHint returns a short string suggesting which TC(s) the dropped
// mock is likely associated with. Since we don't track TC→mock mapping at
// AddMock time, we use req_ts of the mock relative to the current clock to
// give the log reader a timestamp anchor for cross-referencing the TC yaml.
func tcCorrHint(mock *models.Mock) string {
	if mock == nil {
		return ""
	}
	if !mock.Spec.ReqTimestampMock.IsZero() {
		return fmt.Sprintf("TC correlation: check mappings.yaml for TCs whose req_ts ≈ %s",
			mock.Spec.ReqTimestampMock.UTC().Format("15:04:05.000"))
	}
	return fmt.Sprintf("TC correlation: check mappings.yaml for TCs around now (ts_ms=%d)",
		time.Now().UnixMilli())
}

const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
const defaultMockBufferCapacity = 100

// maxRecentWindows bounds the recently-resolved-window ring (see
// SyncMockManager.recentWindows). The ring is also time-pruned to the
// 7 s staleness horizon, so this cap only matters under a burst of very
// short test windows; 256 covers far more than the ~7 s of history the
// staleness cutoff keeps reachable, while staying O(1) memory.
const maxRecentWindows = 256

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
	// recentWindows.
	mu           sync.Mutex
	buffer       []*models.Mock
	mappingChan  chan<- models.TestMockMapping
	firstReqSeen bool
	memoryPause  bool

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

	// dropCount tracks send-path drops caused by outChan being full
	// past the bounded send budget. Sampled to an Error so customers
	// get a loud signal without the log-flood anti-pattern. Using
	// the typed atomic.Uint64 wrapper removes the 32-bit-alignment
	// footgun that a bare uint64 + sync/atomic.AddUint64 would carry
	// if this struct ever got embedded or reordered.
	dropCount atomic.Uint64

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
	// [HTTPReq.Timestamp, HTTPResp.Timestamp] window. Using pressure ranges
	// instead of per-mock drop timestamps is RACE-FREE — memoryguard records
	// the start the instant it flips memoryPause, BEFORE any mock-parser
	// goroutine sees the result; so even if the paired AddMock fires AFTER
	// the TC has reached routes/record.go, the overlap is already visible.
	pressureRanges []pressureRange

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

// Global instance is initialized at package load time
var instance = &SyncMockManager{
	buffer:       make([]*models.Mock, 0, defaultMockBufferCapacity),
	firstReqSeen: false,
}

// Get returns the global manager.
func Get() *SyncMockManager {
	return instance
}

// ShutdownProbe is a point-in-time snapshot of the SyncMockManager
// returned by ShutdownSnapshot(). All fields are numeric so callers
// can format it with fmt.Fprintf to stderr (bypassing zap) when they
// need a SIGTERM-survivable log line.
type ShutdownProbe struct {
	BufferLen          int    // mocks parked in the windowing buffer
	OutChanLen         int    // mocks queued for the live HTTP stream
	OutChanCap         int    // outChan buffer capacity
	OutChanBound       bool   // SetOutputChannel has fired
	OutChanClosed      bool   // CloseOutChan has fired
	TotalAdded         int64  // lifetime AddMock successes (post-dedup, post-pressure)
	PressureDropped    int64  // mocks dropped by memory-pressure gate (BEFORE TotalAdded)
	SendDropsTotal     uint64 // mocks dropped by sendToOutChan overflow (AFTER TotalAdded)
	OutChanClosedDrops int64  // mocks dropped because outChan already closed (AFTER TotalAdded)
	FirstReqSeen       bool   // windowing has started
	RecentWindows      int    // resolved windows still tracked
}

// ShutdownSnapshot returns a best-effort read of the manager's state.
// Designed for the SIGTERM probe registered via
// utils.RegisterPreCancelHook — must be cheap and never block. Takes
// the manager's mutexes briefly; safe to call concurrently with
// AddMock / sendToOutChan because all reads are guarded.
//
// Returns the zero value if m is nil.
func (m *SyncMockManager) ShutdownSnapshot() ShutdownProbe {
	if m == nil {
		return ShutdownProbe{}
	}
	var snap ShutdownProbe
	m.mu.Lock()
	snap.BufferLen = len(m.buffer)
	snap.FirstReqSeen = m.firstReqSeen
	snap.RecentWindows = len(m.recentWindows)
	m.mu.Unlock()
	m.outChanMu.RLock()
	if m.outChan != nil {
		snap.OutChanLen = len(m.outChan)
		snap.OutChanCap = cap(m.outChan)
		snap.OutChanBound = true
	}
	snap.OutChanClosed = m.outChanClosed
	m.outChanMu.RUnlock()
	snap.TotalAdded = m.totalAdded.Load()
	snap.PressureDropped = m.pressureDropped.Load()
	snap.SendDropsTotal = m.dropCount.Load()
	snap.OutChanClosedDrops = m.outChanClosedDrops.Load()
	return snap
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
	m.outChanMu.RLock()
	defer m.outChanMu.RUnlock()
	if m.outChanClosed || m.outChan == nil {
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

// DropCount exposes the cumulative drop counter for tests and
// external observability. The value is monotonically increasing;
// readers that need a delta should snapshot and diff.
func (m *SyncMockManager) DropCount() uint64 {
	if m == nil {
		return 0
	}
	return m.dropCount.Load()
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
		// Unknown timestamp → keep (safer than orphaning a captured TC).
		reqTime := mock.Spec.ReqTimestampMock
		if !reqTime.IsZero() && m.pressureActiveAtLocked(reqTime) {
			m.mu.Unlock()
			totalDropped := m.pressureDropped.Add(1)
			totalAdded := m.totalAdded.Load()
			if logger := m.dropLogger(); logger != nil {
				logger.Info("agent: mock dropped by memory pressure",
					zap.String("dropped_mock", pressureDropSummary(mock, totalDropped, totalAdded)),
					zap.String("tc_correlation", tcCorrHint(mock)),
					zap.String("mock_kind", string(mock.Kind)),
					zap.Time("mock_req_time", mock.Spec.ReqTimestampMock),
					zap.Int64("ts_ms", time.Now().UnixMilli()),
					zap.Int64("mocks_dropped_total", totalDropped),
					zap.Int64("mocks_added_total", totalAdded),
				)
			}
			return
		}
		// Request was during calm (or timestamp unknown) → its TC was captured
		// → keep this mock; fall through to the normal buffer/forward path.
	}
	// Mock is being kept — count it as successfully added.
	m.totalAdded.Add(1)

	// Tag app-bootstrap traffic. Any mock captured before the first
	// inbound request is startup/init traffic (e.g. an AWS Secret Manager
	// fetch at boot) that ran before any test window exists. It can never
	// claim a per-test window, so the reapers below (dedup DeleteMocks-
	// StrictlyBefore, the ResolveRange stale-cutoff, FlushOwnedWindows and
	// the memory-pressure wipe) would otherwise drop it. firstReqSeen,
	// flipped on the first inbound request, is the boot-vs-test boundary;
	// tagging here is the sole signal isStartupMock keys off.
	if mock != nil && !m.firstReqSeen {
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
			logger.Info("diag/AddMock: outChan already closed, mock dropped",
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

	var mocksToSend []*models.Mock
	var lateMappings map[string][]string

	m.mu.Lock()
	outChanBound, _ := m.outChanStatus()
	if !outChanBound {
		m.mu.Unlock()
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
			// matching ResolveRange's lifetime carve-out.
			mocksToSend = append(mocksToSend, mock)
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
			mocksToSend = append(mocksToSend, mock)
			continue
		}
		// STARTUP RESCUE: app-bootstrap traffic owns no window (it ran
		// before any test) so the ownerWindow check above never claims it.
		// Flush it to disk proactively on the ticker rather than leaving it
		// parked in the buffer where a dedup cleanup, stale-cutoff, or
		// memory-pressure wipe could reap it before it is ever persisted.
		if isStartupMock(mock) {
			mock.Name = "mock-" + generateRandomString(8)
			mocksToSend = append(mocksToSend, mock)
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
	for _, mock := range mocksToSend {
		m.sendToOutChan(mock)
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

// PressureRangesUnixMilli returns every recorded range as a [startMs, endMs]
// pair in Unix milliseconds. A still-open range reports endMs = -1.
// Diagnostics only — used to dump the actual ranges to the log so overlap
// math can be verified by hand against a TC window.
func (m *SyncMockManager) PressureRangesUnixMilli() [][2]int64 {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][2]int64, 0, len(m.pressureRanges))
	for _, r := range m.pressureRanges {
		endMs := int64(-1)
		if !r.end.IsZero() {
			endMs = r.end.UnixMilli()
		}
		out = append(out, [2]int64{r.start.UnixMilli(), endMs})
	}
	return out
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
		// Don't wipe the whole buffer: a mock whose request happened during
		// calm belongs to an already-captured TC and must survive (else replay
		// orphans it). Drop only mocks whose request was during pressure — the
		// ingress never captured those, so there is no TC to orphan. Unknown
		// timestamp → keep (safe default). In-place filter, then nil the tail.
		before := len(m.buffer)
		keep := m.buffer[:0]
		for _, mk := range m.buffer {
			if mk == nil {
				continue
			}
			// Startup mocks (captured before the first inbound request) own no
			// test window and must reach disk via FlushOwnedWindows — never wipe them.
			if isStartupMock(mk) {
				keep = append(keep, mk)
				continue
			}
			if rt := mk.Spec.ReqTimestampMock; rt.IsZero() || !m.pressureActiveAtLocked(rt) {
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
	m.mu.Unlock() // NEVER hold mu while logging — logging inside a lock causes a deadlock under I/O pressure (see BUG 5: 70-minute CI hang)

	// Only log on state TRANSITIONS to avoid flooding logs on every 500ms memoryguard tick.
	// Pressure can fire hundreds of times per second — we only want to see the moment it starts and clears.
	if logger := m.dropLogger(); logger != nil {
		if enabled && !wasEnabled {
			// Pressure just turned ON (false→true transition)
			logger.Info("agent: memory pressure activated — pressure-request mocks dropped, calm-captured kept",
				zap.Int("mocks_cleared_from_buffer", clearedFromBuffer),
				zap.Int64("mocks_dropped_so_far", m.pressureDropped.Load()),
				zap.Int64("mocks_added_so_far", m.totalAdded.Load()),
			)
		} else if !enabled && wasEnabled {
			logger.Info("agent: memory pressure cleared",
				zap.Int64("mocks_dropped_total", m.pressureDropped.Load()),
				zap.Int64("mocks_added_total", m.totalAdded.Load()),
			)
		}
	}
}

// isStartupMock reports whether a buffered per-test mock is app-bootstrap
// traffic that can never legitimately claim a test window: it was captured
// before the first inbound request (tagged IsStartup at ingest in AddMock).
// Such mocks (e.g. an AWS Secret Manager fetch at boot) must be flushed to
// disk so replay's startup tier can serve them, NOT reaped as duplicate
// debris or stale-cutoff. The tag — rather than a "request pre-dates the
// first window" timestamp test — is deliberately the only signal: a mock
// captured before the FIRST inbound request is unambiguously startup
// traffic, whereas a per-test mock that merely lands before its window
// (genuine stale cross-test bleed) must still be reaped to bound buffer
// growth. Conflating the two would resurrect that bleed.
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
	var mocksToSend []*models.Mock
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
			// per-test matches).
			mocksToSend = append(mocksToSend, mock)
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
				mocksToSend = append(mocksToSend, mock)
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
				mocksToSend = append(mocksToSend, mock)
				lateBinned++
			}
			// Handled (flushed or retained); drop from the current buffer.
			continue
		}

		// STARTUP RESCUE: a mock captured before the first inbound request
		// (IsStartup) is app-bootstrap traffic — an AWS Secret Manager
		// fetch, DB/driver handshake, config load, etc. fired at boot. It
		// can never claim a window, so the stale-cutoff below would
		// silently reap it (the "present in some test sets, missing in
		// others" bug). Flush it to disk instead so replay's startup tier
		// can serve it. Placed BEFORE the cutoff so a slow boot (>7 s to
		// the first request) can't lose it. A per-test mock that merely
		// lands before the current window (genuine stale cross-test bleed)
		// is NOT tagged IsStartup and still falls to the cutoff.
		if isStartupMock(mock) {
			if !outChanBound {
				m.buffer[keepIdx] = mock
				keepIdx++
				continue
			}
			mock.Name = "mock-" + generateRandomString(8)
			mocksToSend = append(mocksToSend, mock)
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
				logger.Info("diag/ResolveRange: stale-cutoff drop (out-of-window per-test mock older than 7s)",
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
		logger.Info("diag/ResolveRange: buffer transition",
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

	// Route mock sends through sendToOutChan so the close-vs-send
	// race is serialized the same way AddMock does it. Mapping
	// channel is never closed by the shutdown path today — if that
	// ever changes, lift the mapping send under an equivalent guard.
	for _, mock := range mocksToSend {
		m.sendToOutChan(mock)
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

	var mocksToSend []*models.Mock
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
		// otherwise retain for a later drain.
		if lt := mock.TestModeInfo.Lifetime; lt == models.LifetimeSession || lt == models.LifetimeConnection {
			if outChanBound {
				mocksToSend = append(mocksToSend, mock)
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
			mocksToSend = append(mocksToSend, mock)
			continue
		}

		// STARTUP RESCUE: app-bootstrap traffic (captured before the first
		// inbound request, or pre-dating the first resolved test window) is
		// NOT the skipped duplicate's debris — it owns no window because it
		// ran before any test existed. Without this, a once-per-boot init
		// call (e.g. AWS Secret Manager) is dropped here whenever the first
		// inbound request happens to hash as a dedup duplicate (recentWindows
		// is still empty, so the ownerWindow rescue above can't save it) —
		// the root of the flaky per-test-set capture. Flush it instead.
		if isStartupMock(mock) {
			if !outChanBound {
				m.buffer[keepIdx] = mock
				keepIdx++
				continue
			}
			mock.Name = "mock-" + generateRandomString(8)
			mocksToSend = append(mocksToSend, mock)
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
	for _, mock := range mocksToSend {
		m.sendToOutChan(mock)
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
