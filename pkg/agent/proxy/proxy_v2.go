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
//     parser and keep the existing relay forwarding raw bytes on the
//     real sockets until peer close so user traffic continues
//     regardless of the parser's fate. (Critically we do NOT call
//     globalPassThrough here: that would create a gap between the
//     relay stopping and a replacement read loop starting — exactly
//     the kind of stall the V2 architecture is meant to eliminate.
//     See invariant I1 in pkg/agent/proxy/README.md.)
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

	// Parser-facing session is constructed before the relay so its
	// MarkMockIncomplete / ClearPendingWork hooks can be wired into
	// the relay's tee callbacks below. ClientStream/DestStream and
	// the directive channels are patched in after r := relay.New().
	// Ctx is overwritten by Supervisor.Run with the supervised
	// lifetime context.
	svSess := &supervisor.Session{
		Mocks:            mocks,
		Logger:           logger,
		ClientConnID:     fmt.Sprint(clientConnID),
		DestConnID:       fmt.Sprint(destConnID),
		Opts:             opts,
		OnPendingCleared: sv.ClearPendingWork,
		// Legacy fields kept populated so a migrated parser can still
		// consult them for fields we haven't promoted yet. The parser
		// must not touch Ingress/Egress net.Conn values on the V2 path.
		TLSUpgrader: nil,
		ErrGroup:    errGrp,
	}

	// Build the relay. It owns srcConn/dstConn for the duration of its
	// Run call but never closes them. The caller's deferred Close still
	// runs on handleConnection return.
	//
	// OnMarkMockIncomplete wires the relay's drop signals (memoryguard
	// pressure / per-conn cap / channel full / write error / short
	// write / KindAbortMock directive) to the session's incomplete
	// flag so EmitMock drops any mock whose underlying tee chunks were
	// lost. Without this wiring partial mocks could still ship despite
	// the documented invariant I4 in PLAN.md.
	//
	// OnClientChunkTeed wires the relay's per-chunk "client bytes
	// delivered to parser" signal to the supervisor's pending-work
	// flag so the activity watchdog can distinguish an idle
	// connection (no pending requests) from a parser that received
	// bytes but isn't emitting a mock (hang candidate). EmitMock's
	// OnPendingCleared clears the flag after each successful emit.
	r := relay.New(relay.Config{
		Logger:               logger,
		TLSUpgradeFn:         newProxyTLSUpgradeFn(logger),
		BumpActivity:         sv.BumpActivity,
		OnMarkMockIncomplete: svSess.MarkMockIncomplete,
		OnClientChunkTeed:    sv.MarkPendingWork,
	}, srcConn, dstConn)

	svSess.ClientStream = r.ClientStream()
	svSess.DestStream = r.DestStream()
	svSess.Directives = r.Directives()
	svSess.Acks = r.Acks()

	// Run the relay in its own goroutine under the supervisor's lifetime.
	// The supervisor's Close (via sv.SessionOnAbort below) closes the
	// FakeConns so the parser's reads unblock on abort.
	relayDone := make(chan error, 1)
	relayCtx, relayCancel := context.WithCancel(ctx)
	defer relayCancel()
	go func() { relayDone <- r.Run(relayCtx) }()

	sv.SessionOnAbort = func() {
		// Pause the tees FIRST so every subsequent chunk drops
		// cheaply via the pause fast-path (atomic-bool check) instead
		// of falling through to the channel-full DropChannelFull
		// branch, which also logs at Debug. On a long-lived
		// post-abort connection the spam would otherwise be one
		// log line per chunk for the rest of the connection.
		//
		// Pausing does NOT stop the real-socket forwarders — every
		// byte still reaches its peer. The relay's raw forwarding
		// continues until peer close; only parser-side delivery is
		// suppressed.
		r.PauseTees()

		// Then unblock the parser's ClientStream/DestStream reads so
		// the supervisor's cancel-select can observe the parser
		// goroutine exiting promptly.
		_ = r.ClientStream().Close()
		_ = r.DestStream().Close()
	}

	// Adapter: the parser's RecordOutgoing takes *integrations.RecordSession
	// but the supervisor's ParserFunc takes *supervisor.Session. Build a
	// RecordSession whose V2 field points at the supervisor.Session.
	//
	// On the V2 path, Ingress/Egress/TLSUpgrader are intentionally nil so
	// that a parser bug that reaches for the legacy fields surfaces as an
	// obvious nil panic (which the supervisor catches) rather than a
	// silent misuse of sockets the relay owns. ErrGroup remains populated
	// because the legacy integration helper layer (ReadBytes, etc.) still
	// retrieves it via context.Value in shared code; once every parser
	// migrates off that accessor the field will be set to nil here as
	// well.
	result := sv.Run(ctx, func(parserCtx context.Context, sv2sess *supervisor.Session) error {
		recSess := &integrations.RecordSession{
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

	if result.FallthroughToPassthrough {
		logger.Debug("parser supervisor triggered passthrough fallback; relay continues raw forwarding until peer close",
			zap.String("parser", string(parserType)),
			zap.String("status", result.Status.String()),
			zap.Error(result.Err),
			zap.String("next_step", "set KEPLOY_NEW_RELAY=off to force legacy path for this parser, or KEPLOY_DISABLE_PARSING=1 to disable record parsing entirely"),
		)
		// Crucial invariant (I1): the relay keeps forwarding client↔dest
		// bytes end-to-end during the fallback. We do NOT cancel it here
		// — cancelling would introduce a gap between the relay stopping
		// and any replacement read loop starting, exactly the kind of
		// stall the V2 architecture is meant to eliminate.
		//
		// SessionOnAbort has already closed the FakeConns so no further
		// tee chunks reach the parser side (no partial mocks, I4). The
		// relay's forwarder goroutines continue draining srcConn/dstConn
		// until either peer closes the connection, which triggers a
		// normal Run exit.
		<-relayDone
		return nil
	}

	// Non-fallthrough path: parser returned normally or with an error.
	// Cancel the relay and drain.
	relayCancel()
	relayErr := <-relayDone
	if relayErr != nil && !errors.Is(relayErr, context.Canceled) {
		logger.Debug("relay exited with error", zap.Error(relayErr))
	}

	if result.Err != nil {
		if isNetworkClosedErr(result.Err) {
			logger.Debug("V2 parser exited with network-closed error", zap.Error(result.Err))
			return nil
		}
		return result.Err
	}
	logger.Debug("V2 parser recorded outgoing message successfully",
		zap.String("parser", string(parserType)),
		zap.String("status", result.Status.String()),
	)
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
// fn only performs the handshake and returns the new net.Conn —
// hence it does not need the caller's *net.Conn handles.
func newProxyTLSUpgradeFn(logger *zap.Logger) relay.TLSUpgradeFn {
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

// waitForConnDrain blocks until either every in-flight
// handleConnection goroutine has returned or ctx is done (typically
// a 5-second shutdown grace). Called from StopProxyServer after the
// listener is closed and the kill switch is tripped.
//
// Implementation: each handleConnection invocation calls
// activeConns.Add(1)/Done(), so a single Wait() drains the whole
// active set. We can't wait on a WaitGroup with a deadline
// directly, so a sentinel goroutine closes a done channel when Wait
// returns and we select on that vs ctx. After ctx-done we leave the
// remaining goroutines to exit via the parent ctx cancellation they
// already inherited (their deferred srcConn/dstConn closes fire on
// their own return).
func (p *Proxy) waitForConnDrain(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		p.activeConns.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-ctx.Done():
		p.logger.Debug("shutdown drain grace expired; remaining connections will exit via ctx cancellation")
		return
	}
}
