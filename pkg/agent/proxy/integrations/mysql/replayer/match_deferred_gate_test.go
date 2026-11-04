package replayer_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// matchGate is match with the strict gate deciding on each fallback of a
// COM_STMT_EXECUTE as the walk meets it (eager) or after the walk, and also
// says what the command learned and how many candidates the gate compared
// with the live request.
func (s *diffSide) matchGate(ctx context.Context, req mysql.Request, dctx *wire.DecodeContext, eng *mocknoise.Engine, eager bool) (string, int) {
	var learned strings.Builder
	resp, ok, miss, compared, err := replayer.MatchCommandGateForTest(ctx, zap.NewNop(), req, learnDb{s.mm, &learned}, dctx, eng, eager)
	return s.describe(resp, ok, miss, err) + "; learned" + learned.String(), compared
}

// TestDeferredGateServesReportsAndLearnsWhatTheEagerGateDoes replays each
// recording twice, the strict gate deciding on a COM_STMT_EXECUTE's FIFO and
// score candidates as the walk meets them on one side and after the walk on
// the other, and requires the two to serve, consume, report (the closest
// recording, its field diffs, how many strict rejected) and learn the same at
// every command. The recordings are replayCorpora's, under every noise
// setting, and the sets of
// TestBetweenTestsCallsOfARecordedSetAreAnsweredWithTheirOwnRequestsRecording
// in each of its replays, with the strict engine off and on, but the strict
// mock replay of 1,000 calls: TestAStrictMockReplayComparesTheRecordingsItsAnswersNeed
// replays that one both ways. The eager side compares every candidate, so the
// sets stop at 1,000 recordings.
func TestDeferredGateServesReportsAndLearnsWhatTheEagerGateDoes(t *testing.T) {
	sideBySide(t)
	seeds := int64(30)
	sizes := []int{100, 1000}
	btSizes := []int{100, 1000}
	if testing.Short() || raceEnabled {
		seeds, sizes, btSizes = 6, sizes[:1], btSizes[:1]
	}
	replayCorporaPerEngine(t, seeds, sizes, [2]string{"eager gate", "deferred gate"},
		func(s *diffSide, ctx context.Context, req mysql.Request, eng *mocknoise.Engine, ref bool) string {
			got, _ := s.matchGate(ctx, req, s.dctx, eng, ref)
			return got
		})
	for _, n := range btSizes {
		for _, engine := range []bool{false, true} {
			for _, rp := range btReplays {
				if n == 1000 && engine && rp.staging == btMockReplay {
					continue
				}
				name := fmt.Sprintf("n=%d/engine=%v/%s-%s", n, engine, rp.staging, rp.timing)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					var eng func() *mocknoise.Engine
					if engine {
						eng = replayer.StrictEngineForTest
					}
					calls, eagerCompared, deferredCompared, own := btReplayBothWays(t, n, eng, rp.staging, rp.timing, func(kind string, call int, c btCall, want, got string) {
						t.Fatalf("%s call %d (%s on %s):\n eager gate:    %s\n deferred gate: %s", kind, call, c.key, c.conn, want, got)
					})
					t.Logf("%d calls (%d recordings of their requests, summed); the gate compared %d candidates eager, %d deferred", calls, own, eagerCompared, deferredCompared)
					if deferredCompared > own {
						t.Errorf("deferred, the gate compared %d candidates over %d calls, want at most the %d recordings of their requests", deferredCompared, calls, own)
					}
				})
			}
		}
	}
}

