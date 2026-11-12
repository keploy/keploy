package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
)

// appdial.go: how the ingress reaches the application it forwards a call to,
// and every redial of it.
//
// The ingress dials the app on the loopback, where a dial to a listener that
// exists completes in the kernel however loaded the host is: a full accept
// queue only delays it until the app accepts. So no time ends a dial; only
// the call's context does, or its client's going. A deadline could only fail
// a call the app would have taken; the one the ingress had did worse. It
// retried a refusal for 3 s and made its last try after that deadline, on
// 1 ms, so on a loaded host a refusal came back as "i/o timeout": the probe a
// lane sent as the app started, served by keploy's hold, reached its dial
// only after the recording had stopped the app, and the refusal the stop
// explains (logAppDialFailure, at DEBUG while stopping) was an ERROR instead.
//
// A refusal is the app not listening at the address dialed. The kernel's
// socket table says where it listens instead (util.TCPListeners, on the port
// dialed, in the network namespace the dial is made in, which is the app's):
//   - The app listens on the port at another address of the namespace's own:
//     [::1] (Node 17 and later often bind listen(port, "localhost") there),
//     :: with IPV6_V6ONLY, another address of 127/8, or an interface's (a
//     pod's or container's IP: Spring's server.address=${POD_IP}, a gRPC
//     server bound to the pod IP). The bind keploy moved named that address.
//     Every address the namespace has is local to the ingress, which shares
//     the namespace, so the call is dialed there, and the address is
//     remembered for the calls after it (listensAt) until a dial there is
//     refused.
//   - The app listens on the port only where no connection reaches it: at an
//     address the namespace does not have (a socket bound with IP_FREEBIND or
//     IP_TRANSPARENT, which a connection would leave the host for:
//     util.IsLocalAddr), or at an IPv6 link-local address with no device to
//     name. The call fails at once, with an error that names the address and
//     why (appUnreachableError, at ERROR).
//   - Nothing listens on the port: the app has not listened yet (keploy's
//     hold serves a call from the app's bind, before its listen) or not again
//     (an app that restarts in a recording: a watcher's rebuild, a
//     supervisor, a container restart). The call waits for the listener: the
//     event that answers it, not a time. While the recording is stopping
//     (SetStopping, which the agent sets before its client stops the app),
//     the stop is what took the listener, and the refusal is returned at
//     once, as the refusal it is.
// Where the table cannot be read (another platform: the bind keploy moves is
// Linux's), a refusal is the last case.
//
// No dial leaves the host (dialHere): the loopback's address and the
// unspecified one (a connect there reaches the loopback) are dialed as they
// are; any other is dialed only while it is one of the namespace's own
// (util.IsLocalAddr: on Linux the kernel's route lookup, elsewhere an
// address one of the host's interfaces has), as that is asked again before
// every dial there. An interface's address can go (a laptop's Wi-Fi changes
// network), and a dial there would then be routed off the host, to
// whichever one has the address now. An address the app was found at that
// has gone is forgotten, and the call is dialed at the address it was given;
// a call given such an address (the redial of a connection first made
// there) fails at once, at ERROR. Where the socket table is not read (every
// platform but Linux), no address is found in it: only an address a call is
// given is dialed, as the destination keploy mock's live handler is handed
// (ServeLive), which is the address the test dialed.
//
// A call's wait ends when the app listens, when the recording begins to stop,
// when the call's context ends (keploy's own end: ctx.Err()) or when its
// client goes (errClientLeft), and nothing is sent on to the app for a client
// that went. A client that half-closes while it waits counts as gone and its
// request is dropped: without keploy that client's connect would have been
// refused, because the app was not listening.
//
// Every call that finds the same address refusing shares one wait
// (appWait): one dial, and after a refusal one look at the socket table, every
// ingressTargetPollInterval, which is how the listener's coming is seen (no
// kernel event tells it here: the socket's CLOSE->LISTEN tracepoint would
// need the ebpf hooks), and which also checks each waiting call's client. The
// connection that finds the listener is handed to one of the waiting calls
// (any one), so the app almost never sees a connection keploy opened for
// itself: only when that call's context ends as the connection is handed to
// it, and keploy then closes it unused (leaveWait). The other calls dial
// their own, at the address the wait found. The last call to leave a wait
// returns only once its poll has stopped: the end of the wait's context
// cannot stop a dial already past the dialer's look at it, and that dial's
// connect would reach an app that listens just after, for a call that has
// ended.

// errClientLeft is a dial's end for a call whose client went while the call
// waited for the app to listen: nothing was sent on to the app.
var errClientLeft = errors.New("the call's client closed its connection while keploy waited for the application to listen; nothing was sent on to the application")

