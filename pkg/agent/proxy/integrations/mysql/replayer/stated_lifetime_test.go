package replayer_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	agentSvc "go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// windowedProxy is the part of the proxy keploy test's staging reaches: it
// forwards the agent's stagings to a real mock manager, as proxy.Proxy does.
type windowedProxy struct {
	coreAgent.Proxy
	mm *proxy.MockManager
}

func (p *windowedProxy) SetMocksWithWindow(_ context.Context, f, u []*models.Mock, start, end time.Time) error {
	p.mm.SetMocksWithWindow(f, u, start, end)
	return nil
}

func (p *windowedProxy) FirstTestWindowStart() time.Time { return p.mm.FirstTestWindowStart() }
func (p *windowedProxy) GetPersistentConsumed() map[string]models.MockState {
	return p.mm.GetPersistentConsumed()
}

// A statement test A prepared on a pooled connection is executed in test B,
// as a statement cache does: B is served its own EXECUTE, and test C, which
// never recorded one and runs before B, misses it rather than being answered
// with B's. That is
// a recording whose recorder stated each mock's lifetime (the PREPARE
// connection, the EXECUTE per-test). A recording made before, whose EXECUTE
// the lax fallback serves to every test, is replayed as it always was.
func TestStatedExecuteIsServedOnlyToItsOwnTest(t *testing.T) {
	const query = "SELECT v FROM kv WHERE id = ?"
	windows := [3][2]time.Time{}
	for i := range windows {
		s := diffBase.Add(time.Duration(i) * 10 * time.Millisecond)
		windows[i] = [2]time.Time{s, s.Add(4 * time.Millisecond)}
	}
	for _, stated := range []bool{true, false} {
		name := "recording that states lifetimes"
		if !stated {
			name = "recording made before"
		}
		t.Run(name, func(t *testing.T) {
			prep := diffPrepareMock("prep-in-A", "c1", query, 1, windows[0][0].Add(time.Millisecond))
			exec := diffMock("exec-in-B", "c1", []mysql.Request{executeReq(1, "k1")}, windows[1][0].Add(time.Millisecond), models.LifetimePerTest, "mocks")
			for _, m := range []*models.Mock{prep, exec} {
				// As read from disk: the lifetime is derived again on load.
				m.TestModeInfo = models.TestModeInfo{}
			}
			if stated {
				prep.Spec.Metadata[models.MetadataLifetimeKey] = models.MetadataLifetimeConnection
				exec.Spec.Metadata[models.MetadataLifetimeKey] = models.MetadataLifetimePerTest
			}
			for _, m := range []*models.Mock{prep, exec} {
				m.DeriveLifetime()
			}
			// The CLI hands the agent the per-test pool and the reusable one.
			var perTest, reusable []*models.Mock
			for _, m := range []*models.Mock{prep, exec} {
				if m.TestModeInfo.Lifetime == models.LifetimePerTest {
					perTest = append(perTest, m)
				} else {
					reusable = append(reusable, m)
				}
			}

			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			cfg := &config.Config{}
			cfg.Agent.Mode = models.MODE_TEST
			a := agentSvc.New(zap.NewNop(), nil, &windowedProxy{mm: mm}, nil, nil, cfg)
			ctx := context.Background()
			if err := a.StoreMocks(ctx, perTest, reusable); err != nil {
				t.Fatal(err)
			}
			stage := func(start, end time.Time) {
				t.Helper()
				// keploy test's filter for a test case: its recorded window,
				// strict as keploy.yaml has it by default.
				if err := a.UpdateMockParams(ctx, models.MockFilterParams{AfterTime: start, BeforeTime: end, StrictMockWindow: true, AgentOwnsConsumed: true}); err != nil {
					t.Fatal(err)
				}
			}
			stage(models.BaseTime, time.Now())

			dctx := &wire.DecodeContext{
				PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{},
				StmtIDToQuery:      map[uint32]string{7: query},
				NextStmtID:         100,
			}
			conn := context.WithValue(ctx, models.ClientConnectionIDKey, "c1")
			serve := func(req mysql.Request) string {
				t.Helper()
				resp, ok, _, err := replayer.MatchCommandForTest(conn, zap.NewNop(), req, mm, dctx, nil, false)
				if err != nil {
					t.Fatal(err)
				}
				if !ok || resp == nil {
					return ""
				}
				if resp.Payload != "" {
					return resp.Payload
				}
				return "served"
			}

			stage(windows[0][0], windows[0][1])
			if got := serve(prepareReq(query)); got == "" {
				t.Fatal("test A's PREPARE missed")
			}
			// Test C runs before B, so what it is served is no question of
			// what B consumed.
			stage(windows[2][0], windows[2][1])
			got := serve(executeReq(7, "k1"))
			if stated && got != "" {
				t.Fatalf("test C's EXECUTE, which it never recorded, was answered by %q; want a miss", got)
			}
			// A recording made before replays as it always did: its EXECUTE is
			// reusable under the lax fallback (KEPLOY_STRICT_MOCK_WINDOW not
			// enabling, the default), and test C is answered with it.
			if !stated && exec.TestModeInfo.Lifetime == models.LifetimeSession && !strings.Contains(got, "exec-in-B") {
				t.Fatalf("on a recording made before, test C's EXECUTE was answered by %q; want exec-in-B, as it always was", got)
			}
			stage(windows[1][0], windows[1][1])
			if got := serve(executeReq(7, "k1")); !strings.Contains(got, "exec-in-B") {
				t.Fatalf("test B's EXECUTE was answered by %q, want its own recording exec-in-B", got)
			}
		})
	}
}

