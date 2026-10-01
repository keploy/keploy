package replayer

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// The tests in this file cover matchCommand's in-window pass: while a test
// window is active, a COM_QUERY or COM_STMT_EXECUTE is first compared with the
// candidates recorded inside that window only, and the whole pool is scanned
// only when none of them is an exact match.

var scanBase = time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)

// scanWindow is the [start, end] window of test i in the fixtures below:
// 10ms apart, 4ms long, so a mock at start+7ms falls between two windows.
func scanWindow(i int) (time.Time, time.Time) {
	start := scanBase.Add(time.Duration(i) * 10 * time.Millisecond)
	return start, start.Add(4 * time.Millisecond)
}

// execPrepareMock is the recorded PREPARE of query on connection c1 that
// buildRecordedPrepIndex resolves stmtID to.
func execPrepareMock(name, query string, stmtID uint32) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.MySQL}
	m.Spec.Metadata = map[string]string{"type": "mocks", "connID": "c1"}
	m.Spec.MySQLRequests = []mysql.Request{{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: uint32(len(query) + 1)}, Type: "COM_STMT_PREPARE"},
		Message: &mysql.StmtPreparePacket{Command: 0x16, Query: query},
	}}}
	m.Spec.MySQLResponses = []mysql.Response{{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 12, SequenceID: 1}, Type: mysql.COM_STMT_PREPARE_OK},
		Message: &mysql.StmtPrepareOkPacket{StatementID: stmtID, NumParams: 1},
	}}}
	return m
}

// windowExecMock is a session-tier (lax-promoted) EXECUTE of statement stmtID on
// c1 with one string parameter, recorded at ts; its response carries name.
func windowExecMock(name string, stmtID uint32, param string, ts time.Time) *models.Mock {
	m := execMock(name, param)
	m.Spec.Metadata["connID"] = "c1"
	m.Spec.ReqTimestampMock = ts
	m.Spec.MySQLRequests[0].PacketBundle.Message.(*mysql.StmtExecutePacket).StatementID = stmtID
	m.Spec.MySQLResponses[0].Payload = name
	return m
}

func liveExecReq(stmtID uint32, param string) mysql.Request {
	b := execBundle(param)
	b.Message.(*mysql.StmtExecutePacket).StatementID = stmtID
	return mysql.Request{PacketBundle: b}
}

// scanOutcome is everything matchCommand hands back or hands on: the response
// it serves, the verdict, the miss diagnostics, and the mock it consumed.
type scanOutcome struct {
	served   string
	ok       bool
	err      string
	miss     string
	consumed []string
}

// noWindowReader hides the fake's integrations.SessionWindowReader, so the scan
// takes the path a store without the window index takes.
type noWindowReader struct{ integrations.MockMemDb }

// scanMode is one way matchCommandWith can be asked to find a command's mock.
type scanMode struct {
	name     string
	fullScan bool // matchOptions.fullScan
	reader   bool // the store offers GetSessionMocksInWindow
}

var (
	modeFullScan = scanMode{name: "full scan", fullScan: true}
	modeIndexed  = scanMode{name: "in-window pass, window index", reader: true}
	modeFiltered = scanMode{name: "in-window pass, no window index"}
)

func runScan(t *testing.T, mode scanMode, req mysql.Request, perTest, session []*models.Mock, winStart, winEnd time.Time, eng *schemanoise.Engine, prep func(*mysqlDecodeCtxShim)) scanOutcome {
	t.Helper()
	db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{
		perTest:  append([]*models.Mock(nil), perTest...),
		session:  append([]*models.Mock(nil), session...),
		winStart: winStart,
		winEnd:   winEnd,
	}}
	var store integrations.MockMemDb = db
	if !mode.reader {
		store = noWindowReader{db}
	}
	dctx := newDecodeCtx()
	if prep != nil {
		prep(&mysqlDecodeCtxShim{dctx.StmtIDToQuery})
	}
	resp, ok, miss, err := matchCommandWith(context.Background(), zap.NewNop(), req, store, dctx, eng, nil, matchOptions{fullScan: mode.fullScan})

	var out scanOutcome
	if resp != nil {
		out.served = fmt.Sprintf("%v|%T", resp.Payload, resp.Message)
	}
	out.ok = ok
	if err != nil {
		out.err = err.Error()
	}
	if miss != nil {
		out.miss = fmt.Sprintf("%+v", *miss)
	}
	for _, m := range db.captured {
		out.consumed = append(out.consumed, m.Name)
	}
	return out
}

// mysqlDecodeCtxShim lets a case register the live statement IDs it executes.
type mysqlDecodeCtxShim struct{ stmtIDToQuery map[uint32]string }

