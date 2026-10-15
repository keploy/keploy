package recorder

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	connphase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// A command that names a prepared statement is recorded with the query the
// connection prepared it with (mysql.MetaQuery), so its mock reads without the
// COM_STMT_PREPARE's (keploy #4810).

func stmtCloseCommand(seq byte, statementID uint32) []byte {
	payload := make([]byte, 5)
	payload[0] = mysql.COM_STMT_CLOSE
	binary.LittleEndian.PutUint32(payload[1:], statementID)
	return wrapPacket(payload, seq)
}

func TestStampStatementQueryRecordsThePreparedQuery(t *testing.T) {
	dctx := &wire.DecodeContext{}
	prepare := &mysql.PacketBundle{Message: &mysql.StmtPreparePacket{Query: " SELECT ? "}}
	rememberPrepared(dctx, prepare, &mysql.PacketBundle{Message: &mysql.StmtPrepareOkPacket{StatementID: 4}})
	rememberPrepared(dctx, &mysql.PacketBundle{Message: &mysql.StmtPreparePacket{Query: "SELECT 2"}}, &mysql.PacketBundle{Message: &mysql.ERRPacket{}})

	for _, msg := range []interface{}{
		&mysql.StmtExecutePacket{StatementID: 4},
		&mysql.StmtSendLongDataPacket{StatementID: 4},
		&mysql.StmtResetPacket{StatementID: 4},
	} {
		cmd := &mysql.PacketBundle{Message: msg}
		stampStatementQuery(dctx, cmd)
		if got := cmd.StoredQuery(); got != "SELECT ?" {
			t.Errorf("%T: stored query %q, want %q", msg, got, "SELECT ?")
		}
	}
	unknown := &mysql.PacketBundle{Message: &mysql.StmtExecutePacket{StatementID: 5}}
	stampStatementQuery(dctx, unknown)
	if unknown.Meta != nil {
		t.Errorf("an EXECUTE of a statement the connection never prepared was stamped: %v", unknown.Meta)
	}
	query := &mysql.PacketBundle{Message: &mysql.QueryPacket{Query: "SELECT 1"}}
	stampStatementQuery(dctx, query)
	if query.Meta != nil {
		t.Errorf("a COM_QUERY was stamped: %v", query.Meta)
	}

	closer := &mysql.PacketBundle{Message: &mysql.StmtClosePacket{StatementID: 4}}
	stampStatementQuery(dctx, closer)
	if got := closer.StoredQuery(); got != "SELECT ?" {
		t.Errorf("the CLOSE's stored query %q, want %q", got, "SELECT ?")
	}
	if _, open := dctx.StmtIDToQuery[4]; open {
		t.Error("the statement is still known after its COM_STMT_CLOSE")
	}
}

// statementMocks collects the recorded mocks of the operations that name a
// statement, in the order recorded.
func statementMocks(mocks []*models.Mock) []*models.Mock {
	var out []*models.Mock
	for _, m := range mocks {
		if len(m.Spec.MySQLRequests) == 0 {
			continue
		}
		if _, ok := mysql.StatementID(m.Spec.MySQLRequests[0].Message); ok {
			out = append(out, m)
		}
	}
	return out
}

func requireStamped(t *testing.T, mocks []*models.Mock, want []string) {
	t.Helper()
	got := statementMocks(mocks)
	if len(got) != len(want) {
		var ops []string
		for _, m := range mocks {
			ops = append(ops, m.Spec.Metadata["requestOperation"])
		}
		t.Fatalf("recorded %d commands that name a statement, want %d (recorded %v)", len(got), len(want), ops)
	}
	for i, m := range got {
		if q := m.Spec.MySQLRequests[0].StoredQuery(); q != want[i] {
			t.Errorf("%s #%d: stored query %q, want %q", m.Spec.Metadata["requestOperation"], i, q, want[i])
		}
	}
}

