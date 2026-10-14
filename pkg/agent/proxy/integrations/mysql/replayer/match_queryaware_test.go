package replayer

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// execBundle builds a minimal COM_STMT_EXECUTE PacketBundle carrying a
// single string parameter, for exercising matchStmtExecutePacketQueryAware.
func execBundle(paramValue string) mysql.PacketBundle {
	return mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &mysql.Header{PayloadLength: 16, SequenceID: 0},
			Type:   "COM_STMT_EXECUTE",
		},
		Message: &mysql.StmtExecutePacket{
			Status:         0x17,
			Flags:          0,
			IterationCount: 1,
			ParameterCount: 1,
			Parameters: []mysql.Parameter{
				{Type: 254 /* MYSQL_TYPE_STRING */, Name: "", Unsigned: false, Value: paramValue},
			},
		},
	}
}

// TestMatchStmtExecuteQueryAware_QueryExactSignal locks in the contract the
// in-window FIFO selection in matchCommand relies on: the third return value
// (queryExactMatched) must be TRUE whenever the recorded prepared-query text
// equals the live query — even when the bound parameters differ.
//
// That "query-exact but params differ" case is exactly the register
// read-back: the app INSERTs a row with a freshly generated id at replay,
// then SELECT ... WHERE id = <new-id>. The new id matches no recorded
// parameter, so this is NOT a definitive match — but it IS query-exact, and
// the selector uses that signal to serve the in-window read-back row in
// recorded order instead of falling back to a wrong same-shape mock.
func TestMatchStmtExecuteQueryAware_QueryExactSignal(t *testing.T) {
	logger := zap.NewNop()
	nc := util.NewNoiseChecker(nil)

	const sameQuery = "SELECT id, username FROM users WHERE id = ?"

	t.Run("query exact + params equal -> definitive, queryExact", func(t *testing.T) {
		exp := execBundle("user-1")
		act := execBundle("user-1")
		def, _, queryExact := matchStmtExecutePacketQueryAware(logger, exp, act, sameQuery, sameQuery, "m", nc)
		if !def {
			t.Errorf("expected definitive match when query and params both match")
		}
		if !queryExact {
			t.Errorf("expected queryExactMatched=true on exact query")
		}
	})

	t.Run("query exact + params differ -> NOT definitive, but queryExact (read-back case)", func(t *testing.T) {
		exp := execBundle("recorded-id")   // recorded param
		act := execBundle("replay-new-id") // freshly generated at replay
		def, _, queryExact := matchStmtExecutePacketQueryAware(logger, exp, act, sameQuery, sameQuery, "m", nc)
		if def {
			t.Errorf("must NOT be a definitive match when bound params differ")
		}
		if !queryExact {
			t.Errorf("queryExactMatched must be true even when params differ — this is the signal the selector uses to serve the in-window read-back row")
		}
	})

	t.Run("different query -> not queryExact", func(t *testing.T) {
		exp := execBundle("x")
		act := execBundle("x")
		def, _, queryExact := matchStmtExecutePacketQueryAware(
			logger, exp, act,
			"SELECT id FROM users WHERE id = ?",
			"SELECT title FROM tasks WHERE owner = ?",
			"m", nc,
		)
		if def {
			t.Errorf("different queries must not be a definitive match")
		}
		if queryExact {
			t.Errorf("queryExactMatched must be false for structurally different queries")
		}
	})
}

