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

	"github.com/cloudflare/cfssl/helpers"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

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
// Returns the upgraded (or original) client and server connections.
func HandlePostgresSSL(
	ctx context.Context,
	logger *zap.Logger,
	clientConn net.Conn,
	serverConn net.Conn,
	sslRequestBuf []byte,
	sourcePort int,
	backdate time.Time,
) (upgradedClient net.Conn, upgradedServer net.Conn, err error) {
	// Default to original connections
	upgradedClient = clientConn
	upgradedServer = serverConn

	// Forward SSLRequest to server
	_, err = serverConn.Write(sslRequestBuf)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to forward SSLRequest to PostgreSQL server: %w", err)
	}

	// Read server's 1-byte response ('S' or 'N')
	responseBuf := make([]byte, 1)
	_, err = io.ReadFull(serverConn, responseBuf)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read PostgreSQL SSL response: %w", err)
	}

	// Forward response to client
	_, err = clientConn.Write(responseBuf)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to forward SSL response to client: %w", err)
	}

	// If server declined SSL, return original connections
	if responseBuf[0] != 'S' {
		logger.Debug("PostgreSQL server declined SSL, continuing with plain connection")
		return upgradedClient, upgradedServer, nil
	}

	logger.Debug("PostgreSQL server accepted SSL, upgrading connections")

	// Upgrade client connection (act as TLS server to the app)
	upgradedClient, err = HandleTLSConnection(ctx, logger, clientConn, backdate)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to upgrade client connection to TLS: %w", err)
	}

	// Get destination URL from source port mapping
	urlValue, ok := SrcPortToDstURL.Load(sourcePort)
	if !ok {
		return nil, nil, fmt.Errorf("failed to find destination URL for source port %d", sourcePort)
	}
	dstURL, ok := urlValue.(string)
	if !ok {
		return nil, nil, fmt.Errorf("destination URL for port %d is not a string", sourcePort)
	}

	// Upgrade server connection (act as TLS client to the real server)
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         dstURL,
	}
	tlsServerConn := tls.Client(serverConn, tlsConfig)
	if err := tlsServerConn.Handshake(); err != nil {
		return nil, nil, fmt.Errorf("failed to upgrade server connection to TLS: %w", err)
	}
	upgradedServer = tlsServerConn

	logger.Debug("PostgreSQL SSL upgrade complete",
		zap.String("serverName", dstURL),
		zap.Int("sourcePort", sourcePort))

	return upgradedClient, upgradedServer, nil
}

// IsMySQLSSLRequest checks if the MySQL handshake response indicates SSL capability.
// MySQL SSL detection is more complex - it happens during the handshake phase.
// The client sends an SSLRequest packet (similar to handshake response but without authentication).
// This is detected by checking:
// - Packet has Client SSL capability flag set (0x0800)
// - Packet length is 32 bytes (SSLRequest) instead of full handshake response
func IsMySQLSSLRequest(buf []byte, serverCapabilities uint32) bool {
	if len(buf) < 4 {
		return false
	}

	// MySQL packet: 3-byte length + 1-byte sequence number + payload
	payloadLen := int(buf[0]) | int(buf[1])<<8 | int(buf[2])<<16
	if payloadLen < 4 {
		return false
	}

	// Skip header (4 bytes) to get to payload
	if len(buf) < 4+4 {
		return false
	}

	// First 4 bytes of payload are client capabilities
	clientCaps := binary.LittleEndian.Uint32(buf[4:8])

	// Check if both server supports SSL and client wants SSL
	const clientSSLFlag = 0x0800
	serverSupportsSSL := (serverCapabilities & clientSSLFlag) != 0
	clientWantsSSL := (clientCaps & clientSSLFlag) != 0

	// SSLRequest is 32 bytes payload (no username/auth yet)
	isSSLRequest := payloadLen == 32

	return serverSupportsSSL && clientWantsSSL && isSSLRequest
}

