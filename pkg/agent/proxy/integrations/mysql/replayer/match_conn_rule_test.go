package replayer_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// connRec is one recording of a connection-rule set: its name, the
// connection it was recorded on, the command (a text query, "exec:P", an
// EXECUTE of statement 1 with parameter P, or "reset", a
// COM_RESET_CONNECTION), when it was recorded (after diffBase), and the test
// whose mapping owns it under exact serving (-1: none). A name ending in
// "/" and a status is answered with an answer that says it (connAnswer);
// any other recording's answer carries no status, as an ERR does.
type connRec struct {
	name, conn, sql string
	at              time.Duration
	test            int
}

// connCall is one live command: the live connection it is sent on, the
// command, and the recording that must answer it ("*": any answer, such as
// the OK keploy writes for a COMMIT nothing recorded).
type connCall struct{ live, sql, want string }

// connTest is one recorded test: its window (after diffBase) and the calls
// its replay makes, in order.
type connTest struct {
	start, end time.Duration
	calls      []connCall
}

const connStmt = "SELECT name, price FROM products WHERE id = ?"

func connReq(sql string) mysql.Request {
	if p, ok := strings.CutPrefix(sql, "exec:"); ok {
		return executeReq(1, p)
	}
	if sql == "reset" {
		return resetReq()
	}
	return queryReq(sql)
}

func resetReq() mysql.Request {
	return mysql.Request{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1}, Type: mysql.CommandStatusToString(mysql.COM_RESET_CONNECTION)},
		Message: &mysql.ResetConnectionPacket{},
	}}
}

// connAnswer gives m the answer its name asks for (connRec). A name ending
// in "/err" is answered with an ERR packet. One ending in "/" and a status,
// "on" (autocommit on), "off" (off), "tx" (a transaction is open, autocommit
// off) or "on+tx", is answered with that session status: a SELECT with a
// result set and an EXECUTE with a binary one, each ended by the OK that
// replaces EOF, and any other command with an OK packet. A form after the
// status changes that: ".eof" ends the result set with an EOF, ".old" with
// an EOF of the protocol before 4.1 (it has no status), ".bad" with an OK
// whose affected rows are not zero (no EOF, and no OK that replaces one: its
// status is not read), and ".pkt" answers with a bare EOF packet (as
// COM_SET_OPTION is).
func connAnswer(m *models.Mock) {
	i := strings.LastIndex(m.Name, "/")
	if i < 0 {
		return
	}
	resp := &m.Spec.MySQLResponses[0]
	if m.Name[i+1:] == "err" {
		resp.Message = &mysql.ERRPacket{Header: mysql.ERR, ErrorCode: 1399, SQLStateMarker: "#", SQLState: "XAE07", ErrorMessage: "XAER_RMFAIL"}
		return
	}
	name, form, _ := strings.Cut(m.Name[i+1:], ".")
	status, ok := map[string]uint16{"on": 0x0002, "off": 0x0000, "tx": 0x0001, "on+tx": 0x0003}[name]
	if !ok {
		panic("connAnswer: no status " + name + " in " + m.Name)
	}
	end := []byte{0x07, 0, 0, 3, 0xfe, 0, 0, byte(status), byte(status >> 8), 0, 0}
	switch form {
	case "eof":
		end = []byte{0x05, 0, 0, 3, 0xfe, 0, 0, byte(status), byte(status >> 8)}
	case "old":
		end = []byte{0x01, 0, 0, 3, 0xfe}
	case "bad":
		end = []byte{0x07, 0, 0, 3, 0xfe, 1, 0, byte(status), byte(status >> 8), 0, 0}
	case "pkt":
		resp.Message = &mysql.EOFPacket{Header: 0xfe, StatusFlags: status}
		return
	}
	terminator := &mysql.GenericResponse{Data: end, Type: mysql.StatusToString(mysql.EOF)}
	if form == "" {
		terminator.Type = mysql.StatusToString(mysql.OK)
	}
	switch req := m.Spec.MySQLRequests[0]; {
	case req.Header != nil && req.Header.Type == mysql.CommandStatusToString(mysql.COM_STMT_EXECUTE):
		resp.Message = &mysql.BinaryProtocolResultSet{FinalResponse: terminator}
	case hasSelect(req):
		resp.Message = &mysql.TextResultSet{FinalResponse: terminator}
	default:
		resp.Message = &mysql.OKPacket{Header: mysql.OK, StatusFlags: status}
	}
}

// hasSelect reports whether req is a text query that reads (SELECT).
func hasSelect(req mysql.Request) bool {
	qp, _ := req.Message.(*mysql.QueryPacket)
	return qp != nil && strings.HasPrefix(qp.Query, "SELECT")
}

// connPool is recs as recorded mocks, after a PREPARE of connStmt on each of
// their connections, made before the first window.
func connPool(recs []connRec) (pool []*models.Mock, byTest map[int][]*models.Mock) {
	byTest = map[int][]*models.Mock{}
	prepared := map[string]bool{}
	for _, r := range recs {
		if !prepared[r.conn] {
			prepared[r.conn] = true
			pool = append(pool, diffPrepareMock("prep-"+r.conn, r.conn, connStmt, 1, diffBase.Add(-time.Millisecond)))
		}
	}
	for _, r := range recs {
		lifetime, typ := models.LifetimePerTest, "mocks"
		if r.test < 0 {
			// No test's: a recording of the connection's, reusable, which
			// every view shows.
			lifetime, typ = models.LifetimeConnection, "connection"
		}
		m := diffMock(r.name, r.conn, []mysql.Request{connReq(r.sql)}, diffBase.Add(r.at), lifetime, typ)
		connAnswer(m)
		pool = append(pool, m)
		byTest[r.test] = append(byTest[r.test], m)
	}
	return pool, byTest
}

// recordedOrderStore is a MockManager under exact serving whose reusable
// tier lists its recordings in recorded order, read by the full scan
// (modeExactInTime). The open test's own that have not answered lead its
// per-test read, as the exact view lists them (proxy's exactView.perTest and
// MockManager.rangeExactPerTest), so the matcher stops at the first exact
// match the test does not own.
type recordedOrderStore struct{ *proxy.MockManager }

func (s recordedOrderStore) GetSessionMocks() ([]*models.Mock, error) {
	session, err := s.MockManager.GetSessionMocks() //nolint:staticcheck // SA1019: the matcher reads the pool's session tier through it (loadPool).
	if err != nil {
		return nil, err
	}
	all := slices.Clone(session)
	slices.SortStableFunc(all, func(a, b *models.Mock) int {
		return a.Spec.ReqTimestampMock.Compare(b.Spec.ReqTimestampMock)
	})
	return all, nil
}

// The modes a connection-rule set is replayed in (checkConnSet).
const (
	modeTest        = "keploy test"
	modeTestScan    = "keploy test, full scan"
	modeExact       = "exact serving"
	modeExactInTime = "exact serving, recorded order"
)

// connLive is a live connection's context and decode state, its statement 1
// prepared as connStmt.
func connLive(live string) (context.Context, *wire.DecodeContext) {
	return context.WithValue(context.Background(), models.ClientConnectionIDKey, live), &wire.DecodeContext{
		PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{},
		StmtIDToQuery:      map[uint32]string{1: connStmt},
		NextStmtID:         100,
	}
}

