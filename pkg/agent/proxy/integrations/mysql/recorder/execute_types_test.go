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
func TestAsyncMySQLDecode_AnExecuteWithoutTypesIsRecorded(t *testing.T) {
	const statementID = uint32(7)
	clientConn := newPipeConn()
	decodeCtx := buildPostHandshakeDecodeCtx(clientConn)
	decodeCtx.LastOp.Store(clientConn, wire.RESET)
	decodeCtx.PreparedStatements[statementID] = &mysql.StmtPrepareOkPacket{StatementID: statementID, NumParams: 2}

	mocks := make(chan *models.Mock, 8)
	wireSyncMockOutput(t, mocks)
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "unbound-types")

	base := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	items := []mysqlDecodeItem{
		{fromClient: true, data: typedExecuteCommand(statementID, true, 1001, "item-1")},
		{data: okResponseWithAffectedRows(1, 1)},
		{fromClient: true, data: typedExecuteCommand(statementID, false, 1002, "item-2")},
		{data: okResponseWithAffectedRows(1, 2)},
		{fromClient: true, data: typedExecuteCommand(statementID, false, 1003, "item-3")},
		{data: okResponseWithAffectedRows(1, 3)},
	}
	decodeItems := make(chan mysqlDecodeItem, len(items))
	for i, it := range items {
		it.ts = base.Add(time.Duration(i) * time.Millisecond)
		decodeItems <- it
	}
	close(decodeItems)
	asyncMySQLDecode(ctx, zaptest.NewLogger(t), decodeItems, mocks, decodeCtx, clientConn,
		models.OutgoingOptions{DstCfg: &models.ConditionalDstCfg{Addr: "127.0.0.1:3306"}})

	var got []*mysql.StmtExecutePacket
	for len(mocks) > 0 {
		m := <-mocks
		if e, ok := m.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket); ok {
			got = append(got, e)
		}
	}
	if len(got) != 3 {
		t.Fatalf("recorded %d EXECUTEs, want all 3", len(got))
	}
	for i, e := range got {
		want := []interface{}{int32(1001 + i), "item-" + string(rune('1'+i))}
		if len(e.Parameters) != 2 || e.Parameters[0].Value != want[0] || e.Parameters[1].Value != want[1] ||
			e.Parameters[0].Type != uint16(mysql.FieldTypeLong) {
			t.Errorf("EXECUTE %d recorded %+v, want LONG %v and %v", i, e.Parameters, want[0], want[1])
		}
	}
}
