//go:build linux

package linux

import "testing"

// An orphan is found by the name the kernel keeps, at most 15 bytes: a
// program whose name is longer (k_udp_sock_release, k_ready_connect4) is
// matched by its cut name, never missed.
func TestAnOrphanIsMatchedByTheNameTheKernelKeeps(t *testing.T) {
	for in, want := range map[string]string{
		"k_connect4":         "k_connect4",
		"k_udp_sock_release": "k_udp_sock_rele",
		"k_ready_connect4":   "k_ready_connect",
		"k_ready_connect6":   "k_ready_connect",
	} {
		if got := kernelProgName(in); got != want {
			t.Errorf("kernelProgName(%q) = %q, want %q", in, got, want)
		}
	}
	for name := range keployBPFProgNames {
		if n := kernelProgName(name); len(n) > 15 {
			t.Errorf("%s: kept as %q, longer than the kernel keeps", name, n)
		}
	}
}

// The orphan cleanup's match is by the kernel's cut name: an attached
// k_ready_connect4 is k_ready_connect there, and found.
func TestAnOrphanCleanupMatchesTheCutName(t *testing.T) {
	if !orphanNamed("k_ready_connect", "k_ready_connect4") || !orphanNamed("k_udp_sock_rele", "k_udp_sock_release") || !orphanNamed("k_connect4", "k_connect4") {
		t.Fatal("an orphan attached under its cut name is not matched")
	}
	if orphanNamed("k_connect6", "k_connect4") {
		t.Fatal("another program matched")
	}
}
