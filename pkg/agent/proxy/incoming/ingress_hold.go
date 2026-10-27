package proxy

import (
	"context"
	"net"
	"strconv"

	"go.uber.org/zap"
)

// IngressPortHolder is the hooks, when they hold the application's moved ports
// from inside their bind with a BPF sk_lookup program (pkg/agent/hooks/linux,
// ingress_lookup.c). The manager type-asserts the hooks for it: hooks that do
// not implement it, or hold nothing on this kernel, leave it nil, and the
// ingress forwarder covers the window as it did before.
//
// While recording, the bind hooks move the app's bind of port P to P' inside
// bind(2). Until keploy's forwarder listens on P, the lookup sends a call to
// P to one park listener the hooks opened before the app started. The park
// keeps the call's original destination as its local address, so the manager
// reads P from it, finds P' from the hooks, and serves it exactly as the
// forwarder would — the call is recorded instead of refused.
type IngressPortHolder interface {
	// IngressParkListener is the park listener, or nil when nothing is held.
	IngressParkListener() net.Listener
	// MovedPort is the port the app's bind of orig was moved to, and whether
	// it is known.
	MovedPort(orig uint16) (uint16, bool)
	// RegisterIngressForwarder steers orig's connections to ln once it
	// listens, instead of to the park.
	RegisterIngressForwarder(orig uint16, ln net.Listener)
	// UnregisterIngressForwarder stops steering orig to a forwarder.
	UnregisterIngressForwarder(orig uint16)
	// IngressHeld reports whether the moved ports are held.
	IngressHeld() bool
	// IngressLookupCounts is what the lookup did with the listener lookups
	// for a moved port, by name, or nil when nothing is held: lookups, not
	// connections (a retransmitted SYN is looked up again), for Debug.
	IngressLookupCounts() map[string]uint64
}

// ingressPark is the hold's park listener, or nil when the hooks hold nothing.
func (pm *IngressProxyManager) ingressPark() net.Listener {
	if pm.portHolder == nil {
		return nil
	}
	return pm.portHolder.IngressParkListener()
}

// ingressHeld reports whether the hooks hold the application's moved ports.
func (pm *IngressProxyManager) ingressHeld() bool {
	return pm.portHolder != nil && pm.portHolder.IngressHeld()
}

func (pm *IngressProxyManager) registerIngressForwarder(orig uint16, ln net.Listener) {
	if pm.portHolder != nil {
		pm.portHolder.RegisterIngressForwarder(orig, ln)
	}
}

func (pm *IngressProxyManager) unregisterIngressForwarder(orig uint16) {
	if pm.portHolder != nil {
		pm.portHolder.UnregisterIngressForwarder(orig)
	}
}

// IngressParked is how many calls the hold's park served before a forwarder
// took over, for the run's capture summary: the manager's own count of the
// parked connections it forwarded (the lookup's counts are of lookups, which
// a retransmitted SYN repeats); 0 when nothing is held.
func (pm *IngressProxyManager) IngressParked() uint64 {
	return pm.parkedServed.Load()
}

// logIngressHoldEnd says, when recording ends, how many calls the hold
// served (Info, when any), and what the lookup did (Debug).
func (pm *IngressProxyManager) logIngressHoldEnd() {
	if parked := pm.IngressParked(); parked > 0 {
		pm.logger.Info("keploy served calls to the application's port that arrived before its forwarder listened, by holding the port from the application's bind",
			zap.Uint64("count", parked))
	}
	if pm.portHolder == nil {
		return
	}
	if counts := pm.portHolder.IngressLookupCounts(); counts != nil {
		pm.logger.Debug("the ingress hold's listener lookups for the application's moved ports", zap.Any("lookups", counts))
	}
}

// portSem is the synchronous mode's one-call-at-a-time semaphore of the
// application's port orig, which its forwarder and the park share: a call
// the park serves and one the forwarder accepts for the same port are taken
// in turn, as two of the forwarder's own are.
func (pm *IngressProxyManager) portSem(orig uint16) chan struct{} {
	pm.semMu.Lock()
	defer pm.semMu.Unlock()
	if pm.portSems == nil {
		pm.portSems = map[uint16]chan struct{}{}
	}
	sem, ok := pm.portSems[orig]
	if !ok {
		sem = make(chan struct{}, 1)
		pm.portSems[orig] = sem
	}
	return sem
}

// serveParkedConnections serves what the hold's park accepts: a call to a
// moved port that arrived before keploy's forwarder for it listened. Each
// connection's local address is the port the caller dialed (the lookup kept
// the original destination), so it is forwarded to that port's moved
// destination and recorded, the same way the forwarder's own accepts are.
func (pm *IngressProxyManager) serveParkedConnections(ctx context.Context, park net.Listener) {
	go func() {
		<-ctx.Done()
		_ = park.Close()
	}()
	for {
		conn, err := park.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
			default:
				pm.logger.Debug("ingress park accept loop stopped", zap.Error(err))
			}
			return
		}
		go pm.serveParked(ctx, conn)
	}
}

func (pm *IngressProxyManager) serveParked(ctx context.Context, conn net.Conn) {
	ta, ok := conn.LocalAddr().(*net.TCPAddr)
	if !ok {
		_ = conn.Close()
		return
	}
	orig := uint16(ta.Port)
	moved, known := pm.portHolder.MovedPort(orig)
	if !known {
		// The app's bind of this port is not in app_port_info (it was
		// unmoved, or has gone): nothing to forward it to.
		pm.logger.Debug("a parked ingress connection names a port keploy did not move; closing it", zap.Uint16("orig_port", orig))
		_ = conn.Close()
		return
	}
	pm.parkedServed.Add(1)
	pm.logger.Debug("serving a call to the app's port before keploy's forwarder listened (held from the app's bind)",
		zap.Uint16("orig_port", orig), zap.Uint16("new_port", moved))
	pm.handleConnection(ctx, conn, "127.0.0.1:"+strconv.Itoa(int(moved)), pm.logger, pm.tcChan, pm.portSem(orig), orig)
}
