package replayer_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// betweenTestsRecording is a set whose statement 7 (diffQueryA) was run with
// id 2 between tests 0 and 1, and with id 1 in test 1.
func betweenTestsRecording() (prep, between, own *models.Mock) {
	s0, _ := diffWindow(0)
	s1, _ := diffWindow(1)
	prep = diffPrepareMock("prep-c1", "c1", diffQueryA, 7, diffBase.Add(-time.Millisecond))
	between = diffMock("between-exec-2", "c1", []mysql.Request{executeReq(7, "2")}, s0.Add(7*time.Millisecond), models.LifetimePerTest, "mocks")
	own = diffMock("t1-exec-1", "c1", []mysql.Request{executeReq(7, "1")}, s1.Add(time.Millisecond), models.LifetimePerTest, "mocks")
	return prep, between, own
}

func betweenTestsConn() (context.Context, *wire.DecodeContext) {
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
	return ctx, &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: map[uint32]string{7: diffQueryA}, NextStmtID: 100}
}

func matchPayload(t *testing.T, ctx context.Context, req mysql.Request, db *proxy.MockManager, dctx *wire.DecodeContext) string {
	t.Helper()
	resp, ok, miss, err := replayer.MatchForTest(ctx, zap.NewNop(), req, db, dctx)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v miss=%s", ok, err, miss)
	}
	return resp.Payload
}

// seedWindows gives the store the recorded windows of tests 0..n-1, as keploy
// test sends them (models.MockFilterParams.RecordedWindows).
func seedWindows(mm *proxy.MockManager, n int) {
	ws := make([]models.TestWindow, n)
	for i := range ws {
		ws[i].TestCase = fmt.Sprintf("test-%d", i)
		ws[i].Start, ws[i].End = diffWindow(i)
	}
	mm.SeedRecordedWindows(ws)
}

// A prepared EXECUTE made between two tests is answered with its own
// recording, the one of its statement and parameters, and leaves the next
// test's recording of the statement with other parameters to that test,
// whether the replay sent the recorded windows or not. keploy test leaves a
// test's window open until it stages the next test, so the call is matched
// in test 0's window, where no recording of the statement is left. Its own
// recording was then only the out-of-window fallback, and the first recording
// of the statement with other parameters (the FIFO fallback, test 1's)
// outranked it: the call got test 1's rows and consumed them, and test 1's
// own call was then answered with the same rows again.
func TestABetweenTestsExecuteIsAnsweredWithItsOwnParametersRecording(t *testing.T) {
	for _, seeded := range []bool{false, true} {
		t.Run(fmt.Sprintf("windows=%v", seeded), func(t *testing.T) {
			prep, between, own := betweenTestsRecording()
			side := newDiffSide([]*models.Mock{prep, between, own})
			defer side.mm.Close()
			if seeded {
				seedWindows(side.mm, 2)
			}
			ctx, dctx := betweenTestsConn()

			side.stage(diffWindow(0)) // test 0 ran; its window stays open
			if got := matchPayload(t, ctx, executeReq(7, "2"), side.mm, dctx); got != "between-exec-2" {
				t.Fatalf("the between-tests EXECUTE id=2 was answered with %s; want between-exec-2", got)
			}

			side.stage(diffWindow(1))
			if got := matchPayload(t, ctx, executeReq(7, "1"), side.mm, dctx); got != "t1-exec-1" {
				t.Fatalf("test 1's EXECUTE id=1 was answered with %s; want t1-exec-1", got)
			}
		})
	}
}

