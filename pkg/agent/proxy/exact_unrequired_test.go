package proxy

import (
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A recording judging marked as the application's background work is served
// to the test that recorded it, in its window, like its own: the call is
// answered.
func TestABackgroundRecordingIsServedInItsTestsWindow(t *testing.T) {
	own := exactMock("own", 0, models.LifetimeSession)
	bg := exactMock("bg", time.Second, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{own, bg})
	mm.SetUnrequired(map[string]string{"bg": "a timer's call"})
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
	mm.SetUnrequired(map[string]string{"bg": "a timer's call"})
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
			mm.SetUnrequired(map[string]string{"elsewhere": "a timer's call"})
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
	mm.SetUnrequired(map[string]string{"bg": "a timer's call"})
	mm.ResetForReplaySession()
	if got := mm.unrequiredNames(); got != nil {
		t.Fatalf("the reset kept the previous set's marks: %v", got)
	}
}

// With exact serving off nothing is said.
func TestUnusedBackgroundNeedsExactServing(t *testing.T) {
	plain := stagedManager(t, nil, []*models.Mock{exactMock("bg", 0, models.LifetimeSession)})
	plain.SetUnrequired(map[string]string{"bg": "a timer's call"})
	if _, _, ok := plain.ExactTestUnusedBackground(); ok {
		t.Fatal("unused background said with exact serving off")
	}
}
