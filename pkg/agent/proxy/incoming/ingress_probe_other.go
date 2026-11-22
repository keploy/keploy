//go:build !linux

package proxy

import "net/netip"

// probeIPv6Port is the hold's IPv6 probe (ingress_probe_linux.go); the hold
// is Linux-only, so elsewhere nothing is held and there is nothing to probe.
func probeIPv6Port(netip.Addr, uint16) error { return nil }