// inFlightProxy is windowedProxy that also says, from the manager's recorded
// windows, when the gap before a test's window began (coreAgent.InFlightPlanner),
// as proxy.Proxy does: the agent's filter keeps the calls in flight then.
type inFlightProxy struct{ windowedProxy }

func (p *inFlightProxy) InFlightFrom(start time.Time) (time.Time, bool) {
	return p.mm.InFlightFrom(start)
}

// A call in flight when test-2 began, sent in the gap before its window and
// answered inside it, is test-2's own to the MySQL matcher, as it is to the
// windowed read (proxy.MockManager.GetPerTestMocksInWindow): two recordings
// of one statement, the one in flight and then test-2's own, answer in record
// order, exactly as they do with both inside test-2's window.
func TestACallInFlightIsItsTestsOwnToTheMySQLMatcher(t *testing.T) {
	const query = "SELECT v FROM kv WHERE id = 1"
	w1s, w1e := diffWindow(0)
	w2s, w2e := diffWindow(1)
	for place, sentAt := range map[string]time.Time{
		"in flight":     w1e.Add(3 * time.Millisecond), // in the gap, 4ms-10ms
		"in its window": w2s.Add(500 * time.Microsecond),
	} {
		t.Run(place, func(t *testing.T) {
			first := diffMock("first", "c1", []mysql.Request{queryReq(query)}, sentAt, models.LifetimePerTest, "mocks")
			first.Spec.ResTimestampMock = w2s.Add(time.Millisecond) // answered inside test-2's window
			own := diffMock("own", "c1", []mysql.Request{queryReq(query)}, w2s.Add(2*time.Millisecond), models.LifetimePerTest, "mocks")
			for _, m := range []*models.Mock{first, own} {
				m.Spec.Metadata[models.MetadataLifetimeKey] = models.MetadataLifetimePerTest
				m.TestModeInfo = models.TestModeInfo{}
				m.DeriveLifetime()
			}
			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-1", Start: w1s, End: w1e}, {TestCase: "test-2", Start: w2s, End: w2e}})
			cfg := &config.Config{}
			cfg.Agent.Mode = models.MODE_TEST
			a := agentSvc.New(zap.NewNop(), nil, &inFlightProxy{windowedProxy{mm: mm}}, nil, nil, cfg)
			ctx := context.Background()
			if err := a.StoreMocks(ctx, []*models.Mock{first, own}, nil); err != nil {
				t.Fatal(err)
			}
			stage := func(start, end time.Time) {
				t.Helper()
				if err := a.UpdateMockParams(ctx, models.MockFilterParams{AfterTime: start, BeforeTime: end, StrictMockWindow: true, AgentOwnsConsumed: true}); err != nil {
					t.Fatal(err)
				}
			}
			stage(models.BaseTime, time.Now())
			stage(w1s, w1e)
			stage(w2s, w2e)
			dctx := &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: map[uint32]string{}, NextStmtID: 100}
			conn := context.WithValue(ctx, models.ClientConnectionIDKey, "c1")
			var got []string
			for i := 0; i < 2; i++ {
				resp, ok, _, err := replayer.MatchCommandForTest(conn, zap.NewNop(), queryReq(query), mm, dctx, nil, false)
				if err != nil {
					t.Fatal(err)
				}
				if !ok || resp == nil {
					got = append(got, "MISS")
					continue
				}
				got = append(got, resp.Payload)
			}
			if strings.Join(got, ",") != "first,own" {
				t.Fatalf("served %v, want [first own]: record order, the call in flight first", got)
			}
		})
	}
}

