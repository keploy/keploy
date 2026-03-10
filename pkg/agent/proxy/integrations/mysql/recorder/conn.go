package recorder

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"time"

	mysqlUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/agent/proxy/orchestrator"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	pUtils "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// handshakeResult holds everything produced by the synchronous handshake
// phase that the async pipeline needs.
type handshakeResult struct {
	// Mocks captured during the handshake (one RawMockEntry for the whole exchange).
	Mocks []RawMockEntry

	// State extracted for the reassembler.
	State handshakeState

	// Connections to use for the command phase (may be TLS-upgraded).
	ClientConn net.Conn
	DestConn   net.Conn

	// TLSOnly is set when TLS was detected but MITM was skipped (SkipTLSMITM mode).
	// When true, only handshake mocks were captured; the command phase is encrypted
	// and should not be parsed from this data source.
	TLSOnly bool
}

// handleHandshake performs the MySQL connection-phase handshake synchronously.
// It reads/writes packets between client and server directly (no TeeForwardConn).
// This is fine because the handshake happens once per connection and is amortised
// by connection pooling.
//
// It returns the extracted handshake state and the raw packets captured.
func handleHandshake(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, opts models.OutgoingOptions) (*handshakeResult, error) {
	result := &handshakeResult{
		ClientConn: clientConn,
		DestConn:   destConn,
	}

	var reqPackets [][]byte
	var respPackets [][]byte

	reqTimestamp := time.Now()

	// For decoding during the handshake we still use the slow-path decoder
	// since it needs map-based state tracking for the different connection
	// wrappers created during TLS upgrade. This only runs once per connection.
	decodeCtx := &wire.DecodeContext{
		Mode:               models.MODE_RECORD,
		LastOp:             wire.NewLastOpMap(),
		ServerGreetings:    wire.NewGreetings(),
		PreparedStatements: make(map[uint32]*mysql.StmtPrepareOkPacket),
		LastOpValue:        wire.RESET,
	}
	decodeCtx.LastOp.Store(clientConn, wire.RESET)

	orchestrator.SetTCPQuickACK(destConn)
	orchestrator.SetTCPQuickACK(clientConn)

	// ── 1. Server Greeting ───────────────────────────────────────────
	greeting, err := readPacketSync(destConn)
	if err != nil {
		return nil, fmt.Errorf("failed to read server greeting: %w", err)
	}
	respPackets = append(respPackets, greeting)

	// Forward to client.
	if _, err := clientConn.Write(greeting); err != nil {
		return nil, fmt.Errorf("failed to forward server greeting: %w", err)
	}
	orchestrator.SetTCPQuickACK(clientConn)

	// Decode greeting to extract server capabilities and plugin name.
	greetingPkt, err := wire.DecodePayload(ctx, logger, greeting, clientConn, decodeCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to decode server greeting: %w", err)
	}

	sg, ok := greetingPkt.Message.(*mysql.HandshakeV10Packet)
	if !ok {
		return nil, fmt.Errorf("expected HandshakeV10Packet, got %T", greetingPkt.Message)
	}

	result.State.ServerCaps = sg.CapabilityFlags
	result.State.PluginName = sg.AuthPluginName
	result.State.ServerGreeting = sg
	decodeCtx.PluginName = sg.AuthPluginName
	decodeCtx.ServerGreeting = sg

	// ── 2. Client Handshake Response (synchronous — need to detect SSL) ──
	orchestrator.SetTCPQuickACK(clientConn)
	hsResp, err := readPacketSync(clientConn)
	if err != nil {
		if err == io.EOF {
			return nil, err
		}
		return nil, fmt.Errorf("failed to read client handshake response: %w", err)
	}
	reqPackets = append(reqPackets, hsResp)

	// Forward to server.
	if _, err := destConn.Write(hsResp); err != nil {
		return nil, fmt.Errorf("failed to forward client handshake response: %w", err)
	}
	orchestrator.SetTCPQuickACK(destConn)

	// Decode to detect SSL and extract client capabilities.
	hsRespPkt, err := wire.DecodePayload(ctx, logger, hsResp, clientConn, decodeCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to decode client handshake response: %w", err)
	}

	switch pkt := hsRespPkt.Message.(type) {
	case *mysql.HandshakeResponse41Packet:
		result.State.ClientCaps = pkt.CapabilityFlags
		decodeCtx.ClientCapabilities = pkt.CapabilityFlags
	case *mysql.SSLRequestPacket:
		result.State.UseSSL = true
		result.State.ClientCaps = pkt.CapabilityFlags
		decodeCtx.ClientCapabilities = pkt.CapabilityFlags
	}

	// ── 3. Handle SSL upgrade if needed ──────────────────────────────
	if decodeCtx.UseSSL {
		reader := bufio.NewReader(clientConn)
		initialData := make([]byte, 5)
		testBuffer, err := reader.Peek(len(initialData))
		if err != nil {
			if err == io.EOF && len(testBuffer) == 0 {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("failed to peek for TLS handshake: %w", err)
		}

		multiReader := io.MultiReader(reader, clientConn)
		wrappedClient := &pUtils.Conn{
			Conn:   clientConn,
			Reader: multiReader,
			Logger: logger,
		}

		isTLS := pTls.IsTLSHandshake(testBuffer)
		if isTLS {
			// If SkipTLSMITM is set, do not perform TLS interception.
			// Record the plaintext handshake portion and return.
			// The encrypted command phase will be captured separately by
			// JSSE/SSL uprobes which provide decrypted plaintext.
			if opts.SkipTLSMITM {
				resTimestamp := time.Now()
				result.TLSOnly = true
				result.Mocks = []RawMockEntry{{
					ReqPackets:   reqPackets,
					RespPackets:  respPackets,
					CmdType:      mysql.HandshakeV10,
					MockType:     "config",
					ReqTimestamp: reqTimestamp,
					ResTimestamp: resTimestamp,
				}}
				return result, nil
			}

			tlsClient, _, err := pTls.HandleTLSConnection(ctx, logger, wrappedClient, opts.Backdate)
			if err != nil {
				return nil, fmt.Errorf("failed to handle TLS connection: %w", err)
			}

			remoteAddr := clientConn.RemoteAddr().(*net.TCPAddr)
			sourcePort := remoteAddr.Port
			serverAddr := destConn.RemoteAddr().String()

			upgradedDest, err := pTls.UpgradeMySQLServerToTLS(ctx, logger, destConn, sourcePort, serverAddr)
			if err != nil {
				return nil, fmt.Errorf("failed to upgrade MySQL server to TLS: %w", err)
			}

			clientConn = tlsClient
			destConn = upgradedDest
			result.ClientConn = clientConn
			result.DestConn = destConn
		} else {
			// Not a TLS handshake — likely Rust proxy already decrypted.
			// Use wrappedClient so peeked bytes aren't lost.
			clientConn = wrappedClient
			result.ClientConn = clientConn
		}

		// Reset state for the upgraded connection.
		decodeCtx.LastOp.Store(clientConn, mysql.HandshakeV10)
		decodeCtx.LastOpValue = mysql.HandshakeV10
		decodeCtx.ServerGreetings.Store(clientConn, sg)
		decodeCtx.ServerGreeting = sg

		// Read the actual handshake response over TLS.
		orchestrator.SetTCPQuickACK(clientConn)
		hsResp2, err := readPacketSync(clientConn)
		if err != nil {
			if err == io.EOF {
				return nil, err
			}
			return nil, fmt.Errorf("failed to read handshake response over TLS: %w", err)
		}
		reqPackets = append(reqPackets, hsResp2)

		// Forward to server.
		if _, err := destConn.Write(hsResp2); err != nil {
			return nil, fmt.Errorf("failed to forward handshake response over TLS: %w", err)
		}
		orchestrator.SetTCPQuickACK(destConn)

		// Decode the TLS-wrapped handshake response.
		_, err = wire.DecodePayload(ctx, logger, hsResp2, clientConn, decodeCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to decode TLS handshake response: %w", err)
		}
	} else {
		// Non-SSL: store server greeting for the current client conn.
		decodeCtx.ServerGreetings.Store(clientConn, sg)
		decodeCtx.ServerGreeting = sg
	}

	// Update CLIENT_DEPRECATE_EOF.
	result.State.DeprecateEOF = (result.State.ServerCaps&mysql.CLIENT_DEPRECATE_EOF) != 0 &&
		(result.State.ClientCaps&mysql.CLIENT_DEPRECATE_EOF) != 0

	// ── 4. Auth exchange ─────────────────────────────────────────────
	authReq, authResp, err := handleAuthExchange(ctx, logger, clientConn, destConn, decodeCtx)
	if err != nil {
		return nil, fmt.Errorf("auth exchange failed: %w", err)
	}
	reqPackets = append(reqPackets, authReq...)
	respPackets = append(respPackets, authResp...)

	resTimestamp := time.Now()

	// Build the handshake mock entry.
	result.Mocks = []RawMockEntry{{
		ReqPackets:   reqPackets,
		RespPackets:  respPackets,
		CmdType:      mysql.HandshakeV10,
		MockType:     "config",
		ReqTimestamp: reqTimestamp,
		ResTimestamp: resTimestamp,
	}}

	return result, nil
}

// handleAuthExchange handles the authentication phase after the initial
// handshake response has been sent.  It supports:
//   - Native password (OK immediately)
//   - AuthSwitchRequest
//   - caching_sha2_password (fast auth + full auth)
//
// Returns the raw request and response packets captured.
func handleAuthExchange(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, decodeCtx *wire.DecodeContext) (reqPackets, respPackets [][]byte, err error) {
	orchestrator.SetTCPQuickACK(destConn)

	// Read the first auth-phase packet from the server.
	authData, err := readPacketSync(destConn)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read auth packet: %w", err)
	}
	respPackets = append(respPackets, authData)

	// Forward to client.
	if _, err := clientConn.Write(authData); err != nil {
		return nil, nil, fmt.Errorf("failed to forward auth packet: %w", err)
	}
	orchestrator.SetTCPQuickACK(clientConn)

	// Decode to determine auth type.
	authPkt, err := wire.DecodePayload(ctx, logger, authData, clientConn, decodeCtx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to decode auth packet: %w", err)
	}

	// Handle AuthSwitchRequest if present.
	if switchPkt, ok := authPkt.Message.(*mysql.AuthSwitchRequestPacket); ok {
		logger.Debug("server sent AuthSwitchRequest", zap.String("plugin", switchPkt.PluginName))
		decodeCtx.PluginName = switchPkt.PluginName

		// Read switch response from client.
		orchestrator.SetTCPQuickACK(clientConn)
		switchResp, err := readPacketSync(clientConn)
		if err != nil {
			return reqPackets, respPackets, fmt.Errorf("failed to read auth switch response: %w", err)
		}
		reqPackets = append(reqPackets, switchResp)

		// Forward to server.
		if _, err := destConn.Write(switchResp); err != nil {
			return reqPackets, respPackets, fmt.Errorf("failed to forward auth switch response: %w", err)
		}
		orchestrator.SetTCPQuickACK(destConn)

		// Read next auth packet from server.
		authData, err = readPacketSync(destConn)
		if err != nil {
			return reqPackets, respPackets, fmt.Errorf("failed to read post-switch auth packet: %w", err)
		}
		respPackets = append(respPackets, authData)

		// Forward to client.
		if _, err := clientConn.Write(authData); err != nil {
			return reqPackets, respPackets, fmt.Errorf("failed to forward post-switch auth packet: %w", err)
		}
		orchestrator.SetTCPQuickACK(clientConn)

		// Re-decode.
		authPkt, err = wire.DecodePayload(ctx, logger, authData, clientConn, decodeCtx)
		if err != nil {
			return reqPackets, respPackets, fmt.Errorf("failed to decode post-switch auth packet: %w", err)
		}
	}

	// Dispatch based on auth type.
	switch authPkt.Message.(type) {
	case *mysql.AuthMoreDataPacket:
		rq, rp, err := handleCachingSha2Auth(ctx, logger, clientConn, destConn, authPkt, decodeCtx)
		reqPackets = append(reqPackets, rq...)
		respPackets = append(respPackets, rp...)
		return reqPackets, respPackets, err
	case *mysql.OKPacket:
		// Native password — auth complete.
		return reqPackets, respPackets, nil
	default:
		return reqPackets, respPackets, nil
	}
}

