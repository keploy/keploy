package recorder

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"time"

	mysqlUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	phase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase"
	connPhase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	intgUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
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
	skipConfigMock    bool
}

func handleInitialHandshake(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, decodeCtx *wire.DecodeContext, opts models.OutgoingOptions, tlsUpgrader models.TLSUpgrader) (handshakeRes, error) {
	logger.Debug("handleInitialHandshake: entered",
		zap.String("connKey", opts.ConnKey),
		zap.Bool("skipTLSMITM", opts.SkipTLSMITM))

	res := handshakeRes{
		req:  make([]mysql.Request, 0),
		resp: make([]mysql.Response, 0),
	}

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

	// Store server capabilities for CLIENT_DEPRECATE_EOF handling during query phase
	if greeting, ok := handshakePkt.Message.(*mysql.HandshakeV10Packet); ok {
		decodeCtx.ServerCaps = greeting.CapabilityFlags
	}

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

	// Decode client handshake response (or SSL) packet
	handshakeResponsePkt, err := wire.DecodePayload(ctx, logger, handshakeResponse, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode handshake response packet")
		return res, err
	}

	// DecodePayload stores the client flags in ClientCapabilities. Also
	// populate ClientCaps so that DeprecateEOF() (which checks ClientCaps
	// via effectiveClientCaps()) works correctly in record mode.
	decodeCtx.ClientCaps = decodeCtx.ClientCapabilities

	res.req = append(res.req, mysql.Request{
		PacketBundle: *handshakeResponsePkt,
	})

	// handle the SSL request
	logger.Debug("handleInitialHandshake: client response decoded",
		zap.Bool("useSSL", decodeCtx.UseSSL),
		zap.Bool("skipTLSMITM", opts.SkipTLSMITM),
		zap.String("packetType", handshakeResponsePkt.Header.Type))
	if decodeCtx.UseSSL {

		// When TLS MITM is skipped, the proxy does not terminate TLS.
		// The pre-TLS config mock (server greeting + SSL request) has been captured;
		// post-TLS command phase data is provided by SSL/GoTLS uprobes separately.
		// Push the raw server greeting to TLSHandshakeStore so the post-TLS
		// uprobe path can reconstruct the decode context for command-phase recording.
		if opts.SkipTLSMITM {
			logger.Debug("SkipTLSMITM set — pushing handshake data to TLSHandshakeStore for post-TLS path to combine")
			hsStore, ok := ctx.Value(models.TLSHandshakeStoreKey).(*models.TLSHandshakeStore)
			if !ok || hsStore == nil {
				return res, fmt.Errorf("SkipTLSMITM requires TLSHandshakeStore in context for MySQL handshake reconstruction")
			}
			dstPort := uint16(0)
			if opts.DstCfg != nil {
				dstPort = uint16(opts.DstCfg.Port)
			}
			storeKey := models.HandshakeStoreKey(opts.ConnKey, dstPort)
			hsEntry := models.TLSHandshakeEntry{
				RespPackets:  [][]byte{handshake},
				ReqPackets:   [][]byte{handshakeResponse},
				ReqTimestamp: res.reqTimestamp,
			}
			hsStore.Push(storeKey, hsEntry)
			logger.Debug("Pushed MySQL server greeting + SSLRequest to TLSHandshakeStore",
				zap.String("key", storeKey),
				zap.String("connKey", opts.ConnKey),
				zap.Uint16("dstPort", dstPort))
			// Also push under the port-only fallback key. The proxy and uprobe
			// see different TCP connections (different ephemeral ports) due to
			// sockmap/eBPF redirection, so conn-specific keys won't match.
			// The port-only key ensures the post-TLS path can find the entry.
			portKey := models.HandshakeStoreKey("", dstPort)
			if portKey != storeKey {
				hsStore.Push(portKey, hsEntry)
				logger.Debug("Also pushed to port-only fallback key",
					zap.String("portKey", portKey))
			}
			// Signal that the pre-TLS config mock should NOT be recorded here;
			// the post-TLS path will produce a single combined config mock.
			res.skipConfigMock = true
			return res, nil
		}

		if tlsUpgrader == nil {
			logger.Debug("TLS upgrade requested but no TLSUpgrader available (sockmap/non-MITM path)")
			return res, nil
		}

		// UpgradeClientTLS peeks the client connection internally to detect
		// a TLS ClientHello, and if found, performs the TLS termination.
		upgradedConn, isTLS, _, err := tlsUpgrader.UpgradeClientTLS(ctx, opts.Backdate)
		if err != nil {
			utils.LogError(logger, err, "failed to upgrade client TLS for mysql")
			return res, err
		}
		clientConn = upgradedConn
		if isTLS {
			// Upgrade destination side via TLSUpgrader.
			remoteAddr := clientConn.RemoteAddr().(*net.TCPAddr)
			sourcePort := remoteAddr.Port

			url, ok := pTls.SrcPortToDstURL.Load(sourcePort)
			if !ok {
				return res, fmt.Errorf("failed to fetch destination url for source port %d", sourcePort)
			}
			dstURL, ok := url.(string)
			if !ok {
				return res, fmt.Errorf("failed to type cast destination url for source port %d", sourcePort)
			}

			tlsConfig := &tls.Config{
				InsecureSkipVerify: true,
				ServerName:         dstURL,
			}
			logger.Debug("Upgrading the destination connection to TLS", zap.String("ServerName", tlsConfig.ServerName))

			destConn, err = tlsUpgrader.UpgradeDestTLS(tlsConfig)
			if err != nil {
				utils.LogError(logger, err, "failed to upgrade the destination connection to TLS for mysql")
				return res, err
			}
			logger.Debug("TLS connection established with the destination server")
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

		// Decode client handshake response packet
		handshakeResponsePkt, err := wire.DecodePayload(ctx, logger, handshakeResponse, clientConn, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to decode handshake response packet")
			return res, err
		}

		// After TLS upgrade, the client sends a new HandshakeResponse41 with
		// the final negotiated capabilities. Update ClientCaps so
		// DeprecateEOF() reflects the post-TLS negotiation.
		decodeCtx.ClientCaps = decodeCtx.ClientCapabilities

		res.req = append(res.req, mysql.Request{
			PacketBundle: *handshakeResponsePkt,
		})
	}

	// Read the next auth packet,
	// It can be either auth more data if authentication from both server and client are agreed.(caching_sha2_password)
	// or auth switch request if the server wants to switch the auth mechanism
	// or it can be OK packet in case of native password
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

	publicKeyReqPkt, err := wire.DecodePayload(ctx, logger, publicKeyRequest, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode public key request packet")
		return res, err
	}

	res.req = append(res.req, mysql.Request{
		PacketBundle: *publicKeyReqPkt,
	})

	// read the "public key" as response from the server
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

// handlePostTLSRecord handles MySQL recording for the post-TLS uprobe path.
// In this mode, the SSL/GoTLS uprobes provide decrypted plaintext starting
// from HandshakeResponse41 (the full auth after TLS handshake). The server
// greeting was captured by the ringbuf path and stored in TLSHandshakeStore.
func handlePostTLSRecord(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, mocks chan<- *models.Mock, decodeCtx *wire.DecodeContext, opts models.OutgoingOptions) error {
	// 1. Pop the server greeting from TLSHandshakeStore.
	dstPort := uint16(0)
	if opts.DstCfg != nil {
		dstPort = uint16(opts.DstCfg.Port)
	}
	hsStore, _ := ctx.Value(models.TLSHandshakeStoreKey).(*models.TLSHandshakeStore)
	if hsStore == nil {
		return fmt.Errorf("TLSHandshakeStore not available in context for post-TLS MySQL recording")
	}
	storeKey := models.HandshakeStoreKey(opts.ConnKey, dstPort)
	logger.Debug("Post-TLS MySQL: popping from TLSHandshakeStore",
		zap.String("key", storeKey),
		zap.String("connKey", opts.ConnKey),
		zap.Uint16("dstPort", dstPort))
	entry, ok := hsStore.PopWait(storeKey, 5*time.Second)
	// If conn-specific key missed, try the port-only fallback key.
	// The proxy and uprobe see different TCP connections with different
	// ephemeral ports, so conn-specific keys may not match.
	if !ok && opts.ConnKey != "" {
		portKey := models.HandshakeStoreKey("", dstPort)
		logger.Debug("Conn-specific key missed, trying port-only fallback",
			zap.String("portKey", portKey))
		entry, ok = hsStore.PopWait(portKey, 2*time.Second)
	}
	var serverGreetingBuf []byte
	if ok && len(entry.RespPackets) > 0 {
		serverGreetingBuf = entry.RespPackets[0]
		logger.Debug("Post-TLS MySQL: successfully popped handshake data from TLSHandshakeStore",
			zap.String("key", storeKey))
	} else {
		// Fallback: the pre-TLS handshake was not captured (e.g. the MySQL
		// connection was established before the proxy started intercepting).
		// Connect to the MySQL server directly to fetch the server greeting.
		logger.Debug("TLSHandshakeStore empty — fetching server greeting directly (this can be transient; if repeated, verify proxy intercept timing and handshake key consistency)",
			zap.String("key", storeKey),
			zap.String("connKey", opts.ConnKey),
			zap.Uint16("dstPort", dstPort))
		var err error
		serverGreetingBuf, err = fetchServerGreeting(ctx, opts)
		if err != nil {
			return fmt.Errorf("no server greeting in TLSHandshakeStore for key %s and direct fetch failed: %w", storeKey, err)
		}
	}

	// 2. Decode the server greeting to initialize decode context.
	greetingPkt, err := wire.DecodePayload(ctx, logger, serverGreetingBuf, clientConn, decodeCtx)
	if err != nil {
		return fmt.Errorf("failed to decode stored server greeting for post-TLS: %w", err)
	}
	pluginName, err := wire.GetPluginName(greetingPkt.Message)
	if err != nil {
		return fmt.Errorf("failed to get plugin name from stored server greeting: %w", err)
	}
	decodeCtx.PluginName = pluginName
	decodeCtx.UseSSL = true

	// Store the server greeting for the clientConn (needed by DecodePayload for command phase).
	sg, ok := greetingPkt.Message.(*mysql.HandshakeV10Packet)
	if !ok {
		return fmt.Errorf("stored server greeting is not HandshakeV10Packet")
	}
	decodeCtx.ServerGreetings.Store(clientConn, sg)
	decodeCtx.LastOp.Store(clientConn, mysql.HandshakeV10)

	// Seed server capabilities from the restored greeting. Without this,
	// decodeCtx.ServerCaps stays 0 and DeprecateEOF() returns false,
	// which makes the TextResultSet handler look for an EOF packet
	// between column definitions and row data. Modern clients
	// (mysql-connector-python, mysql-connector-j, Go's go-sql-driver
	// with DEPRECATE_EOF) send the row bytes immediately, and the
	// recorder aborts with "expected EOF packet for column definition".
	// Mirror what handleInitialHandshake does on its path.
	decodeCtx.ServerCaps = sg.CapabilityFlags

	logger.Debug("Post-TLS MySQL: restored server greeting",
		zap.String("key", storeKey),
		zap.String("pluginName", pluginName))

	// 3. Read the first packet from the client to determine if this is
	//    a fresh connection (HandshakeResponse41) or an existing connection
	//    already in command phase (COM_QUERY, COM_STMT_*, etc.).
	//
	//    Distinction: HandshakeResponse41 has sequence number >= 1 (follows
	//    server greeting at seq 0). Command phase packets have seq 0
	//    (each command starts a new sequence).
	firstPkt, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		return fmt.Errorf("failed to read first post-TLS client packet: %w", err)
	}

	if len(firstPkt) < 4 {
		return fmt.Errorf("first post-TLS packet too short (%d bytes)", len(firstPkt))
	}

	seqNum := firstPkt[3] // 4th byte of MySQL packet header is the sequence number

	if seqNum == 0 {
		// Sequence 0 = command phase packet. The connection was already
		// authenticated before interception started. Skip auth exchange
		// and go directly to command phase recording.
		logger.Debug("Post-TLS MySQL: existing connection detected (seq=0), skipping auth — entering command phase directly")

		// We didn't decode a HandshakeResponse41 on this path, so
		// ClientCaps is still 0. Assume the client is modern and
		// advertises CLIENT_DEPRECATE_EOF — this matches every
		// supported driver (mysql-connector-python/j, Go's
		// go-sql-driver, Node mysql2). If the server ALSO advertised
		// it (check on line above), DeprecateEOF() will return true
		// and the EOF-less result set decode path will be used.
		decodeCtx.ClientCaps = wire.CLIENT_DEPRECATE_EOF
		decodeCtx.ClientCapabilities = wire.CLIENT_DEPRECATE_EOF

		// Produce a synthetic config mock from pre-TLS data so test mode
		// can match the SSLRequest + HandshakeResponse41 during replay.
		if ok && len(entry.ReqPackets) > 0 {
			if err := recordSyntheticConfigMock(ctx, logger, clientConn, mocks, decodeCtx, greetingPkt, entry, opts); err != nil {
				logger.Debug("best-effort synthetic config mock generation failed for seq=0 path; continuing with command-phase capture", zap.Error(err))
			}
		}

		// Feed the first packet back to the parser by wrapping clientConn.
		wrappedClient := &pUtils.Conn{
			Conn:   clientConn,
			Reader: pUtils.NewPrefixReader(firstPkt, clientConn),
			Logger: logger,
		}

		// Re-key decode context maps: handleClientQueries will use wrappedClient
		// (a different net.Conn pointer) for map lookups in DecodePayload.
		decodeCtx.ServerGreetings.Store(wrappedClient, sg)
		decodeCtx.LastOp.Store(wrappedClient, wire.RESET)

		return handleClientQueries(ctx, logger, wrappedClient, destConn, mocks, decodeCtx, opts)
	}

	var requests []mysql.Request
	var responses []mysql.Response

	// Prepend the pre-TLS SSLRequest and server greeting from TLSHandshakeStore
	// so we produce a single combined config mock matching the hosted format:
	//   requests:  [SSLRequest, HandshakeResponse41, ...]
	//   responses: [HandshakeV10, ..., OK]
	// Note: SSLRequest MUST be decoded FIRST before HandshakeResponse41 so that decodeCtx.LastOp is still HandshakeV10
	if ok && len(entry.ReqPackets) > 0 {
		sslReqPkt, sslErr := wire.DecodePayload(ctx, logger, entry.ReqPackets[0], clientConn, decodeCtx)
		if sslErr != nil {
			logger.Debug("failed to decode stored SSLRequest for combined config mock; proceeding with post-TLS packets only", zap.Error(sslErr))
		} else {
			requests = append(requests, mysql.Request{PacketBundle: *sslReqPkt})
		}
	}

	// Sequence >= 1 = HandshakeResponse41 (auth exchange in progress).
	// Decoding this will update LastOp to HandshakeResponse41.
	handshakeResponsePkt, err := wire.DecodePayload(ctx, logger, firstPkt, clientConn, decodeCtx)
	if err != nil {
		return fmt.Errorf("failed to decode post-TLS HandshakeResponse41: %w", err)
	}

	// Mirror handleInitialHandshake: after decoding the client response,
	// populate ClientCaps so DeprecateEOF() short-circuits the EOF read
	// in the TextResultSet/BinaryProtocol handlers. Without this the
	// recorder aborts on the first SELECT response for modern clients
	// that negotiate CLIENT_DEPRECATE_EOF.
	decodeCtx.ClientCaps = decodeCtx.ClientCapabilities

	requests = append(requests, mysql.Request{PacketBundle: *handshakeResponsePkt})

	// Prepend server greeting to responses.
	responses = append(responses, mysql.Response{PacketBundle: *greetingPkt})

	// 4. Handle the auth exchange (same flow as the normal handshake after TLS).
	//    Read auth response from server (OK/AuthSwitch/AuthMoreData).
	authData, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
	if err != nil {
		return fmt.Errorf("failed to read post-TLS auth response: %w", err)
	}
	authDecider, err := wire.DecodePayload(ctx, logger, authData, clientConn, decodeCtx)
	if err != nil {
		return fmt.Errorf("failed to decode post-TLS auth response: %w", err)
	}

	// Handle AuthSwitchRequest if needed.
	if _, isSwitch := authDecider.Message.(*mysql.AuthSwitchRequestPacket); isSwitch {
		responses = append(responses, mysql.Response{PacketBundle: *authDecider})
		pkt := authDecider.Message.(*mysql.AuthSwitchRequestPacket)
		decodeCtx.PluginName = pkt.PluginName

		switchResp, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
		if err != nil {
			return fmt.Errorf("failed to read post-TLS auth switch response: %w", err)
		}
		switchPkt, err := mysqlUtils.BytesToMySQLPacket(switchResp)
		if err != nil {
			return fmt.Errorf("failed to parse post-TLS auth switch response: %w", err)
		}
		requests = append(requests, mysql.Request{
			PacketBundle: mysql.PacketBundle{
				Header:  &mysql.PacketInfo{Header: &switchPkt.Header, Type: mysql.AuthSwithResponse},
				Message: intgUtils.EncodeBase64(switchPkt.Payload),
			},
		})

		// Read next auth response after switch.
		authData, err = mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
		if err != nil {
			return fmt.Errorf("failed to read post-TLS auth data after switch: %w", err)
		}
		authDecider, err = wire.DecodePayload(ctx, logger, authData, clientConn, decodeCtx)
		if err != nil {
			return fmt.Errorf("failed to decode post-TLS auth data after switch: %w", err)
		}
	}

	// Handle AuthMoreData or OK.
	authRes, err := handleAuth(ctx, logger, authDecider, clientConn, destConn, decodeCtx)
	if err != nil {
		return fmt.Errorf("failed to handle post-TLS auth: %w", err)
	}
	requests = append(requests, authRes.req...)
	responses = append(responses, authRes.resp...)

	// Record the combined config mock (SSLRequest + HandshakeResponse41 + auth).
	reqOp := handshakeResponsePkt.Header.Type
	if len(requests) > 0 && requests[0].Header != nil {
		reqOp = requests[0].Header.Type // Use SSLRequest type if present
	}
	recordMock(ctx, requests, responses, "config",
		reqOp, authRes.responseOperation,
		mocks, entry.ReqTimestamp, time.Now(), opts)

	logger.Debug("Post-TLS MySQL: auth exchange recorded, proceeding to command phase")

	// 5. Handle command phase.
	return handleClientQueries(ctx, logger, clientConn, destConn, mocks, decodeCtx, opts)
}

// recordSyntheticConfigMock produces a complete config mock from pre-TLS
// handshake data when the post-TLS uprobe only sees command-phase packets
// (seq=0). It synthesizes a HandshakeResponse41 from the SSLRequest fields
// and fabricates fast-auth success responses so that test mode can replay
// the initial handshake.
func recordSyntheticConfigMock(ctx context.Context, logger *zap.Logger, clientConn net.Conn, mocks chan<- *models.Mock, decodeCtx *wire.DecodeContext, greetingPkt *mysql.PacketBundle, entry models.TLSHandshakeEntry, opts models.OutgoingOptions) error {
	// Decode the stored SSLRequest.
	sslReqPkt, err := wire.DecodePayload(ctx, logger, entry.ReqPackets[0], clientConn, decodeCtx)
	if err != nil {
		return fmt.Errorf("failed to decode stored SSLRequest: %w", err)
	}
	sslReq, ok := sslReqPkt.Message.(*mysql.SSLRequestPacket)
	if !ok {
		return fmt.Errorf("stored packet is not SSLRequest, got %T", sslReqPkt.Message)
	}

	// Synthesize a HandshakeResponse41 from the SSLRequest fields.
	// Username/Database are not present in SSLRequest and remain empty; matcher
	// handles empty expected username/database as backward-compatible wildcards.
	syntheticHR41 := &mysql.HandshakeResponse41Packet{
		CapabilityFlags: sslReq.CapabilityFlags,
		MaxPacketSize:   sslReq.MaxPacketSize,
		CharacterSet:    sslReq.CharacterSet,
		Filler:          sslReq.Filler,
		AuthPluginName:  decodeCtx.PluginName,
	}
	hr41Payload, err := connPhase.EncodeHandshakeResponse41(ctx, logger, syntheticHR41)
	if err != nil {
		return fmt.Errorf("failed to encode synthetic HandshakeResponse41: %w", err)
	}

	authMorePacket := &mysql.AuthMoreDataPacket{
		StatusTag: mysql.AuthMoreData,
		Data:      "FastAuthSuccess",
	}
	authMorePayload, err := connPhase.EncodeAuthMoreData(ctx, authMorePacket)
	if err != nil {
		return fmt.Errorf("failed to encode synthetic AuthMoreData: %w", err)
	}

	okPacket := &mysql.OKPacket{
		Header:      mysql.OK,
		StatusFlags: 2,
	}
	serverCaps := decodeCtx.ServerCaps
	if greeting, ok := greetingPkt.Message.(*mysql.HandshakeV10Packet); ok {
		serverCaps = greeting.CapabilityFlags
	}
	okPayload, err := phase.EncodeOk(ctx, okPacket, serverCaps)
	if err != nil {
		return fmt.Errorf("failed to encode synthetic OK packet: %w", err)
	}

	sslReqSeq := byte(1) // Default sequence for SSLRequest in the SSL handshake path.
	if sslReqPkt.Header != nil && sslReqPkt.Header.Header != nil {
		sslReqSeq = sslReqPkt.Header.Header.SequenceID
	}
	hr41Seq := sslReqSeq + 1
	authMoreSeq := hr41Seq + 1
	okSeq := authMoreSeq + 1

	hr41Bundle := mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &mysql.Header{PayloadLength: uint32(len(hr41Payload)), SequenceID: hr41Seq},
			Type:   mysql.HandshakeResponse41,
		},
		Message: syntheticHR41,
	}

	// Synthesize auth responses: AuthMoreData(FastAuthSuccess) + OK.
	authMoreBundle := mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &mysql.Header{PayloadLength: uint32(len(authMorePayload)), SequenceID: authMoreSeq},
			Type:   mysql.AuthStatusToString(mysql.AuthMoreData),
		},
		Message: authMorePacket,
	}
	okBundle := mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &mysql.Header{PayloadLength: uint32(len(okPayload)), SequenceID: okSeq},
			Type:   mysql.StatusToString(mysql.OK),
		},
		Message: okPacket,
	}

	requests := []mysql.Request{
		{PacketBundle: *sslReqPkt},
		{PacketBundle: hr41Bundle},
	}
	responses := []mysql.Response{
		{PacketBundle: *greetingPkt},
		{PacketBundle: authMoreBundle},
		{PacketBundle: okBundle},
	}

	recordMock(ctx, requests, responses, "config",
		sslReqPkt.Header.Type, mysql.StatusToString(mysql.OK),
		mocks, entry.ReqTimestamp, time.Now(), opts)

	logger.Debug("Post-TLS MySQL: recorded synthetic config mock for seq=0 path")
	return nil
}