// A prepared EXECUTE made while no test window is open is answered with its
// own recording, the one of its statement and parameters, in place of the
// first recording of the statement with other parameters. keploy test opens
// no window before a set's first test (the app's start-up); this row closes
// one, with the same set as the between-tests row above.
func TestAnExecuteWithNoWindowOpenIsAnsweredWithItsOwnParametersRecording(t *testing.T) {
	for _, seeded := range []bool{false, true} {
		t.Run(fmt.Sprintf("windows=%v", seeded), func(t *testing.T) {
			prep, between, own := betweenTestsRecording()
			side := newDiffSide([]*models.Mock{prep, between, own})
			defer side.mm.Close()
			if seeded {
				seedWindows(side.mm, 2)
			}
			ctx, dctx := betweenTestsConn()

			side.stage(diffWindow(0))
			side.mm.SetCurrentTestWindow(time.Time{}, time.Time{}) // no window open
			if got := matchPayload(t, ctx, executeReq(7, "2"), side.mm, dctx); got != "between-exec-2" {
				t.Fatalf("the between-tests EXECUTE id=2 was answered with %s; want between-exec-2", got)
			}

			side.stage(diffWindow(1))
			if got := matchPayload(t, ctx, executeReq(7, "1"), side.mm, dctx); got != "t1-exec-1" {
				t.Fatalf("test 1's EXECUTE id=1 was answered with %s; want t1-exec-1", got)
			}
		})
	}
}

// A start-up EXECUTE is answered with its own recording, made before the
// first test, though the first test recorded the same request, and that
// recording, staged in the first test's tier, comes first in the pool: a
// recording made in a test's window is that test's. Its call is then
// answered with it. Taken by the start-up call, it left test 0's call none of
// its own, and that call was answered with another request's rows.
func TestAStartUpExecuteIsAnsweredWithItsOwnRecordingNotTheFirstTests(t *testing.T) {
	s0, _ := diffWindow(0)
	prep, _, _ := betweenTestsRecording()
	startup := diffMock("startup-exec-5", "c1", []mysql.Request{executeReq(7, "5")}, diffBase.Add(-300*time.Microsecond), models.LifetimePerTest, "mocks")
	t0 := diffMock("t0-exec-5", "c1", []mysql.Request{executeReq(7, "5")}, s0.Add(time.Millisecond), models.LifetimePerTest, "mocks")
	t0b := diffMock("t0-exec-6", "c1", []mysql.Request{executeReq(7, "6")}, s0.Add(2*time.Millisecond), models.LifetimePerTest, "mocks")
	side := newDiffSide([]*models.Mock{prep, startup, t0, t0b})
	defer side.mm.Close()
	seedWindows(side.mm, 1)
	ctx, dctx := betweenTestsConn()

	side.stage(diffWindow(0))
	side.mm.SetCurrentTestWindow(time.Time{}, time.Time{}) // start-up: test 0 not begun
	if got := matchPayload(t, ctx, executeReq(7, "5"), side.mm, dctx); got != "startup-exec-5" {
		t.Fatalf("the start-up EXECUTE id=5 was answered with %s; want startup-exec-5", got)
	}
	side.stage(diffWindow(0))
	if got := matchPayload(t, ctx, executeReq(7, "5"), side.mm, dctx); got != "t0-exec-5" {
		t.Fatalf("test 0's EXECUTE id=5 was answered with %s; want t0-exec-5", got)
	}
}

// A between-tests EXECUTE whose own recording the set lacks, but which a test
// recorded with the call's parameters, is answered with that recording, not
// with another test's recording of the statement with other parameters: no
// candidate is the open test's own, and a recording of the call's own
// parameters is the stronger evidence.
func TestABetweenTestsExecuteWithNoRecordingOfItsOwnTakesOneOfItsParameters(t *testing.T) {
	s1, _ := diffWindow(1)
	s2, _ := diffWindow(2)
	prep, _, _ := betweenTestsRecording()
	t1 := diffMock("t1-exec-1", "c1", []mysql.Request{executeReq(7, "1")}, s1.Add(time.Millisecond), models.LifetimePerTest, "mocks")
	t2 := diffMock("t2-exec-2", "c1", []mysql.Request{executeReq(7, "2")}, s2.Add(time.Millisecond), models.LifetimePerTest, "mocks")
	side := newDiffSide([]*models.Mock{prep, t1, t2})
	defer side.mm.Close()
	seedWindows(side.mm, 3)
	ctx, dctx := betweenTestsConn()

	side.stage(diffWindow(0)) // test 0 ran; its window stays open
	if got := matchPayload(t, ctx, executeReq(7, "2"), side.mm, dctx); got != "t2-exec-2" {
		t.Fatalf("the between-tests EXECUTE id=2 was answered with %s; want t2-exec-2, the recording of its parameters", got)
	}
}

