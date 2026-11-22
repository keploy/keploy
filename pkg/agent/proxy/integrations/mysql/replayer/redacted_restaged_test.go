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
)

// A command whose store could not show it the pool it was decided against
// misses with its report redacting the command whole: the replay staged a
// test set again under it (the held store then reads nothing), or its
// store could not hold it to its round (it reads nothing at all). The
// recording of the command's test set redacted the live value's place.
func TestMySQLRestagedOrUnheldMissRedactsTheCommandWhole(t *testing.T) {
	const placeholder = "xq7Lm2Pw9Rt4Zk"
	const secret = "sk9LIVEsecret77"
	recorded := "SELECT v FROM kv WHERE api_key = '" + placeholder + "'"
	live := "SELECT v FROM kv WHERE api_key = '" + secret + "'"
	for _, tc := range []struct {
		name string
		note string
	}{
		{"staged again before its consume", "staged a test set again"},
		{"its store cannot hold it", integrations.UnheldNote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			own := diffMock("kv", "c1", []mysql.Request{queryReq(recorded)}, diffBase.Add(time.Millisecond), models.LifetimePerTest, "mocks")
			own.Noise = []string{"^" + regexp.QuoteMeta(placeholder) + "$"}
			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			stage := func() {
				mm.SetMocksWithWindow(nil, []*models.Mock{own}, models.BaseTime, time.Now().Add(time.Hour))
				mm.ServeExactTest("T", []*models.Mock{own})
			}
			stage()
			staged := false
			var db integrations.MockMemDb = &stagedBeforeConsume{MockManager: mm, moved: func() { stage(); staged = true }}
			if tc.note == integrations.UnheldNote {
				marks := &stagedUnderMark{MockManager: mm}
				db = marksNoHold{MockMemDb: marks, f: marks}
				staged = true
			}
			conn := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
			dctx := &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: map[uint32]string{}}
			ok, miss, shown, err := replayer.MatchShownForTest(conn, zap.NewNop(), queryReq(live), db, dctx)
			if err != nil || ok || !staged || !strings.Contains(miss, tc.note) {
				t.Fatalf("precondition: ok %v, err %v, staged %v, miss %s: want the miss that says %q", ok, err, staged, miss, tc.note)
			}
			if shown != "(redacted)" {
				t.Errorf("the report shows the command as %q; want it redacted whole", shown)
			}
		})
	}
}

// stagedAfterRead is the agent's store whose held store (HoldToRound) runs
// moved once, right after the command's first read of the open test's
// tier: the replay staging the set again while the command is decided.
type stagedAfterRead struct {
	*proxy.MockManager
	moved func()
}

func (s *stagedAfterRead) HoldToRound(call integrations.CallRound) (integrations.MockMemDb, func() bool) {
	held, restaged := s.MockManager.HoldToRound(call)
	after := s.moved
	s.moved = nil
	return &perTestReadHook{MockMemDb: held, after: after}, restaged
}

type perTestReadHook struct {
	integrations.MockMemDb
	after func()
}

func (h *perTestReadHook) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	mocks, err := h.MockMemDb.GetPerTestMocksInWindow()
	if f := h.after; f != nil {
		h.after = nil
		f()
	}
	return mocks, err
}

// A strict miss decided while the replay staged a test set again redacts
// every field diff too: its pool's places cannot be read.
func TestMySQLRestagedStrictMissRedactsEveryFieldDiff(t *testing.T) {
	recorded := "SELECT v FROM kv WHERE api_key = 'k1' AND org = 'acme'"
	live := "SELECT v FROM kv WHERE api_key = 'k2' AND org = 'globex'"
	kv := diffMock("kv", "c1", []mysql.Request{queryReq(recorded)}, diffBase.Add(time.Millisecond), models.LifetimePerTest, "mocks")
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	stage := func() {
		mm.SetMocksWithWindow(nil, []*models.Mock{kv}, models.BaseTime, time.Now().Add(time.Hour))
		mm.ServeExactTest("T", []*models.Mock{kv})
	}
	stage()
	staged := false
	db := &stagedAfterRead{MockManager: mm, moved: func() { stage(); staged = true }}
	conn := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
	dctx := &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: map[uint32]string{}}
	_, ok, miss, err := replayer.MatchWithEngineForTest(conn, zap.NewNop(), queryReq(live), db, dctx, replayer.StrictEngineForTest())
	if err != nil || ok || !staged || !strings.Contains(miss, "staged a test set again") || !strings.Contains(miss, "body.query.literals") {
		t.Fatalf("precondition: ok %v, err %v, staged %v, miss %s: want the restaged strict miss with its field diffs", ok, err, staged, miss)
	}
	for _, v := range []string{"k2", "globex"} {
		if strings.Contains(miss, v) {
			t.Errorf("a field diff of the restaged miss shows the live value %q: %s", v, miss)
		}
	}
}
