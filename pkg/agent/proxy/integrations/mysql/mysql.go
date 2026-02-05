// Package mysql provides the MySQL integration.
package mysql

import (
	"context"
	"io"
	"net"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/recorder"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/models/mysql"

	"go.keploy.io/server/v3/utils"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func init() {
	integrations.Register(integrations.MYSQL, &integrations.Parsers{
		Initializer: New,
		Priority:    100,
	})
}

type MySQL struct {
	logger *zap.Logger
}

func New(logger *zap.Logger) integrations.Integrations {
	return &MySQL{
		logger: logger,
	}
}

// MatchType detects MySQL protocol by examining packet structure.
// This is used when protocol-based detection strategy is enabled.
func (m *MySQL) MatchType(ctx context.Context, buf []byte) bool {
	return isMySQLPacket(buf)
}

// isMySQLPacket detects MySQL protocol by examining packet structure
func isMySQLPacket(buf []byte) bool {
	// Need at least 4 bytes for MySQL packet header
	if len(buf) < 4 {
		return false
	}

	// Extract payload length from first 3 bytes (little-endian)
	payloadLength := uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16

	// MySQL packet payload length should be reasonable (max 16MB-1)
	if payloadLength > 0xffffff {
		return false
	}

	// Check if we have enough bytes for the payload
	if len(buf) < 4+int(payloadLength) {
		// Partial packet, but structure looks valid - check first payload byte
		if len(buf) >= 5 {
			firstPayloadByte := buf[4]
			return isMySQLPacketByte(firstPayloadByte)
		}
		return false
	}

	// Full packet available - check first payload byte
	if len(buf) >= 5 {
		firstPayloadByte := buf[4]
		return isMySQLPacketByte(firstPayloadByte)
	}

	return false
}

// isMySQLPacketByte checks if the byte is a valid MySQL packet identifier
func isMySQLPacketByte(b byte) bool {
	// Server handshake packet
	if b == mysql.HandshakeV10 { // 0x0a
		return true
	}

	// Client command packets (common ones)
	validCommands := []byte{
		0x01, // COM_QUIT
		0x02, // COM_INIT_DB
		0x03, // COM_QUERY
		0x0e, // COM_PING
		0x16, // COM_STMT_PREPARE
		0x17, // COM_STMT_EXECUTE
		0x19, // COM_STMT_CLOSE
		0x1a, // COM_STMT_RESET
		0x1c, // COM_STMT_SEND_LONG_DATA
	}
	for _, cmd := range validCommands {
		if b == cmd {
			return true
		}
	}

	// Response packets: OK (0x00), ERR (0xff), EOF (0xfe)
	if b == mysql.OK || b == mysql.ERR || b == mysql.EOF {
		return true
	}

	return false
}

func (m *MySQL) RecordOutgoing(ctx context.Context, src net.Conn, dst net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	logger := m.logger.With(zap.Any("Client ConnectionID", ctx.Value(models.ClientConnectionIDKey).(string)), zap.Any("Destination ConnectionID", ctx.Value(models.DestConnectionIDKey).(string)), zap.Any("Client IP Address", src.RemoteAddr().String()))

	err := recorder.Record(ctx, logger, src, dst, mocks, opts)
	if err != nil {
		utils.LogError(logger, err, "failed to encode the mysql message into the yaml")
		return err
	}
	return nil
}

func (m *MySQL) MockOutgoing(ctx context.Context, src net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	logger := m.logger.With(zap.Any("Client ConnectionID", ctx.Value(models.ClientConnectionIDKey).(string)), zap.Any("Destination ConnectionID", ctx.Value(models.DestConnectionIDKey).(string)), zap.Any("Client IP Address", src.RemoteAddr().String()))
	err := replayer.Replay(ctx, logger, src, dstCfg, mockDb, opts)
	if err != nil && err != io.EOF {
		utils.LogError(logger, err, "failed to decode the mysql message from the yaml")
		return err
	}
	return nil
}