// A stated limit, not the right answer for every call: inside a window the
// open test's own recording of the statement, made with other parameters,
// still outranks a recording with the call's parameters made outside every
// test. That is right for the test's own call (a read-back of an id the
// replay made anew). It is wrong for a call the app made between test 0 and
// test 1 that the replay sends once test 1 began: that call takes test 1's
// recording, and test 1's own call then takes the between-tests one. This row
// holds the answer the matcher gives, so a change to it is seen.
func TestStatedLimitInsideAWindowTheTestsOwnRecordingOutranksABetweenTestsOne(t *testing.T) {
	prep, between, own := betweenTestsRecording()
	side := newDiffSide([]*models.Mock{prep, between, own})
	defer side.mm.Close()
	ctx, dctx := betweenTestsConn()

	side.stage(diffWindow(1))
	if got := matchPayload(t, ctx, executeReq(7, "2"), side.mm, dctx); got != "t1-exec-1" {
		t.Fatalf("an EXECUTE id=2 in test 1's window was answered with %s; the matcher gives test 1's t1-exec-1 (stated limit)", got)
	}
}

// gapRecording is a recording of EXECUTE id=<p> of statement 7 named name,
// made between test i and test i+1 (diffWindow: a test's window is 4ms of
// every 10ms).
func gapRecording(name string, i int, p string, offset time.Duration) *models.Mock {
	s, _ := diffWindow(i)
	return diffMock(name, "c1", []mysql.Request{executeReq(7, p)}, s.Add(5*time.Millisecond+offset), models.LifetimePerTest, "mocks")
}

// pollRecording is a set whose statement 7 was run with id 9 (a status poll)
// once in each of gaps 0..n-1 (after tests 0..n-1) and by no test.
func pollRecording(n int) []*models.Mock {
	prep, _, _ := betweenTestsRecording()
	pool := []*models.Mock{prep}
	for i := 0; i < n; i++ {
		pool = append(pool, gapRecording(fmt.Sprintf("gap%d-poll", i), i, "9", 0))
	}
	return pool
}

// A request the app makes between every two tests (a status poll) was
// recorded once in each gap, and keploy test keeps a recording made outside
// every test in the session pool after it answers. keploy test sends a test
// as soon as the previous one is answered, so a poll made after test i is
// usually replayed inside test i+1's window. Each poll is answered with the
// recording of the gap it was made in: gap 0's, gap 1's, gap 2's, not gap
// 0's every time, whose rows may differ.
func TestEachBetweenTestsCallOfARequestIsAnsweredWithItsOwnGapsRecording(t *testing.T) {
	side := newDiffSide(pollRecording(3))
	defer side.mm.Close()
	seedWindows(side.mm, 4)
	ctx, dctx := betweenTestsConn()

	for i := 0; i < 3; i++ {
		side.stage(diffWindow(i + 1)) // test i+1 began
		want := fmt.Sprintf("gap%d-poll", i)
		if got := matchPayload(t, ctx, executeReq(7, "9"), side.mm, dctx); got != want {
			t.Fatalf("the poll made after test %d, sent in test %d's window, was answered with %s; want %s", i, i+1, got, want)
		}
	}
}