// btReplayBothWays replays the recorded set of n calls of
// TestBetweenTestsCallsOfARecordedSetAreAnsweredWithTheirOwnRequestsRecording
// (staging, timing) on two stores at once, the strict gate eager on one and
// deferred on the other (eng nil: no engine), and calls differ at a call the
// two answer, consume, report or learn differently. It returns how many calls
// it made, how many candidates the gate compared on each side, and own, the
// sum over the calls of how many recordings of its request the set holds.
func btReplayBothWays(t *testing.T, n int, eng func() *mocknoise.Engine, staging btStaging, timing btTiming, differ func(kind string, call int, c btCall, want, got string)) (calls, eagerCompared, deferredCompared, own int) {
	t.Helper()
	polls := timing == btNextWindow
	preps, startup, tests, between := btRecordedSet(n, 4, int64(n), polls)
	pool := append([]*models.Mock{}, preps...)
	recordings := map[string]int{}
	for _, part := range [][][]btCall{{startup}, tests, between} {
		for _, calls := range part {
			for _, c := range calls {
				pool = append(pool, c.mock)
				recordings[c.key]++
			}
		}
	}
	type side struct {
		*diffSide
		eager bool
		dctxs map[string]*wire.DecodeContext
	}
	sides := []*side{{diffSide: newDiffSide(t, pool), eager: true}, {diffSide: newDiffSide(t, pool)}}
	ctxs := map[string]context.Context{}
	for _, s := range sides {
		defer s.mm.Close()
		if staging != btMockReplay {
			seedWindows(s.mm, len(tests))
		}
		s.dctxs = map[string]*wire.DecodeContext{}
		for c := 1; c <= 4; c++ {
			conn := fmt.Sprintf("c%d", c)
			ctxs[conn] = context.WithValue(context.Background(), models.ClientConnectionIDKey, conn)
			s.dctxs[conn] = &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: btSetStatements(polls), NextStmtID: 100}
		}
	}
	run := func(kind string, c btCall) {
		calls++
		own += recordings[c.key]
		var got [2]string
		for i, s := range sides {
			var e *mocknoise.Engine
			if eng != nil {
				e = eng()
			}
			desc, compared := s.matchGate(ctxs[c.conn], c.req, s.dctxs[c.conn], e, s.eager)
			got[i] = desc
			if s.eager {
				eagerCompared += compared
			} else {
				deferredCompared += compared
			}
		}
		if got[0] != got[1] {
			differ(kind, calls, c, got[0], got[1])
		}
	}
	open := -1
	stage := func(ti int) {
		for _, s := range sides {
			switch staging {
			case btStrictWindow:
				st, en := diffWindow(ti)
				btStageStrictWindow(s.diffSide, st, en)
			case btLax:
				s.stage(diffWindow(ti))
			default:
				return // a mock replay opens no window
			}
		}
		open = ti
	}
	for _, s := range sides {
		s.stage(models.BaseTime, time.Now())
		if staging != btMockReplay {
			s.mm.SetCurrentTestWindow(time.Time{}, time.Time{})
		}
	}
	for _, c := range startup {
		run("startup", c)
	}
	for _, ti := range btReplayedTests(len(tests)) {
		if open != ti {
			stage(ti)
		}
		for _, c := range tests[ti] {
			run("test", c)
		}
		if timing == btNextWindow && ti+1 < len(tests) {
			stage(ti + 1)
		}
		for _, c := range between[ti] {
			run("between", c)
		}
	}
	return calls, eagerCompared, deferredCompared, own
}

// TestAStrictMockReplayComparesTheRecordingsItsAnswersNeed: keploy mock
// replay of a recorded set under strict enforcement opens no window, so each
// call reads every recording of the set, and most are recordings of its
// statement with other parameters, the FIFO candidates' kind. Each call has a
// recording of its own request, which ranks above them, so the strict gate
// compares only the recordings of the call's request (the definitive
// matches, gated as the walk meets them) and none of the others: at most, a
// call, as many as the set holds of its request. Gated as the walk met them,
// every recording of the call's statement was compared, a call (n² a
// replay: 64% of the CPU of the 10,000-call replay, which timed the package
// out under load). Counted, not timed. The bound and the row's check that the
// eager side still pays that cost hold at any size, so under -race, where the
// eager side's n² comparisons of 1,000 calls took 54 s, the set is 200 calls.
func TestAStrictMockReplayComparesTheRecordingsItsAnswersNeed(t *testing.T) {
	sideBySide(t)
	n := 1000
	if raceEnabled {
		n = 200
	}
	calls, eager, deferred, own := btReplayBothWays(t, n, replayer.StrictEngineForTest, btMockReplay, btStillOpen, func(kind string, call int, c btCall, want, got string) {
		t.Fatalf("%s call %d (%s on %s):\n eager gate:    %s\n deferred gate: %s", kind, call, c.key, c.conn, want, got)
	})
	t.Logf("%d calls (%d recordings of their requests, summed); the gate compared %d candidates eager, %d deferred", calls, own, eager, deferred)
	if eager < calls*n/10 {
		t.Fatalf("eager, the gate compared %d candidates over %d calls of a set of %d: the row no longer measures the cost it guards", eager, calls, n)
	}
	if deferred > own {
		t.Errorf("deferred, the gate compared %d candidates over %d calls, want at most the %d recordings of their requests", deferred, calls, own)
	}
}