// stagedUnderMark is the agent's store, with moved run once, right after the
// first read of the round a command makes (its mark, MarkCallRound): the
// replay staging the next set while the command is held after that read.
type stagedUnderMark struct {
	*proxy.MockManager
	moved func()
}

func (s *stagedUnderMark) MarkCallRound() integrations.CallRound {
	call := s.MockManager.MarkCallRound()
	if f := s.moved; f != nil {
		s.moved = nil
		f()
	}
	return call
}

// A MySQL command held after its first read of the round while the replay
// stages the next set, whose open test owns a recording that is the command
// itself, is never answered with that recording, nor diagnosed against it:
// the matcher reads the store held to the command's first read
// (integrations.HoldToRound), which finds nothing once the staging moved.
// The command misses, saying the set was staged again under it, and the
// recording, never spent, answers the next set's own command.
func TestAMySQLCommandHeldWhileTheNextSetIsStagedIsNeverAnsweredFromIt(t *testing.T) {
	const query = "SELECT v FROM kv WHERE id = 1"
	at := diffBase.Add(time.Millisecond)
	mine := diffMock("s1-other", "c1", []mysql.Request{queryReq("SELECT 1")}, at, models.LifetimePerTest, "mocks")
	next := diffMock("s2-kv", "c1", []mysql.Request{queryReq(query)}, at.Add(time.Second), models.LifetimePerTest, "mocks")
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	stage := func(own *models.Mock, test string) {
		mm.SetMocksWithWindow(nil, []*models.Mock{own}, models.BaseTime, time.Now().Add(time.Hour))
		mm.ServeExactTest(test, []*models.Mock{own})
	}
	stage(mine, "T1")
	staged := false
	db := &stagedUnderMark{MockManager: mm, moved: func() { stage(next, "S2-T1"); staged = true }}
	conn := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
	dctx := &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: map[uint32]string{}}

	resp, ok, miss, err := replayer.MatchForTest(conn, zap.NewNop(), queryReq(query), db, dctx)
	if err != nil {
		t.Fatal(err)
	}
	if !staged {
		t.Fatal("precondition: the replay never staged the next set under the command")
	}
	if ok {
		t.Fatalf("a command of the set before was answered with the next set's %s: %q", next.Name, resp.Payload)
	}
	if strings.Contains(miss, next.Name) {
		t.Fatalf("a command of the set before was diagnosed against the next set's %s: %s", next.Name, miss)
	}
	if !strings.Contains(miss, "staged a test set again while it decided this command, so keploy answered it with no recording") {
		t.Fatalf("the miss does not say the set was staged again under the command: %s", miss)
	}

	// Never spent on the command before, it answers the next set's own.
	resp, ok, miss, err = replayer.MatchForTest(conn, zap.NewNop(), queryReq(query), db, dctx)
	if err != nil || !ok || resp.Payload != next.Name {
		t.Fatalf("the next set's own command was not answered with %s: ok=%v err=%v miss=%s", next.Name, ok, err, miss)
	}
}

// marksNoHold is a store that marks a command's round
// (integrations.StagedFinder) but does not hold it to that round (no
// integrations.RoundHolder): a parser-side wrapper that forgot to forward it.
type marksNoHold struct {
	integrations.MockMemDb
	f integrations.StagedFinder
}