// A known limit: a poll made after test 1 that the replay sends while test
// 1's window is still open (before it sent test 2) falls in the gap after
// the open window, which the matcher ranks below the gap before it (the
// usual timing's, above), so it is answered with gap 0's recording again
// when gap 0 holds one. No event tells the matcher which of the two timings
// it is in. This row holds the answer the matcher gives, so a change to it
// is seen. With no recording in the gap before, the gap after answers, above
// an earlier gap's: the poll after test 0 takes gap 0's, and the poll after
// test 3 (gap 2 holds none) gap 3's.
func TestStatedLimitACallSentWhileItsTestsWindowIsStillOpenTakesTheGapBeforeIt(t *testing.T) {
	side := newDiffSide(append(pollRecording(2), gapRecording("gap3-poll", 3, "9", 0)))
	defer side.mm.Close()
	seedWindows(side.mm, 5)
	ctx, dctx := betweenTestsConn()

	side.stage(diffWindow(0))
	if got := matchPayload(t, ctx, executeReq(7, "9"), side.mm, dctx); got != "gap0-poll" {
		t.Fatalf("the poll made after test 0, with its window open, was answered with %s; want gap0-poll", got)
	}
	side.stage(diffWindow(1))
	if got := matchPayload(t, ctx, executeReq(7, "9"), side.mm, dctx); got != "gap0-poll" {
		t.Fatalf("the poll made after test 1, with its window open, was answered with %s; the matcher gives gap0-poll (stated limit)", got)
	}
	side.stage(diffWindow(3))
	if got := matchPayload(t, ctx, executeReq(7, "9"), side.mm, dctx); got != "gap3-poll" {
		t.Fatalf("the poll made after test 3, with its window open, was answered with %s; want gap3-poll, not an earlier gap's", got)
	}
}

// A known limit: a poll made after test 0 that the replay sends two tests
// later, with test 2's window open (a background job slower than test 1's
// whole replay), takes the recording of the gap before the open window, gap
// 1's, a later gap's, where main's pool order gave it gap 0's. The poll made
// after test 1, sent in test 3's window, then takes gap 0's, the one left.
// No event tells the matcher how late a call is. This row holds the answers
// the matcher gives, so a change to them is seen.
func TestStatedLimitACallReplayedTwoTestsAfterItsGapTakesALaterGapsRecording(t *testing.T) {
	side := newDiffSide(pollRecording(2))
	defer side.mm.Close()
	seedWindows(side.mm, 5)
	ctx, dctx := betweenTestsConn()

	side.stage(diffWindow(2))
	if got := matchPayload(t, ctx, executeReq(7, "9"), side.mm, dctx); got != "gap1-poll" {
		t.Fatalf("the poll made after test 0, sent in test 2's window, was answered with %s; the matcher gives gap1-poll (stated limit)", got)
	}
	side.stage(diffWindow(3))
	if got := matchPayload(t, ctx, executeReq(7, "9"), side.mm, dctx); got != "gap0-poll" {
		t.Fatalf("the poll made after test 1, sent in test 3's window, was answered with %s; the matcher gives gap0-poll (stated limit)", got)
	}
}

// A call made between test 0 and test 1 that the replay sends once test 1
// began (keploy test sends a test as soon as the previous one is answered) is
// in the gap before the open window: with test 1 holding no recording of the
// statement, it is answered with that gap's recording, not with the one of
// the same request made at start-up, an earlier gap.
func TestABetweenTestsCallSentOnceTheNextTestBeganTakesTheGapBeforeIt(t *testing.T) {
	prep, _, _ := betweenTestsRecording()
	startup := diffMock("startup-exec-2", "c1", []mysql.Request{executeReq(7, "2")}, diffBase.Add(-500*time.Microsecond), models.LifetimePerTest, "mocks")
	gap0 := gapRecording("gap0-exec-2", 0, "2", 0)
	side := newDiffSide([]*models.Mock{prep, startup, gap0})
	defer side.mm.Close()
	seedWindows(side.mm, 2)
	ctx, dctx := betweenTestsConn()

	side.stage(diffWindow(1))
	if got := matchPayload(t, ctx, executeReq(7, "2"), side.mm, dctx); got != "gap0-exec-2" {
		t.Fatalf("the EXECUTE id=2 made after test 0, sent in test 1's window, was answered with %s; want gap0-exec-2", got)
	}
}

