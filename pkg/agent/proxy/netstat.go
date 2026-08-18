//go:build linux

package proxy

import (
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

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
// Long-lived connections (DB/connection-pool sockets, keep-alive, gRPC streams)
// stay open for the whole recording, so accounting ONLY at teardown would miss the
// bulk of their bytes — exactly where an app's heaviest traffic (e.g. large DB
// result sets) lives. To close that gap, a connection is registered with
// TrackConnNetworkIO at setup and a background sampler folds each interval's byte
// DELTA into the sink while it's still open; RecordConnNetworkIO takes the final
// delta at teardown. Connections that are never tracked keep the original
// teardown-only behavior (their full lifetime total, once, at close).

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

// RecordConnNetworkIO takes the FINAL byte reading off an app-facing socket at
// teardown and forwards it to the sink. Best-effort and cheap (one getsockopt);
// a non-TCP conn or an unreadable fd is silently skipped. Call once per connection
// just before closing it.
//
// If the connection was registered with TrackConnNetworkIO, only the DELTA since
// the last periodic sample is forwarded (the sampler already counted the rest) and
// the registry entry is removed. If it was never tracked, the connection's full
// lifetime total is forwarded — preserving the original teardown-only behavior.
//
// Pass the socket that keploy owns facing the APP — the outgoing proxy's srcConn
// (app dials keploy) or the ingress proxy's upConn (keploy dials the app). In both
// cases keploy is the app's peer, so the direction mapping is identical:
//   - Bytes_received = bytes keploy RECEIVED on this socket = app → keploy = the
//     app's EGRESS.  → tx
//   - Bytes_acked    = bytes keploy SENT (and were acked) = keploy → app = the
//     app's INGRESS. → rx
func RecordConnNetworkIO(conn net.Conn) {
	sinkP := networkIOSink.Load()
	if sinkP == nil || conn == nil {
		return
	}
	rx, tx, ok := readSocketBytes(conn)

	// Always drop any registry entry for this conn (it's closing), capturing its
	// last sampled cursor so we can emit only the un-sampled tail.
	liveConnsMu.Lock()
	tc, tracked := liveConns[conn]
	if tracked {
		delete(liveConns, conn)
	}
	liveConnsMu.Unlock()

	if !ok {
		// Can't read final totals (fd already closed). For a tracked conn the
		// periodic sampler already counted the bulk; only the sub-interval tail is
		// lost. For an untracked conn nothing is recorded (unchanged from before).
		return
	}

	var drx, dtx uint64
	if tracked {
		drx = forwardDelta(rx, tc.lastRx)
		dtx = forwardDelta(tx, tc.lastTx)
	} else {
		drx, dtx = rx, tx // never tracked → full lifetime total (original behavior)
	}
	if drx == 0 && dtx == 0 {
		return
	}
	(*sinkP)(drx, dtx)
}

// netioSampleInterval is how often the background sampler reads live tracked
// connections' TCP_INFO and folds the byte delta into the sink. Matches the
// proxyless eBPF drain cadence.
const netioSampleInterval = 15 * time.Second

// trackedConn holds a live app-facing socket and the last cumulative (rx, tx) the
// sampler observed on it, so each tick emits only the new bytes.
type trackedConn struct {
	conn   net.Conn
	lastRx uint64
	lastTx uint64
}

var (
	liveConnsMu sync.Mutex
	liveConns   = map[net.Conn]*trackedConn{}
	samplerOnce sync.Once
)

// TrackConnNetworkIO registers an app-facing socket for continuous network-I/O
// accounting. Call once when the connection is established, passing the SAME
// (raw) conn later handed to RecordConnNetworkIO at teardown. The background
// sampler then folds each interval's byte delta into the sink for as long as the
// connection stays open, so long-lived connections are metered throughout their
// life rather than only at close. No-op when no sink is installed or the conn
// isn't a readable TCP socket.
func TrackConnNetworkIO(conn net.Conn) {
	if networkIOSink.Load() == nil || conn == nil {
		return
	}
	// Only TCP sockets expose TCP_INFO; skip anything else so the registry and the
	// sampler stay cheap and never chase un-readable fds.
	if _, _, ok := readSocketBytes(conn); !ok {
		return
	}
	startNetioSampler()
	liveConnsMu.Lock()
	if _, exists := liveConns[conn]; !exists {
		// lastRx/lastTx start at 0: we register at connection setup (before traffic),
		// so the first delta correctly counts from zero.
		liveConns[conn] = &trackedConn{conn: conn}
	}
	liveConnsMu.Unlock()
}

// startNetioSampler launches (once) the daemon goroutine that periodically drains
// live tracked connections. Process-lifetime; there is nothing to stop since the
// agent process is torn down with the pod.
func startNetioSampler() {
	samplerOnce.Do(func() {
		go func() {
			t := time.NewTicker(netioSampleInterval)
			defer t.Stop()
			for range t.C {
				sampleLiveConns()
			}
		}()
	})
}

// sampleLiveConns reads each tracked connection's current TCP_INFO, forwards the
// delta since its last reading to the sink, and advances the stored cursor. A
// connection whose fd is momentarily unreadable is left in place (teardown removes
// it and takes the final delta); a counter that went backwards contributes its
// full current value (forwardDelta guard). Deltas — not absolutes — so a
// connection is never double counted across ticks or against its teardown read.
func sampleLiveConns() {
	sinkP := networkIOSink.Load()
	if sinkP == nil {
		return
	}
	liveConnsMu.Lock()
	snapshot := make([]*trackedConn, 0, len(liveConns))
	for _, tc := range liveConns {
		snapshot = append(snapshot, tc)
	}
	liveConnsMu.Unlock()

	for _, tc := range snapshot {
		rx, tx, ok := readSocketBytes(tc.conn)
		if !ok {
			continue
		}
		liveConnsMu.Lock()
		if _, still := liveConns[tc.conn]; !still {
			liveConnsMu.Unlock() // teardown removed it between snapshot and now
			continue
		}
		drx := forwardDelta(rx, tc.lastRx)
		dtx := forwardDelta(tx, tc.lastTx)
		tc.lastRx, tc.lastTx = rx, tx
		liveConnsMu.Unlock()
		if drx > 0 || dtx > 0 {
			(*sinkP)(drx, dtx)
		}
	}
}

// forwardDelta returns cur-prev for a monotonic counter, or the full cur when the
// counter went backwards (fd/id reuse), so a reset never subtracts.
func forwardDelta(cur, prev uint64) uint64 {
	if cur >= prev {
		return cur - prev
	}
	return cur
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
