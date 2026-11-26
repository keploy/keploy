package recorder

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase"
	connphase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

func cannedSelectResultSet(t *testing.T, val string, statusFlags uint16) []byte {
	t.Helper()
	col := columnPayload(t, &mysql.ColumnDefinition41{
		Catalog: "def", Schema: "app", Table: "settings", OrgTable: "settings",
		Name: "val", OrgName: "val", FixedLength: 0x0c, CharacterSet: 0x21,
		ColumnLength: 64, Type: byte(mysql.FieldTypeVarString),
	})
	eofAfterCols := []byte{0xfe, 0x00, 0x00, 0x02, 0x00}
	eofFinal := []byte{0xfe, 0x00, 0x00, byte(statusFlags), byte(statusFlags >> 8)}
	pkts := frame([][]byte{
		{1},
		col,
		eofAfterCols,
		textRowPayload(val),
		eofFinal,
	})
	var out []byte
	for _, p := range pkts {
		out = append(out, p...)
	}
	return out
}

func cannedHandshakeResponse41WithDB(t *testing.T, seq byte, db string) []byte {
	t.Helper()
	caps := uint32(mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SECURE_CONNECTION | mysql.CLIENT_CONNECT_WITH_DB)
	hr := &mysql.HandshakeResponse41Packet{
		CapabilityFlags: caps,
		MaxPacketSize:   1 << 24,
		CharacterSet:    0x21,
		Username:        "root",
		AuthResponse:    bytes.Repeat([]byte{0xAB}, 20),
		Database:        db,
		AuthPluginName:  string(mysql.Native),
	}
	buf, err := connphase.EncodeHandshakeResponse41(context.Background(), zap.NewNop(), hr)
	if err != nil {
		t.Fatalf("encode handshake response 41: %v", err)
	}
	return wrapPacket(buf, seq)
}

func cannedCOMInitDB(seq byte, schema string) []byte {
	body := make([]byte, 1+len(schema))
	body[0] = mysql.COM_INIT_DB
	copy(body[1:], schema)
	return wrapPacket(body, seq)
}

func cannedERR(t *testing.T, seq byte, code uint16, msg string, serverCaps uint32) []byte {
	t.Helper()
	errPkt := &mysql.ERRPacket{
		Header:         mysql.ERR,
		ErrorCode:      code,
		SQLStateMarker: "#",
		SQLState:       "42000",
		ErrorMessage:   msg,
	}
	payload, err := phase.EncodeErr(context.Background(), errPkt, serverCaps)
	if err != nil {
		t.Fatalf("encode err: %v", err)
	}
	return wrapPacket(payload, seq)
}

