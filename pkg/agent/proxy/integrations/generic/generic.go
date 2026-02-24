package generic

import (
	"context"
	"net"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/orchestrator"
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

func (g *Generic) RecordOutgoing(ctx context.Context, src net.Conn, dst net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	logger := g.logger.With(zap.String("Client ConnectionID", ctx.Value(models.ClientConnectionIDKey).(string)), zap.String("Destination ConnectionID", ctx.Value(models.DestConnectionIDKey).(string)), zap.String("Client IP Address", src.RemoteAddr().String()))

	reqBuf, err := util.ReadInitialBuf(ctx, logger, src)
	if err != nil {
		utils.LogError(logger, err, "failed to read the initial generic message")
		return err
	}

	// Forward initial request to destination (already read from src, needs explicit forward)
	if _, err := dst.Write(reqBuf); err != nil {
		utils.LogError(logger, err, "failed to forward initial request to destination. Check destination server connectivity and verify the address is correct")
		return err
	}

	// Create TeeForwardConn wrappers for zero-latency forwarding.
	// Dedicated goroutines read from src/dst and forward at wire speed,
	// while the parser reads buffered copies asynchronously.
	clientTee := orchestrator.NewTeeForwardConn(ctx, logger, src, dst)
	destTee := orchestrator.NewTeeForwardConn(ctx, logger, dst, src)

	err = encodeGeneric(ctx, logger, reqBuf, clientTee, destTee, mocks, opts)
	if err != nil {
		utils.LogError(logger, err, "failed to encode the generic message into the yaml")
		return err
	}
	return nil
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
