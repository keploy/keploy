package replayer

import (
	"context"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// fakeMockDb is a minimal integrations.MockMemDb for exercising matchCommand
// directly. Only the accessors matchCommand touches are backed by real state;
// every other method is a no-op stub. Session-tier mocks are returned from
// GetSessionMocks (matching how lax-mode promotes per-test MySQL data mocks
// into the session pool), and UpdateUnFilteredMock is a no-op (session mocks
// are reusable, never consumed) — which is exactly the condition that made the
// COM_QUERY matcher serve the first recorded row to every test.
type fakeMockDb struct {
	perTest    []*models.Mock
	session    []*models.Mock
	winStart   time.Time
	winEnd     time.Time
	deletedFil []string                // names passed to DeleteFilteredMock
	deletedMk  []models.Mock           // full mocks passed to DeleteFilteredMock
	updatedNew map[string]*models.Mock // name -> updated copy passed to UpdateUnFilteredMock
}

func (f *fakeMockDb) GetSessionMocks() ([]*models.Mock, error)         { return f.session, nil }
func (f *fakeMockDb) GetPerTestMocksInWindow() ([]*models.Mock, error) { return f.perTest, nil }
func (f *fakeMockDb) GetConnectionMocks(string) ([]*models.Mock, error) {
	return nil, nil
}
func (f *fakeMockDb) CurrentTestWindow() (time.Time, time.Time) { return f.winStart, f.winEnd }

// --- remaining MockMemDb surface: inert stubs ---
func (f *fakeMockDb) GetFilteredMocks() ([]*models.Mock, error)         { return nil, nil }
func (f *fakeMockDb) GetUnFilteredMocks() ([]*models.Mock, error)       { return nil, nil }
func (f *fakeMockDb) GetMySQLCounts() (int, int, int)                   { return 0, 0, 0 }
func (f *fakeMockDb) GetFilteredMocksInWindow() ([]*models.Mock, error) { return nil, nil }
func (f *fakeMockDb) GetStartupMocks() ([]*models.Mock, error)          { return nil, nil }
func (f *fakeMockDb) GetStartupMocksByKind(models.Kind) ([]*models.Mock, error) {
	return nil, nil
}
func (f *fakeMockDb) GetSessionScopedMocks() ([]*models.Mock, error) { return nil, nil }
func (f *fakeMockDb) SessionMockHitCounts() map[string]uint64        { return nil }
func (f *fakeMockDb) UpdateUnFilteredMock(_ *models.Mock, n *models.Mock) bool {
	if n != nil {
		if f.updatedNew == nil {
			f.updatedNew = map[string]*models.Mock{}
		}
		f.updatedNew[n.Name] = n
	}
	return true // session mocks are reusable: re-stamped, not removed
}
func (f *fakeMockDb) DeleteFilteredMock(m models.Mock) bool {
	f.deletedFil = append(f.deletedFil, m.Name)
	f.deletedMk = append(f.deletedMk, m)
	// Real semantics: a per-test mock present in the filtered pool is consumed
	// (removed) and returns true; a per-test mock that has been lax-staged into
	// the session pool is not in the filtered tree, so DeleteFilteredMock misses
	// (returns false) and updateMock falls back to UpdateUnFilteredMock — which
	// is exactly the un-consumed condition the COM_QUERY in-window fix targets.
	for i, pm := range f.perTest {
		if pm.Name == m.Name {
			f.perTest = append(f.perTest[:i], f.perTest[i+1:]...)
			return true
		}
	}
	return false
}
func (f *fakeMockDb) DeleteUnFilteredMock(models.Mock) bool     { return false }
func (f *fakeMockDb) DeleteStartupMock(models.Mock) bool        { return false }
func (f *fakeMockDb) MarkMockAsUsed(models.Mock) bool           { return true }
func (f *fakeMockDb) SetCurrentTestWindow(time.Time, time.Time) {}
func (f *fakeMockDb) IsTestWindowActive() bool                  { return !f.winStart.IsZero() }
func (f *fakeMockDb) HasFirstTestFired() bool                   { return true }
func (f *fakeMockDb) FirstTestWindowStart() time.Time           { return f.winStart }
func (f *fakeMockDb) WindowSnapshot() models.WindowSnapshot     { return models.WindowSnapshot{} }

// readbackMock builds a session-tier COM_QUERY data mock: same SQL text across
// all of them, a distinct response payload marker, and a recorded request
// timestamp so the matcher can window-discriminate.
func readbackMock(name, sql, marker string, reqTs time.Time) *models.Mock {
	const payloadLen = 64
	m := &models.Mock{
		Name: name,
		Kind: models.MySQL,
	}
	m.TestModeInfo.Lifetime = models.LifetimeSession // lax-promoted data mock
	m.Spec.Metadata = map[string]string{"type": "mocks"}
	m.Spec.ReqTimestampMock = reqTs
	m.Spec.MySQLRequests = []mysql.Request{{
		PacketBundle: mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: payloadLen, SequenceID: 0}, Type: "COM_QUERY"},
			Message: &mysql.QueryPacket{Query: sql},
		},
	}}
	m.Spec.MySQLResponses = []mysql.Response{{
		PacketBundle: mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: payloadLen, SequenceID: 1}, Type: "TextResultSet"},
			Message: &mysql.TextResultSet{},
		},
		Payload: marker, // distinguishes which recorded row was served
	}}
	return m
}

