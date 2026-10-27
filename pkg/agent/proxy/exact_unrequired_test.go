package proxy

import (
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// A recording judging marked as the application's background work is served
// to the test that recorded it, in its window, like its own: the call is
// answered.
func TestABackgroundRecordingIsServedInItsTestsWindow(t *testing.T) {
	own := exactMock("own", 0, models.LifetimeSession)
	bg := exactMock("bg", time.Second, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{own, bg})
	mm.SetUnrequired(models.BackgroundMarks{"T": {"bg": "a timer's call"}})
	mm.ServeExactTest("T", []*models.Mock{own, bg})
	pt, _ := mm.GetPerTestMocksInWindow()
	equalNames(t, "per-test", pt, "own", "bg")
	var served *models.Mock
	for _, mk := range pt {
		if mk.Name == "bg" {
			served = mk
		}
	}
	if !mm.MarkMockAsUsed(*served) {
		t.Fatal("the test's background recording did not answer its call in the test's window")
	}
	test, consumed, unconsumed, ok := mm.ExactTestOutcome()
	if !ok || test != "T" || !slices.Equal(consumed, []string{"bg"}) || !slices.Equal(unconsumed, []string{"own"}) {
		t.Fatalf("outcome %q consumed %v unconsumed %v ok %v", test, consumed, unconsumed, ok)
	}
	if _, unused, ok := mm.ExactTestUnusedBackground(); !ok || len(unused) != 0 {
		t.Fatalf("a background recording that answered is listed unused: %v", unused)
	}
}

// A marked recording the test does not call never fails it: it is not among
// the test's unconsumed recordings, and is listed unused on its own, so
// judging can take it out. An unmarked recording of the same test is still
// required (never a false green).
func TestABackgroundRecordingTheTestDidNotCallIsNotRequired(t *testing.T) {
	own := exactMock("own", 0, models.LifetimeSession)
	bg := exactMock("bg", time.Second, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{own, bg})
	mm.SetUnrequired(models.BackgroundMarks{"T": {"bg": "a timer's call"}})
	mm.ServeExactTest("T", []*models.Mock{own, bg})
	test, _, unconsumed, ok := mm.ExactTestOutcome()
	if !ok || test != "T" || !slices.Equal(unconsumed, []string{"own"}) {
		t.Fatalf("outcome %q unconsumed %v ok %v: only the unmarked recording is required", test, unconsumed, ok)
	}
	bt, unused, ok := mm.ExactTestUnusedBackground()
	if !ok || bt != "T" || !slices.Equal(unused, []string{"bg"}) {
		t.Fatalf("unused background %q %v %v", bt, unused, ok)
	}
}

// Without a mark every test mock is required, as before: a replay that sends
// none, an empty set, or a session reset after one.
func TestWithoutAMarkEveryTestMockIsRequired(t *testing.T) {
	cases := map[string]func(mm *MockManager){
		"none sent": func(*MockManager) {},
		"empty set": func(mm *MockManager) { mm.SetUnrequired(nil) },
		"another recording marked": func(mm *MockManager) {
			mm.SetUnrequired(models.BackgroundMarks{"T": {"elsewhere": "a timer's call"}})
		},
		// The mark is another test's entry of the same recording: T's own
		// entry has none, so T must still make the call.
		"another test's entry marked": func(mm *MockManager) {
			mm.SetUnrequired(models.BackgroundMarks{"A": {"bg": "a timer's call"}})
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			bg := exactMock("bg", 0, models.LifetimeSession)
			mm := stagedManager(t, nil, []*models.Mock{bg})
			setup(mm)
			mm.ServeExactTest("T", []*models.Mock{bg})
			if _, _, unconsumed, _ := mm.ExactTestOutcome(); !slices.Equal(unconsumed, []string{"bg"}) {
				t.Fatalf("an unmarked recording the test did not call is not required: unconsumed %v", unconsumed)
			}
			if _, unused, _ := mm.ExactTestUnusedBackground(); len(unused) != 0 {
				t.Fatalf("an unmarked recording is listed as unused background: %v", unused)
			}
		})
	}
}

// A session reset forgets the marks of the set it staged: mock names repeat
// from set to set, so the next set's recording of the same name is required.
func TestASessionResetForgetsTheMarks(t *testing.T) {
	bg := exactMock("bg", 0, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{bg})
	mm.SetUnrequired(models.BackgroundMarks{"T": {"bg": "a timer's call"}})
	mm.ResetForReplaySession()
	if got := mm.exact.unrequired.Load(); got != nil {
		t.Fatalf("the reset kept the previous set's marks: %v", got)
	}
}

// With exact serving off nothing is said.
func TestUnusedBackgroundNeedsExactServing(t *testing.T) {
	plain := stagedManager(t, nil, []*models.Mock{exactMock("bg", 0, models.LifetimeSession)})
	plain.SetUnrequired(models.BackgroundMarks{"T": {"bg": "a timer's call"}})
	if _, _, ok := plain.ExactTestUnusedBackground(); ok {
		t.Fatal("unused background said with exact serving off")
	}
}

