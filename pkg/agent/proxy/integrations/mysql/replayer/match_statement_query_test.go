package replayer

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// keploy #4810: the mock prune deleted the COM_STMT_PREPARE of a kept
// COM_STMT_EXECUTE. The EXECUTE's statement then resolved to "", and another
// test's EXECUTE of the same INSERT answered the command: an order's OK in
// place of this order's duplicate-key error.

const ordersInsert = "INSERT INTO orders (idmp_key, created_at) VALUES (?, ?)"

var stmtT0 = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func stmtParams(vals ...string) []mysql.Parameter {
	out := make([]mysql.Parameter, len(vals))
	for i, v := range vals {
		out[i] = mysql.Parameter{Type: 253, Value: v}
	}
	return out
}

// preparedMock is the recorded PREPARE of query on connection connID, answered
// with statement stmtID.
func preparedMock(name, connID string, stmtID uint32, query string, at time.Time) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.MySQL}
	m.TestModeInfo.Lifetime = models.LifetimeConnection
	m.Spec.Metadata = map[string]string{"type": "connection", "connID": connID}
	m.Spec.ReqTimestampMock = at
	m.Spec.MySQLRequests = []mysql.Request{{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: uint32(len(query) + 1)}, Type: "COM_STMT_PREPARE"},
		Message: &mysql.StmtPreparePacket{Command: 0x16, Query: query},
	}}}
	m.Spec.MySQLResponses = []mysql.Response{{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 12, SequenceID: 1}, Type: mysql.COM_STMT_PREPARE_OK},
		Message: &mysql.StmtPrepareOkPacket{StatementID: stmtID, NumParams: 2, WarningAvailable: true},
	}}}
	return m
}

// executedMock is a recorded EXECUTE of statement stmtID on connection "0",
// lax-promoted to the session tier as the k8s agent serves it, answered with
// a response marked marker. stored is the query the recorder kept on it, if
// any.
func executedMock(name string, stmtID uint32, at time.Time, marker, stored string, params ...string) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.MySQL}
	m.TestModeInfo.Lifetime = models.LifetimeSession
	m.Spec.Metadata = map[string]string{"type": "mocks", "connID": "0"}
	m.Spec.ReqTimestampMock = at
	req := mysql.PacketBundle{
		Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 40}, Type: "COM_STMT_EXECUTE"},
		Message: &mysql.StmtExecutePacket{Status: 0x17, StatementID: stmtID, IterationCount: 1, NewParamsBindFlag: 1,
			ParameterCount: len(params), Parameters: stmtParams(params...)},
	}
	req.SetStoredQuery(stored)
	m.Spec.MySQLRequests = []mysql.Request{{PacketBundle: req}}
	m.Spec.MySQLResponses = []mysql.Response{{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 7, SequenceID: 1}, Type: "OK"},
		Message: &mysql.OKPacket{Header: mysql.OK},
	}, Payload: marker}}
	return m
}

// liveExecute is the replayed EXECUTE: statement 33 of the live connection,
// prepared from ordersInsert.
func liveExecute(params ...string) mysql.Request {
	return mysql.Request{PacketBundle: mysql.PacketBundle{
		Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 40}, Type: "COM_STMT_EXECUTE"},
		Message: &mysql.StmtExecutePacket{Status: 0x17, StatementID: 33, IterationCount: 1, NewParamsBindFlag: 1,
			ParameterCount: len(params), Parameters: stmtParams(params...)},
	}}
}

