package models

import (
	"fmt"

	"go.keploy.io/server/v3/pkg/models/mysql"
)

func init() {
	// Declared here, not in the MySQL parser, so every binary that removes
	// mocks knows it: whatever links the mock store links this package.
	RegisterMockDeps(MySQL, mysqlMockDeps)
}

// MySQLStatementKey names a prepared statement of a recorded MySQL
// connection: the statement ID a COM_STMT_PREPARE_OK gave on the connection
// the recorder named connID.
func MySQLStatementKey(connID string, stmtID uint32) string {
	return fmt.Sprintf("mysql statement %d on connection %q", stmtID, connID)
}

// MySQLStatementCloseKey names the COM_STMT_CLOSE of a prepared statement of a
// recorded MySQL connection (see MySQLStatementKey).
func MySQLStatementCloseKey(connID string, stmtID uint32) string {
	return fmt.Sprintf("mysql close of statement %d on connection %q", stmtID, connID)
}

// mysqlMockDeps links a MySQL mock to the others of its test set, so that a
// statement whose commands a test used is kept whole: its COM_STMT_PREPARE,
// and its COM_STMT_CLOSE.
//
// A mock whose exchange prepared a statement (its first request a
// COM_STMT_PREPARE, its first response the COM_STMT_PREPARE_OK, as the
// replayer reads a PREPARE mock) provides that statement; a mock whose command
// names a statement by its ID (COM_STMT_EXECUTE, COM_STMT_SEND_LONG_DATA,
// COM_STMT_RESET, COM_STMT_CLOSE) requires it. Only an EXECUTE needs it to
// replay: the replayer tells which statement a recorded EXECUTE ran by the
// query its PREPARE recorded, unless the recorder kept that query on the
// EXECUTE (mysql.MetaQuery). A CLOSE or a SEND_LONG_DATA has no answer to
// serve, and a RESET is answered without its mock, so they replay without it.
//
// A CLOSE provides its statement's close, which the statement's other
// commands require: it is kept with them, though nothing of theirs depends on
// it (the app closes the statement at replay too, and a CLOSE no recorded
// CLOSE answers is a miss).
func mysqlMockDeps(m *Mock) MockDeps {
	connID := m.Spec.Metadata["connID"]
	var deps MockDeps
	if len(m.Spec.MySQLRequests) > 0 && len(m.Spec.MySQLResponses) > 0 {
		if stmtID, _, ok := mysql.Prepared(m.Spec.MySQLRequests[0].Message, m.Spec.MySQLResponses[0].Message); ok {
			deps.Provides = append(deps.Provides, MySQLStatementKey(connID, stmtID))
		}
	}
	for i := range m.Spec.MySQLRequests {
		req := &m.Spec.MySQLRequests[i]
		stmtID, ok := mysql.StatementID(req.Message)
		if !ok {
			continue
		}
		standalone := true
		if _, isExecute := req.Message.(*mysql.StmtExecutePacket); isExecute {
			standalone = req.StoredQuery() != ""
		}
		deps.Requires = append(deps.Requires, MockRequirement{
			Key:        MySQLStatementKey(connID, stmtID),
			Standalone: standalone,
		})
		if _, closes := req.Message.(*mysql.StmtClosePacket); closes {
			deps.Provides = append(deps.Provides, MySQLStatementCloseKey(connID, stmtID))
		} else {
			deps.Requires = append(deps.Requires, MockRequirement{
				Key:        MySQLStatementCloseKey(connID, stmtID),
				Standalone: true,
			})
		}
	}
	return deps
}
