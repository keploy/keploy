package replay

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func perTest(name string) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.Mongo}
	m.TestModeInfo.Lifetime = models.LifetimePerTest
	return m
}

func configTagged(name string) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.Mongo}
	m.TestModeInfo.Lifetime = models.LifetimeSession
	m.Spec.Metadata = map[string]string{"type": "config"}
	return m
}

func names(ms []*models.Mock) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A mock retagged reusable (config) but still sitting in the per-test pool must
// be moved into the reusable pool so SetMocksWithWindow does not window-filter
// it. Non-reusable mocks stay put; order is preserved in both pools.
func TestRebalanceReusableMocks_MovesRetaggedOnly(t *testing.T) {
	filtered := []*models.Mock{perTest("pt-1"), configTagged("promoted"), perTest("pt-2")}
	unfiltered := []*models.Mock{configTagged("cfg-existing")}

	f, u := rebalanceReusableMocks(filtered, unfiltered, false)

	if got := names(f); !eq(got, []string{"pt-1", "pt-2"}) {
		t.Fatalf("filtered pool = %v, want [pt-1 pt-2]", got)
	}
	if got := names(u); !eq(got, []string{"cfg-existing", "promoted"}) {
		t.Fatalf("unfiltered pool = %v, want [cfg-existing promoted]", got)
	}
}

// Nothing to move: an all-per-test filtered pool is returned unchanged.
func TestRebalanceReusableMocks_NoReusableIsNoop(t *testing.T) {
	filtered := []*models.Mock{perTest("a"), perTest("b")}
	unfiltered := []*models.Mock{}
	f, u := rebalanceReusableMocks(filtered, unfiltered, false)
	if got := names(f); !eq(got, []string{"a", "b"}) {
		t.Fatalf("filtered pool changed unexpectedly: %v", got)
	}
	if len(u) != 0 {
		t.Fatalf("unfiltered pool must stay empty, got %v", names(u))
	}
}

// Empty/nil filtered input is safe and a no-op.
func TestRebalanceReusableMocks_EmptyInput(t *testing.T) {
	f, u := rebalanceReusableMocks(nil, []*models.Mock{configTagged("c")}, false)
	if len(f) != 0 {
		t.Fatalf("filtered must stay empty, got %v", names(f))
	}
	if got := names(u); !eq(got, []string{"c"}) {
		t.Fatalf("unfiltered must be unchanged, got %v", got)
	}
}

// A nil entry in the filtered pool is dropped from the per-test pool (it can
// never match) and never treated as reusable.
func TestRebalanceReusableMocks_NilEntrySafe(t *testing.T) {
	filtered := []*models.Mock{perTest("a"), nil, configTagged("p")}
	f, u := rebalanceReusableMocks(filtered, nil, false)
	if got := names(f); !eq(got, []string{"a"}) {
		t.Fatalf("filtered pool = %v, want [a]", got)
	}
	if got := names(u); !eq(got, []string{"p"}) {
		t.Fatalf("unfiltered pool = %v, want [p]", got)
	}
}

// Mapping-based replay resolves the per-test pool BY NAME from the test<->mock
// mapping, so it never window-filters that pool — the only thing the move
// guards against. Relocating a retagged mock out of it would leave
// LoadByNames with nothing to resolve and the per-test pool EMPTY, which the
// mongo matcher reports as no_mocks before any scoring. The retag is kept; the
// mock must stay where the mapping that names it expects to find it.
func TestRebalanceReusableMocks_MappingBasedKeepsRetaggedInPerTestPool(t *testing.T) {
	filtered := []*models.Mock{perTest("pt-1"), configTagged("promoted"), perTest("pt-2")}
	unfiltered := []*models.Mock{configTagged("cfg-existing")}

	f, u := rebalanceReusableMocks(filtered, unfiltered, true)

	if got := names(f); !eq(got, []string{"pt-1", "promoted", "pt-2"}) {
		t.Fatalf("filtered pool = %v, want [pt-1 promoted pt-2] (no move in mapping mode)", got)
	}
	if got := names(u); !eq(got, []string{"cfg-existing"}) {
		t.Fatalf("unfiltered pool = %v, want [cfg-existing] (nothing added)", got)
	}
}

// The mapping-mode guard must not disturb the strict-window path: the same
// input with mappingBased=false still moves the retagged mock.
func TestRebalanceReusableMocks_StrictWindowStillMoves(t *testing.T) {
	filtered := []*models.Mock{perTest("pt-1"), configTagged("promoted")}
	f, u := rebalanceReusableMocks(filtered, nil, false)
	if got := names(f); !eq(got, []string{"pt-1"}) {
		t.Fatalf("filtered pool = %v, want [pt-1]", got)
	}
	if got := names(u); !eq(got, []string{"promoted"}) {
		t.Fatalf("unfiltered pool = %v, want [promoted]", got)
	}
}