// Two calls of one request in one gap, each recorded, are answered with the
// two recordings in recorded order: one that answered goes behind the others
// in the session pool.
func TestTwoBetweenTestsCallsOfARequestInOneGapTakeTheGapsTwoRecordings(t *testing.T) {
	prep, _, _ := betweenTestsRecording()
	first := gapRecording("gap0-exec-2-first", 0, "2", 0)
	second := gapRecording("gap0-exec-2-second", 0, "2", time.Millisecond)
	side := newDiffSide([]*models.Mock{prep, first, second})
	defer side.mm.Close()
	seedWindows(side.mm, 2)
	ctx, dctx := betweenTestsConn()

	side.stage(diffWindow(0))
	for _, want := range []string{"gap0-exec-2-first", "gap0-exec-2-second"} {
		if got := matchPayload(t, ctx, executeReq(7, "2"), side.mm, dctx); got != want {
			t.Fatalf("an EXECUTE id=2 after test 0 was answered with %s; want %s", got, want)
		}
	}
}

// A between-tests call whose request no recording outside every test holds,
// but two tests recorded, is answered with the earlier test's recording, one
// its replay left over, not a later test's, which that test's call, still to
// come, will want.
func TestABetweenTestsCallTakesTheEarlierTestsRecordingOfItsParameters(t *testing.T) {
	s0, _ := diffWindow(0)
	s3, _ := diffWindow(3)
	prep, _, _ := betweenTestsRecording()
	t0 := diffMock("t0-exec-2", "c1", []mysql.Request{executeReq(7, "2")}, s0.Add(time.Millisecond), models.LifetimePerTest, "mocks")
	t3 := diffMock("t3-exec-2", "c1", []mysql.Request{executeReq(7, "2")}, s3.Add(time.Millisecond), models.LifetimePerTest, "mocks")
	side := newDiffSide([]*models.Mock{prep, t0, t3})
	defer side.mm.Close()
	seedWindows(side.mm, 4)
	ctx, dctx := betweenTestsConn()

	side.stage(diffWindow(1)) // test 1 ran; its window stays open
	if got := matchPayload(t, ctx, executeReq(7, "2"), side.mm, dctx); got != "t0-exec-2" {
		t.Fatalf("the EXECUTE id=2 after test 1 was answered with %s; want t0-exec-2, test 0's left over", got)
	}
}

// A start-up call whose request was recorded by no start-up call, but by test
// 0 and between tests 2 and 3, is answered with the one made outside every
// test, not test 0's, which test 0's own call then takes: a test's recording
// answers its own call once.
func TestAStartUpCallWithNoStartUpRecordingLeavesTheTestsRecordingToItsCall(t *testing.T) {
	s0, _ := diffWindow(0)
	prep, _, _ := betweenTestsRecording()
	t0 := diffMock("t0-exec-2", "c1", []mysql.Request{executeReq(7, "2")}, s0.Add(time.Millisecond), models.LifetimePerTest, "mocks")
	gap2 := gapRecording("gap2-exec-2", 2, "2", 0)
	side := newDiffSide([]*models.Mock{prep, t0, gap2})
	defer side.mm.Close()
	seedWindows(side.mm, 4)
	ctx, dctx := betweenTestsConn()

	side.stage(models.BaseTime, time.Now()) // keploy test's first staging: no window
	if got := matchPayload(t, ctx, executeReq(7, "2"), side.mm, dctx); got != "gap2-exec-2" {
		t.Fatalf("the start-up EXECUTE id=2 was answered with %s; want gap2-exec-2, not test 0's", got)
	}
	side.stage(diffWindow(0))
	if got := matchPayload(t, ctx, executeReq(7, "2"), side.mm, dctx); got != "t0-exec-2" {
		t.Fatalf("test 0's EXECUTE id=2 was answered with %s; want its own t0-exec-2", got)
	}
}