// TestRecordV2_StampsDatabaseMetadataFromHandshakeInitDBAndUse verifies that
// V2 MySQL recording tracks the connection's active database across the login
// HandshakeResponse41, COM_INIT_DB, and USE <db> COM_QUERY (including inline
// comments and backticked identifiers), ignores failed switches returning ERR,
// and fails closed (databaseUnknown="true") on COM_CHANGE_USER, multi-statement
// queries, and unparseable USE statements until an explicit switch restores it.
func TestRecordV2_StampsDatabaseMetadataFromHandshakeInitDBAndUse(t *testing.T) {
	h := newV2Harness(t)
	caps := uint32(mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SECURE_CONNECTION | mysql.CLIENT_CONNECT_WITH_DB)
	base := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	step := 0
	at := func() time.Time {
		step++
		return base.Add(time.Duration(step) * 5 * time.Millisecond)
	}

	// 1. Handshake with initial database = "tenant_a".
	h.pushDest(cannedHandshakeV10(t), at())
	h.pushClient(cannedHandshakeResponse41WithDB(t, 1, "tenant_a"), at())
	h.pushDest(cannedOK(t, 2, caps), at())

	// 2. Query 1 under tenant_a.
	h.pushClient(cannedCOMQuery(t, 0, "SELECT * FROM settings LIMIT 1"), at())
	h.pushDest(cannedSelectResultSet(t, "a", 0x0002), at())

	// 3. Switch via COM_INIT_DB to tenant_b (OK).
	h.pushClient(cannedCOMInitDB(0, "tenant_b"), at())
	h.pushDest(cannedOK(t, 1, caps), at())

	// 4. Failed COM_INIT_DB (ERR) must NOT overwrite tenant_b.
	h.pushClient(cannedCOMInitDB(0, "no_such_db"), at())
	h.pushDest(cannedERR(t, 1, 1049, "Unknown database 'no_such_db'", caps), at())

	// 5. Query 2 under tenant_b.
	h.pushClient(cannedCOMQuery(t, 0, "SELECT * FROM settings LIMIT 1"), at())
	h.pushDest(cannedSelectResultSet(t, "b", 0x0002), at())

	// 6. Switch via USE with inline & trailing comments: USE /*x*/ tenant_c -- switch.
	h.pushClient(cannedCOMQuery(t, 0, "USE /*x*/ tenant_c -- switch"), at())
	h.pushDest(cannedOK(t, 1, caps), at())

	// 7. Failed USE bad_db (ERR) must NOT overwrite tenant_c.
	h.pushClient(cannedCOMQuery(t, 0, "USE bad_db"), at())
	h.pushDest(cannedERR(t, 1, 1049, "Unknown database 'bad_db'", caps), at())

	// 8. Query 3 under tenant_c.
	h.pushClient(cannedCOMQuery(t, 0, "SELECT * FROM settings LIMIT 1"), at())
	h.pushDest(cannedSelectResultSet(t, "c", 0x0002), at())

	// 9. Switch via backticked name containing semicolon: USE `a;b`.
	h.pushClient(cannedCOMQuery(t, 0, "USE `a;b`"), at())
	h.pushDest(cannedOK(t, 1, caps), at())

	// 10. Query 4 under `a;b`.
	h.pushClient(cannedCOMQuery(t, 0, "SELECT * FROM settings LIMIT 1"), at())
	h.pushDest(cannedSelectResultSet(t, "ab", 0x0002), at())

	// 11. Multi-statement batch answered with real wire SERVER_MORE_RESULTS_EXISTS
	//     (0x0008) OK + second OK hits moreResults -> errNotRecorded -> continue,
	//     and MUST still trip databaseUnknown=true fail-closed!
	moreResultsOK, err := phase.EncodeOk(context.Background(), &mysql.OKPacket{Header: mysql.OK, StatusFlags: 0x000a}, caps)
	if err != nil {
		t.Fatalf("encode more-results ok: %v", err)
	}
	h.pushClient(cannedCOMQuery(t, 0, "USE tenant_d; SELECT 1"), at())
	h.pushDest(wrapPacket(moreResultsOK, 1), at())
	h.pushDest(cannedOK(t, 2, caps), at())

	// 12. Query 5 while databaseUnknown=true MUST be rejected by ReadThroughKey.
	h.pushClient(cannedCOMQuery(t, 0, "SELECT * FROM settings LIMIT 1"), at())
	h.pushDest(cannedSelectResultSet(t, "unk", 0x0002), at())

	// 13. Recovery via explicit COM_INIT_DB restores known state.
	h.pushClient(cannedCOMInitDB(0, "tenant_e"), at())
	h.pushDest(cannedOK(t, 1, caps), at())

	// 14. Query 6 under tenant_e succeeds again.
	h.pushClient(cannedCOMQuery(t, 0, "SELECT * FROM settings LIMIT 1"), at())
	h.pushDest(cannedSelectResultSet(t, "e", 0x0002), at())

	// 15. COM_CHANGE_USER answered with AuthMoreData (0x01 + data) + OK hits
	//     framing-lost re-align (fail()) and MUST mark databaseUnknown=true so
	//     the subsequent SELECT fails closed instead of inheriting tenant_e!
	changeUserPayload := []byte{mysql.COM_CHANGE_USER, 'a', 'p', 'p', 0x00, 0x00, 't', 'e', 'n', 'a', 'n', 't', '_', 'f', 0x00}
	h.pushClient(wrapPacket(changeUserPayload, 0), at())
	h.pushDest(wrapPacket([]byte{0x01, 0x03}, 1), at())
	h.pushDest(cannedOK(t, 2, caps), at())

	// 16. Query 7 after COM_CHANGE_USER re-align MUST carry databaseUnknown=true,
	//     followed by a confirming COM_PING so the taken-up exchange emits.
	h.pushClient(cannedCOMQuery(t, 0, "SELECT * FROM settings LIMIT 1"), at())
	h.pushDest(cannedSelectResultSet(t, "unk2", 0x0002), at())
	h.pushClient(wrapPacket([]byte{mysql.COM_PING}, 0), at())
	h.pushDest(cannedOK(t, 1, caps), at())

	h.closeStreams()

	if err := RecordV2(context.Background(), h.logger, h.sess); err != nil {
		t.Fatalf("RecordV2: %v", err)
	}

	var queryMocks []*models.Mock
	for len(h.mocks) > 0 {
		m := <-h.mocks
		if len(m.Spec.MySQLRequests) == 1 {
			if qp, ok := m.Spec.MySQLRequests[0].Message.(*mysql.QueryPacket); ok && qp.Query == "SELECT * FROM settings LIMIT 1" {
				queryMocks = append(queryMocks, m)
			}
		}
	}

	type wantRow struct {
		db      string
		unknown bool
	}
	wantRows := []wantRow{
		{db: "tenant_a"},
		{db: "tenant_b"},
		{db: "tenant_c"},
		{db: "a;b"},
		{unknown: true},
		{db: "tenant_e"},
		{unknown: true},
	}
	if len(queryMocks) != len(wantRows) {
		t.Fatalf("expected %d SELECT mocks, got %d", len(wantRows), len(queryMocks))
	}
	keys := make(map[string]string, len(wantRows))
	for i, w := range wantRows {
		gotDB := queryMocks[i].Spec.Metadata["database"]
		gotUnk := queryMocks[i].Spec.Metadata["databaseUnknown"] == "true"
		if gotUnk != w.unknown || gotDB != w.db {
			t.Fatalf("query #%d metadata = (db=%q unknown=%v), want (db=%q unknown=%v)", i+1, gotDB, gotUnk, w.db, w.unknown)
		}
		k, ok := models.ReadThroughKey(queryMocks[i])
		if w.unknown {
			if ok {
				t.Fatalf("query #%d with databaseUnknown=true MUST be rejected by ReadThroughKey", i+1)
			}
			continue
		}
		if !ok || k == "" {
			t.Fatalf("query #%d (%s) unexpectedly ineligible for read-through", i+1, w.db)
		}
		if prev, dup := keys[k]; dup {
			t.Fatalf("identical SELECT across %s and %s collided on key %q", prev, w.db, k)
		}
		keys[k] = w.db
	}
}

