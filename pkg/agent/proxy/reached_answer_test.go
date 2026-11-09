package proxy

import (
	"sort"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// tierNames is the sorted names of the per-test tier.
func tierNames(t *testing.T, mm *MockManager) []string {
	t.Helper()
	got, err := mm.GetFilteredMocks()
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, mk := range got {
		out = append(out, mk.Name)
	}
	sort.Strings(out)
	return out
}

// windowedNames is the sorted names the windowed read serves while the
// window [start, end] is open.
func windowedNames(t *testing.T, mm *MockManager, start, end time.Time) []string {
	t.Helper()
	mm.SetCurrentTestWindow(start, end)
	got, err := mm.GetFilteredMocksInWindow()
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, mk := range got {
		out = append(out, mk.Name)
	}
	sort.Strings(out)
	return out
}

// A consumer reads a message in test-1, and its next poll, which the recording
// answered with the message's second copy, was answered in the gap after
// test-1's response, before test-2 began: placed at its answer, it belongs to
// no test. With the proven switch off it is reached from test-1, the test
// before the gap, and stays reachable in the later windows until a consume
// takes it; a poll answered in test-2 is not reached in test-1. The agent
// hands each window only its own load and what it reaches past it
// (ReachedAnswerLoadRange): the untaken ones stay from the closing window's
// tier. Under the switch the exact serving's worker scope serves them, and
// the staging is as before.
func TestWithTheSwitchOffAPollAnsweredBetweenTwoTestsIsReachedFromTheTestBeforeTheGapUntilTaken(t *testing.T) {
	registerAnswerPlacedPolls(t)
	stage := func(t *testing.T) *MockManager {
		mm := NewMockManager(nil, nil, zap.NewNop())
		t.Cleanup(mm.Close)
		mm.ResetForReplaySession()
		mm.SeedStartupCutoff(winStart(1))
		mm.SeedRecordedWindows(windowsOf("test-1", "test-2", "test-3"))
		mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
		return mm
	}
	// test-1 = [1000, 1100], test-2 = [2000, 2100], test-3 = [3000, 3100].
	own1 := inFlightMock("own-1", 1010, 1011)
	poll1 := answerPlacedPoll("poll-1", 900, 1060)      // answered in test-1
	gapPoll := answerPlacedPoll("gap-poll", 1090, 1150) // answered in the gap after test-1
	poll2 := answerPlacedPoll("poll-2", 1160, 2050)     // answered in test-2
	lateGap := answerPlacedPoll("late-gap", 2060, 2500) // answered in the gap after test-2
	gapCall := inFlightMock("gap-call", 1120, 1130)     // a call made in the gap, not a poll
	t.Run("switch off", func(t *testing.T) {
		mm := stage(t)
		mm.SetMocksWithWindow([]*models.Mock{own1, poll1, gapPoll, gapCall}, nil, winStart(1), winEnd(1))
		want := []string{"gap-poll", "own-1", "poll-1"}
		if got := tierNames(t, mm); !equalStrings(got, want) {
			t.Fatalf("test-1's per-test tier holds %v; want %v: the poll answered in the gap after it is reached, the call made there is not", got, want)
		}
		if got := windowedNames(t, mm, winStart(1), winEnd(1)); !equalStrings(got, want) {
			t.Fatalf("test-1's windowed read serves %v; want %v", got, want)
		}
		if mm.OwnedByOpenTest(gapPoll) {
			t.Error("test-1 owns the poll answered in the gap after it; want no test to")
		}
		// Read with test-2's window over test-1's staging, the windowed read
		// keeps the polls reached and untaken, not test-1's own call.
		if got := windowedNames(t, mm, winStart(2), winEnd(2)); !equalStrings(got, []string{"gap-poll", "poll-1"}) {
			t.Fatalf("test-1's tier read in test-2's window serves %v; want the two polls only", got)
		}
		if !mm.DeleteFilteredMock(*poll1) {
			t.Fatal("poll-1's consume was refused")
		}
		// test-2: the agent hands its own load only.
		mm.SetMocksWithWindow([]*models.Mock{poll2, lateGap}, nil, winStart(2), winEnd(2))
		want = []string{"gap-poll", "late-gap", "poll-2"}
		if got := tierNames(t, mm); !equalStrings(got, want) {
			t.Fatalf("test-2's per-test tier holds %v; want %v: the gap poll test-1 left untaken stays, the consumed one does not", got, want)
		}
		if got := windowedNames(t, mm, winStart(2), winEnd(2)); !equalStrings(got, want) {
			t.Fatalf("test-2's windowed read serves %v; want %v", got, want)
		}
		// Read with test-1's window, test-2's staging serves only what test-1
		// reached: never a poll before its place.
		if got := windowedNames(t, mm, winStart(1), winEnd(1)); !equalStrings(got, []string{"gap-poll"}) {
			t.Fatalf("test-2's tier read in test-1's window serves %v; want the gap poll only", got)
		}
		if !mm.DeleteFilteredMock(*gapPoll) {
			t.Fatal("the gap poll's consume in test-2 was refused")
		}
		mm.SetMocksWithWindow(nil, nil, winStart(3), winEnd(3))
		want = []string{"late-gap", "poll-2"}
		if got := tierNames(t, mm); !equalStrings(got, want) {
			t.Fatalf("test-3's per-test tier holds %v; want %v: a consume took the gap poll", got, want)
		}
		// A pass that goes back to test-1 has not reached what was answered
		// after test-1's gap.
		mm.SetMocksWithWindow(nil, nil, winStart(1), winEnd(1))
		if got := tierNames(t, mm); len(got) != 0 {
			t.Fatalf("test-1's per-test tier, gone back to, holds %v; want none of the later places", got)
		}
	})
	t.Run("switch on", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		mm := stage(t)
		mm.SetMocksWithWindow([]*models.Mock{own1, poll1, gapPoll}, nil, winStart(1), winEnd(1))
		if got := tierNames(t, mm); !equalStrings(got, []string{"own-1", "poll-1"}) {
			t.Fatalf("under the switch test-1's per-test tier holds %v; want its own two, as before", got)
		}
		if got := windowedNames(t, mm, winStart(2), winEnd(2)); len(got) != 0 {
			t.Fatalf("under the switch test-1's tier read in test-2's window serves %v; want none, as before", got)
		}
		mm.SetMocksWithWindow([]*models.Mock{poll2}, nil, winStart(2), winEnd(2))
		if got := tierNames(t, mm); !equalStrings(got, []string{"poll-2"}) {
			t.Fatalf("under the switch test-2's per-test tier holds %v; want its own, as before", got)
		}
		if _, _, ok := mm.ReachedAnswerLoadRange(winStart(2)); ok {
			t.Error("under the switch the agent is asked to load the reached polls")
		}
	})
}