// appUnreachableError is a dial's end for a call to an app that, on the port
// dialed, listens only where no connection reaches it: not a readiness
// question (the app listens), and not one time answers. With no listeners
// (at), it is the end of a call given an address no dial is made to
// (dialHere), and why says why.
type appUnreachableError struct {
	dialed string
	at     []unreachableListener
	why    string
}

// unreachableListener is a listener no connection reaches, and why.
type unreachableListener struct {
	at  netip.AddrPort
	why string
}

const (
	whyNotLocal = "an address this network namespace does not have (a socket can listen there only with IP_FREEBIND or IP_TRANSPARENT), so a connection to it would be routed off the host, not to the application"
	whyNoDevice = "an IPv6 link-local address bound to no device, so no connection can name the link it is on"
	whyNotOwn   = "it is not an address of the network namespace keploy's ingress shares with the application (an interface's address can go, as a laptop's does when its Wi-Fi changes network), so a connection to it would be routed off the host, not to the application"
	whyNotAsked = "keploy could not ask the kernel whether it is an address of the network namespace keploy's ingress shares with the application, and a connection to an address that is not would be routed off the host"
	whyNotAnIP  = "it is not an IP address and port, so keploy cannot ask whether a connection to it stays on the host"
)

func (e *appUnreachableError) Error() string {
	if len(e.at) == 0 {
		return fmt.Sprintf("keploy's ingress did not dial the application at %s: %s", e.dialed, e.why)
	}
	at := make([]string, len(e.at))
	for i, l := range e.at {
		at[i] = l.at.String() + ", " + l.why
	}
	return fmt.Sprintf("the application does not listen at %s, and no connection from keploy's ingress, which shares the application's network namespace, reaches where it listens on that port: %s",
		e.dialed, strings.Join(at, "; "))
}

// appWait is the one wait of every call that found addr refusing while the
// recording was not stopping. Its fields but ctx, cancel, polled and
// tableErr are guarded by the manager's waitsMu; tableErr is the poll's own.
type appWait struct {
	addr     string
	callers  map[*appWaiter]struct{}
	ctx      context.Context // ends when the wait does, or its last caller leaves
	cancel   context.CancelFunc
	polled   chan struct{} // closed once the poll (pollApp) has returned
	tableErr bool          // the poll could not read the socket table, and said so
}

// appWaiter is one call's place in an appWait.
type appWaiter struct {
	client net.Conn
	done   chan struct{} // closed once the wait has ended for this call
	ended  bool
	// Set before done is closed: the poll's connection, handed to this call,
	// or the end that stops the call's dial; neither: dial target.
	conn   net.Conn
	err    error
	target string
}

// appNetwork is the network a dial to addr uses: tcp6 for an IPv6 address
// (a link-local one with its zone too), else tcp4.
func appNetwork(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		if ip, err := netip.ParseAddr(host); err == nil && ip.Is6() && !ip.Is4In6() {
			return "tcp6"
		}
	}
	return "tcp4"
}

// dialHere dials addr, unless a connection there could leave the host
// (offHost), where it returns that end and dials nothing.
func dialHere(ctx context.Context, d *net.Dialer, addr string) (net.Conn, error) {
	if err := offHost(addr); err != nil {
		return nil, err
	}
	return d.DialContext(ctx, appNetwork(addr), addr)
}

// offHost is nil for an address a connection to stays on the host: the
// loopback's, the unspecified address (0.0.0.0 or ::, an IPv4-mapped one
// too: a connect there is delivered to the loopback, on Linux and macOS
// alike), or one that is the namespace's own now (util.IsLocalAddr). For
// any other address, one that cannot be asked about and one that is not an
// IP address, it is the appUnreachableError that says why no dial is made
// there.
func offHost(addr string) error {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return &appUnreachableError{dialed: addr, why: whyNotAnIP}
	}
	if a := ap.Addr().Unmap(); a.IsLoopback() || a.IsUnspecified() {
		return nil
	}
	local, err := util.IsLocalAddr(ap.Addr())
	switch {
	case err != nil:
		return &appUnreachableError{dialed: addr, why: fmt.Sprintf("%s (%v)", whyNotAsked, err)}
	case !local:
		return &appUnreachableError{dialed: addr, why: whyNotOwn}
	}
	return nil
}