// replayConnSet replays tests in order against recs, as keploy test does
// (each test's window staged as the agent stages it) or under exact serving
// (each test's mapping is the recordings it owns), with a full scan the whole
// pool scanned for every command (modeTestScan, and modeExactInTime, which
// reads it in recorded order: recordedOrderStore). It returns what each call
// was answered with, "miss" for none. Each live connection keeps its state
// across tests, as an app's pool does. With greeting set, each live
// connection begins as one the server greeted with that status does.
func replayConnSet(t *testing.T, greeting *uint16, recs []connRec, tests []connTest, mode string) [][]string {
	t.Helper()
	exact := mode == modeExact || mode == modeExactInTime
	fullScan := mode == modeTestScan || mode == modeExactInTime
	pool, byTest := connPool(recs)
	var mm *proxy.MockManager
	var side *diffSide
	if exact {
		mm = proxy.NewMockManager(nil, nil, zap.NewNop())
		mm.SetMocksWithWindow(nil, pool, models.BaseTime, time.Now().Add(time.Hour))
	} else {
		side = newDiffSide(t, pool)
		mm = side.mm
	}
	t.Cleanup(mm.Close)
	var db integrations.MockMemDb = mm
	if mode == modeExactInTime {
		db = recordedOrderStore{mm}
	}
	ctxs, dctxs := map[string]context.Context{}, map[string]*wire.DecodeContext{}
	var got [][]string
	for i, tc := range tests {
		if exact {
			mm.ServeExactTest(fmt.Sprintf("T%d", i), byTest[i])
		} else {
			side.stage(diffBase.Add(tc.start), diffBase.Add(tc.end))
		}
		var answers []string
		for _, c := range tc.calls {
			if dctxs[c.live] == nil {
				ctxs[c.live], dctxs[c.live] = connLive(c.live)
				if greeting != nil {
					replayer.StartSessionForTest(dctxs[c.live], *greeting)
				}
			}
			var resp *mysql.Response
			var ok bool
			var err error
			if fullScan {
				resp, ok, _, err = replayer.MatchBoundForTest(ctxs[c.live], zap.NewNop(), connReq(c.sql), db, dctxs[c.live], true)
			} else {
				resp, ok, _, err = replayer.MatchWithEngineForTest(ctxs[c.live], zap.NewNop(), connReq(c.sql), db, dctxs[c.live], nil)
			}
			if err != nil {
				t.Fatalf("test %d, %q on %s: %v", i, c.sql, c.live, err)
			}
			if !ok || resp == nil {
				answers = append(answers, "miss")
				continue
			}
			answers = append(answers, resp.Payload)
		}
		got = append(got, answers)
	}
	return got
}

// checkConnSet replays the set as keploy test does (first pass and full
// scan) and under exact serving, and fails for each call any of them answers
// with another recording than the call's want.
func checkConnSet(t *testing.T, recs []connRec, tests []connTest, modes ...string) {
	t.Helper()
	checkConnSession(t, nil, recs, tests, modes...)
}

// checkConnSession is checkConnSet with each live connection greeted with
// the status greeting (nil: no greeting read).
func checkConnSession(t *testing.T, greeting *uint16, recs []connRec, tests []connTest, modes ...string) {
	t.Helper()
	if len(modes) == 0 {
		modes = []string{modeTest, modeTestScan, modeExact, modeExactInTime}
	}
	for _, mode := range modes {
		got := replayConnSet(t, greeting, recs, tests, mode)
		for i, tc := range tests {
			for j, c := range tc.calls {
				if c.want != "*" && got[i][j] != c.want {
					t.Errorf("%s: test %d call %d (%q on %s) was answered with %s; want %s", mode, i, j, c.sql, c.live, got[i][j], c.want)
				}
			}
		}
	}
}

// A test's window holds the whole window of a shorter test that began after
// it, and both tests' transactions read one product: the later test's read,
// recorded inside both windows, comes first in the earlier test's window,
// and the earlier test's own read was recorded after the later test ended.
// keploy test replays the earlier test first. Its transaction began with a
// command only it made, recorded on its connection, so each of its later
// commands is answered with its own recording on that connection, and the
// later test is left its own: the first exact match in the window answered
// the earlier test with the later test's read, and the later test's window
// then held no recording of it.
func TestConnectionRuleAnswersEachTransactionWithItsOwnRead(t *testing.T) {
	recs := []connRec{
		{"a-start", "cA", "START TRANSACTION", 500 * time.Microsecond, 0},
		{"a-cust", "cA", "SELECT * FROM customers WHERE id = 'a'", 600 * time.Microsecond, 0},
		{"b-start", "cB", "START TRANSACTION", 2100 * time.Microsecond, 1},
		{"b-cust", "cB", "SELECT * FROM customers WHERE id = 'b'", 2200 * time.Microsecond, 1},
		{"b-upd", "cB", "UPDATE products SET n = n - 1 WHERE id = 'x'", 2300 * time.Microsecond, 1},
		{"b-sel", "cB", "SELECT * FROM products WHERE id = 'x'", 2400 * time.Microsecond, 1},
		{"b-exec", "cB", "exec:x", 2450 * time.Microsecond, 1},
		{"b-ins", "cB", "INSERT INTO orders VALUES ('b')", 2500 * time.Microsecond, 1},
		{"b-commit", "cB", "COMMIT", 2600 * time.Microsecond, 1},
		{"a-upd", "cA", "UPDATE products SET n = n - 1 WHERE id = 'x'", 6000 * time.Microsecond, 0},
		{"a-sel", "cA", "SELECT * FROM products WHERE id = 'x'", 6100 * time.Microsecond, 0},
		{"a-exec", "cA", "exec:x", 6150 * time.Microsecond, 0},
		{"a-ins", "cA", "INSERT INTO orders VALUES ('a')", 6200 * time.Microsecond, 0},
		{"a-commit", "cA", "COMMIT", 6300 * time.Microsecond, 0},
	}
	tx := func(live, who string) []connCall {
		return []connCall{
			{live, "START TRANSACTION", who + "-start"},
			{live, "SELECT * FROM customers WHERE id = '" + who + "'", who + "-cust"},
			{live, "UPDATE products SET n = n - 1 WHERE id = 'x'", who + "-upd"},
			{live, "SELECT * FROM products WHERE id = 'x'", who + "-sel"},
			{live, "exec:x", who + "-exec"},
			{live, "INSERT INTO orders VALUES ('" + who + "')", who + "-ins"},
			{live, "COMMIT", who + "-commit"},
		}
	}
	tests := []connTest{
		{0, 10 * time.Millisecond, tx("L1", "a")},
		{2 * time.Millisecond, 5 * time.Millisecond, tx("L1", "b")},
	}
	checkConnSet(t, recs, tests)
}

// The tests of TestConnectionRuleAnswersEachTransactionWithItsOwnRead, with
// both transactions reading one customer first, and another customer's read
// recorded inside the later test's window. The later test's read (on cB) was
// recorded inside both windows, the earlier test's (on cA) after the later
// test ended. keploy test replays the earlier test first: its read follows
// START, so it binds nothing yet and takes the first exact match in its
// window, the later test's. The later test's window then holds no recording
// of its read but z-cust, which a score pick would serve. Its read is
// answered with a recording of its own text, the earlier test's: the two are
// reads of one customer, so either answers with the same bytes. Exact
// serving answers each test with its own.
func TestConnectionRuleAnswersTheLaterTestsFirstReadWithItsOwnText(t *testing.T) {
	recs := []connRec{
		{"a-start", "cA", "START TRANSACTION", 500 * time.Microsecond, 0},
		{"b-start", "cB", "START TRANSACTION", 2100 * time.Microsecond, 1},
		{"z-cust", "cZ", "SELECT * FROM customers WHERE id = 'z'", 2150 * time.Microsecond, 1},
		{"b-cust", "cB", "SELECT * FROM customers WHERE id = 'c'", 2200 * time.Microsecond, 1},
		{"b-upd", "cB", "UPDATE products SET n = n - 1 WHERE id = 'y'", 2300 * time.Microsecond, 1},
		{"b-ins", "cB", "INSERT INTO orders VALUES ('b')", 2400 * time.Microsecond, 1},
		{"a-cust", "cA", "SELECT * FROM customers WHERE id = 'c'", 6000 * time.Microsecond, 0},
		{"a-upd", "cA", "UPDATE products SET n = n - 1 WHERE id = 'x'", 6100 * time.Microsecond, 0},
		{"a-ins", "cA", "INSERT INTO orders VALUES ('a')", 6200 * time.Microsecond, 0},
	}
	tx := func(cust, p, o string) []connCall {
		return []connCall{
			{"L1", "START TRANSACTION", "*"},
			{"L1", "SELECT * FROM customers WHERE id = 'c'", cust},
			{"L1", "UPDATE products SET n = n - 1 WHERE id = '" + p + "'", o + "-upd"},
			{"L1", "INSERT INTO orders VALUES ('" + o + "')", o + "-ins"},
			{"L1", "COMMIT", "*"},
		}
	}
	window := func(cust0, cust1 string) []connTest {
		return []connTest{
			{0, 10 * time.Millisecond, tx(cust0, "x", "a")},
			{2 * time.Millisecond, 5 * time.Millisecond, tx(cust1, "y", "b")},
		}
	}
	checkConnSet(t, recs, window("*", "*"), modeTest, modeTestScan)
	for _, mode := range []string{modeTest, modeTestScan} {
		got := replayConnSet(t, nil, recs, window("*", "*"), mode)
		for i := range got {
			if a := got[i][1]; a != "a-cust" && a != "b-cust" {
				t.Errorf("%s: test %d's read of customer 'c' was answered with %s; want a recording of that read", mode, i, a)
			}
		}
	}
	checkConnSet(t, recs, window("a-cust", "b-cust"), modeExact, modeExactInTime)
}

