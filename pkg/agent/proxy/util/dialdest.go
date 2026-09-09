// Package util provides utility functions for the proxy package.
package util

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/neterr"
	"go.uber.org/zap"
)

// loopbackFallbackTimeout bounds the counterpart dial.
//
// The first dial failed with ECONNREFUSED, which on loopback is one round
// trip, so the caller has effectively paid nothing yet. The counterpart is not
// guaranteed to be as quick: a host that DROPs traffic to ::1 (an ip6tables
// rule, or IPv6 blackholed rather than absent) turns the retry into a full TCP
// connect timeout — measured at ~130s with tcp_syn_retries=6 — and this sits on
// the proxy's hot path, once per connection. A refusal that is genuinely
// instant is worth waiting a moment for; anything slower is not a loopback
// listener worth having.
const loopbackFallbackTimeout = 2 * time.Second

// loopbackFallbackReported keeps the notice to one line per process. A fallback
// is a property of the node's topology (a listener bound to one address family
// behind a name that resolves to both), not of any one connection, so
// repeating it per connection would be noise on exactly the setup where every
// connection takes that path.
//
// An atomic.Bool rather than a sync.Once because tests must be able to reset
// it, and reassigning a package-level sync.Once is a non-atomic write that
// races live connections.
var loopbackFallbackReported atomic.Bool

// DialTarget is a destination to dial on the application's behalf.
type DialTarget struct {
	Addr string

	// Fabricated marks Addr as a stand-in the capture layer synthesized because
	// it could not resolve the connection's real destination — see
	// models.ConditionalDstCfg.AddrFabricated, which states that such an
	// address does not point at the server this connection talked to.
	//
	// It suppresses the counterpart retry, and must. The canonical fabricated
	// value is literally "127.0.0.1:3306", so inferring a counterpart for it
	// would take the documented SAFE outcome for a stand-in (refused instantly,
	// capture aborts loudly) and turn it into the documented DANGEROUS one
	// (silently connect to an unrelated local server and record its traffic as
	// the dependency's).
	Fabricated bool
}

// DialDestination dials the application's original destination, restoring the
// address-family fallback that interception removed.
//
// An application resolving a dual-stack name like "localhost:3306" may try
// [::1] first. Left alone, a refusal there makes it immediately try 127.0.0.1 —
// that fallback is what lets such a name work against an IPv4-only listener,
// which is what a Docker published port is by default.
//
// Interception removes it. The redirect makes the application's connect to
// [::1] SUCCEED, because it is now connected to this proxy rather than to the
// dependency, so it has nothing to fall back from. The proxy then dials the
// literal captured address; when nothing listens on that family the connection
// is closed and the application sees EOF instead of ECONNREFUSED. EOF is
// unrecoverable — it looks like a server that accepted and hung up — so drivers
// report a dead connection rather than trying the other address.
//
// The retry fires only on ECONNREFUSED, which is what makes this a
// family fallback and not a general-purpose retry: a refusal means nothing is
// listening on the requested family at all, so no live service is being
// passed over. A timeout or a reset is a different condition and is left alone.
//
// What this is NOT: a proof that the application would itself have tried the
// counterpart. That stdlib fallback is name-driven — it happens because
// "localhost" resolved to both addresses and the dialer walks the resolved set.
// An application that dialed the literal [::1] gets no fallback at all, and
// eBPF hands the proxy an ADDRESS, not the name behind it, so the two cases are
// indistinguishable here. For 127.0.0.1 and ::1 specifically the trade is still
// worth making — a literal-loopback dial to a family nothing serves is a
// configuration accident, and the alternative is an unrecoverable EOF — but it
// is a judgement, not a proof.
func DialDestination(ctx context.Context, logger *zap.Logger, network string, target DialTarget) (net.Conn, error) {
	return DialDestinationWith(ctx, logger, target, func(ctx context.Context, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	})
}

// DialDestinationTLS is DialDestination for a TLS upstream. A dependency
// reached over TLS breaks in exactly the same way and needs the same retry;
// leaving it out would make the fix hold only for plaintext.
func DialDestinationTLS(ctx context.Context, logger *zap.Logger, network string, target DialTarget, cfg *tls.Config) (net.Conn, error) {
	requestedHost, _, _ := net.SplitHostPort(target.Addr)
	return DialDestinationWith(ctx, logger, target, func(ctx context.Context, addr string) (net.Conn, error) {
		d := &tls.Dialer{Config: tlsConfigForAddr(cfg, requestedHost, addr)}
		return d.DialContext(ctx, network, addr)
	})
}

