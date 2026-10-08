package replay

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func mockStateNameSet(ms []models.MockState) map[string]bool {
	out := make(map[string]bool, len(ms))
	for _, m := range ms {
		out[m.Name] = true
	}
	return out
}

// A per-test single-use mock consumed BEFORE the first test fired must be
// RE-ARMED, not folded into the startup section. The classic case: the
// app-readiness gate (waitForAppReady) polls a /health endpoint that runs
// SELECT 1 against a mocked DB, so the proxy serves and DeleteFilteredMock
// permanently consumes a per-test mock that belongs to a recorded test case
// (e.g. the recorded get-health test's own SELECT 1). Marking it consumed here
// strips it from every later per-test filterOutDeleted, so its owning test
// fails with no_mocks. Reusable/session boot traffic (handshakes, auth,
// pool warm-up) must still be recorded as startup.
func TestPartitionInitialConsumed_ReArmsPerTestGateMocks(t *testing.T) {
	consumed := []models.MockState{
		// The readiness gate's SELECT 1 -- a per-test mock owned by get-health.
		{Name: "select-1", Usage: models.Deleted, Lifetime: models.LifetimePerTest},
		// Genuine boot traffic -- must stay in the startup section.
		{Name: "mysql-handshake", Usage: models.Deleted, Lifetime: models.LifetimeSession},
		{Name: "conn-warmup", Usage: models.Deleted, Lifetime: models.LifetimeConnection},
		{Name: "config-probe", Usage: models.Deleted, Type: "config"},
		{Name: "connection-typed", Usage: models.Deleted, Type: "connection"},
	}

	startup, rearm := partitionInitialConsumed(consumed)

	startupNames := mockStateNameSet(startup)
	rearmNames := mockStateNameSet(rearm)

	// Only the per-test gate mock is re-armed (withheld from consumed accounting).
	if len(rearm) != 1 || !rearmNames["select-1"] {
		t.Fatalf("expected exactly the per-test gate mock 'select-1' re-armed, got %v", startupOrRearmNames(rearm))
	}
	// Every reusable/session/connection/config mock stays in the startup section.
	for _, n := range []string{"mysql-handshake", "conn-warmup", "config-probe", "connection-typed"} {
		if !startupNames[n] {
			t.Fatalf("reusable boot mock %q must remain in the startup section, got %v", n, startupOrRearmNames(startup))
		}
	}
	// ...and the per-test mock must NOT leak into startup.
	if startupNames["select-1"] {
		t.Fatalf("per-test gate mock must NOT be recorded as startup")
	}
}

// A drain with only reusable traffic re-arms nothing -- byte-identical to the
// pre-fix behaviour, so the empty-mappings.yaml startup fix is preserved.
func TestPartitionInitialConsumed_AllReusableReArmsNothing(t *testing.T) {
	consumed := []models.MockState{
		{Name: "s1", Lifetime: models.LifetimeSession},
		{Name: "c1", Lifetime: models.LifetimeConnection},
	}
	startup, rearm := partitionInitialConsumed(consumed)
	if len(rearm) != 0 {
		t.Fatalf("no per-test mocks -> nothing to re-arm, got %v", startupOrRearmNames(rearm))
	}
	if len(startup) != 2 {
		t.Fatalf("both reusable mocks must be startup, got %d", len(startup))
	}
}

// An empty drain is a no-op in both partitions.
func TestPartitionInitialConsumed_Empty(t *testing.T) {
	startup, rearm := partitionInitialConsumed(nil)
	if len(startup) != 0 || len(rearm) != 0 {
		t.Fatalf("empty input must yield empty partitions, got startup=%d rearm=%d", len(startup), len(rearm))
	}
}

// Locks the classifier contract at the boundaries: a zero-value MockState
// (Lifetime == LifetimePerTest, empty Type) is the safe default and must be
// re-armed, while the Type branch ("config"/"connection") tiers a mock as
// reusable even when its Lifetime is the per-test zero value.
func TestPartitionInitialConsumed_TierEdgeCases(t *testing.T) {
	cases := []struct {
		name      string
		state     models.MockState
		wantRearm bool
	}{
		{"zero-value defaults to per-test (re-armed)", models.MockState{Name: "zero"}, true},
		{"explicit per-test re-armed", models.MockState{Name: "pt", Lifetime: models.LifetimePerTest}, true},
		{"session is startup", models.MockState{Name: "sess", Lifetime: models.LifetimeSession}, false},
		{"connection is startup", models.MockState{Name: "conn", Lifetime: models.LifetimeConnection}, false},
		{"config type is startup despite per-test lifetime", models.MockState{Name: "cfg", Type: "config"}, false},
		{"connection type is startup despite per-test lifetime", models.MockState{Name: "ct", Type: "connection"}, false},
		{"DNS per-test mock is re-armed", models.MockState{Name: "dns", Kind: models.Kind("DNS")}, true},
	}
	for _, tc := range cases {
		startup, rearm := partitionInitialConsumed([]models.MockState{tc.state})
		gotRearm := len(rearm) == 1 && len(startup) == 0
		gotStartup := len(startup) == 1 && len(rearm) == 0
		if tc.wantRearm && !gotRearm {
			t.Errorf("%s: expected re-armed, got startup=%v rearm=%v", tc.name, startupOrRearmNames(startup), startupOrRearmNames(rearm))
		}
		if !tc.wantRearm && !gotStartup {
			t.Errorf("%s: expected startup, got startup=%v rearm=%v", tc.name, startupOrRearmNames(startup), startupOrRearmNames(rearm))
		}
	}
}

func startupOrRearmNames(ms []models.MockState) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}