// Every transaction sends START TRANSACTION, so the recording that answers
// it says nothing of which transaction is open, and binds nothing: here the
// first START recorded is the transaction on cA's, which read the product
// after the one on cB did. Bound from its START, the live transaction that
// is cB's would be answered with cA's read, and cA's with cB's. Bound from
// the command after START, each is answered with its own.
func TestConnectionRuleBindsFromTheCommandAfterStart(t *testing.T) {
	recs := []connRec{
		{"a-start", "cA", "START TRANSACTION", 1000 * time.Microsecond, 0},
		{"b-start", "cB", "START TRANSACTION", 1100 * time.Microsecond, 0},
		{"b-sel", "cB", "SELECT * FROM products WHERE id = 'x'", 1200 * time.Microsecond, 0},
		{"b-ins", "cB", "INSERT INTO orders VALUES ('b')", 1300 * time.Microsecond, 0},
		{"b-sel2", "cB", "SELECT * FROM products WHERE id = 'y'", 1400 * time.Microsecond, 0},
		{"a-sel", "cA", "SELECT * FROM products WHERE id = 'x'", 2200 * time.Microsecond, 0},
		{"a-sel2", "cA", "SELECT * FROM products WHERE id = 'y'", 2300 * time.Microsecond, 0},
		{"a-ins", "cA", "INSERT INTO orders VALUES ('a')", 2400 * time.Microsecond, 0},
	}
	tests := []connTest{{0, 4 * time.Millisecond, []connCall{
		{"L1", "START TRANSACTION", "a-start"},
		{"L1", "SELECT * FROM products WHERE id = 'x'", "b-sel"},
		{"L1", "INSERT INTO orders VALUES ('b')", "b-ins"},
		{"L1", "SELECT * FROM products WHERE id = 'y'", "b-sel2"},
		{"L1", "COMMIT", "*"},
		{"L2", "START TRANSACTION", "b-start"},
		{"L2", "SELECT * FROM products WHERE id = 'x'", "a-sel"},
		{"L2", "SELECT * FROM products WHERE id = 'y'", "a-sel2"},
		{"L2", "INSERT INTO orders VALUES ('a')", "a-ins"},
	}}}
	checkConnSet(t, recs, tests)
}

// A test's two transactions ran at once on two connections and read one
// product each time; the replay runs them one after the other, and then
// interleaved. Each command after the transaction's first is answered with
// the recording made on the connection that answered the transaction's
// latest command, after it, whichever came first in the window, in keploy
// test and under exact serving alike: the test's mapping owns both reads, as
// its window holds both.
func TestConnectionRuleHoldsUnderExactServingAsInKeployTest(t *testing.T) {
	recs := []connRec{
		{"a-cust", "cA", "SELECT * FROM customers WHERE id = 'a'", 1000 * time.Microsecond, 0},
		{"b-cust", "cB", "SELECT * FROM customers WHERE id = 'b'", 1050 * time.Microsecond, 0},
		{"b-sel", "cB", "SELECT * FROM products WHERE id = 'x'", 1100 * time.Microsecond, 0},
		{"b-exec", "cB", "exec:x", 1150 * time.Microsecond, 0},
		{"a-sel", "cA", "SELECT * FROM products WHERE id = 'x'", 1200 * time.Microsecond, 0},
		{"a-exec", "cA", "exec:x", 1250 * time.Microsecond, 0},
		{"a-sel2", "cA", "SELECT * FROM products WHERE id = 'x'", 1300 * time.Microsecond, 0},
		{"b-sel2", "cB", "SELECT * FROM products WHERE id = 'x'", 1400 * time.Microsecond, 0},
	}
	tx := func(live, who string) []connCall {
		return []connCall{
			{live, "BEGIN", "*"},
			{live, "SELECT * FROM customers WHERE id = '" + who + "'", who + "-cust"},
			{live, "SELECT * FROM products WHERE id = 'x'", who + "-sel"},
			{live, "exec:x", who + "-exec"},
			{live, "SELECT * FROM products WHERE id = 'x'", who + "-sel2"},
		}
	}
	t.Run("one after the other", func(t *testing.T) {
		checkConnSet(t, recs, []connTest{{0, 4 * time.Millisecond, append(tx("L1", "a"), tx("L2", "b")...)}})
	})
	t.Run("interleaved", func(t *testing.T) {
		a, b := tx("L1", "a"), tx("L2", "b")
		var calls []connCall
		for i := range a {
			calls = append(calls, b[i], a[i])
		}
		checkConnSet(t, recs, []connTest{{0, 4 * time.Millisecond, calls}})
	})
}

// A command is answered with a recording made on the bound connection after
// the transaction's latest command, never before: the connection ran another
// transaction before this one, which read the same product, and its read is
// still unanswered in the window.
func TestConnectionRuleTakesTheBoundConnectionsNextCommand(t *testing.T) {
	recs := []connRec{
		{"earlier-sel", "cA", "SELECT * FROM products WHERE id = 'x'", 500 * time.Microsecond, 0},
		{"b-cust", "cB", "SELECT * FROM customers WHERE id = 'b'", 900 * time.Microsecond, 0},
		{"a-cust", "cA", "SELECT * FROM customers WHERE id = 'a'", 1000 * time.Microsecond, 0},
		{"b-sel", "cB", "SELECT * FROM products WHERE id = 'x'", 1100 * time.Microsecond, 0},
		{"a-sel", "cA", "SELECT * FROM products WHERE id = 'x'", 1200 * time.Microsecond, 0},
	}
	checkConnSet(t, recs, []connTest{{0, 4 * time.Millisecond, []connCall{
		{"L1", "START TRANSACTION", "*"},
		{"L1", "SELECT * FROM customers WHERE id = 'a'", "a-cust"},
		{"L1", "SELECT * FROM products WHERE id = 'x'", "a-sel"},
	}}})
}

// Under exact serving a recording outside every test answers the open test
// only when the test owns no exact match: the test's own read, made on
// another connection than the bound one, still outranks it.
func TestConnectionRuleKeepsTheTestsOwnAboveThePool(t *testing.T) {
	recs := []connRec{
		{"a-cust", "cA", "SELECT * FROM customers WHERE id = 'a'", 1000 * time.Microsecond, 0},
		{"b-sel", "cB", "SELECT * FROM products WHERE id = 'x'", 1100 * time.Microsecond, 0},
		{"b-exec", "cB", "exec:x", 1150 * time.Microsecond, 0},
		// Recorded between the two tests: no test owns them.
		{"pool-sel", "cA", "SELECT * FROM products WHERE id = 'x'", 7 * time.Millisecond, -1},
		{"pool-exec", "cA", "exec:x", 7100 * time.Microsecond, -1},
		{"pool-z-b", "cB", "SELECT * FROM products WHERE id = 'z'", 7200 * time.Microsecond, -1},
		{"pool-z-a", "cA", "SELECT * FROM products WHERE id = 'z'", 7300 * time.Microsecond, -1},
	}
	checkConnSet(t, recs, []connTest{{0, 4 * time.Millisecond, []connCall{
		{"L1", "START TRANSACTION", "*"},
		{"L1", "SELECT * FROM customers WHERE id = 'a'", "a-cust"},
		{"L1", "SELECT * FROM products WHERE id = 'x'", "b-sel"},
		{"L1", "exec:x", "b-exec"},
		// The rule ranks the test's own recordings only: of the recordings
		// made outside every test, the first answers, as before.
		{"L1", "SELECT * FROM products WHERE id = 'z'", "pool-z-b"},
	}}}, modeExact, modeExactInTime)
}

