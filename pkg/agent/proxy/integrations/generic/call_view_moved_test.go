package generic

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// movedStore is exactStore whose open test answered answered
// (integrations.ExactConsumedReader), holding each call to itself, and whose
// call's view moved (integrations.CallViewTeller): since it was held
// (moved), or inside its read of the answered list (inList), so that only a
// question asked after the list is read sees it.
type movedStore struct {
	exactStore
	answered      []*models.Mock
	moved, inList bool
}

func (s *movedStore) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return s, func() bool { return false }
}

func (s *movedStore) ConsumedTestMocks() []*models.Mock {
	if s.inList {
		s.moved = true
	}
	return s.answered
}

func (s *movedStore) CallViewMoved() bool { return s.moved }

// Under exact serving an exchange whose view moved between its reads (a
// test's begin or end installed another view since it was held, or while it
// read the test's answered exchanges) is told no verdict built from the
// test's answered list: its miss and the list may be of two views. It is
// not one exchange more than recorded, it is compared with none of the
// answered exchanges, and no exchange made elsewhere is named for it: it
// misses plainly. With no move, the same store tells each. Before, the
// answered list was read whatever view it was of: a moved exchange was told
// one more than recorded, compared with the list, or named made elsewhere
// by a list that held none of it.
func TestAnExchangeWhoseViewMovedIsToldNoVerdictOfTheAnsweredList(t *testing.T) {
	ping := exchange("mock-ping", "PING")
	set := exchange("mock-set", "SET k v")
	elsewhere := exchange("mock-auth-x", "AUTH step2")
	elsewhere.Spec.Metadata = map[string]string{"type": "config", models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
	store := func(moved, inList bool, answered ...*models.Mock) *movedStore {
		return &movedStore{exactStore: exactStore{exact: true, own: map[string]bool{}, staged: []*models.Mock{elsewhere}}, answered: answered, moved: moved, inList: inList}
	}
	for _, mv := range []struct {
		name          string
		moved, inList bool
	}{
		{name: "not moved"},
		{name: "moved since the hold", moved: true},
		{name: "moved inside the read of the answered list", inList: true},
	} {
		t.Run(mv.name, func(t *testing.T) {
			told := !mv.moved && !mv.inList
			r := missReport(t, store(mv.moved, mv.inList, ping), "PING")
			if extra := r.MissKind == models.MissExtraCall && r.Recorded == 1; extra != told {
				t.Errorf("an exchange identical to the test's answered one missed as %q, recorded %d; want one more than recorded %v", r.MissKind, r.Recorded, told)
			}
			r = missReport(t, store(mv.moved, mv.inList, set), "SET k w")
			if compared := r.ClosestMock == "mock-set"; compared != told {
				t.Errorf("a changed exchange's closest is %q; want it compared with the test's answered mock-set %v", r.ClosestMock, told)
			}
			r = missReport(t, store(mv.moved, mv.inList), "AUTH step2")
			if named := r.NotOwned && r.ClosestMock == "mock-auth-x"; named != told {
				t.Errorf("an exchange another test recorded is named %q (not owned %v); want it named made elsewhere %v", r.ClosestMock, r.NotOwned, told)
			}
			if r.MissKind == models.MissExtraCall {
				t.Errorf("an exchange the answered list does not hold missed as %q", r.MissKind)
			}
		})
	}
}

// movedSecondStore is movedStore whose call's view moves only inside its
// second read of the answered list: the extra-call count read it in one
// view, the answered comparison reads it after the move.
type movedSecondStore struct {
	movedStore
	reads int
}

func (s *movedSecondStore) ConsumedTestMocks() []*models.Mock {
	s.reads++
	if s.reads == 2 {
		s.moved = true
	}
	return s.answered
}

func (s *movedSecondStore) CallViewMoved() bool { return s.moved }

func (s *movedSecondStore) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return s, func() bool { return false }
}

// A changed exchange whose view moves inside the answered comparison's read
// of the list, after the extra-call count read it in one view, is not
// compared with the list: the comparison asks after its own read too.
func TestAGenericExchangeMovedInsideTheSecondListReadIsNotCompared(t *testing.T) {
	set := exchange("mock-set", "SET k v")
	s := &movedSecondStore{movedStore: movedStore{exactStore: exactStore{exact: true, own: map[string]bool{}}, answered: []*models.Mock{set}}}
	r := missReport(t, s, "SET k w")
	if s.reads < 2 {
		t.Fatalf("precondition: the list was read %d times; want 2", s.reads)
	}
	if r.ClosestMock == "mock-set" {
		t.Errorf("a changed exchange whose view moved inside the second read is compared with the answered mock-set")
	}
}
