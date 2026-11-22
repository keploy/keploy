package recorder

import (
	"encoding/binary"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models/mysql"
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

// Connector/J writes a streamed BLOB as a pipeline:
//
//	COM_STMT_RESET -> COM_STMT_SEND_LONG_DATA -> COM_STMT_EXECUTE
//
// RESET and EXECUTE each get an OK; SEND_LONG_DATA gets none. Each command is
// recorded with its own answer, in order. The EXECUTE carries no value for the
// BLOB, whose null bitmap still marks it NOT NULL: the value arrived by
// SEND_LONG_DATA. Read without that, the EXECUTE does not decode, it is never
// recorded, and at replay Connector/J reports "Can not read response from
// server" (issue #4262).
func TestRecordV2_PipelinedStreamedBlobExecuteIsRecorded(t *testing.T) {
	t.Parallel()
	blob := &mysql.ColumnDefinition41{Catalog: "def", Name: "?", FixedLength: 0x0c, CharacterSet: 0x3f, Type: byte(mysql.FieldTypeBLOB)}
	execute := wrapPacket([]byte{
		mysql.COM_STMT_EXECUTE,
		byte(undecodableStmtID), 0x00, 0x00, 0x00,
		0x00,                   // flags
		0x01, 0x00, 0x00, 0x00, // iteration count
		0x00,       // NULL bitmap: parameter 0 is NOT null
		0x01,       // new params bind flag
		0xfc, 0x00, // MYSQL_TYPE_BLOB, and no value: it came by SEND_LONG_DATA
	}, 0)
	got, leftOut, err := runRecordV2(t, func(h *v2Harness, at func() time.Time) {
		h.pushClient(wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "INSERT INTO files (data) VALUES (?)"...), 0), at())
		for _, p := range prepareOK(false, [][]byte{columnPayload(t, blob)}, nil) {
			h.pushDest(p, at())
		}
		h.pushClient(stmtResetCommand(0, undecodableStmtID), at())
		h.pushClient(stmtSendLongDataCommand(0, undecodableStmtID, 0, []byte("blob-bytes")), at())
		h.pushClient(execute, at())
		h.pushDest(okResponseWithAffectedRows(1, 0), at())
		h.pushDest(okResponseWithAffectedRows(1, 1), at())
	})
	if err != nil {
		t.Fatalf("RecordV2: %v", err)
	}
	if leftOut.count() != 0 {
		t.Fatalf("%d exchanges left out, want none", leftOut.count())
	}
	if sld := mocksOf(got, "COM_STMT_SEND_LONG_DATA"); len(sld) != 1 || len(sld[0].Spec.MySQLResponses) != 0 {
		t.Fatalf("COM_STMT_SEND_LONG_DATA recorded %d mocks, want 1 with no response", len(sld))
	}
	answered := func(op string, affected uint64) {
		t.Helper()
		ms := mocksOf(got, op)
		if len(ms) != 1 {
			t.Fatalf("%s recorded %d mocks, want 1", op, len(ms))
		}
		if len(ms[0].Spec.MySQLResponses) != 1 {
			t.Fatalf("%s recorded %d responses, want 1", op, len(ms[0].Spec.MySQLResponses))
		}
		ok, isOK := ms[0].Spec.MySQLResponses[0].Message.(*mysql.OKPacket)
		if !isOK || ok.AffectedRows != affected {
			t.Fatalf("%s recorded %+v, want its own OK (%d affected rows)", op, ms[0].Spec.MySQLResponses[0].Message, affected)
		}
	}
	answered("COM_STMT_RESET", 0)
	answered("COM_STMT_EXECUTE", 1)
	if e, _ := mocksOf(got, "COM_STMT_EXECUTE")[0].Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket); e == nil || len(e.Parameters) != 1 {
		t.Fatalf("the EXECUTE recorded %+v, want its one BLOB parameter", e)
	}
}