// COMMIT and ROLLBACK end the binding: a command after them, outside every
// transaction, is answered with the first exact match in the window, as it
// was before the rule, not with one made on the connection the ended
// transaction was bound to.
func TestConnectionRuleEndsAtCommitAndRollback(t *testing.T) {
	// A START, or a COMMIT AND CHAIN, ends the transaction too, and begins
	// another, which the read after it binds. An XA transaction begins at
	// XA START and ends at XA END (or COMMIT, or ROLLBACK).
	for _, c := range []struct{ begin, end, next string }{
		{"START TRANSACTION", "COMMIT", "y-a"},
		{"START TRANSACTION", "rollback work", "y-a"},
		{"START TRANSACTION", "/* app */ COMMIT AND NO CHAIN", "y-a"},
		{"START TRANSACTION", "START TRANSACTION", "y-b"},
		{"START TRANSACTION", "COMMIT AND CHAIN", "y-b"},
		{"XA START 'x1'", "XA END 'x1'", "y-a"},
		{"xa begin 'x1'", "XA COMMIT 'x1' ONE PHASE", "y-a"},
		{"XA START 'x1'", "XA ROLLBACK 'x1'", "y-a"},
		{"START TRANSACTION", "XA START 'x2'", "y-b"},
	} {
		begin, end, next := c.begin, c.end, c.next
		t.Run(begin+" "+end, func(t *testing.T) {
			recs := []connRec{
				{"a-cust", "cA", "SELECT * FROM customers WHERE id = 'a'", 1000 * time.Microsecond, 0},
				{"b-count", "cB", "SELECT COUNT(*) FROM orders", 1500 * time.Microsecond, 0},
				{"a-count", "cA", "SELECT COUNT(*) FROM orders", 2500 * time.Microsecond, 0},
				{"y-a", "cA", "SELECT * FROM products WHERE id = 'y'", 2600 * time.Microsecond, 0},
				{"y-b", "cB", "SELECT * FROM products WHERE id = 'y'", 2700 * time.Microsecond, 0},
			}
			checkConnSet(t, recs, []connTest{{0, 4 * time.Millisecond, []connCall{
				{"L1", begin, "*"},
				{"L1", "SELECT * FROM customers WHERE id = 'a'", "a-cust"},
				{"L1", end, "*"},
				{"L1", "SELECT COUNT(*) FROM orders", "b-count"},
				{"L1", "SELECT * FROM products WHERE id = 'y'", next},
			}}})
		})
	}
}

// A session with autocommit off is always in a transaction (JDBC's
// setAutoCommit(false) sends SET autocommit = 0, and no START), as the SET's
// answer says: the read after it binds; a COMMIT under it begins the next at
// once, which the read after it binds again; and turning autocommit on ends
// it, as the server commits it and says so, so the reads after it are
// outside every transaction and answered with the first exact match in the
// window. Turning it off again while it is off keeps the transaction (its
// answer says it is open) and its binding.
func TestConnectionRuleBindsATransactionOfAutocommitOff(t *testing.T) {
	recs := []connRec{
		{"off/off", "cA", "SET autocommit = 0", 500 * time.Microsecond, 0},
		{"a-cust", "cA", "SELECT * FROM customers WHERE id = 'a'", 1000 * time.Microsecond, 0},
		{"again/tx", "cA", "SET autocommit = 0", 1200 * time.Microsecond, 0},
		{"b-count", "cB", "SELECT COUNT(*) FROM orders", 1500 * time.Microsecond, 0},
		{"a-count", "cA", "SELECT COUNT(*) FROM orders", 2500 * time.Microsecond, 0},
		{"y-b", "cB", "SELECT * FROM products WHERE id = 'y'", 2600 * time.Microsecond, 0},
		{"y-a", "cA", "SELECT * FROM products WHERE id = 'y'", 2700 * time.Microsecond, 0},
		{"z-a", "cA", "SELECT * FROM products WHERE id = 'z'", 2800 * time.Microsecond, 0},
		{"z-b", "cB", "SELECT * FROM products WHERE id = 'z'", 2900 * time.Microsecond, 0},
		{"on/on", "cB", "SET autocommit = 1", 3000 * time.Microsecond, 0},
		{"w-a", "cA", "SELECT * FROM products WHERE id = 'w'", 3100 * time.Microsecond, 0},
		{"w-b", "cB", "SELECT * FROM products WHERE id = 'w'", 3200 * time.Microsecond, 0},
	}
	checkConnSet(t, recs, []connTest{{0, 4 * time.Millisecond, []connCall{
		{"L1", "SET autocommit = 0", "off/off"},
		{"L1", "SELECT * FROM customers WHERE id = 'a'", "a-cust"},
		{"L1", "SET autocommit = 0", "again/tx"},
		{"L1", "SELECT COUNT(*) FROM orders", "a-count"},
		{"L1", "COMMIT", "*"},
		{"L1", "SELECT * FROM products WHERE id = 'y'", "y-b"},
		{"L1", "SELECT * FROM products WHERE id = 'z'", "z-b"},
		{"L1", "SET autocommit = 1", "on/on"},
		{"L1", "SELECT * FROM products WHERE id = 'w'", "w-a"},
	}}})
}

// Autocommit turned off inside a transaction begun by START keeps it open
// and bound (its answer says it is open), and its COMMIT begins the next at
// once, which the read after it binds.
func TestConnectionRuleTurnsAutocommitOffInsideATransaction(t *testing.T) {
	recs := []connRec{
		{"a-cust", "cA", "SELECT * FROM customers WHERE id = 'a'", 1000 * time.Microsecond, 0},
		{"off/tx", "cA", "SET autocommit = 0", 1200 * time.Microsecond, 0},
		{"y-b", "cB", "SELECT * FROM products WHERE id = 'y'", 2600 * time.Microsecond, 0},
		{"y-a", "cA", "SELECT * FROM products WHERE id = 'y'", 2700 * time.Microsecond, 0},
		{"z-a", "cA", "SELECT * FROM products WHERE id = 'z'", 2800 * time.Microsecond, 0},
		{"z-b", "cB", "SELECT * FROM products WHERE id = 'z'", 2900 * time.Microsecond, 0},
	}
	checkConnSet(t, recs, []connTest{{0, 4 * time.Millisecond, []connCall{
		{"L1", "START TRANSACTION", "*"},
		{"L1", "SELECT * FROM customers WHERE id = 'a'", "a-cust"},
		{"L1", "SET autocommit = 0", "off/tx"},
		{"L1", "COMMIT", "*"},
		{"L1", "SELECT * FROM products WHERE id = 'y'", "y-b"},
		{"L1", "SELECT * FROM products WHERE id = 'z'", "z-b"},
	}}})
}

