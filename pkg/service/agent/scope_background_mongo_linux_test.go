//go:build linux

package agent

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

type bgMongoServingProxy struct {
	*bgServingProxy
	ruleSeeds int
}

func (p *bgMongoServingProxy) SeedBackgroundMongoRules(rules []models.BackgroundMongoRule) {
	p.ruleSeeds++
	p.mm.SeedBackgroundMongoRules(rules)
}

// stageForExactServing forwards BackgroundMongoRules to the proxy's MockManager,
// skips redundant restaging when rules are unchanged, restages when they change,
// and clears them when a new scope table sends none.
func TestKeployMockReplayStagesBackgroundMongoRulesOnProxy(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	p := &bgMongoServingProxy{bgServingProxy: &bgServingProxy{servingProxy: newServingProxy()}}
	a.Proxy = p
	ctx := context.Background()

	r1 := []models.BackgroundMongoRule{{
		ID:          "ui-join-backfill-index-ready",
		DB:          "api-server",
		Command:     "listIndexes",
		Collections: []string{"uiTestSetJoins", "testsets"},
	}}
	r2 := []models.BackgroundMongoRule{{
		ID:          "e2e-pack-sweep-unread",
		DB:          "api-server",
		Command:     "find",
		Collections: []string{"e2eRecs"},
		Where:       []models.BackgroundMongoWhere{{Field: "body.cmd.filter.kind", Equals: "startup"}},
	}}

	require.NoError(t, a.StoreMocks(ctx, nil, x2Set(t)))
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{".::TestGetCurrentUser": {"mock-3734"}}))
	require.NoError(t, a.SetRecordedWindows(ctx, []models.TestWindow{{TestCase: ".::TestGetCurrentUser", Start: x2At(t, "08:25:06.8"), End: x2At(t, "08:25:07.0")}}))
	require.NoError(t, a.SetScopeBackgroundMongo(ctx, r1))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{".": {"mock-0"}}))
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "."}, os.Getpid())
	require.Nil(t, serr)

	require.Equal(t, r1, p.mm.BackgroundMongoRules())
	seeds := p.ruleSeeds
	require.Greater(t, seeds, 0)

	// Identical rules do not trigger a restage.
	require.NoError(t, a.SetScopeBackgroundMongo(ctx, r1))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{".": {"mock-0"}}))
	require.Equal(t, seeds, p.ruleSeeds, "unchanged backgroundMongo rules unexpectedly restaged the set")

	// Changed rules restage and replace the proxy's rules.
	require.NoError(t, a.SetScopeBackgroundMongo(ctx, r2))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{".": {"mock-0"}}))
	require.Greater(t, p.ruleSeeds, seeds, "changed backgroundMongo rules did not restage the set")
	require.Equal(t, r2, p.mm.BackgroundMongoRules())

	// Clearing with nil restages and clears the proxy's rules.
	seeds = p.ruleSeeds
	require.NoError(t, a.SetScopeBackgroundMongo(ctx, nil))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{".": {"mock-0"}}))
	require.Greater(t, p.ruleSeeds, seeds, "clearing backgroundMongo rules did not restage the set")
	require.Empty(t, p.mm.BackgroundMongoRules())
}
