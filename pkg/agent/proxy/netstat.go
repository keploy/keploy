//go:build linux

package proxy

import (
	"sync/atomic"
)

// Network-I/O sink for the app's wire volume.
//
// The app's true wire volume (rx = app ingress, tx = app egress, pre-dedup) is
// now metered in-kernel by the app network-I/O counter (keploy_ebpf.c
// keploy_netio_bytes, drained by StartKernelNetioDrain) — the userspace TCP_INFO
// accounting that used to read each proxied socket at teardown has been removed in
// favour of the continuous, single-counted kernel counter.
//
// The sink is kept as the dependency-inversion seam: the OSS package can't import
// the enterprise resourceio package, so the enterprise agent installs a sink here
// and the kernel drain feeds it.

// networkIOSink, when set, receives app ingress/egress byte deltas from the kernel
// netio drain. nil ⇒ no accounting (default; the enterprise agent installs it).
var networkIOSink atomic.Pointer[func(rx, tx uint64)]

// SetNetworkIOSink installs the accumulator invoked with the app's ingress/egress
// byte deltas. Safe to call once at startup. Passing nil disables accounting.
func SetNetworkIOSink(sink func(rx, tx uint64)) {
	if sink == nil {
		networkIOSink.Store(nil)
		return
	}
	networkIOSink.Store(&sink)
}
