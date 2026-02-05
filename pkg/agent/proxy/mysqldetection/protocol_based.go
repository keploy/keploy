package mysqldetection

import (
	"context"
	"net"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// ProtocolBasedDetection implements MySQL detection based on packet protocol analysis
type ProtocolBasedDetection struct {
	logger *zap.Logger
}

// NewProtocolBasedDetection creates a new protocol-based detection strategy
func NewProtocolBasedDetection(logger *zap.Logger) *ProtocolBasedDetection {
	return &ProtocolBasedDetection{
		logger: logger,
	}
}

// ShouldHandle returns false for protocol-based detection
// This allows the connection to fall through to the normal MatchType flow
// where the MySQL integration's MatchType will detect MySQL packets by protocol
func (p *ProtocolBasedDetection) ShouldHandle(_ context.Context, _ *agent.NetworkAddress, _ []byte) bool {
	return false
}

// HandleConnection is a no-op for protocol-based detection
// The actual handling happens through the normal integration MatchType flow
func (p *ProtocolBasedDetection) HandleConnection(
	_ context.Context,
	_ context.Context,
	_ net.Conn,
	_ string,
	_ *agent.NetworkAddress,
	_ *agent.Session,
	_ models.OutgoingOptions,
	_ integrations.Integrations,
	_ interface{},
	_ *zap.Logger,
	_ func(error),
) error {
	// Protocol-based detection doesn't need special handling here
	// It relies on MatchType in the MySQL integration
	return nil
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