// TestDeferredGateServesExactServingAsTheEagerGateDoes replays the recorded
// sets of TestExactServingAnswersARecordedSetAsKeployTestDoes under exact
// serving on two stores at once, the strict gate eager on one and deferred on
// the other, and requires the two to answer, consume, report and learn the
// same at every command. Under exact serving a recording of the command's
// statement made with other parameters answers only when every parameter
// that differs is noise, strict enforcement on or not (allowsDrift); in the
// reversed order each command finds such recordings before its own.
func TestDeferredGateServesExactServingAsTheEagerGateDoes(t *testing.T) {
	sideBySide(t)
	sizes := []int{100, 1000}
	if testing.Short() || raceEnabled {
		sizes = sizes[:1]
	}
	for _, n := range sizes {
		for _, run := range setRuns {
			t.Run(fmt.Sprintf("n=%d/%s/strict=%v", n, run.order, run.strict), func(t *testing.T) {
				t.Parallel()
				preps, tests := recordedSet(n, 4, int64(n))
				outside := recordedOutside(4, outsideTests(len(tests)), int64(n))
				sides := [2]*setReplay{
					newSetReplay(t, preps, tests, outside, true, run.strict),
					newSetReplay(t, preps, tests, outside, true, run.strict),
				}
				var compared [2]int
				commands := 0
				both := func(where string, cmds []setCommand) {
					for i, c := range ordered(cmds, run.order) {
						commands++
						var got [2]string
						for k, s := range sides {
							d := &diffSide{mm: s.mm, consumed: map[string]bool{}}
							desc, n := d.matchGate(s.ctxs[c.conn], c.req, s.dctxs[c.conn], s.eng, k == 0)
							got[k] = desc
							compared[k] += n
						}
						if got[0] != got[1] {
							t.Fatalf("%s command %d (%s on %s):\n eager gate:    %s\n deferred gate: %s", where, i, c.key, c.conn, got[0], got[1])
						}
					}
				}
				for _, s := range sides {
					s.openPool(outside)
				}
				both("start-up", outside.startup)
				for _, ti := range replayedTests(len(tests)) {
					for _, s := range sides {
						s.open(tests, ti)
					}
					both(fmt.Sprintf("test %d", ti), tests[ti])
					for _, s := range sides {
						s.openPool(outside)
					}
					both(fmt.Sprintf("after test %d,", ti), outside.after[ti])
				}
				t.Logf("%d commands; the gate compared %d candidates eager, %d deferred", commands, compared[0], compared[1])
			})
		}
	}
}

