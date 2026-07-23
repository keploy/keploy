package recorder

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	mysqlUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query/rowscols"
	intgUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// RecordV2 is the V2 record path for MySQL. It consumes the supervisor
// Session's FakeConns (ClientStream and DestStream) and uses the
// directive channel for the CLIENT_SSL TLS upgrade. The relay is the
// sole writer to real peers; the parser never writes.
//
// The mock output shape is semantically identical to the legacy path:
//   - A "config" mock carrying the handshake + auth exchange.
//     Recorded once at the start of the connection.
//   - A "mocks" mock per command-response pair, or a "connection" mock
//     for COM_STMT_PREPARE (prepared-statement setup is connection-
//     scoped so executes can still find it across test windows).
//
// Metadata matches the legacy path: type, requestOperation,
// responseOperation, connID, and destAddr (when present).
//
// Timestamps:
//   - ReqTimestampMock: the FakeConn's LastReadTime() captured right
//     after the command packet finished reading. This is the ReadAt
//     of the chunk that delivered the command bytes.
//   - ResTimestampMock: the FakeConn's LastReadTime() captured right
//     after the server's final response packet finished reading. This
//     is the ReadAt of the chunk that delivered the last response byte.
//
// TLS upgrade:
//   - Parser detects an SSLRequest on ClientStream (CLIENT_SSL bit set
//     AND packet body length == 32, which is the short-form SSLRequest).
//   - Records the pre-TLS prelude mock.
//   - Sends directive.UpgradeTLS on sess.Directives with dest and client
//     TLS configs derived from sess.Opts.
//   - Blocks on sess.Acks. On OK==true, subsequent chunks from
//     ClientStream/DestStream are plaintext; reads the re-sent
//     HandshakeResponse41 and continues.
//   - On OK==false, calls sess.MarkMockIncomplete and returns the err;
//     the supervisor falls through to raw passthrough.
func RecordV2(ctx context.Context, logger *zap.Logger, sess *supervisor.Session) error {
	if sess == nil {
		return errors.New("recorder: nil supervisor session")
	}
	if logger == nil {
		logger = sess.Logger
	}

	decodeCtx := &wire.DecodeContext{
		Mode:               models.MODE_RECORD,
		LastOp:             wire.NewLastOpMap(),
		ServerGreetings:    wire.NewGreetings(),
		PreparedStatements: make(map[uint32]*mysql.StmtPrepareOkPacket),
	}

	// The DecodeContext's internal maps key by net.Conn pointer. We use
	// the ClientStream FakeConn as the stable key for the duration of
	// the connection (pre- and post-TLS on the V2 path, since the relay
	// owns the real socket swap). After TLS upgrade we deliberately
	// re-key to a fresh *net.Conn sentinel so the post-TLS auth
	// exchange decodes with a clean LastOp — matching the legacy path
	// which swaps to a new TLS-wrapped conn.
	clientKey := sess.ClientStream
	var clientKeyNetConn net.Conn = clientKey
	decodeCtx.LastOp.Store(clientKeyNetConn, wire.RESET)

	// PostTLSMode marks the decrypted (SSL/GoTLS/JSSE uprobe) half of a TLS
	// connection: the pre-TLS server greeting arrived on a different, observe-
	// only stream and was stashed in TLSHandshakeStore. In that mode the greeting
	// is popped from the store instead of read from DestStream, and no relay TLS
	// upgrade is attempted (observe-only capture has no upstream socket).
	postTLS := false
	if v, ok := ctx.Value(models.PostTLSModeKey).(bool); ok && v {
		postTLS = true
	}

	handshake, err := handleInitialHandshakeV2(ctx, logger, sess, decodeCtx, &clientKeyNetConn, postTLS)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, fakeconn.ErrClosed) {
			logger.Debug("EOF during MySQL V2 initial handshake")
			return nil
		}
		// Joined mid-stream (connection opened before recording started, e.g.
		// a pre-warmed pool): the server greeting was never captured, so the
		// connection is un-decodable. Skip it gracefully rather than failing
		// the capture. See wire.ErrServerGreetingNotFound.
		if errors.Is(err, wire.ErrServerGreetingNotFound) {
			logger.Warn("skipping MySQL connection joined mid-stream: no server greeting was captured, so it cannot be recorded. This connection was opened before recording started (e.g. a pre-warmed connection pool). To capture it, restart the application after starting the recording so its connections are re-established and captured from the greeting.")
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		utils.LogError(logger, err, "V2: failed to handle initial mysql handshake")
		return err
	}

	if !handshake.skipConfigMock {
		emitMockV2(ctx, sess, handshake.req, handshake.resp, "config", handshake.requestOperation, handshake.responseOperation, handshake.reqTimestamp, handshake.resTimestamp)
	}

	if err := handleCommandsV2(ctx, logger, sess, decodeCtx, clientKeyNetConn); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, fakeconn.ErrClosed) {
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	return nil
}

// v2HandshakeResult mirrors the legacy handshakeRes shape but omits the
// TLS conn pointers (the relay owns those in V2).
type v2HandshakeResult struct {
	req               []mysql.Request
	resp              []mysql.Response
	requestOperation  string
	responseOperation string
	reqTimestamp      time.Time
	resTimestamp      time.Time
	skipConfigMock    bool
}

