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
	seeds := int64(30)
	sizes := []int{100, 1000}
	btSizes := []int{100, 1000}
	if testing.Short() || raceEnabled {
		seeds, sizes, btSizes = 6, sizes[:1], btSizes[:1]
	}
	for _, ne := range noiseEngines {
		learned, rejected := replayCorpora(t, ne.name, ne.eng, seeds, sizes, [2]string{"eager gate", "deferred gate"},
			func(s *diffSide, ctx context.Context, req mysql.Request, eng *mocknoise.Engine, ref bool) string {
				got, _ := s.matchGate(ctx, req, s.dctx, eng, ref)
				return got
			})
		detects, stricts := ne.name == "detect", strings.Contains(ne.name, "strict")
		if detects != (learned > 0) || stricts != (rejected > 0) {
			t.Errorf("noise %s: %d commands learned noise and strict rejected candidates on %d", ne.name, learned, rejected)
		}
	}
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
					calls, eagerCompared, deferredCompared, _ := btReplayBothWays(t, n, eng, rp.staging, rp.timing, func(kind string, call int, c btCall, want, got string) {
						t.Fatalf("%s call %d (%s on %s):\n eager gate:    %s\n deferred gate: %s", kind, call, c.key, c.conn, want, got)
					})
					t.Logf("%d calls; the gate compared %d candidates eager, %d deferred", calls, eagerCompared, deferredCompared)
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
	sides := []*side{{diffSide: newDiffSide(pool), eager: true}, {diffSide: newDiffSide(pool)}}
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
// out under load). Counted, not timed.
func TestAStrictMockReplayComparesTheRecordingsItsAnswersNeed(t *testing.T) {
	n := 1000
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
// walk order, with the gate deferred as with it eager: the field diffs are
// the first's, the recording of the command's request, and it counts among
// the two rejected.
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
		s := newDiffSide(pool)
		s.stageNoWindow()
		got[k], _ = s.matchGate(context.Background(), floatExec(float32(0)), s.dctx, replayer.StrictEngineForTest(), eager)
		s.mm.Close()
	}
	if got[0] != got[1] {
		t.Fatalf("the miss differs:\n eager gate:    %s\n deferred gate: %s", got[0], got[1])
	}
	for _, want := range []string{"ok=false", "fieldDiffs:[{Path:body.parameters.0.value Kind:value_changed Expected:1e-300 Actual:0}]", "strictRejected:2"} {
		if !strings.Contains(got[1], want) {
			t.Errorf("the miss does not say %q: %s", want, got[1])
		}
	}
}
