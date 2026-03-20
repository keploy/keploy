// Package tls provides in-protocol TLS handling for database protocols.
// This file handles SSL upgrade for protocols like PostgreSQL and MySQL
// where TLS negotiation happens after the initial protocol handshake.
package tls

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	postgres "go.keploy.io/server/v3/pkg/models/postgres"
	"go.uber.org/zap"
)

// NewPostgresSSLConfigMock creates a synthetic SSLRequest/SSLResponse config mock
// for backward compatibility. The main branch replayer expects this config mock
// to exist when it encounters SSLRequest during replay. Without it, mocks
// recorded on branches with proxy-level SSL handling break when replayed on
// main/latest.
func NewPostgresSSLConfigMock(connID string, sslResponse byte) *models.Mock {
	if sslResponse != 'S' && sslResponse != 'N' {
		sslResponse = 'N'
	}

	reqTimestamp := time.Now()
	sslReqPacket := postgres.Packet{
		Header: &postgres.PacketInfo{
			Header: &postgres.Header{
				PayloadLength: 8,
			},
			Type: "SSLRequest",
		},
		Message: map[string]interface{}{"code": SSLRequestCode},
	}
	sslRespPacket := postgres.Packet{
		Header: &postgres.PacketInfo{
			Header: &postgres.Header{
				PayloadLength: 1,
			},
			Type: "SSLResponse",
		},
		Message: map[string]interface{}{"response": string(sslResponse)},
	}
	return &models.Mock{
		Version: models.GetVersion(),
		Name:    "config",
		Kind:    models.PostgresV2,
		Spec: models.MockSpec{
			PostgresRequestsV2:  []postgres.Request{{PacketBundle: postgres.PacketBundle{Packets: []postgres.Packet{sslReqPacket}}}},
			PostgresResponsesV2: []postgres.Response{{PacketBundle: postgres.PacketBundle{Packets: []postgres.Packet{sslRespPacket}}}},
			ReqTimestampMock:    reqTimestamp,
			ResTimestampMock:    time.Now(),
			Metadata: map[string]string{
				"type":              "config",
				"requestOperation":  "SSLRequest",
				"responseOperation": "SSLResponse",
				"connID":            connID,
			},
		},
		ConnectionID: connID,
	}
}

// PostgreSQL protocol constants
const (
	// SSLRequestCode is the protocol code for PostgreSQL SSLRequest message
	// Format: 4 bytes length (8) + 4 bytes code (80877103)
	SSLRequestCode = 80877103 // 0x04D2162F
)

// IsPostgresSSLRequest checks if the buffer is a PostgreSQL SSLRequest packet.
// SSLRequest packet format:
// - 4 bytes: message length (always 8)
// - 4 bytes: SSL request code (80877103)
func IsPostgresSSLRequest(buf []byte) bool {
	if len(buf) < 8 {
		return false
	}
	// Check length is 8 and request code matches
	length := binary.BigEndian.Uint32(buf[0:4])
	code := binary.BigEndian.Uint32(buf[4:8])
	return length == 8 && code == SSLRequestCode
}