// handleInitialHandshakeV2 walks the MySQL connection phase on the V2
// path: server greeting → client response (possibly SSLRequest) →
// optional TLS upgrade via directive → re-sent HandshakeResponse41 →
// auth exchange. Returns the accumulated request/response bundles plus
// request/response timestamps sampled from chunk ReadAt times.
func handleInitialHandshakeV2(ctx context.Context, logger *zap.Logger, sess *supervisor.Session, decodeCtx *wire.DecodeContext, clientKeyPtr *net.Conn, postTLS bool) (v2HandshakeResult, error) {
	res := v2HandshakeResult{
		req:  make([]mysql.Request, 0),
		resp: make([]mysql.Response, 0),
	}

	clientKey := *clientKeyPtr

	// Server greeting (HandshakeV10). Pre-TLS: it arrives on DestStream. Post-TLS
	// (decrypted SSL/GoTLS/JSSE uprobe stream): it was exchanged pre-TLS on a
	// different, observe-only stream and stashed in TLSHandshakeStore, so it is
	// popped from there. This is the V2 analogue of the legacy handlePostTLSRecord
	// reconstruction. Either way `handshake` ends up holding the raw greeting.
	var handshake []byte
	var stashedSSLReq []byte
	if postTLS {
		g, sslReq, perr := popStoredGreetingV2(ctx, logger, sess)
		if perr != nil {
			return res, perr
		}
		handshake = g
		stashedSSLReq = sslReq
		// No DestStream read happened; stamp from wall clock (the greeting's
		// original arrival time is preserved on the pre-TLS config-mock shell).
		res.reqTimestamp = time.Now()
	} else {
		g, rerr := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.DestStream)
		if rerr != nil {
			return res, fmt.Errorf("read server greeting: %w", rerr)
		}
		handshake = g
		// The request timestamp of the config mock is the time the server
		// greeting arrived. Must be chunk-derived: LastReadTime is non-zero
		// because ReadPacketBuffer above succeeded (>=1 chunk consumed).
		res.reqTimestamp = sess.DestStream.LastReadTime()
	}

	handshakePkt, err := wire.DecodePayload(ctx, logger, handshake, clientKey, decodeCtx)
	if err != nil {
		return res, fmt.Errorf("decode server greeting: %w", err)
	}
	res.requestOperation = handshakePkt.Header.Type
	if greeting, ok := handshakePkt.Message.(*mysql.HandshakeV10Packet); ok {
		decodeCtx.ServerCaps = greeting.CapabilityFlags
		decodeCtx.ServerGreetings.Store(clientKey, greeting)
	}
	pluginName, err := wire.GetPluginName(handshakePkt.Message)
	if err != nil {
		return res, fmt.Errorf("get initial plugin name: %w", err)
	}
	decodeCtx.PluginName = pluginName

	res.resp = append(res.resp, mysql.Response{PacketBundle: *handshakePkt})

	// Post-TLS: prepend the pre-TLS client SSLRequest (stashed by the observe
	// stream) so the config mock's requests are [SSLRequest, HandshakeResponse41]
	// — the exact shape the replayer's SSL-mock selection requires
	// (simulateInitialHandshake collects mocks whose requests[0] is an
	// SSLRequest, then matches HandshakeResponse41 at requests[1]). It MUST be
	// decoded BEFORE the post-TLS HandshakeResponse41 so decodeCtx.LastOp stays
	// HandshakeV10 for it. Mirrors legacy handlePostTLSRecord.
	if postTLS && len(stashedSSLReq) > 0 {
		if sslReqPkt, sErr := wire.DecodePayload(ctx, logger, stashedSSLReq, clientKey, decodeCtx); sErr == nil {
			res.req = append(res.req, mysql.Request{PacketBundle: *sslReqPkt})
		} else {
			logger.Debug("post-TLS: failed to decode stashed SSLRequest; config mock will lack the SSL-selected shape", zap.Error(sErr))
		}
	}

	// Read the client's first response: HandshakeResponse41 OR
	// SSLRequest (short form).
	clientFirst, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.ClientStream)
	if err != nil {
		return res, fmt.Errorf("read client handshake response: %w", err)
	}
	clientFirstPkt, err := wire.DecodePayload(ctx, logger, clientFirst, clientKey, decodeCtx)
	if err != nil {
		return res, fmt.Errorf("decode client handshake response: %w", err)
	}
	decodeCtx.ClientCaps = decodeCtx.ClientCapabilities
	res.req = append(res.req, mysql.Request{PacketBundle: *clientFirstPkt})

	if decodeCtx.UseSSL {
		switch {
		case !postTLS && sess.Opts.SkipTLSMITM:
			// Observe-only proxyless TLS (TLS terminated in-JVM / by an
			// SSL/GoTLS/JSSE uprobe, not by us): there is NO upstream socket to
			// relay a live TLS upgrade onto, so performTLSUpgradeV2 cannot
			// succeed here. Instead stash the pre-TLS server greeting + client
			// SSLRequest in TLSHandshakeStore; the POST-TLS decrypted stream
			// (routed here with PostTLSMode) pops them and records the full
			// handshake + command phase. The pre-TLS config-mock shell (greeting
			// + SSLRequest, already in res) is returned so it is emitted here.
			// This is the V2 analogue of the legacy conn.go SkipTLSMITM branch.
			if err := stashPreTLSHandshakeV2(ctx, sess, handshake, clientFirst, res.reqTimestamp); err != nil {
				return res, err
			}
			// Do NOT emit a config mock from the pre-TLS observe stream: it would
			// be an incomplete [SSLRequest]-only mock that the replayer selects as
			// SSL-eligible but then can't match the HandshakeResponse41 against
			// ("no mysql mocks matched the HandshakeResponse41 within SSL-selected
			// mocks"). The POST-TLS decrypted stream emits the authoritative
			// combined [SSLRequest, HandshakeResponse41] config mock instead.
			res.skipConfigMock = true
			return res, nil

		case postTLS:
			// The client packet just read (clientFirst) IS the post-TLS
			// HandshakeResponse41 with credentials, delivered in plaintext by the
			// uprobe over the already-established TLS channel. There is no upgrade
			// to perform — fall through to the auth read below. The greeting was
			// already restored from the stash above, so decodeCtx is initialised.

		default:
			// Proxy mode (keploy terminates TLS): perform the relay TLS upgrade.
			if err := performTLSUpgradeV2(ctx, logger, sess); err != nil {
				sess.MarkMockIncomplete("tls upgrade failed")
				return res, err
			}
			// After TLS upgrade, the relay is writing plaintext into our
			// FakeConns. The client will re-send HandshakeResponse41 with
			// credentials. Swap the clientKey to a fresh sentinel so legacy
			// decode-state under the old key does not bleed in.
			newKey := newSentinelConn()
			decodeCtx.LastOp.Store(newKey, mysql.HandshakeV10)
			if sg, ok := decodeCtx.ServerGreetings.Load(clientKey); ok {
				decodeCtx.ServerGreetings.Store(newKey, sg)
			}
			*clientKeyPtr = newKey
			clientKey = newKey

			// Read post-TLS HandshakeResponse41.
			hr41Buf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.ClientStream)
			if err != nil {
				return res, fmt.Errorf("read post-TLS handshake response: %w", err)
			}
			hr41Pkt, err := wire.DecodePayload(ctx, logger, hr41Buf, clientKey, decodeCtx)
			if err != nil {
				return res, fmt.Errorf("decode post-TLS handshake response: %w", err)
			}
			decodeCtx.ClientCaps = decodeCtx.ClientCapabilities
			res.req = append(res.req, mysql.Request{PacketBundle: *hr41Pkt})
		}
	}

	// Auth decider: AuthSwitchRequest / AuthMoreData / OK / ERR.
	authData, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.DestStream)
	if err != nil {
		return res, fmt.Errorf("read auth data from server: %w", err)
	}
	authDecider, err := wire.DecodePayload(ctx, logger, authData, clientKey, decodeCtx)
	if err != nil {
		return res, fmt.Errorf("decode auth data from server: %w", err)
	}

	if _, ok := authDecider.Message.(*mysql.AuthSwitchRequestPacket); ok {
		res.resp = append(res.resp, mysql.Response{PacketBundle: *authDecider})
		pkt := authDecider.Message.(*mysql.AuthSwitchRequestPacket)
		decodeCtx.PluginName = pkt.PluginName

		switchResp, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.ClientStream)
		if err != nil {
			return res, fmt.Errorf("read auth switch response: %w", err)
		}
		switchPkt, err := mysqlUtils.BytesToMySQLPacket(switchResp)
		if err != nil {
			return res, fmt.Errorf("parse auth switch response: %w", err)
		}
		res.req = append(res.req, mysql.Request{
			PacketBundle: mysql.PacketBundle{
				Header:  &mysql.PacketInfo{Header: &switchPkt.Header, Type: mysql.AuthSwithResponse},
				Message: intgUtils.EncodeBase64(switchPkt.Payload),
			},
		})

		authData, err = mysqlUtils.ReadPacketBuffer(ctx, logger, sess.DestStream)
		if err != nil {
			return res, fmt.Errorf("read auth data after switch: %w", err)
		}
		authDecider, err = wire.DecodePayload(ctx, logger, authData, clientKey, decodeCtx)
		if err != nil {
			return res, fmt.Errorf("decode auth data after switch: %w", err)
		}
	}

	// Consume the auth decider (AuthMoreData/OK/ERR) and any follow-ups.
	authRes, err := handleAuthV2(ctx, logger, sess, decodeCtx, clientKey, authDecider)
	if err != nil {
		return res, err
	}
	res.req = append(res.req, authRes.req...)
	res.resp = append(res.resp, authRes.resp...)
	res.responseOperation = authRes.responseOperation

	// The response timestamp is the time the last response packet
	// arrived at the relay — sampled from DestStream's chunk time.
	res.resTimestamp = sess.DestStream.LastReadTime()

	return res, nil
}