// handleCachingSha2Auth handles caching_sha2_password auth (both fast and full).
func handleCachingSha2Auth(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, authPkt *mysql.PacketBundle, decodeCtx *wire.DecodeContext) (reqPackets, respPackets [][]byte, err error) {
	authMore, ok := authPkt.Message.(*mysql.AuthMoreDataPacket)
	if !ok {
		return nil, nil, fmt.Errorf("expected AuthMoreDataPacket, got %T", authPkt.Message)
	}

	// authMore.Data is already a string from the decoder.
	mechanism, mErr := wire.GetCachingSha2PasswordMechanism(authMore.Data[0])
	if mErr != nil {
		return nil, nil, fmt.Errorf("failed to get caching_sha2 mechanism: %w", mErr)
	}

	auth, mErr := wire.StringToCachingSha2PasswordMechanism(mechanism)
	if mErr != nil {
		return nil, nil, fmt.Errorf("failed to convert caching_sha2 mechanism: %w", mErr)
	}

	switch auth {
	case mysql.FastAuthSuccess:
		return handleFastAuthSync(clientConn, destConn)
	case mysql.PerformFullAuthentication:
		return handleFullAuthSync(ctx, logger, clientConn, destConn, decodeCtx)
	default:
		return nil, nil, fmt.Errorf("unsupported caching_sha2 mechanism: %s", mechanism)
	}
}

