package recorder

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

func TestRecordMock_PopulatesQueryMetadata(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "conn-1")
	mocksChan := make(chan *models.Mock, 16)
	mgr := syncMock.New(zap.NewNop())
	mgr.SetOutputChannel(mocksChan)
	ctx = syncMock.NewContext(ctx, mgr)

	dctx := &wire.DecodeContext{
		StmtIDToQuery: map[uint32]string{
			77: "INSERT INTO orders (id) VALUES (?)",
			88: "SELECT id FROM users WHERE id = ?",
		},
	}

	opts := models.OutgoingOptions{DstCfg: &models.ConditionalDstCfg{Addr: "127.0.0.1:3306"}}

	t.Run("COM_STMT_EXECUTE gets query from decodeCtx", func(t *testing.T) {
		req := []mysql.Request{{
			PacketBundle: mysql.PacketBundle{
				Header: &mysql.PacketInfo{Type: "COM_STMT_EXECUTE"},
				Message: &mysql.StmtExecutePacket{
					StatementID: 77,
				},
			},
		}}
		recordMock(ctx, req, nil, "mocks", "COM_STMT_EXECUTE", "OK", nil, t0, t0, opts, dctx)
		m := <-mocksChan
		if m.Spec.Metadata["query"] != "INSERT INTO orders (id) VALUES (?)" {
			t.Errorf("expected query in metadata, got %q", m.Spec.Metadata["query"])
		}
	})

	t.Run("COM_STMT_CLOSE gets query from decodeCtx", func(t *testing.T) {
		req := []mysql.Request{{
			PacketBundle: mysql.PacketBundle{
				Header: &mysql.PacketInfo{Type: "COM_STMT_CLOSE"},
				Message: &mysql.StmtClosePacket{
					StatementID: 88,
				},
			},
		}}
		recordMock(ctx, req, nil, "mocks", "COM_STMT_CLOSE", "NO Response Packet", nil, t0, t0, opts, dctx)
		m := <-mocksChan
		if m.Spec.Metadata["query"] != "SELECT id FROM users WHERE id = ?" {
			t.Errorf("expected query in metadata, got %q", m.Spec.Metadata["query"])
		}
	})

	t.Run("COM_QUERY gets query from QueryPacket", func(t *testing.T) {
		req := []mysql.Request{{
			PacketBundle: mysql.PacketBundle{
				Header: &mysql.PacketInfo{Type: "COM_QUERY"},
				Message: &mysql.QueryPacket{
					Query: "SELECT 1",
				},
			},
		}}
		recordMock(ctx, req, nil, "mocks", "COM_QUERY", "OK", nil, t0, t0, opts)
		m := <-mocksChan
		if m.Spec.Metadata["query"] != "SELECT 1" {
			t.Errorf("expected query in metadata, got %q", m.Spec.Metadata["query"])
		}
	})

	t.Run("COM_STMT_PREPARE gets query from StmtPreparePacket", func(t *testing.T) {
		req := []mysql.Request{{
			PacketBundle: mysql.PacketBundle{
				Header: &mysql.PacketInfo{Type: "COM_STMT_PREPARE"},
				Message: &mysql.StmtPreparePacket{
					Query: "SELECT 2",
				},
			},
		}}
		recordMock(ctx, req, nil, "connection", "COM_STMT_PREPARE", "COM_STMT_PREPARE_OK", nil, t0, t0, opts)
		m := <-mocksChan
		if m.Spec.Metadata["query"] != "SELECT 2" {
			t.Errorf("expected query in metadata, got %q", m.Spec.Metadata["query"])
		}
	})
}

func TestEmitMockV2_PopulatesQueryMetadata(t *testing.T) {
	h := newV2Harness(t)
	dctx := &wire.DecodeContext{
		StmtIDToQuery: map[uint32]string{
			77: "INSERT INTO orders (id) VALUES (?)",
		},
	}
	t0 := time.Unix(1_700_000_000, 0).UTC()
	req := []mysql.Request{{
		PacketBundle: mysql.PacketBundle{
			Header: &mysql.PacketInfo{Type: "COM_STMT_EXECUTE"},
			Message: &mysql.StmtExecutePacket{
				StatementID: 77,
			},
		},
	}}
	emitMockV2(context.Background(), h.sess, req, nil, "mocks", "COM_STMT_EXECUTE", "OK", t0, t0, dctx)
	select {
	case m := <-h.mocks:
		if m.Spec.Metadata["query"] != "INSERT INTO orders (id) VALUES (?)" {
			t.Errorf("expected query in metadata, got %q", m.Spec.Metadata["query"])
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for mock from emitMockV2")
	}
}