func handleAuthV2(ctx context.Context, logger *zap.Logger, sess *supervisor.Session, decodeCtx *wire.DecodeContext, clientKey net.Conn, authPkt *mysql.PacketBundle) (v2HandshakeResult, error) {
	res := v2HandshakeResult{
		req:  make([]mysql.Request, 0),
		resp: make([]mysql.Response, 0),
	}
	switch mysql.AuthPluginName(decodeCtx.PluginName) {
	case mysql.Native:
		res.resp = append(res.resp, mysql.Response{PacketBundle: *authPkt})
		res.responseOperation = authPkt.Header.Type
		return res, nil
	case mysql.CachingSha2:
		return handleCachingSha2PasswordV2(ctx, logger, sess, decodeCtx, clientKey, authPkt)
	case mysql.Sha256:
		return res, fmt.Errorf("Sha256 Password authentication is not supported")
	default:
		// Some drivers receive an OK/ERR directly — treat as terminal.
		switch authPkt.Message.(type) {
		case *mysql.OKPacket, *mysql.ERRPacket:
			res.resp = append(res.resp, mysql.Response{PacketBundle: *authPkt})
			res.responseOperation = authPkt.Header.Type
			return res, nil
		}
		return res, fmt.Errorf("unsupported authentication plugin: %s", decodeCtx.PluginName)
	}
}

