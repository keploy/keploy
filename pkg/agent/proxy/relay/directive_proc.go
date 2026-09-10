package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.uber.org/zap"
)

// processDirectives is the directive processor goroutine. It reads
// from r.directives until either the channel is closed, stopping is
// closed, or ctx is cancelled. Each directive is dispatched to the
// appropriate handler, which returns an [directive.Ack] that is
// sent (non-blocking) on r.acks.
//
// If r.acks is full (parser not draining) the ack is dropped with a
// debug log: the parser contract is to drain acks before sending
// more directives, and the relay is not going to stall traffic over
// a missing ack.
func (r *Relay) processDirectives(ctx context.Context, stopping <-chan struct{}) {
	log := r.cfg.Logger
	for {
		select {
		case <-ctx.Done():
			return
		case <-stopping:
			return
		case d, ok := <-r.directives:
			if !ok {
				return
			}
			ack := r.handleDirective(ctx, stopping, d)
			select {
			case r.acks <- ack:
			default:
				if log != nil {
					log.Debug("relay: ack dropped (ack channel full)",
						zap.String("kind", d.Kind.String()),
						zap.Bool("ok", ack.OK),
					)
				}
			}
		}
	}
}

// handleDirective dispatches on Kind. Returns the Ack to emit.
// stopping is the relay-wide teardown signal; handlers that need to
// wait for forwarder coordination (currently only KindUpgradeTLS) plumb
// it through to avoid an indefinite block during shutdown.
func (r *Relay) handleDirective(ctx context.Context, stopping <-chan struct{}, d directive.Directive) directive.Ack {
	switch d.Kind {
	case directive.KindUpgradeTLS:
		return r.handleUpgradeTLS(ctx, stopping, d)
	case directive.KindPauseDir:
		return r.handlePauseDir(d)
	case directive.KindResumeDir:
		return r.handleResumeDir(d)
	case directive.KindAbortMock:
		return r.handleAbortMock(d)
	case directive.KindFinalizeMock:
		// The relay is not the mock committer — that is the
		// supervisor's job. Ack and move on.
		return directive.Ack{Kind: d.Kind, OK: true}
	case directive.KindResumePreDispatch:
		return r.handleResumePreDispatch(ctx, stopping, d)
	case directive.KindReleaseClient:
		return r.handleReleaseClient(ctx, stopping, d)
	default:
		return directive.Ack{
			Kind: d.Kind,
			OK:   false,
			Err:  fmt.Errorf("relay: unknown directive kind %d", d.Kind),
		}
	}
}

