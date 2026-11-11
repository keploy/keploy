package replayer_test

import (
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// viewMover is the agent's store, served exactly, with the replay ending the
// open test (the pool view installed) at one point of a command's match, once:
// right after the command's round is marked (afterMark), right after the
// command is held to it (afterHold), or inside the held store's read of the
// test's answered recordings, before it lists them (inList: listEnder). ahead
// marks the command as one the learn tracker began a test for that the store
// had not opened (integrations.CallRound.TrackerAhead): it reads the view in
// force.
type viewMover struct {
	*proxy.MockManager
	ahead                        bool
	afterMark, afterHold, inList func()
}

func (s *viewMover) MarkCallRound() integrations.CallRound {
	c := s.MockManager.MarkCallRound()
	if s.ahead {
		c = c.WithTrackerAhead(true)
	}
	if f := s.afterMark; f != nil {
		s.afterMark = nil
		f()
	}
	return c
}

func (s *viewMover) HoldToRound(call integrations.CallRound) (integrations.MockMemDb, func() bool) {
	h, restaged := s.MockManager.HoldToRound(call)
	if f := s.afterHold; f != nil {
		s.afterHold = nil
		f()
	}
	if f := s.inList; f != nil {
		s.inList = nil
		return &listEnder{MockMemDb: h, end: f}, restaged
	}
	return h, restaged
}

// listEnder is the store a command is held to, which runs end once inside
// its first read of the test's answered recordings, before the held store
// lists them, and tells whether the command's view moved as the held store
// does. A view installed there is seen only by a question asked after the
// list is read.
type listEnder struct {
	integrations.MockMemDb
	end func()
}

func (s *listEnder) ConsumedTestMocks() []*models.Mock {
	if f := s.end; f != nil {
		s.end = nil
		f()
	}
	return s.MockMemDb.(integrations.ExactConsumedReader).ConsumedTestMocks()
}

func (s *listEnder) CallViewMoved() bool { return integrations.CallViewMoved(s.MockMemDb) }

// Under exact serving a command is read in the view its round was marked in:
// its pool reads are that test's, even once the test ended. A retry of the
// test's call, one more than recorded, finds the test's recording in none of
// them (it answered), and its miss is shown by the places of the recordings
// the test answered in that view too, never those of the view in force when
// the report is built: the pool view, which lists no answered recording. A
// command that reads the view in force (decided in the test the store opens
// next) and finds another view installed since it was held is redacted
// whole: its pool and the test's answered recordings may be of two views.
// Read in the view in force with none installed since, it is shown by its
// view's places. Before, the report read the answered recordings of the view
// in force alone, and showed the key the test's recording redacted.
func TestMySQLAMissReadInATestsViewIsShownByItsAnsweredPlacesAfterTheTestEnds(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name         string
		ahead, moves bool
		hold         bool
		whole        bool
	}{
		{name: "marked in the test, which ends before the command reads its pool", moves: true},
		{name: "held in the test, which ends right after", moves: true, hold: true},
		{name: "reading the view in force, which another view replaced since the hold", ahead: true, moves: true, hold: true, whole: true},
		{name: "reading the view in force, which stays", ahead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replayer.ForgetAnsweredForTest()
			t.Cleanup(replayer.ForgetAnsweredForTest)
			s1, _ := diffWindow(1)
			first, _ := orderMocks(s1.Add(time.Millisecond), s1.Add(2*time.Millisecond))
			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			mm.SetMocksWithWindow(nil, []*models.Mock{first}, models.BaseTime, time.Now().Add(time.Hour))
			seedWindows(mm, 2)
			mm.ServeExactTest("T1", []*models.Mock{first})
			if !mm.ExactServing() {
				t.Fatal("precondition: T1 is not served exactly")
			}
			closeAnswer(t, mm, "T1", orderLiveFirst)
			db := &viewMover{MockManager: mm, ahead: tc.ahead}
			ended := false
			end := func() {
				if mm.ExactTestServed() != "T1" {
					t.Fatalf("precondition: T1 is not the open test inside the match: %q", mm.ExactTestServed())
				}
				mm.ServeExactPool("pool", nil)
				ended = true
			}
			if tc.moves && tc.hold {
				db.afterHold = end
			} else if tc.moves {
				db.afterMark = end
			}
			shown, miss := closeMiss(t, db, "T1 retry", orderLiveFirst)
			if ended != tc.moves || ended && mm.ExactTestServed() != "" {
				t.Fatalf("precondition: T1 ended %v inside the match (test %q); want %v", ended, mm.ExactTestServed(), tc.moves)
			}
			if strings.Contains(shown+miss, closeSecret) {
				t.Fatalf("the retry's miss shows the key T1's recording redacted: %q\nmiss: %s", shown, miss)
			}
			if tc.whole {
				if shown != "(redacted)" {
					t.Fatalf("the retry's miss, its pool and the answered list maybe of two views, is shown %q; want it redacted whole", shown)
				}
				return
			}
			if !strings.Contains(shown, "api_key = '") || !strings.Contains(shown, "'acme'") {
				t.Fatalf("the retry's miss does not show its statement by its view's places, the org no recording redacted as sent: %q", shown)
			}
		})
	}
}

