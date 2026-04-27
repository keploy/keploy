package relay

import (
	"context"
	"fmt"
	"net"
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
	if r.cfg.TLSUpgradeFn == nil {
		return directive.Ack{Kind: d.Kind, OK: false, Err: ErrNoTLSUpgrader}
	}
	params := d.TLS
	if params == nil {
		params = &directive.UpgradeTLSParams{}
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

	var preamblePayload []byte
	if params.PreambleReadFromDest > 0 {
		// 1a. Try the D2C stash first.
		if stashed := r.takeStashed(fakeconn.FromDest); len(stashed) > 0 {
			if len(stashed) >= params.PreambleReadFromDest {
				preamblePayload = stashed[:params.PreambleReadFromDest]
				// If the forwarder stashed more than we needed
				// (unlikely for a 1-byte preamble but defensive
				// against future protocols), the surplus is dropped
				// — the upgraded socket has no clean way to
				// consume cleartext bytes captured pre-upgrade.
				if log != nil && len(stashed) > params.PreambleReadFromDest {
					log.Debug("relay: dropping stashed surplus past preamble window",
						zap.Int("requested", params.PreambleReadFromDest),
						zap.Int("stashed", len(stashed)),
					)
				}
			} else {
				// Stash fell short of what the parser asked for;
				// top up by reading the remainder directly from
				// the live socket. This branch is rare in
				// practice — the Postgres SSL preamble is a
				// single byte — but keeps the contract strict for
				// future protocols.
				preamblePayload = make([]byte, params.PreambleReadFromDest)
				copy(preamblePayload, stashed)
				clearDeadline(r.dst.Load())
				dst := *r.dst.Load()
				_, err := readFullPreamble(dst, preamblePayload[len(stashed):])
				if err != nil {
					if log != nil {
						log.Debug("relay: TLS upgrade preamble read (post-stash) failed",
							zap.Error(err),
							zap.Int("stashed", len(stashed)),
							zap.Int("requested", params.PreambleReadFromDest),
							zap.String("directive_reason", d.Reason),
							zap.String("next_step", "the upstream closed mid-preamble; verify the destination is the protocol the parser was matched to and consider KEPLOY_DISABLE_PARSING=1 to bypass parsing"),
						)
					}
					r.endPause()
					return directive.Ack{
						Kind:            d.Kind,
						OK:              false,
						Err:             fmt.Errorf("TLS upgrade preamble read: %w", err),
						PreamblePayload: preamblePayload[:len(stashed)],
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
				r.endPause()
				return directive.Ack{
					Kind:            d.Kind,
					OK:              false,
					Err:             fmt.Errorf("TLS upgrade preamble read: %w", err),
					PreamblePayload: preamblePayload[:n],
				}
			}
		}

		// 1c. Drop the stashed C2D payload if any. The forwarder
		// captured the client's reply to the server's preamble
		// (e.g. TLS ClientHello after seeing 'S') without writing
		// it to the live dest socket. The upgraded src conn will
		// run its own handshake from a fresh ClientHello; the
		// stashed bytes have no place to go. We log the size at
		// debug so a future operator can correlate it with a
		// supervisor warning if the discard ever turns out to drop
		// genuine application bytes.
		if c2dStash := r.takeStashed(fakeconn.FromClient); len(c2dStash) > 0 && log != nil {
			log.Debug("relay: dropping stashed client-side bytes captured during pause",
				zap.Int("dropped_bytes", len(c2dStash)),
				zap.String("directive_reason", d.Reason),
			)
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
				r.endPause()
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
			r.endPause()
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
	var (
		upgradedDst net.Conn
		upgradedSrc net.Conn
	)

	if params.DestTLSConfig != nil {
		dst := *r.dst.Load()
		var err error
		upgradedDst, err = r.cfg.TLSUpgradeFn(ctx, dst, true, params.DestTLSConfig)
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
					zap.String("next_step", "if the upstream uses a self-signed or private-CA cert, add it to the system trust store or run with KEPLOY_NEW_RELAY=off to fall back to the legacy parser path"),
				)
			}
			r.endPause()
			return directive.Ack{
				Kind:            d.Kind,
				OK:              false,
				Err:             fmt.Errorf("dest TLS upgrade: %w", err),
				PreamblePayload: preamblePayload,
			}
		}
	}

	if params.ClientTLSConfig != nil {
		src := *r.src.Load()
		var err error
		upgradedSrc, err = r.cfg.TLSUpgradeFn(ctx, src, false, params.ClientTLSConfig)
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
			r.endPause()
			return directive.Ack{
				Kind:            d.Kind,
				OK:              false,
				Err:             fmt.Errorf("client TLS upgrade: %w", err),
				PreamblePayload: preamblePayload,
			}
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
	r.endPause()

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
