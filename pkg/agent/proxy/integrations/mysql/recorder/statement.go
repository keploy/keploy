package recorder

import (
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// A command that names a prepared statement by its ID (COM_STMT_EXECUTE,
// COM_STMT_SEND_LONG_DATA, COM_STMT_RESET, COM_STMT_CLOSE) is recorded with
// the query the statement was prepared with on its connection
// (mysql.MetaQuery), so its mock is read without the COM_STMT_PREPARE's: a
// test set can lose that one (keploy #4810), and a statement the connection
// prepared before the recording began has none.
//
// The recorder reads a connection's packets in one goroutine with one
// decodeCtx, so decodeCtx.StmtIDToQuery is the connection's map of the
// statements it prepared, by ID.

// rememberPrepared notes the statement an exchange prepared: the query of the
// COM_STMT_PREPARE (cmd) under the ID its COM_STMT_PREPARE_OK (resp) gave it.
// Any other exchange notes nothing.
func rememberPrepared(decodeCtx *wire.DecodeContext, cmd, resp *mysql.PacketBundle) {
	if decodeCtx == nil || cmd == nil || resp == nil {
		return
	}
	stmtID, query, ok := mysql.Prepared(cmd.Message, resp.Message)
	if !ok {
		return
	}
	if decodeCtx.StmtIDToQuery == nil {
		decodeCtx.StmtIDToQuery = make(map[uint32]string)
	}
	decodeCtx.StmtIDToQuery[stmtID] = query
}

// stampStatementQuery records, on a command that names a prepared statement,
// the query the connection prepared that statement with, when it saw the
// statement prepared. A COM_STMT_CLOSE ends the statement, and its ID with it.
func stampStatementQuery(decodeCtx *wire.DecodeContext, cmd *mysql.PacketBundle) {
	if decodeCtx == nil || cmd == nil {
		return
	}
	stmtID, ok := mysql.StatementID(cmd.Message)
	if !ok {
		return
	}
	cmd.SetStoredQuery(decodeCtx.StmtIDToQuery[stmtID])
	if _, closes := cmd.Message.(*mysql.StmtClosePacket); closes {
		delete(decodeCtx.StmtIDToQuery, stmtID)
	}
}

// forgetPrepared forgets the statement sp named when its COM_STMT_PREPARE's
// exchange is left out (sp nil, a reply that is no COM_STMT_PREPARE_OK,
// forgets nothing). The wire decoder learns a statement from its
// COM_STMT_PREPARE_OK head, before the rest of the reply is read; kept, a
// later COM_STMT_EXECUTE of it would decode and be recorded, and it cannot
// replay without the PREPARE's mock. Forgotten, the statement is one the
// recording never saw prepared, as one prepared before the recording began
// is: its EXECUTE does not decode, so its exchange is left out with the test
// cases over it, and a command that names it is recorded without its query.
// A later PREPARE the server answers with the same ID makes it known again.
func forgetPrepared(decodeCtx *wire.DecodeContext, sp *mysql.StmtPrepareOkPacket) {
	if sp == nil {
		return
	}
	delete(decodeCtx.PreparedStatements, sp.StatementID)
	delete(decodeCtx.StmtIDToQuery, sp.StatementID)
}
