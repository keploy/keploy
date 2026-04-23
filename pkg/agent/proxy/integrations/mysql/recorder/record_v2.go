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

	handshake, err := handleInitialHandshakeV2(ctx, logger, sess, decodeCtx, &clientKeyNetConn)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, fakeconn.ErrClosed) {
			logger.Debug("EOF during MySQL V2 initial handshake")
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
func handleInitialHandshakeV2(ctx context.Context, logger *zap.Logger, sess *supervisor.Session, decodeCtx *wire.DecodeContext, clientKeyPtr *net.Conn) (v2HandshakeResult, error) {
	res := v2HandshakeResult{
		req:  make([]mysql.Request, 0),
		resp: make([]mysql.Response, 0),
	}

	clientKey := *clientKeyPtr

	// Read HandshakeV10 from DestStream.
	handshake, err := mysqlUtils.ReadPacketBuffer(ctx, logger, sess.DestStream)
	if err != nil {
		return res, fmt.Errorf("read server greeting: %w", err)
	}

	// The request timestamp of the config mock is the time the server
	// greeting arrived — matches the semantic the legacy recorder used
	// (it stamped reqTimestamp right after the handshake read, before
	// writing it to the client). Must be chunk-derived: LastReadTime
	// is non-zero because ReadPacketBuffer above succeeded, which
	// guarantees at least one chunk was consumed from the FakeConn.
	res.reqTimestamp = sess.DestStream.LastReadTime()

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
		// Before the TLS upgrade we could record an incomplete mock
		// shape mirroring the legacy SkipTLSMITM branch; on the V2
		// path, however, we must ALWAYS attempt the upgrade via the
		// directive channel. SkipTLSMITM is a legacy concept tied to
		// uprobe coordination that does not exist in V2.

		if err := performTLSUpgradeV2(ctx, logger, sess); err != nil {
			sess.MarkMockIncomplete("tls upgrade failed")
			return res, err
		}

		// After TLS upgrade, the relay is writing plaintext into our
		// FakeConns. The client will re-send HandshakeResponse41 with
		// credentials. Reset the decode LastOp so the next decode
		// treats the new HandshakeResponse41 as a fresh handshake.
		//
		// We also swap the clientKey pointer to a fresh sentinel so
		// legacy-stored decode-state under the old key does not bleed
		// in. The existing maps (LastOp, ServerGreetings) are keyed by
		// net.Conn pointer; we store under the new sentinel.
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
func buildDestTLSConfigV2(sess *supervisor.Session) *tls.Config {
	cfg := &tls.Config{InsecureSkipVerify: true}
	if sess.Opts.DstCfg != nil {
		addr := sess.Opts.DstCfg.Addr
		if host, _, err := net.SplitHostPort(addr); err == nil && host != "" {
			cfg.ServerName = host
		} else if addr != "" {
			// Fall back to raw addr; TLS will still proceed with
			// InsecureSkipVerify but SNI may be wrong.
			cfg.ServerName = addr
		}
	}
	return cfg
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
