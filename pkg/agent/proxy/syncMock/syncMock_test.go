package manager

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func TestSetMemoryPressureDropsNewMocksWithoutClearingBuffer(t *testing.T) {
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

	mgr.SetMemoryPressure(true)
	if len(mgr.buffer) != 2 {
		t.Fatalf("expected existing buffer to be preserved, got %d items", len(mgr.buffer))
	}

	mgr.AddMock(&models.Mock{Spec: models.MockSpec{ReqTimestampMock: time.Now()}})
	if len(mgr.buffer) != 2 {
		t.Fatalf("expected memory pressure to drop new mocks, got %d buffered items", len(mgr.buffer))
	}

	mgr.SetMemoryPressure(false)
	mgr.AddMock(&models.Mock{Spec: models.MockSpec{ReqTimestampMock: time.Now()}})
	if len(mgr.buffer) != 3 {
		t.Fatalf("expected buffer to accept mocks after recovery, got %d buffered items", len(mgr.buffer))
	}
}
