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
	intgUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/agent/proxy/orchestrator"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	pUtils "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// Record mode
type handshakeRes struct {
	req               []mysql.Request
	resp              []mysql.Response
	requestOperation  string
	responseOperation string
	reqTimestamp      time.Time
	resTimestamp      time.Time
	tlsClientConn     net.Conn
	tlsDestConn       net.Conn
	// TeeForwardConns created after SSL detection — auth + command phases
	// flow through these at wire speed, decoupled from the parser.
	clientTeeConn net.Conn
	destTeeConn   net.Conn
}

func handleInitialHandshake(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, decodeCtx *wire.DecodeContext, opts models.OutgoingOptions) (handshakeRes, error) {

	res := handshakeRes{
		req:  make([]mysql.Request, 0),
		resp: make([]mysql.Response, 0),
	}

	// ── Phase 1: Initial TeeForwardConn ──────────────────────────────
	// Create TeeForwardConns immediately for the initial handshake phase.
	// NOTE: We ONLY create it for the Destination (Server -> Client) direction initially.
	// We CANNOT create it for Client -> Server key yet, because if the client sends
	// SSLRequest followed immediately by ClientHello (TLS), a blind forwarder would
	// send the ClientHello to the raw destination, preventing us from intercepting
	// the TLS handshake (MITM).
	destTeeConn := orchestrator.NewTeeForwardConn(ctx, logger, destConn, clientConn)

	// re-enable TCP_QUICKACK done inside TeeForwardConn loop for destConn.
	// For clientConn, we might want to set it manually since we are reading synchronously initially.
	orchestrator.SetTCPQuickACK(clientConn)

	// 1. Read Server Greeting (from destTeeConn)
	// This uses the ring buffer, decoupling the server's write from our parser.
	handshake, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destTeeConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read initial handshake from server")
		return res, err
	}
	// NO clientConn.Write needed (handled by destTeeConn forwarder).

	// Set the timestamp of the initial request
	res.reqTimestamp = time.Now()

	// Decode server handshake packet
	handshakePkt, err := wire.DecodePayload(ctx, logger, handshake, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode handshake packet")
		return res, err
	}

	// Set the intial request operation
	res.requestOperation = handshakePkt.Header.Type

	// Get the initial Plugin Name
	pluginName, err := wire.GetPluginName(handshakePkt.Message)
	if err != nil {
		utils.LogError(logger, err, "failed to get initial plugin name")
		return res, err
	}

	// Set the initial plugin name
	decodeCtx.PluginName = pluginName

	res.resp = append(res.resp, mysql.Response{
		PacketBundle: *handshakePkt,
	})

	// 2. Read Handshake Response (Synchronously from clientConn)
	// We MUST read this synchronously to inspect if it is an SSL Request
	// before we allow any further data (like ClientHello) to reach the server.
	orchestrator.SetTCPQuickACK(clientConn)
	handshakeResponse, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		if err == io.EOF {
			logger.Debug("received request buffer is empty in record mode for mysql call")
			return res, err
		}
		utils.LogError(logger, err, "failed to read handshake response from client")
		return res, err
	}

	// We manually write this to the destination because we don't have a clientTeeConn yet.
	// Use destConn directly.
	_, err = destConn.Write(handshakeResponse)
	if err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write handshake response to server")
		return res, err
	}
	orchestrator.SetTCPQuickACK(destConn)

	// Decode client handshake response (or SSL) packet
	handshakeResponsePkt, err := wire.DecodePayload(ctx, logger, handshakeResponse, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode handshake response packet")
		return res, err
	}

	res.req = append(res.req, mysql.Request{
		PacketBundle: *handshakeResponsePkt,
	})

	// Handle the SSL request.
	var clientTeeConn *orchestrator.TeeForwardConn

	if decodeCtx.UseSSL {
		// Stop the initial dest forwarder because we are about to upgrade the underlying socket.
		// Use destTeeConn.Stop() to clean up the goroutine.
		// Stop() filters expected i/o timeout errors caused by the deadline trick.
		if err := destTeeConn.Stop(); err != nil {
			logger.Debug("dest tee conn stop returned error during ssl upgrade", zap.Error(err))
		}
		// Reset the deadline on destConn, as Stop() might have set it to unblock the reader.
		if err := destConn.SetReadDeadline(time.Time{}); err != nil {
			logger.Warn("failed to reset read deadline on dest conn", zap.Error(err))
		}

		reader := bufio.NewReader(clientConn)
		initialData := make([]byte, 5)
		// reading the initial data from the client connection to determine if the connection is a TLS handshake
		testBuffer, err := reader.Peek(len(initialData))
		if err != nil {
			if err == io.EOF && len(testBuffer) == 0 {
				logger.Debug("received EOF, closing conn", zap.Error(err))
				return res, nil
			}
			utils.LogError(logger, err, "failed to peek the mysql request message in proxy")
			return res, err
		}

		multiReader := io.MultiReader(reader, clientConn)
		clientConn = &pUtils.Conn{
			Conn:   clientConn,
			Reader: multiReader,
			Logger: logger,
		}

		// handle the TLS connection and get the upgraded client connection
		isTLS := pTls.IsTLSHandshake(testBuffer)
		if isTLS {
			clientConn, _, err = pTls.HandleTLSConnection(ctx, logger, clientConn, opts.Backdate)
			if err != nil {
				utils.LogError(logger, err, "failed to handle TLS conn")
				return res, err
			}

			// Upgrade server connection to TLS using centralized helper
			remoteAddr := clientConn.RemoteAddr().(*net.TCPAddr)
			sourcePort := remoteAddr.Port
			serverAddr := destConn.RemoteAddr().String()

			upgradedDest, tlsErr := pTls.UpgradeMySQLServerToTLS(
				ctx, logger, destConn, sourcePort, serverAddr)
			if tlsErr != nil {
				utils.LogError(logger, tlsErr, "failed to upgrade MySQL server connection to TLS")
				return res, tlsErr
			}
			destConn = upgradedDest
		}

		// Update this tls connection information in the handshake result
		res.tlsClientConn = clientConn
		res.tlsDestConn = destConn

		// Store (Reset) the last operation for the upgraded client connection
		decodeCtx.LastOp.Store(clientConn, mysql.HandshakeV10)

		// Store the server greeting packet for the upgraded client connection
		sg, ok := handshakePkt.Message.(*mysql.HandshakeV10Packet)
		if !ok {
			return res, fmt.Errorf("failed to type assert handshake packet")
		}
		decodeCtx.ServerGreetings.Store(clientConn, sg)

		// ── Phase 2: Create TeeForwardConns for TLS ──────────────────────
		// Now that we have TLS connections, wrap them in NEW TeeForwardConns.
		clientTeeConn = orchestrator.NewTeeForwardConn(ctx, logger, clientConn, destConn)
		destTeeConn = orchestrator.NewTeeForwardConn(ctx, logger, destConn, clientConn)

		// ALSO store the greeting for the wrapper `clientTeeConn` because record.go
		// uses this wrapper to call DecodePayload later.
		decodeCtx.ServerGreetings.Store(clientTeeConn, sg)

		// Read the handshake response from Client (again, but over TLS)
		handshakeResponse, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientTeeConn)
		if err != nil {
			if err == io.EOF {
				logger.Debug("received request buffer is empty in record mode for mysql call")
				return res, err
			}
			utils.LogError(logger, err, "failed to read handshake response from client")
			return res, err
		}
		// NO destConn.Write needed.

		// Decode client handshake response packet
		handshakeResponsePkt, err := wire.DecodePayload(ctx, logger, handshakeResponse, clientConn, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to decode handshake response packet")
			return res, err
		}

		res.req = append(res.req, mysql.Request{
			PacketBundle: *handshakeResponsePkt,
		})
	} else {
		// Non-SSL Path:
		// We already have destTeeConn. We need to start clientTeeConn now.
		clientTeeConn = orchestrator.NewTeeForwardConn(ctx, logger, clientConn, destConn)

		// Access the greeting from the initial packet and store it for the new wrapper
		if sg, ok := handshakePkt.Message.(*mysql.HandshakeV10Packet); ok {
			decodeCtx.ServerGreetings.Store(clientTeeConn, sg)
		} else {
			logger.Warn("failed to type assert handshake packet to store for tee conn (non-ssl)")
		}
	}

	// Read the next auth packet,
	// It can be either auth more data if authentication from both server and client are agreed.(caching_sha2_password)
	// or auth switch request if the server wants to switch the auth mechanism
	// or it can be OK packet in case of native password
	orchestrator.SetTCPQuickACK(destConn)
	// Use destTeeConn to read. Forwarding to client is automatic.
	authData, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destTeeConn)
	if err != nil {
		if err == io.EOF {
			logger.Debug("received request buffer is empty in record mode for mysql call")

			return res, err
		}
		utils.LogError(logger, err, "failed to read auth or final response packet from server during handshake")
		return res, err
	}

	// AuthSwitchRequest: If the server sends an AuthSwitchRequest, then there must be a diff auth type with its data
	// AuthMoreData: If the server sends an AuthMoreData, then it tells the auth mechanism type for the initial plugin name or for the auth switch request.
	// OK/ERR: If the server sends an OK/ERR packet, in case of native password.
	// NO clientConn.Write needed.

	orchestrator.SetTCPQuickACK(clientConn)

	// Decode auth or final response packet
	authDecider, err := wire.DecodePayload(ctx, logger, authData, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode auth packet during handshake")
		return res, err
	}

	// check if the authDecider is of type AuthSwitchRequestPacket.
	// AuthSwitchRequestPacket is sent by the server to the client to switch the auth mechanism
	if _, ok := authDecider.Message.(*mysql.AuthSwitchRequestPacket); ok {

		logger.Debug("Server is changing the auth mechanism by sending AuthSwitchRequestPacket")

		//save the auth switch request packet
		res.resp = append(res.resp, mysql.Response{
			PacketBundle: *authDecider,
		})

		pkt := authDecider.Message.(*mysql.AuthSwitchRequestPacket)

		// Change the plugin name due to auth switch request
		decodeCtx.PluginName = pkt.PluginName

		// read the auth switch response from the client
		orchestrator.SetTCPQuickACK(clientConn)
		// Use clientTeeConn to read. Forwarding to server is automatic.
		authSwitchResponse, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientTeeConn)
		if err != nil {
			if err == io.EOF {
				logger.Debug("received request buffer is empty in record mode for mysql call")
				return res, err
			}
			utils.LogError(logger, err, "failed to read auth switch response from client")
			return res, err
		}

		// NO destConn.Write needed.
		orchestrator.SetTCPQuickACK(destConn)

		// Decode the auth switch response packet
		authSwithResp, err := mysqlUtils.BytesToMySQLPacket(authSwitchResponse)
		if err != nil {
			utils.LogError(logger, err, "failed to parse MySQL packet")
			return res, err
		}

		authSwithRespPkt := &mysql.PacketBundle{
			Header: &mysql.PacketInfo{
				Header: &authSwithResp.Header,
				Type:   mysql.AuthSwithResponse, // there is no specific identifier for AuthSwitchResponse
			},
			Message: intgUtils.EncodeBase64(authSwithResp.Payload),
		}

		// save the auth switch response packet
		res.req = append(res.req, mysql.Request{
			PacketBundle: *authSwithRespPkt,
		})

		logger.Debug("Auth mechanism is switched successfully")

		// read the further auth packet, now it can be either auth more data or OK packet
		orchestrator.SetTCPQuickACK(destConn)
		// Use destTeeConn.
		authData, err = mysqlUtils.ReadPacketBuffer(ctx, logger, destTeeConn)
		if err != nil {
			if err == io.EOF {
				logger.Debug("received request buffer is empty in record mode for mysql call")
				return res, err
			}
			utils.LogError(logger, err, "failed to read auth data from the server after handling auth switch response")

			return res, err
		}

		// NO clientConn.Write needed.
		orchestrator.SetTCPQuickACK(clientConn)

		// It can be either auth more data or OK packet
		authDecider, err = wire.DecodePayload(ctx, logger, authData, clientConn, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to decode auth data packet after handling auth switch response")
			return res, err
		}
	}

	var authRes handshakeRes
	switch authDecider.Message.(type) {
	case *mysql.AuthMoreDataPacket:
		// Pass TeeConns to handleAuth for continued forwarding support
		authRes, err = handleAuth(ctx, logger, authDecider, clientTeeConn, destTeeConn, decodeCtx)
		if err != nil {
			return res, fmt.Errorf("failed to handle auth more data: %w", err)
		}
	case *mysql.OKPacket:
		authRes, err = handleAuth(ctx, logger, authDecider, clientTeeConn, destTeeConn, decodeCtx)
		if err != nil {
			return res, fmt.Errorf("failed to handle ok packet: %w", err)
		}
	}

	res.resTimestamp = time.Now()

	setHandshakeResult(&res, authRes)

	// Populate the TeeForwardConns in the result so record.go can use them
	res.clientTeeConn = clientTeeConn
	res.destTeeConn = destTeeConn

	return res, nil
}