// orderTests is the recording of the issue: test A ordered k1, test B ordered
// k1 again and got ERR 1062. B's PREPARE was pruned; its EXECUTE was kept.
// The replay is in test B's window.
func orderTests(stored string) (*fakeMockDb, *models.Mock, *models.Mock) {
	testA, testB := stmtT0.Add(10*time.Second), stmtT0.Add(20*time.Second)
	prepA := preparedMock("mock-prepare-a", "0", 1, ordersInsert, testA)
	execA := executedMock("mock-execute-a", 1, testA.Add(time.Millisecond), "A: 201", "", "k1", "2026-10-14 12:00:10")
	execB := executedMock("mock-execute-b", 2, testB.Add(time.Millisecond), "B: ERR 1062", stored, "k1", "2026-10-14 12:00:20")
	db := &fakeMockDb{
		session:  []*models.Mock{prepA, execA, execB},
		winStart: testB.Add(-time.Second),
		winEnd:   testB.Add(time.Second),
	}
	return db, execA, execB
}

func matchLiveOrder(t *testing.T, db *fakeMockDb, params ...string) (*mysql.Response, bool, *mockMiss) {
	t.Helper()
	req := liveExecute(params...)
	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[33] = ordersInsert
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	resp, ok, miss, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, nil, nil)
	if err != nil {
		t.Fatalf("matchCommand: %v", err)
	}
	return resp, ok, miss
}

func TestMatchCommand_AnExecuteWhosePrepareWasPrunedIsNotAnsweredByAnotherTest(t *testing.T) {
	db, execA, execB := orderTests("")
	resp, ok, miss := matchLiveOrder(t, db, "k1", "2026-10-15 09:00:00")
	if ok {
		t.Fatalf("served %q: test B's own EXECUTE lost its statement, and another test's mock answered for it", resp.Payload)
	}
	if miss == nil {
		t.Fatal("no miss report")
	}
	if miss.closestMock != execB.Name {
		t.Errorf("the miss names %q as closest, want the EXECUTE that lost its statement, %q", miss.closestMock, execB.Name)
	}
	for _, want := range []string{execB.Name, execA.Name, "COM_STMT_PREPARE"} {
		if !strings.Contains(miss.why, want) {
			t.Errorf("the miss does not say %q: %s", want, miss.why)
		}
	}
	if !strings.Contains(miss.nextSteps, "prune") || !strings.Contains(miss.nextSteps, "Re-record") {
		t.Errorf("the miss does not say why the set lost it or what to do: %s", miss.nextSteps)
	}
}

func TestMatchCommand_AnExecuteThatKeptItsQueryNeedsNoPrepare(t *testing.T) {
	db, _, _ := orderTests(ordersInsert)
	resp, ok, _ := matchLiveOrder(t, db, "k1", "2026-10-15 09:00:00")
	if !ok || resp.Payload != "B: ERR 1062" {
		t.Fatalf("served %v (ok %v), want test B's own EXECUTE, which kept its query", resp, ok)
	}
}

