package replay

import (
	"context"
	"fmt"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// Ownership and exact consumption for keploy test, under the proven switch
// (design v14 §5.2, S12): the replay hands the agent what each test case
// owns, and the agent serves each test case exactly its own mocks, each at
// most once and in recorded order, through the proxy's exact serving.
//
//   - A test case owns the mocks its mapping lists (the agent keeps the
//     data-plane ones). A test case the mapping omits, or lists with none,
//     owns none, and so does every test case of a set with no mappings.yaml
//     or an all-empty one: a call it makes is a miss. disableMapping does not
//     apply.
//   - A mapping that cannot be read refuses the set.
//   - Every mock no test case lists is the set's pool. It answers between
//     test cases, beside the protocol-phase mocks, with reuse or
//     cursor-saturate as recorded.
//   - The set is staged on the agent once; a test case's call opens it, and
//     the call after its consumption is read serves the pool again.
//   - The replay never writes mappings.yaml: only record does.

// exactPlan is the set's exact serving: what each test case owns, and the
// set's pool.
type exactPlan struct {
	owned map[string][]string
	pool  []string
}

// exactServing reports whether this replay serves each test case exactly
// its own mocks: under the proven switch, with an agent to serve them.
func (r *Replayer) exactServing() bool {
	return provenswitch.On() && r.instrument && !r.noAgent
}

// provenMappings reads what each test case of testSetID owns, under the
// proven switch: none for a set with no mapping store or no mappings.yaml,
// and an error for a mapping that cannot be read.
func (r *Replayer) provenMappings(ctx context.Context, testSetID string) (map[string][]models.MockEntry, error) {
	if r.mappingDB == nil {
		return map[string][]models.MockEntry{}, nil
	}
	mappings, _, err := r.mappingDB.Get(ctx, testSetID)
	if err != nil {
		return nil, fmt.Errorf("keploy cannot judge each test case of %s: its mappings.yaml cannot be read: %w; record the set again", testSetID, err)
	}
	if mappings == nil {
		mappings = map[string][]models.MockEntry{}
	}
	return mappings, nil
}

// newExactPlan is the plan for a set whose test cases own mappings and whose
// stored mocks are stored: every stored mock no mapping lists is the pool's.
// A name a mapping lists that no stored mock has is left to the agent, which
// refuses the test case that owns it (the store may prune mocks of test
// cases this run does not select).
func newExactPlan(mappings map[string][]models.MockEntry, stored ...[]*models.Mock) *exactPlan {
	p := &exactPlan{owned: make(map[string][]string, len(mappings))}
	mapped := map[string]bool{}
	for test, entries := range mappings {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.Name == "" {
				continue
			}
			names = append(names, e.Name)
			mapped[e.Name] = true
		}
		p.owned[test] = names
	}
	seen := map[string]bool{}
	for _, list := range stored {
		for _, mk := range list {
			if mk == nil || mk.Name == "" || mapped[mk.Name] || seen[mk.Name] {
				continue
			}
			seen[mk.Name] = true
			p.pool = append(p.pool, mk.Name)
		}
	}
	return p
}

// serveExactTest opens test case name on the agent, served exactly its own
// mocks; vars are the test variables its request carries.
func (r *Replayer) serveExactTest(ctx context.Context, plan *exactPlan, name string, vars ...models.VariableBinding) error {
	err := r.instrumentation.UpdateMockParams(ctx, models.MockFilterParams{
		Variables: vars,
		Exact:     &models.ExactServing{Test: name, Owned: append([]string(nil), plan.owned[name]...)},
	})
	if err != nil {
		utils.LogError(r.logger, err, "failed to serve the test case its own mocks", zap.String("testCase", name))
	}
	return err
}

// serveExactPool serves the set's pool again, once a test case's
// consumption is read: a call the application makes between test cases is
// the pool's.
func (r *Replayer) serveExactPool(ctx context.Context, plan *exactPlan) error {
	if plan == nil {
		return nil
	}
	err := r.instrumentation.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{}})
	if err != nil {
		utils.LogError(r.logger, err, "failed to serve the test set's pool between test cases")
	}
	return err
}

// mappingsIgnoredOnce says once per run that disableMapping does not apply
// under the proven switch.
func (r *Replayer) noteMappingAlwaysUsed() {
	r.disableMappingNoticeOnce.Do(func() {
		r.logger.Warn("disableMapping is ignored under KEPLOY_PROVEN: each test case is served exactly the mocks its mapping lists",
			zap.String("next_step", "remove disableMapping (or --disable-mapping) from the configuration"))
	})
}
