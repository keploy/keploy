//go:build linux

package proxy

import (
	"net"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
)

// Kernel-sourced network I/O accounting for the PROXY capture path.
//
// The proxy owns the app-facing socket (srcConn), so the kernel's own per-socket
// byte counters (TCP_INFO) are the authoritative measure of how much traffic the
// app actually moved on that connection — the TRUE wire volume, BEFORE the capture
// pipeline dedups/samples/drops artifacts. This is exactly what a per-captured-
// artifact tally cannot see (and what the eBPF cgroup counter does for proxyless).
//
// We read TCP_INFO once at connection teardown (each connection's counters are
// cumulative for its lifetime), and feed the totals to a sink the enterprise agent
// wires to its resource snapshot. The OSS package can't import the enterprise
// resourceio package, so the dependency is inverted via a sink — mirroring the
// existing captured-IO sink pattern.
//
// NOTE (v1 scope): reporting at teardown means a connection that stays open for the
// whole recording won't contribute until it closes. Long-lived connections would
// want periodic delta sampling of the live sockets; left as a follow-up.

// networkIOSink, when set, receives a connection's app-relative ingress/egress
// byte totals (rx = bytes the app received, tx = bytes the app sent) at teardown.
// nil ⇒ no accounting (default; the enterprise agent installs it).
var networkIOSink atomic.Pointer[func(rx, tx uint64)]

// SetNetworkIOSink installs the accumulator invoked with each proxied connection's
// app ingress/egress byte totals (from TCP_INFO). Safe to call once at startup.
// Passing nil disables accounting.
func SetNetworkIOSink(sink func(rx, tx uint64)) {
	if sink == nil {
		networkIOSink.Store(nil)
		return
	}
	networkIOSink.Store(&sink)
}

// RecordConnNetworkIO reads the kernel TCP_INFO byte counters off an app-facing
// socket and forwards them to the sink. Best-effort and cheap (one getsockopt);
// a non-TCP conn or an unreadable fd is silently skipped. Call once per connection
// just before closing it.
//
// Pass the socket that keploy owns facing the APP — the outgoing proxy's srcConn
// (app dials keploy) or the ingress proxy's upConn (keploy dials the app). In both
// cases keploy is the app's peer, so the direction mapping is identical:
//   - Bytes_received = bytes keploy RECEIVED on this socket = app → keploy = the
//     app's EGRESS.  → tx
//   - Bytes_acked    = bytes keploy SENT (and were acked) = keploy → app = the
//     app's INGRESS. → rx
func RecordConnNetworkIO(conn net.Conn) {
	// When the in-kernel netio counter is live it is authoritative for the app's
	// wire volume, so the userspace TCP_INFO path stands down to avoid double
	// counting the same traffic. See StartKernelNetioDrain.
	if kernelNetioActive.Load() {
		return
	}
	sinkP := networkIOSink.Load()
	if sinkP == nil || conn == nil {
		return
	}
	rx, tx, ok := readSocketBytes(conn)
	if !ok || (rx == 0 && tx == 0) {
		return
	}
	(*sinkP)(rx, tx)
}

// readSocketBytes returns the app-relative (rx, tx) cumulative byte totals for a
// TCP connection from the kernel's TCP_INFO. ok is false for a non-TCP conn, a
// conn without a raw syscall handle, or a getsockopt failure.
func readSocketBytes(conn net.Conn) (rx, tx uint64, ok bool) {
	sc, err := rawConn(conn)
	if err != nil || sc == nil {
		return 0, 0, false
	}
	var (
		info    *unix.TCPInfo
		infoErr error
	)
	ctrlErr := sc.Control(func(fd uintptr) {
		info, infoErr = unix.GetsockoptTCPInfo(int(fd), unix.SOL_TCP, unix.TCP_INFO)
	})
	if ctrlErr != nil || infoErr != nil || info == nil {
		return 0, 0, false
	}
	// See direction note above: acked = app ingress (rx), received = app egress (tx).
	return info.Bytes_acked, info.Bytes_received, true
}

// rawConn unwraps a net.Conn to its SyscallConn, tolerating the util wrappers the
// proxy layers over the raw socket. Only *net.TCPConn (or a wrapper exposing
// SyscallConn) yields a usable handle.
func rawConn(conn net.Conn) (syscall.RawConn, error) {
	type syscallConner interface {
		SyscallConn() (syscall.RawConn, error)
	}
	if sc, ok := conn.(syscallConner); ok {
		return sc.SyscallConn()
	}
	return nil, nil
}