// The refusal is scoped to what may be the command's own mock, and only to
// what would otherwise answer it from another test.
func TestMatchCommand_AnUnknownStatementRefusesOnlyAnotherTestsMock(t *testing.T) {
	t.Run("an exact match of the test's own", func(t *testing.T) {
		db, _, _ := orderTests("")
		own := executedMock("mock-execute-b2", 1, db.winStart.Add(500*time.Millisecond), "B: own", "", "k9", "t9")
		db.session = append(db.session, own)
		resp, ok, _ := matchLiveOrder(t, db, "k9", "t9")
		if !ok || resp.Payload != "B: own" {
			t.Fatalf("served %v (ok %v), want the test's own exact match", resp, ok)
		}
	})
	t.Run("the test's own mock of the query, its parameters drifted", func(t *testing.T) {
		db, _, _ := orderTests("")
		own := executedMock("mock-execute-b2", 1, db.winStart.Add(500*time.Millisecond), "B: own", "", "k9", "t9")
		db.session = append(db.session, own)
		resp, ok, _ := matchLiveOrder(t, db, "k9", "2026-10-15 09:00:00")
		if !ok || resp.Payload != "B: own" {
			t.Fatalf("served %v (ok %v), want the test's own EXECUTE of the query (the in-window choice is the matcher's, as before)", resp, ok)
		}
	})
	t.Run("the unknown statement matched on every parameter", func(t *testing.T) {
		db, _, _ := orderTests("")
		resp, ok, _ := matchLiveOrder(t, db, "k1", "2026-10-14 12:00:20")
		if !ok || resp.Payload != "B: ERR 1062" {
			t.Fatalf("served %v (ok %v), want test B's EXECUTE, which matched on every parameter", resp, ok)
		}
	})
	t.Run("the unknown statement matched on every parameter, beside the test's own mock of the query", func(t *testing.T) {
		db, _, _ := orderTests("")
		own := executedMock("mock-execute-b2", 1, db.winStart.Add(500*time.Millisecond), "B: own", "", "k9", "t9")
		db.session = append(db.session, own)
		resp, ok, _ := matchLiveOrder(t, db, "k1", "2026-10-14 12:00:20")
		if !ok || resp.Payload != "B: ERR 1062" {
			t.Fatalf("served %v (ok %v), want test B's EXECUTE matched on every parameter, ranked above a match on the query alone, as before", resp, ok)
		}
	})
	t.Run("an unknown statement of another parameter count", func(t *testing.T) {
		db, _, execB := orderTests("")
		msg := execB.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket)
		msg.ParameterCount, msg.Parameters = 3, stmtParams("a", "b", "c")
		resp, ok, _ := matchLiveOrder(t, db, "k1", "2026-10-15 09:00:00")
		if !ok || resp.Payload != "A: 201" {
			t.Fatalf("served %v (ok %v): an EXECUTE of three parameters ran another statement, and refused nothing", resp, ok)
		}
	})
	t.Run("an unknown statement of other parameter types", func(t *testing.T) {
		db, _, execB := orderTests("")
		msg := execB.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket)
		msg.Parameters[1].Type = 8 // LONGLONG, where the live command binds a string
		resp, ok, _ := matchLiveOrder(t, db, "k1", "2026-10-15 09:00:00")
		if !ok || resp.Payload != "A: 201" {
			t.Fatalf("served %v (ok %v): an EXECUTE that bound another type ran another statement, and refused nothing", resp, ok)
		}
	})
	t.Run("an unknown statement executed without its types", func(t *testing.T) {
		// new_params_bind_flag 0: the types are not on the wire, and the
		// decoder fills in a default, so they say nothing.
		db, _, execB := orderTests("")
		msg := execB.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket)
		msg.NewParamsBindFlag = 0
		msg.Parameters[1].Type = uint16(mysql.FieldTypeString)
		msg.Parameters[0].Type = uint16(mysql.FieldTypeString)
		if resp, ok, _ := matchLiveOrder(t, db, "k1", "2026-10-15 09:00:00"); ok {
			t.Fatalf("served %q: an EXECUTE sent without its types may have run the live statement", resp.Payload)
		}
	})
	t.Run("another test's unknown statement", func(t *testing.T) {
		db, _, _ := orderTests("")
		testC := stmtT0.Add(30 * time.Second)
		db.winStart, db.winEnd = testC.Add(-time.Second), testC.Add(time.Second)
		resp, ok, _ := matchLiveOrder(t, db, "k1", "2026-10-15 09:00:00")
		if !ok || resp.Payload != "A: 201" {
			t.Fatalf("served %v (ok %v): test B's EXECUTE cannot be test C's own, and refused nothing", resp, ok)
		}
	})
	t.Run("no test window", func(t *testing.T) {
		db, _, _ := orderTests("")
		db.winStart, db.winEnd = time.Time{}, time.Time{}
		if _, ok, _ := matchLiveOrder(t, db, "k1", "2026-10-15 09:00:00"); !ok {
			t.Fatal("refused outside any test window, where no mock belongs to another test")
		}
	})
}

