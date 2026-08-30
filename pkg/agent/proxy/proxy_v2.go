package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const (
	// orphanIdleGrace is how long a retired connection must carry no bytes
	// before its suppression window is closed. Longer than any plausible
	// gap WITHIN one app request (the window must never close mid-request
	// and let a half-covered test case through), short enough that a
	// connection idling between bursts stops suppressing quickly.
	orphanIdleGrace = 1 * time.Second

	// orphanIdleCheck is how often idleness is re-evaluated. The window can
	// therefore over-cover by up to orphanIdleCheck past the true idle
	// point, which errs toward suppressing — the safe direction.
	orphanIdleCheck = 250 * time.Millisecond
)

// orphanWindowOpener is the slice of supervisor.Session that trackOrphanWhileActive
// needs, so the loop can be tested without a live connection.
type orphanWindowOpener interface {
	OpenOrphanWindow(start time.Time) func()
}

// trackOrphanWhileActive keeps a suppression window open only while a retired
// connection is actually carrying bytes, and closes it whenever the connection
// falls idle.
//
// A retired connection can no longer be captured, but that only costs a test
// case if the app USED it while serving that test case. A single window held
// from the fallthrough to end-of-session says "everything after this point is
// unreliable", which for a connection that then sat idle for ten minutes
// throws away ten minutes of perfectly good recording — and on a fallthrough
// early in a run, the entire run. Following activity instead suppresses the
// spans that can really have lost a mock and nothing else.
//
// It always returns with its window closed. stop must be closed by the caller
// once the connection has ended.
// idleGrace and checkEvery are parameters rather than the package constants
// so the loop is testable in milliseconds; production passes
// orphanIdleGrace / orphanIdleCheck.
func trackOrphanWhileActive(stop <-chan struct{}, sess orphanWindowOpener, lastForwardNanos *atomic.Int64, idleGrace, checkEvery time.Duration) {
	closeWindow := sess.OpenOrphanWindow(time.Now())
	defer func() {
		if closeWindow != nil {
			closeWindow()
		}
	}()

	ticker := time.NewTicker(checkEvery)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case now := <-ticker.C:
			idle := now.Sub(time.Unix(0, lastForwardNanos.Load())) > idleGrace
			switch {
			case idle && closeWindow != nil:
				closeWindow()
				closeWindow = nil
			case !idle && closeWindow == nil:
				// Traffic resumed on a connection that still cannot be
				// captured. Reopen from the RESUME instant, not from now:
				// the tick only tells us traffic happened at some point in
				// the last checkEvery, and bytes moved from
				// lastForwardNanos onwards. Opening at `now` would leave up
				// to checkEvery of live traffic uncovered, and a request
				// served entirely inside that hole is exactly the
				// mock-less test case this exists to catch.
				closeWindow = sess.OpenOrphanWindow(time.Unix(0, lastForwardNanos.Load()))
			}
		}
	}
}

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
		// A parser that identifies its in-flight request as a long-poll
		// async lane calls this to disarm the hang watchdog for the
		// connection (the poll makes no byte progress for far longer than
		// the hang budget). See supervisor.SuspendWatchdog.
		SuspendWatchdog: sv.SuspendWatchdog,
		// Route EmitMock through the SyncMockManager (obtained via
		// syncMock.Get(), then mgr.AddMock) so V2 parsers pick up the
		// same firstReqSeen session-window buffering, lifetime
		// derivation, and drop accounting that legacy parsers (http,
		// mysql, generic) get. Without this, V2-recorded mocks
		// captured before the first app test request bypass the
		// session window and are lost at replay — the symptom that
		// broke postgres e2e record-build-replay-build runs in
		// integrations#133 (the app's startup DB queries never found
		// their mocks).
		RouteMocksViaSyncMock: true,
		// Per-app manager carried on the parser ctx by a multi-app caller
		// (nil otherwise ⇒ EmitMock uses the package-global). Routes V2
		// parser mocks to the right app's manager, like the legacy parsers.
		Mgr: syncMock.FromContext(ctx),
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
	// Opt-in pre-dispatch pause: parsers that need to deterministically
	// observe the first chunk on a connection before any byte reaches
	// the real peer implement the WantsPreDispatchPause capability.
	// Today only postgres v3 sets this (to close the SSL preamble
	// race; see keploy/enterprise#2012). Most parsers don't need it
	// and would deadlock if their first action wasn't a ResumePreDispatch
	// directive — so we ONLY engage pre-dispatch when the parser
	// explicitly asks for it via this opt-in.
	//
	// Duck-typed instead of extending the IntegrationsV2 interface so
	// each parser stays free to add the method independently and we
	// don't have to touch every IntegrationsV2 implementation in this
	// change.
	var preDispatchPause bool
	if pp, ok := parser.(interface{ WantsPreDispatchPause() bool }); ok {
		preDispatchPause = pp.WantsPreDispatchPause()
	}

	// Opt-in gap resync. See [parserCanResyncAfterGap].
	canResyncAfterGap := parserCanResyncAfterGap(parser)

	// RealCertHook wires the V2-relay upstream-TLS chokepoint into
	// the cbshim. The post-handshake upgradeDstConn carries the real
	// upstream cert; we publish (connID = source-port-as-string,
	// realDER, sigAlgo) so cbshim can pair it with the MITM cert from
	// CertForClient. Nil-safe: p.cbshim is nil when BPF cbshim
	// failed to load, in which case the relay just skips publishing.
	var realCertHook func(connID string, realDER []byte, sigAlgo x509.SignatureAlgorithm)
	if p.cbshim != nil {
		realCertHook = p.cbshim.RegisterReal
	}

	// lastForwardNanos is when this connection last moved a byte in either
	// direction, stamped by the wrapped BumpActivity below. Read by the
	// activity-scoped suppression loop.
	var lastForwardNanos atomic.Int64
	lastForwardNanos.Store(time.Now().UnixNano())

	// ONE suppression tracker per connection, started by whichever cause
	// fires first and stopped when the connection ends.
	//
	// Both causes — a tee desync and a retired parser — leave the connection
	// in the same state: it can no longer be captured. So they share one
	// activity-scoped window rather than opening two. They are also not
	// independent: the incident's chain was memory-pressure drop → parser
	// starves mid-frame → hang watchdog → passthrough fallthrough, so the
	// DESYNC fires first. Giving it a plain open-ended window of its own
	// would subsume the fallthrough's scoped one (WasMockOrphanedInWindow
	// ORs the ranges) and silently restore the whole-session suppression
	// this design exists to avoid.
	var (
		trackerOnce sync.Once
		trackerStop = make(chan struct{})
	)
	startOrphanTracking := func() {
		trackerOnce.Do(func() {
			go trackOrphanWhileActive(trackerStop, svSess, &lastForwardNanos, orphanIdleGrace, orphanIdleCheck)
		})
	}
	// Safe whether or not the tracker ever started — closing a channel with
	// no reader is a no-op — and it guarantees the goroutine cannot outlive
	// the connection.
	defer close(trackerStop)

	r := relay.New(relay.Config{
		Logger: logger,
		// verify / rootCAs / srcConn come from record.upstreamTls.* and are
		// captured once per connection: the relay calls the returned fn with
		// no options in hand. With the flag unset this is the historic
		// InsecureSkipVerify=true dial, unchanged.
		TLSUpgradeFn: newProxyTLSUpgradeFn(logger, opts.UpstreamTLSVerify, opts.UpstreamTLSRootCAs, srcConn),
		// Only when verification is on: run the client-side handshake first so
		// the application's SNI is captured before keploy dials the upstream it
		// has to verify. Off (the default) keeps the historic dest-first order
		// untouched — a dest-side failure is survivable there. See
		// relay.Config.ClientTLSFirst.
		ClientTLSFirst: opts.UpstreamTLSVerify,
		// Wrapped to also stamp when this connection last carried bytes.
		// After a parser is retired the relay keeps forwarding, so this is
		// the signal that says WHEN the dead connection was actually in
		// use — which is what bounds the suppression window below to the
		// periods that can really have cost a mock.
		BumpActivity: func() {
			sv.BumpActivity()
			lastForwardNanos.Store(time.Now().UnixNano())
		},
		OnMarkMockIncomplete: svSess.MarkMockIncomplete,
		OnClientChunkTeed:    sv.MarkPendingWork,
		RealCertHook:         realCertHook,
		// A hole in this connection's capture. Remember when it opened so
		// the teardown below can mark the whole span: from here on the
		// parser frames from the wrong offset and emits nothing, so every
		// test case overlapping the span must be suppressed rather than
		// shipped mock-less (replay would report match_phase=no_mocks).
		// Suppression is what closes the failure by construction —
		// retirement below only restores capture for what follows.
		OnCaptureDesync: func(string) { startOrphanTracking() },
		// User-tunable record-buffer caps. Snapshotted onto the Proxy
		// at startup from config.Record.RecordBuffer (yaml/flag/env).
		// Zero values fall through to relay package defaults via
		// withDefaults() — preserving the zero-config path.
		PerConnCap:         p.recordBufferCap,
		TeeChanBuf:         p.recordBufferQueueSize,
		ConsumerStallGrace: p.recordBufferStallGrace,
		PreDispatchPause:   preDispatchPause,
		// Default false — see relay.Config.ParserCanResyncAfterGap for why
		// "cannot resync" is the safe default for everything that has not
		// explicitly claimed otherwise.
		ParserCanResyncAfterGap: canResyncAfterGap,
	}, srcConn, dstConn)

	svSess.ClientStream = r.ClientStream()
	svSess.DestStream = r.DestStream()
	svSess.Directives = r.Directives()
	svSess.Acks = r.Acks()
	svSess.PreClaimPauseBarrier = r.PreClaimPauseBarrier

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
		// A cancel that lands after the supervisor's grace period is the
		// NORMAL way a `keploy record` stop ends a live V2 connection —
		// the parser is blocked in FakeConn.Read, which observes only
		// Close and read deadlines, so it cannot return inside the grace.
		// There is no "rest of the connection" left to lose, so neither
		// the warning nor the suppression window applies: emitting them
		// would tell the user their recording is broken at the exact
		// moment they are reading the logs, on every clean stop.
		shuttingDown := result.Status == supervisor.StatusCanceled && ctx.Err() != nil

		if !shuttingDown {
			// Warn, not Debug. This is permanent, silent capture loss for
			// the rest of a connection's life, and several of the exits
			// that land here (StatusError, StatusMemCap) log nothing of
			// their own — so at Debug a whole recording could come back
			// unreplayable without a single line above Debug to explain
			// it. Bounded: one line per connection, only on a retired
			// parser.
			//
			// result.Err is deliberately NOT attached here. For
			// StatusPanicked it reads "supervisor: parser panic: ..."
			// (wrapPanic), and the memory-load lane scripts grep record.txt
			// for /panic:|fatal error:/ under `set -Eeuo pipefail` to catch
			// a CRASHED keploy. A recovered, structured, handled panic is
			// not a crash, but the token is the token: putting it in an
			// Info-level line would convert "some tests failed" into "Fatal
			// error detected in record.txt" and kill the lane before it
			// reaches its report. Status names the mechanism; the full
			// error follows at Debug.
			logger.Warn("parser retired; this connection can no longer be recorded and its test cases will be suppressed",
				zap.String("parser", string(parserType)),
				zap.String("status", result.Status.String()),
				zap.String("clientConnID", svSess.ClientConnID),
				zap.String("next_step", "user traffic is unaffected — the relay keeps forwarding raw bytes — but no further mock can be captured on this connection, so tests recorded against it are dropped rather than shipped unreplayable. Re-run with --debug for the underlying error. If this repeats, set KEPLOY_NEW_RELAY=off to force the legacy path for this parser, or KEPLOY_DISABLE_PARSING=1 to disable record parsing entirely"),
			)
		}
		logger.Debug("parser supervisor triggered passthrough fallback; relay continues raw forwarding until peer close",
			zap.String("parser", string(parserType)),
			zap.String("status", result.Status.String()),
			zap.Bool("shutting_down", shuttingDown),
			zap.Error(result.Err),
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
		//
		// That is correct for USER TRAFFIC and wrong for RECORDING, and
		// until now nothing said so. From this instant the connection
		// emits no further mock — there is no path anywhere that puts a
		// parser back on a live connection — while the app keeps issuing
		// requests over it perfectly happily. On a pooled client the peer
		// never closes, so "until peer close" means "until shutdown": every
		// test case recorded from here on is streamed with no mocks behind
		// it and fails replay with match_phase=no_mocks. Left silent, that
		// is indistinguishable from a healthy recording until replay.
		//
		// So mark the span un-capturable for as long as it lasts. The TC
		// suppressor in pkg/agent/routes/record.go drops every test case
		// whose window overlaps it, which is the same coverage-for-honesty
		// trade the memory-pressure and resync-hole suppressors already
		// make: a smaller recording in which every test replays, instead of
		// a larger one that lies.
		// The tracker goroutine owns its window and closes it on the way
		// out; `defer close(trackerStop)` at the top of this function is
		// what stops it, so the close survives a panic here rather than
		// only the happy path. An orphan window that never closes would
		// suppress every test case for the REST OF THE SESSION.
		if !shuttingDown {
			// Shared with the desync path — see startOrphanTracking. If a
			// tee already desynced this connection the tracker is running,
			// and this is a no-op rather than a second window.
			startOrphanTracking()
		}
		<-relayDone
		return nil
	}

	// Non-fallthrough path: parser returned normally or with an error.
	//
	// The parser has EXITED, so nothing will ever read these streams again.
	// Say so, rather than leaving the relay to infer it: closing the
	// FakeConns fires their Done() channels, which is what releases a tee
	// drain still holding chunks for a full out channel. Without this the
	// drain has no way to distinguish "parser is slow" from "parser is gone"
	// and has to wait out ConsumerStallGrace — on a path where the answer is
	// already known for certain. This is the same guarantee tokio gets for
	// free when a Receiver is dropped; Go has no goroutine-death event, so
	// the owner of the goroutine has to publish it.
	//
	// Ordering matters: this must precede <-relayDone, which is where the
	// relay waits for the drains.
	_ = r.ClientStream().Close()
	_ = r.DestStream().Close()

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
//     Upstream cert verification is skipped unless the operator opted
//     in — see the dest-side rationale on the cfg clone below.
//   - For isClient=false (upgrading the client side — keploy acts
//     as TLS server presenting the MITM cert), hands off to
//     pTls.HandleTLSConnection which already implements the server-
//     side handshake used elsewhere in the proxy.
//
// verify / rootCAs come from the session's OutgoingOptions
// (record.upstreamTls.verify / .caCert, resolved once in Proxy.New).
// They are captured at closure-construction time because the relay
// calls this fn per connection with no options in hand.
//
// srcConn is the application-facing socket for THIS connection. It is
// held only to recover the client's source port, which is the key
// CertForClient files the application's SNI under; the fn never reads
// or writes it (the relay owns it). Nil is safe.
//
// The conn pointer update (so the forwarders switch to the upgraded
// conn on subsequent iterations) is the relay's responsibility; this
// fn only performs the handshake and returns the new net.Conn —
// hence it does not need the caller's *net.Conn handles.
func newProxyTLSUpgradeFn(logger *zap.Logger, verify bool, rootCAs *x509.CertPool, srcConn net.Conn) relay.TLSUpgradeFn {
	return func(ctx context.Context, conn net.Conn, isClient bool, cfg *tls.Config) (net.Conn, error) {
		if cfg == nil {
			return conn, nil
		}
		if isClient {
			// By default, upstream identity verification is keploy's
			// responsibility to NOT do. Keploy is a transparent MITM
			// record/replay proxy: the real client (pgx, asyncpg, libpq,
			// JDBC, mongo driver, etc.) already made its trust decision
			// against keploy's minted cert when it dialed in, and the
			// upstream it points at in record mode is whatever the
			// application would have dialed itself — typically a
			// self-signed dev / CI / staging Postgres or Mongo, or a
			// Kubernetes service reachable only by ClusterIP. Either way
			// the upstream cert's SAN/CN often does not match the IP
			// literal keploy sees in `Destination Address` (e.g. cert
			// valid for 127.0.0.1, dial target 10.224.0.152), and Go's
			// default hostname/IP verification would surface that as
			// `dest TLS handshake failed: x509: certificate is valid
			// for X, not Y` and trip the parser supervisor's
			// passthrough fallback — silently dropping all recording
			// for that connection. A recording proxy must not be
			// stricter than the app it records.
			//
			// This used to be an UNCONDITIONAL override, justified by the
			// claim that parsers set InsecureSkipVerify on their own
			// DestTLSConfig anyway. That claim was false for the one OSS
			// parser it named: mysql/recorder.buildDestTLSConfigV2 sets no
			// such field, so the override was silently stripping a
			// decision a layer above had already made — and that parser's
			// docstring, which promised verification against the system
			// trust store, described behaviour that had not existed for as
			// long as this override had. Both are now driven by
			// record.upstreamTls.verify: off reproduces the old behaviour
			// exactly, on leaves the caller's cfg alone and lets Go verify.
			//
			// Either way the parser's cfg is CLONED, never mutated —
			// ServerName / NextProtos / ClientCert material on it is
			// preserved (shallow clone via tls.Config.Clone), so SNI for
			// vhosted PG-as-a-service providers (RDS, Cloud SQL, Neon)
			// and any upstream-mTLS material a parser might wire in
			// still reaches the wire. RootCAs is only overwritten when
			// the operator configured roots of their own; a parser that
			// pinned its own pool keeps it otherwise.
			dialCfg := cfg.Clone()
			dialCfg.InsecureSkipVerify = !verify //#nosec G402 -- MITM record-time proxy: upstream verification is opt-in via record.upstreamTls.verify. See the docstring above.
			if verify {
				if rootCAs != nil {
					dialCfg.RootCAs = rootCAs
				}
				// ServerName, in descending order of trustworthiness:
				//
				//  1. The SNI the APPLICATION sent. Parsers derive their
				//     ServerName from the destination ADDRESS — e.g.
				//     mysql/recorder.buildDestTLSConfigV2 takes the host of
				//     sess.Opts.DstCfg.Addr, which the proxy always sets to
				//     the `ip:port` eBPF reported, never the hostname the
				//     app dialled. Against a DNS-SAN-only upstream (a
				//     hostname DSN, the normal shape outside dev) that IP
				//     literal fails verification with `doesn't contain any
				//     IP SANs`, the supervisor falls through to raw
				//     passthrough, and the mock vanishes with a Debug log.
				//     A NON-EMPTY parser ServerName is therefore not enough
				//     to leave alone — it has to be overridden, not merely
				//     backfilled. The relay runs the client-side handshake
				//     first whenever verification is on (see
				//     relay.Config.ClientTLSFirst) precisely so this value
				//     exists by the time we get here.
				//  2. The parser's own ServerName, for parsers that pin a
				//     vhost name of their own (RDS/Cloud SQL/Neon proxies)
				//     and for apps that sent no SNI at all.
				//  3. The peer we are already connected to. An IP literal
				//     is correct here, Go matches it against the
				//     certificate's IP SANs; without it crypto/tls rejects
				//     an empty ServerName outright ("either ServerName or
				//     InsecureSkipVerify must be specified") before it
				//     looks at any certificate.
				//
				// The whole ladder is guarded by `verify` so the default
				// path never gains an SNI the application did not send.
				if sni := capturedSNIForSrc(srcConn); sni != "" {
					dialCfg.ServerName = sni
				}
				if dialCfg.ServerName == "" {
					dialCfg.ServerName = hostFromConn(conn)
				}
			}
			tlsConn := tls.Client(conn, dialCfg)
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

// capturedSNIForSrc returns the SNI the application sent on srcConn's
// connection, as filed by CertForClient under the client's source port
// (pTls.SrcPortToDstURL). "" when the app sent none — the normal case for an
// IP-literal destination, which RFC 6066 forbids in SNI — or when the value
// has not been captured yet.
//
// srcConn is only ever asked for its RemoteAddr; the relay owns the socket.
func capturedSNIForSrc(srcConn net.Conn) string {
	if srcConn == nil {
		return ""
	}
	tcpAddr, ok := srcConn.RemoteAddr().(*net.TCPAddr)
	if !ok || tcpAddr == nil {
		return ""
	}
	v, ok := pTls.SrcPortToDstURL.Load(tcpAddr.Port)
	if !ok {
		return ""
	}
	sni, _ := v.(string)
	return sni
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

// parserCanResyncAfterGap reports whether parser can re-anchor its framer
// after a HOLE in capture — bytes the proxy observed and forwarded but never
// delivered to the parser, because a tee dropped them.
//
// The relay uses the answer to decide whether to keep feeding a tee that has
// already desynced permanently. For a parser that cannot recover, feeding it
// is worse than useless: every subsequent length prefix is read out of the
// middle of a body, so the connection produces no further mocks, and a
// postgres framer that read four bytes of misread row data as a uint32 length
// once tried to allocate multiple gigabytes. For a parser that CAN recover it
// is mandatory — mongo/v2 finds the next message boundary by content-scanning
// the bytes that arrive after the hole, so cutting its feed would strand it
// desynced for the life of a pooled connection.
//
// Absent the capability the answer is FALSE. A parser that has never heard of
// this mechanism is precisely the one least likely to have a resync path, so
// omission must not be an opt-in.
//
// The parser is already chosen by the time recordViaSupervisor builds the
// relay, so the answer rides in through relay.Config exactly like
// WantsPreDispatchPause — no post-construction setter is needed, and none is
// offered: relay's tees read the flag from their forwarder goroutines without
// synchronisation precisely because it is fixed before Run starts them.
//
// Asserted against the named [integrations.GapResyncCapable] rather than an
// anonymous interface so the contract has one documented home; the assertion
// still keeps non-implementers compiling.
func parserCanResyncAfterGap(parser integrations.Integrations) bool {
	gr, ok := parser.(integrations.GapResyncCapable)
	return ok && gr.CanResyncAfterGap()
}
