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
	FirstReqSeen bool
}

// Global instance is initialized at package load time
var instance = &SyncMockManager{
	buffer:       make([]*models.Mock, 0, 100),
	FirstReqSeen: false,
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
	if !m.FirstReqSeen && m.outChan != nil {
		m.outChan <- mock
		return
	}
	m.buffer = append(m.buffer, mock)
}

func (m *SyncMockManager) SetFirstRequestSignaled() {
	m.mu.Lock()
	m.FirstReqSeen = true
	m.mu.Unlock()
}

func (m *SyncMockManager) ResolveRange(start, end time.Time, keep bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Process in-place to avoid allocations
	keepIdx := 0

	for i := 0; i < len(m.buffer); i++ {
		mock := m.buffer[i]
		mockTime := mock.Spec.ReqTimestampMock

		// Mocks WITHIN this request window:
		if (mockTime.Equal(start) || mockTime.After(start)) && (mockTime.Equal(end) || mockTime.Before(end)) {
			if keep {
				m.outChan <- mock
			}
			// We skip these, effectively discarding them from the slice
			continue
		}

		// Mocks AFTER this range (Future Mocks):
		// Shift them to the front. This operation is fast because it is just a pointer copy.
		m.buffer[keepIdx] = mock
		keepIdx++
	}

	// Nil out the tail to prevent memory leaks.
	// This allows garbage collector to reclaim the underlying mock data immediately.
	for i := keepIdx; i < len(m.buffer); i++ {
		m.buffer[i] = nil
	}

	m.buffer = m.buffer[:keepIdx]
}
