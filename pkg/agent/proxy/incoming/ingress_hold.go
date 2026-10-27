package proxy

import (
	"context"
	"errors"
	"net"
	"strconv"
	"syscall"
	"time"

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
	// YieldIngressPort stops holding orig, whose forwarder could not listen
	// there because another listener holds it: the port's calls are the
	// kernel's (that listener's), as without the hold, until a forwarder for
	// orig is registered.
	YieldIngressPort(orig uint16)
	// StopIngressPark takes the park out of the lookup and closes it: a call
	// to a moved port that no forwarder takes is then the kernel's (refused),
	// as without the hold.
	StopIngressPark()
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

func (pm *IngressProxyManager) yieldIngressPort(orig uint16) {
	if pm.portHolder != nil {
		pm.portHolder.YieldIngressPort(orig)
	}
}

// IngressParked is how many calls the hold's park served: the manager's own
// count of the parked connections it forwarded (the lookup's counts are of
// lookups, which a retransmitted SYN repeats); 0 when nothing is held.
func (pm *IngressProxyManager) IngressParked() uint64 {
	return pm.parkedServed.Load() + pm.parkedServedV6.Load()
}

// logIngressHoldEnd says, when recording ends, how many calls the hold
// served (Info, when any), and what the lookup did (Debug). IPv4 calls reach
// the park only before the port's forwarder listens (it then takes them);
// IPv6 ones (an app bound to :: takes both families) for the whole run, as
// keploy's forwarder listens on IPv4 only: without the hold they were
// refused. So they are counted apart.
func (pm *IngressProxyManager) logIngressHoldEnd() {
	if parked := pm.parkedServed.Load(); parked > 0 {
		pm.logger.Info("keploy served calls to the application's port that arrived before its forwarder listened, by holding the port from the application's bind",
			zap.Uint64("count", parked))
	}
	if parked := pm.parkedServedV6.Load(); parked > 0 {
		pm.logger.Info("keploy served IPv6 calls to the application's port, which its IPv4 forwarder cannot take, by holding the port from the application's bind",
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
// moved port that arrived before keploy's forwarder for it listened (or an
// IPv6 one, which the IPv4 forwarder cannot take). Each connection's local
// address is the port the caller dialed (the lookup kept the original
// destination), so it is forwarded to that port's moved destination and
// recorded, the same way the forwarder's own accepts are.
//
// The park must never be left steered to and not accepting: the lookup would
// still give it calls, which complete their handshake and are never served,
// so their callers hang. An Accept that fails for want of a resource
// (EMFILE, ENFILE, ENOBUFS, ENOMEM) is retried after a pause that grows from
// 5 ms to 1 s, as net/http's server does, with one WARN; any other failure
// stops the park (StopIngressPark): its calls are then the kernel's, refused,
// as without the hold.
func (pm *IngressProxyManager) serveParkedConnections(ctx context.Context, park net.Listener) {
	go func() {
		<-ctx.Done()
		_ = park.Close()
	}()
	var pause time.Duration
	warned := false
	for {
		conn, err := park.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if parkAcceptCanRecover(err) {
				pause *= 2
				if pause == 0 {
					pause = 5 * time.Millisecond
				}
				if pause > time.Second {
					pause = time.Second
				}
				if !warned {
					warned = true
					pm.logger.Warn("keploy's ingress hold could not accept a call to the application's port for want of a resource; it retries, and the call waits until then",
						zap.Error(err))
				}
				t := time.NewTimer(pause)
				select {
				case <-t.C:
				case <-ctx.Done():
					t.Stop()
					return
				}
				continue
			}
			pm.portHolder.StopIngressPark()
			_ = park.Close()
			pm.logger.Warn("keploy's ingress hold stopped: its park could not accept calls; a call to the application's port before keploy's forwarder listens there is refused, as without the hold",
				zap.Error(err))
			return
		}
		pause = 0
		go pm.serveParked(ctx, conn)
	}
}

// parkAcceptCanRecover is whether an Accept error is one a later Accept can
// get past: the process or the system out of file descriptors or memory.
func parkAcceptCanRecover(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ENOBUFS) || errors.Is(err, syscall.ENOMEM)
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
	if ta.IP.To4() == nil {
		pm.parkedServedV6.Add(1)
	} else {
		pm.parkedServed.Add(1)
	}
	pm.logger.Debug("serving a call to the app's port before keploy's forwarder listened (held from the app's bind)",
		zap.Uint16("orig_port", orig), zap.Uint16("new_port", moved))
	pm.handleConnection(ctx, conn, "127.0.0.1:"+strconv.Itoa(int(moved)), pm.logger, pm.tcChan, pm.portSem(orig), orig)
}
