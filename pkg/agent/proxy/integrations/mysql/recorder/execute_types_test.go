package recorder

import (
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models/mysql"
)

// typedExecuteCommand is a COM_STMT_EXECUTE of statementID binding an INT and
// a VAR_STRING, with their types on the wire only when bound is true
// (new_params_bind_flag 1).
func typedExecuteCommand(statementID uint32, bound bool, n int32, s string) []byte {
	p := []byte{mysql.COM_STMT_EXECUTE}
	p = binary.LittleEndian.AppendUint32(p, statementID)
	p = append(p, 0x00)
	p = binary.LittleEndian.AppendUint32(p, 1)
	p = append(p, 0x00)
	if bound {
		p = append(p, 0x01, byte(mysql.FieldTypeLong), 0x00, byte(mysql.FieldTypeVarString), 0x00)
	} else {
		p = append(p, 0x00)
	}
	p = binary.LittleEndian.AppendUint32(p, uint32(n))
	return wrapPacket(append(append(p, byte(len(s))), s...), 0)
}

// Connector/J sends a re-executed server-prepared statement without its
// parameter types (new_params_bind_flag 0), and Connector/J 5.1 sends every
// EXECUTE whose types did not change so. Each such EXECUTE is recorded, with
// the values it bound, read with the types its statement was last sent with.
// It used to fail to decode: its exchange was not recorded, and the test case
// over it was left out of the recording.
func TestRecordV2_AnExecuteWithoutTypesIsRecorded(t *testing.T) {
	t.Parallel()
	param := &mysql.ColumnDefinition41{Catalog: "def", Name: "?", FixedLength: 0x0c, CharacterSet: 0x3f, Type: byte(mysql.FieldTypeVarString)}
	got, leftOut, err := runRecordV2(t, func(h *v2Harness, at func() time.Time) {
		h.pushClient(wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "INSERT INTO items (n, s) VALUES (?, ?)"...), 0), at())
		for _, p := range prepareOK(false, [][]byte{columnPayload(t, param), columnPayload(t, param)}, nil) {
			h.pushDest(p, at())
		}
		for i, bound := range []bool{true, false, false} {
			h.pushClient(typedExecuteCommand(undecodableStmtID, bound, int32(1001+i), fmt.Sprintf("item-%d", i+1)), at())
			h.pushDest(okResponseWithAffectedRows(1, byte(i+1)), at())
		}
	})
	if err != nil {
		t.Fatalf("RecordV2: %v", err)
	}
	if leftOut.count() != 0 {
		t.Fatalf("%d exchanges left out, want none", leftOut.count())
	}
	executes := mocksOf(got, "COM_STMT_EXECUTE")
	if len(executes) != 3 {
		t.Fatalf("recorded %d EXECUTEs, want all 3", len(executes))
	}
	for i, m := range executes {
		e, ok := m.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket)
		if !ok {
			t.Fatalf("EXECUTE %d recorded a %T", i, m.Spec.MySQLRequests[0].Message)
		}
		want := []interface{}{int32(1001 + i), fmt.Sprintf("item-%d", i+1)}
		if len(e.Parameters) != 2 || e.Parameters[0].Value != want[0] || e.Parameters[1].Value != want[1] ||
			e.Parameters[0].Type != uint16(mysql.FieldTypeLong) {
			t.Errorf("EXECUTE %d recorded %+v, want LONG %v and %v", i, e.Parameters, want[0], want[1])
		}
	}
}
