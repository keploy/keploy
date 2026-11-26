package replayer

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

type rtMySQLMockDb struct {
	*fakeMockDb
	readThrough []*models.Mock
}

func (d *rtMySQLMockDb) MatchReadThrough(live *models.Mock) (*models.Mock, bool) {
	lk, ok := models.ReadThroughRequestKey(live)
	if !ok {
		return nil, false
	}
	for _, rt := range d.readThrough {
		if !models.IsReadThrough(rt) {
			continue
		}
		if rk, ok := models.ReadThroughKey(rt); ok && lk == rk {
			return rt, true
		}
	}
	return nil, false
}

var _ integrations.ReadThroughMatcher = (*rtMySQLMockDb)(nil)

func newMySQLReadThroughMock(name, dbName, query string, rowTag uint64) *models.Mock {
	return newMySQLReadThroughMockAt(name, "192.0.2.10:3306", dbName, query, rowTag)
}

func newMySQLReadThroughMockAt(name, destAddr, dbName, query string, rowTag uint64) *models.Mock {
	meta := map[string]string{
		"type":                     "mocks",
		"requestOperation":         "COM_QUERY",
		"responseOperation":        string(mysql.Text),
		"connID":                   "1",
		"destAddr":                 destAddr,
		models.MetadataLifetimeKey: models.MetadataLifetimePerTest,
	}
	if dbName != "" {
		meta["database"] = dbName
	}
	return &models.Mock{
		Version: models.Version("api.keploy.io/v1beta2"),
		Name:    name,
		Kind:    models.MySQL,
		Spec: models.MockSpec{
			Metadata: meta,
			MySQLRequests: []mysql.Request{{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 0}, Type: "COM_QUERY"},
					Message: &mysql.QueryPacket{Query: query},
				},
			}},
			MySQLResponses: []mysql.Response{{
				PacketBundle: mysql.PacketBundle{
					Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 1}, Type: string(mysql.Text)},
					Message: &mysql.TextResultSet{
						ColumnCount: rowTag,
						FinalResponse: &mysql.GenericResponse{
							Data: []byte{0x05, 0x00, 0x00, 0x05, 0xfe, 0x00, 0x00, 0x02, 0x00},
							Type: "EOF",
						},
					},
				},
			}},
		},
	}
}

func TestMySQLReadThrough_ExactFallbackOnlyAndRejectsWritesBothProvenModes(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", proven)
			logger := zap.NewNop()

			rtSelect := newMySQLReadThroughMock(
				"mock-rt-mysql-cfg",
				"appdb",
				"SELECT owner_id FROM service_configs WHERE service_key = 'svc-alpha'",
				1,
			)
			if !models.MarkReadThrough(rtSelect) {
				t.Fatal("MarkReadThrough failed for MySQL SELECT")
			}

			db := &rtMySQLMockDb{
				fakeMockDb:  &fakeMockDb{},
				readThrough: []*models.Mock{rtSelect},
			}
			decodeCtx := &wire.DecodeContext{Database: "appdb", DestPort: "3306"}

			// 1. Exact SELECT query hits read-through fallback on empty per-test pool.
			exactReq := mysql.Request{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 0}, Type: "COM_QUERY"},
					Message: &mysql.QueryPacket{Query: "SELECT owner_id FROM service_configs WHERE service_key = 'svc-alpha'"},
				},
			}
			resp, ok, miss, err := matchCommand(context.Background(), logger, exactReq, db, decodeCtx, nil, nil)
			if err != nil || !ok || resp == nil || miss != nil {
				t.Fatalf("expected exact MySQL read-through fallback hit, got ok=%v resp=%v miss=%v err=%v", ok, resp, miss, err)
			}

			// 2. Different WHERE predicate MUST miss (zero fuzzy fallback).
			diffReq := mysql.Request{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 0}, Type: "COM_QUERY"},
					Message: &mysql.QueryPacket{Query: "SELECT owner_id FROM service_configs WHERE service_key = 'svc-beta'"},
				},
			}
			if _, ok, _, _ := matchCommand(context.Background(), logger, diffReq, db, decodeCtx, nil, nil); ok {
				t.Fatal("different MySQL SELECT predicate MUST miss read-through tier")
			}

			// 3. Locking read / write MUST miss.
			lockReq := mysql.Request{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 0}, Type: "COM_QUERY"},
					Message: &mysql.QueryPacket{Query: "SELECT owner_id FROM service_configs WHERE service_key = 'svc-alpha'\nFOR UPDATE"},
				},
			}
			if _, ok, _, _ := matchCommand(context.Background(), logger, lockReq, db, decodeCtx, nil, nil); ok {
				t.Fatal("locking SELECT FOR UPDATE MUST miss read-through tier")
			}
		})
	}
}

