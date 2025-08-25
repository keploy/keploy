//go:build linux

package v1

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/utils"

	"go.keploy.io/server/v2/pkg/models"

	"go.uber.org/zap"
)

func init() {
	integrations.Register(integrations.POSTGRES_V1, &integrations.Parsers{
		Initializer: New,
		Priority:    100,
	})
}

type PostgresV1 struct {
	logger *zap.Logger
}

func New(logger *zap.Logger) integrations.Integrations {
	return &PostgresV1{
		logger: logger,
	}
}

// MatchType determines if the outgoing network call is Postgres by comparing the
// message format with that of a Postgres text message.
func (p *PostgresV1) MatchType(_ context.Context, reqBuf []byte) bool {
	const ProtocolVersion = 0x00030000 // Protocol version 3.0

	if len(reqBuf) < 8 {
		// Not enough data for a complete header
		p.logger.Debug("Postgres match failed: insufficient data", zap.Int("buffer_length", len(reqBuf)))
		return false
	}

	// Check for SSL/TLS packet first
	if isSSLPacket(reqBuf) {
		p.logger.Debug("Detected SSL/TLS packet, not a plain postgres packet")
		return false
	}

	// The first four bytes are the message length, but we don't need to check those
	// The next four bytes are the protocol version
	version := binary.BigEndian.Uint32(reqBuf[4:8])

	// Check for SSL request (special case - we can handle this)
	if version == 80877103 {
		p.logger.Debug("Detected postgres SSL request")
		return true
	}

	// Check for standard protocol version
	if version == ProtocolVersion {
		p.logger.Debug("Detected postgres protocol version 3.0")
		return true
	}

	// Check for other valid postgres message codes
	if version == 80877102 { // Cancel request
		p.logger.Debug("Detected postgres cancel request")
		return true
	}

	if version == 80877104 { // GSS encryption request
		p.logger.Debug("Detected postgres GSS encryption request")
		return true
	}

	p.logger.Debug("Postgres match failed: unknown version",
		zap.Uint32("version", version),
		zap.String("version_hex", fmt.Sprintf("0x%08X", version)))
	return false
}

func (p *PostgresV1) RecordOutgoing(ctx context.Context, src net.Conn, dst net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	logger := p.logger.With(zap.Any("Client ConnectionID", ctx.Value(models.ClientConnectionIDKey).(string)), zap.Any("Destination ConnectionID", ctx.Value(models.DestConnectionIDKey).(string)), zap.Any("Client IP Address", src.RemoteAddr().String()))

	reqBuf, err := util.ReadInitialBuf(ctx, logger, src)
	if err != nil {
		utils.LogError(logger, err, "failed to read the initial postgres message")
		return err
	}

	// Validate the request buffer before processing
	if valid, validationErr := isValidPostgresPacket(reqBuf); !valid {
		logger.Warn("Invalid postgres packet in RecordOutgoing, attempting passthrough",
			zap.Error(validationErr),
			zap.Int("buffer_length", len(reqBuf)))

		// For recording mode, we still try to pass through but log the issue
		if strings.Contains(validationErr.Error(), "SSL") || strings.Contains(validationErr.Error(), "encrypted") {
			logger.Info("SSL/TLS connection detected in recording mode")
		}
	}

	err = encodePostgres(ctx, logger, reqBuf, src, dst, mocks, opts)
	if err != nil {
		logger.Error("failed to encode the postgres message into the yaml", zap.Error(err))
		return err
	}
	return nil
}

func (p *PostgresV1) MockOutgoing(ctx context.Context, src net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	logger := p.logger.With(zap.Any("Client ConnectionID", ctx.Value(models.ClientConnectionIDKey).(string)), zap.Any("Destination ConnectionID", ctx.Value(models.DestConnectionIDKey).(string)), zap.Any("Client IP Address", src.RemoteAddr().String()))
	reqBuf, err := util.ReadInitialBuf(ctx, logger, src)
	if err != nil {
		utils.LogError(logger, err, "failed to read the initial postgres message")
		return err
	}

	err = decodePostgres(ctx, logger, reqBuf, src, dstCfg, mockDb, opts)
	if err != nil {
		logger.Error("failed to decode the postgres message from the yaml", zap.Error(err))
		return err
	}
	return nil
}
