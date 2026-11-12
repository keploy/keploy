package replayer_test

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A recording that answered a call is in no later read of the pool: the
// store takes a test's recording out once it answers. The same call made
// once more (a retry) then misses against a pool that holds no recording
// that redacted its secret, and its report and its logs still redact the
// live value at the place the answered recording redacted: the pool whose
// places redact a miss is every recording the call's test held this run,
// answered ones included. Served exactly (each test its own recordings) and
// not; the retry comes on the same connection and on another one, and with
// a recording that redacted nothing still in the test's pool beside it.
func TestMySQLRetryOfAnAnsweredRedactingRecordingStaysRedacted(t *testing.T) {
	const placeholder = "xq7Lm2Pw9Rt4Zk"
	const secret = "sk9LIVEsecret77"
	recorded := "SELECT v FROM kv WHERE api_key = '" + placeholder + "'"
	live := "SELECT v FROM kv WHERE api_key = '" + secret + "'"
	for _, tc := range []struct {
		name      string
		exact     bool
		retryConn string
		// endMoved moves the window's end before the retry: the same
		// test's window still.
		endMoved bool
		// beside keeps a recording of the test that redacted nothing in
		// its pool at the retry: the pool read is not empty.
		beside bool
	}{
		{"served from the test's window, the retry on the same connection", false, "c1", false, false},
		{"served from the test's window, the retry on another connection", false, "c2", false, false},
		{"served from the test's window, its end moved before the retry", false, "c1", true, false},
		{"served from the test's window, beside a recording that redacted nothing", false, "c1", false, true},
		{"served exactly, the retry on the same connection", true, "c1", false, false},
		{"served exactly, the retry on another connection", true, "c2", false, false},
		{"served exactly, beside a recording that redacted nothing", true, "c1", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replayer.ForgetAnsweredForTest()
			t.Cleanup(replayer.ForgetAnsweredForTest)
			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			start, end := diffWindow(1)
			own := diffMock("kv", "c1", []mysql.Request{queryReq(recorded)}, start.Add(time.Millisecond), models.LifetimePerTest, "mocks")
			own.Noise = []string{"^" + regexp.QuoteMeta(placeholder) + "$"}
			const plainSQL = "SELECT name FROM users WHERE id = 1"
			test := []*models.Mock{own}
			if tc.beside {
				test = append(test, diffMock("users", "c1", []mysql.Request{queryReq(plainSQL)}, start.Add(2*time.Millisecond), models.LifetimePerTest, "mocks"))
			}
			if tc.exact {
				mm.SetMocksWithWindow(nil, test, models.BaseTime, time.Now().Add(time.Hour))
				mm.ServeExactTest("T", test)
			} else {
				// The test's window: its recording is in the per-test
				// tier, which an answer takes it out of.
				mm.SetMocksWithWindow(test, nil, start, end)
			}
			dctx := func() *wire.DecodeContext {
				return &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: map[uint32]string{}}
			}
			first := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
			if ok, miss, _, err := replayer.MatchShownForTest(first, zap.NewNop(), queryReq(live), mm, dctx()); err != nil || !ok {
				t.Fatalf("precondition: the first call was not answered by the redacting recording: ok %v, err %v, miss %s", ok, err, miss)
			}
			if tc.endMoved {
				mm.SetMocksWithWindow(nil, nil, start, end.Add(time.Millisecond))
			}
			core, logs := observer.New(zapcore.DebugLevel)
			retry := context.WithValue(context.Background(), models.ClientConnectionIDKey, tc.retryConn)
			ok, miss, shown, err := replayer.MatchShownForTest(retry, zap.New(core), queryReq(live), mm, dctx())
			if err != nil || ok || miss == "" {
				t.Fatalf("precondition: the retry was not a miss: ok %v, err %v, miss %q", ok, err, miss)
			}
			if strings.Contains(miss, secret) {
				t.Errorf("the retry's report carries the live value the answered recording redacted:\n%s", miss)
			}
			if strings.Contains(shown, secret) || !strings.Contains(shown, "(redacted)") {
				t.Errorf("the retry's report shows the command as %q; want its redacted literal redacted", shown)
			}
			for _, e := range logs.All() {
				if text := e.Message + " " + strings.Join(fieldTexts(e), " "); strings.Contains(text, secret) {
					t.Errorf("the retry logged the live value the answered recording redacted: %s", text)
				}
			}
			if tc.beside {
				// The recording that redacted nothing was in the pool the
				// retry was read against: it still answers.
				if ok, miss, _, err := replayer.MatchShownForTest(retry, zap.NewNop(), queryReq(plainSQL), mm, dctx()); err != nil || !ok {
					t.Fatalf("precondition: the recording that redacted nothing was not in the test's pool: ok %v, err %v, miss %s", ok, err, miss)
				}
			}
		})
	}
}