// TestMySQLReadThrough_MultiTenantDatabaseScopingAndSwitch verifies that
// identical SELECT queries executed against different MySQL databases
// (tenant_a vs tenant_b) or different MySQL server ports (3306 vs 3307)
// produce distinct read-through keys and never collide on replay, including
// after switching databases via COM_INIT_DB or USE, and fail closed when
// DatabaseUnknown is true.
func TestMySQLReadThrough_MultiTenantDatabaseScopingAndSwitch(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", proven)
			logger := zap.NewNop()

			const sql = "SELECT * FROM settings LIMIT 1"
			rtA := newMySQLReadThroughMockAt("mock-rt-tenant-a", "192.0.2.10:3306", "tenant_a", sql, 101)
			rtB := newMySQLReadThroughMockAt("mock-rt-tenant-b", "192.0.2.10:3306", "tenant_b", sql, 202)
			rtShardB := newMySQLReadThroughMockAt("mock-rt-shard-b", "192.0.2.10:3307", "tenant_a", sql, 303)
			if !models.MarkReadThrough(rtA) || !models.MarkReadThrough(rtB) || !models.MarkReadThrough(rtShardB) {
				t.Fatal("MarkReadThrough failed for tenant/shard mocks")
			}

			initDBMock := &models.Mock{
				Version: models.Version("api.keploy.io/v1beta2"),
				Name:    "mock-initdb-b",
				Kind:    models.MySQL,
				Spec: models.MockSpec{
					Metadata: map[string]string{
						"type":                     "config",
						"requestOperation":         "COM_INIT_DB",
						"responseOperation":        "OK",
						models.MetadataLifetimeKey: models.MetadataLifetimeSession,
					},
					MySQLRequests: []mysql.Request{{
						PacketBundle: mysql.PacketBundle{
							Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 0}, Type: "COM_INIT_DB"},
							Message: &mysql.InitDBPacket{Command: mysql.COM_INIT_DB, Schema: "tenant_b"},
						},
					}},
					MySQLResponses: []mysql.Response{{
						PacketBundle: mysql.PacketBundle{
							Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 1}, Type: "OK"},
							Message: &mysql.OKPacket{StatusFlags: 0x0002},
						},
					}},
				},
				TestModeInfo: models.TestModeInfo{
					Lifetime:        models.LifetimeSession,
					LifetimeDerived: true,
				},
			}

			db := &rtMySQLMockDb{
				fakeMockDb:  &fakeMockDb{session: []*models.Mock{initDBMock}},
				readThrough: []*models.Mock{rtA, rtB, rtShardB},
			}
			selectReq := mysql.Request{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 0}, Type: "COM_QUERY"},
					Message: &mysql.QueryPacket{Query: sql},
				},
			}

			// Tenant A on port 3306 gets tenant_a rows (101).
			ctxA := &wire.DecodeContext{Database: "tenant_a", DestPort: "3306"}
			respA, ok, _, err := matchCommand(context.Background(), logger, selectReq, db, ctxA, nil, nil)
			if err != nil || !ok || respA == nil {
				t.Fatalf("tenant_a miss: ok=%v err=%v", ok, err)
			}
			if rs, _ := respA.Message.(*mysql.TextResultSet); rs == nil || rs.ColumnCount != 101 {
				t.Fatalf("tenant_a got wrong response: %+v", respA.Message)
			}

			// Tenant B on port 3306 gets tenant_b rows (202).
			ctxB := &wire.DecodeContext{Database: "tenant_b", DestPort: "3306"}
			respB, ok, _, err := matchCommand(context.Background(), logger, selectReq, db, ctxB, nil, nil)
			if err != nil || !ok || respB == nil {
				t.Fatalf("tenant_b miss: ok=%v err=%v", ok, err)
			}
			if rs, _ := respB.Message.(*mysql.TextResultSet); rs == nil || rs.ColumnCount != 202 {
				t.Fatalf("tenant_b got wrong response: %+v", respB.Message)
			}

			// Tenant A on shard port 3307 gets shard B rows (303), never port 3306's (101).
			ctxShardB := &wire.DecodeContext{Database: "tenant_a", DestPort: "3307"}
			respShardB, ok, _, err := matchCommand(context.Background(), logger, selectReq, db, ctxShardB, nil, nil)
			if err != nil || !ok || respShardB == nil {
				t.Fatalf("shard B miss: ok=%v err=%v", ok, err)
			}
			if rs, _ := respShardB.Message.(*mysql.TextResultSet); rs == nil || rs.ColumnCount != 303 {
				t.Fatalf("shard B got wrong response: %+v", respShardB.Message)
			}

			// Unknown tenant (tenant_c) or empty DB must MISS (never leak tenant A/B rows).
			for _, otherDB := range []string{"tenant_c", ""} {
				if respOther, ok, _, _ := matchCommand(context.Background(), logger, selectReq, db, &wire.DecodeContext{Database: otherDB, DestPort: "3306"}, nil, nil); ok {
					t.Fatalf("database %q must NOT receive tenant_a/tenant_b rows, got %+v", otherDB, respOther)
				}
			}

			// DatabaseUnknown=true MUST fail closed and miss read-through even if Database still held an old value.
			if respUnk, ok, _, _ := matchCommand(context.Background(), logger, selectReq, db, &wire.DecodeContext{Database: "tenant_a", DatabaseUnknown: true, DestPort: "3306"}, nil, nil); ok {
				t.Fatalf("DatabaseUnknown=true must fail closed and miss read-through, got %+v", respUnk)
			}

			// Switching ctxA via COM_INIT_DB("tenant_b") updates active database to tenant_b.
			initReq := mysql.Request{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 0}, Type: "COM_INIT_DB"},
					Message: &mysql.InitDBPacket{Command: mysql.COM_INIT_DB, Schema: "tenant_b"},
				},
			}
			if _, ok, _, err := matchCommand(context.Background(), logger, initReq, db, ctxA, nil, nil); err != nil || !ok {
				t.Fatalf("COM_INIT_DB match failed: ok=%v err=%v", ok, err)
			}
			if ctxA.Database != "tenant_b" || ctxA.DatabaseUnknown {
				t.Fatalf("expected ctxA=(tenant_b, unknown=false) after COM_INIT_DB, got (%q, %v)", ctxA.Database, ctxA.DatabaseUnknown)
			}
			respSwitched, ok, _, err := matchCommand(context.Background(), logger, selectReq, db, ctxA, nil, nil)
			if err != nil || !ok || respSwitched == nil {
				t.Fatalf("post-switch tenant_b miss: ok=%v err=%v", ok, err)
			}
			if rs, _ := respSwitched.Message.(*mysql.TextResultSet); rs == nil || rs.ColumnCount != 202 {
				t.Fatalf("post-switch query got wrong tenant response: %+v", respSwitched.Message)
			}
		})
	}
}