// handleUpgradeTLS runs the TLS upgrade choreography:
//  1. Install the pause barrier. Forwarders park on their next loop
//     iteration — i.e. after finishing any Read already in flight.
//  2. (Optional) Read [UpgradeTLSParams.PreambleReadFromDest] bytes
//     from the real destination socket directly, bypassing the
//     forwarders' tee, so a synchronous protocol-preamble exchange
//     (e.g. Postgres SSLResponse byte) is observed before the
//     forwarders reawaken. If [UpgradeTLSParams.PreambleForwardToSrc]
//     is true, write those bytes to the real source socket before
//     touching TLS — closing the race where the C2D forwarder would
//     otherwise pick up the client's TLS ClientHello bytes (sent in
//     reaction to the preamble) and deliver them upstream as
//     cleartext, corrupting the upstream wire before the handshake
//     even starts.
//  3. (Optional) Gate handshakes on
//     [UpgradeTLSParams.ProceedOnPreamble] matching the read preamble.
//     A mismatch is OK=true with TLSUpgraded=false: it lets a
//     protocol that allows the server to decline TLS at the preamble
//     stage (Postgres 'N') return without forcing the whole mock
//     incomplete.
//  4. Handshake dest first (keploy = TLS client to real server),
//     then client (keploy = TLS server, presenting MITM cert).
//  5. On either failure, release the pause and return OK=false; the
//     relay stays on the original (cleartext) conns. The supervisor
//     is expected to fall through to raw passthrough.
//  6. On success, replace the atomic conn pointers with the upgraded
//     versions, release the pause, and return OK=true with boundary
//     timestamps.
//
// Correctness precondition: the parser must have drained its FakeConn
// to a known protocol boundary before sending KindUpgradeTLS (this is
// the BarrierBeforeDirective contract from PLAN.md §3.5). Forwarders
// finish any in-flight Read and forward it on cleartext before
// parking; the TLS handshake starts from whatever the real peer
// sends next. If the parser sends the directive while a real Read
// was about to return TLS-handshake bytes, those bytes are forwarded
// as-is, which is wrong — but the contract puts the responsibility
// for boundary detection on the parser. The PreambleReadFromDest /
// PreambleForwardToSrc fields exist precisely to give the parser a
// way to satisfy that precondition for protocols (Postgres SSL) where
// the boundary is "the byte the server is about to send AND the
// reaction the client is about to write" rather than something the
// parser can detect from already-forwarded bytes alone.
func (r *Relay) handleUpgradeTLS(ctx context.Context, stopping <-chan struct{}, d directive.Directive) directive.Ack {
	log := r.cfg.Logger

	// Whether a client write hold was in force when this directive
	// arrived. Every exit below routes through endUpgradePause, which
	// uses this to decide what happens to bytes still held: if the
	// client side did not end up behind TLS, they are ordinary
	// cleartext that the server is still waiting for, and dropping them
	// would truncate the connection mid-message.
	//
	// Computed FIRST, above every early return. An exit that skips it
	// leaves the hold armed with no one left to release it, and the
	// client direction is blackholed for the rest of the connection —
	// the no-upgrader return below was exactly that bug.
	heldAtEntry := r.holdClient.Load()

	if r.cfg.TLSUpgradeFn == nil {
		// No pause was installed, so endUpgradePause only does the
		// flush half here — which is the half that matters: the parser
		// asked for an upgrade this relay cannot perform, and the bytes
		// it held for that upgrade still belong to the server.
		r.endUpgradePause(heldAtEntry, false, 0)
		return directive.Ack{Kind: d.Kind, OK: false, Err: ErrNoTLSUpgrader}
	}
	params := d.TLS
	if params == nil {
		params = &directive.UpgradeTLSParams{}
	}

	// A flush count is only meaningful under a hold. Asking for one
	// without a hold means the parser believes bytes are being held
	// back that were in fact forwarded in real time — exactly the leak
	// the hold exists to prevent, and acking OK would tell the parser
	// its byte-exact split had been honoured when nothing was split at
	// all. Refuse before the barrier goes up.
	if params.ClientFlushBytes > 0 && !heldAtEntry {
		return directive.Ack{
			Kind: d.Kind,
			OK:   false,
			Err: fmt.Errorf("TLS upgrade: ClientFlushBytes=%d but no client write hold is active",
				params.ClientFlushBytes),
		}
	}

	// Barrier up. Forwarders will park on their next loop iteration.
	// beginPause also nudges SetReadDeadline on both live sockets so
	// any forwarder blocked in Read wakes up promptly.
	r.beginPause()

	// Wait for both forwarders to actually be parked on the pause
	// channel before proceeding. This is the synchronisation point
	// that lets takeStashed below observe any bytes a forwarder Read
	// returned in flight (Postgres SSLResponse 'S' is the canonical
	// case: D2C's blocked Read wakes from the deadline kick with the
	// 'S' byte, the post-Read pause check stashes it onto the relay,
	// then the forwarder calls markForwarderParked). Without this
	// wait the directive handler races the forwarder, sees an empty
	// stash, falls through to readFullPreamble on the live socket,
	// and deadlocks because the byte the parser is asking us to read
	// has already been consumed by the forwarder Read that just woke
	// up.
	r.waitForwardersParked(ctx, stopping)

	// The parser's client stream is now exactly this long, and it
	// cannot grow while the forwarders are parked. Under a client
	// write hold this number is the boundary between "bytes the parser
	// was meant to read" and "bytes keploy's own client-side handshake
	// is about to consume": the parser has read the SSLRequest it based
	// this directive on, and everything the tee accepted after that is
	// the ClientHello behind it. Captured here rather than derived
	// later so it names a position the forwarders could not have moved.
	// See the use in endUpgradePause.
	//
	// Both ways of being wrong here are safe in the same direction.
	// waitForwardersParked gives up if the relay is stopping, so C2D can
	// in principle still be live and tee past this point — those bytes
	// are ABOVE the offset and are simply left alone, never swallowed.
	// And a parser that had over-read past its own flush prefix has
	// already seen bytes we cannot take back, but the offset still
	// bounds the discard to what was teed before the barrier, so live
	// post-upgrade traffic is untouchable either way.
	barrierTeeOffset := r.teeC2D.acceptedBytes()

	boundaryReadAt := time.Now()

	// Step 1 — synchronous preamble exchange. The preamble (e.g.
	// Postgres SSLResponse byte) may already have been read by the
	// D2C forwarder before the pause barrier was raised; in that
	// case the forwarder stashed it via stashInflightFromPause
	// rather than writing it to the live src socket, and we claim it
	// here. If the stash is empty we read directly from the live
	// dest socket — the byte is still in flight from the server and
	// no forwarder consumed it.
	//
	// Either path completes synchronously under the pause, so the
	// directive handler is the sole owner of the protocol state at
	// this boundary. Without this two-source design, the obvious
	// "always read from real_dst" approach would race with D2C: in
	// the case where D2C already consumed 'S' from the server, the
	// next byte on real_dst is whatever the server sends after 'S'
	// (the start of TLS ServerHello, if the C2D forwarder also
	// already forwarded the client's TLS ClientHello to the server).
	// We saw 0x16 ('handshake' TLS record type) instead of 'S' /
	// 0x53 from postgres in exactly that case before this fix.
	// Clear the past-time deadline beginPause installed on the live
	// sockets so the synchronous TLS handshakes (and the
	// preamble-from-real-dst Read on the no-stash branch) aren't
	// instantly aborted by the same kick. A blocking forwarder Read
	// has already woken up by now and the post-Read recheck has
	// already parked it on the pause channel; clearing the deadline
	// here is safe because the forwarder won't issue another Read
	// until the pause channel closes (which happens in endPause,
	// after this function returns). endPause clears the deadline
	// again — that's a no-op here but keeps the invariant tidy.
	clearDeadline(r.dst.Load())
	clearDeadline(r.src.Load())

	// Pre-dispatch C2D flush. Under Config.PreDispatchPause, the C2D
	// forwarder stashes the client's first message (e.g. Postgres
	// SSLRequest) without forwarding it, so the parser can inspect
	// the chunk via its FakeConn tee and decide whether to issue
	// UpgradeTLS or ResumePreDispatch. If we entered handleUpgradeTLS
	// from that path, the SSLRequest is still sitting in r.stashedC2D
	// and the real Postgres server has not seen it yet — so the
	// preamble Read from the live dst socket below will block until
	// the deadline kicks (i/o timeout), the parser returns Err, the
	// supervisor falls through to passthrough, and the live app sees
	// the connection EOF before any byte reaches Postgres.
	//
	// Flush the C2D stash to dst here so the upstream protocol
	// exchange that produces the preamble byte can actually happen.
	// Gated on preDispatchActive so the legacy (post-#4196-pause-only)
	// path — where the forwarder forwarded the SSLRequest in real time
	// and only the post-pause-boundary 'S' byte ended up stashed on
	// the D2C side — keeps the existing semantics.
	//
	// A client write hold (Config.HoldClientWrites) needs the same
	// flush for the same reason, but a byte-exact one. Under
	// pre-dispatch the stash is the client's first message and all of
	// it belongs upstream. Under a hold the stash can hold the
	// SSLRequest AND the ClientHello behind it — often from a single
	// read — and only the first belongs upstream; the remainder is
	// claimed further down as the prepend for the client-side
	// handshake. So the hold branch flushes exactly
	// params.ClientFlushBytes and leaves the rest in place.
	if r.holdClient.Load() {
		// Clear the hold before the flush, not after. The forwarders
		// are parked behind the pause barrier and cannot observe the
		// flag until endPause below, but leaving it set through an
		// early return (a short stash, a failed Write) would strand
		// the connection in a hold that nothing will ever release.
		r.holdClient.Store(false)
		r.heldClientBytes.Store(0)

		if params.ClientFlushBytes > 0 {
			// Check the length BEFORE claiming anything.
			// takeStashedPrefix mutates the stash, so taking first and
			// validating second would consume the prefix and then
			// abandon it on the error return — the server would be left
			// waiting for a message whose head we had silently eaten.
			if avail := r.stashedLen(fakeconn.FromClient); avail < params.ClientFlushBytes {
				// The parser asked us to forward more than it ever saw.
				// Fail the directive; endUpgradePause below still
				// delivers everything held, so the connection stays
				// consistent even though the split did not happen.
				if log != nil {
					log.Debug("relay: TLS upgrade client-hold flush short",
						zap.Int("requested", params.ClientFlushBytes),
						zap.Int("available", avail),
						zap.String("directive_reason", d.Reason),
					)
				}
				r.endUpgradePause(heldAtEntry, false, barrierTeeOffset)
				return directive.Ack{
					Kind: d.Kind,
					OK:   false,
					Err: fmt.Errorf("TLS upgrade client-hold flush: asked for %d bytes, stash held %d",
						params.ClientFlushBytes, avail),
				}
			}
			c2dForward := r.takeStashedPrefix(fakeconn.FromClient, params.ClientFlushBytes)
			dstPtr := r.dst.Load()
			if dstPtr == nil || *dstPtr == nil {
				r.endUpgradePause(heldAtEntry, false, barrierTeeOffset)
				return directive.Ack{
					Kind: d.Kind,
					OK:   false,
					Err:  errors.New("TLS upgrade client-hold C2D flush: no destination conn"),
				}
			}
			wn, werr := (*dstPtr).Write(c2dForward.bytes)
			if werr == nil && wn != c2dForward.len() {
				werr = fmt.Errorf("short write %d of %d bytes", wn, c2dForward.len())
			}
			if werr != nil {
				if log != nil {
					log.Debug("relay: TLS upgrade client-hold C2D flush failed",
						zap.Error(werr),
						zap.Int("bytes", c2dForward.len()),
						zap.String("directive_reason", d.Reason),
					)
				}
				// The server did not get the client's message, so
				// whatever this connection records next is missing its
				// head. Say so rather than scoring the mock clean.
				if r.cfg.OnMarkMockIncomplete != nil {
					r.cfg.OnMarkMockIncomplete("write_error")
				}
				r.endUpgradePause(heldAtEntry, false, barrierTeeOffset)
				return directive.Ack{
					Kind: d.Kind,
					OK:   false,
					Err:  fmt.Errorf("TLS upgrade client-hold C2D flush: %w", werr),
				}
			}
		}
	} else if r.preDispatchActive.Load() {
		c2dForward := r.takeStashed(fakeconn.FromClient)
		if c2dForward.len() > 0 {
			dst := *r.dst.Load()
			if _, werr := dst.Write(c2dForward.bytes); werr != nil {
				if log != nil {
					log.Debug("relay: TLS upgrade pre-dispatch C2D flush failed",
						zap.Error(werr),
						zap.Int("bytes", c2dForward.len()),
						zap.String("directive_reason", d.Reason),
					)
				}
				r.endUpgradePause(heldAtEntry, false, barrierTeeOffset)
				return directive.Ack{
					Kind: d.Kind,
					OK:   false,
					Err:  fmt.Errorf("TLS upgrade pre-dispatch C2D flush: %w", werr),
				}
			}
		}
	}

	var preamblePayload []byte
	if params.PreambleReadFromDest > 0 {
		// 1a. Try the D2C stash first.
		if stashed := r.takeStashedPrefix(fakeconn.FromDest, params.PreambleReadFromDest); stashed.len() > 0 {
			if stashed.len() >= params.PreambleReadFromDest {
				preamblePayload = stashed.bytes[:params.PreambleReadFromDest]
			} else {
				// Stash fell short of what the parser asked for;
				// top up by reading the remainder directly from
				// the live socket. This branch is rare in
				// practice — the Postgres SSL preamble is a
				// single byte — but keeps the contract strict for
				// future protocols.
				preamblePayload = make([]byte, params.PreambleReadFromDest)
				copy(preamblePayload, stashed.bytes)
				clearDeadline(r.dst.Load())
				dst := *r.dst.Load()
				_, err := readFullPreamble(dst, preamblePayload[stashed.len():])
				if err != nil {
					if log != nil {
						log.Debug("relay: TLS upgrade preamble read (post-stash) failed",
							zap.Error(err),
							zap.Int("stashed", stashed.len()),
							zap.Int("requested", params.PreambleReadFromDest),
							zap.String("directive_reason", d.Reason),
							zap.String("next_step", "the upstream closed mid-preamble; verify the destination is the protocol the parser was matched to and consider KEPLOY_DISABLE_PARSING=1 to bypass parsing"),
						)
					}
					r.endUpgradePause(heldAtEntry, false, barrierTeeOffset)
					return directive.Ack{
						Kind:            d.Kind,
						OK:              false,
						Err:             fmt.Errorf("TLS upgrade preamble read: %w", err),
						PreamblePayload: preamblePayload[:stashed.len()],
					}
				}
			}
		} else {
			// 1b. No stash; read straight from the live dest
			// socket. This path runs when the parser sent the
			// directive before the D2C forwarder's Read returned
			// — i.e. before the server replied with the preamble
			// byte at all. The Read here blocks until the
			// preamble arrives; ctx-cancel propagates via the
			// underlying conn's deadline plumbing.
			//
			// beginPause set a past-time SetReadDeadline on dst
			// to wake any blocked forwarder Read; we now need a
			// clean deadline so this synchronous Read isn't
			// instantly aborted by the same kick. clearDeadline
			// drops the deadline; endPause will reapply it later
			// (no-op since it sets the zero deadline anyway).
			preamblePayload = make([]byte, params.PreambleReadFromDest)
			clearDeadline(r.dst.Load())
			dst := *r.dst.Load()
			n, err := readFullPreamble(dst, preamblePayload)
			if err != nil {
				if log != nil {
					log.Debug("relay: TLS upgrade preamble read failed",
						zap.Error(err),
						zap.Int("requested", params.PreambleReadFromDest),
						zap.Int("read", n),
						zap.String("directive_reason", d.Reason),
						zap.String("next_step", "the upstream closed the connection or returned fewer bytes than the parser expected for its preamble; verify the destination is the protocol the parser was matched to (Postgres on a non-Postgres port, etc.) and consider KEPLOY_DISABLE_PARSING=1 to bypass parsing"),
					)
				}
				r.endUpgradePause(heldAtEntry, false, barrierTeeOffset)
				return directive.Ack{
					Kind:            d.Kind,
					OK:              false,
					Err:             fmt.Errorf("TLS upgrade preamble read: %w", err),
					PreamblePayload: preamblePayload[:n],
				}
			}
		}

		if params.PreambleForwardToSrc {
			// Clear any past-time deadline on src as well; though
			// SetReadDeadline does not affect Write blocking on
			// most net.Conn implementations, some wrappers
			// propagate deadlines to both directions, so the
			// belt-and-braces clear keeps the Write below clean.
			clearDeadline(r.src.Load())
			src := *r.src.Load()
			if _, werr := src.Write(preamblePayload); werr != nil {
				if log != nil {
					log.Debug("relay: TLS upgrade preamble forward failed",
						zap.Error(werr),
						zap.String("directive_reason", d.Reason),
					)
				}
				r.endUpgradePause(heldAtEntry, false, barrierTeeOffset)
				return directive.Ack{
					Kind:            d.Kind,
					OK:              false,
					Err:             fmt.Errorf("TLS upgrade preamble forward: %w", werr),
					PreamblePayload: preamblePayload,
				}
			}
		}
		// Optional gate: if the parser said "only proceed when the
		// preamble matches X", short-circuit on mismatch. This is
		// OK=true (the directive carried out its protocol-aware job)
		// with TLSUpgraded=false (no actual handshake happened) so
		// the parser can record the alternate-path mock without
		// marking it incomplete.
		if len(params.ProceedOnPreamble) > 0 && !bytesEqual(params.ProceedOnPreamble, preamblePayload) {
			boundaryWrittenAt := time.Now()
			r.endUpgradePause(heldAtEntry, false, barrierTeeOffset)
			return directive.Ack{
				Kind:              d.Kind,
				OK:                true,
				PreamblePayload:   preamblePayload,
				TLSUpgraded:       false,
				BoundaryReadAt:    boundaryReadAt,
				BoundaryWrittenAt: boundaryWrittenAt,
			}
		}
	}

	// Step 2 — TLS handshakes. Atomic two-sided upgrade: run both
	// handshakes FIRST (keeping the new *tls.Conn values in local
	// vars), only publish the upgraded conn pointers via
	// r.{dst,src}.Store AFTER both handshakes succeed. A naive
	// two-step "upgrade dest, publish; upgrade client, publish"
	// would leave the relay in a mixed state if the second handshake
	// failed (e.g. dest already TLS-wrapped, client still cleartext)
	// — the forwarders would then be moving TLS bytes one way and
	// plaintext the other, corrupting any traffic in flight before
	// the outer layer torn the sockets down. The local-then-store
	// pattern keeps the corruption window at zero.
	//
	// The two handshakes are independent (keploy is the TLS server for one
	// and the TLS client for the other, on two separate sockets), so the
	// order between them is a policy choice, not a protocol constraint. It is
	// destination-first by default — the order every release has used — and
	// client-first under Config.ClientTLSFirst, which upstream verification
	// turns on so the dest dial can use the ServerName the application itself
	// sent rather than the IP eBPF reported. See Config.ClientTLSFirst.
	//
	// Whichever runs second closes whatever the first produced on failure:
	// nothing is published until both have succeeded, so a half-upgraded
	// relay is never observable by the forwarders parked on the barrier.
	var (
		upgradedDst net.Conn
		upgradedSrc net.Conn
		// heldConsumedByHandshake records that the client write hold's
		// remainder was handed to the client-side handshake as a prepend,
		// and is therefore no longer the relay's to deliver — while a copy
		// of it is still stranded in the parser's stream. Set at the
		// hand-over rather than after a successful handshake: the bytes are
		// spent either way, and a failed handshake that reported them
		// unspent would have endUpgradePause try to deliver bytes that are
		// already gone.
		//
		// Distinct from `upgradedSrc != nil`, and NOT for the reason it
		// is tempting to write down. It is not about ClientTLSFirst
		// ordering: upgradedSrc is assigned at the end of upgradeClient
		// and upgradeDest's failure path only Closes it, so
		// `upgradedSrc != nil` is true there too.
		//
		// The case that separates them is a parser that flushes the
		// WHOLE hold — ClientFlushBytes covering everything held, so
		// nothing is left to prepend — and then upgrades successfully.
		// `upgradedSrc != nil` is true, but no held byte was consumed by
		// the handshake: every one of them went to the real server.
		// Discarding them from the parser's stream on that basis would
		// hide bytes the server actually received. See endUpgradePause.
		heldConsumedByHandshake bool
	)

	upgradeDest := func() *directive.Ack {
		if params.DestTLSConfig == nil {
			return nil
		}
		dst := *r.dst.Load()
		// If the D2C forwarder stashed any cleartext bytes beyond the
		// preamble window (e.g. a protocol that frames more than the
		// parser's PreambleReadFromDest tells us about), prepend them
		// to the dst handshake conn so tls.Client.Handshake reads
		// them as the first bytes of the ServerHello sequence. The
		// canonical Postgres SSL flow (1-byte 'S'/'N' preamble, then
		// pure TLS) leaves nothing past the preamble, so this branch
		// is defensive — but it keeps the contract that no stashed
		// bytes are ever silently dropped at the upgrade boundary.
		if stashed := r.takeStashed(fakeconn.FromDest); stashed.len() > 0 {
			if log != nil {
				log.Debug("relay: prepending stashed dst bytes to TLS handshake",
					zap.Int("bytes", stashed.len()),
				)
			}
			dst = newPrependingConnWithReadAt(dst, stashed.bytes, stashed.readAt)
		}
		trackedDst := newReadTrackingConn(dst)
		var err error
		upgradedDst, err = r.cfg.TLSUpgradeFn(ctx, trackedDst, true, params.DestTLSConfig)
		if err != nil {
			if log != nil {
				// Debug-level: TLS upgrade failures are expected on some
				// environments (self-signed dest certs, TLS-optional
				// servers, parser probing behaviour). The supervisor's
				// FallthroughToPassthrough signal already surfaces the
				// condition; an actionable error is returned in the Ack
				// and the parser decides whether to mark the mock
				// incomplete. No operator log action is needed.
				log.Debug("relay: dest-side TLS upgrade failed",
					zap.Error(err),
					zap.String("directive_reason", d.Reason),
					zap.String("next_step", "if the upstream uses a self-signed or private-CA cert, either turn on record.upstreamTls.verify with record.upstreamTls.caCert pointing at its CA PEM (resolved on the agent's filesystem), or leave verification off — the default"),
				)
			}
			// If the client side was upgraded first (Config.ClientTLSFirst),
			// its *tls.Conn was never published either — close it so the
			// handshake state does not outlive this failed directive.
			if upgradedSrc != nil {
				_ = upgradedSrc.Close()
			}
			r.endUpgradePause(heldAtEntry, heldConsumedByHandshake, barrierTeeOffset)
			return &directive.Ack{
				Kind:            d.Kind,
				OK:              false,
				Err:             fmt.Errorf("dest TLS upgrade: %w", err),
				PreamblePayload: preamblePayload,
			}
		}
		// V2-relay equivalent of dialPostgresSSLUpstream's
		// cb.RegisterReal call. Capture the real upstream cert here
		// so the channel-binding shim can rendezvous it with the MITM
		// cert minted by CertForClient. This is the chokepoint for
		// every parser that drives upstream TLS through the supervisor
		// relay (Postgres V3, MySQL, Mongo, etc.) so wiring here
		// covers all of them without per-parser changes.
		//
		// Order matters: must run BEFORE newReadTimeReportingConn
		// below. That wrapper embeds net.Conn anonymously and does
		// not implement Unwrap()/NetConn(), so unwrapToTLSConn cannot
		// see through it to the underlying *tls.Conn — the hook
		// would silently no-op and cbmap stays empty. Verified by
		// running keploy record --debug locally with psycopg2-binary
		// + channel_binding=require: with the hook in this position,
		// "relay: RealCertHook fired" prints and SCRAM-PLUS succeeds;
		// with the hook after the wrapper, it never logs.
		publishRealCertFromUpgraded(r, upgradedDst, log)

		upgradedDst = newReadTimeReportingConn(upgradedDst, trackedDst)
		return nil
	}

	upgradeClient := func() *directive.Ack {
		if params.ClientTLSConfig == nil {
			return nil
		}
		src := *r.src.Load()
		// If the C2D forwarder stashed any bytes (canonically the
		// SUT's TLS ClientHello, which can land in the C2D forwarder's
		// pre-pause Read when the SUT pipelines its SSLRequest tightly
		// with its ClientHello — observed intermittently with lib/pq
		// + libpq under sslmode=require, surfaced as the listmonk +
		// pgtype-tour `candidates: 0` hash misses on TLS-enabled CI
		// fixtures), prepend them to the src handshake conn. Without
		// this, tls.Server.Handshake reads from the bare TCP socket,
		// MISSES the stashed ClientHello bytes (they were already
		// consumed by the forwarder's Read), and either hangs forever
		// or returns "tls: server did not echo the legacy session ID"
		// on the SUT side / "tls: illegal parameter" on the dst side
		// once whatever bytes DO arrive are interpreted as a partial
		// handshake state. The connection then falls through to
		// passthrough, the recorder sees zero queries on it, and
		// every test that happened to land on that connection misses
		// at replay time.
		//
		// The takeStashed call also clears r.stashedC2D so endPause
		// does not silently drop those same bytes after the handshake
		// returns.
		if stashed := r.takeStashed(fakeconn.FromClient); stashed.len() > 0 {
			if log != nil {
				log.Debug("relay: prepending stashed src bytes to TLS handshake",
					zap.Int("bytes", stashed.len()),
				)
			}
			src = newPrependingConnWithReadAt(src, stashed.bytes, stashed.readAt)
			heldConsumedByHandshake = true
		}
		trackedSrc := newReadTrackingConn(src)
		var err error
		upgradedSrc, err = r.cfg.TLSUpgradeFn(ctx, trackedSrc, false, params.ClientTLSConfig)
		if err != nil {
			if log != nil {
				// Debug-level: see dest-side upgrade comment above.
				log.Debug("relay: client-side TLS upgrade failed",
					zap.Error(err),
					zap.String("directive_reason", d.Reason),
					zap.String("next_step", "check the MITM cert chain configuration; run with KEPLOY_DISABLE_PARSING=1 to bypass parsing entirely"),
				)
			}
			// The dest-side handshake may have succeeded and allocated
			// a *tls.Conn wrapper around r.dst's socket (if
			// DestTLSConfig != nil). We never published it to
			// r.dst.Load(), so the forwarders still see the original
			// cleartext conn; the wrapper will be GC'd. The outer
			// layer will tear the connection down on this error.
			if upgradedDst != nil {
				_ = upgradedDst.Close()
			}
			r.endUpgradePause(heldAtEntry, heldConsumedByHandshake, barrierTeeOffset)
			return &directive.Ack{
				Kind:            d.Kind,
				OK:              false,
				Err:             fmt.Errorf("client TLS upgrade: %w", err),
				PreamblePayload: preamblePayload,
			}
		}
		upgradedSrc = newReadTimeReportingConn(upgradedSrc, trackedSrc)
		return nil
	}

	steps := []func() *directive.Ack{upgradeDest, upgradeClient}
	if r.cfg.ClientTLSFirst {
		steps = []func() *directive.Ack{upgradeClient, upgradeDest}
	}
	for _, step := range steps {
		if ack := step(); ack != nil {
			return *ack
		}
	}

	// Both handshakes (or only those requested) succeeded. Publish
	// atomically — the forwarders still on their pause barrier
	// above will observe the new conns the instant we call
	// r.endPause below. Until then, no side has seen the swap.
	if upgradedDst != nil {
		r.dst.Store(&upgradedDst)
	}
	if upgradedSrc != nil {
		r.src.Store(&upgradedSrc)
	}

	boundaryWrittenAt := time.Now()
	// The CLIENT-side handshake is what consumes the held remainder (it
	// is that side's ClientHello). A dest-only upgrade never claims it,
	// so it stays plain client bytes the server is still owed — and the
	// parser's copy of them stays legitimate stream content.
	r.endUpgradePause(heldAtEntry, heldConsumedByHandshake, barrierTeeOffset)

	return directive.Ack{
		Kind:              d.Kind,
		OK:                true,
		PreamblePayload:   preamblePayload,
		TLSUpgraded:       upgradedDst != nil || upgradedSrc != nil,
		BoundaryReadAt:    boundaryReadAt,
		BoundaryWrittenAt: boundaryWrittenAt,
	}
}