// TestAsyncMySQLDecode_StatementCommandsKeepTheirQuery: the legacy recorder
// stamps each command as it decodes it, so a pipelined CLOSE behind an
// EXECUTE does not end the statement before the EXECUTE is stamped.
func TestAsyncMySQLDecode_StatementCommandsKeepTheirQuery(t *testing.T) {
	clientConn := newPipeConn()
	decodeCtx := buildPostHandshakeDecodeCtx(clientConn)
	decodeCtx.LastOp.Store(clientConn, wire.RESET)

	mocks := make(chan *models.Mock, 16)
	wireSyncMockOutput(t, mocks)
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "stamped")

	base := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	items := []mysqlDecodeItem{
		{fromClient: true, data: prepareCmd(0, "SELECT NOW()")},
		{data: prepareOkShort(1, 1)},
		{fromClient: true, data: stmtExecuteCommand(0, 1)},
		{data: okResponseWithAffectedRows(1, 0)},
		// Pipelined: the EXECUTE and the CLOSE of the same statement before
		// the EXECUTE's answer.
		{fromClient: true, data: stmtExecuteCommand(0, 1)},
		{fromClient: true, data: stmtCloseCommand(0, 1)},
		{data: okResponseWithAffectedRows(1, 1)},
		// The same query prepared again, under a new ID.
		{fromClient: true, data: prepareCmd(0, "SELECT NOW()")},
		{data: prepareOkShort(1, 2)},
		{fromClient: true, data: stmtExecuteCommand(0, 2)},
		{data: okResponseWithAffectedRows(1, 0)},
	}
	decodeItems := make(chan mysqlDecodeItem, len(items))
	for i, it := range items {
		it.ts = base.Add(time.Duration(i) * time.Millisecond)
		decodeItems <- it
	}
	close(decodeItems)

	asyncMySQLDecode(ctx, zaptest.NewLogger(t), decodeItems, mocks, decodeCtx, clientConn,
		models.OutgoingOptions{DstCfg: &models.ConditionalDstCfg{Addr: "127.0.0.1:3306"}})

	var got []*models.Mock
	for len(mocks) > 0 {
		got = append(got, <-mocks)
	}
	requireStamped(t, got, []string{"SELECT NOW()", "SELECT NOW()", "SELECT NOW()", "SELECT NOW()"})
}

// TestRecordV2_StatementCommandsKeepTheirQuery: the same, through RecordV2.
func TestRecordV2_StatementCommandsKeepTheirQuery(t *testing.T) {
	t.Parallel()
	h := newV2Harness(t)
	base := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Millisecond) }

	handshakeBuf := cannedHandshakeV10(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), handshakeBuf[4:])
	if err != nil {
		t.Fatalf("decode handshake v10: %v", err)
	}
	h.pushDest(handshakeBuf, at(0))
	h.pushClient(cannedHandshakeResponse41(t, 1, false), at(1))
	h.pushDest(cannedOK(t, 2, greeting.CapabilityFlags), at(2))

	h.pushClient(prepareCmd(0, "SELECT NOW()"), at(3))
	h.pushDest(prepareOkShort(1, 1), at(4))
	h.pushClient(stmtExecuteCommand(0, 1), at(5))
	h.pushDest(cannedOK(t, 1, greeting.CapabilityFlags), at(6))
	h.pushClient(stmtCloseCommand(0, 1), at(7))
	h.pushClient(prepareCmd(0, "SELECT CURDATE()"), at(8))
	h.pushDest(prepareOkShort(1, 2), at(9))
	h.pushClient(stmtExecuteCommand(0, 2), at(10))
	h.pushDest(cannedOK(t, 1, greeting.CapabilityFlags), at(11))
	// The handshake, two PREPAREs, two EXECUTEs and a CLOSE.
	const recorded = 6
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		done <- RecordV2(ctx, h.logger, h.sess)
	}()
	var got []*models.Mock
	for len(got) < recorded {
		select {
		case m := <-h.mocks:
			got = append(got, m)
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for mocks (got %d)", len(got))
		}
	}
	h.closeStreams()
	if err := <-done; err != nil {
		t.Fatalf("RecordV2: %v", err)
	}
	for len(h.mocks) > 0 {
		got = append(got, <-h.mocks)
	}
	requireStamped(t, got, []string{"SELECT NOW()", "SELECT NOW()", "SELECT CURDATE()"})
}
