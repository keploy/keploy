package replayer_test

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.uber.org/zap"
)

// TestRegression_IdempotencyRepeatedPreparedExecution verifies the reported
// bug where repeated parameterized MySQL executions of the same query text on
// one connection (one returning OK, one returning ERR 1062) survive auto-replay
// pruning and correctly replay on a subsequent run without serving the wrong mock.
func TestRegression_IdempotencyRepeatedPreparedExecution(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	const testSet = "test-set-0"
	const connID = "conn-orders-1"
	const orderSQL = "INSERT INTO orders (id, status) VALUES (?, ?)"

	t0 := time.Unix(1_700_000_000, 0).UTC()
	ys := mockdb.NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatYAML)

	// Test 1: First order creation -> returns OK (AffectedRows: 1)
	mock1Prep := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.MySQL,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"connID": connID, "type": "connection"},
			ReqTimestampMock: t0,
			MySQLRequests: []mysql.Request{{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Type: "COM_STMT_PREPARE"},
					Message: &mysql.StmtPreparePacket{Command: 0x16, Query: orderSQL},
				},
			}},
			MySQLResponses: []mysql.Response{{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Type: "COM_STMT_PREPARE_OK"},
					Message: &mysql.StmtPrepareOkPacket{StatementID: 14},
				},
			}},
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeConnection, LifetimeDerived: true},
	}
	mock2Exec := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.MySQL,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"connID": connID, "type": "mocks"},
			ReqTimestampMock: t0.Add(time.Second),
			MySQLRequests: []mysql.Request{{
				PacketBundle: mysql.PacketBundle{
					Header: &mysql.PacketInfo{
						Header: &mysql.Header{PayloadLength: 16, SequenceID: 0},
						Type:   "COM_STMT_EXECUTE",
					},
					Message: &mysql.StmtExecutePacket{
						Status:         0x17,
						StatementID:    14,
						IterationCount: 1,
						ParameterCount: 1,
						Parameters: []mysql.Parameter{
							{Type: 254, Name: "", Value: "order-idempotent-key-1"},
						},
					},
				},
			}},
			MySQLResponses: []mysql.Response{{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Type: "OK"},
					Message: &mysql.OKPacket{Header: 0x00, AffectedRows: 1},
				},
			}},
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true},
	}
	mock3Close := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.MySQL,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"connID": connID, "type": "mocks"},
			ReqTimestampMock: t0.Add(2 * time.Second),
			MySQLRequests: []mysql.Request{{
				PacketBundle: mysql.PacketBundle{
					Header: &mysql.PacketInfo{
						Header: &mysql.Header{PayloadLength: 5, SequenceID: 0},
						Type:   "COM_STMT_CLOSE",
					},
					Message: &mysql.StmtClosePacket{StatementID: 14},
				},
			}},
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true},
	}

	// Test 2: Second order creation with duplicate idempotency key -> returns ERR 1062
	mock4Prep := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.MySQL,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"connID": connID, "type": "connection"},
			ReqTimestampMock: t0.Add(3 * time.Second),
			MySQLRequests: []mysql.Request{{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Type: "COM_STMT_PREPARE"},
					Message: &mysql.StmtPreparePacket{Command: 0x16, Query: orderSQL},
				},
			}},
			MySQLResponses: []mysql.Response{{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Type: "COM_STMT_PREPARE_OK"},
					Message: &mysql.StmtPrepareOkPacket{StatementID: 77},
				},
			}},
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeConnection, LifetimeDerived: true},
	}
	mock5Exec := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.MySQL,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"connID": connID, "type": "mocks"},
			ReqTimestampMock: t0.Add(4 * time.Second),
			MySQLRequests: []mysql.Request{{
				PacketBundle: mysql.PacketBundle{
					Header: &mysql.PacketInfo{
						Header: &mysql.Header{PayloadLength: 16, SequenceID: 0},
						Type:   "COM_STMT_EXECUTE",
					},
					Message: &mysql.StmtExecutePacket{
						Status:         0x17,
						StatementID:    77,
						IterationCount: 1,
						ParameterCount: 1,
						Parameters: []mysql.Parameter{
							{Type: 254, Name: "", Value: "order-idempotent-key-1"},
						},
					},
				},
			}},
			MySQLResponses: []mysql.Response{{
				PacketBundle: mysql.PacketBundle{
					Header: &mysql.PacketInfo{Type: "ERR"},
					Message: &mysql.ERRPacket{
						Header:       0xff,
						ErrorCode:    1062,
						ErrorMessage: "Error 1062 Duplicate entry 'order-idempotent-key-1' for key 'orders.uq_orders_idmp'",
					},
				},
			}},
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true},
	}
	mock6Close := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.MySQL,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"connID": connID, "type": "mocks"},
			ReqTimestampMock: t0.Add(5 * time.Second),
			MySQLRequests: []mysql.Request{{
				PacketBundle: mysql.PacketBundle{
					Header: &mysql.PacketInfo{
						Header: &mysql.Header{PayloadLength: 5, SequenceID: 0},
						Type:   "COM_STMT_CLOSE",
					},
					Message: &mysql.StmtClosePacket{StatementID: 77},
				},
			}},
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true},
	}

	for _, m := range []*models.Mock{mock1Prep, mock2Exec, mock3Close, mock4Prep, mock5Exec, mock6Close} {
		if err := ys.InsertMock(ctx, m, testSet); err != nil {
			t.Fatalf("InsertMock(%s): %v", m.Name, err)
		}
	}
	if err := ys.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Auto-replay: Test 1 consumed mock2Exec, Test 2 consumed mock5Exec.
	// Crucially, mock4Prep (PREPARE for stmt 77) was NOT recorded as consumed.
	consumed := map[string]models.MockState{
		mock2Exec.Name: {Name: mock2Exec.Name, Kind: models.MySQL},
		mock5Exec.Name: {Name: mock5Exec.Name, Kind: models.MySQL},
	}

	pruneBefore := t0.Add(time.Hour)
	ysPrune := mockdb.NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatYAML)
	if err := ysPrune.UpdateMocks(ctx, testSet, consumed, pruneBefore, time.Time{}); err != nil {
		t.Fatalf("UpdateMocks: %v", err)
	}

	// 1. Verify mock4Prep survived the prune because mock5Exec (EXECUTE 77) was kept.
	setMocks, err := ysPrune.GetTestSetMocks(ctx, testSet, models.BaseTime, time.Now(), nil, nil)
	if err != nil {
		t.Fatalf("GetTestSetMocks: %v", err)
	}
	allSurviving := append(setMocks.AllPerTest, setMocks.AllSession...)
	survivingMap := make(map[string]*models.Mock)
	for _, m := range allSurviving {
		survivingMap[m.Name] = m
	}

	if _, exists := survivingMap[mock4Prep.Name]; !exists {
		t.Fatalf("PRUNE REGRESSION: mock4Prep (%s, PREPARE stmt 77) was pruned even though mock5Exec was kept!", mock4Prep.Name)
	}

	// 2. Second replay: simulate replaying Test 2 against the pruned mock set.
	// The app re-prepares orderSQL (assigned live statement ID 33) and executes it with
	// the duplicate key parameter.
	test2Start := t0.Add(3 * time.Second)
	test2End := t0.Add(6 * time.Second)

	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	// Stage mocks for Test 2: in-window per-test mocks go to perTestMocks;
	// earlier test mocks (mock2Exec) are promoted to sessionMocks.
	var perTestMocks, sessionMocks []*models.Mock
	for _, m := range allSurviving {
		if m.TestModeInfo.Lifetime == models.LifetimePerTest {
			reqTs := m.Spec.ReqTimestampMock
			if !reqTs.Before(test2Start) && !reqTs.After(test2End) {
				perTestMocks = append(perTestMocks, m)
			} else {
				sessionMocks = append(sessionMocks, m)
			}
		} else {
			sessionMocks = append(sessionMocks, m)
		}
	}
	mm.SetMocksWithWindow(perTestMocks, sessionMocks, test2Start, test2End)

	dctx := &wire.DecodeContext{
		PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{},
		StmtIDToQuery:      map[uint32]string{33: orderSQL},
		NextStmtID:         100,
	}

	// Live client sends COM_STMT_EXECUTE for statement 33
	liveExecReq := mysql.Request{
		PacketBundle: mysql.PacketBundle{
			Header: &mysql.PacketInfo{
				Header: &mysql.Header{PayloadLength: 16, SequenceID: 0},
				Type:   "COM_STMT_EXECUTE",
			},
			Message: &mysql.StmtExecutePacket{
				Status:         0x17,
				StatementID:    33,
				IterationCount: 1,
				ParameterCount: 1,
				Parameters: []mysql.Parameter{
					{Type: 254, Name: "", Value: "order-idempotent-key-1"},
				},
			},
		},
	}

	clientCtx := context.WithValue(ctx, models.ClientConnectionIDKey, connID)
	logger, _ := zap.NewDevelopment()
	resp, ok, miss, err := replayer.MatchCommandForTest(clientCtx, logger, liveExecReq, mm, dctx, nil, false)
	if err != nil {
		t.Fatalf("MatchCommandForTest error: %v", err)
	}
	if !ok || resp == nil {
		t.Fatalf("MatchCommandForTest failed to match: ok=%v, miss=%s", ok, miss)
	}

	errPkt, isErr := resp.Message.(*mysql.ERRPacket)
	if !isErr {
		if okPkt, isOk := resp.Message.(*mysql.OKPacket); isOk {
			t.Fatalf("REPLAY REGRESSION: served OKPacket (AffectedRows=%d) from mock2Exec instead of ERRPacket 1062 from mock5Exec!", okPkt.AffectedRows)
		}
		t.Fatalf("expected response to be *mysql.ERRPacket, got %T: %+v", resp.Message, resp.Message)
	}

	if errPkt.ErrorCode != 1062 {
		t.Errorf("expected ErrorCode 1062, got %d: %s", errPkt.ErrorCode, errPkt.ErrorMessage)
	}

	// 3. Defense-in-depth check: Even if the PREPARE mock was already pruned from
	// an older recording made before this fix, an EXECUTE mock stamped with
	// metadata["query"] still matches definitive and serves ERR 1062.
	t.Run("DefenseInDepth_MissingPrepareMatchesViaMetadataQuery", func(t *testing.T) {
		mmLegacy := proxy.NewMockManager(nil, nil, zap.NewNop())
		// Stage mock5Exec WITH metadata["query"], but without mock4Prep (simulating pruned PREPARE).
		mock5WithQuery := mock5Exec.DeepCopy()
		mock5WithQuery.Spec.Metadata["query"] = orderSQL

		// mock2Exec is in sessionMocks (earlier test OK packet whose PREPARE survived)
		mmLegacy.SetMocksWithWindow([]*models.Mock{mock5WithQuery}, []*models.Mock{mock1Prep, mock2Exec, mock3Close}, test2Start, test2End)

		resp, ok, miss, err := replayer.MatchCommandForTest(clientCtx, zap.NewNop(), liveExecReq, mmLegacy, dctx, nil, false)
		if err != nil {
			t.Fatalf("MatchCommandForTest error: %v", err)
		}
		if !ok || resp == nil {
			t.Fatalf("MatchCommandForTest failed to match: ok=%v, miss=%s", ok, miss)
		}
		errPkt, isErr := resp.Message.(*mysql.ERRPacket)
		if !isErr {
			if okPkt, isOk := resp.Message.(*mysql.OKPacket); isOk {
				t.Fatalf("DEFENSE IN DEPTH FAILURE: served OKPacket (%d) instead of ERRPacket 1062!", okPkt.AffectedRows)
			}
			t.Fatalf("expected ERRPacket, got %T: %+v", resp.Message, resp.Message)
		}
		if errPkt.ErrorCode != 1062 {
			t.Errorf("expected ErrorCode 1062, got %d", errPkt.ErrorCode)
		}
	})
}
