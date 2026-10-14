package agent

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
	"go.uber.org/zap"
)

// resetProxy counts the times the agent forgets the bound variables.
type resetProxy struct {
	coreAgent.Proxy
	resets int
}

func (p *resetProxy) ResetVariables() { p.resets++ }

// bindProxy keeps a real table.
type bindProxy struct {
	coreAgent.Proxy
	table variables.Table
}

func (p *bindProxy) ResetVariables() { p.table.Reset() }
func (p *bindProxy) StartVariables(given []models.VariableBinding) bool {
	p.table.Start(given)
	return true
}
func (p *bindProxy) BoundVariables() ([]models.VariableBinding, bool) {
	return p.table.Snapshot(), true
}

// A test begins with what the previous one bound forgotten and the values its
// request gave bound, marked as given; an agent whose proxy keeps no table
// refuses given values.
func TestStartTestVariables(t *testing.T) {
	p := &bindProxy{}
	a := &Agent{logger: zap.NewNop(), Proxy: p}
	p.table.Bind(models.VariableBinding{Name: "old", Value: "x"})
	if err := a.StartTestVariables([]models.VariableBinding{{Name: "id", Value: "u1"}}); err != nil {
		t.Fatal(err)
	}
	got, kept := a.TestVariables()
	if !kept || len(got) != 1 || got[0].Name != "id" || got[0].Value != "u1" || !got[0].Given {
		t.Fatalf("bound %+v (%v)", got, kept)
	}
	none := &Agent{logger: zap.NewNop(), Proxy: &resetProxy{}}
	if err := none.StartTestVariables([]models.VariableBinding{{Name: "id", Value: "u1"}}); err == nil {
		t.Fatal("an agent that keeps no table took given values")
	}
	if err := none.StartTestVariables(nil); err != nil {
		t.Fatalf("an agent that keeps no table, given nothing: %v", err)
	}
	if _, kept := none.TestVariables(); kept {
		t.Fatal("an agent that keeps no table said it keeps one")
	}
}

// Every test begins with no variable bound, and a test begun again (go test
// -count=2) starts afresh.
func TestEveryTestBeginsWithNoVariableBound(t *testing.T) {
	a, p := orderAgent(t)
	a.clientMocks.Store(uint64(0), &ClientMockStorage{})
	ctx := context.Background()
	for i, test := range []string{"f_test.go::TestOrder", "f_test.go::TestUser", "f_test.go::TestUser", "g_test.go::TestOrder"} {
		if err := a.BeginScopeAt(ctx, test, 0, models.BaseTime); err != nil {
			t.Fatalf("begin %s: %v", test, err)
		}
		resets := 0
		for _, e := range p.events {
			if e == "reset" {
				resets++
			}
		}
		if resets != i+1 {
			t.Fatalf("after %s began the variables were forgotten %d times, want %d", test, resets, i+1)
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

// managerlessProxy keeps variables but no mock manager to hold them.
type managerlessProxy struct{ coreAgent.Proxy }

func (managerlessProxy) ResetVariables()                                  {}
func (managerlessProxy) StartVariables([]models.VariableBinding) bool     { return false }
func (managerlessProxy) BoundVariables() ([]models.VariableBinding, bool) { return nil, false }

// A proxy with no mock manager cannot bind a test's values: the filter that
// carries some fails, one that carries none begins an empty scope, and the
// read says it keeps none.
func TestAManagerlessProxyCannotBeginGivenValues(t *testing.T) {
	a := &Agent{logger: zap.NewNop(), Proxy: managerlessProxy{}}
	if err := a.StartTestVariables([]models.VariableBinding{{Name: "id", Value: "u1"}}); err == nil {
		t.Fatal("given values were accepted with no manager to hold them")
	}
	if err := a.StartTestVariables(nil); err != nil {
		t.Fatalf("no given values: %v", err)
	}
	if _, kept := a.TestVariables(); kept {
		t.Fatal("a proxy with no manager said it keeps variables")
	}
}
