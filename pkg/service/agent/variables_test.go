package agent

import (
	"context"
	"testing"
	"time"

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

// Every test begins with no variable bound: the integrations begin only
// top-level tests, so a Go subtest shares its parent's scope by never beginning
// one, and a test begun again (go test -count=2) starts afresh.
func TestEveryTestBeginsWithNoVariableBound(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := &resetProxy{}
	a := &Agent{logger: zap.NewNop(), Proxy: p, config: &config.Config{Agent: config.Agent{Mode: models.MODE_TEST}}}
	ctx := context.Background()
	for i, test := range []string{"f_test.go::TestOrder", "f_test.go::TestUser", "f_test.go::TestUser", "g_test.go::TestOrder"} {
		// The begin goes on to narrow the pool, which this agent has none
		// to narrow: its error is not what is tested here.
		_ = a.BeginScopeAt(ctx, test, 0, models.BaseTime)
		if p.resets != i+1 {
			t.Fatalf("after %s began the variables were forgotten %d times, want %d", test, p.resets, i+1)
		}
		_ = a.EndScopeAt(ctx, test, 0, models.BaseTime)
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

// The mock stream, the one path /storemocks takes, leaves out under the switch
// a mock whose variables keploy cannot follow, and keeps it with the switch
// off.
func TestStoreMocksStreamRefusesVariablesKeployCannotFollow(t *testing.T) {
	vars := models.MockVariables{"id": {Recorded: "1", Bind: "body.id"}}
	for _, tc := range []struct {
		proven string
		want   int
	}{{"1", 1}, {"0", 2}} {
		t.Run("KEPLOY_PROVEN="+tc.proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, tc.proven)
			RegisterHooks(&captureHook{})
			a := newTestAgent()
			httpVars := mkMock("http", models.HTTP, time.Time{})
			httpVars.Spec.Variables = vars
			pg := mkMock("pg", models.PostgresV2, time.Time{})
			pg.Spec.Variables = vars
			header, dec := readHeaderAndDecoder(t, encodeStreamBody(t, nil, []*models.Mock{httpVars, pg}))
			if err := a.StoreMocksStream(context.Background(), header, dec); err != nil {
				t.Fatal(err)
			}
			st := loadStorage(t, a)
			if len(st.unfiltered) != tc.want {
				t.Fatalf("stored %d mocks, want %d", len(st.unfiltered), tc.want)
			}
		})
	}
}
