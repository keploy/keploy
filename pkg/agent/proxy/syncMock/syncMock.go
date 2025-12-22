package manager

import (
	"context"
	"fmt"
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

type contextKey struct{}

var mockManagerKey = contextKey{}

// Context Helpers
func GetMockManager(ctx context.Context) *SyncMockManager {
	if val, ok := ctx.Value(mockManagerKey).(*SyncMockManager); ok {
		return val
	}
	// Fallback to shared instance if not in context
	return instance
}

func WithMockManager(ctx context.Context, mgr *SyncMockManager) context.Context {
	return context.WithValue(ctx, mockManagerKey, mgr)
}

var (
	instance *SyncMockManager
	once     sync.Once
)

// GetSharedManager ensures both proxies see the exact same object.
func GetSharedManager(outChan chan<- *models.Mock) *SyncMockManager {
	if outChan != nil {
		once.Do(func() {
			instance = &SyncMockManager{
				buffer:  make([]*models.Mock, 0, 100),
				outChan: outChan,
			}
		})
	}
	return instance
}

func ResetSharedManager() {
	instance = nil
	once = sync.Once{}
}

func (m *SyncMockManager) AddMock(mock *models.Mock) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.firstReqSeen {
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
				fmt.Println("storing mock :", mock.Name)
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