// A command no mock answers is the ordinary miss; when the test's recording
// holds EXECUTEs whose statement is lost, the miss names them too.
func TestMatchCommand_AMissNamesTheTestsUnknownStatements(t *testing.T) {
	for _, tc := range []struct {
		name   string
		window bool
		named  bool
	}{{"in a test window", true, true}, {"outside any", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			db, _, execB := orderTests("")
			// Only the PREPARE and B's EXECUTE are left, and a strict gate
			// that refuses it.
			db.session = []*models.Mock{db.session[0], execB}
			if !tc.window {
				db.winStart, db.winEnd = time.Time{}, time.Time{}
			}
			req := liveExecute("k1", "2026-10-15 09:00:00")
			dctx := newDecodeCtx()
			dctx.StmtIDToQuery[33] = ordersInsert
			ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
			_, ok, miss, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, StrictEngineForTest(), nil)
			if err != nil || ok || miss == nil {
				t.Fatalf("ok %v, err %v, miss %v: want a miss", ok, err, miss)
			}
			if miss.strictRejected == 0 || len(miss.fieldDiffs) == 0 {
				t.Errorf("the miss lost what the strict gate found: rejected %d, diffs %v", miss.strictRejected, miss.fieldDiffs)
			}
			if named := strings.Contains(miss.why, execB.Name); named != tc.named {
				t.Errorf("the miss names the unknown statement: %v, want %v (%q)", named, tc.named, miss.why)
			}
			if miss.nextSteps != "" {
				t.Errorf("the strict gate's advice was replaced: %q", miss.nextSteps)
			}
		})
	}
}

// A recorded CLOSE that kept its query is scored on it, though its PREPARE is
// gone; one that did not is scored on its PREPARE.
func TestMatchCommand_ACloseIsScoredOnTheQueryItKept(t *testing.T) {
	closeMock := func(name string, stmtID uint32, stored string) *models.Mock {
		m := &models.Mock{Name: name, Kind: models.MySQL}
		m.TestModeInfo.Lifetime = models.LifetimePerTest
		m.Spec.Metadata = map[string]string{"type": "mocks", "connID": "0"}
		req := mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 5}, Type: "COM_STMT_CLOSE"},
			Message: &mysql.StmtClosePacket{Status: 0x19, StatementID: stmtID},
		}
		req.SetStoredQuery(stored)
		m.Spec.MySQLRequests = []mysql.Request{{PacketBundle: req}}
		m.Spec.MySQLResponses = []mysql.Response{{}}
		return m
	}
	other := "SELECT idmp_key FROM orders WHERE idmp_key = ?"
	db := &fakeMockDb{
		session: []*models.Mock{preparedMock("mock-prepare-other", "0", 6, other, stmtT0)},
		perTest: []*models.Mock{closeMock("mock-close-other", 6, ""), closeMock("mock-close-insert", 9, ordersInsert)},
	}
	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[33] = ordersInsert
	req := mysql.Request{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 5}, Type: "COM_STMT_CLOSE"},
		Message: &mysql.StmtClosePacket{Status: 0x19, StatementID: 33},
	}}
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	if _, ok, _, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, nil, nil); err != nil || !ok {
		t.Fatalf("matchCommand: ok %v, err %v", ok, err)
	}
	if len(db.deletedFil) != 1 || db.deletedFil[0] != "mock-close-insert" {
		t.Fatalf("consumed %v, want the CLOSE that kept the live statement's query", db.deletedFil)
	}
}

// lenencString is s as a length-encoded string of the binary protocol.
func lenencString(s string) []byte { return append([]byte{byte(len(s))}, s...) }