func handleCachingSha2PasswordV2(ctx context.Context, logger *zap.Logger, sess *supervisor.Session, decodeCtx *wire.DecodeContext, clientKey net.Conn, authPkt *mysql.PacketBundle) (v2HandshakeResult, error) {
	res := v2HandshakeResult{
		req:  make([]mysql.Request, 0),
		resp: make([]mysql.Response, 0),
	}

	// Fast-path: server may have sent an OK directly (no full auth
	// needed).
	if _, ok := authPkt.Message.(*mysql.OKPacket); ok {
		res.resp = append(res.resp, mysql.Response{PacketBundle: *authPkt})
		res.responseOperation = authPkt.Header.Type
		return res, nil
	}

	authMorePkt, ok := authPkt.Message.(*mysql.AuthMoreDataPacket)
	if !ok {
		return res, fmt.Errorf("invalid packet type for caching sha2 password mechanism, expected AuthMoreDataPacket, got %T", authPkt.Message)
	}

	authMechanism, err := wire.GetCachingSha2PasswordMechanism(authMorePkt.Data[0])
	if err != nil {
		return res, fmt.Errorf("get caching sha2 password mechanism: %w", err)
	}
	authMorePkt.Data = authMechanism
	res.resp = append(res.resp, mysql.Response{PacketBundle: *authPkt})

	mech, err := wire.StringToCachingSha2PasswordMechanism(authMechanism)
	if err != nil {
		return res, fmt.Errorf("convert caching sha2 password mechanism: %w", err)
	}

	var follow v2HandshakeResult
	switch mech {
	case mysql.PerformFullAuthentication:
		follow, err = handleFullAuthV2(ctx, logger, sess, decodeCtx, clientKey)
		if err != nil {
			return res, fmt.Errorf("caching sha2 password full auth: %w", err)
		}
	case mysql.FastAuthSuccess:
		follow, err = handleFastAuthSuccessV2(ctx, logger, sess, decodeCtx, clientKey)
		if err != nil {
			return res, fmt.Errorf("caching sha2 password fast auth success: %w", err)
		}
	}
	res.req = append(res.req, follow.req...)
	res.resp = append(res.resp, follow.resp...)
	res.responseOperation = follow.responseOperation
	return res, nil
}

func handleFastAuthSuccessV2(ctx context.Context, logger *zap.Logger, sess *supervisor.Session, decodeCtx *wire.DecodeContext, clientKey net.Conn) (v2HandshakeResult, error) {
	res := v2HandshakeResult{
		req:  make([]mysql.Request, 0),
		resp: make([]mysql.Response, 0),
	}
	finalResp, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.DestStream)
	if err != nil {
		return res, fmt.Errorf("read final auth response: %w", err)
	}
	finalPkt, err := wire.DecodePayload(ctx, logger, finalResp, clientKey, decodeCtx)
	if err != nil {
		return res, fmt.Errorf("decode final auth response: %w", err)
	}
	res.resp = append(res.resp, mysql.Response{PacketBundle: *finalPkt})
	res.responseOperation = finalPkt.Header.Type
	return res, nil
}

func handleFullAuthV2(ctx context.Context, logger *zap.Logger, sess *supervisor.Session, decodeCtx *wire.DecodeContext, clientKey net.Conn) (v2HandshakeResult, error) {
	res := v2HandshakeResult{
		req:  make([]mysql.Request, 0),
		resp: make([]mysql.Response, 0),
	}

	if decodeCtx.UseSSL {
		// Plain password path: client sends the password in the clear
		// over the already-established TLS channel.
		return handlePlainPasswordV2(ctx, logger, sess, decodeCtx, clientKey)
	}

	// Client requests server's public key.
	pubKeyReqBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.ClientStream)
	if err != nil {
		return res, fmt.Errorf("read public key request: %w", err)
	}
	pubKeyReqPkt, err := wire.DecodePayload(ctx, logger, pubKeyReqBuf, clientKey, decodeCtx)
	if err != nil {
		return res, fmt.Errorf("decode public key request: %w", err)
	}
	res.req = append(res.req, mysql.Request{PacketBundle: *pubKeyReqPkt})

	// Server sends public key.
	pubKey, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.DestStream)
	if err != nil {
		return res, fmt.Errorf("read public key response: %w", err)
	}
	pubKeyPkt, err := wire.DecodePayload(ctx, logger, pubKey, clientKey, decodeCtx)
	if err != nil {
		return res, fmt.Errorf("decode public key response: %w", err)
	}
	pubKeyPkt.Meta = map[string]string{"auth operation": "public key response"}
	res.resp = append(res.resp, mysql.Response{PacketBundle: *pubKeyPkt})

	// Client sends encrypted password.
	encPassBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.ClientStream)
	if err != nil {
		return res, fmt.Errorf("read encrypted password: %w", err)
	}
	encPass, err := mysqlUtils.BytesToMySQLPacket(encPassBuf)
	if err != nil {
		return res, fmt.Errorf("parse encrypted password: %w", err)
	}
	res.req = append(res.req, mysql.Request{
		PacketBundle: mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &encPass.Header, Type: mysql.EncryptedPassword},
			Message: intgUtils.EncodeBase64(encPass.Payload),
		},
	})

	// Final OK/ERR from server.
	finalBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.DestStream)
	if err != nil {
		return res, fmt.Errorf("read final auth response: %w", err)
	}
	finalPkt, err := wire.DecodePayload(ctx, logger, finalBuf, clientKey, decodeCtx)
	if err != nil {
		return res, fmt.Errorf("decode final auth response: %w", err)
	}
	res.resp = append(res.resp, mysql.Response{PacketBundle: *finalPkt})
	res.responseOperation = finalPkt.Header.Type
	return res, nil
}

