package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/synhold"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// An application's connect is redirected to this proxy, whose listener would
// complete the handshake at once, whatever the destination does. While a
// connection's upstream will be dialled, the proxy holds its handshake
// (synhold) and dials the destination first: the application's connect then
// completes when the destination accepted, and fails the way the destination
// failed it — refused, unreachable, or never answered. The connection that
// dial opened is the one the proxy then uses for it (util.Predialed), so the
// destination sees the application's one connection, as it would without
// keploy.

// predialTTL bounds how long an upstream connection opened for a held
// handshake waits for the proxy to accept the application's end — on
// loopback that follows the answer within milliseconds. Past it the
// application gave up on the connection between the last check and the
// answer, and the upstream is closed.
const predialTTL = 5 * time.Second

type predialEntry struct {
	p  *util.Predialed
	at time.Time
}

// predials holds each pre-dialled upstream from the moment its handshake is
// released until handleConnection takes it, keyed by the application's end.
type predials struct {
	mu sync.Mutex
	m  map[netip.AddrPort]predialEntry
}

func (t *predials) put(client netip.AddrPort, p *util.Predialed) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[netip.AddrPort]predialEntry{}
	}
	if old, ok := t.m[client]; ok {
		old.p.CloseIfUnused() // a new handshake from the same end: the old one was abandoned
	}
	t.m[client] = predialEntry{p: p, at: time.Now()}
}

// drop closes and forgets the pre-dial kept for client, if any: a new
// handshake from that end supersedes it.
func (t *predials) drop(client netip.AddrPort) {
	if p := t.take(client); p != nil {
		p.CloseIfUnused()
	}
}

func (t *predials) take(client netip.AddrPort) *util.Predialed {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[client]
	if !ok {
		return nil
	}
	delete(t.m, client)
	return e.p
}

// reap closes the upstreams whose application never completed its handshake.
func (t *predials) reap(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, e := range t.m {
		if now.Sub(e.at) > predialTTL {
			e.p.CloseIfUnused()
			delete(t.m, k)
		}
	}
}

func (t *predials) closeAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, e := range t.m {
		e.p.CloseIfUnused()
		delete(t.m, k)
	}
}

// failedDests rate-limits the Info line for a destination that fails held
// connects: once a minute per destination, so a retry loop against a
// dependency that is down does not flood the log.
type failedDests struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (f *failedDests) first(dest string, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last == nil || len(f.last) >= 1024 {
		f.last = map[string]time.Time{}
	}
	if t, ok := f.last[dest]; ok && now.Sub(t) < time.Minute {
		return false
	}
	f.last[dest] = now
	return true
}

// clientEnd is the application's end of an accepted connection, as its SYN
// carried it (an IPv4 address for a v4-mapped one).
func clientEnd(a net.Addr) (netip.AddrPort, bool) {
	tcp, ok := a.(*net.TCPAddr)
	if !ok {
		return netip.AddrPort{}, false
	}
	ap := tcp.AddrPort()
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), true
}

// destAddr is a redirected connection's destination as the proxy dials it.
func destAddr(d *agent.NetworkAddress) string {
	if d.Version == 6 {
		return fmt.Sprintf("[%v]:%v", util.ToIPv6AddressStr(d.IPv6Addr), d.Port)
	}
	return fmt.Sprintf("%v:%v", util.ToIP4AddressStr(d.IPv4Addr), d.Port)
}

// dialsUpstream reports whether handleConnection will dial the destination
// of a connection under this session: always while recording; while testing,
// only when traffic passes through to real dependencies instead of being
// served from mocks. Read from the session, not from the proxy's fields that
// Record rewrites, since it runs on decision goroutines.
func (p *Proxy) dialsUpstream(rule *agent.Session) bool {
	if rule.Mode != models.MODE_TEST {
		return true
	}
	return p.GlobalPassthrough || rule.OpportunisticTLSIntercept || !rule.Mocking
}