// Inside a window a recording with the call's parameters made before the
// window (a start-up lookup of a row a test then writes) does not stop the
// scan: the window's own recording with those parameters, later in the pool
// (lax mode promotes a test's recordings to the session pool in recorded
// order), is the answer, by the window's index and by a scan of the whole pool.
func TestInsideAWindowAnEarlierRecordingWithTheCallsParametersIsNotItsAnswer(t *testing.T) {
	s1, e1 := diffWindow(1)
	prep, _, _ := betweenTestsRecording()
	startup := diffMock("startup-exec-2", "c1", []mysql.Request{executeReq(7, "2")}, diffBase.Add(-500*time.Microsecond), models.LifetimeSession, "mocks")
	own := diffMock("t1-exec-2", "c1", []mysql.Request{executeReq(7, "2")}, s1.Add(time.Millisecond), models.LifetimeSession, "mocks")
	for _, fullScan := range []bool{false, true} {
		mm := proxy.NewMockManager(nil, nil, zap.NewNop())
		mm.SetMocksWithWindow(nil, []*models.Mock{prep, startup, own}, s1, e1)
		ctx, dctx := betweenTestsConn()
		resp, ok, miss, err := replayer.MatchCommandForTest(ctx, zap.NewNop(), executeReq(7, "2"), mm, dctx, nil, fullScan)
		mm.Close()
		if err != nil || !ok {
			t.Fatalf("fullScan=%v: ok=%v err=%v miss=%s", fullScan, ok, err, miss)
		}
		if resp.Payload != "t1-exec-2" {
			t.Fatalf("fullScan=%v: test 1's EXECUTE id=2 was answered with %s; want its own t1-exec-2", fullScan, resp.Payload)
		}
	}
}

// Under exact serving the open test's own recordings are its mapping's (no
// window is open while a test runs). Its own recording of the statement made
// with other parameters does not answer the call: that is another call's
// rows (sameStatementOnly). A recording with the call's parameters that no
// test owns (a worker-scope one) does. With the call's parameters, the
// test's own outranks it, as inside a window.
func TestUnderExactServingTheOpenTestsOwnRecordingWithOtherParametersDoesNotAnswer(t *testing.T) {
	prep, worker, own := betweenTestsRecording()
	ownOfTheCall := diffMock("t1-exec-2", "c1", []mysql.Request{executeReq(7, "2")}, own.Spec.ReqTimestampMock.Add(time.Millisecond), models.LifetimePerTest, "mocks")
	for _, c := range []struct {
		name string
		own  *models.Mock
		want string
	}{
		{"its own with other parameters", own, "between-exec-2"},
		{"its own with the call's", ownOfTheCall, "t1-exec-2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			defer mm.Close()
			mm.SetMocksWithWindow(nil, []*models.Mock{prep, worker, c.own}, models.BaseTime, time.Now().Add(time.Hour))
			mm.ServeExactWorker([]string{worker.Name})
			mm.ServeExactTest("T1", []*models.Mock{c.own})
			ctx, dctx := betweenTestsConn()
			if got := matchPayload(t, ctx, executeReq(7, "2"), mm, dctx); got != c.want {
				t.Fatalf("test T1's EXECUTE id=2 was answered with %s; want %s", got, c.want)
			}
		})
	}
}