// TestMySQLReadThrough_MissPhaseStrictGuard verifies that:
//   - An unrelated MySQL mock in the test's pool does NOT block a cold-cache
//     read-through hit.
//   - A same-skeleton MySQL mock in the test's pool whose literal drifted and
//     was rejected by strict/exact comparison (MatchPhaseStrict / strictRejected>0)
//     blocks read-through so changed queries are never masked.
func TestMySQLReadThrough_MissPhaseStrictGuard(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	logger := zap.NewNop()

	rt := newMySQLReadThroughMock("mock-rt-settings", "tenant_a", "SELECT * FROM settings WHERE id = 2", 22)
	if !models.MarkReadThrough(rt) {
		t.Fatal("MarkReadThrough failed")
	}

	// 1. Unrelated pool mock: cold-cache SELECT on settings MUST still hit read-through.
	unrelated := newMySQLReadThroughMock("mock-own-orders", "tenant_a", "SELECT * FROM orders WHERE id = 99", 99)
	unrelated.TestModeInfo.IsFiltered = true
	unrelated.TestModeInfo.Lifetime = models.LifetimePerTest
	unrelated.TestModeInfo.LifetimeDerived = true
	dbUnrelated := &rtMySQLMockDb{
		fakeMockDb:  &fakeMockDb{perTest: []*models.Mock{unrelated}},
		readThrough: []*models.Mock{rt},
	}
	liveReq := mysql.Request{
		PacketBundle: mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 0}, Type: "COM_QUERY"},
			Message: &mysql.QueryPacket{Query: "SELECT * FROM settings WHERE id = 2"},
		},
	}
	resp, ok, miss, err := matchCommand(context.Background(), logger, liveReq, dbUnrelated, &wire.DecodeContext{Database: "tenant_a", DestPort: "3306"}, nil, nil)
	if err != nil || !ok || resp == nil || miss != nil {
		t.Fatalf("unrelated pool mock must not block read-through hit: ok=%v miss=%+v err=%v", ok, miss, err)
	}

	// 2. Same-skeleton drifted pool mock (id=1 recorded, id=2 live) under
	//    strict schema-noise enforcement rejects with MatchPhaseStrict ->
	//    read-through MUST NOT mask the drifted query.
	drifted := newMySQLReadThroughMock("mock-own-settings-1", "tenant_a", "SELECT * FROM settings WHERE id = 1", 11)
	drifted.TestModeInfo.IsFiltered = true
	drifted.TestModeInfo.Lifetime = models.LifetimePerTest
	drifted.TestModeInfo.LifetimeDerived = true
	drifted.Spec.ReqBodyNoise = map[string][]string{"unrelated": nil}
	dbDrifted := &rtMySQLMockDb{
		fakeMockDb:  &fakeMockDb{perTest: []*models.Mock{drifted}},
		readThrough: []*models.Mock{rt},
	}
	strictEngine := mocknoise.New(mysqlNoiseAdapter{}, false, true)
	if resp2, ok2, miss2, err2 := matchCommand(context.Background(), logger, liveReq, dbDrifted, &wire.DecodeContext{Database: "tenant_a", DestPort: "3306"}, strictEngine, nil); err2 != nil || ok2 || resp2 != nil {
		t.Fatalf("same-skeleton strict-rejected drift must NOT be masked by read-through: ok=%v resp=%v err=%v", ok2, resp2, err2)
	} else if miss2 == nil || miss2.matchPhase != models.MatchPhaseStrict || miss2.strictRejected == 0 {
		t.Fatalf("expected MatchPhaseStrict miss with strictRejected>0, got %+v", miss2)
	}
}

