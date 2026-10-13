package agent

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// resetProxy counts the times the agent forgets the bound variables.
type resetProxy struct {
	coreAgent.Proxy
	resets int
}

func (p *resetProxy) ResetVariables() { p.resets++ }

func TestRootTest(t *testing.T) {
	for _, c := range []struct {
		id, root string
		sub      bool
	}{
		{"TestOrder", "TestOrder", false},
		{"TestOrder/read", "TestOrder", true},
		{"TestOrder/read/deep", "TestOrder", true},
		{"pkg/a_test.go::TestOrder/read", "pkg/a_test.go::TestOrder", true},
		{"proj::pkg/a/b_test.go::TestOrder", "proj::pkg/a/b_test.go::TestOrder", false},
		{"proj::pkg/a/b_test.go::Test%3A%3Ax/y", "proj::pkg/a/b_test.go::Test%3A%3Ax", true},
	} {
		root, sub := rootTest(c.id)
		if root != c.root || sub != c.sub {
			t.Fatalf("rootTest(%q) = %q, %v; want %q, %v", c.id, root, sub, c.root, c.sub)
		}
	}
}

// A root test begins with no variable bound; its subtests keep what it bound;
// the same root test begun again (go test -count=2) starts afresh.
func TestARootTestAndItsSubtestsShareTheirVariables(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := &resetProxy{}
	a := &Agent{logger: zap.NewNop(), Proxy: p, config: &config.Config{Agent: config.Agent{Mode: models.MODE_TEST}}}
	ctx := context.Background()
	for _, step := range []struct {
		test   string
		resets int
	}{
		{"f_test.go::TestOrder", 1},
		{"f_test.go::TestOrder/create", 1},
		{"f_test.go::TestOrder/read", 1},
		{"f_test.go::TestUser", 2},
		{"f_test.go::TestUser", 3},
		{"f_test.go::TestOrder/read", 4},
		{"g_test.go::TestOrder/read", 5},
	} {
		// The begin goes on to narrow the pool, which this agent has none
		// to narrow: its error is not what is tested here.
		_ = a.BeginScopeAt(ctx, step.test, 0, models.BaseTime)
		if p.resets != step.resets {
			t.Fatalf("after %s began the variables were forgotten %d times, want %d", step.test, p.resets, step.resets)
		}
		_ = a.EndScopeAt(ctx, step.test, 0, models.BaseTime)
	}
}

func TestATestKeepsItsVariablesWithTheSwitchOff(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	p := &resetProxy{}
	a := &Agent{logger: zap.NewNop(), Proxy: p, config: &config.Config{Agent: config.Agent{Mode: models.MODE_TEST}}}
	_ = a.BeginScopeAt(context.Background(), "TestOrder", 0, models.BaseTime)
	if p.resets != 0 {
		t.Fatal("switch off: a begin forgot the variables")
	}
}

// Under the switch a mock whose variables this keploy cannot follow is not
// served: it is left out of the pool and named.
func TestVariablesKeployCannotFollowAreRefusedAtLoad(t *testing.T) {
	vars := models.MockVariables{"id": {Recorded: "1", Bind: "body.id"}}
	httpVars := &models.Mock{Name: "http", Kind: models.HTTP, Spec: models.MockSpec{Variables: vars}}
	pg := &models.Mock{Name: "pg", Kind: models.PostgresV2, Spec: models.MockSpec{Variables: vars}}
	async := &models.Mock{Name: "async", Kind: models.HTTP, Spec: models.MockSpec{Variables: vars, Async: &models.AsyncMeta{Lane: "l"}}}
	plain := &models.Mock{Name: "plain", Kind: models.PostgresV2}
	names := func(ms []*models.Mock) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.Name)
		}
		return out
	}
	a := &Agent{logger: zap.NewNop()}
	got := names(a.refuseUnfollowable([]*models.Mock{httpVars, pg, async, plain}))
	if len(got) != 2 || got[0] != "http" || got[1] != "plain" {
		t.Fatalf("kept %v, want [http plain]", got)
	}
}