// TestMatchCommand_InWindowPassServesWhatTheFullScanServes runs the same
// commands through matchCommand with the in-window pass on and off, over
// generated pools that mix every shape the scan distinguishes: the same text in
// many windows, drifted literals, DML that only matches by structure, mocks
// between windows and mocks with no timestamp, strict gating, and prepared
// EXECUTEs. The pass may only change how much of the pool is compared, never
// what is served, consumed or reported.
func TestMatchCommand_InWindowPassServesWhatTheFullScanServes(t *testing.T) {
	const execQuery = "SELECT v FROM kv WHERE id = ?"
	queryTexts := func(r *rand.Rand) string {
		switch r.Intn(7) {
		case 0:
			return "START TRANSACTION"
		case 1:
			return "COMMIT"
		case 2:
			return fmt.Sprintf("SELECT v FROM kv WHERE id = 'k%d'", r.Intn(4))
		case 3:
			return fmt.Sprintf("INSERT INTO kv (id, v) VALUES ('k%d', %d)", r.Intn(4), r.Intn(3))
		case 4:
			return fmt.Sprintf("UPDATE kv SET v = %d WHERE id = 'k%d'", r.Intn(3), r.Intn(4))
		case 5:
			return "SELECT @@session.transaction_isolation"
		default:
			return fmt.Sprintf("SELECT COUNT(*) FROM kv WHERE v > %d", r.Intn(3))
		}
	}
	liveQueries := []string{
		"START TRANSACTION",
		"COMMIT",
		"SELECT v FROM kv WHERE id = 'k1'",
		"SELECT v FROM kv WHERE id = 'k9'",                         // drifted literal: fallback only
		"INSERT INTO kv (id, v) VALUES ('k2', 1)",                  // exact in some windows
		"INSERT INTO kv (id, v) VALUES ('zz', 7)",                  // structure match only
		"DELETE FROM kv WHERE id = 'k1'",                           // never recorded
		"SELECT @@session.transaction_read_only",                   // a different variable
		"SELECT @@session.transaction_isolation",                   // a recorded variable
		"SELECT COUNT(*) FROM kv WHERE v > 2",                      // exact in some windows
		"/* traceparent=00-ab */ SELECT v FROM kv WHERE id = 'k3'", // comment-prologued
	}

	engines := map[string]func() *schemanoise.Engine{
		"lenient": func() *schemanoise.Engine { return nil },
		"strict":  func() *schemanoise.Engine { return schemanoise.New(mysqlNoiseAdapter{}, false, true) },
	}

	for seed := int64(1); seed <= 12; seed++ {
		r := rand.New(rand.NewSource(seed))
		const windows = 6
		var session []*models.Mock
		perTest := []*models.Mock{execPrepareMock(fmt.Sprintf("s%d-prepare", seed), execQuery, 1)}
		n := 0
		add := func(m *models.Mock) {
			session = append(session, m)
			n++
		}
		for w := 0; w < windows; w++ {
			start, _ := scanWindow(w)
			for j := 0; j < 1+r.Intn(5); j++ {
				ts := start.Add(time.Duration(j) * 700 * time.Microsecond)
				name := fmt.Sprintf("s%d-w%d-q%d", seed, w, n)
				add(readbackMock(name, queryTexts(r), name, ts))
			}
			for j := 0; j < r.Intn(3); j++ {
				ts := start.Add(time.Duration(j) * 900 * time.Microsecond)
				add(windowExecMock(fmt.Sprintf("s%d-w%d-x%d", seed, w, n), 1, fmt.Sprintf("k%d", r.Intn(4)), ts))
			}
			if r.Intn(3) == 0 { // between this window and the next
				name := fmt.Sprintf("s%d-gap%d-q%d", seed, w, n)
				add(readbackMock(name, queryTexts(r), name, start.Add(7*time.Millisecond)))
			}
			if r.Intn(4) == 0 { // no timestamp: in every window
				name := fmt.Sprintf("s%d-zero%d-q%d", seed, w, n)
				add(readbackMock(name, queryTexts(r), name, time.Time{}))
			}
		}
		// The pool is not always in recorded order (a consumed session mock is
		// re-stamped to the end), so shuffle part of it.
		if seed%2 == 0 {
			sort.SliceStable(session, func(i, j int) bool { return r.Intn(5) == 0 })
		}

		type win struct {
			name       string
			start, end time.Time
		}
		var wins []win
		for w := 0; w < windows; w++ {
			s, e := scanWindow(w)
			wins = append(wins, win{fmt.Sprintf("window%d", w), s, e})
		}
		wins = append(wins, win{name: "no-window"})

		for engName, mk := range engines {
			for _, wnd := range wins {
				var reqs []mysql.Request
				var names []string
				for _, q := range liveQueries {
					reqs = append(reqs, comQueryReq(q))
					names = append(names, q)
				}
				for _, p := range []string{"k0", "k1", "k2", "k3", "k9"} {
					reqs = append(reqs, liveExecReq(7, p))
					names = append(names, "EXECUTE "+p)
				}
				for i, req := range reqs {
					prep := func(s *mysqlDecodeCtxShim) { s.stmtIDToQuery[7] = execQuery }
					want := runScan(t, modeFullScan, req, perTest, session, wnd.start, wnd.end, mk(), prep)
					for _, mode := range []scanMode{modeIndexed, modeFiltered} {
						got := runScan(t, mode, req, perTest, session, wnd.start, wnd.end, mk(), prep)
						if fmt.Sprintf("%+v", want) != fmt.Sprintf("%+v", got) {
							t.Errorf("seed %d, %s, %s, %q:\n full scan: %+v\n %s: %+v", seed, engName, wnd.name, names[i], want, mode.name, got)
						}
					}
				}
			}
		}
	}
}