// What a SET made of the session's autocommit is what its recorded answer's
// status says, whatever its text: SET autocommit = DEFAULT on a server whose
// default is off begins a transaction, and on one whose default is on
// begins none. A SET the server refused (an ERR answer: inside an XA
// transaction) changes nothing, and nor does one no recording answered.
// Inside a transaction begun by START, turning autocommit off, or on while
// it is on, keeps the transaction and its binding, as its answer says.
func TestConnectionRuleReadsAutocommitFromTheAnswer(t *testing.T) {
	recs := func(set string) []connRec {
		r := []connRec{
			{"a-cust", "cA", "SELECT * FROM customers WHERE id = 'a'", 1000 * time.Microsecond, 0},
			{"b-count", "cB", "SELECT COUNT(*) FROM orders", 1500 * time.Microsecond, 0},
			{"a-count", "cA", "SELECT COUNT(*) FROM orders", 2500 * time.Microsecond, 0},
		}
		if set != "" {
			r = append(r, connRec{set, "cA", "SET autocommit = DEFAULT", 500 * time.Microsecond, 0})
		}
		return r
	}
	for _, c := range []struct {
		name, set string
		start     bool
		count     string
	}{
		{"default off", "set/off", false, "a-count"},
		{"default on", "set/on", false, "b-count"},
		{"refused", "set/err", false, "b-count"},
		{"not answered", "", false, "b-count"},
		{"off in START", "set/tx", true, "a-count"},
		{"on in START", "set/on+tx", true, "a-count"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var calls []connCall
			if c.start {
				calls = append(calls,
					connCall{"L1", "START TRANSACTION", "*"},
					connCall{"L1", "SELECT * FROM customers WHERE id = 'a'", "a-cust"},
					connCall{"L1", "SET autocommit = DEFAULT", c.set})
			} else {
				calls = append(calls,
					connCall{"L1", "SET autocommit = DEFAULT", "*"},
					connCall{"L1", "SELECT * FROM customers WHERE id = 'a'", "a-cust"})
			}
			calls = append(calls, connCall{"L1", "SELECT COUNT(*) FROM orders", c.count})
			checkConnSet(t, recs(c.set), []connTest{{0, 4 * time.Millisecond, calls}})
		})
	}
}

// A reset of the connection ends its transaction, and its session's
// autocommit is the server's default, which the reset's recorded answer
// says: on, and nothing is bound after it, though autocommit was off before
// it; off, and the read after it binds. A reset no recording answered
// leaves autocommit on.
func TestConnectionRuleResetBringsBackTheDefaultAutocommit(t *testing.T) {
	for _, c := range []struct{ reset, count string }{
		{"reset/on", "b-count"},
		{"reset/off", "a-count"},
		{"", "b-count"},
	} {
		t.Run(c.reset, func(t *testing.T) {
			recs := []connRec{
				{"off/off", "cA", "SET autocommit = 0", 300 * time.Microsecond, 0},
				{"a-cust", "cA", "SELECT * FROM customers WHERE id = 'a'", 1000 * time.Microsecond, 0},
				{"b-count", "cB", "SELECT COUNT(*) FROM orders", 1500 * time.Microsecond, 0},
				{"a-count", "cA", "SELECT COUNT(*) FROM orders", 2500 * time.Microsecond, 0},
			}
			if c.reset != "" {
				recs = append(recs, connRec{c.reset, "cA", "reset", 500 * time.Microsecond, 0})
			}
			checkConnSet(t, recs, []connTest{{0, 4 * time.Millisecond, []connCall{
				{"L1", "SET autocommit = 0", "off/off"},
				{"L1", "reset", "*"},
				{"L1", "SELECT * FROM customers WHERE id = 'a'", "a-cust"},
				{"L1", "SELECT COUNT(*) FROM orders", c.count},
			}}})
		})
	}
}

// A reset ends the open transaction's binding with it: the read after it is
// outside every transaction (autocommit is on after it, as the reset's
// recorded answer says, or, with none, as the greeting said), though the
// transaction's read before it bound the connection the next read was
// recorded on too.
func TestConnectionRuleEndsTheBindingAtAReset(t *testing.T) {
	for _, reset := range []string{"reset/on", ""} {
		t.Run(reset, func(t *testing.T) {
			recs := []connRec{
				{"a-ord", "cA", "SELECT * FROM orders WHERE id = 7", 800 * time.Microsecond, 0},
				{"b-count", "cB", "SELECT COUNT(*) FROM orders", 1500 * time.Microsecond, 0},
				{"a-count", "cA", "SELECT COUNT(*) FROM orders", 2500 * time.Microsecond, 0},
			}
			if reset != "" {
				recs = append(recs, connRec{reset, "cA", "reset", 1000 * time.Microsecond, 0})
			}
			on := uint16(0x0002)
			checkConnSession(t, &on, recs, []connTest{{0, 4 * time.Millisecond, []connCall{
				{"L1", "START TRANSACTION", "*"},
				{"L1", "SELECT * FROM orders WHERE id = 7", "a-ord"},
				{"L1", "reset", "*"},
				{"L1", "SELECT COUNT(*) FROM orders", "b-count"},
			}}})
		})
	}
}

// A session whose init_connect turns autocommit off (the greeting and the
// login said it was on: the server sends them before it runs init_connect)
// is in a transaction from its first read, as that read's answer says (the
// status that ends its result set), so the first read binds, whatever form
// its result set ends in, an EXECUTE's binary one too; and a read whose
// answer says no transaction is open binds nothing. So does a command
// answered with a bare EOF packet. Inside a transaction begun by START with
// autocommit on, a read whose answer says the transaction is open binds as
// any other. An EOF of the protocol before 4.1 has no status, nor has a
// terminator of another shape: the read is outside every transaction, as
// the greeting said.
func TestConnectionRuleReadsTheTransactionFromEachAnswer(t *testing.T) {
	const cust = "SELECT * FROM customers WHERE id = 'a'"
	for _, c := range []struct {
		first, sql string
		start      bool
		count      string
	}{
		{"a-cust/tx", cust, false, "a-count"},
		{"a-cust/tx.eof", cust, false, "a-count"},
		{"a-cust/tx.pkt", cust, false, "a-count"},
		{"a-exec/tx", "exec:a", false, "a-count"},
		{"a-exec/tx.eof", "exec:a", false, "a-count"},
		{"a-cust/on", cust, false, "b-count"},
		{"a-cust/tx.old", cust, false, "b-count"},
		{"a-cust/tx.bad", cust, false, "b-count"},
		{"a-cust/on+tx", cust, true, "a-count"},
	} {
		t.Run(c.first, func(t *testing.T) {
			recs := []connRec{
				{c.first, "cA", c.sql, 1000 * time.Microsecond, 0},
				{"b-count", "cB", "SELECT COUNT(*) FROM orders", 1500 * time.Microsecond, 0},
				{"a-count", "cA", "SELECT COUNT(*) FROM orders", 2500 * time.Microsecond, 0},
			}
			var calls []connCall
			if c.start {
				calls = append(calls, connCall{"L1", "START TRANSACTION", "*"})
			}
			calls = append(calls,
				connCall{"L1", c.sql, c.first},
				connCall{"L1", "SELECT COUNT(*) FROM orders", c.count})
			on := uint16(0x0002)
			checkConnSession(t, &on, recs, []connTest{{0, 4 * time.Millisecond, calls}})
		})
	}
}

