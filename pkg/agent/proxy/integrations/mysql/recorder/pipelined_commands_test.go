package recorder

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap/zaptest"
)

func stmtResetCommand(seq byte, statementID uint32) []byte {
	payload := make([]byte, 5)
	payload[0] = mysql.COM_STMT_RESET
	binary.LittleEndian.PutUint32(payload[1:], statementID)
	return wrapPacket(payload, seq)
}

func stmtSendLongDataCommand(seq byte, statementID uint32, parameterID uint16, data []byte) []byte {
	payload := make([]byte, 7+len(data))
	payload[0] = mysql.COM_STMT_SEND_LONG_DATA
	binary.LittleEndian.PutUint32(payload[1:], statementID)
	binary.LittleEndian.PutUint16(payload[5:], parameterID)
	copy(payload[7:], data)
	return wrapPacket(payload, seq)
}

func stmtExecuteCommand(seq byte, statementID uint32) []byte {
	payload := make([]byte, 10)
	payload[0] = mysql.COM_STMT_EXECUTE
	binary.LittleEndian.PutUint32(payload[1:], statementID)
	payload[5] = 0 // CURSOR_TYPE_NO_CURSOR
	binary.LittleEndian.PutUint32(payload[6:], 1)
	return wrapPacket(payload, seq)
}

func okResponseWithAffectedRows(seq byte, affectedRows byte) []byte {
	payload := []byte{
		mysql.OK,
		affectedRows,
		0x00,       // last_insert_id
		0x02, 0x00, // status_flags = AUTOCOMMIT
		0x00, 0x00, // warnings
	}
	return wrapPacket(payload, seq)
}

// TestAsyncMySQLDecode_PipelinedResetLongDataExecute verifies that response-
// bearing commands remain FIFO-ordered when a client pipelines them around a
// no-response COM_STMT_SEND_LONG_DATA packet. Connector/J uses this sequence
// for repeated streamed BLOB writes:
//
//	COM_STMT_RESET -> COM_STMT_SEND_LONG_DATA -> COM_STMT_EXECUTE
//
// RESET and EXECUTE each receive an OK packet; SEND_LONG_DATA receives none.
// The affected-row counts intentionally differ so the test catches both a
// missing command and a response paired with the wrong request.
func TestAsyncMySQLDecode_PipelinedResetLongDataExecute(t *testing.T) {
	const statementID = uint32(7)

	clientConn := newPipeConn()
	decodeCtx := buildPostHandshakeDecodeCtx(clientConn)
	decodeCtx.LastOp.Store(clientConn, wire.RESET)
	decodeCtx.PreparedStatements[statementID] = &mysql.StmtPrepareOkPacket{
		StatementID: statementID,
	}

	mocks := make(chan *models.Mock, 8)
	wireSyncMockOutput(t, mocks)
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "pipelined-commands")

	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	decodeItems := make(chan mysqlDecodeItem, 5)
	decodeItems <- mysqlDecodeItem{
		fromClient: true,
		data:       stmtResetCommand(0, statementID),
		ts:         base,
	}
	decodeItems <- mysqlDecodeItem{
		fromClient: true,
		data:       stmtSendLongDataCommand(0, statementID, 0, []byte("blob-chunk")),
		ts:         base.Add(time.Millisecond),
	}
	decodeItems <- mysqlDecodeItem{
		fromClient: true,
		data:       stmtExecuteCommand(0, statementID),
		ts:         base.Add(2 * time.Millisecond),
	}
	decodeItems <- mysqlDecodeItem{
		data: okResponseWithAffectedRows(1, 0),
		ts:   base.Add(3 * time.Millisecond),
	}
	decodeItems <- mysqlDecodeItem{
		data: okResponseWithAffectedRows(1, 1),
		ts:   base.Add(4 * time.Millisecond),
	}
	close(decodeItems)

	asyncMySQLDecode(
		ctx,
		zaptest.NewLogger(t),
		decodeItems,
		mocks,
		decodeCtx,
		clientConn,
		models.OutgoingOptions{DstCfg: &models.ConditionalDstCfg{Addr: "127.0.0.1:3306"}},
	)

	byOperation := make(map[string]*models.Mock)
	for {
		select {
		case mock := <-mocks:
			if mock != nil {
				byOperation[mock.Spec.Metadata["requestOperation"]] = mock
			}
		default:
			goto collected
		}
	}

collected:
	if len(byOperation) != 3 {
		t.Fatalf("recorded operations = %v, want RESET, SEND_LONG_DATA, and EXECUTE", byOperation)
	}

	sendLongData := byOperation["COM_STMT_SEND_LONG_DATA"]
	if sendLongData == nil {
		t.Fatal("COM_STMT_SEND_LONG_DATA mock was not recorded")
	}
	if got := len(sendLongData.Spec.MySQLResponses); got != 0 {
		t.Fatalf("COM_STMT_SEND_LONG_DATA responses = %d, want 0", got)
	}

	assertOKAffectedRows := func(operation string, want uint64) {
		t.Helper()
		mock := byOperation[operation]
		if mock == nil {
			t.Fatalf("%s mock was not recorded", operation)
		}
		if got := len(mock.Spec.MySQLResponses); got != 1 {
			t.Fatalf("%s responses = %d, want 1", operation, got)
		}
		okPacket, ok := mock.Spec.MySQLResponses[0].PacketBundle.Message.(*mysql.OKPacket)
		if !ok {
			t.Fatalf("%s response type = %T, want *mysql.OKPacket",
				operation, mock.Spec.MySQLResponses[0].PacketBundle.Message)
		}
		if okPacket.AffectedRows != want {
			t.Fatalf("%s affected rows = %d, want %d", operation, okPacket.AffectedRows, want)
		}
	}

	assertOKAffectedRows("COM_STMT_RESET", 0)
	assertOKAffectedRows("COM_STMT_EXECUTE", 1)
}