func newDecodeCtx() *wire.DecodeContext {
	return &wire.DecodeContext{
		PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{},
		StmtIDToQuery:      map[uint32]string{},
		NextStmtID:         1,
	}
}

func comQueryReq(sql string) mysql.Request {
	return mysql.Request{
		PacketBundle: mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 64, SequenceID: 0}, Type: "COM_QUERY"},
			Message: &mysql.QueryPacket{Query: sql},
		},
	}
}

func stmtPrepareReq(sql string) mysql.Request {
	return mysql.Request{
		PacketBundle: mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: uint32(len(sql) + 1), SequenceID: 0}, Type: "COM_STMT_PREPARE"},
			Message: &mysql.StmtPreparePacket{Command: 0x16, Query: sql},
		},
	}
}

// TestMatchCommand_OrphanPrepareSynthesizesPrepareOk is the regression guard for
// the prepared-statement orphan fix (keploy#4226): JDBC cachePrepStmts reuses a
// server stmtID allocated before the record window, so the recorder captures an
// EXECUTE with no matching COM_STMT_PREPARE. On replay the cold-cache driver
// DOES send COM_STMT_PREPARE; with no PREPARE mock to match, matchCommand must
// synthesize a COM_STMT_PREPARE_OK (and register the stmtID→query mapping so the
// following EXECUTE resolves) instead of dropping the connection with
// "no matching mock found".
func TestMatchCommand_OrphanPrepareSynthesizesPrepareOk(t *testing.T) {
	logger := zap.NewNop()
	const sql = "SELECT ? AS v"

	// Pool is non-empty (an unrelated reusable mock) but contains NO PREPARE
	// mock for this query — the orphaned condition.
	filler := readbackMock("unrelated", "SELECT 1", "x", time.Time{})
	db := &fakeMockDb{session: []*models.Mock{filler}}
	dctx := newDecodeCtx()

	resp, ok, _, _, err := matchCommand(context.Background(), logger, stmtPrepareReq(sql), db, dctx, false, false)
	if err != nil || !ok || resp == nil {
		t.Fatalf("orphaned PREPARE must synthesize a PREPARE_OK, got ok=%v err=%v resp=%v", ok, err, resp)
	}
	prepOk, isPrepOk := resp.Message.(*mysql.StmtPrepareOkPacket)
	if !isPrepOk {
		t.Fatalf("expected a *StmtPrepareOkPacket response, got %T", resp.Message)
	}
	if prepOk.NumParams != 1 {
		t.Errorf("synthetic PREPARE_OK NumParams: want 1 (one '?'), got %d", prepOk.NumParams)
	}
	if len(prepOk.ParamDefs) != 1 {
		t.Errorf("synthetic PREPARE_OK must carry one placeholder ColumnDefinition41 per '?', got %d", len(prepOk.ParamDefs))
	}
	// The stmtID→query mapping must be wired so the following EXECUTE resolves.
	if got := dctx.StmtIDToQuery[prepOk.StatementID]; got != sql {
		t.Errorf("StmtIDToQuery[%d] = %q, want %q", prepOk.StatementID, got, sql)
	}
	if _, present := dctx.PreparedStatements[prepOk.StatementID]; !present {
		t.Errorf("synthetic stmtID %d not registered in PreparedStatements", prepOk.StatementID)
	}
}