// The agent loads the polls a window reaches past its own once in a pass:
// from the set's first window up to the window's release horizon (the start
// of the next window, less a nanosecond), then from where the previous load
// ended, from the first window again when a pass goes back, and to no bound
// at the last window. A staging call loads none, nor does a set with no
// recorded windows or no kind placed at its answer.
func TestReachedAnswerLoadRangeLoadsEachReachedPollOnceInAPass(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.ResetForReplaySession()
	mm.SeedStartupCutoff(winStart(1))
	mm.SeedRecordedWindows(windowsOf("test-1", "test-2", "test-3"))
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if _, _, ok := mm.ReachedAnswerLoadRange(winStart(1)); ok {
		t.Fatal("a range with no kind placed at its answer")
	}
	registerAnswerPlacedPolls(t)
	check := func(start time.Time, wantFrom, wantTo time.Time) {
		t.Helper()
		from, to, ok := mm.ReachedAnswerLoadRange(start)
		if !ok || !from.Equal(wantFrom) || !to.Equal(wantTo) {
			t.Fatalf("the range at %v is [%v, %v] ok=%v; want [%v, %v]", start, from, to, ok, wantFrom, wantTo)
		}
	}
	if _, _, ok := mm.ReachedAnswerLoadRange(models.BaseTime); ok {
		t.Fatal("a staging call is asked to load the reached polls")
	}
	ns := time.Nanosecond
	check(winStart(1), winStart(1), winStart(2).Add(-ns))
	mm.SetMocksWithWindow(nil, nil, winStart(1), winEnd(1))
	check(winStart(2), winStart(2), winStart(3).Add(-ns))
	mm.SetMocksWithWindow(nil, nil, winStart(2), winEnd(2))
	check(winStart(3), winStart(3), maxRecordedTime)
	check(winStart(1), winStart(1), winStart(2).Add(-ns)) // gone back
	mm.SetMocksWithWindow(nil, nil, winStart(3), winEnd(3))
	if _, _, ok := mm.ReachedAnswerLoadRange(winStart(3)); ok {
		t.Error("the last window loads past its unbounded horizon again")
	}

	// Recorded windows none of which has a time make no schedule to reach
	// by: a window's untaken poll is not kept past it, as before.
	untimed := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(untimed.Close)
	untimed.ResetForReplaySession()
	untimed.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-1"}, {TestCase: "test-2"}})
	untimed.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	untimed.SetMocksWithWindow([]*models.Mock{answerPlacedPoll("poll-1", 900, 1060)}, nil, winStart(1), winEnd(1))
	if got := windowedNames(t, untimed, winStart(2), winEnd(2)); len(got) != 0 {
		t.Fatalf("with no timed recorded window test-1's tier read in test-2's window serves %v; want none", got)
	}
	untimed.SetMocksWithWindow(nil, nil, winStart(2), winEnd(2))
	if got := tierNames(t, untimed); len(got) != 0 {
		t.Fatalf("with no timed recorded window test-2's tier holds %v; want none", got)
	}
	if _, _, ok := untimed.ReachedAnswerLoadRange(winStart(2)); ok {
		t.Error("a set with no timed recorded window is asked to load the reached polls")
	}

	none := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(none.Close)
	none.ResetForReplaySession()
	if _, _, ok := none.ReachedAnswerLoadRange(winStart(1)); ok {
		t.Error("a set with no recorded windows is asked to load the reached polls")
	}
}
