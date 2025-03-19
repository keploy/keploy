//go:build linux

package redis

import (
	"context"
	"net"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	integrations.Register(integrations.REDIS, &integrations.Parsers{NewRedis, 100})
}

type Redis struct {
	logger *zap.Logger
}

func NewRedis(logger *zap.Logger) integrations.Integrations {
	return &Redis{
		logger: logger,
	}
}

func (r *Redis) MatchType(_ context.Context, buf []byte) bool {
	if len(buf) == 0 {
		return false
	}

	// Check the first byte to determine the RESP data type
	switch buf[0] {
	case '+', '-', ':', '$', '*', '_', '#', ',', '(', '!', '=', '%', '~', '>':
		return true
	default:
		return false
	}
}

func (r *Redis) RecordOutgoing(ctx context.Context, src net.Conn, dst net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	logger := r.logger.With(zap.Any("Client IP Address", src.RemoteAddr().String()), zap.Any("Client ConnectionID", ctx.Value(models.ClientConnectionIDKey).(string)), zap.Any("Destination ConnectionID", ctx.Value(models.DestConnectionIDKey).(string)))

	reqBuf, err := util.ReadInitialBuf(ctx, logger, src)
	if err != nil {
		utils.LogError(logger, err, "failed to read the initial redis message")
		return err
	}

	err = encodeRedis(ctx, logger, reqBuf, src, dst, mocks, opts)
	if err != nil {
		utils.LogError(logger, err, "failed to encode the redis message into the yaml")
		return err
	}
	return nil
}

func (r *Redis) MockOutgoing(ctx context.Context, src net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	logger := r.logger.With(zap.Any("Client IP Address", src.RemoteAddr().String()), zap.Any("Client ConnectionID", ctx.Value(models.ClientConnectionIDKey).(string)), zap.Any("Destination ConnectionID", ctx.Value(models.DestConnectionIDKey).(string)))
	reqBuf, err := util.ReadInitialBuf(ctx, logger, src)
	if err != nil {
		utils.LogError(logger, err, "failed to read the initial redis message")
		return err
	}

	err = decodeRedis(ctx, logger, reqBuf, src, dstCfg, mockDb, opts)
	if err != nil {
		utils.LogError(logger, err, "failed to decode the redis message")
		return err
	}
	return nil
}