// readFullPreamble reads exactly len(buf) bytes from conn into buf.
// Returns the number of bytes read and the first error encountered.
// io.ErrUnexpectedEOF is returned on a partial read with EOF.
//
// We keep this as a thin wrapper rather than calling io.ReadFull
// directly so the loop is visible in stack traces and so future
// changes (e.g. a deadline / cancellation hook) have a single place
// to land.
func readFullPreamble(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			// A zero-byte non-error Read shouldn't happen on a TCP
			// socket; treat as protocol error to avoid an infinite
			// busy loop.
			return total, fmt.Errorf("zero-byte read after %d/%d bytes", total, len(buf))
		}
	}
	return total, nil
}

// bytesEqual reports whether a and b are byte-for-byte equal.
// Used to gate ProceedOnPreamble on an exact match. Inlined here so
// the relay package does not pull in bytes.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// handlePauseDir pauses tee delivery for d.Dir. Real forwarding is
// NOT affected — bytes still flow between the peers. This is a
// parser-facing mute, used when the parser wants to keep the TCP
// connection alive but stop receiving chunks (e.g. a mock has been
// finalized and further traffic is noise).
func (r *Relay) handlePauseDir(d directive.Directive) directive.Ack {
	t := r.teeFor(d.Dir)
	if t == nil {
		return directive.Ack{
			Kind: d.Kind,
			OK:   false,
			Err:  fmt.Errorf("relay: unknown direction %d", d.Dir),
		}
	}
	t.setPaused(true)
	return directive.Ack{Kind: d.Kind, OK: true}
}