// TestAMissReportsTheRecordingOfItsRequestStrictRejected: a recording the
// matcher takes for one of a live COM_STMT_EXECUTE's own request (a
// definitive match) can still be one strict enforcement rejects: the matcher
// compares a FLOAT parameter at float32 precision (paramValueEqual), so a
// recorded 1e-300 equals a live 0, while strict compares the values as
// recorded. Strict rejects that recording as the walk meets it, and a
// recording of the statement with another value after it. Nothing answers, and the miss reports what the gate rejected in
// walk order, with the gate deferred as with it eager: it names the first,
// the recording of the command's request, the closest, the field diffs are
// that recording's, and it counts among the two rejected.
func TestAMissReportsTheRecordingOfItsRequestStrictRejected(t *testing.T) {
	floatExec := func(v any) mysql.Request {
		req := executeReq(8, "")
		req.Message.(*mysql.StmtExecutePacket).Parameters[0] = mysql.Parameter{Type: 4 /* MYSQL_TYPE_FLOAT */, Value: v}
		return req
	}
	own, other := floatExec(1e-300), floatExec(2.5)
	own.SetStoredQuery(diffQueryB)
	other.SetStoredQuery(diffQueryB)
	pool := []*models.Mock{
		diffMock("m-own", "c1", []mysql.Request{own}, diffBase, models.LifetimeSession, "mocks"),
		diffMock("m-other", "c1", []mysql.Request{other}, diffBase.Add(time.Millisecond), models.LifetimeSession, "mocks"),
	}
	var got [2]string
	for k, eager := range []bool{true, false} {
		s := newDiffSide(t, pool)
		s.stageNoWindow()
		got[k], _ = s.matchGate(context.Background(), floatExec(float32(0)), s.dctx, replayer.StrictEngineForTest(), eager)
		s.mm.Close()
	}
	if got[0] != got[1] {
		t.Fatalf("the miss differs:\n eager gate:    %s\n deferred gate: %s", got[0], got[1])
	}
	for _, want := range []string{"ok=false", "closestMock:m-own fieldDiffs:[{Path:body.parameters.0.value Kind:value_changed Expected:1e-300 Actual:0}]", "strictRejected:2"} {
		if !strings.Contains(got[1], want) {
			t.Errorf("the miss does not say %q: %s", want, got[1])
		}
	}
}

// TestAMissNamesTheFirstRecordingOfItsRequestStrictRejected: of two
// recordings the matcher takes for a live COM_STMT_EXECUTE's own request,
// both of which strict enforcement rejects (each recorded a FLOAT the
// matcher's float32 comparison reads as the live 0), the miss names the
// first the walk met, as the gate's first rejection, and shows its field
// diffs, with the gate deferred as with it eager.
func TestAMissNamesTheFirstRecordingOfItsRequestStrictRejected(t *testing.T) {
	floatExec := func(v any) mysql.Request {
		req := executeReq(8, "")
		req.Message.(*mysql.StmtExecutePacket).Parameters[0] = mysql.Parameter{Type: 4 /* MYSQL_TYPE_FLOAT */, Value: v}
		return req
	}
	first, second := floatExec(1e-300), floatExec(1e-310)
	first.SetStoredQuery(diffQueryB)
	second.SetStoredQuery(diffQueryB)
	pool := []*models.Mock{
		diffMock("m-first", "c1", []mysql.Request{first}, diffBase, models.LifetimeSession, "mocks"),
		diffMock("m-second", "c1", []mysql.Request{second}, diffBase.Add(time.Millisecond), models.LifetimeSession, "mocks"),
	}
	for _, eager := range []bool{true, false} {
		s := newDiffSide(t, pool)
		s.stageNoWindow()
		got, _ := s.matchGate(context.Background(), floatExec(float32(0)), s.dctx, replayer.StrictEngineForTest(), eager)
		s.mm.Close()
		if want := "closestMock:m-first fieldDiffs:[{Path:body.parameters.0.value Kind:value_changed Expected:1e-300 Actual:0}]"; !strings.Contains(got, want) {
			t.Errorf("eager %v: the miss does not say %q: %s", eager, want, got)
		}
	}
}

// TestAStrictCommandGivesTheEngineItsLiveBodyOnce: a live COM_STMT_EXECUTE
// under strict enforcement, the gate eager, compares its live body with each
// of the three recordings of its statement made with other parameters, and
// rejects them all. It gives MySQL's engine that body once, which validates
// and decodes it once for all three (mocknoise.JSONBody, through the
// adapter's DiffJSONBodies), not once a candidate. Counted, not timed.
func TestAStrictCommandGivesTheEngineItsLiveBodyOnce(t *testing.T) {
	exec := func(v string) mysql.Request {
		req := executeReq(8, "")
		req.Message.(*mysql.StmtExecutePacket).Parameters[0] = mysql.Parameter{Type: 254, Value: v}
		return req
	}
	var pool []*models.Mock
	for i, v := range []string{"a", "b", "c"} {
		rec := exec(v)
		rec.SetStoredQuery(diffQueryB)
		pool = append(pool, diffMock("m-"+v, "c1", []mysql.Request{rec}, diffBase.Add(time.Duration(i)*time.Millisecond), models.LifetimeSession, "mocks"))
	}
	s := newDiffSide(t, pool)
	defer s.mm.Close()
	s.stageNoWindow()
	compared, docs, reads, err := replayer.MatchCommandDocsForTest(context.Background(), zap.NewNop(), exec("live"), s.mm, s.dctx, replayer.StrictEngineForTest(), true)
	if err != nil {
		t.Fatal(err)
	}
	if compared != 3 || docs != 1 || reads != 2 {
		t.Errorf("the gate compared %d candidates and gave the engine %d live bodies, read %d times; want 3, 1 and 2 (validated once, decoded once)", compared, docs, reads)
	}
}

