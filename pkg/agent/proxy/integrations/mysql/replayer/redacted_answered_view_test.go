package replayer_test

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// viewMover is the agent's store, served exactly, with the replay ending the
// open test (the pool view installed) at one point of a command's match, once:
// right after the command's round is marked (afterMark), or right after the
// command is held to it (afterHold). ahead marks the command as one the learn
// tracker began a test for that the store had not opened
// (integrations.CallRound.TrackerAhead): it reads the view in force.
type viewMover struct {
	*proxy.MockManager
	ahead                bool
	afterMark, afterHold func()
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
	return h, restaged
}

// Under exact serving a command is read in the view its round was marked in:
// its pool reads are that test's, even once the test ended. A retry of the
// test's call, one more than recorded, finds the test's recording in none of
// them (it answered), and its miss is shown by the places of the recordings
// the test answered in that view too, never those of the view in force when
// the report is built: the pool view, which lists no answered recording. A
// command that reads the view in force (decided in the test the store opens
// next) and finds another view installed since it was held is redacted
// whole: its pool and the test's answered recordings may be of two views. Read in the view in force with none installed since, it is
// shown by its view's places. Before, the report read the answered
// recordings of the view in force alone, and showed the key the test's
// recording redacted.
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