// lanePool builds the shape of a load test's recording: every test runs a short
// transaction, so START TRANSACTION and COMMIT recur in every window while the
// SELECT and the INSERT are unique to their test. All of it is session tier,
// as lax mode promotes per-test MySQL data mocks.
func lanePool(tests int) []*models.Mock {
	pool := make([]*models.Mock, 0, 4*tests)
	for i := 0; i < tests; i++ {
		start, _ := scanWindow(i)
		at := func(n int) time.Time { return start.Add(time.Duration(n) * 500 * time.Microsecond) }
		pool = append(pool,
			readbackMock(fmt.Sprintf("t%d-begin", i), "START TRANSACTION", "ok", at(0)),
			readbackMock(fmt.Sprintf("t%d-select", i), fmt.Sprintf("SELECT id, email FROM customers WHERE id = 'c-%06d'", i), "row", at(1)),
			readbackMock(fmt.Sprintf("t%d-insert", i), fmt.Sprintf("INSERT INTO orders (id, customer_id, total) VALUES ('o-%06d', 'c-%06d', %d)", i, i, i), "ok", at(2)),
			readbackMock(fmt.Sprintf("t%d-commit", i), "COMMIT", "ok", at(3)),
		)
	}
	return pool
}

func laneQueries(i int) []string {
	return []string{
		"START TRANSACTION",
		fmt.Sprintf("SELECT id, email FROM customers WHERE id = 'c-%06d'", i),
		fmt.Sprintf("INSERT INTO orders (id, customer_id, total) VALUES ('o-%06d', 'c-%06d', %d)", i, i, i),
		"COMMIT",
	}
}

// examinedForTest matches test i's four statements in its own window and
// returns how many candidates the scans visited.
func examinedForTest(tb testing.TB, pool []*models.Mock, i int, opts matchOptions) int {
	start, end := scanWindow(i)
	db := &fakeMockDb{session: pool, winStart: start, winEnd: end}
	examined := 0
	opts.examined = &examined
	for _, q := range laneQueries(i) {
		resp, ok, _, err := matchCommandWith(context.Background(), zap.NewNop(), comQueryReq(q), db, newDecodeCtx(), nil, nil, opts)
		if err != nil || !ok || resp == nil {
			tb.Fatalf("test %d: %q was not served (ok=%v err=%v)", i, q, ok, err)
		}
		if got := resp.Payload; got != "ok" && got != "row" {
			tb.Fatalf("test %d: %q served %q", i, q, got)
		}
	}
	return examined
}

// TestMatchCommand_PerTestCostDoesNotGrowWithTheTestIndex is the regression
// guard for a replay whose total time grew with the square of its test count.
// The command scan walked the pool from its start up to the current test's
// window, so test N compared each statement with the mocks of every test
// before it. Matching a test's statements must visit the same number of
// candidates wherever the test sits in the set.
func TestMatchCommand_PerTestCostDoesNotGrowWithTheTestIndex(t *testing.T) {
	const tests = 4000
	pool := lanePool(tests)

	first := examinedForTest(t, pool, 0, matchOptions{})
	last := examinedForTest(t, pool, tests-1, matchOptions{})
	if first != last {
		t.Errorf("test 1 visited %d candidates, test %d visited %d: the scan grows with the test index", first, tests, last)
	}
	// Each statement is the first in-window candidate it can be matched to, or
	// close behind it: a handful of candidates, not the pool.
	if last > 16 {
		t.Errorf("test %d visited %d candidates for 4 statements", tests, last)
	}

	// The counter does see a walk of the pool: the full scan, which is how
	// every command was matched before, passes every earlier test's mocks.
	fullFirst := examinedForTest(t, pool, 0, matchOptions{fullScan: true})
	fullLast := examinedForTest(t, pool, tests-1, matchOptions{fullScan: true})
	if fullLast < len(pool) {
		t.Errorf("the full scan of test %d visited only %d of %d candidates", tests, fullLast, len(pool))
	}
	t.Logf("candidates visited for test 1 / test %d: %d / %d (full scan: %d / %d)", tests, first, last, fullFirst, fullLast)
}

// BenchmarkMatchCommand_TestPosition reports the cost of matching one test's
// statements at the start, the middle and the end of a 4,000-test pool.
func BenchmarkMatchCommand_TestPosition(b *testing.B) {
	const tests = 4000
	pool := lanePool(tests)
	for _, pos := range []struct {
		name string
		i    int
	}{{"first", 0}, {"middle", tests / 2}, {"last", tests - 1}} {
		b.Run(pos.name, func(b *testing.B) {
			for n := 0; n < b.N; n++ {
				examinedForTest(b, pool, pos.i, matchOptions{})
			}
		})
	}
}