// TestDecodeContext_DatabaseUnknownFailClosed verifies every fail-closed
// transition on wire.DecodeContext: COM_CHANGE_USER, unparseable USE, and
// multi-statement queries all set DatabaseUnknown=true, while clean COM_INIT_DB
// and USE (with comments/backticks) restore known state.
func TestDecodeContext_DatabaseUnknownFailClosed(t *testing.T) {
	okResp := &mysql.PacketBundle{Message: &mysql.OKPacket{}}

	t.Run("COM_CHANGE_USER", func(t *testing.T) {
		dc := &wire.DecodeContext{Database: "tenant_a"}
		dc.UpdateDatabaseFromCommand(&mysql.PacketBundle{Message: &mysql.ChangeUserPacket{Command: mysql.COM_CHANGE_USER}}, okResp)
		if !dc.DatabaseUnknown || dc.Database != "" {
			t.Fatalf("COM_CHANGE_USER must fail closed to DatabaseUnknown=true, got db=%q unknown=%v", dc.Database, dc.DatabaseUnknown)
		}
	})

	t.Run("unparseable-USE-and-executable-comment", func(t *testing.T) {
		for _, q := range []string{
			"USE tenant_b extra_token",
			"USE /*!",
			"/*!50000 USE tenant_b */",
			"SELECT 1 /*!50000 ; USE tenant_b */",
			"USE `unterminated",
			"USE ",
		} {
			dc := &wire.DecodeContext{Database: "tenant_a"}
			dc.UpdateDatabaseFromCommand(&mysql.PacketBundle{Message: &mysql.QueryPacket{Query: q}}, okResp)
			if !dc.DatabaseUnknown || dc.Database != "" {
				t.Fatalf("unparseable/executable %q must fail closed to DatabaseUnknown=true, got db=%q unknown=%v", q, dc.Database, dc.DatabaseUnknown)
			}
		}
	})

	t.Run("multi-statement", func(t *testing.T) {
		dc := &wire.DecodeContext{Database: "tenant_a"}
		dc.UpdateDatabaseFromCommand(&mysql.PacketBundle{Message: &mysql.QueryPacket{Query: "SELECT 1; USE tenant_b"}}, okResp)
		if !dc.DatabaseUnknown || dc.Database != "" {
			t.Fatalf("multi-statement query must fail closed to DatabaseUnknown=true, got db=%q unknown=%v", dc.Database, dc.DatabaseUnknown)
		}
	})

	t.Run("mid-stream-join-emits-databaseUnknown", func(t *testing.T) {
		// Mirror v2_test.go seq==0 mid-stream post-TLS join: no HandshakeResponse41
		// was observed so emitted SELECT mocks must carry databaseUnknown="true".
		h := newV2Harness(t)
		base := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
		handshakeBuf := cannedHandshakeV10(t)
		greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), handshakeBuf[4:])
		if err != nil {
			t.Fatalf("decode handshake v10: %v", err)
		}
		sslReq := cannedSSLRequest(t, 1)

		// seq==0 command immediately on decrypted stream (no HandshakeResponse41).
		h.pushClient(cannedCOMQuery(t, 0, "SELECT * FROM settings LIMIT 1"), base.Add(20*time.Millisecond))
		h.pushDest(cannedOK(t, 1, greeting.CapabilityFlags), base.Add(25*time.Millisecond))

		h.sess.Opts.NetNS = testNetNS
		ctx := postTLSCtx(t, handshakeBuf, sslReq, base, 3306)
		store, _ := ctx.Value(models.TLSHandshakeStoreKey).(*models.TLSHandshakeStore)
		store.RememberLast(lastGreetingKey(h.sess.Opts.PassThroughScope, h.sess.Opts.NetNS, h.sess.Opts.DstCfg), models.TLSHandshakeEntry{
			RespPackets:  [][]byte{handshakeBuf},
			ReqPackets:   [][]byte{sslReq},
			ReqTimestamp: base,
		})
		got := runPostTLS(t, h, ctx, 2)

		var foundSelect *models.Mock
		for _, m := range got {
			if len(m.Spec.MySQLRequests) == 1 {
				if qp, ok := m.Spec.MySQLRequests[0].Message.(*mysql.QueryPacket); ok && qp.Query == "SELECT * FROM settings LIMIT 1" {
					foundSelect = m
				}
			}
		}
		if foundSelect == nil {
			t.Fatal("expected mid-stream SELECT mock to be recorded")
		}
		if foundSelect.Spec.Metadata["databaseUnknown"] != "true" {
			t.Fatalf("mid-stream join mock must carry databaseUnknown=true, got %+v", foundSelect.Spec.Metadata)
		}
		if _, ok := models.ReadThroughKey(foundSelect); ok {
			t.Fatal("mid-stream join MySQL mock must NOT enter read-through tier")
		}
	})
}