// TestReplay_AnExecuteWhosePrepareWasPrunedFailsSayingWhy drives the whole
// replayed connection of the issue: the client prepares the INSERT (answered
// by test A's PREPARE, the one left) and executes it in test B's window. The
// command is a miss the proxy reports, naming B's EXECUTE and saying what to
// do, and the client gets no answer: no OK of test A's.
func TestReplay_AnExecuteWhosePrepareWasPrunedFailsSayingWhy(t *testing.T) {
	db, _, execB := orderTests("")
	db.session = append([]*models.Mock{nativeHandshake()}, db.session...)
	client, done := replay(t, db)
	client.greetAndAnswer("app", mysql.Native)
	if ok := client.read(); len(ok) == 0 || ok[0] != mysql.OK {
		t.Fatalf("the client read %x after its handshake; want the recorded OK", ok)
	}

	client.send(0, append([]byte{mysql.COM_STMT_PREPARE}, ordersInsert...))
	prepareOK := client.read()
	if len(prepareOK) < 5 || prepareOK[0] != 0x00 {
		t.Fatalf("the PREPARE was answered with %x, want a COM_STMT_PREPARE_OK", prepareOK)
	}
	// The recorded answer carries no parameter definitions: nothing follows.
	stmtID := binary.LittleEndian.Uint32(prepareOK[1:5])

	exec := []byte{mysql.COM_STMT_EXECUTE}
	exec = binary.LittleEndian.AppendUint32(exec, stmtID)
	exec = append(exec, 0x00)                        // flags
	exec = binary.LittleEndian.AppendUint32(exec, 1) // iteration count
	exec = append(exec, 0x00, 0x01)                  // null bitmap, new params bound
	exec = append(exec, 0xfd, 0x00, 0xfd, 0x00)      // VAR_STRING, VAR_STRING
	exec = append(exec, lenencString("k1")...)       // the idempotency key
	exec = append(exec, lenencString("2026-10-15 09:00:00")...)
	client.send(0, exec)
	if got, err := io.ReadAll(client.c); len(got) != 0 || err != nil {
		t.Fatalf("the client read %x, %v after its EXECUTE; want its connection closed unanswered", got, err)
	}

	err := <-done
	var r reportCarrier
	if !errors.Is(err, models.ErrNoMockMatched) || !errors.As(err, &r) || r.MismatchReport() == nil {
		t.Fatalf("Replay returned %v; want the EXECUTE's miss with its report", err)
	}
	rep := r.MismatchReport()
	if rep.ClosestMock != execB.Name || !strings.Contains(rep.Diff, execB.Name) || !strings.Contains(rep.Diff, "COM_STMT_PREPARE") {
		t.Errorf("the report does not name the EXECUTE that lost its statement: closest %q, diff %q", rep.ClosestMock, rep.Diff)
	}
	if !strings.Contains(rep.NextSteps, "prune") || !strings.Contains(rep.NextSteps, "Re-record") {
		t.Errorf("the report does not say why or what to do: %q", rep.NextSteps)
	}
	if !strings.Contains(rep.ActualSummary, ordersInsert) {
		t.Errorf("the report does not name the live statement: %q", rep.ActualSummary)
	}
}

// liveRead is a replayed EXECUTE of query with one parameter, as statement 33.
func liveRead(t *testing.T, db *fakeMockDb, query, param string) (*mysql.Response, bool, *mockMiss) {
	t.Helper()
	req := mysql.Request{PacketBundle: mysql.PacketBundle{
		Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 40}, Type: "COM_STMT_EXECUTE"},
		Message: &mysql.StmtExecutePacket{Status: 0x17, StatementID: 33, IterationCount: 1, NewParamsBindFlag: 1,
			ParameterCount: 1, Parameters: stmtParams(param)},
	}}
	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[33] = query
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "0")
	resp, ok, miss, err := matchCommand(ctx, zap.NewNop(), req, db, dctx, nil, nil)
	if err != nil {
		t.Fatalf("matchCommand: %v", err)
	}
	return resp, ok, miss
}

