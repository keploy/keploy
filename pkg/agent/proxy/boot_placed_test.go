package proxy

import (
	"sort"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// bootRecording is a set whose first recorded test begins at winStart(1): a
// start-up call, a test's call, and polls placed at their answer (a worker's
// blocking pop) answered at start-up, exactly as the first test began, inside
// test-1 and inside test-2.
func bootRecording() []*models.Mock {
	return []*models.Mock{
		inFlightMock("boot-call", 300, 310),
		inFlightMock("own-2", 2010, 2011),
		answerPlacedPoll("boot-answered", 300, 400),
		answerPlacedPoll("answered-at-first-start", 500, 1000),
		answerPlacedPoll("answered-in-1", 500, 1050),
		answerPlacedPoll("boot-poll", 500, 2060), // sent at start-up, answered in test-2
	}
}

func startupNames(t *testing.T, mm *MockManager) []string {
	t.Helper()
	got, err := mm.GetStartupMocks()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, mk := range got {
		out = append(out, mk.Name)
	}
	sort.Strings(out)
	return out
}

// A models.BaseTime staging, the one before any test and the whole pool
// restored between tests, keeps a poll placed at its answer out of the
// start-up tier unless it was answered before the set's first recorded test
// began: the start-up tier would otherwise answer a worker's start-up pop with
// the job a later test queued. Every other mock stays in it, a test's own
// included, as before. The cutoff is the earlier of the seeded start-up cutoff
// and the first seeded recorded window, whichever the replayer sent.
func TestABaseTimeStagingKeepsAPollAnsweredInATestOutOfTheStartUpTier(t *testing.T) {
	registerAnswerPlacedPolls(t)
	want := []string{"boot-answered", "boot-call", "own-2"}
	for _, tc := range []struct {
		name string
		seed func(mm *MockManager)
	}{
		{"cutoff and windows", func(mm *MockManager) {
			mm.SeedStartupCutoff(winStart(1))
			mm.SeedRecordedWindows(windowsOf("test-1", "test-2"))
		}},
		{"cutoff alone", func(mm *MockManager) { mm.SeedStartupCutoff(winStart(1)) }},
		{"one window alone", func(mm *MockManager) { mm.SeedRecordedWindows(windowsOf("test-1")) }},
		{"a window earlier than the cutoff", func(mm *MockManager) {
			mm.SeedStartupCutoff(winStart(2))
			mm.SeedRecordedWindows(windowsOf("test-1", "test-2"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eachSwitchState(t, func(t *testing.T) {
				mm := NewMockManager(nil, nil, zap.NewNop())
				t.Cleanup(mm.Close)
				mm.ResetForReplaySession()
				tc.seed(mm)
				mm.SetMocksWithWindow(bootRecording(), nil, models.BaseTime, time.Now())
				if got := startupNames(t, mm); !equalStrings(got, want) {
					t.Fatalf("before any test the start-up tier holds %v; want %v", got, want)
				}
				mm.SetMocksWithWindow(bootRecording(), nil, winStart(1), winEnd(1))
				// Between tests: a mid-set staging from models.BaseTime.
				mm.SetMocksWithWindow(bootRecording(), nil, models.BaseTime, time.Now())
				if got := startupNames(t, mm); !equalStrings(got, want) {
					t.Fatalf("between tests the start-up tier holds %v; want %v", got, want)
				}
			})
		})
	}
}

// keploy mock replay with the switch off seeds no recorded window and no
// cutoff: nothing tells a poll answered at start-up from one answered in a
// test, so every poll placed at its answer is left out of the start-up tier,
// before any test and in the pool restored between tests (SetScopeMocks), and
// its parser holds the call until a test's own mocks answer it. Every other
// mock stays in it.
func TestABaseTimeStagingWithNoRecordedWindowLeavesOutEveryPollPlacedAtItsAnswer(t *testing.T) {
	registerAnswerPlacedPolls(t)
	eachSwitchState(t, func(t *testing.T) {
		mm := NewMockManager(nil, nil, zap.NewNop())
		t.Cleanup(mm.Close)
		mm.ResetForReplaySession()
		want := []string{"boot-call", "own-2"}
		mm.SetMocksWithWindow(bootRecording(), nil, models.BaseTime, time.Now())
		if got := startupNames(t, mm); !equalStrings(got, want) {
			t.Fatalf("before any test the start-up tier holds %v; want %v", got, want)
		}
		own := []*models.Mock{answerPlacedPoll("boot-poll", 500, 2060), inFlightMock("own-2", 2010, 2011)}
		mm.SetScopeMocks(own, nil, true)
		if got, _ := mm.GetFilteredMocks(); len(got) != 2 {
			t.Fatalf("the open test's per-test tier holds %d mocks; want its 2", len(got))
		}
		mm.SetScopeMocks(bootRecording(), nil, false)
		if got := startupNames(t, mm); !equalStrings(got, want) {
			t.Fatalf("between tests the start-up tier holds %v; want %v", got, want)
		}
	})
}

// namesSorted is the names of ms, sorted.
func namesSorted(ms []*models.Mock) []string {
	var out []string
	for _, mk := range ms {
		out = append(out, mk.Name)
	}
	sort.Strings(out)
	return out
}

// The start-up tier stores a held poll (one placed at its answer and answered
// in a test) and serves it to no read without exact serving: the start-up
// reads by kind, the session union and its keyed walk, and HasMocksByKind
// all leave it out alike. Every reader of the staged set still finds it: it
// is a recording that has answered nothing (HasUnservedRecording), which is
// what a parser holding the call asks. A poll handed in the session slice,
// which the staging files in both tiers, stays out of the start-up tier when a
// match files an updated copy over it in the session tier (mirrorStartup).
func TestEveryStartUpReadLeavesOutAHeldPollThatTheStagedSetStillHolds(t *testing.T) {
	registerAnswerPlacedPolls(t)
	eachSwitchState(t, func(t *testing.T) {
		mm := NewMockManager(nil, nil, zap.NewNop())
		t.Cleanup(mm.Close)
		mm.ResetForReplaySession()
		mm.SeedStartupCutoff(winStart(1))
		// A poll handed in the session slice: the staging files it in the
		// session tier and the start-up tier both.
		mm.SetMocksWithWindow(bootRecording(), []*models.Mock{answerPlacedPoll("session-poll", 500, 2060)}, models.BaseTime, time.Now())
		served := []string{"boot-answered", "boot-call", "own-2"}
		byKind, err := mm.GetStartupMocksByKind(models.HTTP)
		if err != nil {
			t.Fatal(err)
		}
		if got := namesSorted(byKind); !equalStrings(got, served) {
			t.Fatalf("the start-up tier of the kind serves %v; want %v", got, served)
		}
		// The session union lists the session tier's copy of the session poll
		// alone: the session tier serves it as it always did.
		union := append(append([]string(nil), served...), "session-poll")
		sort.Strings(union)
		session, err := mm.GetSessionMocks()
		if err != nil {
			t.Fatal(err)
		}
		if got := namesSorted(session); !equalStrings(got, union) {
			t.Fatalf("the session union serves %v; want %v", got, union)
		}
		var walked []*models.Mock
		ix := &integrations.MockIndex{Keys: func(*models.Mock) []string { return []string{"k"} }}
		if err := mm.RangeSessionMocksWithKey(ix, "k", func(mk *models.Mock) bool {
			walked = append(walked, mk)
			return true
		}); err != nil {
			t.Fatal(err)
		}
		if got := namesSorted(walked); !equalStrings(got, union) {
			t.Fatalf("the session union's keyed walk serves %v; want %v", got, union)
		}
		named := func(name string) func(*models.Mock) bool {
			return func(mk *models.Mock) bool { return mk.Name == name }
		}
		if !mm.HasMocksByKind(models.HTTP, named("boot-call")) {
			t.Fatal("HasMocksByKind does not find the start-up call the tier serves")
		}
		for _, held := range []string{"answered-at-first-start", "answered-in-1", "boot-poll"} {
			if mm.HasMocksByKind(models.HTTP, named(held)) {
				t.Errorf("HasMocksByKind finds %s, which the start-up tier holds and does not serve", held)
			}
			found, err := mm.HasUnservedRecording(models.HTTP, named(held))
			if err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Errorf("the staged set no longer holds %s: a parser holding its call is told no recording of it remains", held)
			}
		}
		var p *models.Mock
		unf, _ := mm.GetUnFilteredMocks()
		for _, mk := range unf {
			if mk.Name == "session-poll" {
				p = mk
			}
		}
		if p == nil {
			t.Fatal("the session tier does not serve the session poll")
		}
		c := *p
		c.TestModeInfo.SortOrder += 100
		if !mm.UpdateUnFilteredMock(p, &c) {
			t.Fatal("the update of the session poll did not land")
		}
		if got := startupNames(t, mm); !equalStrings(got, served) {
			t.Fatalf("after a match filed an updated copy of the session poll the start-up tier serves %v; want %v", got, served)
		}
	})
}
