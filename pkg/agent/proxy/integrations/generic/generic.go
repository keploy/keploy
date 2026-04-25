package generic

import (
	"context"
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
func (g *Generic) IsV2() bool { return true }

func (g *Generic) RecordOutgoing(ctx context.Context, session *integrations.RecordSession) error {
	if session != nil && session.V2 != nil {
		return g.recordV2(ctx, session.V2)
	}
	return g.recordLegacy(ctx, session)
}

func (g *Generic) recordLegacy(ctx context.Context, session *integrations.RecordSession) error {
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
