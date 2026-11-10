package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
)

// The mock manager's test variables keep what each exactly served test's
// scope bound itself under the name the manager opened it by
// (OpenExactTest), for a background call the application makes for that
// test once it ended (variables.Table.EndedOf): what test-1 bound is
// test-1's once test-2 opens, and never test-2's. The scope the next test's
// calls in flight bind in (AheadVariables) is read by no background call,
// and keeps none.
func TestTheManagerKeepsWhatEachExactTestBoundByItsName(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	own1 := varMockOf("own-1", "/apps", "app-1", true, models.LifetimePerTest)
	own2 := varMockOf("own-2", "/apps", "app-2", true, models.LifetimePerTest)
	mm := stagedManager(t, []*models.Mock{own1, own2}, nil)
	mm.NewVariableSet()
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.OpenExactTest("test-1", []*models.Mock{own1}, nil)
	bound := variables.Binding{Name: "key", Value: "app-live-1", Recorded: "app-1", Mock: "own-1", Place: "body.key"}
	if _, ok := mm.Variables().Bind(bound); !ok {
		t.Fatal("test-1's scope did not bind its value")
	}
	if _, ok := mm.AheadVariables().Bind(variables.Binding{Name: "next", Value: "n-live", Recorded: "n-rec", Mock: "own-2", Place: "body.key"}); !ok {
		t.Fatal("the gap's scope did not bind")
	}
	mm.OpenExactTest("test-2", []*models.Mock{own2}, nil)
	if got := mm.Variables().EndedOf("test-1"); len(got) != 1 || got[0].Value != bound.Value || got[0].Mock != bound.Mock {
		t.Fatalf("EndedOf(test-1) once test-2 opened = %+v; want test-1's binding", got)
	}
	if got := mm.Variables().EndedOf("test-2"); len(got) != 0 {
		t.Fatalf("EndedOf(test-2) while it runs = %+v; want none", got)
	}
	for _, test := range []string{"test-1", "test-2", ""} {
		if got := mm.AheadVariables().EndedOf(test); len(got) != 0 {
			t.Fatalf("the gap's scope kept %+v as %q's", got, test)
		}
	}
}