// A server whose global autocommit is off begins every session with it off,
// as its greeting's status says: the session is in a transaction from its
// first statement, so the first read binds though its answer carries no
// status (as an ERR's does not); a COMMIT before it (nothing recorded
// answered it: its OK is keploy's) begins the next transaction at once, so
// the read after it binds too; and a reset nothing recorded answered brings
// that autocommit back. Greeted with autocommit on, none binds.
func TestConnectionRuleReadsTheGreetingsAutocommit(t *testing.T) {
	recs := []connRec{
		{"on/on", "cA", "SET autocommit = 1", 300 * time.Microsecond, 0},
		{"a-cust", "cA", "SELECT * FROM customers WHERE id = 'a'", 1000 * time.Microsecond, 0},
		{"b-count", "cB", "SELECT COUNT(*) FROM orders", 1500 * time.Microsecond, 0},
		{"a-count", "cA", "SELECT COUNT(*) FROM orders", 2500 * time.Microsecond, 0},
	}
	for _, c := range []struct {
		name     string
		greeting uint16
		first    string
		count    string
	}{
		{"off", 0x0000, "", "a-count"},
		{"on", 0x0002, "", "b-count"},
		{"off, commit", 0x0000, "commit", "a-count"},
		{"on, commit", 0x0002, "commit", "b-count"},
		{"off, reset", 0x0000, "reset", "a-count"},
		{"on, reset", 0x0002, "reset", "b-count"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var calls []connCall
			switch c.first {
			case "commit":
				calls = append(calls, connCall{"L1", "COMMIT", "*"})
			case "reset":
				calls = append(calls,
					connCall{"L1", "SET autocommit = 1", "on/on"},
					connCall{"L1", "reset", "*"})
			}
			calls = append(calls,
				connCall{"L1", "SELECT * FROM customers WHERE id = 'a'", "a-cust"},
				connCall{"L1", "SELECT COUNT(*) FROM orders", c.count})
			checkConnSession(t, &c.greeting, recs, []connTest{{0, 4 * time.Millisecond, calls}})
		})
	}
}

// A statement that commits the open transaction implicitly (CREATE TABLE,
// as every DDL and LOCK TABLES do) ends it, as its answer says, though its
// text is no COMMIT: the reads after it are outside every transaction, and
// the first one, answered with no status, binds nothing, so the next is
// answered with the first exact match too. One whose answer says the
// transaction is still open keeps it.
func TestConnectionRuleEndsAtAnImplicitCommit(t *testing.T) {
	for _, c := range []struct{ ddl, count string }{
		{"ddl/on", "b-count"},
		{"ddl/on+tx", "a-count"},
	} {
		t.Run(c.ddl, func(t *testing.T) {
			recs := []connRec{
				{"a-cust", "cA", "SELECT * FROM customers WHERE id = 'a'", 1000 * time.Microsecond, 0},
				{c.ddl, "cA", "CREATE TABLE t (id INT)", 1200 * time.Microsecond, 0},
				{"b-count", "cB", "SELECT COUNT(*) FROM orders", 1500 * time.Microsecond, 0},
				{"a-count", "cA", "SELECT COUNT(*) FROM orders", 2500 * time.Microsecond, 0},
				{"y-a", "cA", "SELECT * FROM products WHERE id = 'y'", 2600 * time.Microsecond, 0},
				{"y-b", "cB", "SELECT * FROM products WHERE id = 'y'", 2700 * time.Microsecond, 0},
			}
			checkConnSet(t, recs, []connTest{{0, 4 * time.Millisecond, []connCall{
				{"L1", "START TRANSACTION", "*"},
				{"L1", "SELECT * FROM customers WHERE id = 'a'", "a-cust"},
				{"L1", "CREATE TABLE t (id INT)", c.ddl},
				{"L1", "SELECT COUNT(*) FROM orders", c.count},
				{"L1", "SELECT * FROM products WHERE id = 'y'", "y-a"},
			}}})
		})
	}
}

// Under autocommit off a COMMIT, or a reset of a session greeted with
// autocommit off, ends the transaction and begins the next at once, and binds
// it to nothing, though the recording that answers it is the test's own and
// carries no status (an ERR): the read after it is answered with the first
// exact match the test owns, recorded on another connection than the COMMIT
// or the reset, not with the one recorded on theirs after them.
func TestConnectionRuleBindsNothingAtAnEndAnsweredWithAnError(t *testing.T) {
	off := uint16(0x0000)
	for _, c := range []struct {
		end      string
		greeting *uint16
	}{
		{"COMMIT", nil},
		{"reset", &off},
	} {
		t.Run(c.end, func(t *testing.T) {
			name := strings.ToLower(c.end) + "/err"
			recs := []connRec{
				{"off/off", "cA", "SET autocommit = 0", 300 * time.Microsecond, 0},
				{name, "cA", c.end, 1000 * time.Microsecond, 0},
				{"y-b", "cB", "SELECT * FROM products WHERE id = 'y'", 1500 * time.Microsecond, 0},
				{"y-a", "cA", "SELECT * FROM products WHERE id = 'y'", 2500 * time.Microsecond, 0},
			}
			checkConnSession(t, c.greeting, recs, []connTest{{0, 4 * time.Millisecond, []connCall{
				{"L1", "SET autocommit = 0", "off/off"},
				{"L1", c.end, name},
				{"L1", "SELECT * FROM products WHERE id = 'y'", "y-b"},
			}}})
		})
	}
}

// Inside a transaction a command answered with no recording (the OK keploy
// writes for a SAVEPOINT nothing recorded), or with one the open test does
// not own (a read recorded outside every test), keeps the binding; and a
// command the bound connection recorded no exact match of is answered with
// the first exact match the test owns, which binds the transaction to its
// connection.
func TestConnectionRuleKeepsTheBindingAcrossOtherAnswers(t *testing.T) {
	recs := []connRec{
		{"cfg-b", "cB", "SELECT v FROM config WHERE k = 'tax'", 7 * time.Millisecond, -1},
		{"a-cust", "cA", "SELECT * FROM customers WHERE id = 'a'", 1000 * time.Microsecond, 0},
		{"b-sel", "cB", "SELECT * FROM products WHERE id = 'x'", 1100 * time.Microsecond, 0},
		{"a-sel", "cA", "SELECT * FROM products WHERE id = 'x'", 1200 * time.Microsecond, 0},
		{"b-only", "cB", "SELECT * FROM products WHERE id = 'z'", 1300 * time.Microsecond, 0},
		{"a-ins", "cA", "INSERT INTO orders VALUES ('a')", 1250 * time.Microsecond, 0},
		{"b-ins", "cB", "INSERT INTO orders VALUES ('a')", 1500 * time.Microsecond, 0},
	}
	checkConnSet(t, recs, []connTest{{0, 4 * time.Millisecond, []connCall{
		{"L1", "START TRANSACTION", "*"},
		{"L1", "SELECT * FROM customers WHERE id = 'a'", "a-cust"},
		{"L1", "SAVEPOINT s1", "*"},
		{"L1", "SELECT v FROM config WHERE k = 'tax'", "cfg-b"},
		{"L1", "SELECT * FROM products WHERE id = 'x'", "a-sel"},
		{"L1", "SELECT * FROM products WHERE id = 'z'", "b-only"},
		{"L1", "INSERT INTO orders VALUES ('a')", "b-ins"},
	}}})
}

// A recording the bound connection made as a reusable one (it answers again)
// does not answer the transaction's next command twice while the connection
// recorded a later one: the next command as recorded is that one.
func TestConnectionRuleTakesTheNextRecordingNotTheBoundAgain(t *testing.T) {
	recs := []connRec{
		{"a-x1", "cA", "SELECT rate FROM fx", 1000 * time.Microsecond, -1},
		{"a-x2", "cA", "SELECT rate FROM fx", 1200 * time.Microsecond, -1},
	}
	checkConnSet(t, recs, []connTest{{0, 4 * time.Millisecond, []connCall{
		{"L1", "START TRANSACTION", "*"},
		{"L1", "SELECT rate FROM fx", "a-x1"},
		{"L1", "SELECT rate FROM fx", "a-x2"},
	}}}, modeTest, modeTestScan)
	// The same statement sent with a comment: not a byte-identical repeat.
	checkConnSet(t, recs, []connTest{{0, 4 * time.Millisecond, []connCall{
		{"L1", "START TRANSACTION", "*"},
		{"L1", "SELECT rate FROM fx", "a-x1"},
		{"L1", "/* again */ SELECT rate FROM fx", "a-x2"},
	}}}, modeTest, modeTestScan)
}

