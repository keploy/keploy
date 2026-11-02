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
// A refusal is the app not listening there. That is final only while the
// recording is stopping (SetStopping, which the agent sets before its client
// stops the app): the stop is what took the app's listener, and the dial
// returns the refusal at once, as the refusal it is. At any other time the
// kernel's socket table says which of three it is (util.TCPListeners, on the
// port dialed, in the network namespace the dial is made in):
//   - Nothing listens on the port: the app has not listened yet (keploy's
//     hold serves a call from the app's bind, before its listen) or not again
//     (an app that restarts in a recording: a watcher's rebuild, a
//     supervisor, a container restart). The call waits for the listener: the
//     event that answers it, not a time.
//   - The app listens on the port at another loopback address: [::1] (Node
//     17 and later often bind listen(port, "localhost") there), :: with
//     IPV6_V6ONLY, or another address of 127/8. The bind keploy moved named
//     that address, and the call is dialed there.
//   - The app listens on the port only at addresses that are not the
//     loopback's (an interface's, such as a pod's or container's IP): the
//     ingress forwards over the loopback, so the call fails at once, with an
//     error that names where the app listens and what to change
//     (appListensElsewhereError, at ERROR).
// Where the table cannot be read (another platform: the bind keploy moves is
// Linux's), a refusal is waited out as the first case.
//
// A call's wait ends when the app listens, when the recording begins to stop,
// when the call's context ends (keploy's own end: ctx.Err()) or when its
// client goes (errClientLeft), and nothing is sent on to the app for a client
// that went. A client that half-closes while it waits counts as gone and its
// request is dropped: without keploy that client's connect would have been
// refused, because the app was not listening.
//
// Every call that finds the same address refusing shares one wait
// (appWait): one dial and one look at the socket table every
// ingressTargetPollInterval, which is how the listener's coming is seen (no
// kernel event tells it here: the socket's CLOSE->LISTEN tracepoint would
// need the ebpf hooks), and which also checks each waiting call's client. The
// connection that finds the listener is handed to one of the waiting calls
// (any one), so the app almost never sees a connection keploy opened for
// itself: only when that call's context ends as the connection is handed to
// it, and keploy then closes it unused (leaveWait). The other calls dial
// their own, at the address the wait found.

// errClientLeft is a dial's end for a call whose client went while the call
// waited for the app to listen: nothing was sent on to the app.
var errClientLeft = errors.New("the call's client closed its connection while keploy waited for the application to listen; nothing was sent on to the application")

// appListensElsewhereError is a dial's end for a call to an app that, on the
// port dialed, listens only at addresses the ingress does not dial: not a
// readiness question (the app listens), and not one time answers.
type appListensElsewhereError struct {
	dialed string
	at     []netip.AddrPort
}

func (e *appListensElsewhereError) Error() string {
	at := make([]string, len(e.at))
	for i, a := range e.at {
		at[i] = a.String()
	}
	return fmt.Sprintf("the application does not listen at %s: on that port it listens only at %s, which keploy's ingress does not forward calls to, since it reaches the application over the loopback. Make the application listen on all addresses (0.0.0.0 or ::) or on a loopback address (127.0.0.1 or ::1)",
		e.dialed, strings.Join(at, ", "))
}

// appWait is the one wait of every call that found addr refusing while the
// recording was not stopping. Its fields but ctx, cancel and tableErr are
// guarded by the manager's waitsMu; tableErr is the poll's own.
type appWait struct {
	addr, network string
	callers       map[*appWaiter]struct{}
	ctx           context.Context // ends when the wait does, or its last caller leaves
	cancel        context.CancelFunc
	tableErr      bool // the poll could not read the socket table, and said so
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

// appNetwork is the network a dial to addr uses: tcp6 for an IPv6 address,
// else tcp4.
func appNetwork(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
			return "tcp6"
		}
	}
	return "tcp4"
}