func handlePlainPasswordV2(ctx context.Context, logger *zap.Logger, sess *supervisor.Session, decodeCtx *wire.DecodeContext, clientKey net.Conn) (v2HandshakeResult, error) {
	res := v2HandshakeResult{
		req:  make([]mysql.Request, 0),
		resp: make([]mysql.Response, 0),
	}
	plainBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.ClientStream)
	if err != nil {
		return res, fmt.Errorf("read plain password: %w", err)
	}
	plainPkt, err := mysqlUtils.BytesToMySQLPacket(plainBuf)
	if err != nil {
		return res, fmt.Errorf("parse plain password: %w", err)
	}
	res.req = append(res.req, mysql.Request{
		PacketBundle: mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &plainPkt.Header, Type: mysql.PlainPassword},
			Message: intgUtils.EncodeBase64(plainPkt.Payload),
		},
	})
	finalBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.DestStream)
	if err != nil {
		return res, fmt.Errorf("read final auth response (plain): %w", err)
	}
	finalPkt, err := wire.DecodePayload(ctx, logger, finalBuf, clientKey, decodeCtx)
	if err != nil {
		return res, fmt.Errorf("decode final auth response (plain): %w", err)
	}
	res.resp = append(res.resp, mysql.Response{PacketBundle: *finalPkt})
	res.responseOperation = finalPkt.Header.Type
	return res, nil
}

// stashPreTLSHandshakeV2 pushes the raw pre-TLS server greeting + client
// SSLRequest into the shared TLSHandshakeStore so the POST-TLS decrypted stream
// (routed here with PostTLSMode) can pop them and reconstruct the handshake.
// V2 analogue of the SkipTLSMITM branch in the legacy conn.go
// handleInitialHandshake. Pushes under BOTH the connection-specific key and the
// port-only fallback: the pre-TLS observe stream and the post-TLS decrypted
// stream are different TCP connections (different ConnKeys), so the port key is
// what bridges them.
func stashPreTLSHandshakeV2(ctx context.Context, sess *supervisor.Session, greeting, sslRequest []byte, reqTimestamp time.Time) error {
	hsStore, ok := ctx.Value(models.TLSHandshakeStoreKey).(*models.TLSHandshakeStore)
	if !ok || hsStore == nil {
		return fmt.Errorf("SkipTLSMITM requires TLSHandshakeStore in context for MySQL handshake reconstruction")
	}
	dstPort := uint16(0)
	if sess.Opts.DstCfg != nil {
		dstPort = uint16(sess.Opts.DstCfg.Port)
	}
	entry := models.TLSHandshakeEntry{
		RespPackets:  [][]byte{greeting},
		ReqPackets:   [][]byte{sslRequest},
		ReqTimestamp: reqTimestamp,
	}
	hsStore.Push(models.HandshakeStoreKey(sess.Opts.ConnKey, dstPort), entry)
	hsStore.Push(models.HandshakeStoreKey("", dstPort), entry)
	return nil
}

// popStoredGreetingV2 pops the pre-TLS server greeting (and client SSLRequest)
// stashed by stashPreTLSHandshakeV2. Tries the connection-specific key first,
// then the port-only fallback (which is what actually matches across the two
// distinct streams). Waits briefly because the decrypted stream can reach here
// before the pre-TLS stream has finished stashing. Returns
// wire.ErrServerGreetingNotFound when nothing is available, so the caller skips
// the connection gracefully (same as a genuine mid-stream join).
func popStoredGreetingV2(ctx context.Context, logger *zap.Logger, sess *supervisor.Session) (greeting []byte, sslRequest []byte, err error) {
	hsStore, ok := ctx.Value(models.TLSHandshakeStoreKey).(*models.TLSHandshakeStore)
	if !ok || hsStore == nil {
		return nil, nil, wire.ErrServerGreetingNotFound
	}
	dstPort := uint16(0)
	if sess.Opts.DstCfg != nil {
		dstPort = uint16(sess.Opts.DstCfg.Port)
	}
	entry, found := hsStore.PopWait(models.HandshakeStoreKey(sess.Opts.ConnKey, dstPort), 5*time.Second)
	if !found {
		entry, found = hsStore.PopWait(models.HandshakeStoreKey("", dstPort), 3*time.Second)
	}
	if !found || len(entry.RespPackets) == 0 {
		if logger != nil {
			logger.Debug("post-TLS MySQL: no stashed server greeting available", zap.Uint16("dstPort", dstPort))
		}
		return nil, nil, wire.ErrServerGreetingNotFound
	}
	greeting = entry.RespPackets[0]
	if len(entry.ReqPackets) > 0 {
		sslRequest = entry.ReqPackets[0]
	}
	return greeting, sslRequest, nil
}

// performTLSUpgradeV2 builds TLS configs from the session options and
// sends a KindUpgradeTLS directive on sess.Directives, blocking on the
// ack. Returns nil on OK, an error on failure. Failure cases MUST call
// sess.MarkMockIncomplete at the call site.
func performTLSUpgradeV2(ctx context.Context, logger *zap.Logger, sess *supervisor.Session) error {
	destCfg := buildDestTLSConfigV2(sess)
	clientCfg := buildClientTLSConfigV2(sess)

	logger.Debug("V2: sending mysql client_ssl upgrade directive",
		zap.Bool("destCfg", destCfg != nil),
		zap.Bool("clientCfg", clientCfg != nil))

	select {
	case <-ctx.Done():
		return ctx.Err()
	case sess.Directives <- directive.UpgradeTLS(destCfg, clientCfg, "mysql client_ssl"):
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case ack, ok := <-sess.Acks:
		if !ok {
			return fmt.Errorf("directive channel closed before TLS upgrade ack")
		}
		if !ack.OK {
			if ack.Err != nil {
				return fmt.Errorf("tls upgrade: %w", ack.Err)
			}
			return errors.New("tls upgrade rejected by relay")
		}
		return nil
	}
}

