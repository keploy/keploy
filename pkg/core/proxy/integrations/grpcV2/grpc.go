//go:build linux

package grpcV2

import (
	"bytes"
	"context"
	"net"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/status"
)

func init() {
	// Register the raw codec for passing raw bytes through the gRPC framework.
	encoding.RegisterCodec(new(rawCodec))

	integrations.Register(integrations.GRPC, &integrations.Parsers{
		Initializer: New,
		Priority:    100,
	})
}

type Grpc struct {
	logger *zap.Logger
}

func New(logger *zap.Logger) integrations.Integrations {
	return &Grpc{
		logger: logger,
	}
}

// MatchType determines if the outgoing network call is gRPC by checking for the HTTP/2 preface.
func (g *Grpc) MatchType(_ context.Context, reqBuf []byte) bool {
	const preface = "PRI * HTTP/2"
	if len(reqBuf) < len(preface) {
		return false
	}
	return bytes.HasPrefix(reqBuf, []byte(preface))
}

func (g *Grpc) RecordOutgoing(ctx context.Context, src net.Conn, dst net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	cid, ok := ctx.Value(models.ClientConnectionIDKey).(string)
	if !ok {
		return status.Errorf(codes.Internal, "missing ClientConnectionID in context")
	}
	did, ok := ctx.Value(models.DestConnectionIDKey).(string)
	if !ok {
		return status.Errorf(codes.Internal, "missing DestinationConnectionID in context")
	}
	logger := g.logger.With(
		zap.String("Client ConnectionID", cid),
		zap.String("Destination ConnectionID", did),
		zap.String("Client IP Address", src.RemoteAddr().String()))
	// Peek the preface (needed for type detection) **but replay it** for the gRPC server.
	preface, err := util.ReadInitialBuf(ctx, logger, src)
	if err != nil {
		utils.LogError(logger, err, "failed to read the initial grpc message")
		return err
	}

	return recordOutgoing(ctx, logger,
		newReplayConn(preface, src), // <- give server the full preface
		dst, mocks)
}

func (g *Grpc) MockOutgoing(ctx context.Context, src net.Conn, _ *models.ConditionalDstCfg, mockDb integrations.MockMemDb, _ models.OutgoingOptions) error {
	cid, ok := ctx.Value(models.ClientConnectionIDKey).(string)
	if !ok {
		return status.Errorf(codes.Internal, "missing ClientConnectionID in context")
	}
	logger := g.logger.With(
		zap.String("Client ConnectionID", cid),
		zap.String("Client IP Address", src.RemoteAddr().String()))
	// Consume the initial preface buffer from the connection.
	preface, err := util.ReadInitialBuf(ctx, logger, src)
	if err != nil {
		utils.LogError(logger, err, "failed to read the initial grpc message")
		return err
	}

	return mockOutgoing(ctx, logger,
		newReplayConn(preface, src), // <- same in mock path
		mockDb)
}
