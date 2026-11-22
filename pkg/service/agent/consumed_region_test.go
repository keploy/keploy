package agent

import (
	"fmt"
	"testing"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The CLI sends the agent only the consumed entries of the set's per-test mocks
// (pkg.ConsumedForAgent). The agent reads that map in filterOutDeleted, over the
// per-test mocks it stages, which all come from the per-test half of StoreMocks.
// So the narrowed map must give the agent exactly the verdict the whole history
// gives: the same mocks kept, with the same IsFiltered and SortOrder carried
// over from their consumed entries. This holds for an agent of any version that
// reads the map only there (v3.3.23 on); an older CLI still sends everything,
// which this agent reads the same way.
// wrappedAgent embeds the agent service, as a wrapper that overrides
// UpdateMockParams would.
type wrappedAgent struct{ *Agent }

// The route advertises the per-test scope (models.ConsumedScopeHeader) for the
// agent service itself, and for nothing that merely embeds it: a wrapper may
// read the consumed history differently in its own UpdateMockParams.
func TestReadsConsumedForPerTestOnlyIsNotInheritedByAWrapper(t *testing.T) {
	a := &Agent{logger: zap.NewNop()}
	if !a.ReadsConsumedForPerTestOnly(a) {
		t.Fatal("the agent service does not advertise its own per-test scope")
	}
	w := wrappedAgent{a}
	if w.ReadsConsumedForPerTestOnly(w) {
		t.Fatal("a service embedding the agent inherited its per-test scope")
	}
	if a.ReadsConsumedForPerTestOnly(&Agent{}) {
		t.Fatal("the scope was answered for another agent")
	}
}

func TestFilterOutDeletedSameVerdictFromThePerTestRegionAlone(t *testing.T) {
	a := &Agent{logger: zap.NewNop()}

	// The per-test half of a set's StoreMocks.
	var region []*models.Mock
	for i := 0; i < 12; i++ {
		m := &models.Mock{Name: fmt.Sprintf("mock-%d", i)}
		m.TestModeInfo.SortOrder = int64(i + 1)
		region = append(region, m)
	}
	region = append(region, &models.Mock{}, nil) // unnamed and nil entries pass through

	// Everything consumed so far: some per-test mocks consumed (Deleted) or
	// re-stamped (Updated), and many more session, connection and config mocks
	// the region never holds.
	total := map[string]models.MockState{}
	for i := 0; i < 12; i += 3 {
		total[fmt.Sprintf("mock-%d", i)] = models.MockState{Name: fmt.Sprintf("mock-%d", i), Usage: models.Deleted}
	}
	for i := 1; i < 12; i += 4 {
		total[fmt.Sprintf("mock-%d", i)] = models.MockState{Name: fmt.Sprintf("mock-%d", i), Usage: models.Updated, IsFiltered: true, SortOrder: int64(900 + i)}
	}
	for i := 0; i < 500; i++ {
		name := fmt.Sprintf("session-%d", i)
		total[name] = models.MockState{Name: name, Usage: models.Updated, Lifetime: models.LifetimeSession, SortOrder: int64(5000 + i)}
	}
	total["config-0"] = models.MockState{Name: "config-0", Usage: models.Deleted, Type: "config"}

	narrowed := pkg.ConsumedForAgent(total, pkg.PerTestRegion(region))
	if len(narrowed) >= len(total) || len(narrowed) == 0 {
		t.Fatalf("precondition: the narrowed map has %d of %d entries", len(narrowed), len(total))
	}

	clone := func() []*models.Mock {
		out := make([]*models.Mock, len(region))
		for i, m := range region {
			if m != nil {
				c := *m
				out[i] = &c
			}
		}
		return out
	}
	describe := func(ms []*models.Mock) string {
		s := ""
		for _, m := range ms {
			if m == nil {
				s += "<nil> "
				continue
			}
			s += fmt.Sprintf("%s/%v/%d ", m.Name, m.TestModeInfo.IsFiltered, m.TestModeInfo.SortOrder)
		}
		return s
	}

	fromTotal := describe(a.filterOutDeleted(clone(), total))
	fromNarrowed := describe(a.filterOutDeleted(clone(), narrowed))
	if fromTotal != fromNarrowed {
		t.Fatalf("the narrowed history changed the agent's verdict:\n whole    %s\n narrowed %s", fromTotal, fromNarrowed)
	}
}