// consumeByName answers a call with the open test's recording name, as a
// parser's consume door does; it fails the test when it cannot.
func consumeByName(t *testing.T, mm *MockManager, name string) {
	t.Helper()
	pt, _ := mm.GetPerTestMocksInWindow()
	for _, mk := range pt {
		if mk.Name == name {
			if !mm.MarkMockAsUsed(*mk) {
				t.Fatalf("%s did not answer", name)
			}
			return
		}
	}
	t.Fatalf("%s is not in the open test's read", name)
}

// A marked recording that answered in its test's window is what a call made
// again is answered with (AnsweredBackground): only marked ones, only once
// they answered, in the order they did, and never a recording another test
// shares (a candidate), which is no recording of the test's own.
func TestAnsweredBackgroundIsTheMarkedRecordingsThatAnswered(t *testing.T) {
	own := exactMock("own", 0, models.LifetimeSession)
	bg1 := exactMock("bg1", time.Second, models.LifetimeSession)
	bg2 := exactMock("bg2", 2*time.Second, models.LifetimeSession)
	shared := exactMock("shared", 3*time.Second, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{own, bg1, bg2, shared})
	mm.SetUnrequired(models.BackgroundMarks{"T": {"bg1": "a timer's call", "bg2": "a timer's call", "shared": "a timer's call"}})
	mm.ServeExactTestConcurrent("T", []*models.Mock{own, bg1, bg2}, []*models.Mock{shared})
	if got := mm.AnsweredBackground(); len(got) != 0 {
		t.Fatalf("a marked recording that has not answered is answered again: %v", got)
	}
	consumeByName(t, mm, "own")
	consumeByName(t, mm, "bg2")
	consumeByName(t, mm, "shared")
	consumeByName(t, mm, "bg1")
	equalNames(t, "answered background", mm.AnsweredBackground(), "bg2", "bg1")
}

// Without a mark, or with no test open, nothing is answered again: an
// unmarked recording's second call is one call more than its test recorded.
func TestAnsweredBackgroundNeedsAMarkAndAnOpenTest(t *testing.T) {
	cases := map[string]func(mm *MockManager){
		"none sent": func(*MockManager) {},
		"another recording marked": func(mm *MockManager) {
			mm.SetUnrequired(models.BackgroundMarks{"T": {"elsewhere": "a timer's call"}})
		},
		"another test's entry marked": func(mm *MockManager) {
			mm.SetUnrequired(models.BackgroundMarks{"A": {"bg": "a timer's call"}})
		},
		"test ended": func(mm *MockManager) {
			mm.SetUnrequired(models.BackgroundMarks{"T": {"bg": "a timer's call"}})
			mm.ServeExactPool(models.PoolKeyNone, nil)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			bg := exactMock("bg", 0, models.LifetimeSession)
			mm := stagedManager(t, nil, []*models.Mock{bg})
			mm.ServeExactTest("T", []*models.Mock{bg})
			consumeByName(t, mm, "bg")
			setup(mm)
			if got := mm.AnsweredBackground(); len(got) != 0 {
				t.Fatalf("answered again: %v", got)
			}
		})
	}
	plain := stagedManager(t, nil, []*models.Mock{exactMock("bg", 0, models.LifetimeSession)})
	plain.SetUnrequired(models.BackgroundMarks{"T": {"bg": "a timer's call"}})
	if got := plain.AnsweredBackground(); got != nil {
		t.Fatalf("answered again with exact serving off: %v", got)
	}
}

// A store held to a call's staging (HoldToRound) answers a call again only
// while the staging is still the call's: the marks and the recordings are
// the staged set's, and recordings' names restart in each set, so a call
// read before the replay staged another set is never answered with that
// set's recording, and its store says it was restaged.
func TestAHeldCallIsAnsweredAgainOnlyInItsOwnStaging(t *testing.T) {
	bg := exactMock("bg", 0, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{bg})
	mm.SetUnrequired(models.BackgroundMarks{"T": {"bg": "a timer's call"}})
	mm.ServeExactTest("T", []*models.Mock{bg})
	consumeByName(t, mm, "bg")
	held, restaged := mm.HoldToRound(integrations.NewCallRound(mm.stagingEpoch.Load(), nil))
	br, ok := held.(integrations.AnsweredBackgroundReader)
	if !ok {
		t.Fatal("the held store offers no answered background recordings")
	}
	equalNames(t, "held, own staging", br.AnsweredBackground(), "bg")
	mm.bumpStagingEpoch()
	if got := br.AnsweredBackground(); len(got) != 0 || !restaged() {
		t.Fatalf("once another set was staged the held call is answered again with %v (restaged %v); want none, restaged", got, restaged())
	}
	equalNames(t, "precondition: the manager, no hold", mm.AnsweredBackground(), "bg")
}

