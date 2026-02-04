// This test is intended to be run with:
//   go test -race ./pkg/agent/proxy
//
// It reproduces a TOCTOU (Time-Of-Check-Time-Of-Use) race condition between
// GetFilteredMocksByKind/GetUnFilteredMocksByKind (readers) and SetFilteredMocks
// (writers). The race occurs because tree pointers are read under treesMu.RLock()
// but used after the lock is released, while writers can replace the entire map.
// This can lead to stale tree access and data inconsistency.
package proxy

import (
	"fmt"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestMockManager_GetFilteredMocks_Race(t *testing.T) {
	manager := NewMockManager(nil, nil, zap.NewNop())
	var wg sync.WaitGroup

	// Start multiple reader goroutines calling GetFilteredMocksByKind
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				_, _ = manager.GetFilteredMocksByKind(models.REDIS)
			}
		}()
	}

	// Start multiple writer goroutines calling SetFilteredMocks
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(iter int) {
			defer wg.Done()
			mocks := []*models.Mock{
				{
					Kind: models.REDIS,
					Name: fmt.Sprintf("mock-%d", iter),
					TestModeInfo: models.TestModeInfo{
						ID:         iter,
						IsFiltered: true,
						SortOrder:  int64(iter),
					},
				},
			}
			manager.SetFilteredMocks(mocks)
		}(i)
	}

	wg.Wait()
}

func TestMockManager_GetUnFilteredMocks_Race(t *testing.T) {
	manager := NewMockManager(nil, nil, zap.NewNop())
	var wg sync.WaitGroup

	// Start multiple reader goroutines calling GetUnFilteredMocksByKind
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				_, _ = manager.GetUnFilteredMocksByKind(models.REDIS)
			}
		}()
	}

	// Start multiple writer goroutines calling SetUnFilteredMocks
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(iter int) {
			defer wg.Done()
			mocks := []*models.Mock{
				{
					Kind: models.REDIS,
					Name: fmt.Sprintf("mock-%d", iter),
					TestModeInfo: models.TestModeInfo{
						ID:         iter,
						IsFiltered: false,
						SortOrder:  int64(iter),
					},
				},
			}
			manager.SetUnFilteredMocks(mocks)
		}(i)
	}

	wg.Wait()
}