// Under exact serving, of two recordings with the call's parameters that are
// not the open test's own, the first in the view's order answers, as on main
// (keploy #4940): the worker scope's, made in test 0's window, before one
// made after that window. Where a recording was made ranks them only without
// exact serving.
func TestUnderExactServingTwoRecordingsOfTheCallsParametersRankInTheViewsOrder(t *testing.T) {
	s0, _ := diffWindow(0)
	prep, _, _ := betweenTestsRecording()
	worker := diffMock("worker-in-t0-exec-2", "c1", []mysql.Request{executeReq(7, "2")}, s0.Add(time.Millisecond), models.LifetimePerTest, "mocks")
	gap := diffMock("gap0-exec-2", "c1", []mysql.Request{executeReq(7, "2")}, s0.Add(7*time.Millisecond), models.LifetimePerTest, "mocks")
	own := diffMock("t1-query", "c1", []mysql.Request{queryReq(poolTextQuery)}, s0.Add(12*time.Millisecond), models.LifetimePerTest, "mocks")
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow(nil, []*models.Mock{prep, worker, gap, own}, models.BaseTime, time.Now().Add(time.Hour))
	seedWindows(mm, 2)
	mm.ServeExactWorker([]string{worker.Name, gap.Name})
	mm.ServeExactTest("T1", []*models.Mock{own})
	if !mm.ExactServing() {
		t.Fatal("precondition: test T1 is not served exactly")
	}
	ctx, dctx := betweenTestsConn()

	if got := matchPayload(t, ctx, executeReq(7, "2"), mm, dctx); got != "worker-in-t0-exec-2" {
		t.Fatalf("an EXECUTE id=2 in test T1 was answered with %s; want worker-in-t0-exec-2, as main serves", got)
	}
}

// Under exact serving with the background rules (proven routing) the view
// shows the worker scope before the pool. Of two recordings with the call's
// parameters that are not the open test's own, the worker scope's answers, as
// on main, and not the pool's made at start-up: where a recording was made
// ranks them only without exact serving.
func TestUnderExactServingAStartUpRecordingDoesNotOutrankTheWorkerScopesTheViewShowsFirst(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	s0, _ := diffWindow(0)
	prep, _, _ := betweenTestsRecording()
	startup := diffMock("startup-exec-2", "c1", []mysql.Request{executeReq(7, "2")}, diffBase.Add(-500*time.Microsecond), models.LifetimePerTest, "mocks")
	worker := diffMock("worker-exec-2", "c1", []mysql.Request{executeReq(7, "2")}, s0.Add(7*time.Millisecond), models.LifetimePerTest, "mocks")
	own := diffMock("t1-query", "c1", []mysql.Request{queryReq(poolTextQuery)}, s0.Add(12*time.Millisecond), models.LifetimePerTest, "mocks")
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow(nil, []*models.Mock{prep, startup, worker, own}, models.BaseTime, time.Now().Add(time.Hour))
	seedWindows(mm, 2)
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{startup.Name}, []string{worker.Name})
	mm.ServeExactWorker([]string{worker.Name})
	mm.ServeExactTest("T1", []*models.Mock{own})
	if !mm.ExactServing() {
		t.Fatal("precondition: test T1 is not served exactly")
	}
	ctx, dctx := betweenTestsConn()

	if got := matchPayload(t, ctx, executeReq(7, "2"), mm, dctx); got != "worker-exec-2" {
		t.Fatalf("an EXECUTE id=2 in test T1 was answered with %s; want worker-exec-2, the first the view shows, as main serves", got)
	}
}

// keploy mock replay (switch off) stages the whole set at once and opens no
// window, so no recording is a test's own: a prepared EXECUTE is answered with
// the recording of its statement and parameters, not with the first one of
// the statement with other parameters, which answered every such call before.
func TestKeployMockReplayAnswersAnExecuteWithItsOwnParametersRecording(t *testing.T) {
	prep, _, _ := betweenTestsRecording()
	a := diffMock("rec-exec-1", "c1", []mysql.Request{executeReq(7, "1")}, diffBase.Add(time.Millisecond), models.LifetimePerTest, "mocks")
	b := diffMock("rec-exec-2", "c1", []mysql.Request{executeReq(7, "2")}, diffBase.Add(2*time.Millisecond), models.LifetimePerTest, "mocks")
	side := newDiffSide([]*models.Mock{prep, a, b})
	defer side.mm.Close()
	side.stage(models.BaseTime, time.Now())
	ctx, dctx := betweenTestsConn()
	if got := matchPayload(t, ctx, executeReq(7, "2"), side.mm, dctx); got != "rec-exec-2" {
		t.Fatalf("the EXECUTE id=2 was answered with %s; want rec-exec-2", got)
	}
	if got := matchPayload(t, ctx, executeReq(7, "1"), side.mm, dctx); got != "rec-exec-1" {
		t.Fatalf("the EXECUTE id=1 was answered with %s; want rec-exec-1", got)
	}
}