// A view made before exact serving began (scopedMockDb) reads the store's
// answered background recordings, so a parser on it still answers a
// background call made again; one over a store that keeps none reads none.
func TestAScopedViewForwardsTheAnsweredBackground(t *testing.T) {
	bg := exactMock("bg", 0, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{bg})
	mm.SetUnrequired(models.BackgroundMarks{"T": {"bg": "a timer's call"}})
	mm.ServeExactTest("T", []*models.Mock{bg})
	consumeByName(t, mm, "bg")
	var view integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	br, ok := view.(integrations.AnsweredBackgroundReader)
	if !ok {
		t.Fatal("a scoped view offers no answered background recordings")
	}
	equalNames(t, "scoped view", br.AnsweredBackground(), "bg")
	type keepsNone struct{ integrations.MockMemDb }
	var bare integrations.AnsweredBackgroundReader = &scopedMockDb{MockMemDb: keepsNone{mm}}
	if got := bare.AnsweredBackground(); got != nil {
		t.Fatalf("a scoped view over a store that keeps none read %v", got)
	}
}

// In the gap before a test, the store may already serve that test's
// recordings (ServeExactPool's ahead): one of them marked as background work
// that answered there is not answered again until its test's window opens,
// as a call in the gap is no test's.
func TestAMarkedRecordingAnsweredInTheGapIsNotAnsweredAgainThere(t *testing.T) {
	bg := exactMock("bg", 0, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{bg})
	mm.SetUnrequired(models.BackgroundMarks{"T": {"bg": "a timer's call"}})
	mm.ServeExactPool("k", nil, bg)
	consumeByName(t, mm, "bg")
	if got := mm.AnsweredBackground(); len(got) != 0 {
		t.Fatalf("in the gap before its test a marked recording is answered again: %v", got)
	}
}

// MARKS ARE PER TEST: one recording listed by test A, whose entry judging
// marked as background work, and by test B, whose entry has no mark. B must
// still make the call: it is among B's unconsumed recordings and never listed
// as B's unused background; and once B's own call answered, B's second
// identical call is not answered again: it is an extra-call miss, on both
// replay paths (each hands the agent per-test marks).
func TestAnotherTestsMarkLeavesThisTestsEntryRequired(t *testing.T) {
	shared := exactMock("shared", 0, models.LifetimeSession)
	marks := models.BackgroundMarks{"A": {"shared": "count_varied: A"}}

	mm := stagedManager(t, nil, []*models.Mock{shared})
	mm.SetUnrequired(marks)
	mm.ServeExactTest("B", []*models.Mock{shared})
	if _, _, unconsumed, _ := mm.ExactTestOutcome(); !slices.Equal(unconsumed, []string{"shared"}) {
		t.Fatalf("B's unmarked entry is not required (A's mark reached it): unconsumed %v", unconsumed)
	}
	if _, unused, _ := mm.ExactTestUnusedBackground(); len(unused) != 0 {
		t.Fatalf("B's unmarked entry is listed as B's unused background: %v", unused)
	}

	mm = stagedManager(t, nil, []*models.Mock{shared})
	mm.SetUnrequired(marks)
	mm.ServeExactTest("B", []*models.Mock{shared})
	consumeByName(t, mm, "shared")
	if got := mm.AnsweredBackground(); len(got) != 0 {
		t.Fatalf("B's own unmarked recording answers B's extra copy unchecked: %v", got[0].Name)
	}

	// A, whose entry is marked, is served it unrequired and answered again.
	mm = stagedManager(t, nil, []*models.Mock{shared})
	mm.SetUnrequired(marks)
	mm.ServeExactTest("A", []*models.Mock{shared})
	if _, _, unconsumed, _ := mm.ExactTestOutcome(); len(unconsumed) != 0 {
		t.Fatalf("A's marked entry is required: unconsumed %v", unconsumed)
	}
	consumeByName(t, mm, "shared")
	equalNames(t, "A's answered background", mm.AnsweredBackground(), "shared")
}

// A recording another test shares with the open one (a concurrency
// candidate), marked on its owner's entry, is never the open test's unused
// background: the open test does not list it.
func TestACandidateMarkedOnItsOwnerIsNotTheOpenTestsUnusedBackground(t *testing.T) {
	own := exactMock("own", 0, models.LifetimeSession)
	shared := exactMock("shared", time.Second, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{own, shared})
	mm.SetUnrequired(models.BackgroundMarks{"B": {"shared": "a timer's call"}, "T": {"own": "a timer's call"}})
	mm.ServeExactTestConcurrent("T", []*models.Mock{own}, []*models.Mock{shared})
	bt, unused, ok := mm.ExactTestUnusedBackground()
	if !ok || bt != "T" || !slices.Equal(unused, []string{"own"}) {
		t.Fatalf("unused background %q %v %v: only T's own marked entry", bt, unused, ok)
	}
}
