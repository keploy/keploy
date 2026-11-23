package replayer

import (
	"slices"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// eventTally is an integrations.OwnershipTally that keeps what it is told.
type eventTally []integrations.OwnershipEvent

func (e *eventTally) TallyOwnership(ev integrations.OwnershipEvent) { *e = append(*e, ev) }

// A call's ownership decisions reach the store's tally (the set's summary):
// an answer the edge rule gave the open window, a first exact match kept from
// answering at once and whether another recording answered, and a drifted
// query ranked over a higher score; nothing for an unanswered call or one the
// first exact match answered at once.
func TestTallyOwnershipReportsEachDecision(t *testing.T) {
	first, other := &models.Mock{Name: "first"}, &models.Mock{Name: "other"}
	for _, c := range []struct {
		name        string
		answered    *models.Mock
		rank        int
		first       *models.Mock
		deferred    bool
		driftRanked bool
		want        []integrations.OwnershipEvent
	}{
		{"answered at once", first, 0, first, false, false, nil},
		{"by the edge rule", first, 1, first, false, false, []integrations.OwnershipEvent{integrations.OwnershipEdgeAnswered}},
		{"deferred, then answered by it", first, 0, first, true, false, []integrations.OwnershipEvent{integrations.OwnershipDeferred}},
		{"deferred, answered by another", other, 0, first, true, false, []integrations.OwnershipEvent{integrations.OwnershipDeferred, integrations.OwnershipPassedOver}},
		{"drifted query ranked", other, 0, nil, false, true, []integrations.OwnershipEvent{integrations.OwnershipDriftRanked}},
		{"unanswered", nil, 1, first, true, true, nil},
	} {
		var got eventTally
		tallyOwnership(&got, c.answered, c.rank, c.first, c.deferred, c.driftRanked)
		if !slices.Equal([]integrations.OwnershipEvent(got), c.want) {
			t.Errorf("%s: tallied %v; want %v", c.name, got, c.want)
		}
	}
}