// An EXECUTE whose PREPARE the test set never held is not one the set lost:
// a statement prepared in an earlier test set of the same recording, on a
// connection that stayed open, whose handshake is in that earlier set too.
// The handshakes of the set's other connections say nothing of it. Nothing
// is refused for it.
func TestMatchCommand_AStatementTheSetNeverHeldIsNotLost(t *testing.T) {
	const cfg = "SELECT v FROM cfg WHERE k = ?"
	testB := stmtT0.Add(20 * time.Second)
	window := func(db *fakeMockDb) *fakeMockDb {
		db.winStart, db.winEnd = testB.Add(-time.Second), testB.Add(time.Second)
		return db
	}
	reusable := executedMock("mock-exec-cfg", 1, stmtT0, "cfg: feature-x=on", "", "feature-x")
	reusable.Spec.Metadata["connID"] = "1"
	for _, tc := range []struct {
		name    string
		unknown *models.Mock
		session []*models.Mock
	}{
		{
			// No PREPARE of its connection was recorded at all.
			name:    "on a connection with no recorded PREPARE",
			unknown: executedMock("mock-exec-unknown", 3, testB, "other", "", "x"),
		},
		{
			// Its ID is below every PREPARE recorded on its connection.
			name:    "below every recorded PREPARE of its connection",
			unknown: executedMock("mock-exec-unknown", 3, testB, "other", "", "x"),
			session: []*models.Mock{preparedMock("mock-prepare-later", "7", 9, "SELECT 1 FROM t WHERE a = ?", stmtT0)},
		},
		{
			// A set that gathers recordings of several agent runs (the
			// k8s-proxy's smart set): a later run numbered one of its own
			// connections 7 too, and its handshake and PREPAREs came after
			// this EXECUTE.
			name:    "beside a later recording's connection of the same ID",
			unknown: executedMock("mock-exec-unknown", 3, testB, "other", "", "x"),
			session: []*models.Mock{
				configMock("mock-handshake-7-later", "7", testB.Add(time.Hour)),
				preparedMock("mock-prepare-7-later", "7", 1, "SELECT 1 FROM t WHERE a = ?", testB.Add(time.Hour)),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.unknown.Spec.Metadata["connID"] = "7"
			db := window(&fakeMockDb{session: append(append([]*models.Mock{
				configMock("mock-handshake-1", "1", stmtT0),
				preparedMock("mock-prepare-cfg", "1", 1, cfg, stmtT0),
				reusable,
			}, tc.session...), tc.unknown)})
			resp, ok, miss := liveRead(t, db, cfg, "feature-y")
			if !ok || resp.Payload != "cfg: feature-x=on" {
				t.Fatalf("served %v (ok %v, miss %+v), want the recorded read, as before", resp, ok, miss)
			}
		})
	}
}

// With an EXECUTE in the window that lost its statement, an exact match on
// the query and every parameter recorded for another test is still served:
// it is the answer the matcher gives a reusable read.
func TestMatchCommand_AnExactMatchIsServedBesideALostStatement(t *testing.T) {
	db, execA, _ := orderTests("")
	resp, ok, miss := matchLiveOrder(t, db, "k1", "2026-10-14 12:00:10")
	if !ok || resp.Payload != "A: 201" {
		t.Fatalf("served %v (ok %v, miss %+v), want %s, the exact match on the query and every parameter", resp, ok, miss, execA.Name)
	}
}

// A match on the parameters alone, of an EXECUTE whose statement is unknown,
// does not stop the scan before the window's own match on the query and every
// parameter: an unknown statement of the same parameters may be another one.
func TestMatchCommand_AParametersOnlyMatchRanksBelowAnExactOne(t *testing.T) {
	const orders = "SELECT total FROM orders WHERE user_id = ?"
	testB := stmtT0.Add(20 * time.Second)
	db := &fakeMockDb{
		session: []*models.Mock{
			preparedMock("mock-prepare-orders", "0", 5, orders, testB),
			executedMock("mock-exec-users", 4, testB.Add(time.Millisecond), "users row", "", "u1"),
			executedMock("mock-exec-orders", 5, testB.Add(2*time.Millisecond), "orders row", "", "u1"),
		},
		winStart: testB.Add(-time.Second), winEnd: testB.Add(time.Second),
	}
	resp, ok, miss := liveRead(t, db, orders, "u1")
	if !ok || resp.Payload != "orders row" {
		t.Fatalf("served %v (ok %v, miss %+v), want the orders EXECUTE, which matched on its query too", resp, ok, miss)
	}
	// Alone, the unknown statement's match on its parameters is served, as
	// before (keploy#4226).
	db.session = db.session[:2]
	if resp, ok, _ := liveRead(t, db, orders, "u1"); !ok || resp.Payload != "users row" {
		t.Fatalf("served %v (ok %v), want the match on the parameters alone", resp, ok)
	}
}