// decideHandshake is synhold's Decide for this proxy: it dials the held
// connection's destination, keeps the connection for handleConnection, and
// answers the application's handshake the way the destination answered.
func (p *Proxy) decideHandshake(lookup agent.HandshakeDestInfo) synhold.Decide {
	return func(ctx context.Context, client, proxy netip.AddrPort) synhold.Decision {
		// Whatever an earlier handshake from this end left is not this
		// connection's.
		p.predials.drop(client)
		rule := p.getSession()
		if rule == nil || !p.dialsUpstream(rule) {
			return synhold.Decision{Outcome: synhold.Accept}
		}
		dest, err := lookup.GetForHandshake(ctx, client, proxy)
		if err != nil {
			// Not a connection the connect hook redirected (or its record is
			// gone): handleConnection treats it as untracked, as before.
			p.logger.Debug("held handshake has no recorded destination; letting it complete",
				zap.String("client", client.String()), zap.Error(err))
			return synhold.Decision{Outcome: synhold.Accept}
		}
		addr := destAddr(dest)
		conn, err := p.dialWhileHeld(ctx, lookup, client, proxy, addr)
		if errors.Is(err, errHandshakeAbandoned) {
			if p.failedDests.first("gave up "+addr, time.Now()) {
				p.logger.Info("the application gave up connecting to a dependency before the dependency's answer "+
					"reached it (its connect timed out or was cancelled), as it would without keploy; nothing is recorded for it",
					zap.String("destination", addr))
			}
			return synhold.Decision{Outcome: synhold.Drop}
		}
		if err != nil {
			// A dial error with no network-level counterpart (the proxy's own
			// limits) is answered with Accept: the connection completes and
			// fails in handleConnection's own dial, which reports it, as
			// before handshakes were held.
			out := synhold.OutcomeFor(err)
			p.logger.Debug("destination did not accept; answering the application's handshake the same way",
				zap.String("client", client.String()), zap.String("destination", addr),
				zap.Stringer("answer", out), zap.Error(err))
			if out != synhold.Accept && p.failedDests.first("refused "+addr, time.Now()) {
				p.logger.Info("a dependency did not accept the application's connection; the application's connect "+
					"failed the same way, as it would without keploy (an application that tries another address of the "+
					"same name, IPv4 after IPv6 say, may still connect), and nothing is recorded for that attempt",
					zap.String("destination", addr), zap.Stringer("answer", out), zap.Error(err))
			}
			return synhold.Decision{Outcome: out}
		}
		pre := util.NewPredialed(addr, conn)
		return synhold.Decision{
			Outcome: synhold.Accept,
			// Published before the SYN is let through, so handleConnection
			// finds it when the connection arrives.
			Keep:    func() { p.predials.put(client, pre) },
			Discard: pre.CloseIfUnused,
		}
	}
}

// handshakeRecheck is how often a held handshake's dial checks that the
// application still waits for it.
var handshakeRecheck = time.Second

var errHandshakeAbandoned = errors.New("the application closed the connecting socket")

// dialWhileHeld dials addr for a held handshake. It has no timeout of its
// own: the kernel's SYN retries bound it, the same retries (same network
// namespace) that bound the application's own connect. But an application
// with a shorter connect timeout gives up first, and its retries would each
// leave a dial — and a held SYN — running for that whole time; so the dial
// is abandoned as soon as the application's connecting socket is gone.
func (p *Proxy) dialWhileHeld(ctx context.Context, lookup agent.HandshakeDestInfo, client, proxy netip.AddrPort, addr string) (*connseq.Upstream, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		conn *connseq.Upstream
		err  error
	}
	res := make(chan result, 1)
	go func() {
		// Sent even if the dial panics, so the decision always returns.
		r := result{err: errors.New("the dial did not complete")}
		defer func() { res <- r }()
		defer utils.Recover(p.logger)
		r.conn, r.err = util.DialUpstream(ctx, nil, "tcp", addr)
	}()
	t := time.NewTicker(handshakeRecheck)
	defer t.Stop()
	for {
		select {
		case r := <-res:
			// The application may have given up while the dial was finishing
			// (a connect timeout shorter than the destination's answer):
			// check once more, so the destination is not left holding a
			// connection nobody will use.
			if r.err == nil {
				if _, err := lookup.GetForHandshake(ctx, client, proxy); errors.Is(err, agent.ErrConnectingSocketGone) {
					_ = r.conn.Close()
					p.logger.Debug("the application gave up on a held connection as its destination answered; closed the connection",
						zap.String("client", client.String()), zap.String("destination", addr))
					return nil, errHandshakeAbandoned
				}
			}
			return r.conn, r.err
		case <-t.C:
			// Only the socket's absence ends the dial; a lookup that failed
			// for any other reason (a netlink hiccup) says nothing about it.
			if _, err := lookup.GetForHandshake(ctx, client, proxy); errors.Is(err, agent.ErrConnectingSocketGone) {
				cancel()
				if r := <-res; r.conn != nil {
					_ = r.conn.Close()
				}
				p.logger.Debug("the application gave up on a held connection; stopped dialling its destination",
					zap.String("client", client.String()), zap.String("destination", addr))
				return nil, errHandshakeAbandoned
			}
		}
	}
}