func setHandshakeResult(res *handshakeRes, authRes handshakeRes) {
	res.req = append(res.req, authRes.req...)
	res.resp = append(res.resp, authRes.resp...)
	res.responseOperation = authRes.responseOperation
}

func handleAuth(ctx context.Context, logger *zap.Logger, authPkt *mysql.PacketBundle, clientConn, destConn net.Conn, decodeCtx *wire.DecodeContext) (handshakeRes, error) {
	res := handshakeRes{
		req:  make([]mysql.Request, 0),
		resp: make([]mysql.Response, 0),
	}

	switch mysql.AuthPluginName(decodeCtx.PluginName) {
	case mysql.Native:
		res.resp = append(res.resp, mysql.Response{
			PacketBundle: *authPkt,
		})

		res.responseOperation = authPkt.Header.Type
		logger.Debug("native password authentication is handled successfully")
	case mysql.CachingSha2:
		result, err := handleCachingSha2Password(ctx, logger, authPkt, clientConn, destConn, decodeCtx)
		if err != nil {
			return res, fmt.Errorf("failed to handle caching sha2 password: %w", err)
		}
		logger.Debug("caching sha2 password authentication is handled successfully")
		setHandshakeResult(&res, result)
	case mysql.Sha256:
		return res, fmt.Errorf("Sha256 Password authentication is not supported")
	default:
		return res, fmt.Errorf("unsupported authentication plugin: %s", decodeCtx.PluginName)
	}

	return res, nil
}

