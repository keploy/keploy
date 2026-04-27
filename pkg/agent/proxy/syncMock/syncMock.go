package manager

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
const defaultMockBufferCapacity = 100

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
	// mu guards buffer, firstReqSeen, memoryPause, mappingChan.
	mu           sync.Mutex
	buffer       []*models.Mock
	mappingChan  chan<- models.TestMockMapping
	firstReqSeen bool
	memoryPause  bool

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

	// loggerMu guards logger so SetLogger and the drop path can run
	// concurrently without a data race. The read lock is taken only
	// on the (sampled, cold) Error path, so contention is negligible.
	loggerMu sync.RWMutex
	logger   *zap.Logger
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
		if n == 1 || n%sendDropSampleRate == 0 {
			m.dropLogger().Error(
				"syncMock outChan overflow; mock dropped — raise consumer throughput or increase outChan capacity",
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
		m.mu.Unlock()
		return
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
		// Per-mock diagnostic: visible signal when AddMock drops a
		// mock because the outChan has already been closed by
		// CloseOutChan. This usually only fires during shutdown but
		// in CI a poorly-ordered teardown can race the recorder's
		// final emit and silently lose mocks captured in the last
		// few milliseconds of the run. The dropLogger is the right
		// receiver — it ALWAYS resolves to a non-nil logger and is
		// safe under the m.mu unlock.
		if logger := m.dropLogger(); logger != nil {
			logger.Debug("AddMock: outChan already closed, mock dropped",
				zap.String("mock_kind", string(mock.Kind)),
				zap.String("connID", mock.ConnectionID),
				zap.String("lifetime", mock.TestModeInfo.Lifetime.String()),
				zap.Time("mock_req_ts", mock.Spec.ReqTimestampMock),
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

// CloseOutChan closes the outgoing mock channel under the writer
// lock so an in-flight sendToOutChan cannot race the close.
// Idempotent; safe to call with outChan still nil.
func (m *SyncMockManager) CloseOutChan() {
	if m == nil {
		return
	}
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

func (m *SyncMockManager) SetMemoryPressure(enabled bool) {
	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.memoryPause = enabled
	if !enabled {
		return
	}

	for i := range m.buffer {
		m.buffer[i] = nil
	}
	m.buffer = make([]*models.Mock, 0, defaultMockBufferCapacity)
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
				logger.Debug("ResolveRange: stale-cutoff drop (out-of-window per-test mock older than 7s)",
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
		logger.Debug("ResolveRange: buffer transition",
			zap.String("test_name", testName),
			zap.Time("window_start", start),
			zap.Time("window_end", end),
			zap.Int("buffer_len_before", bufferLenBefore),
			zap.Int("buffer_len_after", bufferLenAfter),
			zap.Int("mocks_flushed", mocksToSendLen),
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
}

func (m *SyncMockManager) DeleteMocksStrictlyBefore(timestamp time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	keepIdx := 0
	for i := 0; i < len(m.buffer); i++ {
		mock := m.buffer[i]

		if mock.Spec.ReqTimestampMock.Before(timestamp) {
			continue
		}

		// Keep the mock
		m.buffer[keepIdx] = mock
		keepIdx++
	}

	// Memory Cleanup: Nil out the deleted entries to allow GC to reclaim the memory
	for i := keepIdx; i < len(m.buffer); i++ {
		m.buffer[i] = nil
	}

	// Reslice the buffer
	m.buffer = m.buffer[:keepIdx]
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