// TestMatchCommand_SendLongDataAcceptedGracefully is the regression guard for
// the COM_STMT_SEND_LONG_DATA connection-drop fix. Connector/J streams a
// setBinaryStream/setBlob parameter as COM_STMT_SEND_LONG_DATA (a no-response
// command) before COM_STMT_EXECUTE. The matcher has no per-mock comparison for
// it; without graceful handling matchCommand returns ok=false and query.go
// drops the connection ("no matching mock") BEFORE its IsNoResponseCommand
// check — surfacing as SQLSTATE 08S01. matchCommand must instead accept it
// (ok=true) so the no-response path can continue.
func TestMatchCommand_SendLongDataAcceptedGracefully(t *testing.T) {
	logger := zap.NewNop()

	// Pool is non-empty but holds no mock for the long-data packet.
	db := &fakeMockDb{session: []*models.Mock{readbackMock("unrelated", "SELECT 1", "x", time.Time{})}}

	req := mysql.Request{
		PacketBundle: mysql.PacketBundle{
			Header: &mysql.PacketInfo{
				Header: &mysql.Header{PayloadLength: 12, SequenceID: 0},
				Type:   "COM_STMT_SEND_LONG_DATA",
			},
			Message: &mysql.StmtSendLongDataPacket{StatementID: 3, ParameterID: 0},
		},
	}
	_, ok, _, _, err := matchCommand(context.Background(), logger, req, db, newDecodeCtx(), false, false)
	if err != nil {
		t.Fatalf("unmocked COM_STMT_SEND_LONG_DATA must not error, got %v", err)
	}
	if !ok {
		t.Errorf("unmocked COM_STMT_SEND_LONG_DATA must be accepted (ok=true) to avoid dropping the connection")
	}
}

// sldMock builds a per-test, no-response COM_STMT_SEND_LONG_DATA data mock.
func sldMock(name string, reqTs time.Time) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.MySQL}
	m.TestModeInfo.Lifetime = models.LifetimePerTest
	m.Spec.Metadata = map[string]string{"type": "mocks"}
	m.Spec.ReqTimestampMock = reqTs
	m.Spec.MySQLRequests = []mysql.Request{{
		PacketBundle: mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 12, SequenceID: 0}, Type: "COM_STMT_SEND_LONG_DATA"},
			Message: &mysql.StmtSendLongDataPacket{StatementID: 3, ParameterID: 0},
		},
	}}
	return m
}

// TestMatchCommand_SendLongDataConsumesRecordedMock verifies the SEND_LONG_DATA
// fallback consumes a recorded SLD mock when one exists (so it isn't flagged
// unused / pruned), while still returning the no-response acceptance.
func TestMatchCommand_SendLongDataConsumesRecordedMock(t *testing.T) {
	logger := zap.NewNop()
	base := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	db := &fakeMockDb{
		perTest:  []*models.Mock{sldMock("sld-1", base)}, // per-test mock routed to the per-test pool
		winStart: base.Add(-time.Second),
		winEnd:   base.Add(time.Second), // sld-1 is in-window
	}
	req := mysql.Request{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 12, SequenceID: 0}, Type: "COM_STMT_SEND_LONG_DATA"},
		Message: &mysql.StmtSendLongDataPacket{StatementID: 3},
	}}
	_, ok, _, _, err := matchCommand(context.Background(), logger, req, db, newDecodeCtx(), false, false)
	if err != nil || !ok {
		t.Fatalf("SLD must be accepted, got ok=%v err=%v", ok, err)
	}
	consumed := false
	for _, n := range db.deletedFil {
		if n == "sld-1" {
			consumed = true
		}
	}
	if !consumed {
		t.Errorf("expected the recorded SLD mock to be consumed (DeleteFilteredMock), deletedFil=%v", db.deletedFil)
	}
}