func (w marksNoHold) MarkCallRound() integrations.CallRound { return w.f.MarkCallRound() }

func (w marksNoHold) FindStagedNotOwned(kind models.Kind, call integrations.CallRound, match func(*models.Mock) bool) (string, bool) {
	return w.f.FindStagedNotOwned(kind, call, match)
}

// A MySQL command marked by a store that cannot hold it to its round is
// never answered from that store (integrations.HoldToRound fails closed),
// whether or not the replay staged the next set under it, and the miss says
// why, never that the set was staged again.
func TestAMySQLCommandMarkedByAStoreThatCannotHoldItIsNeverAnsweredFromIt(t *testing.T) {
	const query = "SELECT v FROM kv WHERE id = 1"
	for _, moves := range []bool{true, false} {
		t.Run(map[bool]string{true: "staging moved", false: "staging never moved"}[moves], func(t *testing.T) {
			at := diffBase.Add(time.Millisecond)
			mine := diffMock("s1-other", "c1", []mysql.Request{queryReq("SELECT 1")}, at, models.LifetimePerTest, "mocks")
			next := diffMock("s2-kv", "c1", []mysql.Request{queryReq(query)}, at.Add(time.Second), models.LifetimePerTest, "mocks")
			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			stage := func(own *models.Mock, test string) {
				mm.SetMocksWithWindow(nil, []*models.Mock{own}, models.BaseTime, time.Now().Add(time.Hour))
				mm.ServeExactTest(test, []*models.Mock{own})
			}
			staged := false
			marks := &stagedUnderMark{MockManager: mm}
			if moves {
				stage(mine, "T1")
				marks.moved = func() { stage(next, "S2-T1"); staged = true }
			} else {
				stage(next, "T1")
			}
			conn := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
			dctx := &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: map[uint32]string{}}

			resp, ok, miss, err := replayer.MatchForTest(conn, zap.NewNop(), queryReq(query), marksNoHold{MockMemDb: marks, f: marks}, dctx)
			if err != nil {
				t.Fatal(err)
			}
			if moves && !staged {
				t.Fatal("precondition: the replay never staged the next set under the command")
			}
			if ok {
				t.Fatalf("a command marked by a store that cannot hold it was answered with %s: %q", next.Name, resp.Payload)
			}
			if strings.Contains(miss, next.Name) {
				t.Fatalf("a command marked by a store that cannot hold it was diagnosed against %s: %s", next.Name, miss)
			}
			if !strings.Contains(miss, integrations.UnheldNote) || strings.Contains(miss, "staged a test set again") {
				t.Fatalf("the miss does not say its store could not hold the command, alone: %s", miss)
			}

			// The bare store holds: the recording, never spent, answers it.
			resp, ok, miss, err = replayer.MatchForTest(conn, zap.NewNop(), queryReq(query), mm, dctx)
			if err != nil || !ok || resp.Payload != next.Name {
				t.Fatalf("the store that holds did not answer the command with %s: ok=%v err=%v miss=%s", next.Name, ok, err, miss)
			}
		})
	}
}

// aloft is a connection with one request aloft (learn.Flight): the parser's
// ctx carries it as the proxy's kept connection does.
type aloft struct{ f *learn.Flight }

func (aloft) Hold() func()              { return func() {} }
func (a aloft) InFlight() *learn.Flight { return a.f }

