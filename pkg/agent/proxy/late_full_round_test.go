package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// A claim made through a round scope held to the view a round snapshot read
// (integrations.WithinRoundOf: record's full round's learn step claims so)
// of one of that view's test's recordings, still unanswered, settles in that
// view after the replay ended the test, through the manager and through the
// scoped store a parser is handed alike. Held to a round snapshot another
// manager read, the scope holds to no view of this manager's: the claim is
// refused once the round moved, as WithinRound's is.
func TestAClaimHeldToASnapshotsViewSettlesItsTestsOwnRecordingThere(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer bool
		scope  func(t *testing.T, mm *MockManager, snap integrations.RoundSnapshot) integrations.RoundScope
	}{
		{"the manager", true, func(t *testing.T, mm *MockManager, snap integrations.RoundSnapshot) integrations.RoundScope {
			return integrations.WithinRoundOf(mm, snap)
		}},
		{"a scoped store", true, func(t *testing.T, mm *MockManager, snap integrations.RoundSnapshot) integrations.RoundScope {
			return integrations.WithinRoundOf(&scopedMockDb{MockMemDb: mm}, snap)
		}},
		{"another manager's snapshot", false, func(t *testing.T, mm *MockManager, snap integrations.RoundSnapshot) integrations.RoundScope {
			_, other, _ := lateOwnRun(t, nil)
			theirs, ok := other.RoundSnapshot(integrations.RoundQuery{})
			if !ok || theirs.Test != "test-1" {
				t.Fatalf("precondition: the other manager's snapshot is of %q (%v); want its test-1's view", theirs.Test, ok)
			}
			return mm.WithinRoundOf(theirs)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, mm, own := lateOwnRun(t, nil)
			snap, ok := mm.RoundSnapshot(integrations.RoundQuery{})
			if !ok || snap.Test != "test-1" {
				t.Fatalf("precondition: the round snapshot is of %q (%v); want test-1's view", snap.Test, ok)
			}
			scope := tc.scope(t, mm, snap)
			mm.ServeExactPool(models.PoolKeyNone, nil)
			if got := scope.DeleteUnFilteredMock(*own); got != tc.answer {
				t.Fatalf("a claim of test-1's %s, unanswered, made after the replay ended test-1 settled %v; want %v", own.Name, got, tc.answer)
			}
			if !tc.answer {
				if scope.Moved() == "" {
					t.Fatal("the refused claim says no move; want the round's")
				}
				return
			}
			if test, consumed, _, ok := mm.ExactTestOutcome(); !ok || test != "test-1" || len(consumed) != 1 || consumed[0] != own.Name {
				t.Fatalf("test-1's outcome is %q, answered %v (%v); want %s answered", test, consumed, ok, own.Name)
			}
		})
	}
}
