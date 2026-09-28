package recorder

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"sync"
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
	"golang.org/x/sync/singleflight"
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

	// Decode server handshake packet
	handshakePkt, err := wire.DecodePayload(ctx, logger, handshake, clientConn, decodeCtx)
	if err != nil {
		// A mid-stream join (first captured server packet is not the greeting)
		// is an expected condition the caller skips gracefully — don't log it
		// as an error. Any other decode failure is a genuine error.
		if !errors.Is(err, wire.ErrServerGreetingNotFound) {
			utils.LogError(logger, err, "failed to decode handshake packet")
		}
		return res, err
	}

	// Set the intial request operation
	res.requestOperation = handshakePkt.Header.Type

	// Store server capabilities for CLIENT_DEPRECATE_EOF handling during query phase
	if greeting, ok := handshakePkt.Message.(*mysql.HandshakeV10Packet); ok {
		decodeCtx.ServerCaps = greeting.CapabilityFlags
		// A live greeting is the freshest word on what this server is, for any
		// later connection to it that has to borrow one (see
		// fetchServerGreetingShared). No-op without a handshake store.
		if hasHandshakeStore(ctx) {
			rememberServerGreeting(ctx, opts, handshake, greetingServerIdentity(greeting))
		}
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

	// Stamp reqTimestamp from the actual handshake-response arrival.
	// CapturedReqTime tracks the most recent request chunk on this
	// connection, so reading it here pins the timestamp to the wire
	// arrival of the client's first packet rather than to whatever
	// stale value was carried forward from a prior request.
	res.reqTimestamp = models.CapturedReqTime(ctx)

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
			hsEntry := models.TLSHandshakeEntry{
				RespPackets:  [][]byte{handshake},
				ReqPackets:   [][]byte{handshakeResponse},
				ReqTimestamp: res.reqTimestamp,
			}
			// Once, under the destination port's key, tagged with this
			// connection: the decrypted leg of the same connection pops it by
			// that identity (see models.HandshakeOwner).
			portKey := models.HandshakeStoreKey("", dstPort)
			hsStore.PushFor(portKey, models.HandshakeOwnerOf(opts), hsEntry)
			logger.Debug("Pushed MySQL server greeting + SSLRequest to TLSHandshakeStore",
				zap.String("key", portKey),
				zap.String("connKey", opts.ConnKey),
				zap.String("connProc", opts.ConnProc),
				zap.Uint16("dstPort", dstPort))
			// Record this greeting as the destination's last-resort fallback for
			// a sibling connection whose own raw leg was dropped. Keyed by the
			// resolved destination so it can only be reused for the SAME server.
			// Note it is NOT skipped for a synthesized address: HandshakeLastKey
			// combines the address with the app/session scope, so a placeholder
			// bucket is still per-scope (see its doc).
			hsStore.RememberLast(lastGreetingKey(opts.PassThroughScope, opts.NetNS, opts.DstCfg), hsEntry)
			// Signal that the pre-TLS config mock should NOT be recorded here;
			// the post-TLS path will produce a single combined config mock.
			res.skipConfigMock = true
			return res, nil
		}

		if tlsUpgrader == nil {
			logger.Debug("TLS upgrade requested but no TLSUpgrader available (non-MITM path)")
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

			// dstURL is the client's own SNI and is empty for every
			// IP-dialled MySQL — see resolveDestServerName for why that
			// blocks verification and what it falls back to.
			serverName := resolveDestServerName(dstURL, destConn, opts.UpstreamTLSVerify)

			tlsConfig := &tls.Config{
				// Off by default: keploy must never be stricter than the app
				// it records. A MySQL client on tls=skip-verify chose not to
				// authenticate its upstream, and a failure here is silent —
				// the handshake error trips the supervisor's passthrough
				// fallback, the app keeps working and the mock is DROPPED.
				// Opt in with record.upstreamTls.verify. Not a CA-bundle
				// limitation: crypto/tls uses the platform root pool when
				// RootCAs is nil.
				InsecureSkipVerify: !opts.UpstreamTLSVerify, //nolint:gosec
				RootCAs:            opts.UpstreamTLSRootCAs,
				ServerName:         serverName,
				KeyLogWriter:       pTls.KeyLogWriter(),
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

	res.resTimestamp = models.CapturedRespTime(ctx)

	setHandshakeResult(&res, authRes)

	return res, nil
}

// resolveDestServerName decides the ServerName keploy puts on its own TLS
// dial to the real MySQL server.
//
// capturedSNI is the SNI CertForClient recovered from the client's
// ClientHello (pTls.SrcPortToDstURL). SrcPortToDstURL stores it
// unconditionally — the empty string included — and MySQL clients
// overwhelmingly dial by IP (keploy's own e2e uses tcp(127.0.0.1:3306)),
// while RFC 6066 forbids IP literals in SNI so the client sends none. Empty
// is therefore the NORMAL case here, not an edge case, and this site has
// never had any fallback at all.
//
// That is fatal the moment verification is on: crypto/tls rejects a config
// with an empty ServerName and InsecureSkipVerify=false outright ("either
// ServerName or InsecureSkipVerify must be specified") before it examines any
// certificate, so record.upstreamTls.verify would be unusable against every
// IP-dialled MySQL. Falling back to the peer keploy is already connected to
// fixes that; an IP literal is the RIGHT value, Go matches it against the
// certificate's IP SANs.
//
// Scoped to the verifying path on purpose: with verification off, ServerName
// only feeds the SNI extension, so filling it in would put an SNI on the wire
// that the application itself never sent. The default must stay byte-identical.
func resolveDestServerName(capturedSNI string, destConn net.Conn, verify bool) string {
	if capturedSNI != "" || !verify {
		return capturedSNI
	}
	if destConn == nil {
		return ""
	}
	addr := destConn.RemoteAddr()
	if addr == nil {
		return ""
	}
	// Addresses that carry no port (unix sockets, anything SplitHostPort
	// rejects) are used verbatim, matching proxy.hostFromAddr.
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
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

// takeLegacyPostTLSGreeting takes the stashed pre-TLS greeting for the legacy
// post-TLS reader, as resolvePreTLSGreeting does on V2: this connection's own
// entry (models.HandshakeOwner) when the capture layer can tell which that is.
// fromShared reports an entry that is not provably its own, only one it may
// take; taker is the owner that entry was pushed with.
//
// A connection that joined mid-stream does not wait, and takes only its own
// entry: the caller then falls back to the app-scoped cache and the per-server
// memo (see resolvePreTLSGreeting). A fresh connection waits for its entry.
func takeLegacyPostTLSGreeting(hsStore *models.TLSHandshakeStore, owner models.HandshakeOwner, dstPort uint16, joinedMidStream bool) (entry models.TLSHandshakeEntry, taker models.HandshakeOwner, ok, fromShared bool) {
	portKey := models.HandshakeStoreKey("", dstPort)
	if joinedMidStream {
		if owner.Conn == "" {
			return models.TLSHandshakeEntry{}, models.HandshakeOwner{}, false, false
		}
		entry, taker, ok = hsStore.PopWaitFor(portKey, owner, true, 0)
		return entry, taker, ok, false
	}
	entry, taker, ok = hsStore.PopWaitFor(portKey, owner, false, 5*time.Second)
	return entry, taker, ok, ok && !owner.Is(taker)
}

// handlePostTLSRecord handles MySQL recording for the post-TLS uprobe path.
// In this mode, the SSL/GoTLS uprobes provide decrypted plaintext starting
// from HandshakeResponse41 (the full auth after TLS handshake). The server
// greeting was captured by the ringbuf path and stored in TLSHandshakeStore.
func handlePostTLSRecord(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, mocks chan<- *models.Mock, decodeCtx *wire.DecodeContext, opts models.OutgoingOptions) (err error) {
	// 1. Pop the server greeting from TLSHandshakeStore.
	dstPort := uint16(0)
	if opts.DstCfg != nil {
		dstPort = uint16(opts.DstCfg.Port)
	}
	hsStore, _ := ctx.Value(models.TLSHandshakeStoreKey).(*models.TLSHandshakeStore)
	if hsStore == nil {
		return fmt.Errorf("TLSHandshakeStore not available in context for post-TLS MySQL recording")
	}
	// The raw leg pushed its greeting under the destination port's key, tagged
	// with its connection (models.HandshakeOwner).
	storeKey := models.HandshakeStoreKey("", dstPort)

	// Read the first packet from the client BEFORE looking for a greeting: it
	// decides whether waiting for one can achieve anything. The client's bytes
	// are buffered, so reading them first costs nothing.
	//
	// HandshakeResponse41 has sequence number >= 1 (it follows the server
	// greeting at seq 0 and the SSLRequest at seq 1). A command-phase packet has
	// seq 0: every command starts a new sequence. So seq==0 on the FIRST
	// decrypted packet means this TLS session was authenticated before capture
	// of its decrypted bytes began: a pooled connection opened before the
	// recording. Nothing this stream needs can arrive by waiting: it has no
	// auth exchange of its own here, its config mock is synthesized from any
	// SSLRequest of the same client, and any greeting OF ITS SERVER decodes its
	// commands. So it takes what is already there and never waits (see
	// resolvePreTLSGreeting for the full argument). Waiting cost every pooled
	// connection 5s here, and 30s on the V2 path, at every recording start.
	firstPkt, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		return fmt.Errorf("failed to read first post-TLS client packet: %w", err)
	}
	if len(firstPkt) < 4 {
		return fmt.Errorf("first post-TLS packet too short (%d bytes)", len(firstPkt))
	}
	joinedMidStream := firstPkt[3] == 0

	logger.Debug("Post-TLS MySQL: popping from TLSHandshakeStore",
		zap.String("key", storeKey),
		zap.String("connKey", opts.ConnKey),
		zap.String("connProc", opts.ConnProc),
		zap.Uint16("dstPort", dstPort),
		zap.Bool("joinedMidStream", joinedMidStream))
	entry, taker, ok, fromShared := takeLegacyPostTLSGreeting(hsStore, models.HandshakeOwnerOf(opts), dstPort, joinedMidStream)
	// Taking an entry that is not provably this connection's is destructive: if
	// this stream fails before recording anything, the greeting it consumed is
	// gone and the connection that pushed it is starved, losing its whole
	// command phase silently. Put an unused entry back, with the owner it was
	// pushed with, so its connection can still find it. ReqTimestamp is stripped for the same reason the cached
	// fallback below strips it — to the next consumer this is a BORROWED entry
	// from another connection, and config mocks are identified and ordered by
	// ReqTimestampMock. Mirrors handlePostTLSHandshakeV2 on the V2 path.
	// Cleared at the greeting-decode failures below: an undecodable entry is
	// garbage, and Push re-stamps its expiry, so recycling it would make it
	// immortal and trip every later stream to this port.
	restoreShared := true
	if fromShared {
		sharedEntry := entry
		sharedEntry.ReqTimestamp = time.Time{}
		defer func() {
			if !restoreShared || err == nil {
				return
			}
			hsStore.PushFor(storeKey, taker, sharedEntry)
			logger.Debug("post-TLS MySQL: returned an unused shared greeting to the port FIFO",
				zap.Uint16("dstPort", dstPort), zap.Error(err))
		}()
	}
	// No stashed entry was taken. Before dialling the server, consult the
	// last-greeting cache — the same fallback the V2 path uses, and the reason
	// handleInitialHandshake seeds it above. Without this read that seeding is a
	// write with no reader, and a POOLED connection (reused long after its
	// handshake, so its own entry was already consumed) loses its whole command
	// phase here exactly as it did on V2.
	//
	// Address-keyed only. The V2 path additionally bridges legs that disagree
	// about the address via a port key, but that key names no server and is only
	// safe with the identity latch that RememberLastForPort applies; this path
	// writes no port key, so there is nothing here that could answer unsafely.
	if !ok || len(entry.RespPackets) == 0 {
		if lastKey := lastGreetingKey(opts.PassThroughScope, opts.NetNS, opts.DstCfg); lastKey != "" {
			if c, found := hsStore.Last(lastKey); found && len(c.RespPackets) > 0 {
				logger.Debug("Post-TLS MySQL: own handshake entry gone; reusing the last greeting recorded for this destination",
					zap.String("lastKey", lastKey))
				// Reuse the PACKETS, not the timing. The store's own contract
				// says so (TLSHandshakeStore.Last: "reuse the server-stable
				// parts ... but not per-connection metadata such as the request
				// timestamp"), and this was the only in-tree violator: both
				// config-mock emitters below pass entry.ReqTimestamp straight to
				// recordMock, so adopting a borrowed one backdates this mock by
				// up to lastGreetingTTL (30 min).
				//
				// That matters twice over. recordMock stamps a "config" mock as
				// LifetimeSession, so it skips the per-test window entirely —
				// but pkg/util.go still sorts the session pool by
				// ReqTimestampMock, and treedb.sameMock IDENTIFIES a mock by
				// Name+Kind+ReqTimestampMock. Every pooled connection borrowing
				// the same cached entry would emit mutually indistinguishable
				// config mocks, and the guard that stops UpdateUnFilteredMock
				// touching the wrong entry silently abstains.
				//
				// The V2 path draws the same line (record_v2.go, staleEntry).
				c.ReqTimestamp = models.CapturedReqTime(ctx)
				entry, ok = c, true
			}
		}
	}

	// A zero ReqTimestamp is worse than a stale one: pkg/util.go routes any
	// mock with a zero request OR response timestamp into filteredMocks with
	// IsFiltered=true and stops there — ABOVE the lifetime-first routing — so
	// a LifetimeSession config mock lands in the per-test pool. That happens
	// whenever both PopWaits and the cache miss and the greeting comes from
	// the direct fetch below, which never sets a timestamp. V2 handles the
	// same case by re-sampling; normalise here so neither emitter can ship a
	// zero.
	if entry.ReqTimestamp.IsZero() {
		entry.ReqTimestamp = models.CapturedReqTime(ctx)
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
		serverGreetingBuf, err = fetchServerGreetingShared(ctx, logger, hsStore, opts)
		if err != nil {
			return fmt.Errorf("no server greeting in TLSHandshakeStore for key %s and direct fetch failed: %w", storeKey, err)
		}
	}

	// 2. Decode the server greeting to initialize decode context.
	greetingPkt, err := wire.DecodePayload(ctx, logger, serverGreetingBuf, clientConn, decodeCtx)
	if err != nil {
		restoreShared = false
		return fmt.Errorf("failed to decode stored server greeting for post-TLS: %w", err)
	}
	pluginName, err := wire.GetPluginName(greetingPkt.Message)
	if err != nil {
		// Decodable but not a handshake (GetPluginName accepts only HandshakeV10
		// / AuthSwitchRequest). This runs BEFORE the HandshakeV10 assertion
		// below, so without clearing the flag here such an entry would be
		// recycled — and Push re-stamps its expiry, making it immortal and
		// tripping every later stream to this port.
		restoreShared = false
		return fmt.Errorf("failed to get plugin name from stored server greeting: %w", err)
	}
	decodeCtx.PluginName = pluginName
	decodeCtx.UseSSL = true

	// Store the server greeting for the clientConn (needed by DecodePayload for command phase).
	sg, ok := greetingPkt.Message.(*mysql.HandshakeV10Packet)
	if !ok {
		restoreShared = false
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

	// 3. The first client packet, read above, decides whether this is a fresh
	//    connection (HandshakeResponse41) or an existing connection already in
	//    command phase (COM_QUERY, COM_STMT_*, etc.).
	if joinedMidStream {
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
			// Spent: a mock is about to be built from this greeting. Past this
			// point the entry must NOT go back — handleClientQueries below
			// returns non-nil at ordinary teardown (ctx.Done), so the error
			// alone cannot distinguish "never used" from "used and then the
			// connection ended".
			restoreShared = false
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
	// Spent, for the same reason as the synthetic-config path above.
	restoreShared = false
	recordMock(ctx, requests, responses, "config",
		reqOp, authRes.responseOperation,
		mocks, entry.ReqTimestamp, models.CapturedRespTime(ctx), opts)

	logger.Debug("Post-TLS MySQL: auth exchange recorded, proceeding to command phase")

	// 5. Handle command phase.
	return handleClientQueries(ctx, logger, clientConn, destConn, mocks, decodeCtx, opts)
}

// recordSyntheticConfigMock produces a complete config mock from pre-TLS
// handshake data when the post-TLS uprobe only sees command-phase packets
// (seq=0). It synthesizes a HandshakeResponse41 from the SSLRequest fields
// and fabricates fast-auth success responses so that test mode can replay
// the initial handshake.
// buildSyntheticPostTLSConfig synthesizes the config-mock request/response
// bundles for a post-TLS connection joined mid-stream (seq==0), where no real
// HandshakeResponse41 / auth exchange was captured (the connection was already
// authenticated before interception). It reconstructs a HandshakeResponse41
// from the stored SSLRequest's fields, plus AuthMoreData(FastAuthSuccess) + OK,
// so the config mock carries an HR41 at requests[1] — which the replayer
// REQUIRES to match the connection at test time (see replayer/conn.go: it
// looks for HandshakeResponse41 at requests[0] or [1] and errors otherwise).
// sslReqPkt must already be decoded. Shared by the legacy recorder
// (recordSyntheticConfigMock) and the V2 post-TLS handshake so both emit an
// identical, replay-compatible seq==0 config mock.
func buildSyntheticPostTLSConfig(ctx context.Context, logger *zap.Logger, decodeCtx *wire.DecodeContext, greetingPkt, sslReqPkt *mysql.PacketBundle) ([]mysql.Request, []mysql.Response, string, error) {
	sslReq, ok := sslReqPkt.Message.(*mysql.SSLRequestPacket)
	if !ok {
		return nil, nil, "", fmt.Errorf("stored packet is not SSLRequest, got %T", sslReqPkt.Message)
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
		return nil, nil, "", fmt.Errorf("failed to encode synthetic HandshakeResponse41: %w", err)
	}

	authMorePacket := &mysql.AuthMoreDataPacket{
		StatusTag: mysql.AuthMoreData,
		Data:      "FastAuthSuccess",
	}
	authMorePayload, err := connPhase.EncodeAuthMoreData(ctx, authMorePacket)
	if err != nil {
		return nil, nil, "", fmt.Errorf("failed to encode synthetic AuthMoreData: %w", err)
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
		return nil, nil, "", fmt.Errorf("failed to encode synthetic OK packet: %w", err)
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
	return requests, responses, mysql.StatusToString(mysql.OK), nil
}

func recordSyntheticConfigMock(ctx context.Context, logger *zap.Logger, clientConn net.Conn, mocks chan<- *models.Mock, decodeCtx *wire.DecodeContext, greetingPkt *mysql.PacketBundle, entry models.TLSHandshakeEntry, opts models.OutgoingOptions) error {
	// Decode the stored SSLRequest, then build the synthetic HR41 + auth bundles.
	sslReqPkt, err := wire.DecodePayload(ctx, logger, entry.ReqPackets[0], clientConn, decodeCtx)
	if err != nil {
		return fmt.Errorf("failed to decode stored SSLRequest: %w", err)
	}
	requests, responses, respOp, err := buildSyntheticPostTLSConfig(ctx, logger, decodeCtx, greetingPkt, sslReqPkt)
	if err != nil {
		return err
	}

	recordMock(ctx, requests, responses, "config",
		sslReqPkt.Header.Type, respOp,
		mocks, entry.ReqTimestamp, models.CapturedRespTime(ctx), opts)

	logger.Debug("Post-TLS MySQL: recorded synthetic config mock for seq=0 path")
	return nil
}

// fetchGreetingDialTimeout bounds each dial fetchServerGreeting makes.
const fetchGreetingDialTimeout = 3 * time.Second

// fetchServerGreeting connects to the MySQL server directly and reads the
// initial HandshakeV10 greeting packet. This is used as a fallback when the
// pre-TLS handshake was not captured by the proxy (e.g. the connection was
// established before interception started).
func fetchServerGreeting(ctx context.Context, logger *zap.Logger, opts models.OutgoingOptions) ([]byte, error) {
	addr := ""
	if opts.DstCfg != nil {
		addr = opts.DstCfg.Addr
	}
	if addr == "" {
		return nil, fmt.Errorf("no destination address available to fetch server greeting")
	}
	// Never dial an address the capture layer fabricated. When the proxyless
	// SSL-uprobe path cannot resolve a decrypted stream's real destination it
	// substitutes a loopback stand-in (and content matching later forces the
	// well-known port), yielding e.g. "127.0.0.1:3306" — an address this
	// connection never talked to. Dialing it either fails instantly
	// (ECONNREFUSED, aborting the capture) or, worse, fetches a greeting from
	// an unrelated local server. Fail here so callers fall through to their
	// stash/cache fallbacks or abort cleanly.
	if opts.DstCfg != nil && opts.DstCfg.AddrFabricated {
		return nil, fmt.Errorf("destination address %s is a capture-layer stand-in (real destination unresolved), refusing to dial it to fetch the server greeting", addr)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	dialer := &net.Dialer{Timeout: fetchGreetingDialTimeout}
	dial := func(ctx context.Context, a string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", a)
	}
	// A connection made from another network namespace to a namespace-local
	// address (a pod's 127.0.0.1) is a different server from keploy's own
	// namespace. The capture layer then supplies a dialer that connects from
	// the connection's namespace; without one the address is dialled here as
	// before.
	if nsDial := models.DstDialerFrom(ctx); nsDial != nil {
		dial = func(ctx context.Context, a string) (net.Conn, error) {
			dctx, cancel := context.WithTimeout(ctx, fetchGreetingDialTimeout)
			defer cancel()
			return nsDial(dctx, a)
		}
	}
	conn, err := pUtils.DialDestinationWith(ctx, logger, pUtils.DialTarget{Addr: addr}, dial)
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

// greetingFetches coalesces concurrent direct greeting fetches for the same
// store and destination key (see fetchServerGreetingShared).
var greetingFetches singleflight.Group

// greetingFetchFailureBackoff is how long a failed direct fetch is answered
// from memory instead of dialled again. A variable so tests can shorten it.
var greetingFetchFailureBackoff = 15 * time.Second

// greetingFetchFailures remembers the most recent failed fetch per flight key
// for greetingFetchFailureBackoff. Values are greetingFetchFailure.
var greetingFetchFailures sync.Map

type greetingFetchFailure struct {
	err error
	at  time.Time
}

// greetingFetchResult is what one flight hands every caller sharing it.
type greetingFetchResult struct {
	buf        []byte
	fromCache  bool // served by an entry remembered before this flight started
	remembered bool // this flight's fetch was remembered for later connections
}

// fetchServerGreetingShared is how the post-TLS paths fall back to dialling the
// server for its greeting. Use it instead of calling fetchServerGreeting
// directly: it dials at most ONCE per server (per greetingMemoKey), not once
// per connection, per app or per recording session.
//
// Every dial reads the greeting and hangs up before authenticating, and MySQL
// counts that as an aborted handshake from the dialling host. Enough of them
// (max_connect_errors, default 100) blocks the host. When the dial comes from a
// node's network (a DaemonSet agent), pods reaching the server through SNAT
// share that host, so the whole node loses its database. Before the capture
// layer reported real destinations, these dials hit the pod's own IP and failed
// instantly. Now they reach the server, and a pool of pre-recording connections
// with nothing captured dialled once per connection at every recording start,
// each costing up to 3s dial + 2s loopback-fallback dial + 3s read when a
// firewall drops it.
//
// So:
//   - A greeting is remembered PER SERVER (models.HandshakeServerKey): a
//     server's greeting belongs to the server, not to the app or the recording
//     session that happened to need it. The key carries no app/session scope
//     for a routable address, so the next session, or another app on the same
//     node, reuses it. A namespace-local address (127.0.0.1, ::1, link-local)
//     names a different server in every pod, so its key is qualified by the
//     connection's network namespace (OutgoingOptions.NetNS). The entry holds
//     the greeting bytes only: no SSLRequest (none was captured), no timing.
//     It lives as long as a live greeting does (lastGreetingTTL), a greeting a
//     raw leg captures replaces it (rememberServerGreeting), and a fetch never
//     replaces one a raw leg recorded meanwhile.
//   - Concurrent callers for the same store and key share ONE in-flight dial.
//     The dial is detached from the calling connection's cancellation, so one
//     stream's teardown does not fail the others waiting on it. It stays bounded
//     by fetchServerGreeting's own dial and read limits. Each caller stops
//     waiting when its own ctx ends, and a caller whose ctx has already ended
//     never STARTS a dial: recording stop is not a reason to reach the server.
//   - A failed fetch caches nothing as a success. Callers already waiting share
//     its error, and callers arriving within greetingFetchFailureBackoff get
//     that error without dialling again. After that the next caller dials. A
//     reply that is not a HandshakeV10 greeting (for example the ERR packet
//     MySQL sends a blocked host) counts as a failure. A dialer that could not
//     reach its own connection's namespace (models.ErrDstDialerUnavailable)
//     does not: that is about one caller, and the others still dial.
//   - Nothing is remembered without an address: never under a port, and never
//     for a fabricated destination, which fetchServerGreeting refuses to dial
//     in the first place.
//
// The flight runs on its own goroutine, where singleflight re-panics any panic
// out of reach of every recover, so it recovers its own and fails instead.
// It also does not log; its callers do, with their own loggers.
func fetchServerGreetingShared(ctx context.Context, logger *zap.Logger, hsStore *models.TLSHandshakeStore, opts models.OutgoingOptions) ([]byte, error) {
	key := greetingMemoKey(opts)
	if hsStore == nil || key == "" {
		// Nothing to remember under and nothing to coalesce on: no store, no
		// address, or a stand-in address (which fetchServerGreeting refuses).
		return fetchServerGreeting(ctx, logger, opts)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The store pointer is part of the key: the server key alone is not unique
	// across stores (each has its own cache), and a flight must remember into
	// the store its callers read.
	flight := fmt.Sprintf("%p|%s", hsStore, key)
	// Same order as inside the flight: a remembered greeting (for instance one a
	// raw leg recorded since the last failure) beats a remembered failure.
	if g, ok := hsStore.ServerGreeting(key); ok {
		logger.Debug("post-TLS MySQL: using the greeting remembered for this server; not dialling",
			zap.String("serverKey", key))
		return g, nil
	}
	if err := recentGreetingFetchFailure(flight); err != nil {
		logger.Debug("post-TLS MySQL: not dialling the server for its greeting again; a fetch failed moments ago",
			zap.String("serverKey", key), zap.Error(err))
		return nil, err
	}
	dialLogger := logger
	// A flight dials with its LEADER's dialer (its ctx). When that dialer could
	// not reach its own connection's namespace (models.ErrDstDialerUnavailable),
	// the failure is the leader's alone: it is not remembered for the server,
	// and a caller that merely shared the flight tries once more, with a flight
	// of its own if no one else has started one.
	for attempt := 0; ; attempt++ {
		led := false // set only if this caller's function runs the flight
		ch := greetingFetches.DoChan(flight, func() (res interface{}, err error) {
			led = true
			dialled := false
			defer func() {
				if r := recover(); r != nil {
					res, err = nil, fmt.Errorf("greeting fetch from %s panicked: %v\n%s", opts.DstCfg.Addr, r, debug.Stack())
					dialled = true
				}
				if !dialled {
					return
				}
				switch {
				case err == nil:
					greetingFetchFailures.Delete(flight)
				case errors.Is(err, models.ErrDstDialerUnavailable):
					// This caller's namespace, not the server: remember nothing.
				default:
					greetingFetchFailures.Store(flight, greetingFetchFailure{err: err, at: time.Now()})
					pruneGreetingFetchFailures()
				}
			}()
			// A flight that finished just before this one started may already have
			// remembered the greeting (so may a raw leg), or may have failed: a
			// caller can pass the check above just as that flight ends. Either way,
			// no dial.
			if g, ok := hsStore.ServerGreeting(key); ok {
				return greetingFetchResult{buf: g, fromCache: true}, nil
			}
			if err := recentGreetingFetchFailure(flight); err != nil {
				return nil, err
			}
			dialled = true
			buf, err := fetchServerGreeting(context.WithoutCancel(ctx), dialLogger, opts)
			if err != nil {
				return nil, err
			}
			if err := validateGreeting(buf); err != nil {
				return nil, fmt.Errorf("server at %s: %w", opts.DstCfg.Addr, err)
			}
			remembered := hsStore.RememberServerGreetingIfAbsent(key, buf)
			return greetingFetchResult{buf: buf, remembered: remembered}, nil
		})
		select {
		case r := <-ch:
			if r.Err != nil {
				if !led && attempt == 0 && errors.Is(r.Err, models.ErrDstDialerUnavailable) {
					logger.Debug("post-TLS MySQL: the shared greeting fetch could not dial from its own connection's namespace; trying with this connection's",
						zap.String("serverKey", key), zap.Error(r.Err))
					continue
				}
				return nil, r.Err
			}
			res, _ := r.Val.(greetingFetchResult)
			switch {
			case res.fromCache:
				logger.Debug("post-TLS MySQL: greeting was remembered by an earlier connection; not dialling",
					zap.String("serverKey", key))
			case r.Shared:
				logger.Debug("post-TLS MySQL: shared another connection's greeting fetch instead of dialling",
					zap.String("serverKey", key))
			case res.remembered:
				logger.Debug("post-TLS MySQL: remembered a directly fetched greeting for later connections to this server",
					zap.String("serverKey", key))
			}
			return res.buf, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// lastGreetingKey is the key of the app-scoped last-greeting cache
// (models.HandshakeLastKey) for a connection, or "" for none.
//
// That key combines the app/session scope with the destination address, and
// the scope is shared by every replica of a deployment. For a namespace-local
// address the capture layer observed (not a stand-in), each replica reaches its
// OWN server there (a sidecar on 127.0.0.1), so the key also carries the
// connection's network namespace, and without one there is no key: nothing is
// shared, as for the per-server memo (models.HandshakeServerKey). A stand-in
// address (AddrFabricated) names no server at all; its per-scope bucket is
// documented on models.HandshakeLastKey.
func lastGreetingKey(scope, netNS string, dst *models.ConditionalDstCfg) string {
	if dst != nil && !dst.AddrFabricated {
		if host, _, err := net.SplitHostPort(dst.Addr); err == nil && models.AddrIsNetnsLocal(host) {
			if netNS == "" {
				return ""
			}
			scope += "|netns=" + netNS
		}
	}
	return models.HandshakeLastKey(scope, dst)
}

// greetingMemoKey is the key a connection's server greeting is remembered
// under: the server behind its destination (models.HandshakeServerKey). "" when
// the destination names no server.
func greetingMemoKey(opts models.OutgoingOptions) string {
	return models.HandshakeServerKey(opts.NetNS, opts.DstCfg)
}

// rememberServerGreeting records a greeting a raw leg CAPTURED from the server
// on a live connection, so a later connection to the same server that needs
// one (a pooled connection whose own greeting predates the recording) is
// served without dialling, and a greeting fetched earlier is refreshed.
//
// identity is greetingServerIdentity of the caller's decoding of greeting, and
// the proof that it IS one: an ERR packet, or a mid-stream packet mistaken for a
// greeting, never decodes as a HandshakeV10 (identity is then ""), and must
// never be handed to another connection. Taking the caller's decoding spares
// every recorded handshake a second decode and a second fingerprint.
func rememberServerGreeting(ctx context.Context, opts models.OutgoingOptions, greeting []byte, identity string) {
	if identity == "" || len(greeting) < 5 || greeting[4] != mysql.HandshakeV10 {
		return
	}
	hsStore, _ := ctx.Value(models.TLSHandshakeStoreKey).(*models.TLSHandshakeStore)
	if hsStore == nil {
		return
	}
	if key := greetingMemoKey(opts); key != "" {
		hsStore.RememberServerGreeting(key, greeting, identity)
	}
}

// hasHandshakeStore reports whether ctx carries a TLSHandshakeStore, so a
// caller can skip work only a store would use.
func hasHandshakeStore(ctx context.Context) bool {
	s, _ := ctx.Value(models.TLSHandshakeStoreKey).(*models.TLSHandshakeStore)
	return s != nil
}

// recentGreetingFetchFailure returns the failure recorded for flight within
// greetingFetchFailureBackoff, or nil.
func recentGreetingFetchFailure(flight string) error {
	v, ok := greetingFetchFailures.Load(flight)
	if !ok {
		return nil
	}
	f, ok := v.(greetingFetchFailure)
	if !ok {
		return nil
	}
	ago := time.Since(f.at)
	if ago >= greetingFetchFailureBackoff {
		return nil
	}
	return fmt.Errorf("greeting fetch failed %s ago, not retrying for %s: %w",
		ago.Truncate(time.Millisecond), greetingFetchFailureBackoff, f.err)
}

// pruneGreetingFetchFailures drops failure records past their backoff so the
// map holds only live ones: one per destination that failed in the last
// greetingFetchFailureBackoff.
func pruneGreetingFetchFailures() {
	greetingFetchFailures.Range(func(k, v interface{}) bool {
		if f, ok := v.(greetingFetchFailure); !ok || time.Since(f.at) >= greetingFetchFailureBackoff {
			greetingFetchFailures.Delete(k)
		}
		return true
	})
}

// validateGreeting accepts only a HandshakeV10 greeting packet: protocol
// version 10 and a payload that decodes. Anything else, such as the ERR packet
// MySQL sends a blocked host or a truncated reply, must not be cached or reused.
func validateGreeting(buf []byte) error {
	if len(buf) < 5 {
		return fmt.Errorf("greeting reply too short (%d bytes)", len(buf))
	}
	if buf[4] != mysql.HandshakeV10 {
		return fmt.Errorf("reply is not a HandshakeV10 greeting (first payload byte 0x%02x)", buf[4])
	}
	if _, err := connPhase.DecodeHandshakeV10(context.Background(), zap.NewNop(), buf[4:]); err != nil {
		return fmt.Errorf("undecodable HandshakeV10 greeting: %w", err)
	}
	return nil
}
