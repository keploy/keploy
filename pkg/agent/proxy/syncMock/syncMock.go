package manager

import (
	"math/rand"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
const defaultMockBufferCapacity = 100

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

// SetOutputChannel plugs a fresh outgoing mock channel into the
// manager and reopens it for sends. Re-record flows call this each
// time a new session starts. The reset of outChanClosed has to
// happen under the same lock as the outChan assignment so a
// concurrent sender always sees a consistent (outChan, closed)
// tuple.
func (m *SyncMockManager) SetOutputChannel(out chan<- *models.Mock) {
	m.outChanMu.Lock()
	m.outChan = out
	m.outChanClosed = false
	m.outChanMu.Unlock()
}

func (m *SyncMockManager) SetMappingChannel(ch chan<- models.TestMockMapping) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mappingChan = ch
}

// sendToOutChan is the single send path to outChan. Holds outChanMu
// read-lock across the whole observation + send so CloseOutChan (the
// writer-lock holder) cannot interleave a close between our
// not-closed check and the chansend. Non-blocking via default — if
// the reader has fallen behind, the mock is dropped rather than
// stalling the caller while holding the read lock (which would also
// stall every concurrent sender and block the closer).
func (m *SyncMockManager) sendToOutChan(mock *models.Mock) {
	m.outChanMu.RLock()
	defer m.outChanMu.RUnlock()
	if m.outChanClosed || m.outChan == nil {
		return
	}
	select {
	case m.outChan <- mock:
	default:
	}
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
	bound, closed := m.outChanStatus()
	switch {
	case closed:
		m.mu.Unlock()
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
	outChanBound := m.outChan != nil // snapshot for the "is wired yet" check only
	mappingChan := m.mappingChan

	// Any mock older than 7 seconds from NOW is considered dead and will be removed.
	cutoffTime := time.Now().Add(-7 * time.Second)
	keepIdx := 0

	for i := 0; i < len(m.buffer); i++ {
		mock := m.buffer[i]
		mockTime := mock.Spec.ReqTimestampMock

		// SAFETY VALVE: Expire old mocks
		// If the mock is older than 7 seconds, we discard it immediately.
		// This stops the infinite growth.
		if mockTime.Before(cutoffTime) {
			continue
		}

		// MATCHING LOGIC: Process mocks in the requested window
		if (mockTime.Equal(start) || mockTime.After(start)) && (mockTime.Equal(end) || mockTime.Before(end)) {
			if keep {
				// If output channel is not wired yet, keep matching mocks buffered
				// so they can be emitted later instead of blocking on a nil channel.
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

		// RETENTION: Keep the mock if it's recent (within 7s) but didn't match this specific window.
		// It might be matched by a future out-of-order request.
		m.buffer[keepIdx] = mock
		keepIdx++
	}

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

	m.mu.Unlock()

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
	Resolved     bool
	IsDuplicate  bool
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
func (dq *DedupQueue) ResolveJob(job *DedupJob, isDuplicate bool, resTimestamp time.Time, enableMapping bool, mockMgr *SyncMockManager) {
	dq.mu.Lock()
	defer dq.mu.Unlock()

	job.IsDuplicate = isDuplicate
	job.Resolved = true
	job.ResTimestamp = resTimestamp

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
			mockMgr.ResolveRange(head.ReqTimestamp, head.ResTimestamp, "", true, enableMapping)

		}

		dq.queue = dq.queue[1:]
	}
}
