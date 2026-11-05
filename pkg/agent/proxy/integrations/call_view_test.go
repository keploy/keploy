package integrations

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// viewStore is a store a call is held to whose open test answered answered
// (ExactConsumedReader), and whose call's view moved (CallViewTeller): since
// it was held (moved), or inside its read of the answered list (inList).
type viewStore struct {
	MockMemDb
	answered      []*models.Mock
	moved, inList bool
}

func (s *viewStore) ConsumedTestMocks() []*models.Mock {
	if s.inList {
		s.moved = true
	}
	return s.answered
}

func (s *viewStore) CallViewMoved() bool { return s.moved }

// A call is told its test's answered recordings only as of one view: none,
// and not one view, once its view moved, whether before the list is read or
// while it is (the question is asked after the read). A store that lists
// none tells none, in one view.
func TestTheAnsweredListIsToldOnlyAsOfTheCallsOneView(t *testing.T) {
	m := &models.Mock{Name: "mock-1"}
	for _, tc := range []struct {
		name          string
		db            MockMemDb
		want, oneView bool
	}{
		{name: "a store that lists none", db: struct{ MockMemDb }{}, oneView: true},
		{name: "not moved", db: &viewStore{answered: []*models.Mock{m}}, want: true, oneView: true},
		{name: "moved since the hold", db: &viewStore{answered: []*models.Mock{m}, moved: true}},
		{name: "moved inside the read of the list", db: &viewStore{answered: []*models.Mock{m}, inList: true}},
	} {
		got, oneView := ConsumedInCallView(tc.db)
		if oneView != tc.oneView || (len(got) == 1 && got[0] == m) != tc.want || !tc.want && len(got) != 0 {
			t.Errorf("%s: told %d recordings, one view %v; want mock-1 %v, one view %v", tc.name, len(got), oneView, tc.want, tc.oneView)
		}
	}
}

// A call's mark made from a round snapshot carries the store's token of the
// view the snapshot read (RoundParts.View), whatever round it is: a test's
// or the pool's, the learn tracker ahead of the store, or a set the store
// has not staged. So a store held to the call is held to that view, never to
// one installed after the snapshot. A snapshot with no token marks none.
func TestARoundSnapshotsCallRoundCarriesTheViewItRead(t *testing.T) {
	type token struct{ n int }
	for _, tc := range []struct {
		name string
		snap RoundSnapshot
	}{
		{name: "a test's view", snap: RoundSnapshot{Test: "test-0", Phase: RoundInTest}},
		{name: "the pool view between tests", snap: RoundSnapshot{Phase: RoundBetween}},
		{name: "the learn tracker ahead of the store", snap: RoundSnapshot{Phase: RoundBoundary, Learn: LearnRead{Bound: true, Open: true}}},
		{name: "a set the store has not staged", snap: RoundSnapshot{Phase: RoundBoundary, Learn: LearnRead{Bound: true, SetAhead: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := &token{n: 1}
			c := tc.snap.WithParts(RoundParts{View: v}).CallRound()
			if got, ok := c.StoreView().(*token); !ok || got != v {
				t.Fatalf("the call's mark carries %v; want the snapshot's view %v", c.StoreView(), v)
			}
			if got := tc.snap.WithParts(RoundParts{}).CallRound().StoreView(); got != nil {
				t.Fatalf("a snapshot with no view marks %v; want none", got)
			}
		})
	}
}
