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
// dropVoidsMock reports whether an OnMarkMockIncomplete callback of the given
// reason should void the in-flight mock. This is a DENY-list, not an allow-list:
// every reason voids the mock EXCEPT the two deliberate bulk-silence drops. The
// callback fires for far more than tee drops — write_error / short_write
// (relay.go), abort_mock (directive AbortMock), and pre_dispatch_drain_* all
// mean bytes never reached the peer and the mock must NOT be recorded as
// complete, for EVERY parser (the callback is wired before the recoverable
// opt-in check). Only these two are excluded:
//
//   - paused: PauseTees (abort — session dead) or KindPauseDir (mock already
//     finalized) — there is no live mock to void. Critically, during
//     abort-recovery the tees stay paused while the FRESH generation's session
//     is already published as curSess, so voiding here would silently drop the
//     recovered generation's first mock.
//   - memory_pressure: already covered by pressureRanges in record.go; voiding
//     here only mis-attributes the loss to a later, unrelated mock.
//
// (This is the OPPOSITE filter to the tee's onDropAt orphan-window allow-list,
// which restricts to channel_full/per_conn_cap to avoid flooding orphanRanges —
// the two policies are not interchangeable.)
func dropVoidsMock(reason string) bool {
	return reason != relay.DropPaused && reason != relay.DropMemoryPressure
}

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
	// The relay is created ONCE per connection, but on the abort-recovery
	// path several parser generations run over its lifetime (see the
	// generation loop below). Its tee callbacks are wired at relay.New()
	// time, so they must route to the CURRENT generation's Supervisor and
	// Session rather than capture a stale gen-0 one. curSv/curSess hold the
	// live generation; the relay callbacks load them per-call. For a parser
	// that does not opt into abort recovery there is exactly one generation,
	// so this indirection is behaviourally identical to a direct wire.
	var curSv atomic.Pointer[supervisor.Supervisor]
	var curSess atomic.Pointer[supervisor.Session]
	// genSawClientChunk records whether the CURRENT generation ever had a
	// client chunk teed to it. Reset per generation and used as the progress
	// guard that stops an abort-recovery respawn from spinning on a parser
	// that dies before reading anything.
	var genSawClientChunk atomic.Bool

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

	r := relay.New(relay.Config{
		Logger:       logger,
		TLSUpgradeFn: newProxyTLSUpgradeFn(logger),
		BumpActivity: func() {
			if s := curSv.Load(); s != nil {
				s.BumpActivity()
			}
		},
		OnMarkMockIncomplete: func(reason string) {
			if !dropVoidsMock(reason) {
				return
			}
			if s := curSess.Load(); s != nil {
				s.MarkMockIncomplete(reason)
			}
		},
		// Time-attributed complement to OnMarkMockIncomplete: for a genuine
		// per-op byte-loss drop (channel_full/per_conn_cap only — the tee
		// excludes memory_pressure/paused, which would flood the ring and
		// mass-suppress healthy TCs), the dropped chunk's own wire instant
		// is recorded as a zero-width orphan window so record.go suppresses
		// TCs whose HTTP window contains it. Unlike the session flag (which
		// voids a later, unrelated mock), this lands inside the dropped op's
		// OWN TC window. Attribution is by time only — a concurrent healthy
		// TC straddling this instant may also be suppressed; that bounded
		// over-suppression is the accepted "drop the TC rather than orphan
		// it" tradeoff. Fast-follow: thread per-op identity for exact-TC.
		OnTeeDropWindow: func(_ string, ts time.Time) {
			if s := curSess.Load(); s != nil {
				s.RecordOrphanWindow(ts, ts)
			}
		},
		OnClientChunkTeed: func() {
			genSawClientChunk.Store(true)
			if s := curSv.Load(); s != nil {
				s.MarkPendingWork()
			}
		},
		RealCertHook: realCertHook,
		// User-tunable record-buffer caps. Snapshotted onto the Proxy
		// at startup from config.Record.RecordBuffer (yaml/flag/env).
		// Zero values fall through to relay package defaults via
		// withDefaults() — preserving the zero-config path.
		PerConnCap:       p.recordBufferCap,
		TeeChanBuf:       p.recordBufferQueueSize,
		PreDispatchPause: preDispatchPause,
	}, srcConn, dstConn)

	// Abort-recovery opt-in (duck-typed like WantsPreDispatchPause): only
	// parsers that declare SupportsAbortRecovery participate in the
	// generation loop below; all others keep the historical behaviour — one
	// generation, then permanent raw passthrough after an abort. Safe only
	// for client-initiates-on-reuse protocols (http, mongo) where the next
	// client byte on a pooled connection is always a fresh request boundary;
	// a server-first-on-reuse protocol must stay opted out.
	recoverable := false
	if rp, ok := parser.(interface{ SupportsAbortRecovery() bool }); ok {
		recoverable = rp.SupportsAbortRecovery()
	}

	// Memory-pressure self-management opt-in (duck-typed like the others). A
	// parser that stays byte-synced across a memoryguard pause and sheds
	// memory itself (mongo/v2 skip-discards whole messages under pressure)
	// tells the relay to STOP dropping its chunks under memory pressure. A
	// mid-message pressure drop desyncs the length-prefix reassembler, which
	// then cannot re-anchor on the app's large multi-chunk messages under the
	// pressure sawtooth and silently stops recording the pooled connection for
	// the rest of the run — the go-memory-load-mongo tail dead zone. Only the
	// memory_pressure tee-drop is suppressed; capacity drops and the
	// abort/finalize pause are unchanged, so genuine mid-message byte loss
	// still triggers the parser's resync.
	if pm, ok := parser.(interface{ SelfManagesMemoryPressure() bool }); ok && pm.SelfManagesMemoryPressure() {
		r.SetPressureSelfManaged(true)
	}

	// Run the relay in its own goroutine under the connection's lifetime.
	// The relay is created ONCE and outlives every parser generation; each
	// generation's SessionOnAbort closes the current FakeConns so the parser's
	// reads unblock on abort.
	relayDone := make(chan error, 1)
	relayCtx, relayCancel := context.WithCancel(ctx)
	defer relayCancel()
	// The relay goroutine is launched inside the loop, once generation 0 is
	// fully wired (see below) — starting it earlier leaves a startup window
	// where the first teed chunk's callbacks find curSv/curSess still nil.

	// maxAbortRecoveryGenerations bounds respawns so a genuinely broken
	// parser (one that aborts on every request) cannot spin forever; after
	// the ceiling the connection falls back to permanent raw passthrough.
	const maxAbortRecoveryGenerations = 8

	// parserExitGrace bounds how long recovery waits for the aborted
	// generation's parser goroutine to retire before reattaching its streams.
	// After SessionOnAbort closes the FakeConns an I/O-bound parser unblocks in
	// microseconds; the generous ceiling only trips for a parser genuinely
	// wedged in a CPU loop that ignores ctx and Closed reads, in which case we
	// abandon recovery rather than race the still-live goroutine on the shared
	// tee channel.
	const parserExitGrace = 2 * time.Second

	for gen := 0; ; gen++ {
		if gen > 0 {
			// MEAS(abort-recovery): TEMP INFO — remove before merge.
			logger.Info("MEAS abort-recovery respawn",
				zap.String("parser", string(parserType)),
				zap.Int64("clientConnID", clientConnID),
				zap.Int("gen", gen))
			// Respawn: the previous generation aborted (tees paused, its
			// FakeConns Closed by SessionOnAbort). Swap in fresh FakeConns over
			// the same still-open tee channels so the new Session wires them.
			// The tees stay paused until ResumeTees below — after everything is
			// wired — so no chunk is delivered to a half-built generation.
			r.ReattachStreams()
		}

		sv := supervisor.New(supervisor.Config{
			Logger: logger,
			// Leave HangBudget / MemCap / PanicReporter defaulted by the
			// supervisor package. Tune via config once we have production
			// telemetry on the fallback rate.
		})

		// Fresh Session per generation. A reused Session would carry a stuck
		// MarkMockIncomplete flag: during the paused window between abort and
		// respawn every dropped chunk (DropPaused) still fires onDrop →
		// MarkMockIncomplete, so a reused Session's next EmitMock would
		// silently drop the new generation's mocks. The app-scoped
		// SyncMockManager (Mgr) IS preserved across generations, so
		// record-session-window continuity (firstReqSeen, lifetime derivation)
		// is unaffected. RouteMocksViaSyncMock keeps V2 mocks on the same
		// session-window path as legacy parsers (integrations#133).
		svSess := &supervisor.Session{
			Mocks:                 mocks,
			Logger:                logger,
			ClientConnID:          fmt.Sprint(clientConnID),
			DestConnID:            fmt.Sprint(destConnID),
			Opts:                  opts,
			OnPendingCleared:      sv.ClearPendingWork,
			RouteMocksViaSyncMock: true,
			Mgr:                   syncMock.FromContext(ctx),
			TLSUpgrader:           nil,
			ErrGroup:              errGrp,
		}
		svSess.ClientStream = r.ClientStream()
		svSess.DestStream = r.DestStream()
		svSess.Directives = r.Directives()
		svSess.Acks = r.Acks()

		// Publish this generation to the relay's indirected tee callbacks
		// before any chunk is delivered, and reset the per-generation progress
		// guard.
		curSv.Store(sv)
		curSess.Store(svSess)
		genSawClientChunk.Store(false)

		// Teach the hang watchdog to tell an idle parser from a stuck one:
		// only declare a hang when the relay still holds unconsumed input.
		sv.SetActivityProbe(r.HasBufferedInput)

		sv.SessionOnAbort = func() {
			// Pause the tees FIRST so subsequent chunks drop cheaply via the
			// pause fast-path instead of the channel-full branch (which logs at
			// Debug per chunk). Pausing does NOT stop the real-socket
			// forwarders — every byte still reaches its peer.
			r.PauseTees()
			// Then unblock the parser's reads so the supervisor's
			// cancel-select observes the parser goroutine exiting promptly.
			_ = r.ClientStream().Close()
			_ = r.DestStream().Close()
		}

		if gen > 0 {
			// This generation is fully wired (streams, curSv/curSess, probe,
			// abort hook); admit chunks again.
			r.ResumeTees()
		} else {
			// Generation 0 is now fully wired. Launch the relay so the first
			// teed chunk's callbacks (BumpActivity / MarkPendingWork /
			// MarkMockIncomplete) route to this live supervisor/session instead
			// of no-oping on the nil guard. The relay is created ONCE and
			// outlives every generation.
			go func() { relayDone <- r.Run(relayCtx) }()
		}

		// Adapter: the parser's RecordOutgoing takes *integrations.RecordSession
		// but the supervisor's ParserFunc takes *supervisor.Session. On the V2
		// path Ingress/Egress/TLSUpgrader are nil so a parser bug that reaches
		// for the legacy fields surfaces as an obvious nil panic (which the
		// supervisor catches) rather than a silent misuse of sockets the relay
		// owns.
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
		// sv.Run already ran `defer s.Close()`, so the supervisor (and its
		// watchdog) is stopped by the time Run returns — no explicit Close here.

		if !result.FallthroughToPassthrough {
			// MEAS(abort-recovery): TEMP INFO — remove before merge.
			logger.Info("MEAS recordViaSupervisor clean-return",
				zap.String("parser", string(parserType)),
				zap.Int64("clientConnID", clientConnID),
				zap.Int("gen", gen),
				zap.String("status", result.Status.String()),
				zap.Error(result.Err))
			// Parser returned normally or with an error. Cancel the relay and
			// drain, then report the outcome.
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

		// ABORT (fallthrough). The relay is still alive and forwarding raw; the
		// tees are paused and this generation's FakeConns were Closed by
		// SessionOnAbort.
		logger.Debug("parser supervisor triggered passthrough fallback; relay continues raw forwarding until peer close",
			zap.String("parser", string(parserType)),
			zap.String("status", result.Status.String()),
			zap.Error(result.Err),
			zap.Int("generation", gen),
			zap.Bool("recoverable", recoverable),
			zap.String("next_step", "set KEPLOY_NEW_RELAY=off to force legacy path for this parser, or KEPLOY_DISABLE_PARSING=1 to disable record parsing entirely"),
		)
		// MEAS(abort-recovery): TEMP INFO — remove before merge.
		logger.Info("MEAS abort detected (fallthrough)",
			zap.String("parser", string(parserType)),
			zap.Int64("clientConnID", clientConnID),
			zap.Int("gen", gen),
			zap.String("status", result.Status.String()),
			zap.Bool("recoverable", recoverable),
			zap.Bool("sawClientChunk", genSawClientChunk.Load()),
			zap.Error(result.Err))

		// Abort-recovery: for an opted-in parser on a still-alive pooled
		// connection, respawn a fresh parser generation so recording resumes
		// on the next request instead of the connection being permanently
		// silenced (the go-memory-load-mongo recording dead zone). Give up —
		// keeping the historical I1 raw passthrough — when the parser is not
		// opted in, the generation budget is exhausted, or this generation
		// never saw a client chunk (a parser that dies before reading anything
		// would otherwise respawn-spin). In every give-up case we do NOT cancel
		// the relay: it keeps forwarding client↔dest bytes raw until a peer
		// close triggers a normal Run exit — no stall gap (invariant I1).
		//
		// Scope note: this cleanly recovers the target dead-zone (an abort
		// between requests — the previous response was dropped at the tee under
		// memory pressure (armed, not yet paused; the pause comes later with the
		// abort), never staged, so the reattached generation reads a clean
		// request boundary). If instead the abort fires MID-request, partial
		// bytes of the aborted op may remain staged and the reattached
		// generation reads them mid-frame; a framed protocol desyncs and
		// re-aborts (bounded by the generation ceiling, then passthrough) rather
		// than reliably recording a corrupt mock — graceful degradation, not
		// silent corruption.
		if !recoverable || gen+1 >= maxAbortRecoveryGenerations || !genSawClientChunk.Load() {
			// MEAS(abort-recovery): TEMP INFO — remove before merge.
			reason := "budget"
			if !recoverable {
				reason = "not-recoverable"
			} else if !genSawClientChunk.Load() {
				reason = "no-client-chunk"
			}
			logger.Info("MEAS abort-recovery give-up",
				zap.String("parser", string(parserType)),
				zap.Int64("clientConnID", clientConnID),
				zap.Int("gen", gen),
				zap.String("reason", reason))
			<-relayDone
			return nil
		}

		// Wait for THIS generation's parser goroutine to fully retire before the
		// next iteration reattaches its streams. On the hang and outer-cancel
		// grace abort paths sv.Run returns while the parser goroutine is still
		// alive — it unblocks asynchronously once SessionOnAbort Closed its
		// FakeConns. The old (now-Closed) FakeConn and the reattached one drain
		// the SAME tee out-channel, so respawning while the old goroutine can
		// still read would let it steal a resumed chunk from the new generation
		// (silent mock loss). If it does not exit within the grace it is
		// genuinely wedged; give up recovery rather than race it.
		select {
		case <-sv.ParserDone():
		case <-time.After(parserExitGrace):
			logger.Debug("previous parser generation did not exit within recovery grace; abandoning abort recovery",
				zap.String("parser", string(parserType)),
				zap.Int("generation", gen),
			)
			// MEAS(abort-recovery): TEMP INFO — remove before merge.
			logger.Info("MEAS abort-recovery give-up",
				zap.String("parser", string(parserType)),
				zap.Int64("clientConnID", clientConnID),
				zap.Int("gen", gen),
				zap.String("reason", "parser-exit-grace"))
			<-relayDone
			return nil
		}

		// The connection may already be over (peer closed). If the relay has
		// exited there is nothing left to reattach to next iteration.
		select {
		case <-relayDone:
			// MEAS(abort-recovery): TEMP INFO — remove before merge.
			logger.Info("MEAS abort-recovery relay-exited-before-respawn",
				zap.String("parser", string(parserType)),
				zap.Int64("clientConnID", clientConnID),
				zap.Int("gen", gen))
			return nil
		default:
		}
	}
}

