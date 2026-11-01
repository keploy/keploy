package proxy

import (
	"context"
	"net"
	"time"
)

// appdial.go: how the ingress reaches the application it forwards a call to.
//
// The ingress dials the app on the loopback, where a dial to a listener that
// exists completes in the kernel however loaded the host is: a full accept
// queue only delays it until the app accepts. So nothing but the call's
// context ends a dial. A deadline could only fail a call the app would have
// taken; the one it had did worse. It retried a refusal for 3 s and made its
// last try after that deadline, on 1 ms, so on a loaded host a refusal came
// back as "i/o timeout": the probe a lane sent as the app started, served by
// keploy's hold, reached its dial only after the recording had stopped the
// app, and the refusal the stop explains (logAppDialFailure, at DEBUG while
// stopping) was an ERROR instead.
//
// Whether the app listens is answered by its listener, not by a time. Until
// a dial to an address has connected, a refusal there means the app has not
// listened yet (keploy's hold serves a call from the app's bind, before its
// listen): the dial waits for the listener, trying again after each refusal,
// for as long as the call's context lasts. Once one has connected, a refusal
// is final: the app closed its listener, as it does when the recording that
// started it stops it, and the dial returns it at once, as the refusal it is.

// dialApp dials the application at addr for a call the ingress serves.
func (pm *IngressProxyManager) dialApp(ctx context.Context, addr string) (net.Conn, error) {
	network := "tcp4"
	if host, _, err := net.SplitHostPort(addr); err == nil {
		if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
			network = "tcp6"
		}
	}
	var d net.Dialer
	for {
		conn, err := d.DialContext(ctx, network, addr)
		if err == nil {
			pm.listened.Store(addr, struct{}{})
			return conn, nil
		}
		// isConnRefused is platform-aware: on Windows a refused connect is
		// WSAECONNREFUSED, not the POSIX ECONNREFUSED.
		if !isConnRefused(err) || pm.hasListened(addr) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, err // the refusal, not the context's end
		case <-time.After(ingressTargetPollInterval):
		}
	}
}

// hasListened reports whether a dial to addr has connected since the ingress
// last forgot it (forgetListened).
func (pm *IngressProxyManager) hasListened(addr string) bool {
	_, ok := pm.listened.Load(addr)
	return ok
}

// forgetListened forgets that the app listened at addr, for a bind that moved
// an app there again: the listener a dial then waits for is the new bind's.
func (pm *IngressProxyManager) forgetListened(addr string) { pm.listened.Delete(addr) }

// forgetAllListened forgets every address the app listened at, as the ingress
// stops: the next recording's apps listen anew.
func (pm *IngressProxyManager) forgetAllListened() { pm.listened.Clear() }
