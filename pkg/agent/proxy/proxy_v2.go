package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// newRelayDisabled reports whether the new supervisor+relay architecture
// is disabled via environment. Set KEPLOY_NEW_RELAY to 0/false/off/no to
// force the legacy path even for parsers that implement IntegrationsV2.
// Any other value (or unset) enables the new path.
func newRelayDisabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("KEPLOY_NEW_RELAY")))
	return v == "0" || v == "false" || v == "off" || v == "no"
}

// recordViaSupervisor runs a V2-capable parser's RecordOutgoing inside
// the new supervisor + relay architecture.
//
// Responsibilities:
//
//  1. Stand up a Relay that owns srcConn and dstConn as sole writer; tees
//     timestamped Chunks into a pair of FakeConns.
//  2. Stand up a Supervisor that wraps the parser call with a panic
//     firewall, an activity-based hang watchdog, and a per-connection
//     memory cap.
//  3. Build a supervisor.Session exposing the FakeConns, directive
//     channels, and legacy fields; attach the Session to RecordSession.V2
//     so the parser receives both surfaces through its existing
//     RecordOutgoing signature.
//  4. Invoke sv.Run; on a FallthroughToPassthrough result, drop the
//     parser and run globalPassThrough on the real sockets so user
//     traffic continues regardless of the parser's fate.
//
// The caller remains responsible for closing srcConn/dstConn in its
// deferred cleanup; this helper never closes them.
func (p *Proxy) recordViaSupervisor(
	ctx context.Context,
	srcConn, dstConn net.Conn,
	parser integrations.Integrations,
	parserType integrations.IntegrationType,
	mocks chan<- *models.Mock,
	errGrp *errgroup.Group,
	logger *zap.Logger,
	clientConnID, destConnID int64,
	opts models.OutgoingOptions,
) error {
	sv := supervisor.New(supervisor.Config{
		Logger: logger,
		// Leave HangBudget / MemCap / PanicReporter defaulted by the
		// supervisor package. Tune via config once we have production
		// telemetry on the fallback rate.
	})
	defer sv.Close()

	// Build the relay. It owns srcConn/dstConn for the duration of its
	// Run call but never closes them. The caller's deferred Close still
	// runs on handleConnection return.
	r := relay.New(relay.Config{
		Logger: logger,
		// MemoryGuardCheck defaults to memoryguard.IsRecordingPaused.
		// PerConnCap / TeeChanBuf defaulted.
		TLSUpgradeFn: newProxyTLSUpgradeFn(&srcConn, &dstConn, logger),
		BumpActivity: sv.BumpActivity,
	}, srcConn, dstConn)

	// Parser-facing session carries the FakeConns and directive channels
	// from the relay plus the legacy fields that un-migrated parser
	// paths still consult. Ctx is overwritten by Supervisor.Run with the
	// supervised lifetime context.
	svSess := &supervisor.Session{
		ClientStream: r.ClientStream(),
		DestStream:   r.DestStream(),
		Directives:   r.Directives(),
		Acks:         r.Acks(),
		Mocks:        mocks,
		Logger:       logger,
		ClientConnID: fmt.Sprint(clientConnID),
		DestConnID:   fmt.Sprint(destConnID),
		Opts:         opts,
		// Legacy fields kept populated so a migrated parser can still
		// consult them for fields we haven't promoted yet. The parser
		// must not touch Ingress/Egress net.Conn values on the V2 path.
		TLSUpgrader: nil,
		ErrGroup:    errGrp,
	}

	// Run the relay in its own goroutine under the supervisor's lifetime.
	// The supervisor's Close (via sv.SessionOnAbort below) closes the
	// FakeConns so the parser's reads unblock on abort.
	relayDone := make(chan error, 1)
	relayCtx, relayCancel := context.WithCancel(ctx)
	defer relayCancel()
	go func() { relayDone <- r.Run(relayCtx) }()

	sv.SessionOnAbort = func() {
		// Unblock the parser's ClientStream/DestStream reads; the
		// relay keeps running for passthrough drain.
		_ = r.ClientStream().Close()
		_ = r.DestStream().Close()
	}

	// Adapter: the parser's RecordOutgoing takes *integrations.RecordSession
	// but the supervisor's ParserFunc takes *supervisor.Session. Build a
	// RecordSession whose V2 field points at the supervisor.Session.
	result := sv.Run(ctx, func(parserCtx context.Context, sv2sess *supervisor.Session) error {
		recSess := &integrations.RecordSession{
			// Parsers on V2 must NOT touch Ingress/Egress/TLSUpgrader/
			// ErrGroup; leaving them nil keeps a bug loud. The V2
			// surface is the only legal one on this path.
			Ingress:      nil,
			Egress:       nil,
			Mocks:        mocks,
			ErrGroup:     errGrp,
			TLSUpgrader:  nil,
			Logger:       logger,
			ClientConnID: fmt.Sprint(clientConnID),
			DestConnID:   fmt.Sprint(destConnID),
			Opts:         opts,
			V2:           sv2sess,
		}
		return parser.RecordOutgoing(parserCtx, recSess)
	}, svSess)

	// Stop the relay and drain any error. ctx cancellation above is
	// idempotent. The relay returns cleanly once both forwarders exit
	// (either on conn close by the caller or on ctx cancel here).
	relayCancel()
	relayErr := <-relayDone
	if relayErr != nil && !errors.Is(relayErr, context.Canceled) {
		logger.Debug("relay exited with error", zap.Error(relayErr))
	}

	if result.FallthroughToPassthrough {
		logger.Warn("parser supervisor triggered passthrough fallback",
			zap.String("parser", string(parserType)),
			zap.String("status", result.Status.String()),
			zap.Error(result.Err),
		)
		// globalPassThrough opens a raw copy between srcConn and dstConn.
		// NOTE: bytes the relay already consumed before the fallback
		// are irrecoverable; the relay drained them into the tee where
		// the parser either processed them or dropped them. Protocols
		// with mid-stream framing may observe corruption on the very
		// next bytes. This is the documented trade-off in PLAN.md §3.3:
		// stability over fidelity. Clients retry or surface errors.
		if err := p.globalPassThrough(ctx, srcConn, dstConn); err != nil {
			utils.LogError(logger, err, "globalPassThrough after supervisor fallback failed")
			return err
		}
		return nil
	}

	if result.Err != nil {
		if isNetworkClosedErr(result.Err) {
			logger.Debug("V2 parser exited with network-closed error", zap.Error(result.Err))
			return nil
		}
		return result.Err
	}
	return nil
}

