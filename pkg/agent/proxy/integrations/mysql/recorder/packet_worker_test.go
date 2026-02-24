package recorder

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

func TestProcessRawMocks(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rawMocks := make(chan *models.Mock, 10)
	finalMocks := make(chan *models.Mock, 10)

	// Start worker
	go ProcessRawMocks(ctx, logger, rawMocks, finalMocks)

	// Test Case 1: Config mock (no rows)
	configMock := &models.Mock{
		Name: "config",
		Spec: models.MockSpec{
			MySQLResponses: []mysql.Response{
				{
					PacketBundle: mysql.PacketBundle{
						Message: &mysql.HandshakeV10Packet{},
					},
				},
			},
		},
	}

	rawMocks <- configMock

	select {
	case m := <-finalMocks:
		if m != configMock {
			t.Errorf("Expected configMock to pass through, got %v", m)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for config mock")
	}

	// Test Case 2: Mock with RawRowData (invalid data, should log error but pass through)
	rawRowMock := &models.Mock{
		Name: "mocks",
		Spec: models.MockSpec{
			MySQLResponses: []mysql.Response{
				{
					PacketBundle: mysql.PacketBundle{
						Message: &mysql.TextResultSet{
							RawRowData: [][]byte{{0x01, 0x02}}, // Invalid data
							Columns:    []*mysql.ColumnDefinition41{},
						},
					},
				},
			},
		},
	}

	rawMocks <- rawRowMock

	select {
	case m := <-finalMocks:
		if m != rawRowMock {
			t.Errorf("Expected rawRowMock to pass through, got %v", m)
		}
		// Verification: RawRowData should be nil after processing
		resp := m.Spec.MySQLResponses[0].PacketBundle.Message.(*mysql.TextResultSet)
		if resp.RawRowData != nil {
			t.Errorf("Expected RawRowData to be nil (cleared), got %v", resp.RawRowData)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for raw row mock")
	}
}
