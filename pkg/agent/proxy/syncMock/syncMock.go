package manager

import (
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

type SyncMockManager struct {
	mu           sync.Mutex
	buffer       []*models.Mock
	outChan      chan<- *models.Mock
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

func (m *SyncMockManager) AddMock(mock *models.Mock) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// storing startup mocks until first request is seen
	if !m.firstReqSeen && m.outChan != nil {
		m.outChan <- mock
		return
	}
	m.buffer = append(m.buffer, mock)
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
func (m *SyncMockManager) ResolveRange(start, end time.Time, keep bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

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
				m.outChan <- mock
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
}
