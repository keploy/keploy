package grpc

import (
	"bytes"
	"context"
	"net"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	// Register the parser with the proxy.
	integrations.Register("grpc", NewGrpc)
}

type Grpc struct {
	logger *zap.Logger
}

func NewGrpc(logger *zap.Logger) integrations.Integrations {
	return &Grpc{
		logger: logger,
	}
}

// MatchType function determines if the outgoing network call is gRPC by comparing the
// message format with that of an gRPC text message.
func (g *Grpc) MatchType(_ context.Context, reqBuf []byte) bool {
	return bytes.HasPrefix(reqBuf[:], []byte("PRI * HTTP/2"))
}

func (g *Grpc) RecordOutgoing(ctx context.Context, src net.Conn, dst net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	logger := g.logger.With(zap.Any("Client IP Address", src.RemoteAddr().String()), zap.Any("Client ConnectionID", ctx.Value(models.ClientConnectionIDKey).(string)), zap.Any("Destination ConnectionID", ctx.Value(models.DestConnectionIDKey).(string)))

	reqBuf, err := util.ReadInitialBuf(ctx, logger, src)
	if err != nil {
		utils.LogError(logger, err, "failed to read the initial grpc message")
		return err
	}

	err = encodeGrpc(ctx, logger, reqBuf, src, dst, mocks, opts)
	if err != nil {
		utils.LogError(logger, err, "failed to encode the grpc message into the yaml")
		return err
	}
	return nil
}

func (g *Grpc) MockOutgoing(ctx context.Context, src net.Conn, dstCfg *integrations.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	logger := g.logger.With(zap.Any("Client IP Address", src.RemoteAddr().String()), zap.Any("Client ConnectionID", ctx.Value(models.ClientConnectionIDKey).(string)), zap.Any("Destination ConnectionID", ctx.Value(models.DestConnectionIDKey).(string)))
	reqBuf, err := util.ReadInitialBuf(ctx, logger, src)
	if err != nil {
		utils.LogError(logger, err, "failed to read the initial grpc message")
		return err
	}

	err = decodeGrpc(ctx, logger, reqBuf, src, dstCfg, mockDb, opts)
	if err != nil {
		utils.LogError(logger, err, "failed to decode the grpc message from the yaml")
		return err
	}
	return nil
}