// buildDestTLSConfigV2 constructs the TLS client config used by the
// relay when dialing TLS to the real upstream MySQL server. We derive
// the ServerName from the destination address (host portion of
// host:port in Opts.DstCfg.Addr) so the handshake uses correct SNI.
//
// Certificate verification uses the system trust store by default.
// If the upstream's cert does not verify (e.g. self-signed), the
// relay's TLS handshake fails and the supervisor falls through to
// raw passthrough — the application's connection continues but the
// mock is dropped. This is the intended trade-off: stability over
// fidelity. Users who need to record against non-system-trust
// upstreams should add the cert to their system store.
func buildDestTLSConfigV2(sess *supervisor.Session) *tls.Config {
	var serverName string
	if sess.Opts.DstCfg != nil {
		addr := sess.Opts.DstCfg.Addr
		if host, _, err := net.SplitHostPort(addr); err == nil && host != "" {
			serverName = host
		} else if addr != "" {
			serverName = addr
		}
	}
	return &tls.Config{
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS12,
		KeyLogWriter: pTls.KeyLogWriter(),
	}
}

// buildClientTLSConfigV2 returns a non-nil placeholder TLS config to
// request a client-side TLS upgrade via the relay. The actual cert
// presented to the client is picked by the relay's injected
// TLSUpgradeFn (pTls.HandleTLSConnection), which already owns the
// keploy MITM cert chain; we just need a non-nil config to signal
// "yes, upgrade the client side".
func buildClientTLSConfigV2(_ *supervisor.Session) *tls.Config {
	return &tls.Config{}
}

// ---- Command phase ---------------------------------------------------

// handleCommandsV2 drives the MySQL command phase on the V2 path. Each
// iteration:
//  1. Reads one command packet from ClientStream.
//  2. Emits a no-response mock for COM_STMT_CLOSE / COM_STMT_SEND_LONG_DATA.
//  3. Otherwise, collects the server's response (single OK/ERR packet
//     or a multi-packet result set / prepare OK), decodes it, and emits
//     one mock matching the legacy path's shape.
//
// Exits cleanly on io.EOF / fakeconn.ErrClosed from either stream.
func handleCommandsV2(ctx context.Context, logger *zap.Logger, sess *supervisor.Session, decodeCtx *wire.DecodeContext, clientKey net.Conn) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		cmdBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.ClientStream)
		if err != nil {
			return err
		}
		// Chunk-derived timestamp: ReadPacketBuffer succeeded, so at
		// least one chunk has been consumed and LastReadTime is set.
		reqTs := sess.ClientStream.LastReadTime()

		cmdPkt, err := wire.DecodePayload(ctx, logger, cmdBuf, clientKey, decodeCtx)
		if err != nil {
			logger.Debug("V2: failed to decode mysql command; resetting state", zap.Error(err))
			decodeCtx.LastOp.Store(clientKey, wire.RESET)
			continue
		}

		// No-response commands: emit immediately with empty response,
		// matching the legacy recorder shape. Response timestamp is
		// the command arrival time since no server response exists.
		if wire.IsNoResponseCommand(cmdPkt.Header.Type) {
			emitMockV2(ctx, sess, []mysql.Request{{PacketBundle: *cmdPkt}}, nil, "mocks",
				cmdPkt.Header.Type, "NO Response Packet", reqTs, reqTs)
			continue
		}
		if strings.HasPrefix(cmdPkt.Header.Type, "0x") {
			// Unknown packet: treat as no-response to avoid desync.
			emitMockV2(ctx, sess, []mysql.Request{{PacketBundle: *cmdPkt}}, nil, "mocks",
				cmdPkt.Header.Type, "NO Response Packet", reqTs, reqTs)
			continue
		}

		// Load lastOp for response shape.
		lastOp, _ := decodeCtx.LastOp.Load(clientKey)

		respBundle, resTs, err := collectResponseV2(ctx, logger, sess, decodeCtx, clientKey, lastOp)
		if err != nil {
			return err
		}
		if respBundle == nil {
			// Desync or benign decode failure — drop this exchange.
			continue
		}

		mockType := "mocks"
		if cmdPkt.Header.Type == mysql.CommandStatusToString(mysql.COM_STMT_PREPARE) {
			mockType = "connection"
		}

		emitMockV2(ctx, sess,
			[]mysql.Request{{PacketBundle: *cmdPkt}},
			[]mysql.Response{{PacketBundle: *respBundle}},
			mockType,
			cmdPkt.Header.Type,
			respBundle.Header.Type,
			reqTs, resTs)
	}
}