// TestRecordV2_RejectsInTransactionMySQLReadThroughAndStampsDestPort verifies:
//   - A sampled-out BEGIN; INSERT ...; SELECT ...; ROLLBACK whose SELECT result
//     set carries SERVER_STATUS_IN_TRANS (0x0001) is stamped inTx="true",
//     rejected by ReadThroughKey, and NOT rescued by SyncMockManager.
//   - An autocommit SELECT (SERVER_STATUS_IN_TRANS clear, 0x0002) is NOT
//     stamped inTx, passes ReadThroughKey, and IS rescued into read-through.
//   - Destination port is derived consistently from DstCfg.Port on both record
//     (meta["destPort"]) and replay (DecodeContext.SetDestPortFromCfg), even
//     when DstCfg.AddrFabricated is true.
func TestRecordV2_RejectsInTransactionMySQLReadThroughAndStampsDestPort(t *testing.T) {
	h := newV2Harness(t)
	h.sess.Opts.DstCfg = &models.ConditionalDstCfg{
		Addr:           "127.0.0.1:0",
		Port:           3307,
		AddrFabricated: true,
	}
	caps := uint32(mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SECURE_CONNECTION | mysql.CLIENT_CONNECT_WITH_DB)
	base := time.Now().Add(-30 * time.Second)
	step := 0
	at := func() time.Time {
		step++
		return base.Add(time.Duration(step) * 5 * time.Millisecond)
	}

	okInTx, err := phase.EncodeOk(context.Background(), &mysql.OKPacket{Header: mysql.OK, StatusFlags: 0x0003}, caps)
	if err != nil {
		t.Fatalf("encode ok in-tx: %v", err)
	}

	// 1. Handshake with database = "appdb".
	h.pushDest(cannedHandshakeV10(t), at())
	h.pushClient(cannedHandshakeResponse41WithDB(t, 1, "appdb"), at())
	h.pushDest(cannedOK(t, 2, caps), at())

	// 2. Transactional sequence: BEGIN; INSERT X; SELECT X; ROLLBACK.
	//    The SELECT terminator carries SERVER_STATUS_IN_TRANS (0x0003).
	h.pushClient(cannedCOMQuery(t, 0, "BEGIN"), at())
	h.pushDest(wrapPacket(okInTx, 1), at())
	h.pushClient(cannedCOMQuery(t, 0, "INSERT INTO items(id) VALUES (42)"), at())
	h.pushDest(wrapPacket(okInTx, 1), at())
	h.pushClient(cannedCOMQuery(t, 0, "SELECT val FROM items WHERE id = 42"), at())
	h.pushDest(cannedSelectResultSet(t, "uncommitted-row", 0x0003), at())
	h.pushClient(cannedCOMQuery(t, 0, "ROLLBACK"), at())
	h.pushDest(cannedOK(t, 1, caps), at())

	// 3. Autocommit SELECT outside any transaction (0x0002, IN_TRANS clear).
	h.pushClient(cannedCOMQuery(t, 0, "SELECT val FROM settings WHERE id = 1"), at())
	h.pushDest(cannedSelectResultSet(t, "committed-cfg", 0x0002), at())

	h.closeStreams()
	if err := RecordV2(context.Background(), h.logger, h.sess); err != nil {
		t.Fatalf("RecordV2: %v", err)
	}

	var inTxSelect, autoSelect *models.Mock
	for len(h.mocks) > 0 {
		m := <-h.mocks
		if len(m.Spec.MySQLRequests) != 1 {
			continue
		}
		qp, ok := m.Spec.MySQLRequests[0].Message.(*mysql.QueryPacket)
		if !ok {
			continue
		}
		switch qp.Query {
		case "SELECT val FROM items WHERE id = 42":
			inTxSelect = m
		case "SELECT val FROM settings WHERE id = 1":
			autoSelect = m
		}
	}
	if inTxSelect == nil || autoSelect == nil {
		t.Fatalf("expected both SELECT mocks recorded, got inTx=%v auto=%v", inTxSelect != nil, autoSelect != nil)
	}

	// Recorder stamps inTx="true" on the transactional SELECT and leaves it unset on autocommit.
	if inTxSelect.Spec.Metadata["inTx"] != "true" {
		t.Fatalf("in-tx SELECT must carry metadata inTx=\"true\", got %+v", inTxSelect.Spec.Metadata)
	}
	if autoSelect.Spec.Metadata["inTx"] != "" {
		t.Fatalf("autocommit SELECT must not carry inTx metadata, got %+v", autoSelect.Spec.Metadata)
	}
	// Recorder stamps destPort="3307" from DstCfg.Port even when AddrFabricated=true,
	// matching DecodeContext.SetDestPortFromCfg on replay.
	if autoSelect.Spec.Metadata["destPort"] != "3307" {
		t.Fatalf("expected destPort=\"3307\" from DstCfg.Port, got %+v", autoSelect.Spec.Metadata)
	}
	replayDC := &wire.DecodeContext{}
	replayDC.SetDestPortFromCfg(h.sess.Opts.DstCfg)
	if replayDC.DestPort != "3307" {
		t.Fatalf("replay DecodeContext.DestPort = %q, want \"3307\" even when AddrFabricated=true", replayDC.DestPort)
	}

	// ReadThroughKey rejects the in-tx SELECT and accepts the autocommit SELECT.
	if _, ok := models.ReadThroughKey(inTxSelect); ok {
		t.Fatal("in-tx SELECT MUST be rejected by ReadThroughKey")
	}
	recKey, ok := models.ReadThroughKey(autoSelect)
	if !ok || recKey == "" {
		t.Fatal("autocommit SELECT must pass ReadThroughKey")
	}
	liveKey, ok := models.ReadThroughRequestKey(&models.Mock{
		Kind: models.MySQL,
		Spec: models.MockSpec{
			Metadata:      map[string]string{"destPort": replayDC.DestPort, "database": "appdb"},
			MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{Message: &mysql.QueryPacket{Query: "SELECT val FROM settings WHERE id = 1"}}}},
		},
	})
	if !ok || liveKey != recKey {
		t.Fatalf("replay key (%q,%v) must match recorded key (%q) under fabricated DstCfg", liveKey, ok, recKey)
	}

	// Feed both sampled-out SELECTs through an isolated SyncMockManager past
	// the startup window: ONLY the autocommit SELECT may be rescued into read-through.
	mgr := syncMock.New(nil)
	mgr.StartRecordSession()
	outCh := make(chan *models.Mock, 16)
	mapCh := make(chan models.TestMockMapping, 16)
	mgr.SetOutputChannel(outCh)
	mgr.SetMappingChannel(context.Background(), mapCh)
	mgr.SetFirstRequestSignaled()
	for i := 0; i < models.StartupMockTestCaseWindow; i++ {
		tBoot := time.Now().Add(-2 * time.Minute).Add(time.Duration(i) * time.Second)
		mgr.ResolveRange(tBoot, tBoot.Add(10*time.Millisecond), fmt.Sprintf("boot-%d", i), true, true)
	}
	mgr.AddMock(inTxSelect)
	mgr.AddMock(autoSelect)
	winStart := time.Now().Add(-200 * time.Millisecond)
	mgr.ResolveRange(winStart, time.Now(), "kept-after", true, true)
	mgr.CloseOutChan()

	var rescuedQueries []string
	for m := range outCh {
		if m.IsReadThrough() && len(m.Spec.MySQLRequests) == 1 {
			if qp, ok := m.Spec.MySQLRequests[0].Message.(*mysql.QueryPacket); ok {
				rescuedQueries = append(rescuedQueries, qp.Query)
			}
		}
	}
	if len(rescuedQueries) != 1 || rescuedQueries[0] != "SELECT val FROM settings WHERE id = 1" {
		t.Fatalf("expected only autocommit SELECT rescued into read-through, got %v", rescuedQueries)
	}
}