// Two recordings of a test answer, each redacting its own place, and the
// first call is made again and misses: its report and its logs redact the
// places of both, as the test's note (and, served exactly, the store's list
// of the test's answered recordings) holds every recording that answered,
// not the last one alone. Served from the test's window and served exactly.
func TestMySQLRetryOfTheFirstOfTwoAnsweredRecordingsStaysRedacted(t *testing.T) {
	const secretKey, secretOrg = "sk9LIVEsecret77", "orgLIVEsecret88"
	ph1, ph2 := "xq7Lm2Pw9Rt4Zk", "Vb3Nc8Qd1Ws6Jh"
	recA := "SELECT v FROM kv WHERE api_key = '" + ph1 + "' AND org = 'acme'"
	recB := "SELECT v FROM kv WHERE api_key = 'pub' AND org = '" + ph2 + "'"
	liveA := "SELECT v FROM kv WHERE api_key = '" + secretKey + "' AND org = 'acme'"
	liveB := "SELECT v FROM kv WHERE api_key = 'pub' AND org = '" + secretOrg + "'"
	for _, exact := range []bool{false, true} {
		t.Run(map[bool]string{false: "served from the test's window", true: "served exactly"}[exact], func(t *testing.T) {
			replayer.ForgetAnsweredForTest()
			t.Cleanup(replayer.ForgetAnsweredForTest)
			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			start, end := diffWindow(1)
			a := diffMock("kv-a", "c1", []mysql.Request{queryReq(recA)}, start.Add(time.Millisecond), models.LifetimePerTest, "mocks")
			a.Noise = []string{"^" + regexp.QuoteMeta(ph1) + "$"}
			b := diffMock("kv-b", "c1", []mysql.Request{queryReq(recB)}, start.Add(2*time.Millisecond), models.LifetimePerTest, "mocks")
			b.Noise = []string{"^" + regexp.QuoteMeta(ph2) + "$"}
			if exact {
				mm.SetMocksWithWindow(nil, []*models.Mock{a, b}, models.BaseTime, time.Now().Add(time.Hour))
				mm.ServeExactTest("T", []*models.Mock{a, b})
			} else {
				mm.SetMocksWithWindow([]*models.Mock{a, b}, nil, start, end)
			}
			dctx := func() *wire.DecodeContext {
				return &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: map[uint32]string{}}
			}
			conn := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
			for _, sql := range []string{liveA, liveB} {
				if ok, miss, _, err := replayer.MatchShownForTest(conn, zap.NewNop(), queryReq(sql), mm, dctx()); err != nil || !ok {
					t.Fatalf("precondition: %q was not answered: ok %v, err %v, miss %s", sql, ok, err, miss)
				}
			}
			core, logs := observer.New(zapcore.DebugLevel)
			ok, miss, shown, err := replayer.MatchShownForTest(conn, zap.New(core), queryReq(liveA), mm, dctx())
			if err != nil || ok || miss == "" {
				t.Fatalf("precondition: the retry was not a miss: ok %v, err %v, miss %q", ok, err, miss)
			}
			if strings.Contains(miss, secretKey) || strings.Contains(shown, secretKey) {
				t.Errorf("the retry of the first answered call shows its secret:\nshown %s\nreport %s", shown, miss)
			}
			for _, e := range logs.All() {
				if text := e.Message + " " + strings.Join(fieldTexts(e), " "); strings.Contains(text, secretKey) {
					t.Errorf("the retry logged the secret the first answered recording redacted: %s", text)
				}
			}
		})
	}
}

// A store staged with no window open (SetFilteredMocks, the agent's
// fallback for a proxy that opens none): a recording of the test answers,
// and the same call made again misses. Its report is redacted by the
// places of the recordings answered with no window open.
func TestMySQLRetryWithNoWindowOpenStaysRedacted(t *testing.T) {
	const placeholder, secret = "xq7Lm2Pw9Rt4Zk", "sk9LIVEsecret77"
	replayer.ForgetAnsweredForTest()
	t.Cleanup(replayer.ForgetAnsweredForTest)
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	start, _ := diffWindow(1)
	own := diffMock("kv", "c1", []mysql.Request{queryReq("SELECT v FROM kv WHERE api_key = '" + placeholder + "'")}, start.Add(time.Millisecond), models.LifetimePerTest, "mocks")
	own.Noise = []string{"^" + regexp.QuoteMeta(placeholder) + "$"}
	mm.SetFilteredMocks([]*models.Mock{own})
	if s, _ := mm.CurrentTestWindow(); !s.IsZero() {
		t.Fatalf("precondition: a window is open: %v", s)
	}
	dctx := func() *wire.DecodeContext {
		return &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: map[uint32]string{}}
	}
	conn := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
	live := "SELECT v FROM kv WHERE api_key = '" + secret + "'"
	if ok, miss, _, err := replayer.MatchShownForTest(conn, zap.NewNop(), queryReq(live), mm, dctx()); err != nil || !ok {
		t.Fatalf("precondition: the call was not answered with no window open: ok %v, err %v, miss %s", ok, err, miss)
	}
	ok, miss, shown, err := replayer.MatchShownForTest(conn, zap.NewNop(), queryReq(live), mm, dctx())
	if err != nil || ok || miss == "" {
		t.Fatalf("precondition: the retry was not a miss: ok %v, err %v, miss %q", ok, err, miss)
	}
	if strings.Contains(miss, secret) || strings.Contains(shown, secret) {
		t.Errorf("the retry with no window open shows the secret:\nshown %s\nreport %s", shown, miss)
	}
}