// HandlePostgresSSL handles PostgreSQL in-protocol SSL negotiation at the proxy level.
// This performs the following steps:
// 1. Forward SSLRequest to the real PostgreSQL server
// 2. Read the server's response ('S' for SSL, 'N' for no SSL)
// 3. Forward the response to the client
// 4. If 'S', upgrade both connections to TLS
//
// Returns the upgraded (or original) client and server connections and the
// server SSL response byte ('S' or 'N').
func HandlePostgresSSL(
	ctx context.Context,
	logger *zap.Logger,
	clientConn net.Conn,
	serverConn net.Conn,
	sslRequestBuf []byte,
	sourcePort int,
	backdate time.Time,
) (upgradedClient net.Conn, upgradedServer net.Conn, sslResponse byte, err error) {
	// Default to original connections
	upgradedClient = clientConn
	upgradedServer = serverConn

	// Forward SSLRequest to server
	_, err = serverConn.Write(sslRequestBuf)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("failed to forward SSLRequest to PostgreSQL server: %w", err)
	}

	// Read server's 1-byte response ('S' or 'N')
	responseBuf := make([]byte, 1)
	_, err = io.ReadFull(serverConn, responseBuf)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("failed to read PostgreSQL SSL response: %w", err)
	}
	sslResponse = responseBuf[0]

	// Forward response to client
	_, err = clientConn.Write(responseBuf)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("failed to forward SSL response to client: %w", err)
	}

	// If server declined SSL, return original connections
	if responseBuf[0] != 'S' {
		logger.Debug("PostgreSQL server declined SSL, continuing with plain connection")
		return upgradedClient, upgradedServer, sslResponse, nil
	}

	logger.Debug("PostgreSQL server accepted SSL, upgrading connections")

	// Upgrade client connection (act as TLS server to the app)
	upgradedClient, _, err = HandleTLSConnection(ctx, logger, clientConn, backdate)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("failed to upgrade client connection to TLS: %w", err)
	}

	// Get destination URL from source port mapping
	urlValue, ok := SrcPortToDstURL.Load(sourcePort)
	if !ok {
		return nil, nil, 0, fmt.Errorf("failed to find destination URL for source port %d", sourcePort)
	}
	dstURL, ok := urlValue.(string)
	if !ok {
		return nil, nil, 0, fmt.Errorf("destination URL for port %d is not a string", sourcePort)
	}

	// Upgrade server connection (act as TLS client to the real server)
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         dstURL,
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS13,
		CipherSuites:       getCompatibleCipherSuites(),
	}
	tlsServerConn := tls.Client(serverConn, tlsConfig)
	if err := tlsServerConn.Handshake(); err != nil {
		return nil, nil, 0, fmt.Errorf("failed to upgrade server connection to TLS: %w", err)
	}
	upgradedServer = tlsServerConn

	logger.Debug("PostgreSQL SSL upgrade complete",
		zap.String("serverName", dstURL),
		zap.Int("sourcePort", sourcePort))

	return upgradedClient, upgradedServer, sslResponse, nil
}

// UpgradeMySQLServerToTLS upgrades only the server connection to TLS for MySQL.
// This is used when the client connection has already been upgraded separately
// via HandleTLSConnection.
//
// Parameters:
// - serverConn: the connection to the server (proxy -> MySQL), will be upgraded to TLS client
// - sourcePort: the source port for looking up destination URL
// - serverAddr: fallback server address for TLS config
//
// Returns the upgraded server connection.
func UpgradeMySQLServerToTLS(
	_ context.Context,
	logger *zap.Logger,
	serverConn net.Conn,
	sourcePort int,
	serverAddr string,
) (upgradedServer net.Conn, err error) {
	// Get destination URL from source port mapping (if available)
	var serverName string
	if urlValue, ok := SrcPortToDstURL.Load(sourcePort); ok {
		if s, ok := urlValue.(string); ok {
			serverName = s
		}
	}
	if serverName == "" {
		// Fall back to extracting from server address
		host, _, _ := net.SplitHostPort(serverAddr)
		serverName = host
	}

	logger.Debug("Upgrading MySQL server connection to TLS",
		zap.String("serverName", serverName),
		zap.Int("sourcePort", sourcePort))

	// Upgrade server connection (act as TLS client to the real server)
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         serverName,
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS13,
		CipherSuites:       getCompatibleCipherSuites(),
	}
	tlsServerConn := tls.Client(serverConn, tlsConfig)
	if err := tlsServerConn.Handshake(); err != nil {
		return nil, fmt.Errorf("failed to upgrade MySQL server connection to TLS: %w", err)
	}

	logger.Debug("MySQL server TLS upgrade complete",
		zap.String("serverName", serverName))

	return tlsServerConn, nil
}

// getCompatibleCipherSuites returns secure cipher suites plus specific legacy ones
// needed for MySQL compatibility (e.g. RSA-CBC), avoiding the blanket InsecureCipherSuites().
func getCompatibleCipherSuites() []uint16 {
	var ids []uint16
	for _, cs := range tls.CipherSuites() {
		ids = append(ids, cs.ID)
	}
	// Explicitly add CBC ciphers for MySQL 5.7 / Legacy device support
	// These are technically "insecure" but often required for older database versions.
	ids = append(ids,
		tls.TLS_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_RSA_WITH_AES_256_CBC_SHA,
	)
	return ids
}
