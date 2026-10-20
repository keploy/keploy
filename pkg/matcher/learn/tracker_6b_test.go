package learn

import (
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func beforeNames(t *Tracker) []string {
	_, _, _, before := t.Test()
	var out []string
	for _, m := range before {
		out = append(out, m.Name)
	}
	return out
}

// A mock served while no test is open (the start-up, between tests) is an
// earlier place of every later test, once; one served in a test is one of
// the next test only, as before.
func TestTrackerKeepsWhatWasServedOutsideATestForEveryLaterTest(t *testing.T) {
	tr := NewTracker()
	tr.ServedMock(&models.Mock{Name: "startup"})
	tr.Begin() // test 1
	if got := beforeNames(tr); !reflect.DeepEqual(got, []string{"startup"}) {
		t.Fatalf("test 1: before %v", got)
	}
	tr.ServedMock(&models.Mock{Name: "own-1"})
	tr.End()
	tr.ServedMock(&models.Mock{Name: "between"})
	tr.ServedMock(&models.Mock{Name: "startup"}) // served again
	tr.Begin()                                   // test 2
	if got := beforeNames(tr); !reflect.DeepEqual(got, []string{"own-1", "between", "startup"}) {
		t.Fatalf("test 2: before %v", got)
	}
	tr.ServedMock(&models.Mock{Name: "own-2"})
	tr.End()
	tr.Begin() // test 3
	if got := beforeNames(tr); !reflect.DeepEqual(got, []string{"own-2", "startup", "between"}) {
		t.Fatalf("test 3: before %v, want test 2's own and everything served outside a test", got)
	}
}

// A new test set forgets what the previous set served: its mocks reuse the
// previous set's names (mock-0 again), and none of the previous set's is an
// earlier place of the new set's tests. Without NewSet, a mock is still kept
// by its identity, not its name: another set's mock-0 never hides this one's.
func TestTrackerTellsTestSetsApart(t *testing.T) {
	at := func(s int) time.Time { return time.Date(2026, 10, 16, s, 0, 0, 0, time.UTC) }
	set1 := &models.Mock{Name: "mock-0", Spec: models.MockSpec{ReqTimestampMock: at(1)}}
	set2 := &models.Mock{Name: "mock-0", Spec: models.MockSpec{ReqTimestampMock: at(2)}}
	for _, newSet := range []bool{true, false} {
		tr := NewTracker()
		tr.ServedMock(set1)
		tr.Begin()
		tr.End()
		if newSet {
			tr.NewSet()
		}
		tr.ServedMock(set2)
		tr.Begin()
		tr.End()
		tr.Begin() // set 2, test 2
		_, _, _, before := tr.Test()
		var has1, has2 bool
		for _, m := range before {
			has1, has2 = has1 || m == set1, has2 || m == set2
		}
		if !has2 || has1 == newSet {
			t.Errorf("NewSet %v: set 2's test 2 has set 1's mock-0 %v, set 2's %v", newSet, has1, has2)
		}
	}
}

// The start-up's calls share one drift cache, so each earlier recording's
// text is read once there; a test, and a new set, start afresh.
func TestTrackerKeepsADriftCacheForTheStartUp(t *testing.T) {
	tr := NewTracker()
	startUp := tr.Drifts()
	if startUp == nil || tr.Drifts() != startUp {
		t.Fatal("the start-up's calls share no drift cache")
	}
	tr.Begin()
	test := tr.Drifts()
	if test == startUp {
		t.Fatal("the first test read the start-up's cache")
	}
	tr.End()
	tr.NewSet()
	if d := tr.Drifts(); d == nil || d == startUp || d == test {
		t.Fatal("a new set's start-up has no cache of its own")
	}
}