func handleCachingSha2Password(ctx context.Context, logger *zap.Logger, authPkt *mysql.PacketBundle, clientConn, destConn net.Conn, decodeCtx *wire.DecodeContext) (handshakeRes, error) {
	res := handshakeRes{
		req:  make([]mysql.Request, 0),
		resp: make([]mysql.Response, 0),
	}

	var authMechanism string
	var err error
	var ok bool
	var authMorePkt *mysql.AuthMoreDataPacket

	// check if the authPkt is of type AuthMoreDataPacket
	if authMorePkt, ok = authPkt.Message.(*mysql.AuthMoreDataPacket); !ok {
		return res, fmt.Errorf("invalid packet type for caching sha2 password mechanism, expected: AuthMoreDataPacket, found: %T", authPkt.Message)
	}

	// Getting the string value of the caching_sha2_password mechanism
	authMechanism, err = wire.GetCachingSha2PasswordMechanism(authMorePkt.Data[0])
	if err != nil {
		return res, fmt.Errorf("failed to get caching sha2 password mechanism: %w", err)
	}
	authMorePkt.Data = authMechanism

	// save the auth more data packet
	res.resp = append(res.resp, mysql.Response{
		PacketBundle: *authPkt,
	})

	auth, err := wire.StringToCachingSha2PasswordMechanism(authMechanism)
	if err != nil {
		return res, fmt.Errorf("failed to convert string to caching sha2 password mechanism: %w", err)
	}

	var result handshakeRes
	switch auth {
	case mysql.PerformFullAuthentication:
		result, err = handleFullAuth(ctx, logger, clientConn, destConn, decodeCtx)
		if err != nil {
			return res, fmt.Errorf("failed to handle caching sha2 password full auth: %w", err)
		}
	case mysql.FastAuthSuccess:
		result, err = handleFastAuthSuccess(ctx, logger, clientConn, destConn, decodeCtx)
		if err != nil {
			return res, fmt.Errorf("failed to handle caching sha2 password fast auth success: %w", err)
		}
	}

	setHandshakeResult(&res, result)

	return res, nil
}