// A recording that answered in one test redacts the misses of that test,
// not those of the next: the next test's misses are redacted by the places
// its own recordings redacted, answered ones included, and show the live
// value at a place only the earlier test's recording redacted as sent (and
// every value, before its own recordings are staged). Served from the tests' windows and served exactly (where each answered
// call sends its recording's unredacted literals).
func TestMySQLAnsweredRedactingRecordingDoesNotRedactTheNextTestsMiss(t *testing.T) {
	const secret = "sk9LIVEsecret77"
	ph1, ph2 := "xq7Lm2Pw9Rt4Zk", "Vb3Nc8Qd1Ws6Jh"
	// The first test's recording redacted the key, the next test's the org
	// (it kept the key the call sent).
	first := "SELECT v FROM kv WHERE api_key = '" + ph1 + "' AND org = 'acme'"
	second := "SELECT v FROM kv WHERE api_key = '" + secret + "' AND org = '" + ph2 + "'"
	liveFirst := "SELECT v FROM kv WHERE api_key = '" + secret + "' AND org = 'acme'"
	live := "SELECT v FROM kv WHERE api_key = '" + secret + "' AND org = 'globex'"
	for _, tc := range []struct {
		name  string
		exact bool
		// stagedOnce stages the next test's window with its recording at
		// once, not first with none.
		stagedOnce bool
	}{
		{"served from the tests' windows", false, false},
		{"served from the tests' windows, the next test staged once", false, true},
		{"served exactly", true, false},
	} {
		exact := tc.exact
		t.Run(tc.name, func(t *testing.T) {
			replayer.ForgetAnsweredForTest()
			t.Cleanup(replayer.ForgetAnsweredForTest)
			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			s1, e1 := diffWindow(1)
			s2, e2 := diffWindow(2)
			own := diffMock("kv-1", "c1", []mysql.Request{queryReq(first)}, s1.Add(time.Millisecond), models.LifetimePerTest, "mocks")
			own.Noise = []string{"^" + regexp.QuoteMeta(ph1) + "$"}
			next := diffMock("kv-2", "c1", []mysql.Request{queryReq(second)}, s2.Add(time.Millisecond), models.LifetimePerTest, "mocks")
			next.Noise = []string{"^" + regexp.QuoteMeta(ph2) + "$"}
			if exact {
				mm.SetMocksWithWindow(nil, []*models.Mock{own, next}, models.BaseTime, time.Now().Add(time.Hour))
				mm.ServeExactTest("T1", []*models.Mock{own})
			} else {
				mm.SetMocksWithWindow([]*models.Mock{own}, nil, s1, e1)
			}
			dctx := func() *wire.DecodeContext {
				return &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: map[uint32]string{}}
			}
			conn := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
			answer := func(test, sql string) {
				t.Helper()
				if ok, miss, _, err := replayer.MatchShownForTest(conn, zap.NewNop(), queryReq(sql), mm, dctx()); err != nil || !ok {
					t.Fatalf("precondition: the %s test's call was not answered: ok %v, err %v, miss %s", test, ok, err, miss)
				}
			}
			answer("first", liveFirst)
			if exact {
				mm.ServeExactTest("T2", []*models.Mock{next})
			} else if tc.stagedOnce {
				mm.SetMocksWithWindow([]*models.Mock{next}, nil, s2, e2)
			} else {
				// The next test's window, before its recording is staged:
				// its miss has no place to redact by.
				mm.SetMocksWithWindow(nil, nil, s2, e2)
				if ok, miss, shown, err := replayer.MatchShownForTest(conn, zap.NewNop(), queryReq(live), mm, dctx()); err != nil || ok || miss == "" {
					t.Fatalf("precondition: the next test's first call was not a miss: ok %v, err %v, miss %q", ok, err, miss)
				} else if shown != live {
					t.Errorf("the next test's first miss shows the command as %q; want it as sent, %q: no recording of its test redacted it", shown, live)
				}
				mm.SetMocksWithWindow([]*models.Mock{next}, nil, s2, e2)
			}
			answer("next", live)
			ok, miss, shown, err := replayer.MatchShownForTest(conn, zap.NewNop(), queryReq(live), mm, dctx())
			if err != nil || ok || miss == "" {
				t.Fatalf("precondition: the next test's retry was not a miss: ok %v, err %v, miss %q", ok, err, miss)
			}
			if strings.Contains(shown, "globex") || !strings.Contains(shown, secret) {
				t.Errorf("the next test's miss shows the command as %q; want the org its own recording redacted redacted, and the key only the first test's recording redacted as sent", shown)
			}
		})
	}
}

// fieldTexts is each field of e as its text.
func fieldTexts(e observer.LoggedEntry) []string {
	var out []string
	for k, v := range e.ContextMap() {
		out = append(out, k+"="+stringOf(v))
	}
	return out
}

func stringOf(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	default:
		return fmt.Sprintf("%+v", v)
	}
}