// HandleMySQLSSL handles MySQL in-protocol SSL negotiation at the proxy level.
// Unlike PostgreSQL, MySQL SSL happens as part of the handshake:
// 1. Server sends initial handshake (with SSL capability flag)
// 2. Client sends SSLRequest packet (if it wants SSL)
// 3. Both sides upgrade to TLS
// 4. Client re-sends full handshake response over TLS
//
// The caller should detect the SSLRequest and call this function.
func HandleMySQLSSL(
	ctx context.Context,
	logger *zap.Logger,
	clientConn net.Conn,
	serverConn net.Conn,
	sslRequestBuf []byte,
	sourcePort int,
	serverAddr string,
	backdate time.Time,
) (upgradedClient net.Conn, upgradedServer net.Conn, err error) {
	// Forward SSLRequest to server
	_, err = serverConn.Write(sslRequestBuf)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to forward MySQL SSLRequest to server: %w", err)
	}

	logger.Debug("MySQL SSLRequest forwarded, upgrading connections")

	// Upgrade client connection (act as TLS server to the app)
	upgradedClient, err = HandleTLSConnection(ctx, logger, clientConn, backdate)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to upgrade MySQL client connection to TLS: %w", err)
	}

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

	// Upgrade server connection (act as TLS client to the real server)
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         serverName,
	}
	tlsServerConn := tls.Client(serverConn, tlsConfig)
	if err := tlsServerConn.Handshake(); err != nil {
		return nil, nil, fmt.Errorf("failed to upgrade MySQL server connection to TLS: %w", err)
	}
	upgradedServer = tlsServerConn

	logger.Debug("MySQL SSL upgrade complete",
		zap.String("serverName", serverName),
		zap.Int("sourcePort", sourcePort))

	return upgradedClient, upgradedServer, nil
}

// GetCACredentials returns the parsed CA private key and certificate.
// This is useful for parsers that need to generate certificates but want
// the proxy to manage the CA loading.
func GetCACredentials(logger *zap.Logger) (caPrivKey interface{}, caCert interface{}, err error) {
	caPrivKey, err = helpers.ParsePrivateKeyPEM(caPKey)
	if err != nil {
		utils.LogError(logger, err, "Failed to parse CA private key")
		return nil, nil, err
	}

	caCert, err = helpers.ParseCertificatePEM(caCrt)
	if err != nil {
		utils.LogError(logger, err, "Failed to parse CA certificate")
		return nil, nil, err
	}

	return caPrivKey, caCert, nil
}

// UpgradeMySQLConnections upgrades both client and server connections to TLS for MySQL.
// This is used when the MySQL parser detects that SSL negotiation is needed.
// Unlike HandleMySQLSSL, this function assumes the SSLRequest has already been
// forwarded to the server and the connections are ready for TLS upgrade.
//
// Parameters:
// - clientConn: the connection to the client (app -> proxy), will be upgraded to TLS server
// - serverConn: the connection to the server (proxy -> MySQL), will be upgraded to TLS client
// - sourcePort: the source port for looking up destination URL
// - serverAddr: fallback server address for TLS config
// - backdate: timestamp for certificate generation
//
// Returns upgraded client and server connections.
func UpgradeMySQLConnections(
	ctx context.Context,
	logger *zap.Logger,
	clientConn net.Conn,
	serverConn net.Conn,
	sourcePort int,
	serverAddr string,
	backdate time.Time,
) (upgradedClient net.Conn, upgradedServer net.Conn, err error) {
	logger.Debug("Upgrading MySQL connections to TLS at proxy level",
		zap.Int("sourcePort", sourcePort),
		zap.String("serverAddr", serverAddr))

	// Upgrade client connection (act as TLS server to the app)
	// Note: clientConn may already have a custom Reader from buffered reads,
	// HandleTLSConnection preserves this
	upgradedClient, err = HandleTLSConnection(ctx, logger, clientConn, backdate)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to upgrade MySQL client connection to TLS: %w", err)
	}

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

	// Upgrade server connection (act as TLS client to the real server)
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         serverName,
	}
	tlsServerConn := tls.Client(serverConn, tlsConfig)
	if err := tlsServerConn.Handshake(); err != nil {
		return nil, nil, fmt.Errorf("failed to upgrade MySQL server connection to TLS: %w", err)
	}
	upgradedServer = tlsServerConn

	logger.Debug("MySQL TLS upgrade complete",
		zap.String("serverName", serverName),
		zap.Int("sourcePort", sourcePort))

	return upgradedClient, upgradedServer, nil
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
	}
	tlsServerConn := tls.Client(serverConn, tlsConfig)
	if err := tlsServerConn.Handshake(); err != nil {
		return nil, fmt.Errorf("failed to upgrade MySQL server connection to TLS: %w", err)
	}

	logger.Debug("MySQL server TLS upgrade complete",
		zap.String("serverName", serverName))

	return tlsServerConn, nil
}