func handleFastAuthSuccess(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, decodeCtx *wire.DecodeContext) (handshakeRes, error) {
	res := handshakeRes{
		req:  make([]mysql.Request, 0),
		resp: make([]mysql.Response, 0),
	}

	// As per wire shark capture, during fast auth success, server sends OK packet just after auth more data.
	// NOTE: When called with TeeForwardConns, reads come from the ring buffer
	// (data already forwarded to peer) and writes are no-ops (forwarding goroutine
	// already sent the data at wire speed). No manual Write needed.

	// read the ok/err packet from the server after auth more data
	finalResp, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
	if err != nil {
		if err == io.EOF {
			logger.Debug("received request buffer is empty in record mode for mysql call")
			return res, err
		}
		utils.LogError(logger, err, "failed to read final response packet from server")
		return res, err
	}

	finalPkt, err := wire.DecodePayload(ctx, logger, finalResp, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode final response packet after auth data packet")
		return res, err
	}

	res.resp = append(res.resp, mysql.Response{
		PacketBundle: *finalPkt,
	})

	// Set the final response operation of the handshake
	res.responseOperation = finalPkt.Header.Type
	logger.Debug("fast auth success is handled successfully")

	return res, nil
}

func handleFullAuth(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, decodeCtx *wire.DecodeContext) (handshakeRes, error) {
	res := handshakeRes{
		req:  make([]mysql.Request, 0),
		resp: make([]mysql.Response, 0),
	}

	// If the connection is using SSL, we don't need to exchange the public key and encrypted password,
	// we can directly handle the plain password.
	// This is because the SSL connection already provides a secure channel for the password exchange.
	if decodeCtx.UseSSL {
		logger.Debug("Handling caching_sha2_password full auth in SSL request, using plain password")
		res2, err := handlePlainPassword(ctx, logger, clientConn, destConn, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to handle plain password in caching_sha2_password(full auth) in ssl request")
			return res, fmt.Errorf("failed to handle plain password in caching_sha2_password full auth: %w", err)
		}
		// Set the final response operation of the handshake
		setHandshakeResult(&res, res2)
		return res, nil
	}

	// Read the public key request from the client.
	// NOTE: With TeeForwardConns, the data is already forwarded to the server
	// by the clientTeeConn goroutine. No manual Write needed.
	publicKeyRequest, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read public key request from client")
		return res, err
	}

	publicKeyReqPkt, err := wire.DecodePayload(ctx, logger, publicKeyRequest, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode public key request packet")
		return res, err
	}

	res.req = append(res.req, mysql.Request{
		PacketBundle: *publicKeyReqPkt,
	})

	// Read the public key response from the server.
	// With TeeForwardConns, forwarding to client is automatic.
	pubKey, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read public key from server")
		return res, err
	}

	pubKeyPkt, err := wire.DecodePayload(ctx, logger, pubKey, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode public key packet")
		return res, err
	}

	pubKeyPkt.Meta = map[string]string{
		"auth operation": "public key response",
	}

	res.resp = append(res.resp, mysql.Response{
		PacketBundle: *pubKeyPkt,
	})

	// Read the encrypted password from the client.
	// With TeeForwardConns, forwarding to server is automatic.
	encryptPass, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read encrypted password from client")
		return res, err
	}

	encPass, err := mysqlUtils.BytesToMySQLPacket(encryptPass)
	if err != nil {
		utils.LogError(logger, err, "failed to parse MySQL packet")
		return res, err
	}

	encryptPassPkt := &mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &encPass.Header,
			Type:   mysql.EncryptedPassword,
		},
		Message: intgUtils.EncodeBase64(encPass.Payload),
	}

	res.req = append(res.req, mysql.Request{
		PacketBundle: *encryptPassPkt,
	})

	// Read the final response from the server (ok or error).
	// With TeeForwardConns, forwarding to client is automatic.
	finalServerResponse, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read final response from server")
		return res, err
	}

	finalResPkt, err := wire.DecodePayload(ctx, logger, finalServerResponse, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode final response packet during caching sha2 password full auth")
		return res, err
	}

	res.resp = append(res.resp, mysql.Response{
		PacketBundle: *finalResPkt,
	})

	// Set the final response operation of the handshake
	res.responseOperation = finalResPkt.Header.Type

	logger.Debug("full auth is handled successfully")
	return res, nil
}