// newProxyTLSUpgradeFn adapts keploy's existing TLS helpers into the
// relay.TLSUpgradeFn shape. The returned function:
//
//   - For isClient=true (upgrading the destination side — keploy
//     acts as TLS client to the real server), dials TLS over the
//     existing conn using tls.Client and performs the handshake.
//   - For isClient=false (upgrading the client side — keploy acts
//     as TLS server presenting the MITM cert), hands off to
//     pTls.HandleTLSConnection which already implements the server-
//     side handshake used elsewhere in the proxy.
//
// The conn pointer update (so the forwarders switch to the upgraded
// conn on subsequent iterations) is the relay's responsibility; this
// fn only performs the handshake and returns the new net.Conn.
func newProxyTLSUpgradeFn(srcPtr, dstPtr *net.Conn, logger *zap.Logger) relay.TLSUpgradeFn {
	return func(ctx context.Context, conn net.Conn, isClient bool, cfg *tls.Config) (net.Conn, error) {
		if cfg == nil {
			return conn, nil
		}
		if isClient {
			tlsConn := tls.Client(conn, cfg)
			if err := tlsConn.HandshakeContext(ctx); err != nil {
				return nil, fmt.Errorf("dest TLS handshake failed: %w", err)
			}
			return tlsConn, nil
		}
		// Server side: reuse the existing proxy TLS server plumbing.
		// HandleTLSConnection performs the TLS handshake presenting
		// keploy's MITM cert chain. It returns the TLS-wrapped conn
		// which we use for subsequent plaintext forwarding. The
		// backdate argument is left zero; the helper clamps it to
		// "now" internally.
		wrapped, _, err := pTls.HandleTLSConnection(ctx, logger, conn, time.Time{})
		if err != nil {
			return nil, fmt.Errorf("client TLS handshake failed: %w", err)
		}
		return wrapped, nil
	}
}

// Compile-time sanity: ensure the dispatcher-side V2 call site can be
// resolved. This guards against the package moving out from under us.
var _ = fakeconn.FromClient
var _ directive.Kind = directive.KindUpgradeTLS
var _ = util.DefaultKillSwitch

// waitForConnDrain blocks until either all tracked client connections
// have closed or ctx is done (typically a 5-second shutdown grace).
// Called from StopProxyServer after the listener is closed and the
// kill switch is tripped. Poll-based rather than channel-based
// because p.clientConnections is a slice of raw net.Conn values we
// don't own lifecycle hooks on; we just watch the count.
func (p *Proxy) waitForConnDrain(ctx context.Context) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		p.connMutex.Lock()
		n := len(p.clientConnections)
		p.connMutex.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-ctx.Done():
			p.logger.Debug("shutdown drain grace expired; force-closing remaining connections",
				zap.Int("remaining", n))
			return
		case <-t.C:
		}
	}
}
