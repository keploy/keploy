package learn

import (
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// previousCallSet is paired, sent at its recorded instant, and the set a
// learn step walks to find its previous call: n recordings sent (and
// answered) before it, one a millisecond apart, the latest last, and n sent
// after it, their answer times not known.
func previousCallSet(n int) (paired *models.Mock, before, after []*models.Mock) {
	at := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	paired = previousCallRec("mock-paired", at, at.Add(100*time.Microsecond))
	for i := n; i > 0; i-- {
		req := at.Add(-time.Duration(i) * time.Millisecond)
		before = append(before, previousCallRec(fmt.Sprintf("mock-b%d", i), req, req.Add(100*time.Microsecond)))
		after = append(after, previousCallRec(fmt.Sprintf("mock-a%d", i), at.Add(time.Duration(i)*time.Millisecond), time.Time{}))
	}
	return paired, before, after
}

func previousCallRec(name string, req, res time.Time) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.Mongo}
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = req, res
	return m
}

// A learn step shows the previous-call search every recording of the set,
// for every call it tries (PreviousCall.See). Each recording costs it a few
// comparisons and no allocation, at any set size, and the walk still finds
// the latest call sent before paired, past the recordings sent later than
// it that are none: paired itself, a call sent at its very instant, one
// sent before it and answered after it was sent, a protocol phase, and a
// sibling. It used to build two strings per recording (MockID) to tell
// paired from the rest.
func TestPreviousCallSeesARecordingAllocatingNothing(t *testing.T) {
	for _, n := range []int{10, 1000} {
		paired, before, after := previousCallSet(n)
		at := paired.Spec.ReqTimestampMock
		hello := previousCallRec("mock-hello", at.Add(-200*time.Microsecond), at.Add(-150*time.Microsecond))
		hello.Spec.Metadata = map[string]string{"type": "config"}
		none := []*models.Mock{
			paired,
			previousCallRec("mock-same", at, at),
			previousCallRec("mock-pending", at.Add(-500*time.Microsecond), at.Add(time.Millisecond)),
			hello,
			previousCallRec("mock-sib", at.Add(-300*time.Microsecond), at.Add(-250*time.Microsecond)),
		}
		set := append(append(append([]*models.Mock(nil), before...), none...), after...)
		p := NewPreviousCallOf(paired, func(m *models.Mock) bool { return m.Name == "mock-sib" })
		allocs := testing.AllocsPerRun(5, func() {
			p.at = time.Time{}
			for _, m := range set {
				p.See(m)
			}
		})
		if allocs != 0 {
			t.Errorf("set of %d: %v allocations a walk of the set; want 0", len(set), allocs)
		}
		if want := before[len(before)-1].Spec.ReqTimestampMock; !p.At().Equal(want) {
			t.Errorf("set of %d: previous call at %v; want the latest call sent before paired, at %v", len(set), p.At(), want)
		}
	}
}

// countingTests is recordedTests counting what OwnCall asks of the store.
type countingTests struct {
	recordedTests
	same int
}

func (c *countingTests) SameRecordedTestWindow(a, b time.Time) (bool, bool) {
	c.same++
	return c.recordedTests.SameRecordedTestWindow(a, b)
}

// The previous-call search asks its costlier questions (the sibling rule,
// a protocol phase, and a test's own call's store: its test windows and
// whose a serve is) only of a recording that would move its answer: one
// sent before paired and after the latest previous call seen so far. Seen
// latest first, a set of any size costs one question of each, the
// recordings sent after paired, before the latest, or at its instant (a
// twin of it) none: they were each a question of the store per recording
// of the set, per call tried.
func TestPreviousCallAsksItsRulesOnlyOfARecordingThatMovesIt(t *testing.T) {
	for _, n := range []int{10, 1000} {
		paired, before, after := previousCallSet(n)
		latest := before[len(before)-1]
		twin := previousCallRec("mock-twin", latest.Spec.ReqTimestampMock, latest.Spec.ResTimestampMock)
		set := append(append([]*models.Mock{latest, twin}, after...), before[:len(before)-1]...)

		siblings := 0
		p := NewPreviousCallOf(paired, func(*models.Mock) bool { siblings++; return false })
		for _, m := range set {
			p.See(m)
		}
		if siblings != 1 || !p.At().Equal(latest.Spec.ReqTimestampMock) {
			t.Errorf("set of %d: the sibling rule asked %d times, previous call at %v; want once, at %v", len(set), siblings, p.At(), latest.Spec.ReqTimestampMock)
		}

		tests := &countingTests{recordedTests: recordedTests{paired.Spec.ReqTimestampMock: 1}}
		for _, m := range set {
			tests.recordedTests[m.Spec.ReqTimestampMock] = 1
		}
		owners := 0
		tr := NewTracker()
		tr.SetServeOwnerOf(func(*models.Mock) ServeOwner { owners++; return OwnerTest })
		oc := tr.NewOwnCall(paired, nil, tests)
		if oc == nil {
			t.Fatalf("set of %d: no own call for a paired recording inside a recorded test", len(set))
		}
		for _, m := range set {
			oc.See(m)
		}
		if tests.same != 1 || owners != 1 || !oc.prev.At().Equal(latest.Spec.ReqTimestampMock) {
			t.Errorf("set of %d: the store asked %d times of a test window and %d of an owner, previous call at %v; want once each, at %v", len(set), tests.same, owners, oc.prev.At(), latest.Spec.ReqTimestampMock)
		}
	}
}
