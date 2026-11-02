//go:build !linux

package testnet

import "testing"

// RefusedAddr's address, held by a connection here, stays its own while the
// test runs and refuses a dial: a TCP connect, a UDP datagram (requireHeld
// and requireUDPHeld say how, per platform).
func TestARefusedAddressRefusesADialForTheTestsLife(t *testing.T) {
	t.Run("tcp", func(t *testing.T) {
		addr := RefusedAddr(t, "tcp")
		requireHeld(t, addr)
		requireDialRefused(t, addr)
	})
	t.Run("udp", func(t *testing.T) {
		requireUDPHeld(t, RefusedAddr(t, "udp"))
	})
}