// TestMatchCommand_ComQueryStatefulReadInWindow is the regression guard for the
// TiDB/Connector-J stateful read-back bug: a
// parameterless statement that issues the SAME SQL text across tests but
// returns a DIFFERENT row each time (e.g. an INSERT read-back
// "SELECT v FROM kv ORDER BY id DESC LIMIT 1"). The data mocks are
// session-promoted (reusable, never consumed), so the matcher used to serve
// the FIRST recorded row to every test. matchCommand must instead prefer the
// exact-text match recorded INSIDE the active test window.
func TestMatchCommand_ComQueryStatefulReadInWindow(t *testing.T) {
	logger := zap.NewNop()
	const sql = "SELECT v FROM kv ORDER BY id DESC LIMIT 1"

	base := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	t1 := base
	t2 := base.Add(10 * time.Second)
	t3 := base.Add(20 * time.Second)

	mocks := []*models.Mock{
		readbackMock("readback-1", sql, "row=1", t1),
		readbackMock("readback-2", sql, "row=2", t2),
		readbackMock("readback-3", sql, "row=3", t3),
	}

	t.Run("in-window match preferred over first-recorded", func(t *testing.T) {
		db := &fakeMockDb{
			session:  mocks,
			winStart: base.Add(5 * time.Second),  // [t1 < winStart < t2 < winEnd < t3]
			winEnd:   base.Add(15 * time.Second), // only readback-2 is in-window
		}
		resp, ok, _, _, err := matchCommand(context.Background(), logger, comQueryReq(sql), db, newDecodeCtx(), false, false)
		if err != nil || !ok || resp == nil {
			t.Fatalf("expected a match, got ok=%v err=%v resp=%v", ok, err, resp)
		}
		if resp.Payload != "row=2" {
			t.Errorf("expected the in-window row (row=2), got %q — the matcher served a stale/first row", resp.Payload)
		}
	})

	t.Run("no active window falls back to first-recorded (no behavior change)", func(t *testing.T) {
		db := &fakeMockDb{session: mocks} // zero window => windowActive == false
		resp, ok, _, _, err := matchCommand(context.Background(), logger, comQueryReq(sql), db, newDecodeCtx(), false, false)
		if err != nil || !ok || resp == nil {
			t.Fatalf("expected a match, got ok=%v err=%v resp=%v", ok, err, resp)
		}
		if resp.Payload != "row=1" {
			t.Errorf("with no active window the legacy first-exact-match wins; expected row=1, got %q", resp.Payload)
		}
	})

	t.Run("match recorded outside window still resolves via fallback", func(t *testing.T) {
		// Active window that contains NONE of the recorded read-backs: the
		// single reusable recording must still be served (out-of-window
		// fallback), not dropped as a miss.
		db := &fakeMockDb{
			session:  []*models.Mock{readbackMock("only", sql, "row=only", t1)},
			winStart: base.Add(100 * time.Second),
			winEnd:   base.Add(200 * time.Second),
		}
		resp, ok, _, _, err := matchCommand(context.Background(), logger, comQueryReq(sql), db, newDecodeCtx(), false, false)
		if err != nil || !ok || resp == nil {
			t.Fatalf("expected the reusable recording to still match, got ok=%v err=%v", ok, err)
		}
		if resp.Payload != "row=only" {
			t.Errorf("expected out-of-window fallback to serve row=only, got %q", resp.Payload)
		}
	})
}

// dmlMock builds a per-test COM_QUERY DML mock (consumable via
// DeleteFilteredMock) carrying the given recorded SQL and optional learned
// QueryNoise. Equal payload length to the live request drives the matcher into
// the structure-equal branch.
func dmlMock(name, sql string, learned map[string][]string) *models.Mock {
	m := readbackMock(name, sql, "served", time.Time{})
	m.TestModeInfo.Lifetime = models.LifetimePerTest
	m.Spec.Metadata = map[string]string{"type": "mocks"}
	m.Spec.MySQLRequests[0].QueryNoise = learned
	return m
}

