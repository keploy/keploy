package replayer_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// connRec is one recording of a connection-rule set: its name, the
// connection it was recorded on, the command (a text query, or "exec:P", an
// EXECUTE of statement 1 with parameter P), when it was recorded (after
// diffBase), and the test whose mapping owns it under exact serving (-1:
// none).
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
	return queryReq(sql)
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
		pool = append(pool, m)
		byTest[r.test] = append(byTest[r.test], m)
	}
	return pool, byTest
}

// replayConnSet replays tests in order against recs, as keploy test does
// (each test's window staged as the agent stages it) or under exact serving
// (each test's mapping is the recordings it owns), with fullScan the whole
// pool scanned for every command. It returns what each call was answered
// with, "miss" for none. Each live connection keeps its state across tests,
// as an app's pool does.
func replayConnSet(t *testing.T, recs []connRec, tests []connTest, exact, fullScan bool) [][]string {
	t.Helper()
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
				ctxs[c.live] = context.WithValue(context.Background(), models.ClientConnectionIDKey, c.live)
				dctxs[c.live] = &wire.DecodeContext{
					PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{},
					StmtIDToQuery:      map[uint32]string{1: connStmt},
					NextStmtID:         100,
				}
			}
			var resp *mysql.Response
			var ok bool
			var err error
			if fullScan {
				resp, ok, _, err = replayer.MatchBoundForTest(ctxs[c.live], zap.NewNop(), connReq(c.sql), mm, dctxs[c.live], true)
			} else {
				resp, ok, _, err = replayer.MatchWithEngineForTest(ctxs[c.live], zap.NewNop(), connReq(c.sql), mm, dctxs[c.live], nil)
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
	if len(modes) == 0 {
		modes = []string{"keploy test", "keploy test, full scan", "exact serving"}
	}
	for _, mode := range modes {
		got := replayConnSet(t, recs, tests, mode == "exact serving", mode == "keploy test, full scan")
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
	}}}, "exact serving")
}

// COMMIT and ROLLBACK end the binding: a command after them, outside every
// transaction, is answered with the first exact match in the window, as it
// was before the rule, not with one made on the connection the ended
// transaction was bound to.
func TestConnectionRuleEndsAtCommitAndRollback(t *testing.T) {
	// A START, or a COMMIT AND CHAIN, ends the transaction too, and begins
	// another, which the read after it binds.
	for end, next := range map[string]string{
		"COMMIT": "y-a", "rollback work": "y-a", "/* app */ COMMIT AND NO CHAIN": "y-a",
		"START TRANSACTION": "y-b", "COMMIT AND CHAIN": "y-b",
	} {
		t.Run(end, func(t *testing.T) {
			recs := []connRec{
				{"a-cust", "cA", "SELECT * FROM customers WHERE id = 'a'", 1000 * time.Microsecond, 0},
				{"b-count", "cB", "SELECT COUNT(*) FROM orders", 1500 * time.Microsecond, 0},
				{"a-count", "cA", "SELECT COUNT(*) FROM orders", 2500 * time.Microsecond, 0},
				{"y-a", "cA", "SELECT * FROM products WHERE id = 'y'", 2600 * time.Microsecond, 0},
				{"y-b", "cB", "SELECT * FROM products WHERE id = 'y'", 2700 * time.Microsecond, 0},
			}
			checkConnSet(t, recs, []connTest{{0, 4 * time.Millisecond, []connCall{
				{"L1", "START TRANSACTION", "*"},
				{"L1", "SELECT * FROM customers WHERE id = 'a'", "a-cust"},
				{"L1", end, "*"},
				{"L1", "SELECT COUNT(*) FROM orders", "b-count"},
				{"L1", "SELECT * FROM products WHERE id = 'y'", next},
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
// does not answer the transaction's next command twice: the next command as
// recorded is a later recording on that connection.
func TestConnectionRuleTakesTheNextRecordingNotTheBoundAgain(t *testing.T) {
	recs := []connRec{
		{"a-x1", "cA", "SELECT rate FROM fx", 1000 * time.Microsecond, -1},
		{"a-x2", "cA", "SELECT rate FROM fx", 1200 * time.Microsecond, -1},
	}
	checkConnSet(t, recs, []connTest{{0, 4 * time.Millisecond, []connCall{
		{"L1", "START TRANSACTION", "*"},
		{"L1", "SELECT rate FROM fx", "a-x1"},
		{"L1", "SELECT rate FROM fx", "a-x2"},
	}}}, "keploy test", "keploy test, full scan")
}

// A recording that names no connection binds nothing: the transaction's
// commands are answered with the first exact match, as before the rule.
func TestConnectionRuleNeedsTheRecordedConnection(t *testing.T) {
	recs := []connRec{
		{"a-cust", "", "SELECT * FROM customers WHERE id = 'a'", 1000 * time.Microsecond, 0},
		{"b-sel", "", "SELECT * FROM products WHERE id = 'x'", 1100 * time.Microsecond, 0},
		{"a-sel", "", "SELECT * FROM products WHERE id = 'x'", 1200 * time.Microsecond, 0},
	}
	checkConnSet(t, recs, []connTest{{0, 4 * time.Millisecond, []connCall{
		{"L1", "START TRANSACTION", "*"},
		{"L1", "SELECT * FROM customers WHERE id = 'a'", "a-cust"},
		{"L1", "SELECT * FROM products WHERE id = 'x'", "b-sel"},
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
		"SET autocommit = 0":                "none",
		"SELECT * FROM t WHERE c = 'BEGIN'": "none",
		"":                                  "none",
		"  begin":                           "begin",
		"\n/* x */ COMMIT":                  "end",
		"BEGIN NOT ATOMIC SELECT 1; END":    "none",
	} {
		if got := replayer.TxControlForTest(queryReq(sql)); got != want {
			t.Errorf("%q: %s, want %s", sql, got, want)
		}
	}
	reset := mysql.Request{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1}, Type: mysql.CommandStatusToString(mysql.COM_RESET_CONNECTION)},
		Message: &mysql.ResetConnectionPacket{},
	}}
	if got := replayer.TxControlForTest(reset); got != "end" {
		t.Errorf("COM_RESET_CONNECTION: %s, want end", got)
	}
	if got := replayer.TxControlForTest(executeReq(1, "x")); got != "none" {
		t.Errorf("COM_STMT_EXECUTE: %s, want none", got)
	}
}
