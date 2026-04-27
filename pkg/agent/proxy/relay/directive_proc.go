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
			ack := r.handleDirective(ctx, d)
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
func (r *Relay) handleDirective(ctx context.Context, d directive.Directive) directive.Ack {
	switch d.Kind {
	case directive.KindUpgradeTLS:
		return r.handleUpgradeTLS(ctx, d)
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
//  2. Handshake dest first (keploy = TLS client to real server),
//     then client (keploy = TLS server, presenting MITM cert).
//  3. On either failure, release the pause and return OK=false; the
//     relay stays on the original (cleartext) conns. The supervisor
//     is expected to fall through to raw passthrough.
//  4. On success, replace the atomic conn pointers with the upgraded
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
// for boundary detection on the parser.
func (r *Relay) handleUpgradeTLS(ctx context.Context, d directive.Directive) directive.Ack {
	log := r.cfg.Logger
	if r.cfg.TLSUpgradeFn == nil {
		return directive.Ack{Kind: d.Kind, OK: false, Err: ErrNoTLSUpgrader}
	}
	params := d.TLS
	if params == nil {
		params = &directive.UpgradeTLSParams{}
	}

	// Barrier up. Forwarders will park on their next loop iteration.
	// We don't have a precise "forwarders have parked" signal, but
	// that is acceptable: any bytes they read just before parking
	// will still be forwarded on cleartext; the parser will have
	// already marked the prelude mock complete. The new TLS stream
	// starts from whatever the real peer sends next.
	r.beginPause()

	boundaryReadAt := time.Now()

	// Atomic two-sided upgrade: run both handshakes FIRST (keeping
	// the new *tls.Conn values in local vars), only publish the
	// upgraded conn pointers via r.{dst,src}.Store AFTER both
	// handshakes succeed. A naive two-step "upgrade dest, publish;
	// upgrade client, publish" would leave the relay in a mixed state
	// if the second handshake failed (e.g. dest already TLS-wrapped,
	// client still cleartext) — the forwarders would then be moving
	// TLS bytes one way and plaintext the other, corrupting any
	// traffic in flight before the outer layer torn the sockets
	// down. The local-then-store pattern keeps the corruption window
	// at zero.
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
				Kind: d.Kind,
				OK:   false,
				Err:  fmt.Errorf("dest TLS upgrade: %w", err),
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
				Kind: d.Kind,
				OK:   false,
				Err:  fmt.Errorf("client TLS upgrade: %w", err),
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
		BoundaryReadAt:    boundaryReadAt,
		BoundaryWrittenAt: boundaryWrittenAt,
	}
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