// TestMatchCommand_ComQueryLiteralNoise exercises the end-to-end COM_QUERY
// request-literal noise wiring through matchCommand: detection learns and
// attaches noise onto the consumed mock; strict enforcement consumes a
// structurally-identical mock iff every literal drift is in the learned-noise
// set; and a WHERE-only drift never matches under strict.
func TestMatchCommand_ComQueryLiteralNoise(t *testing.T) {
	logger := zap.NewNop()
	recorded := "update orders set views=5, updated_at='2026-01-01 12:48:36' where region='north'"
	liveSetDrift := "update orders set views=5, updated_at='2026-06-17 14:00:24' where region='north'"
	liveWhereDrift := "update orders set views=5, updated_at='2026-01-01 12:48:36' where region='south'"

	t.Run("detection attaches learned noise onto consumed mock", func(t *testing.T) {
		mock := dmlMock("upd", recorded, nil)
		db := &fakeMockDb{perTest: []*models.Mock{mock}}

		// Detection ON, strict OFF. The structurally-equal mock is served via
		// the score-based partial path (ok=true overall because matchedResp is
		// set), and the detected updated_at noise must be attached to the mock
		// that DeleteFilteredMock consumed.
		resp, ok, _, _, err := matchCommand(context.Background(), logger, comQueryReq(liveSetDrift), db, newDecodeCtx(), true, false)
		if err != nil || !ok || resp == nil {
			t.Fatalf("expected a served response on detection path, got ok=%v err=%v resp=%v", ok, err, resp)
		}
		if len(db.deletedMk) == 0 {
			t.Fatalf("expected the mock to be consumed via DeleteFilteredMock")
		}
		got := db.deletedMk[0].Spec.MySQLRequests[0].QueryNoise
		want := map[string][]string{"set:updated_at#0": {"2026-01-01 12:48:36"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("detected noise attached to consumed mock mismatch:\n want %v\n got  %v", want, got)
		}
		// The original pooled mock's map must be untouched (fresh copy).
		if mock.Spec.MySQLRequests[0].QueryNoise != nil {
			t.Errorf("detection mutated the shared pooled mock's QueryNoise: %v", mock.Spec.MySQLRequests[0].QueryNoise)
		}
	})

	t.Run("strict consumes mock when only learned-noise literal drifts", func(t *testing.T) {
		learned := map[string][]string{"set:updated_at#0": {"2026-01-01 12:48:36"}}
		db := &fakeMockDb{perTest: []*models.Mock{dmlMock("upd", recorded, learned)}}

		_, ok, _, _, err := matchCommand(context.Background(), logger, comQueryReq(liveSetDrift), db, newDecodeCtx(), false, true)
		if err != nil || !ok {
			t.Fatalf("strict: expected within-noise match to be consumed, got ok=%v err=%v", ok, err)
		}
		if len(db.deletedFil) == 0 {
			t.Errorf("strict within-noise match should consume the mock (DeleteFilteredMock)")
		}
	})

	t.Run("strict rejects WHERE-only drift (never learnable)", func(t *testing.T) {
		learned := map[string][]string{"set:updated_at#0": {"2026-01-01 12:48:36"}}
		db := &fakeMockDb{perTest: []*models.Mock{dmlMock("upd", recorded, learned)}}

		_, ok, _, _, err := matchCommand(context.Background(), logger, comQueryReq(liveWhereDrift), db, newDecodeCtx(), false, true)
		if ok {
			t.Errorf("strict: a changed WHERE predicate must NOT match (err=%v)", err)
		}
	})
}

// TestMatchCommand_ComQueryStrictHardRejectForcesMiss is the regression guard
// for Finding 3: when the live query's structural counterpart is found but
// drifted in a non-noise / predicate literal (a real regression), strict mode
// must force an OVERALL MISS — it must NOT fall back to serving an unrelated
// candidate that merely scored a payload-length partial.
func TestMatchCommand_ComQueryStrictHardRejectForcesMiss(t *testing.T) {
	logger := zap.NewNop()
	// Same skeleton as the live query; only the WHERE region literal drifts ->
	// hard reject under strict (region is non-eligible).
	counterpart := "update orders set views=5, updated_at='2026-01-01 12:48:36' where region='north'"
	live := "update orders set views=5, updated_at='2026-01-01 12:48:36' where region='south'"
	// Unrelated query (different table/columns) that still scores a payload-
	// length partial (matchCount=1). Without the hard-reject, matchCommand would
	// serve THIS as the best partial for the live query.
	unrelated := "update events set code='x' where id=1"

	learned := map[string][]string{"set:updated_at#0": {"2026-01-01 12:48:36"}}
	db := &fakeMockDb{perTest: []*models.Mock{
		dmlMock("unrelated", unrelated, nil),
		dmlMock("counterpart", counterpart, learned),
	}}

	resp, ok, _, _, err := matchCommand(context.Background(), logger, comQueryReq(live), db, newDecodeCtx(), false, true)
	if ok || resp != nil {
		t.Fatalf("strict: WHERE-drift counterpart must force an overall MISS even with a competing partial; got ok=%v resp=%v err=%v", ok, resp, err)
	}
	if len(db.deletedFil) != 0 || len(db.deletedMk) != 0 {
		t.Errorf("nothing should be consumed on a hard-reject miss; deletedFil=%v deletedMk=%d", db.deletedFil, len(db.deletedMk))
	}
}