func TestMatchCommand_ExecMatchesMetadataQueryWithoutPrepareMock(t *testing.T) {
	logger := zap.NewNop()
	t0 := time.Unix(1_700_000_000, 0).UTC()

	buildExecMock := func(name, query, connID string, stmtID uint32, paramVal string) *models.Mock {
		b := execBundle(paramVal)
		b.Message.(*mysql.StmtExecutePacket).StatementID = stmtID
		return &models.Mock{
			Version: "api.keploy.io/v1beta1",
			Kind:    models.MySQL,
			Name:    name,
			Spec: models.MockSpec{
				Metadata: map[string]string{
					"connID": connID,
					"type":   "mocks",
					"query":  query,
				},
				ReqTimestampMock: t0,
				MySQLRequests:    []mysql.Request{{PacketBundle: b}},
				MySQLResponses: []mysql.Response{{
					PacketBundle: mysql.PacketBundle{
						Header:  &mysql.PacketInfo{Type: "OK_PACKET"},
						Message: &mysql.OKPacket{Header: 0x00, AffectedRows: 1},
					},
				}},
			},
		}
	}

	mockOrders := buildExecMock("mock-exec-orders", "INSERT INTO orders VALUES (?)", "conn-1", 77, "order-123")
	mockOrders.Spec.MySQLResponses[0].Message.(*mysql.OKPacket).AffectedRows = 42
	mockUsers := buildExecMock("mock-exec-users", "SELECT * FROM users WHERE id = ?", "conn-1", 88, "user-456")
	mockUsers.Spec.MySQLResponses[0].Message.(*mysql.OKPacket).AffectedRows = 99

	db := &fakeMockDb{
		perTest: []*models.Mock{mockOrders, mockUsers},
	}

	dctx := newDecodeCtx()
	// Live prepare assigned statement ID 33 to "INSERT INTO orders VALUES (?)"
	dctx.StmtIDToQuery[33] = "INSERT INTO orders VALUES (?)"

	reqBundle := execBundle("order-123")
	reqBundle.Message.(*mysql.StmtExecutePacket).StatementID = 33
	req := mysql.Request{PacketBundle: reqBundle}

	matchedResp, ok, _, err := matchCommand(context.Background(), logger, req, db, dctx, nil, nil)
	if err != nil {
		t.Fatalf("matchCommand returned error: %v", err)
	}
	if !ok {
		t.Fatalf("matchCommand expected match, got ok=false")
	}
	if len(db.deletedFil) == 0 || db.deletedFil[0] != "mock-exec-orders" {
		t.Errorf("expected mock-exec-orders to be consumed, got %v", db.deletedFil)
	}
	if matchedResp.Message.(*mysql.OKPacket).AffectedRows != 42 {
		t.Errorf("expected mock-exec-orders response (AffectedRows=42), got %d", matchedResp.Message.(*mysql.OKPacket).AffectedRows)
	}
}

func TestMatchCommand_CloseMatchesMetadataQueryWithoutPrepareMock(t *testing.T) {
	logger := zap.NewNop()
	t0 := time.Unix(1_700_000_000, 0).UTC()

	buildCloseMock := func(name, query, connID string, stmtID uint32) *models.Mock {
		return &models.Mock{
			Version: "api.keploy.io/v1beta1",
			Kind:    models.MySQL,
			Name:    name,
			Spec: models.MockSpec{
				Metadata: map[string]string{
					"connID": connID,
					"type":   "mocks",
					"query":  query,
				},
				ReqTimestampMock: t0,
				MySQLRequests: []mysql.Request{{
					PacketBundle: mysql.PacketBundle{
						Header: &mysql.PacketInfo{
							Header: &mysql.Header{PayloadLength: 5, SequenceID: 0},
							Type:   "COM_STMT_CLOSE",
						},
						Message: &mysql.StmtClosePacket{StatementID: stmtID},
					},
				}},
			},
		}
	}

	mockCloseOrders := buildCloseMock("mock-close-orders", "INSERT INTO orders VALUES (?)", "conn-1", 77)
	mockCloseUsers := buildCloseMock("mock-close-users", "SELECT * FROM users WHERE id = ?", "conn-1", 88)

	db := &fakeMockDb{
		perTest: []*models.Mock{mockCloseOrders, mockCloseUsers},
	}

	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[33] = "INSERT INTO orders VALUES (?)"

	closeReqBundle := mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &mysql.Header{PayloadLength: 5, SequenceID: 0},
			Type:   "COM_STMT_CLOSE",
		},
		Message: &mysql.StmtClosePacket{StatementID: 33},
	}
	req := mysql.Request{PacketBundle: closeReqBundle}

	_, ok, _, err := matchCommand(context.Background(), logger, req, db, dctx, nil, nil)
	if err != nil {
		t.Fatalf("matchCommand returned error: %v", err)
	}
	if !ok {
		t.Fatalf("matchCommand expected match, got ok=false")
	}
	if len(db.deletedFil) == 0 || db.deletedFil[0] != "mock-close-orders" {
		t.Errorf("expected mock-close-orders to be consumed, got %v", db.deletedFil)
	}
}
