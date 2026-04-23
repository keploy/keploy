package relay

import (
	"context"
	"fmt"
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

	if params.DestTLSConfig != nil {
		dst := *r.dst.Load()
		upgraded, err := r.cfg.TLSUpgradeFn(ctx, dst, true, params.DestTLSConfig)
		if err != nil {
			if log != nil {
				log.Warn("relay: dest-side TLS upgrade failed", zap.Error(err))
			}
			r.endPause()
			return directive.Ack{
				Kind: d.Kind,
				OK:   false,
				Err:  fmt.Errorf("dest TLS upgrade: %w", err),
			}
		}
		r.dst.Store(&upgraded)
	}

	if params.ClientTLSConfig != nil {
		src := *r.src.Load()
		upgraded, err := r.cfg.TLSUpgradeFn(ctx, src, false, params.ClientTLSConfig)
		if err != nil {
			if log != nil {
				log.Warn("relay: client-side TLS upgrade failed", zap.Error(err))
			}
			// We have already upgraded dest. The real client side
			// is still cleartext. Leave the conn pointers as-is
			// (dest upgraded, src cleartext); the caller will tear
			// the connection down. Release pause so the forwarders
			// exit via the subsequent read error.
			r.endPause()
			return directive.Ack{
				Kind: d.Kind,
				OK:   false,
				Err:  fmt.Errorf("client TLS upgrade: %w", err),
			}
		}
		r.src.Store(&upgraded)
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
