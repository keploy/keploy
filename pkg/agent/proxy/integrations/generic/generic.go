package generic

import (
	"context"
	"errors"
	"fmt"
	"net"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func init() {
	integrations.Register(integrations.GENERIC, &integrations.Parsers{
		Initializer: New,
		Priority:    100,
	})
}

type Generic struct {
	logger *zap.Logger
}

func New(logger *zap.Logger) integrations.Integrations {
	return &Generic{
		logger: logger,
	}
}

func (g *Generic) MatchType(_ context.Context, _ []byte) bool {
	// generic is checked explicitly in the proxy
	return false
}

// IsV2 reports that this parser consumes RecordSession.V2 and is safe
// to run under supervisor.Run. The generic parser is protocol-agnostic
// and needs no TLS upgrade or mid-stream directives; it simply observes
// chunks in each direction and pairs them into mocks.
// Generic deliberately does NOT implement integrations.GapResyncCapable, and
// should not be "fixed" to.
//
// The tempting argument is that the conservative default exists for
// length-prefix framers — a hole makes them read the next header from the middle
// of a body, so every later frame is garbage and one Postgres framer once tried
// to allocate gigabytes from misread row data. Generic has no framer at all; it
// pairs chunks by direction transitions. So the framer rationale does not apply,
// and it looks like generic is being penalised for someone else's failure mode.
//
// But the capability asserts something generic cannot honour. Returning true
// means the parser DETECTS the hole — the relay stamps a monotonic
// fakeconn.Chunk.SeqNo per direction and a dropped chunk still consumes its
// ordinal, so a gap is visible as a discontinuity — and re-anchors on a real
// message boundary. This recorder reads no SeqNo and has no notion of a
// boundary to re-anchor on.
//
// Its failure mode after a hole is not garbage frames; it is worse in the way
// that matters. It would pair a request with the WRONG response and emit a mock
// that looks perfectly well-formed, which replays incorrectly and silently. The
// conservative default instead marks the capture desynced and suppresses the
// affected test cases — loud, and safe. Losing a test case beats shipping a
// confidently wrong one.
//
// Implementing real gap detection here (compare SeqNo, drop the in-flight
// exchange, resume cleanly on the next request) would justify flipping this. A
// bare `return true` would not.

func (g *Generic) IsV2() bool { return true }

func (g *Generic) RecordOutgoing(ctx context.Context, session *integrations.RecordSession) error {
	if session != nil && session.V2 != nil {
		return g.recordV2(ctx, session.V2)
	}
	return g.recordLegacy(ctx, session)
}

func (g *Generic) recordLegacy(ctx context.Context, session *integrations.RecordSession) error {
	if session == nil {
		return errors.New("generic: record session is nil; ensure RecordOutgoing is called with a valid session")
	}
	logger := session.Logger

	ingress, err := session.IngressConn()
	if err != nil {
		return fmt.Errorf("generic: %w", err)
	}
	egress, err := session.EgressConn()
	if err != nil {
		return fmt.Errorf("generic: %w", err)
	}

	reqBuf, err := util.ReadInitialBuf(ctx, logger, ingress)
	if err != nil {
		utils.LogError(logger, err, "failed to read the initial generic message")
		return err
	}

	err = encodeGeneric(ctx, logger, reqBuf, ingress, egress, session.Mocks, session.Opts)
	if err != nil {
		utils.LogError(logger, err, "failed to encode the generic message into the yaml")
		return err
	}
	return nil
}

// recordV2 is the native V2 record path for the generic parser. It reads
// chunks from sess.ClientStream (requests) and sess.DestStream (responses)
// concurrently and pairs them into mocks. Timestamps are carried from the
// chunks rather than being captured with time.Now() so replay ordering is
// preserved exactly as it was seen at the real-socket boundary.
//
// Exchange boundary: mirroring the legacy generic parser, a req/resp pair
// is flushed the moment the first response chunk arrives after a run of
// request chunks. A response split across multiple chunks lands as only
// its head chunk, same as the legacy path. Subsequent "orphan" response
// chunks are dropped at the start of the next request, also matching the
// legacy path. Protocol-aware parsers should supersede generic where
// framing matters; preserving this behaviour keeps replay compatible
// with mocks recorded before the V2 migration.
func (g *Generic) recordV2(_ context.Context, sess *supervisor.Session) error {
	if sess == nil {
		return nil
	}
	logger := sess.Logger
	if logger == nil {
		logger = g.logger
	}
	return encodeGenericV2(sess, logger)
}

func (g *Generic) MockOutgoing(ctx context.Context, src net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	logger := g.logger.With(zap.String("Client ConnectionID", ctx.Value(models.ClientConnectionIDKey).(string)), zap.String("Destination ConnectionID", ctx.Value(models.DestConnectionIDKey).(string)), zap.String("Client IP Address", src.RemoteAddr().String()))
	reqBuf, err := util.ReadInitialBuf(ctx, logger, src)
	if err != nil {
		utils.LogError(logger, err, "failed to read the initial generic message")
		return err
	}

	err = decodeGeneric(ctx, logger, reqBuf, src, dstCfg, mockDb, opts)
	if err != nil {
		utils.LogError(logger, err, "failed to decode the generic message")
		return err
	}
	return nil
}