// A reusable recording the bound connection made once answers its
// transaction's repeat of the command again, over the read another test's
// transaction made on another connection, which that test is left.
func TestConnectionRuleAnswersTheBoundReusableRecordingAgain(t *testing.T) {
	recs := []connRec{
		{"a-cust", "cA", "SELECT * FROM customers WHERE id = 'a'", 900 * time.Microsecond, 0},
		{"a-x1", "cA", "SELECT rate FROM fx", 1000 * time.Microsecond, -1},
		{"b-x", "cB", "SELECT rate FROM fx", 1100 * time.Microsecond, 0},
	}
	checkConnSet(t, recs, []connTest{
		{0, 4 * time.Millisecond, []connCall{
			{"L1", "START TRANSACTION", "*"},
			{"L1", "SELECT * FROM customers WHERE id = 'a'", "a-cust"},
			{"L1", "SELECT rate FROM fx", "a-x1"},
			{"L1", "SELECT rate FROM fx", "a-x1"},
			{"L1", "COMMIT", "*"},
		}},
		{1050 * time.Microsecond, 2 * time.Millisecond, []connCall{
			{"L2", "SELECT rate FROM fx", "b-x"},
		}},
	}, modeTest, modeTestScan)
}

// Two tests recorded the same reads. Under exact serving each test is
// answered with its own, inside a bound transaction the one on the bound
// connection first, else one on another connection (it waited), and the view
// shows neither test the other's while it is open, whichever recorded the
// text first. In keploy test, of the EXECUTEs, a definitive match the test
// does not own ranks below the test's own recording of the statement with
// another parameter (the FIFO candidate); exact serving answers neither
// (sameStatementOnly).
func TestConnectionRuleRanksTheTestsOwnAboveAnotherTestsMatch(t *testing.T) {
	recs := []connRec{
		{"a-cust", "cA", "SELECT * FROM customers WHERE id = 'a'", 1000 * time.Microsecond, 0},
		{"o-sel", "cB", "SELECT * FROM products WHERE id = 'x'", 1100 * time.Microsecond, 1},
		{"a-sel", "cA", "SELECT * FROM products WHERE id = 'x'", 1200 * time.Microsecond, 0},
		{"o-sel2", "cB", "SELECT * FROM products WHERE id = 'y'", 1300 * time.Microsecond, 1},
		{"c-sel2", "cC", "SELECT * FROM products WHERE id = 'y'", 1400 * time.Microsecond, 0},
		{"a-w", "cA", "SELECT * FROM products WHERE id = 'w'", 1500 * time.Microsecond, 0},
		{"o-w", "cB", "SELECT * FROM products WHERE id = 'w'", 6 * time.Millisecond, 1},
	}
	checkConnSet(t, recs, []connTest{
		{0, 4 * time.Millisecond, []connCall{
			{"L1", "START TRANSACTION", "*"},
			{"L1", "SELECT * FROM customers WHERE id = 'a'", "a-cust"},
			{"L1", "SELECT * FROM products WHERE id = 'x'", "a-sel"},
			{"L1", "SELECT * FROM products WHERE id = 'y'", "c-sel2"},
			{"L1", "COMMIT", "*"},
		}},
		{5 * time.Millisecond, 9 * time.Millisecond, []connCall{
			{"L2", "SELECT * FROM products WHERE id = 'x'", "o-sel"},
			{"L2", "SELECT * FROM products WHERE id = 'y'", "o-sel2"},
			// Outside a transaction: its own, though the earlier test
			// recorded the text first and left it unanswered.
			{"L2", "SELECT * FROM products WHERE id = 'w'", "o-w"},
		}},
	}, modeExact, modeExactInTime)
	execs := []connRec{
		{"a-cust", "cA", "SELECT * FROM customers WHERE id = 'a'", 1000 * time.Microsecond, 0},
		{"a-fifo", "cA", "exec:y", 1200 * time.Microsecond, 0},
		{"o-exec", "cB", "exec:x", 6 * time.Millisecond, 1},
	}
	checkConnSet(t, execs, []connTest{
		{0, 4 * time.Millisecond, []connCall{
			{"L1", "START TRANSACTION", "*"},
			{"L1", "SELECT * FROM customers WHERE id = 'a'", "a-cust"},
			{"L1", "exec:x", "a-fifo"},
			{"L1", "COMMIT", "*"},
		}},
		{5 * time.Millisecond, 9 * time.Millisecond, []connCall{
			{"L2", "exec:x", "o-exec"},
		}},
	}, modeTest, modeTestScan)
}

// Under exact serving the store lists every mock the open test owns and has
// not answered ahead of the rest, so a command's walk stops at the first
// exact match the test does not own, where main's stops, with no scan of the
// whole pool: each call visits the candidates main's walk visits, with 10 and
// with 1000 reusable recordings of its text that no test owns (a poller's,
// recorded thousands of times), outside a transaction and inside a bound one.
// One call visits one more: inside the bound transaction the test's own read,
// recorded on another connection, waits for the bound one, and the walk goes
// on to the first exact match no test owns and stops there; the waiting read
// outranks it. A command nothing recorded (the START and the COMMIT here) is
// scanned whole, as on main.
func TestConnectionRuleVisitsMainsCandidates(t *testing.T) {
	const poll = "SELECT id FROM jobs WHERE status = 'pending'"
	const sel = "SELECT * FROM products WHERE id = 'x'"
	// want is the recording each call must be answered with; "poll-" is any
	// recording of the poll. visits is how many candidates the call visits:
	// main's walk visits the test's unanswered recordings, then the first
	// exact match (a-cust and b-sel, then a poll, for the first two polls).
	// -1: not counted (a command nothing recorded, scanned whole).
	calls := []struct {
		connCall
		visits int
	}{
		{connCall{"L1", poll, "poll-"}, 3},
		{connCall{"L1", poll, "poll-"}, 3},
		{connCall{"L1", "START TRANSACTION", "*"}, -1},
		{connCall{"L1", "SELECT * FROM customers WHERE id = 'a'", "a-cust"}, 1},
		// The test's own read, recorded on cB, waits for the bound cA, whose
		// recordings of the text are no test's: the walk stops at the first
		// of them (main's stops at b-sel, 1), and b-sel outranks it.
		{connCall{"L1", sel, "b-sel"}, 2},
		{connCall{"L1", poll, "poll-"}, 1},
		{connCall{"L1", "COMMIT", "*"}, -1},
		{connCall{"L1", poll, "poll-"}, 1},
	}
	for _, n := range []int{10, 1000} {
		recs := []connRec{
			{"a-cust", "cA", "SELECT * FROM customers WHERE id = 'a'", 1000 * time.Microsecond, 0},
			{"b-sel", "cB", sel, 1100 * time.Microsecond, 0},
		}
		for i := range n {
			at := 7*time.Millisecond + time.Duration(i)*time.Microsecond
			recs = append(recs,
				connRec{fmt.Sprintf("poll-%d", i), "cA", poll, at, -1},
				connRec{fmt.Sprintf("x-%d", i), "cA", sel, at, -1})
		}
		pool, byTest := connPool(recs)
		mm := proxy.NewMockManager(nil, nil, zap.NewNop())
		t.Cleanup(mm.Close)
		mm.SetMocksWithWindow(nil, pool, models.BaseTime, time.Now().Add(time.Hour))
		mm.ServeExactTest("T0", byTest[0])
		ctx, dctx := connLive("L1")
		for _, c := range calls {
			resp, ok, examined, err := replayer.MatchBoundExaminedForTest(ctx, connReq(c.sql), mm, dctx)
			if err != nil {
				t.Fatalf("n=%d %q: %v", n, c.sql, err)
			}
			if c.visits >= 0 && examined != c.visits {
				t.Errorf("n=%d %q visited %d candidates; want %d", n, c.sql, examined, c.visits)
			}
			if c.want == "*" {
				continue
			}
			if !ok || resp == nil || !strings.HasPrefix(resp.Payload, c.want) || (c.want != "poll-" && resp.Payload != c.want) {
				t.Fatalf("n=%d %q was answered with %v (ok %v); want %s", n, c.sql, resp, ok, c.want)
			}
		}
	}
}

