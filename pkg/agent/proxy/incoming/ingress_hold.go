package proxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
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
	// ArmIngressPark puts the park in the lookup, as the manager's accept
	// loop on it starts: the lookup never steers to a park nothing accepts
	// on (its calls would hang), so until then a call is the kernel's.
	ArmIngressPark() error
	// AppIPv6Binds is the IPv6 addresses the app's moved sockets of orig
	// take calls on, one per note of what a socket bound: an IPv6 socket's
	// address (:: for any, with IPV6_V6ONLY or without); none for an IPv4
	// socket, or an IPv6 one on a v4-mapped address, which takes IPv4 only.
	// The hold takes the port's IPv6 calls only on these.
	AppIPv6Binds(orig uint16) ([]netip.Addr, error)
	// IngressHeld reports whether the moved ports are held.
	IngressHeld() bool
	// IngressLookupCounts is what the lookup did with the listener lookups
	// for a moved port, by name, or nil when nothing is held: lookups, not
	// connections (a retransmitted SYN is looked up again), for Debug.
	IngressLookupCounts() map[string]uint64
	// IngressSteers reports whether the hooks steer the app's ports: the
	// bind hooks move no bind, so the app listens on its own port, and the
	// park takes every call the app's sockets would have taken. The manager
	// then forwards each to the address it was sent to, and binds no port
	// of the app's: when keploy goes, its sockets go with it, and the app's
	// own socket takes its calls.
	IngressSteers() bool
	// OwnDialControl is the Control of a dialer that reaches the app while
	// the hold is on: it names the socket as keploy's own, and the kernel
	// notes the connection as it begins it, so the lookup leaves it to the
	// app's socket instead of steering it back to the park. A no-op with the
	// hold off.
	OwnDialControl(network, address string, c syscall.RawConn) error
	// OwnConnNoted is nil once the kernel noted a connection made with
	// OwnDialControl as keploy's own; an error means it may have been
	// steered back to keploy, and the caller closes it.
	OwnConnNoted(conn net.Conn) error
	// SteeredDialTarget is where a steered call sent to to is forwarded:
	// the loopback, on to's port, where the app's socket that takes the call
	// is one bound to any address and the loopback reaches that same socket
	// (a connection there whose client closes first leaves a TIME_WAIT the
	// kernel reuses), else to.
	SteeredDialTarget(to netip.AddrPort) netip.AddrPort
	// KeepSteerTable keeps the ports the hooks steer where an agent
	// restarted in the pod finds them, to steer them again: the manager
	// calls it as it starts on a steered port.
	KeepSteerTable()
}

// ingressSteers reports whether the hooks steer the app's ports.
func (pm *IngressProxyManager) ingressSteers() bool {
	return pm.portHolder != nil && pm.portHolder.IngressSteers()
}

// appDialControl is the Control of every dialer the ingress reaches the app
// with: the hooks' (OwnDialControl), then the test's (pollControl) for a
// poll's dial.
func (pm *IngressProxyManager) appDialControl(poll bool) func(network, address string, c syscall.RawConn) error {
	test := pm.pollControl
	if !poll {
		test = nil
	}
	if pm.portHolder == nil {
		return test
	}
	holder := pm.portHolder
	return func(network, address string, c syscall.RawConn) error {
		if err := holder.OwnDialControl(network, address, c); err != nil {
			return err
		}
		if test != nil {
			return test(network, address, c)
		}
		return nil
	}
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

// logIngressHoldEnd says, when recording ends, how many calls the hold
// served (Info, when any), and what the lookup did (Debug). IPv4 calls reach
// the park only before the port's forwarder listens (it then takes them);
// IPv6 ones (an app bound to :: takes both families) for the whole run, as
// keploy's forwarder listens on IPv4 only: without the hold they were
// refused. So they are counted apart, and the IPv6 line says where they
// went: an app that listens on IPv6 only does not answer them there.
func (pm *IngressProxyManager) logIngressHoldEnd() {
	if steered := pm.steeredServed.Load(); steered > 0 {
		pm.logger.Debug("keploy steered calls to the application's own port to its ingress", zap.Uint64("count", steered))
	}
	if parked := pm.parkedServed.Load(); parked > 0 {
		pm.logger.Info("keploy served calls to the application's port that arrived before its forwarder listened, by holding the port from the application's bind",
			zap.Uint64("count", parked))
	}
	if parked := pm.parkedServedV6.Load(); parked > 0 {
		pm.logger.Info("keploy held IPv6 calls to the application's port, which its IPv4 forwarder cannot take, from the application's bind, and passed each to the application on 127.0.0.1",
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
	// The lookup steers to the park only from here, as this loop accepts on
	// it: an agent restarted while the app runs leaves the app's port to the
	// app until its recorder connects and this loop starts.
	if err := pm.portHolder.ArmIngressPark(); err != nil {
		pm.portHolder.StopIngressPark()
		_ = park.Close()
		pm.logger.Warn("keploy's ingress hold did not start: its park could not be put in the lookup; a call to the application's port is the kernel's, and while keploy steers the port, it reaches the application unrecorded",
			zap.Error(err))
		return
	}
	var pause time.Duration
	warned := false
	for {
		conn, err := park.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if parkAcceptCanRecover(err) {
				pause = parkRetryPause(pause)
				if !warned {
					warned = true
					pm.logger.Warn("keploy's ingress hold could not accept a call to the application's port for want of a resource; it retries, and the call waits until then",
						zap.Error(err))
				}
				if !parkRetryWait(ctx, pause) {
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

// parkRetryPause is the pause before the park's next Accept after one more
// that failed for want of a resource, given the last pause (0 after a good
// Accept): 5 ms, doubled each time, at most 1 s, as net/http's server does.
func parkRetryPause(last time.Duration) time.Duration {
	if last == 0 {
		return 5 * time.Millisecond
	}
	if last *= 2; last > time.Second {
		return time.Second
	}
	return last
}

// parkRetryWait waits out the park's pause d before its next Accept, and
// reports false, at once, when ctx ends first (recording ended: no Accept
// follows). A variable so a test sees each pause without waiting it.
var parkRetryWait = func(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
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
	if !known && pm.ingressSteers() {
		pm.serveSteered(ctx, conn, ta)
		return
	}
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

// serveSteered serves a call the lookup steered to the park while the hooks
// steer: the app listens on the port the call was sent to (its local
// address, which the lookup kept), so it is forwarded to the app's socket
// there (SteeredDialTarget: over the loopback where that reaches the same
// socket), from a socket the lookup leaves to the app (appDialControl), and
// recorded on that port.
func (pm *IngressProxyManager) serveSteered(ctx context.Context, conn net.Conn, ta *net.TCPAddr) {
	ap := ta.AddrPort()
	to := pm.portHolder.SteeredDialTarget(netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()))
	pm.steeredServed.Add(1)
	pm.handleConnection(ctx, conn, to.String(), pm.logger, pm.tcChan, pm.portSem(ap.Port()), ap.Port())
}