// collectResponseV2 reads the response to the current command. It
// returns the assembled response bundle and the timestamp of the last
// chunk that arrived. Single-packet responses (OK/ERR) are returned as
// decoded. Multi-packet responses (result sets, stmt-prepare ok) are
// assembled using the same state machine as the legacy async decoder.
func collectResponseV2(ctx context.Context, logger *zap.Logger, sess *supervisor.Session, decodeCtx *wire.DecodeContext, clientKey net.Conn, lastOp byte) (*mysql.PacketBundle, time.Time, error) {
	firstBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.DestStream)
	if err != nil {
		return nil, time.Time{}, err
	}
	firstPkt, err := wire.DecodePayload(ctx, logger, firstBuf, clientKey, decodeCtx)
	if err != nil {
		logger.Debug("V2: failed to decode mysql response head", zap.Error(err))
		return nil, sess.DestStream.LastReadTime(), nil
	}

	// Simple single-packet case: OK/ERR.
	if firstPkt.Header.Type == mysql.StatusToString(mysql.OK) ||
		firstPkt.Header.Type == mysql.StatusToString(mysql.ERR) {
		return firstPkt, sess.DestStream.LastReadTime(), nil
	}

	// Multi-packet dispatch keyed on lastOp (the command just issued).
	switch lastOp {
	case mysql.COM_QUERY:
		rs, ok := firstPkt.Message.(*mysql.TextResultSet)
		if !ok {
			return firstPkt, sess.DestStream.LastReadTime(), nil
		}
		ts, err := assembleTextResultSetV2(ctx, logger, sess, decodeCtx, firstPkt, rs)
		if err != nil {
			return nil, ts, err
		}
		decodeCtx.LastOp.Store(clientKey, wire.RESET)
		return firstPkt, ts, nil

	case mysql.COM_STMT_EXECUTE:
		rs, ok := firstPkt.Message.(*mysql.BinaryProtocolResultSet)
		if !ok {
			return firstPkt, sess.DestStream.LastReadTime(), nil
		}
		ts, err := assembleBinaryResultSetV2(ctx, logger, sess, decodeCtx, firstPkt, rs)
		if err != nil {
			return nil, ts, err
		}
		decodeCtx.LastOp.Store(clientKey, wire.RESET)
		return firstPkt, ts, nil

	case mysql.COM_STMT_PREPARE:
		sp, ok := firstPkt.Message.(*mysql.StmtPrepareOkPacket)
		if !ok {
			return firstPkt, sess.DestStream.LastReadTime(), nil
		}
		ts, err := assembleStmtPrepareV2(ctx, logger, sess, decodeCtx, sp)
		if err != nil {
			return nil, ts, err
		}
		decodeCtx.LastOp.Store(clientKey, mysql.OK)
		return firstPkt, ts, nil

	default:
		// Unexpected multi-packet shape: treat as single.
		return firstPkt, sess.DestStream.LastReadTime(), nil
	}
}

func assembleTextResultSetV2(ctx context.Context, logger *zap.Logger, sess *supervisor.Session, decodeCtx *wire.DecodeContext, headPkt *mysql.PacketBundle, rs *mysql.TextResultSet) (time.Time, error) {
	ts := sess.DestStream.LastReadTime()
	// Read column definitions.
	for i := uint64(0); i < rs.ColumnCount; i++ {
		buf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.DestStream)
		if err != nil {
			return ts, err
		}
		ts = sess.DestStream.LastReadTime()
		col, _, err := rowscols.DecodeColumn(ctx, logger, buf)
		if err != nil {
			return ts, fmt.Errorf("decode text column %d: %w", i, err)
		}
		rs.Columns = append(rs.Columns, col)
	}

	// EOF after columns unless CLIENT_DEPRECATE_EOF.
	if !decodeCtx.DeprecateEOF() {
		eofBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.DestStream)
		if err != nil {
			return ts, err
		}
		ts = sess.DestStream.LastReadTime()
		rs.EOFAfterColumns = eofBuf
	}

	// Rows until terminator.
	for {
		buf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.DestStream)
		if err != nil {
			return ts, err
		}
		ts = sess.DestStream.LastReadTime()
		if mysqlUtils.IsResultSetTerminator(buf, decodeCtx.DeprecateEOF()) {
			respType := mysql.StatusToString(mysql.EOF)
			if decodeCtx.DeprecateEOF() && mysqlUtils.IsOKReplacingEOF(buf) {
				respType = mysql.StatusToString(mysql.OK)
			}
			rs.FinalResponse = &mysql.GenericResponse{Data: buf, Type: respType}
			// Preserve the wire Header set by processFirstResponse via
			// the initial DecodePayload; only the bundle Header needs
			// the result-set type set.
			headPkt.Header.Type = string(mysql.Text)
			return ts, nil
		}
		row, _, err := rowscols.DecodeTextRow(ctx, logger, buf, rs.Columns)
		if err != nil {
			logger.Debug("V2: decode text row failed", zap.Error(err))
			continue
		}
		rs.Rows = append(rs.Rows, row)
	}
}

func assembleBinaryResultSetV2(ctx context.Context, logger *zap.Logger, sess *supervisor.Session, decodeCtx *wire.DecodeContext, headPkt *mysql.PacketBundle, rs *mysql.BinaryProtocolResultSet) (time.Time, error) {
	ts := sess.DestStream.LastReadTime()
	for i := uint64(0); i < rs.ColumnCount; i++ {
		buf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.DestStream)
		if err != nil {
			return ts, err
		}
		ts = sess.DestStream.LastReadTime()
		col, _, err := rowscols.DecodeColumn(ctx, logger, buf)
		if err != nil {
			return ts, fmt.Errorf("decode binary column %d: %w", i, err)
		}
		rs.Columns = append(rs.Columns, col)
	}
	if !decodeCtx.DeprecateEOF() {
		eofBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.DestStream)
		if err != nil {
			return ts, err
		}
		ts = sess.DestStream.LastReadTime()
		rs.EOFAfterColumns = eofBuf
	}
	for {
		buf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.DestStream)
		if err != nil {
			return ts, err
		}
		ts = sess.DestStream.LastReadTime()
		if mysqlUtils.IsResultSetTerminator(buf, decodeCtx.DeprecateEOF()) {
			respType := mysql.StatusToString(mysql.EOF)
			if decodeCtx.DeprecateEOF() && mysqlUtils.IsOKReplacingEOF(buf) {
				respType = mysql.StatusToString(mysql.OK)
			}
			rs.FinalResponse = &mysql.GenericResponse{Data: buf, Type: respType}
			headPkt.Header.Type = string(mysql.Binary)
			return ts, nil
		}
		row, _, err := rowscols.DecodeBinaryRow(ctx, logger, buf, rs.Columns)
		if err != nil {
			logger.Debug("V2: decode binary row failed", zap.Error(err))
			continue
		}
		rs.Rows = append(rs.Rows, row)
	}
}