// debugText is every line logs holds, with its string fields.
func debugText(logs *observer.ObservedLogs) string {
	var lines strings.Builder
	for _, e := range logs.All() {
		lines.WriteString(e.Message)
		for k, v := range e.ContextMap() {
			if str, isStr := v.(string); isStr {
				lines.WriteString(" " + k + "=" + str)
			}
		}
		lines.WriteString("\n")
	}
	return lines.String()
}

// Under exact serving a command marked in the pool view matches the
// recording the pool view serves as the next test's in flight (its ahead),
// which redacted the key, while the replay begins T1, owning none of it:
// right after the mark, before the command is held, or right after the
// hold. T1's view refuses the copy the pool view served, and a pool view's
// ahead are the next test's, settled in its view, never in the pool view:
// the command reads the view in force from then on and misses there. Its
// reads were made in two views, and its miss and every debug line show it
// redacted whole, wherever between the mark and the hold T1 began. Before, a
// command held after T1 began was taken as held in the view in force, its
// view never moved: the miss and a debug line showed the key as sent.
func TestMySQLAMissMovedOntoTheViewInForceByARefusedConsumeIsRedactedWhole(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name string
		hold bool
	}{
		{name: "T1 begins between the mark and the hold"},
		{name: "T1 begins right after the hold", hold: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replayer.ForgetAnsweredForTest()
			t.Cleanup(replayer.ForgetAnsweredForTest)
			s1, _ := diffWindow(1)
			first, _ := orderMocks(s1.Add(time.Millisecond), s1.Add(2*time.Millisecond))
			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			mm.SetMocksWithWindow(nil, []*models.Mock{first}, models.BaseTime, time.Now().Add(time.Hour))
			seedWindows(mm, 2)
			var ahead *models.Mock
			for _, mk := range mm.StagedMocks() {
				if mk.Name == first.Name {
					ahead = mk
				}
			}
			if ahead == nil {
				t.Fatal("precondition: the recording is not staged")
			}
			mm.ServeExactPool("pool", nil, ahead)
			if !mm.ExactServing() {
				t.Fatal("precondition: the pool view is not served exactly")
			}
			db := &viewMover{MockManager: mm}
			began := false
			begin := func() {
				mm.ServeExactTest("T1", nil)
				began = true
			}
			if tc.hold {
				db.afterHold = begin
			} else {
				db.afterMark = begin
			}
			var consumes atomic.Int32
			mm.OnConsume(func(name string) {
				if name == first.Name {
					consumes.Add(1)
				}
			})
			t.Cleanup(func() { mm.OnConsume(nil) })
			core, logs := observer.New(zapcore.DebugLevel)
			ok, miss, shown, err := replayer.MatchShownForTest(closeConn, zap.New(core), queryReq(orderLiveFirst), db, closeDctx())
			if !began {
				t.Fatal("precondition: T1 did not begin inside the match")
			}
			if consumes.Load() == 0 {
				t.Fatal("precondition: the command never went to consume the recording the pool view serves; the row is about a refused consume")
			}
			if err != nil || ok || miss == "" {
				t.Fatalf("precondition: the command was not a miss: ok %v, err %v, miss %q", ok, err, miss)
			}
			if strings.Contains(shown+miss, closeSecret) {
				t.Errorf("the miss shows the key the recording redacted: shown %q\nmiss: %s", shown, miss)
			}
			if shown != "(redacted)" {
				t.Errorf("the miss, read in two views, is shown %q; want it redacted whole", shown)
			}
			if text := debugText(logs); strings.Contains(text, closeSecret) {
				t.Errorf("a debug line shows the key the recording redacted:\n%s", text)
			}
		})
	}
}

