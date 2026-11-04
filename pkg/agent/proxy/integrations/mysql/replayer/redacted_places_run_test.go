package replayer_test

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The agent stages each test's pool from copies of the recordings
// (pkg.FilterConfigMocksMapping). A recording's redacted places are read
// once a run, whichever copy of it a test's pool holds and however many of
// the test's commands miss or log their statement at debug level, and a
// recording with no patterns is not read: a miss against a pool of
// thousands of recordings does not parse thousands of statements again.
// The misses stay redacted by the places kept.
func TestMySQLReadsEachRecordingsPlacesOnceARun(t *testing.T) {
	const redacted, plain, tests = 20, 20, 3
	const secret = "sk9LIVEsecret77"
	var mocks []*models.Mock
	var names []string
	for i := 0; i < redacted+plain; i++ {
		ph := fmt.Sprintf("xq7Lm2Pw9Rt4Zk%06d", i)
		m := diffMock(fmt.Sprintf("m%d", i), "c1", []mysql.Request{queryReq(fmt.Sprintf("SELECT v FROM t%d WHERE api_key = '%s' AND id = %d", i, ph, i))}, diffBase.Add(time.Duration(i)*time.Microsecond), models.LifetimeSession, "mocks")
		if i < redacted {
			m.Noise = []string{"^" + regexp.QuoteMeta(ph) + "$"}
		}
		mocks = append(mocks, m)
		names = append(names, m.Name)
	}
	replayer.ForgetPlacesForTest()
	t.Cleanup(replayer.ForgetPlacesForTest)
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
	dctx := func() *wire.DecodeContext {
		return &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: map[uint32]string{}}
	}
	before := replayer.PlacesReadsForTest()
	for test := 0; test < tests; test++ {
		mm := proxy.NewMockManager(nil, nil, zap.NewNop())
		mm.SetMocksWithWindow(nil, pkg.FilterConfigMocksMapping(ctx, zap.NewNop(), mocks, names), models.BaseTime, time.Now().Add(time.Hour))
		core, logs := observer.New(zapcore.DebugLevel)
		for call := 0; call < 2; call++ {
			live := fmt.Sprintf("SELECT v FROM nosuch WHERE api_key = '%s' AND id = %d", secret, call)
			ok, miss, shown, err := replayer.MatchShownForTest(ctx, zap.New(core), queryReq(live), mm, dctx())
			if err != nil || ok || miss == "" {
				t.Fatalf("test %d call %d: precondition: not a miss: ok %v, err %v", test, call, ok, err)
			}
			if strings.Contains(miss, secret) || strings.Contains(shown, secret) {
				t.Errorf("test %d call %d: the miss shows the live secret:\nreport %s\nshown %s", test, call, miss, shown)
			}
		}
		answered := "SELECT v FROM t1 WHERE api_key = '" + secret + "' AND id = 1"
		if ok, _, _, err := replayer.MatchShownForTest(ctx, zap.New(core), queryReq(answered), mm, dctx()); err != nil || !ok {
			t.Fatalf("test %d: precondition: the debug-logged command was not answered: ok %v, err %v", test, ok, err)
		}
		for _, e := range logs.All() {
			if line := fmt.Sprintf("%s %v", e.Message, e.ContextMap()); strings.Contains(line, secret) {
				t.Errorf("test %d: a log line shows the live secret: %s", test, line)
			}
		}
		mm.Close()
		if n := replayer.PlacesReadsForTest() - before; n != redacted {
			t.Errorf("after test %d, %d recordings were read for their places; want each of the %d with patterns once", test, n, redacted)
		}
	}
}
