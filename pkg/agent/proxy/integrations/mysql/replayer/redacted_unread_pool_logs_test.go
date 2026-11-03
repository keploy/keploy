package replayer_test

import (
	"context"
	"fmt"
	"regexp"
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
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func unreadPoolLogText(entries []observer.LoggedEntry) string {
	enc := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	var b strings.Builder
	for _, e := range entries {
		line, err := enc.EncodeEntry(e.Entry, e.Context)
		if err != nil {
			fmt.Fprintf(&b, "%s %v\n", e.Message, e.ContextMap())
			continue
		}
		b.Write(line.Bytes())
	}
	return b.String()
}

// The debug lines of a command the replay staged a test set again under,
// and of one its store could not hold to its mark, redact its statement
// whole, as its report does: the pool it was decided against cannot be read
// for the places a recording of it redacted.
func TestMySQLRestagedOrUnheldMissLogsRedactTheStatementWhole(t *testing.T) {
	const placeholder = "xq7Lm2Pw9Rt4Zk"
	const secret = "sk9LIVEsecret77"
	recorded := "SELECT v FROM kv WHERE api_key = '" + placeholder + "'"
	live := "SELECT v FROM kv WHERE api_key = '" + secret + "'"
	for _, tc := range []string{"restaged under mark", "unheld"} {
		t.Run(tc, func(t *testing.T) {
			own := diffMock("kv", "c1", []mysql.Request{queryReq(recorded)}, diffBase.Add(time.Millisecond), models.LifetimePerTest, "mocks")
			own.Noise = []string{"^" + regexp.QuoteMeta(placeholder) + "$"}
			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			stage := func() {
				mm.SetMocksWithWindow(nil, []*models.Mock{own}, models.BaseTime, time.Now().Add(time.Hour))
				mm.ServeExactTest("T", []*models.Mock{own})
			}
			stage()
			var db integrations.MockMemDb
			if tc == "unheld" {
				marks := &stagedUnderMark{MockManager: mm}
				db = marksNoHold{MockMemDb: marks, f: marks}
			} else {
				db = &stagedUnderMark{MockManager: mm, moved: stage}
			}
			core, logs := observer.New(zapcore.DebugLevel)
			conn := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
			dctx := &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: map[uint32]string{}}
			ok, miss, shown, err := replayer.MatchShownForTest(conn, zap.New(core), queryReq(live), db, dctx)
			t.Logf("ok=%v err=%v shown=%q miss=%s", ok, err, shown, miss)
			if shown != "(redacted)" {
				t.Errorf("report shows %q", shown)
			}
			if lt := unreadPoolLogText(logs.All()); strings.Contains(lt, secret) {
				t.Errorf("LEAK: the debug logs of the %s miss show the live secret:\n%s", tc, lt)
			}
		})
	}
}

// A PREPARE the replay answers itself is logged at INFO with its statement,
// redacted by the pool's places: whole when the replay staged a test set
// again under its mark, or its store could not hold it to the mark, as the
// pool cannot be read then. A held, readable store redacts the literal the
// recording redacted alone.
func TestMySQLSynthesizedPrepareLogRedactsByAPoolItCanRead(t *testing.T) {
	const placeholder = "xq7Lm2Pw9Rt4Zk"
	const secret = "sk9LIVEsecret77"
	recorded := "SELECT v FROM kv WHERE api_key = '" + placeholder + "'"
	live := "SELECT v, w FROM kv WHERE api_key = '" + secret + "' AND id = ?"
	for _, tc := range []string{"held, readable (control)", "restaged under mark", "unheld"} {
		t.Run(tc, func(t *testing.T) {
			own := diffMock("kv", "c1", []mysql.Request{queryReq(recorded)}, diffBase.Add(time.Millisecond), models.LifetimePerTest, "mocks")
			own.Noise = []string{"^" + regexp.QuoteMeta(placeholder) + "$"}
			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			stage := func() {
				mm.SetMocksWithWindow(nil, []*models.Mock{own}, models.BaseTime, time.Now().Add(time.Hour))
				mm.ServeExactTest("T", []*models.Mock{own})
			}
			stage()
			var db integrations.MockMemDb
			switch tc {
			case "unheld":
				marks := &stagedUnderMark{MockManager: mm}
				db = marksNoHold{MockMemDb: marks, f: marks}
			case "restaged under mark":
				db = &stagedUnderMark{MockManager: mm, moved: stage}
			default:
				db = &stagedUnderMark{MockManager: mm}
			}
			core, logs := observer.New(zapcore.InfoLevel)
			conn := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
			dctx := &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: map[uint32]string{}}
			ok, miss, shown, err := replayer.MatchShownForTest(conn, zap.New(core), prepareReq(live), db, dctx)
			lt := unreadPoolLogText(logs.All())
			t.Logf("ok=%v err=%v shown=%q miss=%.200s\nINFO logs:\n%s", ok, err, shown, miss, lt)
			if strings.Contains(lt, secret) {
				t.Errorf("LEAK: an INFO log of the %s PREPARE shows the live secret", tc)
			}
			// A pool it can read redacts the literal alone, in the
			// client's own text.
			if want := "SELECT v, w FROM kv WHERE api_key = '(redacted)' AND id = ?"; tc == "held, readable (control)" && !strings.Contains(lt, want) {
				t.Errorf("the INFO log of the %s PREPARE does not show %q:\n%s", tc, want, lt)
			}
		})
	}
}