// newProxyTLSUpgradeFn adapts keploy's existing TLS helpers into the
// relay.TLSUpgradeFn shape. The returned function:
//
//   - For isClient=true (upgrading the destination side — keploy
//     acts as TLS client to the real server), dials TLS over the
//     existing conn using tls.Client and performs the handshake.
//     Upstream cert verification is ALWAYS skipped here — see the
//     dest-side InsecureSkipVerify rationale on the cfg clone below.
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
			// Upstream identity verification is keploy's responsibility
			// to NOT do. Keploy is a transparent MITM record/replay
			// proxy: the real client (pgx, asyncpg, libpq, JDBC, mongo
			// driver, etc.) already made its trust decision against
			// keploy's minted cert when it dialed in, and the upstream
			// it points at in record mode is whatever the application
			// would have dialed itself — typically a self-signed dev /
			// CI / staging Postgres or Mongo, or a Kubernetes service
			// reachable only by ClusterIP. Either way the upstream
			// cert's SAN/CN often does not match the IP literal keploy
			// sees in `Destination Address` (e.g. cert valid for
			// 127.0.0.1, dial target 10.224.0.152), and Go's default
			// hostname/IP verification would surface that as
			// `dest TLS handshake failed: x509: certificate is valid
			// for X, not Y` and trip the parser supervisor's
			// passthrough fallback — silently dropping all recording
			// for that connection.
			//
			// Parsers that build their DestTLSConfig already set
			// InsecureSkipVerify on it (see e.g.
			// integrations/pkg/postgres/v3/recorder.buildDestTLSConfigV2,
			// keploy/pkg/agent/proxy/integrations/mysql/recorder.buildDestTLSConfigV2)
			// — but if a parser ever forgets, or if some future call
			// site lands here with a strict config, we still need the
			// MITM-correct posture. So clone the parser's cfg, force
			// InsecureSkipVerify=true on the clone, and dial against
			// that. ServerName / RootCAs / NextProtos / ClientCert
			// material on the parser-supplied cfg is preserved
			// (shallow clone via tls.Config.Clone), so SNI for
			// vhosted PG-as-a-service providers (RDS, Cloud SQL, Neon)
			// and any upstream-mTLS material a parser might wire in
			// still reaches the wire.
			dialCfg := cfg.Clone()
			dialCfg.InsecureSkipVerify = true //#nosec G402 -- MITM record-time proxy: keploy intentionally never validates the upstream cert. See the docstring above.
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