// handleFastAuthSync handles the fast auth success path: just read the final OK.
func handleFastAuthSync(clientConn, destConn net.Conn) (reqPackets, respPackets [][]byte, err error) {
	orchestrator.SetTCPQuickACK(destConn)
	finalResp, err := readPacketSync(destConn)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read final OK after fast auth: %w", err)
	}
	respPackets = append(respPackets, finalResp)

	// Forward to client.
	if _, err := clientConn.Write(finalResp); err != nil {
		return nil, respPackets, fmt.Errorf("failed to forward final OK: %w", err)
	}
	return nil, respPackets, nil
}

// handleFullAuthSync handles full caching_sha2 authentication.
func handleFullAuthSync(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, decodeCtx *wire.DecodeContext) (reqPackets, respPackets [][]byte, err error) {
	if decodeCtx.UseSSL {
		// SSL: plain password exchange.
		return handlePlainPasswordSync(clientConn, destConn)
	}

	// Non-SSL: public key exchange + encrypted password.
	// 1. Read public key request from client.
	orchestrator.SetTCPQuickACK(clientConn)
	pubKeyReq, err := readPacketSync(clientConn)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read public key request: %w", err)
	}
	reqPackets = append(reqPackets, pubKeyReq)

	// Forward to server.
	if _, err := destConn.Write(pubKeyReq); err != nil {
		return reqPackets, nil, fmt.Errorf("failed to forward public key request: %w", err)
	}
	orchestrator.SetTCPQuickACK(destConn)

	// 2. Read public key from server.
	pubKey, err := readPacketSync(destConn)
	if err != nil {
		return reqPackets, nil, fmt.Errorf("failed to read public key: %w", err)
	}
	respPackets = append(respPackets, pubKey)

	// Forward to client.
	if _, err := clientConn.Write(pubKey); err != nil {
		return reqPackets, respPackets, fmt.Errorf("failed to forward public key: %w", err)
	}
	orchestrator.SetTCPQuickACK(clientConn)

	// 3. Read encrypted password from client.
	encPass, err := readPacketSync(clientConn)
	if err != nil {
		return reqPackets, respPackets, fmt.Errorf("failed to read encrypted password: %w", err)
	}
	reqPackets = append(reqPackets, encPass)

	// Forward to server.
	if _, err := destConn.Write(encPass); err != nil {
		return reqPackets, respPackets, fmt.Errorf("failed to forward encrypted password: %w", err)
	}
	orchestrator.SetTCPQuickACK(destConn)

	// 4. Read final response.
	finalResp, err := readPacketSync(destConn)
	if err != nil {
		return reqPackets, respPackets, fmt.Errorf("failed to read final auth response: %w", err)
	}
	respPackets = append(respPackets, finalResp)

	// Forward to client.
	if _, err := clientConn.Write(finalResp); err != nil {
		return reqPackets, respPackets, fmt.Errorf("failed to forward final auth response: %w", err)
	}

	return reqPackets, respPackets, nil
}

