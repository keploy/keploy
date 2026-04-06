package manager

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func TestSetMemoryPressureClearsBufferedMocksAndDropsNewOnes(t *testing.T) {
	t.Parallel()

	mgr := &SyncMockManager{
		buffer: []*models.Mock{
			{
				Spec: models.MockSpec{ReqTimestampMock: time.Now()},
			},
			{
				Spec: models.MockSpec{ReqTimestampMock: time.Now()},
			},
		},
	}
	oldBuffer := mgr.buffer

	mgr.SetMemoryPressure(true)
	if len(mgr.buffer) != 0 {
		t.Fatalf("expected memory pressure to clear buffered mocks, got %d items", len(mgr.buffer))
	}
	if cap(mgr.buffer) != defaultMockBufferCapacity {
		t.Fatalf("expected buffer capacity to reset to %d, got %d", defaultMockBufferCapacity, cap(mgr.buffer))
	}
	for i, mock := range oldBuffer {
		if mock != nil {
			t.Fatalf("expected cleared buffer entry %d to be nil", i)
		}
	}

	mgr.AddMock(&models.Mock{Spec: models.MockSpec{ReqTimestampMock: time.Now()}})
	if len(mgr.buffer) != 0 {
		t.Fatalf("expected memory pressure to drop new mocks, got %d buffered items", len(mgr.buffer))
	}

	mgr.SetMemoryPressure(false)
	mgr.AddMock(&models.Mock{Spec: models.MockSpec{ReqTimestampMock: time.Now()}})
	if len(mgr.buffer) != 1 {
		t.Fatalf("expected buffer to accept mocks after recovery, got %d buffered items", len(mgr.buffer))
	}
}
