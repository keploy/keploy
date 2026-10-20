package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

var (
	_ integrations.PoolSlice = (*MockManager)(nil)
	_ integrations.PoolSlice = (*scopedMockDb)(nil)
)

// The learn step asks the store whose a pool mock is by the claim key (integrations.PoolSlice): while no
// test is open, the key's slice served (ServeExactPool) says it; a test open, or exact serving off, says
// nothing (known false), and the learn step decides as before.
func TestMockManagerInPoolSliceIsTheClaimKeysServedSlice(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	if _, known := mm.InPoolSlice("pool-0"); known {
		t.Fatal("exact serving off: known")
	}
	mm.ServeExactPool("e2e/roles.spec.ts", []string{"pool-0"})
	if in, known := mm.InPoolSlice("pool-0"); !known || !in {
		t.Fatalf("the key's slice: in %v known %v", in, known)
	}
	if in, known := mm.InPoolSlice("pool-9"); !known || in {
		t.Fatalf("another key's mock: in %v known %v", in, known)
	}
	mm.ServeExactTest("t1", []*models.Mock{{Name: "mock-1", Kind: models.HTTP}})
	if _, known := mm.InPoolSlice("pool-0"); known {
		t.Fatal("a test open: known")
	}
}