// TestAnExecuteThatScoresOneIsAnsweredByItsScore: an EXECUTE whose statement
// neither side knows (the live statement id was never prepared, the recording
// has no PREPARE) is answered by score, without exact serving, by the
// recording that shares the most of its packet. Here the only recording
// shares one field, its status byte: other header, flags, iteration and
// parameter counts, bind flag and parameters. A score of 1 is still a score,
// with the strict gate eager or deferred.
func TestAnExecuteThatScoresOneIsAnsweredByItsScore(t *testing.T) {
	recorded := mysql.Request{PacketBundle: mysql.PacketBundle{
		Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 40}, Type: "COM_STMT_EXECUTE"},
		Message: &mysql.StmtExecutePacket{Status: 0x17, StatementID: 51, Flags: 1, IterationCount: 2, ParameterCount: 2, NewParamsBindFlag: 1,
			Parameters: []mysql.Parameter{{Type: 254, Value: "x"}, {Type: 254, Value: "y"}}},
	}}
	pool := []*models.Mock{diffMock("scores-one", "c1", []mysql.Request{recorded}, diffBase.Add(time.Millisecond), models.LifetimePerTest, "mocks")}
	live := executeReq(50, "z")
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
	for _, eager := range []bool{true, false} {
		s := newDiffSide(t, pool)
		s.stageNoWindow()
		got, _ := s.matchGate(ctx, live, s.dctx, nil, eager)
		s.mm.Close()
		if !strings.HasPrefix(got, "served scores-one ") {
			t.Errorf("eager gate %v: %s, want the recording that scores 1 served", eager, got)
		}
	}
}

// TestAMockReplayEndsItsWalkAtItsRequestsFirstRecording: keploy mock replay
// without exact serving opens no window, so no recording is the open test's
// own, and nothing ranks above a call's first recording of its own request
// (the gate allowing it): the walk ends there (execDecided). The recorded set
// of 1,000 calls is replayed in its recorded order, and each recording is
// spent once it answers, so at each call the first recording of its
// statement the store holds is the call's own: each call is answered with its
// own recording and examines that one candidate alone. Walked whole, each
// call examined every recording of its statement left. Counted, not timed.
func TestAMockReplayEndsItsWalkAtItsRequestsFirstRecording(t *testing.T) {
	n := 1000
	preps, startup, tests, between := btRecordedSet(n, 4, int64(n), false)
	pool := append([]*models.Mock{}, preps...)
	var calls []btCall
	for _, part := range [][][]btCall{{startup}, tests, between} {
		for _, cs := range part {
			for _, c := range cs {
				pool = append(pool, c.mock)
			}
		}
	}
	calls = append(calls, startup...)
	for _, ti := range btReplayedTests(len(tests)) {
		calls = append(append(calls, tests[ti]...), between[ti]...)
	}
	s := newDiffSide(t, pool)
	defer s.mm.Close()
	s.stage(models.BaseTime, time.Now())
	dctx := &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: btSetStatements(false), NextStmtID: 100}
	for i, c := range calls {
		ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, c.conn)
		resp, ok, examined, err := replayer.MatchCommandExaminedForTest(ctx, c.req, s.mm, dctx, nil, false)
		if err != nil || !ok || resp == nil {
			t.Fatalf("call %d (%s): ok=%v err=%v", i, c.key, ok, err)
		}
		if resp.Payload != c.mock.Name {
			t.Fatalf("call %d (%s): served %v, want %s, its own recording, the first of its request left", i, c.key, resp.Payload, c.mock.Name)
		}
		if examined != 1 {
			t.Fatalf("call %d (%s): examined %d candidates, want 1: its own recording, the first of its statement left", i, c.key, examined)
		}
	}
}

