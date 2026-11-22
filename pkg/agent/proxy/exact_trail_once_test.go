package proxy

import (
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// trailPolls is a staged manager whose worker scope holds tr, a trailing
// write of TestA (the replay's poolTrails names it), and w, an ordinary
// worker: recording, both answering the same request.
func trailPolls(t *testing.T) (*MockManager, map[string]*models.Mock) {
	t.Helper()
	mks := []*models.Mock{httpCall("tr", time.Second, "/delete"), httpCall("w", 2*time.Second, "/poll")}
	byName := map[string]*models.Mock{}
	for _, mk := range mks {
		byName[mk.Name] = mk
	}
	mm := stagedManager(t, nil, mks)
	mm.ServeExactWorker([]string{"tr", "w"})
	mm.SetPoolTrails(map[string]string{"tr": "TestA"})
	return mm, byName
}

// answer is one call's answer through the session tier and a door that keeps
// the recording (MarkMockAsUsed): the first of name's recordings the reads
// show it is served, or "a miss".
func answer(mm *MockManager, byName map[string]*models.Mock, name string) string {
	mocks, err := mm.GetSessionScopedMocks()
	if err != nil {
		return "a miss"
	}
	if !slices.ContainsFunc(mocks, func(mk *models.Mock) bool { return mk.Name == name }) {
		return "a miss"
	}
	if !mm.MarkMockAsUsed(*byName[name]) {
		return "a miss"
	}
	return name
}

// A recording a trail move put in the worker scope answers once in the
// staging, wherever its call lands: twice between tests, the second call is
// not answered from it (the reads no longer show it, and a consume of it is
// refused); used in a test's window, a call between tests after is not
// answered from it either. An ordinary worker: recording answers again
// between tests, as before.
func TestATrailingWriteAnswersOnceInTheStaging(t *testing.T) {
	t.Run("twice between tests", func(t *testing.T) {
		mm, byName := trailPolls(t)
		mm.ServeExactPool("k", nil)
		got := []string{answer(mm, byName, "tr"), answer(mm, byName, "tr"), answer(mm, byName, "w"), answer(mm, byName, "w")}
		if want := []string{"tr", "a miss", "w", "w"}; !slices.Equal(got, want) {
			t.Fatalf("answers %v, want %v", got, want)
		}
		if mm.MarkMockAsUsed(*byName["tr"]) {
			t.Fatal("a second consume of the trailing write between tests was served")
		}
	})
	t.Run("in a test's window, then between tests", func(t *testing.T) {
		mm, byName := trailPolls(t)
		mm.ServeExactTest("TestB", nil)
		if got := answer(mm, byName, "tr"); got != "tr" {
			t.Fatalf("the trailing write in TestB's window was answered %q, want tr", got)
		}
		if got := answer(mm, byName, "tr"); got != "a miss" {
			t.Fatalf("a second call in the window was answered %q, want a miss", got)
		}
		mm.ServeExactPool("k", nil)
		if got := answer(mm, byName, "tr"); got != "a miss" {
			t.Fatalf("a call between tests after the window used it was answered %q, want a miss", got)
		}
		if got := answer(mm, byName, "w"); got != "w" {
			t.Fatalf("an ordinary worker recording between tests was answered %q, want w", got)
		}
	})
	t.Run("between tests, then in a test's window", func(t *testing.T) {
		mm, byName := trailPolls(t)
		mm.ServeExactPool("k", nil)
		if got := answer(mm, byName, "tr"); got != "tr" {
			t.Fatalf("the trailing write between tests was answered %q, want tr", got)
		}
		mm.ServeExactTest("TestB", nil)
		if got := answer(mm, byName, "tr"); got != "a miss" {
			t.Fatalf("a call in a window after a call between tests used it was answered %q, want a miss", got)
		}
		if got := answer(mm, byName, "w"); got != "w" {
			t.Fatalf("an ordinary worker recording in the window was answered %q, want w", got)
		}
	})
	t.Run("not a trail: answers again between tests", func(t *testing.T) {
		mm, byName := trailPolls(t)
		mm.SetPoolTrails(nil)
		mm.ServeExactPool("k", nil)
		got := []string{answer(mm, byName, "tr"), answer(mm, byName, "tr")}
		if want := []string{"tr", "tr"}; !slices.Equal(got, want) {
			t.Fatalf("answers %v, want %v", got, want)
		}
	})
}

// The consume itself is the guard a read cannot be (two calls may read a
// trailing write before either consumes it): a second consume of one in the
// staging is refused, in a window or between tests, whichever came first,
// and an ordinary worker: recording's is served between tests again. A test
// process that claims its key again (RestartExactKey) is a new run of its
// tests: the trailing write answers again, once. A manager no replay sent
// trails to has none.
func TestASecondConsumeOfATrailingWriteIsRefused(t *testing.T) {
	for _, first := range []string{"between", "in a window"} {
		t.Run(first, func(t *testing.T) {
			mm, byName := trailPolls(t)
			mm.prepareOutside()
			consume := map[string]func(*models.Mock) exactVerdict{
				"between":     func(mk *models.Mock) exactVerdict { v, _ := mm.consumeBetween(mk); return v },
				"in a window": func(mk *models.Mock) exactVerdict { return mm.consumeOutside(mk, true) },
			}
			then := map[string]string{"between": "in a window", "in a window": "between"}[first]
			if v := consume[first](byName["tr"]); v != exactServed {
				t.Fatalf("the first consume %s: %v", first, v)
			}
			if v := consume[then](byName["tr"]); v != exactRefused {
				t.Fatalf("a second consume %s after one %s: %v, want refused", then, first, v)
			}
			if v := consume[first](byName["tr"]); v != exactRefused {
				t.Fatalf("a second consume %s: %v, want refused", first, v)
			}
			if v, _ := mm.consumeBetween(byName["w"]); v != exactServed {
				t.Fatalf("an ordinary worker recording between tests: %v", v)
			}
			if v, _ := mm.consumeBetween(byName["w"]); v != exactServed {
				t.Fatalf("an ordinary worker recording between tests again: %v", v)
			}
			mm.RestartExactKey("k", []string{"tr"})
			mm.prepareOutside()
			if v := consume[then](byName["tr"]); v != exactServed {
				t.Fatalf("after the key was claimed again: %v, want served", v)
			}
		})
	}
	t.Run("no trails sent", func(t *testing.T) {
		mks := []*models.Mock{httpCall("tr", time.Second, "/delete")}
		mm := stagedManager(t, nil, mks)
		mm.ServeExactWorker([]string{"tr"})
		mm.prepareOutside()
		for i := 0; i < 2; i++ {
			if v, _ := mm.consumeBetween(mks[0]); v != exactServed {
				t.Fatalf("consume %d between tests with no trails sent: %v", i, v)
			}
		}
		mm.ServeExactPool("k", nil)
		if mocks, _ := mm.GetSessionScopedMocks(); !slices.ContainsFunc(mocks, func(mk *models.Mock) bool { return mk.Name == "tr" }) {
			t.Fatal("with no trails sent the reads between tests hide a worker recording a call used")
		}
	})
	t.Run("the set staged again: the trailing write answers again", func(t *testing.T) {
		mm, byName := trailPolls(t)
		mm.ServeExactPool("k", nil)
		if got := answer(mm, byName, "tr"); got != "tr" {
			t.Fatalf("first answer %q", got)
		}
		// A new staging of the set, the same view in force, before any
		// door builds its outside answers: what the old staging used is
		// not this staging's.
		mm.stagingEpoch.Add(1)
		if mocks, _ := mm.GetSessionScopedMocks(); !slices.ContainsFunc(mocks, func(mk *models.Mock) bool { return mk.Name == "tr" }) {
			t.Fatal("the reads of a new staging hide a trailing write the staging before it used")
		}
	})
	t.Run("the reads hide a used trailing write", func(t *testing.T) {
		mm, byName := trailPolls(t)
		mm.ServeExactPool("k", nil)
		if got := answer(mm, byName, "tr"); got != "tr" {
			t.Fatalf("first answer %q", got)
		}
		if mocks, _ := mm.GetSessionScopedMocks(); slices.ContainsFunc(mocks, func(mk *models.Mock) bool { return mk.Name == "tr" }) {
			t.Fatal("the reads between tests still show a trailing write a call used")
		}
	})
}