// TestMySQLReadThrough_InTxRefusedAndAutocommitAllowed verifies that replay
// refuses read-through fallback whenever decodeCtx.InTx is true (both when set
// directly and when entered via BEGIN through matchCommand) while continuing
// to serve autocommit reads (InTx == false, including after COMMIT), and that
// fabricated DstCfg (Addr:"127.0.0.1:0", Port:3307) derives DestPort="3307".
func TestMySQLReadThrough_InTxRefusedAndAutocommitAllowed(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", proven)
			logger := zap.NewNop()

			rt := newMySQLReadThroughMockAt("mock-rt-settings", "127.0.0.1:0", "appdb", "SELECT val FROM settings WHERE id = 1", 77)
			rt.Spec.Metadata["destPort"] = "3307"
			if !models.MarkReadThrough(rt) {
				t.Fatal("MarkReadThrough failed")
			}

			makeTxCtrlMock := func(name, sql string, status uint16) *models.Mock {
				return &models.Mock{
					Version: models.Version("api.keploy.io/v1beta2"),
					Name:    name,
					Kind:    models.MySQL,
					Spec: models.MockSpec{
						Metadata: map[string]string{
							"type":                     "config",
							"requestOperation":         "COM_QUERY",
							"responseOperation":        "OK",
							models.MetadataLifetimeKey: models.MetadataLifetimeSession,
						},
						MySQLRequests: []mysql.Request{{
							PacketBundle: mysql.PacketBundle{
								Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 0}, Type: "COM_QUERY"},
								Message: &mysql.QueryPacket{Query: sql},
							},
						}},
						MySQLResponses: []mysql.Response{{
							PacketBundle: mysql.PacketBundle{
								Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 1}, Type: "OK"},
								Message: &mysql.OKPacket{StatusFlags: status},
							},
						}},
					},
					TestModeInfo: models.TestModeInfo{
						Lifetime:        models.LifetimeSession,
						LifetimeDerived: true,
					},
				}
			}

			beginMock := makeTxCtrlMock("mock-begin", "BEGIN", 0x0003)
			commitMock := makeTxCtrlMock("mock-commit", "COMMIT", 0x0002)

			db := &rtMySQLMockDb{
				fakeMockDb:  &fakeMockDb{session: []*models.Mock{beginMock, commitMock}},
				readThrough: []*models.Mock{rt},
			}
			selectReq := mysql.Request{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 0}, Type: "COM_QUERY"},
					Message: &mysql.QueryPacket{Query: "SELECT val FROM settings WHERE id = 1"},
				},
			}

			// Fabricated DstCfg (Addr port 0, Port 3307) must populate DestPort="3307" from Port.
			dc := &wire.DecodeContext{Database: "appdb"}
			dc.SetDestPortFromCfg(&models.ConditionalDstCfg{
				Addr:           "127.0.0.1:0",
				Port:           3307,
				AddrFabricated: true,
			})
			if dc.DestPort != "3307" {
				t.Fatalf("expected DestPort=\"3307\" from fabricated DstCfg.Port, got %q", dc.DestPort)
			}

			// 1. Autocommit (InTx == false) serves cleanly.
			resp, ok, miss, err := matchCommand(context.Background(), logger, selectReq, db, dc, nil, nil)
			if err != nil || !ok || resp == nil || miss != nil {
				t.Fatalf("autocommit SELECT must hit read-through: ok=%v resp=%v miss=%v err=%v", ok, resp, miss, err)
			}

			// 2. Direct InTx == true: tryReadThrough and matchCommand MUST refuse.
			dc.InTx = true
			if rtResp, hit := tryReadThrough(logger, selectReq, dc, db); hit || rtResp != nil {
				t.Fatalf("tryReadThrough with InTx=true MUST refuse, got hit=%v resp=%+v", hit, rtResp)
			}
			if respTx, okTx, _, _ := matchCommand(context.Background(), logger, selectReq, db, dc, nil, nil); okTx || respTx != nil {
				t.Fatalf("matchCommand with InTx=true MUST NOT serve read-through, got ok=%v resp=%+v", okTx, respTx)
			}

			// 3. Transition via real BEGIN / COMMIT through matchCommand:
			//    BEGIN sets dc.InTx=true via bindAfter -> SELECT refuses read-through;
			//    COMMIT clears dc.InTx=false -> SELECT hits read-through again.
			dc.InTx = false
			beginReq := mysql.Request{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 0}, Type: "COM_QUERY"},
					Message: &mysql.QueryPacket{Query: "BEGIN"},
				},
			}
			if _, okB, _, errB := matchCommand(context.Background(), logger, beginReq, db, dc, nil, nil); errB != nil || !okB || !dc.InTx {
				t.Fatalf("BEGIN must match and set InTx=true, got ok=%v inTx=%v err=%v", okB, dc.InTx, errB)
			}
			if respInTx, okInTx, _, _ := matchCommand(context.Background(), logger, selectReq, db, dc, nil, nil); okInTx || respInTx != nil {
				t.Fatalf("SELECT after BEGIN MUST NOT serve read-through, got ok=%v resp=%+v", okInTx, respInTx)
			}
			commitReq := mysql.Request{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 0}, Type: "COM_QUERY"},
					Message: &mysql.QueryPacket{Query: "COMMIT"},
				},
			}
			if _, okC, _, errC := matchCommand(context.Background(), logger, commitReq, db, dc, nil, nil); errC != nil || !okC || dc.InTx {
				t.Fatalf("COMMIT must match and clear InTx=false, got ok=%v inTx=%v err=%v", okC, dc.InTx, errC)
			}
			if respAfter, okAfter, _, errAfter := matchCommand(context.Background(), logger, selectReq, db, dc, nil, nil); errAfter != nil || !okAfter || respAfter == nil {
				t.Fatalf("autocommit SELECT after COMMIT must hit read-through again: ok=%v resp=%v err=%v", okAfter, respAfter, errAfter)
			}
		})
	}
}