// Under exact serving a command marked in T1's view matches T1's recording,
// unanswered, while the replay ends T1: right after the mark, before the
// command is held, or right after the hold. Its consume of its own test's
// recording settles in T1's view, as at once, and it is answered.
func TestMySQLACommandOfAnEndedTestIsAnsweredByItsOwnRecording(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, hold := range []bool{false, true} {
		t.Run(fmt.Sprintf("hold=%v", hold), func(t *testing.T) {
			replayer.ForgetAnsweredForTest()
			t.Cleanup(replayer.ForgetAnsweredForTest)
			s1, _ := diffWindow(1)
			first, _ := orderMocks(s1.Add(time.Millisecond), s1.Add(2*time.Millisecond))
			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			mm.SetMocksWithWindow(nil, []*models.Mock{first}, models.BaseTime, time.Now().Add(time.Hour))
			seedWindows(mm, 2)
			mm.ServeExactTest("T1", []*models.Mock{first})
			db := &viewMover{MockManager: mm}
			ended := false
			end := func() {
				mm.ServeExactPool("pool", nil)
				ended = true
			}
			if hold {
				db.afterHold = end
			} else {
				db.afterMark = end
			}
			ok, miss, _, err := replayer.MatchShownForTest(closeConn, zap.NewNop(), queryReq(orderLiveFirst), db, closeDctx())
			if !ended {
				t.Fatal("precondition: T1 did not end inside the match")
			}
			if err != nil || !ok {
				t.Fatalf("T1's late command was not answered by T1's own recording: ok %v, err %v, miss %q", ok, err, miss)
			}
			if test, consumed, _, known := mm.ExactTestOutcome(); !known || test != "T1" || !slices.Equal(consumed, []string{first.Name}) {
				t.Fatalf("T1's outcome is %q, answered %v (%v); want T1 with %s answered", test, consumed, known, first.Name)
			}
		})
	}
}

// Under exact serving a command that reads the view in force (decided in the
// test the store opens next) is redacted whole once the view in force is not
// the one it was held in, whatever the test's answered recordings in the new
// view redacted, and whenever the view moved: right after the hold, to the
// pool view or to T2's, whose answered recording redacted the org, not the
// key; or inside the read of the answered list, after the question whether
// it moved would have been asked first. With no move, the same store shows
// it by the places of T1's answered recording: the key redacted, the org as
// sent.
func TestMySQLAMissReadingTheViewInForceIsRedactedWholeWhereverItMoved(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name         string
		toT2, inList bool
		stays        bool
	}{
		{name: "to T2, whose answered recording redacted another place, right after the hold", toT2: true},
		{name: "to the pool view, inside the read of the answered list", inList: true},
		{name: "to T2, inside the read of the answered list", toT2: true, inList: true},
		{name: "no move, the answered list read through the same store", inList: true, stays: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replayer.ForgetAnsweredForTest()
			t.Cleanup(replayer.ForgetAnsweredForTest)
			s1, _ := diffWindow(1)
			first, second := orderMocks(s1.Add(time.Millisecond), s1.Add(2*time.Millisecond))
			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			mm.SetMocksWithWindow(nil, []*models.Mock{first, second}, models.BaseTime, time.Now().Add(time.Hour))
			seedWindows(mm, 2)
			mm.ServeExactTest("T1", []*models.Mock{first})
			if !mm.ExactServing() {
				t.Fatal("precondition: T1 is not served exactly")
			}
			closeAnswer(t, mm, "T1", orderLiveFirst)
			db := &viewMover{MockManager: mm, ahead: true}
			moved := false
			move := func() {
				if mm.ExactTestServed() != "T1" {
					t.Fatalf("precondition: T1 is not the open test inside the match: %q", mm.ExactTestServed())
				}
				moved = true
				if !tc.toT2 {
					mm.ServeExactPool("pool", nil)
					return
				}
				mm.ServeExactTest("T2", []*models.Mock{second})
				closeAnswer(t, mm, "T2", orderLiveSecond)
			}
			switch {
			case tc.stays:
				db.inList = func() {}
			case tc.inList:
				db.inList = move
			default:
				db.afterHold = move
			}
			shown, miss := closeMiss(t, db, "T1 retry", orderLiveFirst)
			if moved == tc.stays {
				t.Fatalf("precondition: the view moved %v inside the match; want %v", moved, !tc.stays)
			}
			if strings.Contains(shown+miss, closeSecret) {
				t.Fatalf("the miss shows the key T1's recording redacted: %q\nmiss: %s", shown, miss)
			}
			if tc.stays {
				if !strings.Contains(shown, "api_key = '") || !strings.Contains(shown, "'acme'") {
					t.Fatalf("with no move the miss is not shown by T1's answered places, the org no recording redacted as sent: %q", shown)
				}
				return
			}
			if shown != "(redacted)" {
				t.Fatalf("the miss, its pool and the answered list maybe of two views, is shown %q; want it redacted whole", shown)
			}
		})
	}
}
