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
		// Don't block on channel send - dispatch async to prevent parser blocking.
		// The parser must drain the ring buffer for the forwarder to continue,
		// so blocking here creates back-pressure to the network forwarding path.
		// Goroutine overhead (~2-3μs) is negligible compared to 40ms ACK delay.
		m.mu.Unlock()
		go func() { m.outChan <- mock }()
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
func (m *SyncMockManager) ResolveRange(start, end time.Time, testName string, keep bool) {
	m.mu.Lock()

	// Any mock older than 7 seconds from NOW is considered dead and will be removed.
	cutoffTime := time.Now().Add(-7 * time.Second)
	var associatedMockIDs []string
	// Collect mocks to send outside the lock to avoid blocking the parser
	// goroutine (which must drain the ring buffer for forwarding to continue).
	var mocksToSend []*models.Mock
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

	// Reslice the buffer while still holding the lock
	m.buffer = m.buffer[:keepIdx]

	// Capture channels before releasing lock
	outCh := m.outChan
	mapCh := m.mappingChan
	m.mu.Unlock()

	// Send mocks and mappings outside the lock to prevent blocking the parser.
	// Channel sends may block if the consumer is slow, so we must not hold
	// the mutex during these operations.
	for _, mock := range mocksToSend {
		outCh <- mock
	}

	if len(associatedMockIDs) > 0 && mapCh != nil {
		mapping := models.TestMockMapping{
			TestName: testName,
			MockIDs:  associatedMockIDs,
		}
		mapCh <- mapping
	}
}