// fetchServerGreeting connects to the MySQL server directly and reads the
// initial HandshakeV10 greeting packet. This is used as a fallback when the
// pre-TLS handshake was not captured by the proxy (e.g. the connection was
// established before interception started).
func fetchServerGreeting(ctx context.Context, opts models.OutgoingOptions) ([]byte, error) {
	addr := ""
	if opts.DstCfg != nil {
		addr = opts.DstCfg.Addr
	}
	if addr == "" {
		return nil, fmt.Errorf("no destination address available to fetch server greeting")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	dialer := &net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()
	cancelRead := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetReadDeadline(time.Now())
		case <-cancelRead:
		}
	}()
	defer close(cancelRead)

	// MySQL server sends greeting immediately upon connection.
	readDeadline := time.Now().Add(3 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(readDeadline) {
		readDeadline = d
	}
	if err := conn.SetReadDeadline(readDeadline); err != nil {
		return nil, fmt.Errorf("set read deadline: %w", err)
	}
	// Read the 4-byte MySQL packet header first.
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("read greeting header: %w", err)
	}
	payloadLen := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(conn, payload); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("read greeting payload: %w", err)
	}

	buf := make([]byte, 4+payloadLen)
	copy(buf, header)
	copy(buf[4:], payload)
	return buf, nil
}
