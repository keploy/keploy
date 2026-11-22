package generic

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// elsewhereView is s, serving exactly, whose open view shows view: the
// exchanges a miss's report compares the call with.
type elsewhereView struct {
	exactStore
	view []*models.Mock
}

func (s elsewhereView) ExactServing() bool                       { return true }
func (s elsewhereView) GetSessionMocks() ([]*models.Mock, error) { return s.view, nil }

// HoldToRound implements integrations.RoundHolder as exactStore's does,
// holding s itself.
func (s elsewhereView) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return s, func() bool { return false }
}

// A miss on an exchange another test recorded names that recording and shows
// no diff: the report's own diff was of the closest exchange it compared in
// the open view, another recording, so the miss would name one recording and
// describe another. Before, the diff named the test's own mock-auth-1.
func TestAMissMadeElsewhereShowsNoDiffOfAnotherExchange(t *testing.T) {
	own, other := exchange("mock-auth-1", "AUTH step1"), exchange("mock-auth-x", "AUTH step2")
	for _, m := range []*models.Mock{own, other} {
		m.Spec.Metadata = map[string]string{"type": "config", models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
	}
	store := elsewhereView{
		exactStore: exactStore{exact: true, own: map[string]bool{"mock-auth-1": true}, staged: []*models.Mock{own, other}},
		view:       []*models.Mock{own},
	}
	r := missReport(t, store, "AUTH step3")
	if r.ClosestMock != "mock-auth-1" || !strings.Contains(r.Diff, "mock-auth-1") || r.NotOwned {
		t.Fatalf("precondition: the miss of an exchange no recording is names %q (not owned %v) with diff %q; want the view's mock-auth-1 and a diff of it", r.ClosestMock, r.NotOwned, r.Diff)
	}
	r = missReport(t, store, "AUTH step2")
	if r.ClosestMock != "mock-auth-x" || !r.NotOwned || r.Diff != "" {
		t.Errorf("the miss made elsewhere names %q (not owned %v) with diff %q; want mock-auth-x and no diff", r.ClosestMock, r.NotOwned, r.Diff)
	}
}
