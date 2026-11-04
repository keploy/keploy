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