// holdState is the handshake hold's lifecycle on a proxy: armed with the
// listener's context when the listener opens, started by the first session
// whose connections dial their destination, stopped when the listener
// closes. A replay served from mocks never dials, so it never loads the
// netfilter modules or adds a hook, and its handshakes complete in the
// kernel as before.
type holdState struct {
	mu   sync.Mutex
	ctx  context.Context // nil until armed, and again once stopped
	stop func()          // non-nil once started
	// tried is the session the last attempt to start was for: each session
	// tries at most once, so a host where holding cannot work pays for it
	// once per session, not once per call.
	tried *agent.Session
	// reported: the reason it could not start has been logged at Info; a
	// later session tries again, and logs a failure only at Debug.
	reported bool
}

// armHandshakeHold records the listener's context and starts holding if the
// current session already needs it.
func (p *Proxy) armHandshakeHold(ctx context.Context) {
	p.hold.mu.Lock()
	p.hold.ctx = ctx
	p.hold.mu.Unlock()
	p.ensureHandshakeHold(p.getSession())
}

// ensureHandshakeHold starts holding handshakes for rule, the session about
// to take effect, if its connections dial their destination and the
// listener is open.
func (p *Proxy) ensureHandshakeHold(rule *agent.Session) {
	if rule == nil || !p.dialsUpstream(rule) {
		return
	}
	p.hold.mu.Lock()
	defer p.hold.mu.Unlock()
	if p.hold.stop != nil || p.hold.ctx == nil || p.hold.ctx.Err() != nil || p.hold.tried == rule {
		return
	}
	p.hold.tried = rule
	p.hold.stop = p.startHandshakeHold(p.hold.ctx, !p.hold.reported)
	if p.hold.stop == nil {
		p.hold.reported = true // tried again with the next session
	}
}

// stopHandshakeHold stops holding for good: the listener has closed.
func (p *Proxy) stopHandshakeHold() {
	p.hold.mu.Lock()
	stop := p.hold.stop
	p.hold.ctx = nil
	p.hold.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// startHandshakeHold holds the handshakes to the proxy's listener until the
// returned stop is called; once ctx ends, the decisions still in flight end
// and new ones accept at once. Where it cannot (no nf_tables or
// NFQUEUE in the kernel, no CAP_NET_ADMIN, no sock_diag, a DestInfo that
// cannot name a connection before it is established), handshakes complete at
// once, as they always did, and the reason is logged once.
//
// The decisions in flight end with ctx, so a SYN held at shutdown is
// answered at once rather than left for the application's next
// retransmission; the caller stops holding when its listener closes.
//
// It returns nil when it could not start, saying why at Info when report is
// set (the first time) and at Debug otherwise.
func (p *Proxy) startHandshakeHold(ctx context.Context, report bool) (stop func()) {
	notHeld := func(reason string, err error) func() {
		log := p.logger.Debug
		// Off Linux it is not a failure: holding handshakes is Linux-only.
		if report && runtime.GOOS == "linux" {
			log = p.logger.Info
		}
		log("could not hold the application's connections until their destination answers, so a "+
			"dependency that refuses, is unreachable, or does not answer is seen by the application as a "+
			"connection that opened and then closed (EOF), not as the connect error it would get without keploy",
			zap.String("reason", reason), zap.Error(err),
			zap.String("needs", "nf_tables with the nft_queue and nft_reject_inet modules, nfnetlink_queue, "+
				"sock_diag (inet_diag, tcp_diag), and CAP_NET_ADMIN"))
		return nil
	}
	if p.DisableHandshakeHold {
		p.logger.Debug("holding handshakes is turned off (--disable-handshake-hold); each connection is accepted at once")
		return func() {}
	}
	lookup, ok := p.DestInfo.(agent.HandshakeDestInfo)
	if !ok {
		return notHeld("the connection hooks in use cannot name a connection's destination during its handshake", nil)
	}
	if err := lookup.CheckHandshakeLookup(ctx); err != nil {
		return notHeld("sock_diag cannot name a connecting socket", err)
	}
	h, err := synhold.Start(ctx, p.logger, uint16(p.Port), p.decideHandshake(lookup))
	if err != nil {
		return notHeld("could not start holding handshakes", err)
	}
	p.logger.Debug("holding each connection's handshake until its destination answers",
		zap.Uint32("proxyPort", p.Port))

	done := make(chan struct{})
	var once sync.Once
	stop = func() {
		once.Do(func() {
			close(done)
			if err := h.Close(); err != nil {
				p.logger.Debug("failed to remove the handshake-hold rules", zap.Error(err))
			}
			p.predials.closeAll()
		})
	}
	go func() {
		defer utils.Recover(p.logger)
		t := time.NewTicker(predialTTL / 3)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-t.C:
				p.predials.reap(now)
			}
		}
	}()
	return stop
}
