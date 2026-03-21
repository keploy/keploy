package manager

import (
	"math/rand"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func generateRandomString(n int) string {
	sb := make([]byte, n)
	for i := range sb {
		sb[i] = charset[rand.Intn(len(charset))]
	}
	return string(sb)
}

type SyncMockManager struct {
	mu           sync.Mutex
	buffer       []*models.Mock
	outChan      chan<- *models.Mock
	mappingChan  chan<- models.TestMockMapping
	firstReqSeen bool
}

// Global instance is initialized at package load time
var instance = &SyncMockManager{
	buffer:       make([]*models.Mock, 0, 100),
	firstReqSeen: false,
}

// Get returns the global manager.
func Get() *SyncMockManager {
	return instance
}

// SetOutputChannel allows the Outgoing Proxy to "plug in" the channel later.
func (m *SyncMockManager) SetOutputChannel(out chan<- *models.Mock) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outChan = out
}

func (m *SyncMockManager) SetMappingChannel(ch chan<- models.TestMockMapping) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mappingChan = ch
}

func (m *SyncMockManager) AddMock(mock *models.Mock) {
	m.mu.Lock()

	// storing startup mocks until first request is seen
	if !m.firstReqSeen && m.outChan != nil {
		outChan := m.outChan
		m.mu.Unlock()
		// Send outside the lock to avoid deadlock: if the channel is
		// full, holding mu would block AddMock callers and any
		// ResolveRange call trying to flush the buffer.
		outChan <- mock
		return
	}
	m.buffer = append(m.buffer, mock)
	m.mu.Unlock()
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
func (m *SyncMockManager) ResolveRange(start, end time.Time, testName string, keep bool, mapping bool) {
	// Collect mocks and mapping data under the lock, then send to channels
	// AFTER releasing it. Sending while holding mu can deadlock: the channel
	// consumer (e.g. gob encoder in HandleOutgoing) may block, and a
	// concurrent AddMock caller waiting on mu would stall the pipeline.
	var mocksToSend []*models.Mock
	var associatedMockIDs []string
	var mappingEntry *models.TestMockMapping

	m.mu.Lock()

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

	if len(associatedMockIDs) > 0 && m.mappingChan != nil && mapping {
		mappingEntry = &models.TestMockMapping{
			TestName: testName,
			MockIDs:  associatedMockIDs,
		}
	}

	outChan := m.outChan
	mappingChan := m.mappingChan
	m.mu.Unlock()

	// Send mocks and mapping outside the lock to avoid deadlock.
	for _, mock := range mocksToSend {
		outChan <- mock
	}
	if mappingEntry != nil && mappingChan != nil {
		mappingChan <- *mappingEntry
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