// tlsConfigForAddr re-points an explicit ServerName at the address actually
// being dialed.
//
// When upstream verification is on, callers set ServerName from the REQUESTED
// host (see resolveUpstreamServerName). Carrying that onto the counterpart
// would verify the certificate against an address this connection did not
// reach — a dependency whose certificate covers 127.0.0.1 would fail with
// "certificate is valid for 127.0.0.1, not ::1" — so the fallback would never
// complete on a verifying setup. Only an explicit ServerName equal to the
// requested host is rewritten: a caller that set something else (a real
// hostname carried over from the client's SNI) meant it, and an empty one is
// left empty so the stdlib infers it from the address it dials.
func tlsConfigForAddr(cfg *tls.Config, requestedHost, addr string) *tls.Config {
	if cfg == nil || cfg.ServerName == "" || cfg.ServerName != requestedHost {
		return cfg
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == requestedHost {
		return cfg
	}
	c := cfg.Clone()
	c.ServerName = host
	return c
}

// DialDestinationWith runs the CALLER'S own dial against the application's
// destination, adding the loopback family fallback to it.
//
// Sites with their own dialer settings — a deadline, a timeout, a tls.Config,
// keepalives — use this rather than DialDestination, so adding the fallback
// can never silently discard them. The retry re-runs the same dial function
// against the counterpart address, so it inherits those settings too.
func DialDestinationWith(ctx context.Context, logger *zap.Logger, target DialTarget,
	dial func(context.Context, string) (net.Conn, error)) (net.Conn, error) {

	conn, err := dial(ctx, target.Addr)
	if err == nil || !neterr.IsConnRefused(err) || target.Fabricated {
		return conn, err
	}

	alt, ok := loopbackCounterpart(target.Addr)
	if !ok {
		return nil, err
	}

	altCtx, cancel := context.WithTimeout(ctx, loopbackFallbackTimeout)
	defer cancel()

	altConn, altErr := dial(altCtx, alt)
	if altErr != nil {
		// Lead with the ORIGINAL failure — the counterpart is this proxy's
		// inference, and naming it alone would send the reader to an address
		// their application never used — but carry the counterpart's error with
		// it. Swallowing that is how "the counterpart answered and the
		// handshake failed its certificate check" gets reported as "nothing is
		// listening", which is the same misdirection this change exists to
		// remove, one level down. %w keeps errors.Is(err, ECONNREFUSED) true
		// for callers that classify.
		return nil, fmt.Errorf("%w (loopback counterpart %s also failed: %v)", err, alt, altErr)
	}

	if logger != nil && loopbackFallbackReported.CompareAndSwap(false, true) {
		logger.Info("proxy: the dependency is reachable on only one loopback address family, so the dial "+
			"fell back to the other — the fallback an application resolving a dual-stack name would have "+
			"made itself had its connect not been intercepted. Bind the dependency on both families to remove the extra dial",
			zap.String("requested", target.Addr), zap.String("connected", alt))
	}
	return altConn, nil
}

// loopbackCounterpart returns the same port on the other loopback address
// family, and whether addr had one at all.
//
// Matched on the EXACT addresses, never on net.IP.IsLoopback: that is true for
// the whole of 127.0.0.0/8, and 127.0.0.2 is a distinct host from 127.0.0.1 —
// a dial to one does not reach a listener on the other. Treating them as
// interchangeable would silently connect an application to a different server
// and record its traffic as the dependency's, which is the shape people get
// from running several database instances on 127.0.0.1, 127.0.0.2, 127.0.0.3.
// ::1 is IPv6's only loopback, so 127.0.0.1 <-> ::1 is the entire relation.
func loopbackCounterpart(addr string) (string, bool) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", false
	}
	// Equal compares by value across 4-byte and 16-byte forms, so the 4-in-6
	// spelling (::ffff:127.0.0.1) matches the IPv4 arm — correct, because that
	// form reaches an IPv4 listener.
	switch {
	case ip.Equal(net.IPv4(127, 0, 0, 1)):
		return net.JoinHostPort("::1", port), true
	case ip.Equal(net.IPv6loopback):
		return net.JoinHostPort("127.0.0.1", port), true
	}
	return "", false
}
