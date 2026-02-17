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

	// Re-enable TCP_QUICKACK before every read — Linux resets it after each ACK.
	orchestrator.SetTCPQuickACK(destConn)

	// Read the initial handshake from the server (server-greetings)
	handshake, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read initial handshake from server")
		return res, err
	}

	// Write the initial handshake to the client
	_, err = clientConn.Write(handshake)
	if err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write server greetings to the client")

		return res, err
	}
	orchestrator.SetTCPQuickACK(clientConn)

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

	// Re-enable TCP_QUICKACK before reading client response.
	orchestrator.SetTCPQuickACK(clientConn)

	// Handshake response from client (or SSL request)
	handshakeResponse, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		if err == io.EOF {
			logger.Debug("received request buffer is empty in record mode for mysql call")
			return res, err
		}
		utils.LogError(logger, err, "failed to read handshake response from client")

		return res, err
	}

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
	//
	// NOTE: Unlike PostgreSQL where SSLRequest is a standalone packet that can be intercepted
	// at the proxy level, MySQL SSL is tightly coupled with the protocol handshake:
	// 1. Server sends initial handshake with SSL capability flag
	// 2. Client responds with SSLRequest (handshake response with SSL flag but no auth)
	// 3. Both sides upgrade to TLS
	// 4. Client re-sends full handshake response with authentication over TLS
	//
	// This tight coupling means proxy-level MySQL SSL handling would require the proxy to:
	// - Parse the MySQL handshake protocol
	// - Track MySQL capability flags
	// - Understand the multi-phase MySQL handshake
	//
	// For now, MySQL SSL handling remains in the parser. Future work may move this to
	// the proxy level if needed.
	//
	// TODO: Consider moving MySQL SSL to proxy level when:
	// - Heap-based TLS key extraction is implemented
	// - MySQL protocol parsing is centralized in proxy
	if decodeCtx.UseSSL {

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
			// Note: Client is already upgraded above, only need to upgrade server side
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

		// Store (Reset) the last operation for the upgraded client connection, because after ssl request the client will send the handshake response packet again.
		decodeCtx.LastOp.Store(clientConn, mysql.HandshakeV10)

		// Store the server greeting packet for the upgraded client connection
		sg, ok := handshakePkt.Message.(*mysql.HandshakeV10Packet)
		if !ok {
			return res, fmt.Errorf("failed to type assert handshake packet")
		}
		decodeCtx.ServerGreetings.Store(clientConn, sg)

		// Read the handshake response packet from the client
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

		_, err = destConn.Write(handshakeResponse)
		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			utils.LogError(logger, err, "failed to write handshake response to server")

			return res, err
		}
		orchestrator.SetTCPQuickACK(destConn)

		// Decode client handshake response packet
		handshakeResponsePkt, err := wire.DecodePayload(ctx, logger, handshakeResponse, clientConn, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to decode handshake response packet")
			return res, err
		}

		res.req = append(res.req, mysql.Request{
			PacketBundle: *handshakeResponsePkt,
		})
	}

	// ── Phase 2: Create TeeForwardConns ──────────────────────────────
	// SSL detection is done.  From this point the auth exchange can
	// proceed at wire speed: a forwarding goroutine per direction
	// copies bytes between src and dest without waiting for the parser.
	// The parser reads already-forwarded data from the ring buffer.
	// Write() is a no-op on TeeForwardConn, so the existing read→write→decode
	// auth code below works unchanged.
	rawClientConn := clientConn // keep ref for decodeCtx re-registration
	clientTeeConn := orchestrator.NewTeeForwardConn(ctx, logger, clientConn, destConn)
	destTeeConn := orchestrator.NewTeeForwardConn(ctx, logger, destConn, clientConn)

	// Re-register decodeCtx entries with TeeForwardConn keys so the auth
	// decoder can look them up under the connection it actually reads from.
	if op, ok := decodeCtx.LastOp.Load(rawClientConn); ok {
		decodeCtx.LastOp.Store(clientTeeConn, op)
	}
	if sg, ok := decodeCtx.ServerGreetings.Load(rawClientConn); ok {
		decodeCtx.ServerGreetings.Store(clientTeeConn, sg)
	}

	// Switch to TeeForwardConns for the rest of the handshake.
	clientConn = clientTeeConn
	destConn = destTeeConn

	// Store TeeForwardConns for the caller (Record) to use in command phase.
	res.clientTeeConn = clientTeeConn
	res.destTeeConn = destTeeConn

	// Read the next auth packet,
	// It can be either auth more data if authentication from both server and client are agreed.(caching_sha2_password)
	// or auth switch request if the server wants to switch the auth mechanism
	// or it can be OK packet in case of native password
	orchestrator.SetTCPQuickACK(destConn)
	authData, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
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
	_, err = clientConn.Write(authData)
	if err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write auth packet to client during handshake")
		return res, err
	}
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
		authSwitchResponse, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
		if err != nil {
			if err == io.EOF {
				logger.Debug("received request buffer is empty in record mode for mysql call")
				return res, err
			}
			utils.LogError(logger, err, "failed to read auth switch response from client")
			return res, err
		}

		_, err = destConn.Write(authSwitchResponse)
		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			utils.LogError(logger, err, "failed to write auth switch response to server")
			return res, err
		}
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
		authData, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
		if err != nil {
			if err == io.EOF {
				logger.Debug("received request buffer is empty in record mode for mysql call")
				return res, err
			}
			utils.LogError(logger, err, "failed to read auth data from the server after handling auth switch response")

			return res, err
		}

		_, err = clientConn.Write(authData)
		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			utils.LogError(logger, err, "failed to write auth data to client after handling auth switch response")
			return res, err
		}
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
		authRes, err = handleAuth(ctx, logger, authDecider, clientConn, destConn, decodeCtx)
		if err != nil {
			return res, fmt.Errorf("failed to handle auth more data: %w", err)
		}
	case *mysql.OKPacket:
		authRes, err = handleAuth(ctx, logger, authDecider, clientConn, destConn, decodeCtx)
		if err != nil {
			return res, fmt.Errorf("failed to handle ok packet: %w", err)
		}
	}

	res.resTimestamp = time.Now()

	setHandshakeResult(&res, authRes)

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

	//As per wire shark capture, during fast auth success, server sends OK packet just after auth more data

	// read the ok/err packet from the server after auth more data
	orchestrator.SetTCPQuickACK(destConn)
	finalResp, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
	if err != nil {
		if err == io.EOF {
			logger.Debug("received request buffer is empty in record mode for mysql call")
			return res, err
		}
		utils.LogError(logger, err, "failed to read final response packet from server")
		return res, err
	}

	// write the ok/err packet to the client
	_, err = clientConn.Write(finalResp)
	if err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write ok/err packet to client during fast auth mechanism")
		return res, err
	}
	orchestrator.SetTCPQuickACK(clientConn)

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

	// read the public key request from the client
	orchestrator.SetTCPQuickACK(clientConn)
	publicKeyRequest, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read public key request from client")
		return res, err
	}
	_, err = destConn.Write(publicKeyRequest)
	if err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write public key request to server")
		return res, err
	}
	orchestrator.SetTCPQuickACK(destConn)

	publicKeyReqPkt, err := wire.DecodePayload(ctx, logger, publicKeyRequest, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode public key request packet")
		return res, err
	}

	res.req = append(res.req, mysql.Request{
		PacketBundle: *publicKeyReqPkt,
	})

	// read the "public key" as response from the server
	orchestrator.SetTCPQuickACK(destConn)
	pubKey, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read public key from server")
		return res, err
	}
	_, err = clientConn.Write(pubKey)
	if err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write public key response to client")
		return res, err
	}
	orchestrator.SetTCPQuickACK(clientConn)

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

	// read the encrypted password from the client
	orchestrator.SetTCPQuickACK(clientConn)
	encryptPass, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read encrypted password from client")

		return res, err
	}
	_, err = destConn.Write(encryptPass)
	if err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write encrypted password to server")
		return res, err
	}
	orchestrator.SetTCPQuickACK(destConn)

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

	// read the final response from the server (ok or error)
	orchestrator.SetTCPQuickACK(destConn)
	finalServerResponse, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read final response from server")
		return res, err
	}
	_, err = clientConn.Write(finalServerResponse)
	if err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write final response to client")

		return res, err
	}
	orchestrator.SetTCPQuickACK(clientConn)

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

	// read the plain password from the client
	orchestrator.SetTCPQuickACK(clientConn)
	plainPassBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read plain password from the client")
		return res, err
	}
	_, err = destConn.Write(plainPassBuf)
	if err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write plain password to the server")
		return res, err
	}
	orchestrator.SetTCPQuickACK(destConn)

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

	// read the final response from the server (ok or error)
	orchestrator.SetTCPQuickACK(destConn)
	finalServerResponse, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read final response from server")
		return res, err
	}
	_, err = clientConn.Write(finalServerResponse)
	if err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write final response to client")

		return res, err
	}
	orchestrator.SetTCPQuickACK(clientConn)

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