// dialApp dials the application at addr for a call from client (nil when
// there is none to watch): first where it was found listening for addr, if
// it was (listensAt), else at addr.
func (pm *IngressProxyManager) dialApp(ctx context.Context, addr string, client net.Conn) (net.Conn, error) {
	target := pm.listenerFound(addr)
	var d net.Dialer
	for {
		conn, err := dialHere(ctx, &d, target)
		if err == nil {
			return conn, nil
		}
		var unreachable *appUnreachableError
		if target != addr && errors.As(err, &unreachable) {
			// An address the app was found at that a dial would leave the
			// host for now (an interface's address that went): forget it,
			// and dial addr.
			pm.forgetListener(addr, target)
			target = addr
			continue
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// isConnRefused is platform-aware: on Windows a refused connect is
		// WSAECONNREFUSED, not the POSIX ECONNREFUSED.
		if !isConnRefused(err) {
			return nil, err
		}
		if target != addr {
			// The app no longer listens where it was found: forget that, and
			// ask from addr again.
			pm.forgetListener(addr, target)
		}
		if pm.stopping.Load() {
			return pm.dialWhileStopping(ctx, &d, addr, target, err)
		}
		if clientLeft(client) {
			return nil, errClientLeft
		}
		// The wait for addr, whether the refusal was addr's or that of an
		// address found earlier (its listener has gone since).
		conn, target, err = pm.awaitListener(ctx, addr, client)
		if conn != nil || err != nil {
			return conn, err
		}
		// The app listens at target: dial it (the poll's connection went to
		// another call).
	}
}

// dialWhileStopping is dialApp's end for a refusal (refused, at tried) once
// the recording is stopping: final, as the stop took the app's listener,
// unless the socket table has the app still listening somewhere else (an app
// on [::1] alone serves until it is stopped, as one on 127.0.0.1 does), where
// one dial is made.
func (pm *IngressProxyManager) dialWhileStopping(ctx context.Context, d *net.Dialer, addr, tried string, refused error) (net.Conn, error) {
	at, err := pm.findListener(addr)
	if err != nil || at == "" || at == tried {
		return nil, refused
	}
	conn, err := dialHere(ctx, d, at)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, refused
	}
	pm.rememberListener(addr, at)
	return conn, nil
}

// listenerFound is where the app was found listening for addr (listensAt),
// or addr.
func (pm *IngressProxyManager) listenerFound(addr string) string {
	pm.waitsMu.Lock()
	defer pm.waitsMu.Unlock()
	if at, ok := pm.listensAt[addr]; ok {
		return at
	}
	return addr
}

// rememberListener has the calls to addr dialed at at from now on.
func (pm *IngressProxyManager) rememberListener(addr, at string) {
	pm.waitsMu.Lock()
	defer pm.waitsMu.Unlock()
	pm.rememberListenerLocked(addr, at)
}

// rememberListenerLocked is rememberListener with waitsMu held.
func (pm *IngressProxyManager) rememberListenerLocked(addr, at string) {
	if pm.listensAt == nil {
		pm.listensAt = make(map[string]string)
	}
	pm.listensAt[addr] = at
}

// forgetListener drops at as where the app listens for addr, once a dial
// there was refused, or would leave the host.
func (pm *IngressProxyManager) forgetListener(addr, at string) {
	pm.waitsMu.Lock()
	defer pm.waitsMu.Unlock()
	if pm.listensAt[addr] == at {
		delete(pm.listensAt, addr)
	}
}

// clientLeft reports whether the call's client has sent its FIN or a reset
// (util.PeerGone). Where that cannot be asked (no socket under client, or a
// platform that cannot say), the client is taken as still there.
func clientLeft(client net.Conn) bool {
	if client == nil {
		return false
	}
	if r, ok := client.(*replayConn); ok {
		client = r.Conn
	}
	gone, known := util.PeerGone(client)
	return gone && known
}

// awaitListener waits, in addr's shared wait, until the app listens: it
// returns the connection that found the listener, if it is this call's, or
// nil, nil and the address to dial (addr, or the one the wait found the app
// listening at). Its other ends are the refusal once the recording is
// stopping, appUnreachableError, ctx.Err() and errClientLeft.
func (pm *IngressProxyManager) awaitListener(ctx context.Context, addr string, client net.Conn) (net.Conn, string, error) {
	w := &appWaiter{client: client, done: make(chan struct{})}
	pm.waitsMu.Lock()
	if pm.waits == nil {
		pm.waits = make(map[string]*appWait)
	}
	wait := pm.waits[addr]
	if wait == nil {
		wait = &appWait{addr: addr, callers: make(map[*appWaiter]struct{}), polled: make(chan struct{})}
		wait.ctx, wait.cancel = context.WithCancel(context.Background())
		pm.waits[addr] = wait
		go pm.pollApp(wait)
	}
	wait.callers[w] = struct{}{}
	pm.waitsMu.Unlock()

	select {
	case <-w.done:
		return w.conn, w.target, w.err
	case <-ctx.Done():
	}
	pm.leaveWait(wait, w)
	return nil, "", ctx.Err()
}