// TestAStartUpExecuteTakesItsStartUpRecordingThoughALaterGapsComesFirst:
// before keploy test's first test no window is open, so no recording is the
// open test's own, and a call's recordings of its request made outside every
// test's window rank by gap (outsideGapRank): the start-up gap's above a
// later gap's. The walk ends at such a recording only at the best rank
// (execDecided). Here the set holds a recording of the call's request made
// between tests 0 and 1 before the start-up one: it does not decide the call,
// and the start-up recording after it answers, as the whole walk answers it.
func TestAStartUpExecuteTakesItsStartUpRecordingThoughALaterGapsComesFirst(t *testing.T) {
	_, end0 := diffWindow(0)
	prep, _, _ := betweenTestsRecording()
	later := diffMock("gap0-exec-5", "c1", []mysql.Request{executeReq(7, "5")}, end0.Add(2*time.Millisecond), models.LifetimePerTest, "mocks")
	startup := diffMock("startup-exec-5", "c1", []mysql.Request{executeReq(7, "5")}, diffBase.Add(-300*time.Microsecond), models.LifetimePerTest, "mocks")
	ctx, _ := betweenTestsConn()
	for _, eager := range []bool{true, false} {
		s := newDiffSide(t, []*models.Mock{prep, later, startup})
		seedWindows(s.mm, 2)
		s.stageNoWindow()
		got, _ := s.matchGate(ctx, executeReq(7, "5"), s.dctx, nil, eager)
		s.mm.Close()
		if !strings.HasPrefix(got, "served startup-exec-5 ") {
			t.Errorf("eager gate and whole walk %v: %s, want startup-exec-5, the start-up gap's recording, served", eager, got)
		}
	}
}

// TestAStartUpExecuteEndsItsWalkAtItsStartUpRecording: before keploy test's
// first test, with the recorded windows known, a call's start-up recording
// of its request holds the best gap rank, so its walk ends there
// (execDecided): of the pool, walked in order (the statement's PREPARE, a
// recording of the request made between tests 0 and 1, the start-up one, one
// made between tests 1 and 2, then the rest), it examines the first three and
// not the later gap's recording after them. Walked whole, the call examines
// all five and is answered the same. Counted, not timed.
func TestAStartUpExecuteEndsItsWalkAtItsStartUpRecording(t *testing.T) {
	_, end0 := diffWindow(0)
	_, end1 := diffWindow(1)
	prep, _, _ := betweenTestsRecording()
	later := diffMock("gap0-exec-5", "c1", []mysql.Request{executeReq(7, "5")}, end0.Add(2*time.Millisecond), models.LifetimePerTest, "mocks")
	startup := diffMock("startup-exec-5", "c1", []mysql.Request{executeReq(7, "5")}, diffBase.Add(-300*time.Microsecond), models.LifetimePerTest, "mocks")
	after := diffMock("gap1-exec-5", "c1", []mysql.Request{executeReq(7, "5")}, end1.Add(2*time.Millisecond), models.LifetimePerTest, "mocks")
	ctx, _ := betweenTestsConn()
	examined := map[bool]int{}
	for _, whole := range []bool{false, true} {
		s := newDiffSide(t, []*models.Mock{prep, later, startup, after})
		seedWindows(s.mm, 2)
		s.stageNoWindow()
		resp, ok, n, err := replayer.MatchCommandWalkForTest(ctx, executeReq(7, "5"), s.mm, s.dctx, whole)
		s.mm.Close()
		if err != nil || !ok || resp == nil || resp.Payload != "startup-exec-5" {
			t.Fatalf("whole walk %v: ok=%v err=%v resp=%v, want startup-exec-5, the start-up gap's recording, served", whole, ok, err, resp)
		}
		examined[whole] = n
	}
	if examined[false] != 3 || examined[true] != 5 {
		t.Fatalf("the walk examined %d candidates and the whole walk %d; want 3 (up to startup-exec-5) and 5 (the whole pool, gap1-exec-5 among them)", examined[false], examined[true])
	}
}