// configMock is the config mock of connection connID: its handshake, which no
// prune deletes.
func configMock(name, connID string, at time.Time) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.MySQL}
	m.TestModeInfo.Lifetime = models.LifetimeSession
	m.Spec.Metadata = map[string]string{"type": "config", "connID": connID,
		"requestOperation": "HandshakeV10", "responseOperation": "OK"}
	m.Spec.ReqTimestampMock = at
	m.Spec.MySQLRequests = []mysql.Request{{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 32, SequenceID: 1}, Type: "HandshakeResponse41"},
		Message: &mysql.HandshakeResponse41Packet{},
	}}}
	m.Spec.MySQLResponses = []mysql.Response{{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 7, SequenceID: 2}, Type: "OK"},
		Message: &mysql.OKPacket{Header: mysql.OK},
	}}}
	return m
}

// #4810 on a pool of two connections: the old prune kept only the FIRST
// recorded PREPARE of the INSERT (connection 0's), so connection 1 kept none
// of its PREPAREs of it, while its EXECUTE (test B's ERR 1062) was kept. Its
// handshake in the set says the statement was prepared in this set, and lost.
func TestMatchCommand_AStatementLostOnAConnectionThatKeptNoPrepare(t *testing.T) {
	testA, testB := stmtT0.Add(10*time.Second), stmtT0.Add(20*time.Second)
	for _, tc := range []struct {
		name  string
		extra []*models.Mock
	}{
		{"connection 1 kept no PREPARE", nil},
		{"connection 1 kept a later PREPARE of another statement", []*models.Mock{
			preparedMock("mock-prepare-other", "1", 2, "SELECT 1 FROM t WHERE a = ? AND b = ?", testB.Add(2*time.Millisecond)),
		}},
		// A smart set: a later agent run numbered a connection of its own 1
		// too. Its handshake, after this EXECUTE, does not hide this run's.
		{"beside a later run's handshake of connection 1", []*models.Mock{
			configMock("mock-handshake-1-later", "1", testB.Add(time.Hour)),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			execB := executedMock("mock-execute-b", 1, testB.Add(time.Millisecond), "B: ERR 1062", "", "k1", "2026-10-14 12:00:20")
			execB.Spec.Metadata["connID"] = "1"
			session := []*models.Mock{
				configMock("mock-handshake-0", "0", stmtT0), configMock("mock-handshake-1", "1", stmtT0),
				preparedMock("mock-prepare-a", "0", 1, ordersInsert, testA),
				executedMock("mock-execute-a", 1, testA.Add(time.Millisecond), "A: 201", "", "k1", "2026-10-14 12:00:10"),
				execB,
			}
			db := &fakeMockDb{session: append(session, tc.extra...), winStart: testB.Add(-time.Second), winEnd: testB.Add(time.Second)}
			resp, ok, miss := matchLiveOrder(t, db, "k1", "2026-10-15 09:00:00")
			if ok {
				t.Fatalf("served %q to test B's INSERT: test A's EXECUTE answered a command whose own mock lost its statement", resp.Payload)
			}
			if miss == nil || miss.closestMock != execB.Name {
				t.Fatalf("the miss %+v does not name %s", miss, execB.Name)
			}
		})
	}
}
