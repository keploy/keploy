package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/variables"
	"go.uber.org/zap"
)

// The mock manager keeps one variable table for the replay, a worker's view of
// it reads that table, and the proxy's reset forgets what it holds.
func TestTheVariableTableIsTheManagersThroughEveryView(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	var db integrations.MockMemDb = mm
	vs, ok := db.(integrations.VariableStore)
	if !ok {
		t.Fatal("the mock manager does not keep the variables")
	}
	vs.Variables().Bind(variables.Binding{Name: "orderId", Value: "o1"})

	view := &scopedMockDb{MockMemDb: mm}
	var wrapped integrations.MockMemDb = view
	wvs, ok := wrapped.(integrations.VariableStore)
	if !ok {
		t.Fatal("a worker's view drops the variable table")
	}
	if b, bound := wvs.Variables().Lookup("orderId"); !bound || b.Value != "o1" {
		t.Fatalf("the view reads %+v (%v)", b, bound)
	}

	p := &Proxy{logger: zap.NewNop(), mockManager: mm}
	p.ResetVariables()
	if _, bound := mm.Variables().Lookup("orderId"); bound {
		t.Fatal("the proxy's reset kept orderId")
	}
}