// handleResumeDir reverses a KindPauseDir.
func (r *Relay) handleResumeDir(d directive.Directive) directive.Ack {
	t := r.teeFor(d.Dir)
	if t == nil {
		return directive.Ack{
			Kind: d.Kind,
			OK:   false,
			Err:  fmt.Errorf("relay: unknown direction %d", d.Dir),
		}
	}
	t.setPaused(false)
	return directive.Ack{Kind: d.Kind, OK: true}
}

// handleAbortMock marks the mock incomplete and keeps forwarding.
// The parser is signalling "I'm giving up on this mock, but the TCP
// connection is still healthy — don't touch it."
func (r *Relay) handleAbortMock(d directive.Directive) directive.Ack {
	if r.cfg.OnMarkMockIncomplete != nil {
		reason := d.Reason
		if reason == "" {
			reason = "abort_mock"
		}
		r.cfg.OnMarkMockIncomplete(reason)
	}
	return directive.Ack{Kind: d.Kind, OK: true}
}

// handleResumePreDispatch ends the pre-dispatch pause window installed
// by run() under Config.PreDispatchPause. It:
//
//  1. Nudges both real sockets' read deadlines into the past and waits
//     for both forwarders to mark themselves parked on the pause
//     channel. This is the critical synchronisation step. Pre-dispatch
//     starts WITHOUT a deadline kick (so the forwarders' first Read
//     proceeds), so by the time this directive arrives a forwarder may
//     still be blocked in a Read that hasn't returned yet. If we
//     snapshot the stash and call endPause without first draining the
//     in-flight Reads through the post-Read pause path, those Reads
//     would return AFTER our snapshot took the stash, land in the
//     post-Read recheck still seeing pauseCh != nil, append to the
//     stash, and then be dropped by endPause's unconditional
//     stashedC2D/stashedD2C = nil. Bytes lost silently, live stream
//     desynchronizes.
//
//  2. Pulls everything from the per-direction stashes under stashMu.
//     With both forwarders parked, no concurrent appends can race
//     this read. The bytes are the prefix the forwarders teed during
//     pre-dispatch but did NOT write to the real peer.
//
//  3. Writes each stashed payload to the corresponding live peer in
//     read order. C2D stash goes to dst (the real upstream service);
//     D2C stash goes to src (the real client). A short write or error
//     on either peer is logged and surfaced via Ack.OK=false, but we
//     still endPause so the connection's forwarders aren't permanently
//     stuck — the supervisor will fall through to passthrough.
//
//  4. Clears r.preDispatchActive so subsequent forward-loop iterations
//     take the standard path (pre-Read park works, post-Read recheck
//     stops teeing during transient pauses).
//
//  5. Calls endPause to close the pause channel — forwarders parked on
//     it wake up and resume normal Read→Write→Tee operation.
//
// handleReleaseClient ends a [Config.HoldClientWrites] hold without a
// TLS upgrade: every byte held for the real destination is written to
// it in read order and the C2D forwarder goes back to forwarding.
//
// This is the path a parser takes when it has inspected the client's
// opening message and concluded the session stays cleartext — MySQL
// sending a plain HandshakeResponse rather than an SSLRequest. Nothing
// about the connection changes except that our brake comes off, so
// unlike the TLS path there is no handshake, no socket swap, and the
// bytes are delivered rather than consumed.
//
// It runs under the pause barrier for the same reason the TLS path
// does: the forwarder appends to the same stash we are draining, and
// the parked-wait is what makes "take the stash" mean "take all of it".
func (r *Relay) handleReleaseClient(ctx context.Context, stopping <-chan struct{}, d directive.Directive) directive.Ack {
	log := r.cfg.Logger

	// Reject a release for a hold that is not up. As with
	// resume-pre-dispatch, the alternative is worse than an error: the
	// pause below nudges read deadlines into the past, and endPause
	// only restores them while a pause is actually installed, so a
	// stray directive could leave the forwarders spinning on EAGAIN
	// for the rest of the connection.
	if !r.holdClient.Load() {
		return directive.Ack{
			Kind: d.Kind,
			OK:   false,
			Err:  errors.New("release-client: no active client write hold to release"),
		}
	}

	r.beginPause()
	r.waitForwardersParked(ctx, stopping)

	// beginPause put a past read deadline on both sockets to wake the
	// forwarders. Clear it before writing: a *tls.Conn Write can need
	// to read (renegotiation, or simply the handshake it defers to the
	// first I/O), and it would fail on the stale deadline.
	clearDeadline(r.dst.Load())
	clearDeadline(r.src.Load())

	err := r.releaseClientHold()
	if err != nil && log != nil {
		log.Debug("relay: client hold release flush failed",
			zap.Error(err),
			zap.String("directive_reason", d.Reason),
		)
	}
	if err != nil && r.cfg.OnMarkMockIncomplete != nil {
		r.cfg.OnMarkMockIncomplete("write_error")
	}

	// Deliver anything the D2C forwarder stashed at the pause boundary
	// before endPause drops it.
	//
	// endPause discards both stashes unconditionally, which is right
	// after a TLS upgrade — the socket underneath has been replaced and
	// cleartext bytes read before it have nowhere sensible to go. It is
	// wrong here: a plain release changes nothing about the connection,
	// so server bytes read at the boundary are still ordinary response
	// data the client is waiting for, and dropping them is silent
	// user-traffic loss on a directive whose whole contract is "our
	// brake comes off, nothing else changes".
	if held := r.takeStashed(fakeconn.FromDest); held.len() > 0 {
		if srcPtr := r.src.Load(); srcPtr != nil {
			if _, werr := (*srcPtr).Write(held.bytes); werr != nil && log != nil {
				log.Debug("relay: delivering the D2C pause stash on release failed",
					zap.Error(werr),
					zap.Int("bytes", held.len()),
				)
			}
		}
	}

	r.endPause()

	if err != nil {
		return directive.Ack{
			Kind: d.Kind,
			OK:   false,
			Err:  fmt.Errorf("release-client flush: %w", err),
		}
	}
	return directive.Ack{Kind: d.Kind, OK: true}
}