// Under the proven switch, on a set served by the background rules, a command
// identical to one the open test's own recording already answered is one
// command more than the test recorded: the background never answers it,
// though its pool (a recording made at start-up) or its worker scope (one a
// worker made in an earlier test's window) holds a recording of it. It
// misses as an extra call, recorded once, and the pool's recording stays the
// background's: between tests it answers the command. Answered by the
// background's copy, a test that ran a recorded write twice passed.
func TestTheBackgroundNeverAnswersACommandOneMoreThanTheTestRecorded(t *testing.T) {
	for _, scope := range []string{"pool", "worker"} {
		t.Run(scope, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			s0, _ := diffWindow(0)
			s1, _ := diffWindow(1)
			prep, _, _ := betweenTestsRecording()
			at := s0.Add(7 * time.Millisecond)
			if scope == "pool" {
				at = diffBase.Add(-500 * time.Microsecond)
			}
			bg := diffMock(scope+"-exec-1", "c1", []mysql.Request{executeReq(7, "1")}, at, models.LifetimePerTest, "mocks")
			// other is the background's recording of the statement with other
			// parameters: no call of the test's own, so it answers one.
			other := diffMock(scope+"-exec-2", "c1", []mysql.Request{executeReq(7, "2")}, at.Add(time.Microsecond), models.LifetimePerTest, "mocks")
			own := diffMock("t1-exec-1", "c1", []mysql.Request{executeReq(7, "1")}, s1.Add(time.Millisecond), models.LifetimePerTest, "mocks")
			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			defer mm.Close()
			mm.SetMocksWithWindow(nil, []*models.Mock{prep, bg, other, own}, models.BaseTime, time.Now().Add(time.Hour))
			seedWindows(mm, 2)
			var pool []string
			if scope == "pool" {
				pool = []string{bg.Name, other.Name}
				mm.ServeExactBackground(models.RecordedWithS12Routing, pool, nil)
			} else {
				mm.ServeExactBackground(models.RecordedWithS12Routing, nil, []string{bg.Name, other.Name})
				mm.ServeExactWorker([]string{bg.Name, other.Name})
			}
			mm.ServeExactTest("T1", []*models.Mock{own})
			if !mm.ExactServing() {
				t.Fatal("precondition: test T1 is not served exactly")
			}
			ctx, dctx := betweenTestsConn()

			if got := matchPayload(t, ctx, executeReq(7, "1"), mm, dctx); got != own.Name {
				t.Fatalf("T1's EXECUTE id=1 was answered with %s; want its own %s", got, own.Name)
			}
			resp, ok, miss, err := replayer.MatchForTest(ctx, zap.NewNop(), executeReq(7, "1"), mm, dctx)
			if ok || err != nil {
				payload := ""
				if resp != nil {
					payload = resp.Payload
				}
				t.Fatalf("T1's second EXECUTE id=1, one more than it recorded, was answered with %q (err %v)", payload, err)
			}
			if !strings.Contains(miss, "extraCall:1") {
				t.Fatalf("the second EXECUTE id=1 missed with %s; want an extra call, recorded once", miss)
			}
			if got := matchPayload(t, ctx, executeReq(7, "2"), mm, dctx); got != other.Name {
				t.Fatalf("T1's EXECUTE id=2, a call it never recorded, was answered with %s; want the background's %s", got, other.Name)
			}

			if scope != "pool" {
				return
			}
			mm.ServeExactPool(models.PoolKeyNone, pool)
			if got := matchPayload(t, ctx, executeReq(7, "1"), mm, dctx); got != bg.Name {
				t.Fatalf("the EXECUTE id=1 between tests was answered with %s; want the pool's %s", got, bg.Name)
			}
		})
	}
}