// handlePlainPasswordSync handles plain password exchange over SSL.
func handlePlainPasswordSync(clientConn, destConn net.Conn) (reqPackets, respPackets [][]byte, err error) {
	// Read plain password from client.
	orchestrator.SetTCPQuickACK(clientConn)
	plainPass, err := readPacketSync(clientConn)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read plain password: %w", err)
	}
	reqPackets = append(reqPackets, plainPass)

	// Forward to server.
	if _, err := destConn.Write(plainPass); err != nil {
		return reqPackets, nil, fmt.Errorf("failed to forward plain password: %w", err)
	}
	orchestrator.SetTCPQuickACK(destConn)

	// Read final response.
	finalResp, err := readPacketSync(destConn)
	if err != nil {
		return reqPackets, nil, fmt.Errorf("failed to read final response: %w", err)
	}
	respPackets = append(respPackets, finalResp)

	// Forward to client.
	if _, err := clientConn.Write(finalResp); err != nil {
		return reqPackets, respPackets, fmt.Errorf("failed to forward final response: %w", err)
	}

	return reqPackets, respPackets, nil
}

// readPacketSync reads a complete MySQL wire packet from a raw net.Conn.
// Uses the same logic as mysqlUtils.ReadPacketBufferOrdered but works on
// plain net.Conn (no Peeker interface needed).
func readPacketSync(conn net.Conn) ([]byte, error) {
	return readPacketFromReader(conn)
}

// readPacketFromReader reads a single MySQL packet from any io.Reader.
// If the reader supports Peek (e.g. *bufio.Reader), the optimised peek
// path is used; otherwise falls back to reading the 4-byte header first.
func readPacketFromReader(r io.Reader) ([]byte, error) {
	// If the reader supports Peek (e.g. bufio.Reader wrapper), use the optimised path.
	if peeker, ok := r.(mysqlUtils.Peeker); ok {
		header, err := peeker.Peek(4)
		if err != nil {
			return nil, err
		}
		payloadLength := mysqlUtils.GetPayloadLength(header[:3])
		totalLen := 4 + int(payloadLength)
		packet := make([]byte, totalLen)
		if _, err := io.ReadFull(r, packet); err != nil {
			return nil, err
		}
		return packet, nil
	}

	// Standard path: read 4-byte header first.
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}

	payloadLength := mysqlUtils.GetPayloadLength(header[:3])
	totalLen := 4 + int(payloadLength)
	packet := make([]byte, totalLen)
	copy(packet, header[:])

	if payloadLength > 0 {
		if _, err := io.ReadFull(r, packet[4:]); err != nil {
			return packet[:4], err
		}
	}

	return packet, nil
}
