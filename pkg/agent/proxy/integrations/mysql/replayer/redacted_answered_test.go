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
// not; the retry comes on the same connection and on another one.
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
	}{
		{"served from the test's window, the retry on the same connection", false, "c1", false},
		{"served from the test's window, the retry on another connection", false, "c2", false},
		{"served from the test's window, its end moved before the retry", false, "c1", true},
		{"served exactly, the retry on the same connection", true, "c1", false},
		{"served exactly, the retry on another connection", true, "c2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replayer.ForgetAnsweredForTest()
			t.Cleanup(replayer.ForgetAnsweredForTest)
			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			start, end := diffWindow(1)
			own := diffMock("kv", "c1", []mysql.Request{queryReq(recorded)}, start.Add(time.Millisecond), models.LifetimePerTest, "mocks")
			own.Noise = []string{"^" + regexp.QuoteMeta(placeholder) + "$"}
			if tc.exact {
				mm.SetMocksWithWindow(nil, []*models.Mock{own}, models.BaseTime, time.Now().Add(time.Hour))
				mm.ServeExactTest("T", []*models.Mock{own})
			} else {
				// The test's window: its recording is in the per-test
				// tier, which an answer takes it out of.
				mm.SetMocksWithWindow([]*models.Mock{own}, nil, start, end)
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
		})
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
	for _, exact := range []bool{false, true} {
		t.Run(map[bool]string{false: "served from the tests' windows", true: "served exactly"}[exact], func(t *testing.T) {
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