func assembleStmtPrepareV2(ctx context.Context, logger *zap.Logger, sess *supervisor.Session, decodeCtx *wire.DecodeContext, sp *mysql.StmtPrepareOkPacket) (time.Time, error) {
	ts := sess.DestStream.LastReadTime()
	// Param defs.
	for i := uint16(0); i < sp.NumParams; i++ {
		buf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.DestStream)
		if err != nil {
			return ts, err
		}
		ts = sess.DestStream.LastReadTime()
		col, _, err := rowscols.DecodeColumn(ctx, logger, buf)
		if err != nil {
			return ts, fmt.Errorf("decode stmt param %d: %w", i, err)
		}
		sp.ParamDefs = append(sp.ParamDefs, col)
	}
	if sp.NumParams > 0 && !decodeCtx.DeprecateEOF() {
		buf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.DestStream)
		if err != nil {
			return ts, err
		}
		ts = sess.DestStream.LastReadTime()
		if mysqlUtils.IsEOFPacket(buf) {
			sp.EOFAfterParamDefs = buf
		}
	}
	// Column defs.
	for i := uint16(0); i < sp.NumColumns; i++ {
		buf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.DestStream)
		if err != nil {
			return ts, err
		}
		ts = sess.DestStream.LastReadTime()
		col, _, err := rowscols.DecodeColumn(ctx, logger, buf)
		if err != nil {
			return ts, fmt.Errorf("decode stmt column %d: %w", i, err)
		}
		sp.ColumnDefs = append(sp.ColumnDefs, col)
	}
	if sp.NumColumns > 0 && !decodeCtx.DeprecateEOF() {
		buf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.DestStream)
		if err != nil {
			return ts, err
		}
		ts = sess.DestStream.LastReadTime()
		if mysqlUtils.IsEOFPacket(buf) {
			sp.EOFAfterColumnDefs = buf
		}
	}
	return ts, nil
}

// emitMockV2 builds a models.Mock matching the legacy recordMock shape
// (metadata, kind, spec) and hands it to the supervisor Session via
// EmitMock so the session's post-record hook chain and incomplete-mock
// gate run consistently. Timestamps come from chunk ReadAt stamps; the
// legacy path's time.Now() use is deliberately NOT replicated here.
func emitMockV2(ctx context.Context, sess *supervisor.Session, requests []mysql.Request, responses []mysql.Response, mockType, reqOp, respOp string, reqTs, resTs time.Time) {
	meta := map[string]string{
		"type":              mockType,
		"requestOperation":  reqOp,
		"responseOperation": respOp,
		"connID":            sess.ClientConnID,
	}
	if sess.Opts.DstCfg != nil && sess.Opts.DstCfg.Addr != "" {
		meta["destAddr"] = sess.Opts.DstCfg.Addr
	}

	lifetime := models.LifetimePerTest
	switch mockType {
	case "config":
		lifetime = models.LifetimeSession
	case "connection":
		lifetime = models.LifetimeConnection
	default:
		if models.IsMySQLSessionReusableCommandType(reqOp) {
			lifetime = models.LifetimeSession
		}
	}
	m := &models.Mock{
		Version: models.GetVersion(),
		Kind:    models.MySQL,
		Name:    mockType,
		Spec: models.MockSpec{
			Metadata:         meta,
			MySQLRequests:    requests,
			MySQLResponses:   responses,
			Created:          time.Now().Unix(),
			ReqTimestampMock: reqTs,
			ResTimestampMock: resTs,
		},
		TestModeInfo: models.TestModeInfo{
			Lifetime:        lifetime,
			LifetimeDerived: true,
		},
	}

	if err := sess.EmitMock(m); err != nil {
		if sess.Logger != nil {
			sess.Logger.Debug("V2: emit mock failed", zap.Error(err))
		}
	}
	// Note: EmitMock auto-clears the incomplete flag when it drops a
	// partial mock (see supervisor.Session.EmitMock). On the healthy-
	// emit path the flag should already be false. We deliberately do
	// NOT call MarkMockComplete here because the relay may have set
	// the flag for memory-guard reasons we must respect.
	_ = ctx
}

// sentinelConn is a zero-behaviour net.Conn used only as a map key in
// DecodeContext after a TLS upgrade, when we need a fresh identity
// distinct from the pre-TLS FakeConn pointer. It never reads or
// writes; any call panics in a way the parser never exercises.
type sentinelConn struct{}

func newSentinelConn() net.Conn {
	return &sentinelConn{}
}

func (*sentinelConn) Read(_ []byte) (int, error)         { return 0, io.EOF }
func (*sentinelConn) Write(_ []byte) (int, error)        { return 0, io.ErrClosedPipe }
func (*sentinelConn) Close() error                       { return nil }
func (*sentinelConn) LocalAddr() net.Addr                { return sentinelAddr{} }
func (*sentinelConn) RemoteAddr() net.Addr               { return sentinelAddr{} }
func (*sentinelConn) SetDeadline(_ time.Time) error      { return nil }
func (*sentinelConn) SetReadDeadline(_ time.Time) error  { return nil }
func (*sentinelConn) SetWriteDeadline(_ time.Time) error { return nil }

type sentinelAddr struct{}

func (sentinelAddr) Network() string { return "mysql-v2-sentinel" }
func (sentinelAddr) String() string  { return "mysql-v2-sentinel" }
