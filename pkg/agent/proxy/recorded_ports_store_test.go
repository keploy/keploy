package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/internal/testnet"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// a later ON-DISK store's seed replaces the earlier one (not only a store with no on-disk store).
func TestALaterOnDiskStoreReplacesTheSeed(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	recorded.forget()
	t.Cleanup(recorded.forget)
	p1, k1 := testnet.RefusedAddr(t, "tcp"), testnet.RefusedAddr(t, "tcp")
	p2, k2 := testnet.RefusedAddr(t, "tcp"), testnet.RefusedAddr(t, "tcp")
	first, second := localPortsStore(t, p1, k1), localPortsStore(t, p2, k2)
	p := selfCallProxy()
	seed1, err := first.LoadDestinations()
	if err != nil {
		t.Fatal(err)
	}
	p.SeedRecordedDestinations(seed1)
	if !RecordedPort(uint32(mustPort(t, p1))) {
		t.Fatal("first store did not seed its port")
	}
	seed2, err := second.LoadDestinations()
	if err != nil {
		t.Fatal(err)
	}
	p.SeedRecordedDestinations(seed2)
	if RecordedPort(uint32(mustPort(t, p1))) {
		t.Fatal("a later on-disk store kept the earlier store's in-test-only port")
	}
	if !RecordedPort(uint32(mustPort(t, p2))) {
		t.Fatal("the later store did not seed its port")
	}
}