// A recording that names no connection binds nothing: the transaction's
// commands are answered with the first exact match, as before the rule, the
// one recorded before the transaction's latest command's too.
func TestConnectionRuleNeedsTheRecordedConnection(t *testing.T) {
	recs := []connRec{
		{"early-sel", "", "SELECT * FROM products WHERE id = 'x'", 500 * time.Microsecond, 0},
		{"a-cust", "", "SELECT * FROM customers WHERE id = 'a'", 1000 * time.Microsecond, 0},
		{"b-sel", "", "SELECT * FROM products WHERE id = 'x'", 1100 * time.Microsecond, 0},
		{"a-sel", "", "SELECT * FROM products WHERE id = 'x'", 1200 * time.Microsecond, 0},
	}
	checkConnSet(t, recs, []connTest{{0, 4 * time.Millisecond, []connCall{
		{"L1", "START TRANSACTION", "*"},
		{"L1", "SELECT * FROM customers WHERE id = 'a'", "a-cust"},
		{"L1", "SELECT * FROM products WHERE id = 'x'", "early-sel"},
		{"L1", "SELECT * FROM products WHERE id = 'x'", "b-sel"},
	}}})
}

// With no exact match the test owns on the bound connection, the first one
// it owns answers, as before the rule: of the reads, and of the EXECUTEs,
// whose window also holds a recording of the statement with another
// parameter (the FIFO candidate, which a definitive match outranks).
func TestConnectionRuleAnswersTheFirstWaitingRecording(t *testing.T) {
	recs := []connRec{
		{"a-cust", "cA", "SELECT * FROM customers WHERE id = 'a'", 1000 * time.Microsecond, 0},
		{"b-other", "cB", "exec:y", 1050 * time.Microsecond, 0},
		{"b-exec", "cB", "exec:x", 1100 * time.Microsecond, 0},
		{"c-exec", "cC", "exec:x", 1150 * time.Microsecond, 0},
		{"a-cust2", "cA", "SELECT * FROM customers WHERE id = 'a2'", 1300 * time.Microsecond, 0},
		{"b-sel", "cB", "SELECT * FROM products WHERE id = 'x'", 1400 * time.Microsecond, 0},
		{"c-sel", "cC", "SELECT * FROM products WHERE id = 'x'", 1450 * time.Microsecond, 0},
	}
	checkConnSet(t, recs, []connTest{{0, 4 * time.Millisecond, []connCall{
		{"L1", "START TRANSACTION", "*"},
		{"L1", "SELECT * FROM customers WHERE id = 'a'", "a-cust"},
		{"L1", "exec:x", "b-exec"},
		{"L1", "COMMIT", "*"},
		{"L1", "START TRANSACTION", "*"},
		{"L1", "SELECT * FROM customers WHERE id = 'a2'", "a-cust2"},
		{"L1", "SELECT * FROM products WHERE id = 'x'", "b-sel"},
	}}})
}

// A command recorded on the bound connection at the same time as the
// transaction's latest command (a coarse clock) is its next command.
func TestConnectionRuleTakesTheBoundConnectionsCommandAtTheSameTime(t *testing.T) {
	recs := []connRec{
		{"b-sel", "cB", "SELECT * FROM products WHERE id = 'x'", 900 * time.Microsecond, 0},
		{"a-cust", "cA", "SELECT * FROM customers WHERE id = 'a'", 1000 * time.Microsecond, 0},
		{"a-sel", "cA", "SELECT * FROM products WHERE id = 'x'", 1000 * time.Microsecond, 0},
	}
	checkConnSet(t, recs, []connTest{{0, 4 * time.Millisecond, []connCall{
		{"L1", "START TRANSACTION", "*"},
		{"L1", "SELECT * FROM customers WHERE id = 'a'", "a-cust"},
		{"L1", "SELECT * FROM products WHERE id = 'x'", "a-sel"},
	}}})
}

// A binding ends with its test's window: the next test's window, which
// overlaps it and holds the recording the binding was made from, answers its
// first exact match first, as before the rule. Under exact serving it ends
// with its test: the next test does not own the recording.
func TestConnectionRuleEndsWithTheWindow(t *testing.T) {
	recs := []connRec{
		{"a-cust", "cA", "SELECT * FROM customers WHERE id = 'a'", 3200 * time.Microsecond, 0},
		{"c-sel", "cC", "SELECT * FROM products WHERE id = 'x'", 3500 * time.Microsecond, 1},
		{"a-sel", "cA", "SELECT * FROM products WHERE id = 'x'", 3800 * time.Microsecond, 1},
	}
	checkConnSet(t, recs, []connTest{
		{0, 4 * time.Millisecond, []connCall{
			{"L1", "START TRANSACTION", "*"},
			{"L1", "SELECT * FROM customers WHERE id = 'a'", "a-cust"},
		}},
		{3 * time.Millisecond, 8 * time.Millisecond, []connCall{
			{"L1", "SELECT * FROM products WHERE id = 'x'", "c-sel"},
		}},
	})
}

// Which live commands begin or end a transaction.
func TestConnectionRuleReadsTransactionControl(t *testing.T) {
	for sql, want := range map[string]string{
		"START TRANSACTION":                           "begin",
		"start transaction read only":                 "begin",
		"START TRANSACTION WITH CONSISTENT SNAPSHOT;": "begin",
		"BEGIN":                             "begin",
		"begin work":                        "begin",
		"/* tx */ BEGIN":                    "begin",
		"COMMIT":                            "end",
		"commit work;":                      "end",
		"COMMIT AND NO CHAIN":               "end",
		"COMMIT AND CHAIN":                  "begin",
		"ROLLBACK":                          "end",
		"ROLLBACK WORK AND CHAIN":           "begin",
		"ROLLBACK TO SAVEPOINT s1":          "none",
		"rollback work to s1":               "none",
		"SAVEPOINT s1":                      "none",
		"START SLAVE":                       "none",
		"STARTS":                            "none",
		"BEGINNING":                         "none",
		"COMMITTED":                         "none",
		"SELECT * FROM t WHERE c = 'BEGIN'": "none",
		"":                                  "none",
		"  begin":                           "begin",
		"\n/* x */ COMMIT":                  "end",
		"BEGIN NOT ATOMIC SELECT 1; END":    "none",
		"BEGIN;":                            "begin",
		"start transaction;":                "begin",
		"COMMIT;":                           "end",
		"ROLLBACK ;":                        "end",
		// XA: START and BEGIN begin; END, COMMIT and ROLLBACK end.
		"XA START 'x'":             "begin",
		"xa begin 'x', 'b', 1":     "begin",
		"XA START 'x' JOIN":        "begin",
		"XA END 'x'":               "end",
		"XA END 'x' SUSPEND":       "end",
		"xa commit 'x' one phase":  "end",
		"XA ROLLBACK 'x'":          "end",
		"XA PREPARE 'x'":           "none",
		"XA RECOVER":               "none",
		"XA":                       "none",
		"XAX START 'x'":            "none",
		"/* app */ XA COMMIT 'x';": "end",
		// What a SET makes of autocommit, its answer's status says.
		"SET autocommit = 0":         "none",
		"SET @@session.autocommit=1": "none",
	} {
		if got := replayer.TxControlForTest(queryReq(sql)); got != want {
			t.Errorf("%q: %s, want %s", sql, got, want)
		}
	}
	if got := replayer.TxControlForTest(resetReq()); got != "reset" {
		t.Errorf("COM_RESET_CONNECTION: %s, want reset", got)
	}
	changeUser := mysql.Request{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1}, Type: mysql.CommandStatusToString(mysql.COM_CHANGE_USER)},
		Message: &mysql.ChangeUserPacket{Command: mysql.COM_CHANGE_USER},
	}}
	if got := replayer.TxControlForTest(changeUser); got != "reset" {
		t.Errorf("COM_CHANGE_USER: %s, want reset", got)
	}
	if got := replayer.TxControlForTest(executeReq(1, "x")); got != "none" {
		t.Errorf("COM_STMT_EXECUTE: %s, want none", got)
	}
}
