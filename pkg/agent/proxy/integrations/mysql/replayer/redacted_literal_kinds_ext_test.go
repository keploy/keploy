package replayer_test

import (
	"context"
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

// A live COM_QUERY that writes its secret as a bit number (0b...) at the
// place the recording redacted misses; neither its report nor its debug
// lines show the bits.
func TestMySQLBitNumberMissRedactsItsReportAndLogs(t *testing.T) {
	const placeholder = "xq7Lm2Pw9Rt4Zk"
	const liveBits = "0b0111001101101011001110010100110001001001"
	recorded := "SELECT v FROM kv WHERE api_key = '" + placeholder + "'"
	for _, live := range []string{
		"SELECT v FROM kv WHERE api_key = " + liveBits,
		"SELECT v FROM kv WHERE api_key = " + liveBits + " AND id = 7",
	} {
		t.Run(live[len(live)-12:], func(t *testing.T) {
			own := diffMock("kv", "c1", []mysql.Request{queryReq(recorded)}, diffBase.Add(time.Millisecond), models.LifetimePerTest, "mocks")
			own.Noise = []string{"^" + regexp.QuoteMeta(placeholder) + "$"}
			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			mm.SetMocksWithWindow(nil, []*models.Mock{own}, models.BaseTime, time.Now().Add(time.Hour))
			mm.ServeExactTest("T", []*models.Mock{own})
			db := &stagedUnderMark{MockManager: mm}
			core, logs := observer.New(zapcore.DebugLevel)
			conn := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
			dctx := &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: map[uint32]string{}}
			ok, miss, shown, err := replayer.MatchShownForTest(conn, zap.New(core), queryReq(live), db, dctx)
			lt := unreadPoolLogText(logs.All())
			t.Logf("ok=%v err=%v shown=%q\nmiss=%.300s", ok, err, shown, miss)
			if ok || shown == "" {
				t.Fatalf("the command did not miss: ok=%v shown=%q", ok, shown)
			}
			if strings.Contains(shown, liveBits) {
				t.Errorf("LEAK: the report shows the live bits at the redacted place")
			}
			if strings.Contains(lt, liveBits) {
				t.Errorf("LEAK: the debug logs show the live bits at the redacted place:\n%.1500s", lt)
			}
		})
	}
}

// stagedInRead is a store the replay stages a test set again into while a
// command held to its mark reads the pool's session tier: the staging lands
// during the read, after the mark, not before it.
type stagedInRead struct {
	*proxy.MockManager
	moved func()
}

func (s *stagedInRead) HoldToRound(call integrations.CallRound) (integrations.MockMemDb, func() bool) {
	held, restaged := s.MockManager.HoldToRound(call)
	return movesInRead{MockMemDb: held, keys: held.(integrations.SessionKeyReader), s: s}, restaged
}

// movesInRead is a held store whose session read stages its store's move
// first. It reads the session tier by key, as the held store does, so the
// command's scan leaves the pool unread and its debug lines read it.
type movesInRead struct {
	integrations.MockMemDb
	keys integrations.SessionKeyReader
	s    *stagedInRead
}

func (r movesInRead) RangeSessionMocksWithKey(ix *integrations.MockIndex, key string, fn func(*models.Mock) bool) error {
	return r.keys.RangeSessionMocksWithKey(ix, key, fn)
}

func (r movesInRead) GetSessionMocks() ([]*models.Mock, error) {
	if f := r.s.moved; f != nil {
		r.s.moved = nil
		f()
	}
	return r.MockMemDb.GetSessionMocks() //nolint:staticcheck // SA1019: the matcher reads the pool's session tier through it (loadPool).
}

// The debug lines of a command show its statement's shape, whether or not
// the replay staged a test set again under it while the pool was read: they
// read no places of the pool, so a read that found the set moved changes
// nothing they show.
func TestMySQLLogsOfAPoolReadWhileTheSetIsStagedAgainShowTheShape(t *testing.T) {
	const placeholder = "xq7Lm2Pw9Rt4Zk"
	const secret = "sk9LIVEsecret77"
	// The live statement misses on a clause the recording lacks: its shape
	// is another, so every literal is redacted.
	recorded := "SELECT v FROM kv WHERE api_key = '" + placeholder + "'"
	live := "SELECT v FROM kv WHERE api_key = '" + secret + "' AND id = 2"
	for _, staged := range []bool{false, true} {
		name := "staged nothing"
		if staged {
			name = "staged during the read"
		}
		t.Run(name, func(t *testing.T) {
			own := diffMock("kv", "c1", []mysql.Request{queryReq(recorded)}, diffBase.Add(time.Millisecond), models.LifetimeSession, "mocks")
			own.Noise = []string{"^" + regexp.QuoteMeta(placeholder) + "$"}
			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			stage := func() {
				mm.SetMocksWithWindow(nil, []*models.Mock{own}, models.BaseTime, time.Now().Add(time.Hour))
				mm.ServeExactTest("T", []*models.Mock{own})
			}
			stage()
			db := &stagedInRead{MockManager: mm}
			if staged {
				db.moved = stage
			}
			core, logs := observer.New(zapcore.DebugLevel)
			conn := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
			dctx := &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: map[uint32]string{}}
			ok, miss, shown, err := replayer.MatchShownForTest(conn, zap.New(core), queryReq(live), db, dctx)
			lt := unreadPoolLogText(logs.All())
			t.Logf("ok=%v err=%v shown=%q miss=%.300s", ok, err, shown, miss)
			if ok {
				t.Fatalf("the command was answered")
			}
			if staged && db.moved != nil {
				t.Fatalf("the pool's session tier was never read")
			}
			if strings.Contains(lt, secret) {
				t.Errorf("LEAK: the debug logs show the live secret:\n%.1500s", lt)
			}
			if shape := "SELECT v FROM kv WHERE api_key = ? AND id = ?"; !strings.Contains(lt, shape) {
				t.Errorf("the debug logs do not show the statement's shape %q:\n%.1500s", shape, lt)
			}
		})
	}
}