// leaveWait takes w, whose call's context has ended, out of wait: the last
// caller to leave ends the wait, and returns once its poll has stopped, so
// that no dial of the poll's reaches the app after the call has ended. When
// the wait ended for w as its context did, a connection the poll handed to
// it goes to no one, and is closed.
func (pm *IngressProxyManager) leaveWait(wait *appWait, w *appWaiter) {
	pm.waitsMu.Lock()
	if w.ended {
		pm.waitsMu.Unlock()
		if w.conn != nil {
			_ = w.conn.Close()
		}
		return
	}
	delete(wait.callers, w)
	last := len(wait.callers) == 0
	if last {
		pm.dropWaitLocked(wait)
	}
	pm.waitsMu.Unlock()
	if last {
		// The poll's dial may be past the dialer's look at the context the
		// drop just ended, its connect still to come. The poll takes waitsMu
		// only after its look (probeApp), and stops there: what is left of
		// it is that dial's connect, which the ended context does not let
		// wait, and at most one read of the socket table.
		<-wait.polled
	}
}

// dropWaitLocked ends wait, which no call waits in any more: a call that
// finds addr refusing after this starts a new one. waitsMu is held.
func (pm *IngressProxyManager) dropWaitLocked(wait *appWait) {
	if pm.waits[wait.addr] == wait {
		delete(pm.waits, wait.addr)
	}
	wait.cancel()
}

// endWaiterLocked ends the wait of one call with conn, the address to dial,
// or err. waitsMu is held.
func endWaiterLocked(wait *appWait, w *appWaiter, conn net.Conn, target string, err error) {
	delete(wait.callers, w)
	w.conn, w.target, w.err, w.ended = conn, target, err, true
	close(w.done)
}