func (r *Relay) handleResumePreDispatch(ctx context.Context, stopping <-chan struct{}, d directive.Directive) directive.Ack {
	log := r.cfg.Logger

	// Defensive precondition: only act on connections that actually
	// have an active pre-dispatch pause. A duplicate ResumePreDispatch
	// or a parser bug that fires it after the pause already ended
	// (e.g. UpgradeTLS already ran) would otherwise nudge deadlines
	// into the past on the live sockets — and since endPause only
	// clears deadlines while pauseCh is non-nil, those past deadlines
	// would persist, putting the forwarders into a tight EAGAIN loop
	// for the rest of the connection. Reject loudly instead: the
	// supervisor will surface the error and the connection falls
	// through to passthrough, which is the right blast radius for a
	// directive-protocol violation.
	if !r.preDispatchActive.Load() || r.currentPauseCh() == nil {
		return directive.Ack{
			Kind: d.Kind,
			OK:   false,
			Err:  errors.New("resume-pre-dispatch: no active pre-dispatch pause to release"),
		}
	}

	// Synchronise with the forwarders before touching the stash. The
	// nudge wakes any forwarder blocked in Read; the wait ensures both
	// of them have reached the post-Read pause path (either with bytes
	// to stash, or with a deadline error producing no stash) and
	// marked themselves parked. After this returns, both forwarders
	// are blocked on pauseCh and no concurrent stash appends can race
	// the snapshot below.
	r.nudgeDeadline(r.dst.Load())
	r.nudgeDeadline(r.src.Load())
	r.waitForwardersParked(ctx, stopping)

	r.stashMu.Lock()
	c2dStash := r.stashedC2D
	d2cStash := r.stashedD2C
	r.stashedC2D = nil
	r.stashedD2C = nil
	r.stashMu.Unlock()

	// Clear the pre-dispatch flag before draining so any path that
	// reads it (currently only the forward loop, which is parked
	// behind pauseCh until endPause below — the flag clear here is
	// belt-and-braces) sees the standard pause semantics.
	r.preDispatchActive.Store(false)

	// A client write hold means "resume" cannot be honoured by lifting
	// the pause alone: the forward loop would come back up and keep
	// swallowing client bytes, and this handler would have acked OK
	// while doing it. Run() refuses to arm both brakes at once, so this
	// is unreachable today and stays as a guard: if a hold is up, a
	// resume ends it. The stash claimed above is drained below, so
	// clear the flag and the counter rather than calling
	// releaseClientHold, which would try to drain it a second time.
	if r.holdClient.Swap(false) {
		r.heldClientBytes.Store(0)
	}

	var drainErr error
	if len(c2dStash) > 0 {
		dst := *r.dst.Load()
		for _, p := range c2dStash {
			if dst == nil {
				break
			}
			wn, werr := dst.Write(p.bytes)
			if werr != nil {
				if log != nil {
					log.Debug("relay: ResumePreDispatch C2D drain write error",
						zap.Error(werr),
						zap.Int("payload_bytes", len(p.bytes)),
						zap.Int("written_bytes", wn),
					)
				}
				if r.cfg.OnMarkMockIncomplete != nil {
					r.cfg.OnMarkMockIncomplete("pre_dispatch_drain_c2d_write_error")
				}
				drainErr = fmt.Errorf("resume-pre-dispatch C2D drain: %w", werr)
				break
			}
			if wn != len(p.bytes) {
				if r.cfg.OnMarkMockIncomplete != nil {
					r.cfg.OnMarkMockIncomplete("pre_dispatch_drain_c2d_short_write")
				}
				drainErr = errors.New("resume-pre-dispatch C2D drain: short write on destination socket")
				break
			}
		}
	}
	if drainErr == nil && len(d2cStash) > 0 {
		src := *r.src.Load()
		for _, p := range d2cStash {
			if src == nil {
				break
			}
			wn, werr := src.Write(p.bytes)
			if werr != nil {
				if log != nil {
					log.Debug("relay: ResumePreDispatch D2C drain write error",
						zap.Error(werr),
						zap.Int("payload_bytes", len(p.bytes)),
						zap.Int("written_bytes", wn),
					)
				}
				if r.cfg.OnMarkMockIncomplete != nil {
					r.cfg.OnMarkMockIncomplete("pre_dispatch_drain_d2c_write_error")
				}
				drainErr = fmt.Errorf("resume-pre-dispatch D2C drain: %w", werr)
				break
			}
			if wn != len(p.bytes) {
				if r.cfg.OnMarkMockIncomplete != nil {
					r.cfg.OnMarkMockIncomplete("pre_dispatch_drain_d2c_short_write")
				}
				drainErr = errors.New("resume-pre-dispatch D2C drain: short write on source socket")
				break
			}
		}
	}

	// Always endPause, even on a partial drain: the alternative is
	// to leave the forwarders permanently parked on the pause channel
	// while the supervisor decides what to do, which deadlocks the
	// only path that can tear the relay down (parser exits → relayCtx
	// cancels → forwarders return via ctx.Done in their pause select).
	// That select IS armed (see the pre-Read park) but we'd rather not
	// rely on cancellation timing here — making the wake-up
	// unconditional is cheap and easier to reason about.
	r.endPause()

	if drainErr != nil {
		return directive.Ack{Kind: d.Kind, OK: false, Err: drainErr}
	}
	return directive.Ack{Kind: d.Kind, OK: true}
}

