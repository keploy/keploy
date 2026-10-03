package proxy

import (
	"context"
	"net"
	"strconv"
	"time"

	"go.uber.org/zap"
)

// getActualDestination returns the address to forward an accepted ingress
// connection to.
//
// Every backend now tells Keploy where the application moved to — Linux and
// macOS through their bind hooks, Windows through the shim's — so the target is
// always known by the time a connection arrives and the fallback IS the answer.
//
// Windows used to be the exception: its kernel packet filter left the
// application on its advertised port and rewrote inbound packets, so the real
// destination had to be recovered per connection from the source port. That
// backend is gone, and with it the only reason this was ever platform-specific.
func (pm *IngressProxyManager) getActualDestination(_ context.Context, _ net.Conn, fallbackAddr string, _ *zap.Logger) string {
	return fallbackAddr
}

// appDialTimeout is the budget for one upstream connect attempt.
const appDialTimeout = 3 * time.Second

// appAddrProbeTimeout is deliberately short: this probes addresses on the
// pod's own loopback-adjacent interfaces, where a listening socket answers
// immediately and a closed one is refused immediately.
const appAddrProbeTimeout = 250 * time.Millisecond

// dialApp connects to the application behind the ingress forwarder.
//
// The bind hook reports only the PORT the application was moved to — it
// rewrites user_port and never touches the address — so the forwarder assumes
// the application is reachable on loopback. That holds for an application
// bound to 0.0.0.0 or to 127.0.0.1, which is almost all of them.
//
// It does not hold for an application that binds ONE address, which is what an
// app does when it is told to advertise itself: a $POD_IP bind is common in
// anything that registers its own address, and several charts template exactly
// that. Such an application keeps listening on <podIP>:<newPort> while the
// forwarder dials 127.0.0.1:<newPort>, so every connection is refused. The
// result is not a crash — the application logs a clean start and keeps running
// — but the forwarder resets every inbound connection, the readiness probe
// never passes, and the pod is dropped from its Service for the length of the
// recording. The log even blames the application for not listening, while it
// is listening perfectly well on an address nobody dialled.
//
// So: try the assumption first, and only when it is refused, find out where the
// application actually is. The happy path pays nothing — no probe, no
// enumeration, one dial exactly as before.
func (pm *IngressProxyManager) dialApp(addr string, logger *zap.Logger) (net.Conn, error) {
	// A previously resolved non-loopback address wins outright: once an
	// application has been found somewhere else, every later connection for
	// that port belongs there.
	if resolved, ok := pm.cachedAppAddr(addr); ok {
		if conn, err := net.DialTimeout("tcp", resolved, appDialTimeout); err == nil {
			return conn, nil
		}
		// The application moved or went away. Drop the cache so the next
		// attempt starts from the assumption again rather than retrying a
		// stale address forever.
		pm.forgetAppAddr(addr)
	}

	conn, err := net.DialTimeout("tcp", addr, appDialTimeout)
	if err == nil {
		return conn, nil
	}

	alt, found := pm.resolveAppAddr(addr)
	if !found {
		// Nothing else is listening on this port either. Return the ORIGINAL
		// error: it names the address the caller asked for, which is what the
		// existing log lines and tests expect.
		return nil, err
	}

	altConn, altErr := net.DialTimeout("tcp", alt, appDialTimeout)
	if altErr != nil {
		return nil, err
	}
	pm.rememberAppAddr(addr, alt)
	if logger != nil {
		// Info, not Debug: this is a real and surprising property of the
		// recorded application, it is logged once per port rather than per
		// connection, and without it the only trace of the retry is that
		// things mysteriously work.
		logger.Info("application is not listening on loopback; forwarding ingress to the address it actually bound",
			zap.String("assumed", addr), zap.String("actual", alt))
	}
	return altConn, nil
}

// resolveAppAddr looks for the port of addr on the pod's other local
// addresses, returning the first that accepts a connection.
//
// Enumerating interfaces is correct here precisely because the agent shares the
// application's network namespace: the addresses it can see ARE the addresses
// the application could have bound. Loopback is skipped — it is the assumption
// that just failed.
func (pm *IngressProxyManager) resolveAppAddr(addr string) (string, bool) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", false
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", false
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP == nil || ipNet.IP.IsLoopback() || !ipNet.IP.IsGlobalUnicast() {
			continue
		}
		// net.JoinHostPort brackets an IPv6 literal, which the v4-only dial
		// elsewhere in this package cannot do — both families are reachable
		// from here because dialApp dials "tcp", not "tcp4".
		candidate := net.JoinHostPort(ipNet.IP.String(), port)
		conn, err := net.DialTimeout("tcp", candidate, appAddrProbeTimeout)
		if err != nil {
			continue
		}
		_ = conn.Close()
		return candidate, true
	}
	return "", false
}

// portOf is the cache key: the application's relocated port. Keying on the port
// rather than the whole address means a restart that lands the application on a
// new port gets a fresh lookup for free, since the bind hook emits a new port.
func portOf(addr string) (uint16, bool) {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil {
		return 0, false
	}
	return uint16(n), true
}

func (pm *IngressProxyManager) cachedAppAddr(addr string) (string, bool) {
	port, ok := portOf(addr)
	if !ok {
		return "", false
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	resolved, ok := pm.appAddr[port]
	return resolved, ok
}

func (pm *IngressProxyManager) rememberAppAddr(addr, resolved string) {
	port, ok := portOf(addr)
	if !ok {
		return
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.appAddr == nil {
		pm.appAddr = make(map[uint16]string)
	}
	pm.appAddr[port] = resolved
}

func (pm *IngressProxyManager) forgetAppAddr(addr string) {
	port, ok := portOf(addr)
	if !ok {
		return
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	delete(pm.appAddr, port)
}