// The MySQL matcher's consume says whose request it serves
// (integrations.ServingIn): the mock manager tells the learn step the serve
// is the command's connection's request, so a request aloft on a parallel
// connection that reads no recording (a health check) never makes the serve
// one keploy cannot place.
func TestAMySQLServeIsToldInItsCommandsRequest(t *testing.T) {
	const query = "SELECT v FROM kv WHERE id = 1"
	kv := diffMock("kv", "c1", []mysql.Request{queryReq(query)}, diffBase.Add(time.Millisecond), models.LifetimeSession, "mocks")
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetMocksWithWindow(nil, []*models.Mock{kv}, models.BaseTime, time.Now().Add(time.Hour))
	var told []*learn.Flight
	mm.SetServedObserver(func(m *models.Mock) {
		if m.Name == kv.Name {
			told = append(told, nil)
		}
	})
	mm.SetServedInObserver(func(m *models.Mock, f *learn.Flight) {
		if m.Name == kv.Name {
			told = append(told, f)
		}
	})
	in := learn.NewTracker().Lift()
	ctx := integrations.WithRequestHolder(context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1"), aloft{in})
	dctx := &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: map[uint32]string{}}
	resp, ok, miss, err := replayer.MatchForTest(ctx, zap.NewNop(), queryReq(query), mm, dctx)
	if err != nil || !ok || resp.Payload != kv.Name {
		t.Fatalf("the command was not answered with %s: ok=%v err=%v miss=%s", kv.Name, ok, err, miss)
	}
	if len(told) != 1 || told[0] != in {
		t.Fatalf("the serve of %s was told %d time(s), in %v; want once, in the command's request", kv.Name, len(told), told)
	}
}

// stagedBeforeConsume is the agent's store whose held store (HoldToRound)
// runs moved once, right before the command's first consume: the replay
// staging the set again between the matcher's read and its consume.
type stagedBeforeConsume struct {
	*proxy.MockManager
	moved func()
}

func (s *stagedBeforeConsume) HoldToRound(call integrations.CallRound) (integrations.MockMemDb, func() bool) {
	held, restaged := s.MockManager.HoldToRound(call)
	before := s.moved
	s.moved = nil
	return &consumeHook{MockMemDb: held, before: before}, restaged
}

type consumeHook struct {
	integrations.MockMemDb
	before func()
}

func (c *consumeHook) fire() {
	if f := c.before; f != nil {
		c.before = nil
		f()
	}
}

func (c *consumeHook) DeleteFilteredMock(m models.Mock) bool {
	c.fire()
	return c.MockMemDb.DeleteFilteredMock(m)
}

func (c *consumeHook) UpdateUnFilteredMock(old, nw *models.Mock) bool {
	c.fire()
	return c.MockMemDb.UpdateUnFilteredMock(old, nw)
}

// A MySQL command whose match the replay stages the set again under, after
// the matcher read its recording and before it consumed it, is not answered
// with what it read (the held store refuses the consume) and is no match
// error that ends the connection either: it misses, saying the set was
// staged again under it. The recording, never spent, answers the command
// made in the new staging.
func TestAMySQLCommandWhoseConsumeTheStagingRefusedMissesSayingSo(t *testing.T) {
	const query = "SELECT v FROM kv WHERE id = 1"
	own := diffMock("kv", "c1", []mysql.Request{queryReq(query)}, diffBase.Add(time.Millisecond), models.LifetimePerTest, "mocks")
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	stage := func() {
		mm.SetMocksWithWindow(nil, []*models.Mock{own}, models.BaseTime, time.Now().Add(time.Hour))
		mm.ServeExactTest("T", []*models.Mock{own})
	}
	stage()
	staged := false
	db := &stagedBeforeConsume{MockManager: mm, moved: func() { stage(); staged = true }}
	conn := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
	dctx := &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: map[uint32]string{}}

	resp, ok, miss, err := replayer.MatchForTest(conn, zap.NewNop(), queryReq(query), db, dctx)
	if !staged {
		t.Fatal("precondition: the matcher never consumed what it read")
	}
	if err != nil {
		t.Fatalf("a consume the staging refused ended the command with an error, not a miss: %v", err)
	}
	if ok {
		t.Fatalf("a command whose consume the staging refused was answered: %q", resp.Payload)
	}
	if !strings.Contains(miss, "staged a test set again while it decided this command") {
		t.Fatalf("the miss does not say the set was staged again under the command: %s", miss)
	}
	resp, ok, miss, err = replayer.MatchForTest(conn, zap.NewNop(), queryReq(query), db, dctx)
	if err != nil || !ok || resp.Payload != own.Name {
		t.Fatalf("the command made in the new staging was not answered with %s: ok=%v err=%v miss=%s", own.Name, ok, err, miss)
	}
}