func handlePlainPassword(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, decodeCtx *wire.DecodeContext) (handshakeRes, error) {
	res := handshakeRes{
		req:  make([]mysql.Request, 0),
		resp: make([]mysql.Response, 0),
	}

	// Read the plain password from the client.
	// NOTE: With TeeForwardConns, reading from clientConn's ring buffer means
	// the data was already forwarded to destConn by the tee goroutine.
	plainPassBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read plain password from the client")
		return res, err
	}

	plainPass, err := mysqlUtils.BytesToMySQLPacket(plainPassBuf)
	if err != nil {
		utils.LogError(logger, err, "failed to parse MySQL packet")
		return res, err
	}

	plainPassPkt := &mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &plainPass.Header,
			Type:   mysql.PlainPassword,
		},
		Message: intgUtils.EncodeBase64(plainPass.Payload),
	}

	res.req = append(res.req, mysql.Request{
		PacketBundle: *plainPassPkt,
	})

	// Read the final response from the server (ok or error).
	// With TeeForwardConns, forwarding to client is automatic.
	finalServerResponse, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read final response from server")
		return res, err
	}

	finalResPkt, err := wire.DecodePayload(ctx, logger, finalServerResponse, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode final response packet during caching sha2 password full auth (plain password)")
		return res, err
	}

	res.resp = append(res.resp, mysql.Response{
		PacketBundle: *finalResPkt,
	})

	// Set the final response operation of the handshake
	res.responseOperation = finalResPkt.Header.Type

	logger.Debug("full auth (plain password) is handled successfully")
	return res, nil
}