// teeFor returns the tee for the given direction, or nil if the
// direction is not recognised.
func (r *Relay) teeFor(d fakeconn.Direction) *tee {
	switch d {
	case fakeconn.FromClient:
		return r.teeC2D
	case fakeconn.FromDest:
		return r.teeD2C
	default:
		return nil
	}
}

// publishRealCertFromUpgraded extracts the real upstream peer cert
// from the just-upgraded dest connection and notifies the configured
// RealCertHook (typically cbshim.RegisterReal). Keyed by the app's
// source-port (matching what tls.CertForClient registered as the
// MITM half via MITMPublishHook).
//
// Best-effort: if the cfg has no hook, if the upgraded conn isn't a
// *tls.Conn (defensive — should always be), if peer certs are empty,
// or if the source-port can't be resolved, we silently skip — channel
// binding then fails as it would without this feature for that
// connection.
func publishRealCertFromUpgraded(r *Relay, upgraded net.Conn, log *zap.Logger) {
	if r.cfg.RealCertHook == nil {
		return
	}
	tlsView, ok := unwrapToTLSConn(upgraded)
	if !ok {
		return
	}
	state := tlsView.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return
	}
	leaf := state.PeerCertificates[0]

	src := r.src.Load()
	if src == nil || *src == nil {
		return
	}
	tcpAddr, ok := (*src).RemoteAddr().(*net.TCPAddr)
	if !ok || tcpAddr == nil || tcpAddr.Port == 0 {
		// Port==0 means the addr resolution returned a sentinel /
		// unspecified port (typically a wrapped conn that doesn't
		// surface the underlying TCP port). connID "0" would
		// rendezvous with every other Port==0 connection in cbshim's
		// pending map, so skip — matches tls.publishMITM's
		// sourcePort==0 guard.
		return
	}
	r.cfg.RealCertHook(strconv.Itoa(tcpAddr.Port), leaf.Raw, leaf.SignatureAlgorithm)
	if log != nil {
		// Gated under log.Check so leaf.Subject.String() (an x509
		// pkix.Name format/escape walk) doesn't run on every TLS
		// upgrade when debug is off. Fires per UpgradeTLS directive,
		// so the cost is non-trivial under load.
		if ce := log.Check(zap.DebugLevel, "relay: RealCertHook fired"); ce != nil {
			ce.Write(
				zap.String("conn_id", strconv.Itoa(tcpAddr.Port)),
				zap.String("subject", leaf.Subject.String()),
			)
		}
	}
}

// unwrapToTLSConn walks a small chain of net.Conn wrappers looking
// for the underlying *tls.Conn. Mirrors the same pattern used by the
// V1 parser path in util/tls_upgrader.go.
func unwrapToTLSConn(c net.Conn) (*tls.Conn, bool) {
	type unwrapper interface{ Unwrap() net.Conn }
	type netConner interface{ NetConn() net.Conn }
	for i := 0; i < 8 && c != nil; i++ {
		if tc, ok := c.(*tls.Conn); ok {
			return tc, true
		}
		if u, ok := c.(unwrapper); ok {
			c = u.Unwrap()
			continue
		}
		if w, ok := c.(netConner); ok {
			c = w.NetConn()
			continue
		}
		return nil, false
	}
	return nil, false
}
