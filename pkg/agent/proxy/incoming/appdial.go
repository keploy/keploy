package proxy

import (
	"context"
	"errors"
	"net"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/util"
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
// app has not listened yet (keploy's hold serves a call from the app's bind,
// before its listen) or not again (an app that restarts in a recording: a
// watcher's rebuild, a supervisor, a container restart), and the call waits
// for the listener: the event that answers it, not a time. Its wait ends when
// the app listens, when the recording begins to stop, when the call's context
// ends (keploy's own end: ctx.Err()) or when its client goes (errClientLeft),
// and nothing is sent on to the app for a client that went.
//
// Every call that finds the same address refusing shares one wait
// (appWait): one dial every ingressTargetPollInterval, which is how the
// listener's coming is seen (no kernel event tells it here: the socket's
// CLOSE->LISTEN tracepoint would need the ebpf hooks), and which also checks
// each waiting call's client. The connection that finds the listener is the
// first waiting call's, so the app sees no connection keploy opened for
// itself; the other calls dial their own.

// errClientLeft is a dial's end for a call whose client went while the call
// waited for the app to listen: nothing was sent on to the app.
var errClientLeft = errors.New("the call's client closed its connection while keploy waited for the application to listen; nothing was sent on to the application")

// appWait is the one wait of every call that found addr refusing while the
// recording was not stopping. Its fields but ctx and cancel are guarded by
// the manager's waitsMu.
type appWait struct {
	addr, network string
	callers       map[*appWaiter]struct{}
	ctx           context.Context // ends when the wait does, or its last caller leaves
	cancel        context.CancelFunc
}

// appWaiter is one call's place in an appWait.
type appWaiter struct {
	client net.Conn
	done   chan struct{} // closed once the wait has ended for this call
	ended  bool
	// Set before done is closed: the poll's connection, handed to this call,
	// or the end that stops the call's dial; neither: dial again.
	conn net.Conn
	err  error
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
	network := appNetwork(addr)
	var d net.Dialer
	for {
		conn, err := d.DialContext(ctx, network, addr)
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
		conn, err = pm.awaitListener(ctx, network, addr, client)
		if conn != nil || err != nil {
			return conn, err
		}
		// The app listens: dial again (the poll's connection went to
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

// awaitListener waits, in addr's shared wait, until the app listens there:
// it returns the connection that found the listener, if it is this call's,
// or nil and nil to dial again. Its other ends are the refusal once the
// recording is stopping, ctx.Err() and errClientLeft.
func (pm *IngressProxyManager) awaitListener(ctx context.Context, network, addr string, client net.Conn) (net.Conn, error) {
	w := &appWaiter{client: client, done: make(chan struct{})}
	pm.waitsMu.Lock()
	if pm.waits == nil {
		pm.waits = make(map[string]*appWait)
	}
	wait := pm.waits[addr]
	if wait == nil {
		wait = &appWait{addr: addr, network: network, callers: make(map[*appWaiter]struct{})}
		wait.ctx, wait.cancel = context.WithCancel(context.Background())
		pm.waits[addr] = wait
		go pm.pollApp(wait)
	}
	wait.callers[w] = struct{}{}
	pm.waitsMu.Unlock()

	select {
	case <-w.done:
		return w.conn, w.err
	case <-ctx.Done():
	}
	pm.waitsMu.Lock()
	if w.ended {
		// The wait ended for it as its context did: a connection handed to
		// it goes to no one.
		pm.waitsMu.Unlock()
		if w.conn != nil {
			_ = w.conn.Close()
		}
		return nil, ctx.Err()
	}
	delete(wait.callers, w)
	if len(wait.callers) == 0 {
		pm.dropWaitLocked(wait)
	}
	pm.waitsMu.Unlock()
	return nil, ctx.Err()
}

// dropWaitLocked ends wait, which no call waits in any more: a call that
// finds addr refusing after this starts a new one. waitsMu is held.
func (pm *IngressProxyManager) dropWaitLocked(wait *appWait) {
	if pm.waits[wait.addr] == wait {
		delete(pm.waits, wait.addr)
	}
	wait.cancel()
}

// endWaiterLocked ends the wait of one call with conn or err. waitsMu is
// held.
func endWaiterLocked(wait *appWait, w *appWaiter, conn net.Conn, err error) {
	delete(wait.callers, w)
	w.conn, w.err, w.ended = conn, err, true
	close(w.done)
}

// pollApp dials wait's address until the app listens there, the recording
// begins to stop, or no call waits any more, and checks each waiting call's
// client between dials.
func (pm *IngressProxyManager) pollApp(wait *appWait) {
	var d net.Dialer
	tick := time.NewTicker(ingressTargetPollInterval)
	defer tick.Stop()
	for {
		conn, err := d.DialContext(wait.ctx, wait.network, wait.addr)
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
		switch {
		case err == nil, !isConnRefused(err):
			// The app listens: the connection is one call's, and the
			// others dial their own. A dial that failed otherwise is each
			// call's own to make, and to say.
			for w := range wait.callers {
				endWaiterLocked(wait, w, conn, nil)
				conn = nil
			}
			if conn != nil {
				_ = conn.Close()
			}
			pm.dropWaitLocked(wait)
			pm.waitsMu.Unlock()
			return
		case pm.stopping.Load():
			for w := range wait.callers {
				endWaiterLocked(wait, w, nil, err)
			}
			pm.dropWaitLocked(wait)
			pm.waitsMu.Unlock()
			return
		}
		for w := range wait.callers {
			if clientLeft(w.client) {
				endWaiterLocked(wait, w, nil, errClientLeft)
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