// pollApp looks for the app's listener (probeApp) until the app listens,
// the recording begins to stop, the socket table says the app listens only
// where no connection reaches, or no call waits any more, and checks each
// waiting call's client between looks. It closes wait.polled as it returns.
func (pm *IngressProxyManager) pollApp(wait *appWait) {
	defer close(wait.polled)
	d := net.Dialer{Control: pm.pollControl}
	tick := time.NewTicker(ingressTargetPollInterval)
	defer tick.Stop()
	for {
		target, conn, err := pm.probeApp(wait, &d)
		pm.waitsMu.Lock()
		if wait.ctx.Err() != nil {
			// Every call left (dropWaitLocked): a connection made as they
			// did goes to no one.
			pm.waitsMu.Unlock()
			if conn != nil {
				_ = conn.Close()
			}
			return
		}
		var unreachable *appUnreachableError
		switch {
		case err == nil, !isConnRefused(err) && !errors.As(err, &unreachable):
			// The app listens at target: the connection is one call's, and
			// the others dial their own there. A dial that failed otherwise
			// is each call's own to make, and to say.
			if err == nil && target != wait.addr {
				pm.rememberListenerLocked(wait.addr, target)
				pm.logger.Debug("the application listens on its port at another address than the one keploy dials; forwarding its calls there",
					zap.String("dialed", wait.addr), zap.String("application_listens_at", target))
			}
			for w := range wait.callers {
				endWaiterLocked(wait, w, conn, target, nil)
				conn = nil
			}
			if conn != nil {
				_ = conn.Close()
			}
			pm.dropWaitLocked(wait)
			pm.waitsMu.Unlock()
			return
		case unreachable != nil, pm.stopping.Load():
			for w := range wait.callers {
				endWaiterLocked(wait, w, nil, "", err)
			}
			pm.dropWaitLocked(wait)
			pm.waitsMu.Unlock()
			return
		}
		for w := range wait.callers {
			if clientLeft(w.client) {
				endWaiterLocked(wait, w, nil, "", errClientLeft)
			}
		}
		if len(wait.callers) == 0 {
			pm.dropWaitLocked(wait)
			pm.waitsMu.Unlock()
			return
		}
		pm.waitsMu.Unlock()
		select {
		case <-wait.ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// probeApp is one look of wait's poll for the app's listener: a dial to
// wait's address and, when that is refused, the kernel's socket table on its
// port (findListener). It returns the address it dialed last and that dial's
// connection or error, or an appUnreachableError when the app listens on the
// port only where no connection reaches, or a dial would leave the host
// (dialHere: wait's address, an interface's, went during the wait). A
// refusal means the app listens nowhere yet: the wait goes on, but once the
// recording is stopping.
func (pm *IngressProxyManager) probeApp(wait *appWait, d *net.Dialer) (string, net.Conn, error) {
	conn, err := dialHere(wait.ctx, d, wait.addr)
	if err == nil || !isConnRefused(err) {
		return wait.addr, conn, err
	}
	at, ferr := pm.findListener(wait.addr)
	var unreachable *appUnreachableError
	switch {
	case at != "" && at != wait.addr:
		conn, err := dialHere(wait.ctx, d, at)
		return at, conn, err
	case errors.As(ferr, &unreachable):
		return wait.addr, nil, ferr
	case ferr != nil && !wait.tableErr && !errors.Is(ferr, util.ErrNoSocketTable):
		// Where the table is not read at all (ErrNoSocketTable: every
		// platform but Linux), that is a stated limit, not news of this
		// wait.
		wait.tableErr = true
		pm.logger.Debug("could not read the kernel's socket table to see where the application listens; waiting for it to listen at the address keploy dials",
			zap.String("dialed", wait.addr), zap.Error(ferr))
	}
	// No listener on the port, or one that takes the address dialed and
	// refused it all the same (it listened since, or refuses a full accept
	// queue's connections: tcp_abort_on_overflow): the next look dials it
	// again.
	return wait.addr, nil, err
}

// findListener reads, in the kernel's tables, where the app listens on
// addr's port (listenerFor): the address to dial, or "" and nil when nothing
// listens there, an appUnreachableError when the app listens only where no
// connection reaches, or the error that kept the tables from being read.
func (pm *IngressProxyManager) findListener(addr string) (string, error) {
	dialed, err := netip.ParseAddrPort(addr)
	if err != nil {
		return "", err
	}
	listeners, err := util.TCPListeners(dialed.Port())
	if err != nil {
		return "", err
	}
	var lerr error
	local := func(a netip.Addr) bool {
		ok, err := util.IsLocalAddr(a)
		if err != nil && lerr == nil {
			lerr = err
		}
		return ok
	}
	target, ok, unreachable := listenerFor(dialed, listeners, local)
	switch {
	case ok:
		return target.String(), nil
	case lerr != nil:
		// Not known to be unreachable: it may be the namespace's own.
		return "", lerr
	case len(unreachable) > 0:
		return "", &appUnreachableError{dialed: addr, at: unreachable}
	}
	return "", nil
}

// listenerFor picks, from the addresses listeners has a listener at on
// dialed's port, the one to dial for dialed: an address that takes dialed
// itself (its own, or the unspecified address of its family), else an IPv4
// loopback one (all of 127/8 is the loopback's, and 0.0.0.0 takes
// 127.0.0.1), else [::1] (which :: takes, with IPV6_V6ONLY or without), else
// one of the namespace's own addresses (local), IPv4 first. ok is false when
// there is none; unreachable is then every address there is a listener at,
// in order, with why no connection reaches it.
func listenerFor(dialed netip.AddrPort, listeners []netip.Addr, local func(netip.Addr) bool) (target netip.AddrPort, ok bool, unreachable []unreachableListener) {
	h := dialed.Addr().Unmap()
	best := 0
	for _, l := range listeners {
		l = l.Unmap()
		var at netip.Addr
		var rank int
		switch {
		case l == h, l.IsUnspecified() && l.Is4() == h.Is4():
			at, rank = h, 1
		case l.IsUnspecified() && l.Is4():
			at, rank = netip.AddrFrom4([4]byte{127, 0, 0, 1}), 2
		case l.IsUnspecified():
			at, rank = netip.IPv6Loopback(), 3
		case l.IsLoopback() && l.Is4():
			at, rank = l, 2
		case l.IsLoopback():
			at, rank = l, 3
		case l.Is6() && l.IsLinkLocalUnicast() && l.Zone() == "":
			unreachable = append(unreachable, unreachableListener{netip.AddrPortFrom(l, dialed.Port()), whyNoDevice})
			continue
		case !local(l):
			unreachable = append(unreachable, unreachableListener{netip.AddrPortFrom(l, dialed.Port()), whyNotLocal})
			continue
		case l.Is4():
			at, rank = l, 4
		default:
			at, rank = l, 5
		}
		c := netip.AddrPortFrom(at, dialed.Port())
		if !ok || rank < best || rank == best && c.Compare(target) < 0 {
			target, best, ok = c, rank, true
		}
	}
	if ok {
		return target, true, nil
	}
	slices.SortFunc(unreachable, func(a, b unreachableListener) int { return a.at.Compare(b.at) })
	return netip.AddrPort{}, false, slices.CompactFunc(unreachable, func(a, b unreachableListener) bool { return a.at == b.at })
}