// dialApp dials the application at addr for a call from client (nil when
// there is none to watch).
func (pm *IngressProxyManager) dialApp(ctx context.Context, addr string, client net.Conn) (net.Conn, error) {
	target := addr
	var d net.Dialer
	for {
		conn, err := d.DialContext(ctx, appNetwork(target), target)
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// isConnRefused is platform-aware: on Windows a refused connect is
		// WSAECONNREFUSED, not the POSIX ECONNREFUSED.
		if !isConnRefused(err) || pm.stopping.Load() {
			return nil, err
		}
		if clientLeft(client) {
			return nil, errClientLeft
		}
		// The wait for addr, whether the refusal was addr's or that of an
		// address an earlier wait found (its listener has gone since).
		conn, target, err = pm.awaitListener(ctx, addr, client)
		if conn != nil || err != nil {
			return conn, err
		}
		// The app listens at target: dial it (the poll's connection went to
		// another call).
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
// stopping, appListensElsewhereError, ctx.Err() and errClientLeft.
func (pm *IngressProxyManager) awaitListener(ctx context.Context, addr string, client net.Conn) (net.Conn, string, error) {
	w := &appWaiter{client: client, done: make(chan struct{})}
	pm.waitsMu.Lock()
	if pm.waits == nil {
		pm.waits = make(map[string]*appWait)
	}
	wait := pm.waits[addr]
	if wait == nil {
		wait = &appWait{addr: addr, network: appNetwork(addr), callers: make(map[*appWaiter]struct{})}
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
// caller to leave ends the wait. When the wait ended for w as its context
// did, a connection the poll handed to it goes to no one, and is closed.
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
	if len(wait.callers) == 0 {
		pm.dropWaitLocked(wait)
	}
	pm.waitsMu.Unlock()
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
// where the ingress does not dial, or no call waits any more, and checks
// each waiting call's client between looks.
func (pm *IngressProxyManager) pollApp(wait *appWait) {
	var d net.Dialer
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
		var elsewhere *appListensElsewhereError
		switch {
		case err == nil, !isConnRefused(err) && !errors.As(err, &elsewhere):
			// The app listens at target: the connection is one call's, and
			// the others dial their own there. A dial that failed otherwise
			// is each call's own to make, and to say.
			if err == nil && target != wait.addr {
				pm.logger.Debug("the application listens on its port at another loopback address than the one keploy dials; forwarding its calls there",
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
		case elsewhere != nil, pm.stopping.Load():
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
// wait's address and, when that is refused while the recording is not
// stopping, the kernel's socket table on its port (listenerFor). It returns
// the address it dialed last and that dial's connection or error, or an
// appListensElsewhereError when the app listens on the port only where the
// ingress does not dial. A refusal means the app listens nowhere the ingress
// dials yet: the wait goes on.
func (pm *IngressProxyManager) probeApp(wait *appWait, d *net.Dialer) (string, net.Conn, error) {
	conn, err := d.DialContext(wait.ctx, wait.network, wait.addr)
	if err == nil || !isConnRefused(err) || pm.stopping.Load() {
		return wait.addr, conn, err
	}
	dialed, perr := netip.ParseAddrPort(wait.addr)
	if perr != nil {
		return wait.addr, nil, err
	}
	listeners, terr := util.TCPListeners(dialed.Port())
	if terr != nil {
		// Where the table is not read at all (ErrNoSocketTable: every
		// platform but Linux), that is a stated limit, not news of this
		// wait.
		if !wait.tableErr && !errors.Is(terr, util.ErrNoSocketTable) {
			wait.tableErr = true
			pm.logger.Debug("could not read the kernel's socket table to see where the application listens; waiting for it to listen at the address keploy dials",
				zap.String("dialed", wait.addr), zap.Error(terr))
		}
		return wait.addr, nil, err
	}
	target, ok, elsewhere := listenerFor(dialed, listeners)
	switch {
	case ok && target != dialed:
		at := target.String()
		conn, err := d.DialContext(wait.ctx, appNetwork(at), at)
		return at, conn, err
	case !ok && len(elsewhere) > 0:
		return wait.addr, nil, &appListensElsewhereError{dialed: wait.addr, at: elsewhere}
	}
	// No listener on the port, or one that takes the address dialed and
	// refused it all the same (it listened since, or refuses a full accept
	// queue's connections: tcp_abort_on_overflow): the next look dials it
	// again.
	return wait.addr, nil, err
}

// listenerFor picks, from the addresses listeners has a listener at on
// dialed's port, the one to dial for dialed: an address that takes dialed
// itself (its own, or the unspecified address of its family), else an IPv4
// loopback one (all of 127/8 is the loopback's, and 0.0.0.0 takes
// 127.0.0.1), else [::1] (which :: takes, with IPV6_V6ONLY or without). ok is
// false when none is the loopback's; elsewhere is then every address there
// is a listener at, in order, the ingress dialing none of them.
func listenerFor(dialed netip.AddrPort, listeners []netip.Addr) (target netip.AddrPort, ok bool, elsewhere []netip.AddrPort) {
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
		default:
			elsewhere = append(elsewhere, netip.AddrPortFrom(l, dialed.Port()))
			continue
		}
		c := netip.AddrPortFrom(at, dialed.Port())
		if !ok || rank < best || rank == best && c.Compare(target) < 0 {
			target, best, ok = c, rank, true
		}
	}
	if ok {
		return target, true, nil
	}
	slices.SortFunc(elsewhere, netip.AddrPort.Compare)
	return netip.AddrPort{}, false, slices.Compact(elsewhere)
}
