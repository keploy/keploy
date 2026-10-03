package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	keployPkg "go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// paramsInstr records the filter params a step sends. Its embedded interface is
// nil: only UpdateMockParams is called.
type paramsInstr struct {
	Instrumentation
	last models.MockFilterParams
}

func (f *paramsInstr) UpdateMockParams(_ context.Context, p models.MockFilterParams) error {
	f.last = p
	return nil
}

// perTestInstr is an agent connection whose agent said it reads the consumed
// history only for its per-test mocks.
type perTestInstr struct{ paramsInstr }

func (f *perTestInstr) AgentReadsConsumedPerTestOnly() bool { return true }

// The runner's per-step filter params cross the wire to the agent like the
// replayer's, so they are narrowed under the same rule: only for an agent that
// said it reads no more than its per-test entries, and then without changing
// what it reads.
func TestSendPerTestParamsNarrowsTheConsumedHistoryOnlyForAnAgentThatSaysSo(t *testing.T) {
	setup := &testSetSetup{totalConsumed: map[string]models.MockState{}}
	var perTest []*models.Mock
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("per-test-%d", i)
		perTest = append(perTest, &models.Mock{Name: name})
		if i%2 == 0 {
			setup.totalConsumed[name] = models.MockState{Name: name, Usage: models.Deleted}
		}
	}
	for i := 0; i < 200; i++ {
		name := fmt.Sprintf("session-%d", i)
		setup.totalConsumed[name] = models.MockState{Name: name, Usage: models.Updated, Lifetime: models.LifetimeSession}
	}
	setup.perTestRegion = keployPkg.PerTestRegion(perTest)

	encode := func(m map[string]models.MockState) string {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	now := time.Now()

	old := &paramsInstr{}
	r := &Runner{logger: zap.NewNop(), instrumentation: old}
	if err := r.sendPerTestParams(context.Background(), setup, nil, now, now); err != nil {
		t.Fatal(err)
	}
	if got, want := encode(old.last.TotalConsumedMocks), encode(setup.totalConsumed); got != want {
		t.Fatalf("an agent that did not say its scope got %d of %d consumed entries", len(old.last.TotalConsumedMocks), len(setup.totalConsumed))
	}

	scoped := &perTestInstr{}
	r = &Runner{logger: zap.NewNop(), instrumentation: scoped}
	if err := r.sendPerTestParams(context.Background(), setup, nil, now, now); err != nil {
		t.Fatal(err)
	}
	if got, want := encode(scoped.last.TotalConsumedMocks), encode(keployPkg.ConsumedForAgent(setup.totalConsumed, setup.perTestRegion)); got != want {
		t.Fatalf("the per-test agent got %s; want %s", got, want)
	}
	if len(scoped.last.TotalConsumedMocks) != 2 {
		t.Fatalf("the per-test agent got %d entries; want its 2 consumed per-test mocks", len(scoped.last.TotalConsumedMocks))
	}
}
