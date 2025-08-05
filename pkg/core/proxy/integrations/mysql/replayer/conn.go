//go:build linux

package replayer

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	mysqlUtils "go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/wire"
	intgUtils "go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	pTls "go.keploy.io/server/v2/pkg/core/proxy/tls"
	pUtils "go.keploy.io/server/v2/pkg/core/proxy/util"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type reqResp struct {
	req  []mysql.Request
	resp []mysql.Response
}

type handshakeRes struct {
	tlsClientConn net.Conn
}

// Replay mode
func simulateInitialHandshake(ctx context.Context, logger *zap.Logger, clientConn net.Conn, mocks []*models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext, opts models.OutgoingOptions) (handshakeRes, error) {
	logger.Debug("[DEBUG] Getting the mock for initial handshake", zap.Any("mocks", mocks))
	initialHandshakeMock := mocks[0]

	// Read the intial request and response for the handshake from the mocks
	resp := initialHandshakeMock.Spec.MySQLResponses
	logger.Debug("[DEBUG] MySQLResponses", zap.Any("resp", resp))
	req := initialHandshakeMock.Spec.MySQLRequests
	logger.Debug("[DEBUG] MySQLRequests", zap.Any("req", req))

	res := handshakeRes{}
	reqIdx, respIdx := 0, 0

	logger.Debug("[DEBUG] Checking if responses or requests are empty", zap.Int("respLen", len(resp)), zap.Int("reqLen", len(req)))
	if len(resp) == 0 || len(req) == 0 {
		utils.LogError(logger, nil, "no mysql mocks found for initial handshake")
		return res, nil
	}

	logger.Debug("[DEBUG] Asserting handshake packet", zap.Any("respIdx", respIdx), zap.Any("resp[respIdx]", resp[respIdx]))
	handshake, ok := resp[respIdx].Message.(*mysql.HandshakeV10Packet)
	if !ok {
		utils.LogError(logger, nil, "failed to assert handshake packet")
		return res, nil
	}

	// Store the server greetings
	logger.Debug("[DEBUG] Storing server greetings", zap.Any("handshake", handshake))
	decodeCtx.ServerGreetings.Store(clientConn, handshake)

	// Set the intial auth plugin
	logger.Debug("[DEBUG] Setting initial auth plugin", zap.String("PluginName", handshake.AuthPluginName))
	decodeCtx.PluginName = handshake.AuthPluginName
	var err error

	// encode the response
	logger.Debug("[DEBUG] Encoding handshake packet to binary", zap.Any("packet", resp[respIdx].PacketBundle))
	buf, err := wire.EncodeToBinary(ctx, logger, &resp[respIdx].PacketBundle, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to encode handshake packet")
		return res, err
	}

	// Write the initial handshake to the client
	logger.Debug("[DEBUG] Writing handshake packet to client", zap.Binary("buf", buf))
	_, err = clientConn.Write(buf)
	if err != nil {
		if ctx.Err() != nil {
			logger.Debug("[DEBUG] Context error after writing handshake packet", zap.Error(ctx.Err()))
			return res, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write server greetings to the client")
		logger.Debug("[DEBUG] Error after writing handshake packet", zap.Error(err))
		return res, err
	}

	respIdx++

	// Read the client request, (handshake response or ssl request)
	logger.Debug("[DEBUG] Reading handshake response from client")
	handshakeResponseBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read handshake response from client")
		logger.Debug("[DEBUG] Error reading handshake response from client", zap.Error(err))
		return res, err
	}

	// Decode the handshakeResponse or sslRequest
	logger.Debug("[DEBUG] Decoding handshake response from client", zap.Binary("handshakeResponseBuf", handshakeResponseBuf))
	pkt, err := wire.DecodePayload(ctx, logger, handshakeResponseBuf, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode handshake response from client")
		logger.Debug("[DEBUG] Error decoding handshake response from client", zap.Error(err))
		return res, err
	}

	// handle the SSL request
	logger.Debug("[DEBUG] Checking if SSL is used", zap.Bool("UseSSL", decodeCtx.UseSSL))
	if decodeCtx.UseSSL {
		logger.Debug("[DEBUG] Asserting SSL request packet", zap.Any("pkt.Message", pkt.Message))
		_, ok := pkt.Message.(*mysql.SSLRequestPacket)
		if !ok {
			utils.LogError(logger, nil, "failed to assert SSL request packet")
			return res, nil
		}

		// Get the SSL request from the mock
		logger.Debug("[DEBUG] Asserting mock SSL request packet", zap.Any("req[reqIdx].Message", req[reqIdx].Message))
		_, ok = req[reqIdx].Message.(*mysql.SSLRequestPacket)
		if !ok {
			utils.LogError(logger, nil, "failed to assert mock SSL request packet", zap.Any("expected", req[reqIdx].Header.Type))
			return res, nil
		}

		// Match the SSL request from the client with the mock
		logger.Debug("[DEBUG] Matching SSL request", zap.Any("req[reqIdx].PacketBundle", req[reqIdx].PacketBundle), zap.Any("pkt", pkt))
		err = matchSSLRequest(ctx, logger, req[reqIdx].PacketBundle, *pkt)
		if err != nil {
			utils.LogError(logger, err, "error while matching SSL request")
			logger.Debug("[DEBUG] Error matching SSL request", zap.Error(err))
			return res, err
		}
		reqIdx++ // matched with the mock so increment the index

		// Upgrade the client connection to TLS
		logger.Debug("[DEBUG] Preparing to peek for TLS handshake")
		reader := bufio.NewReader(clientConn)
		initialData := make([]byte, 5)
		testBuffer, err := reader.Peek(len(initialData))
		if err != nil {
			if err == io.EOF && len(testBuffer) == 0 {
				logger.Debug("received EOF, closing conn", zap.Error(err))
				return res, nil
			}
			utils.LogError(logger, err, "failed to peek the mysql request message in proxy")
			logger.Debug("[DEBUG] Error peeking for TLS handshake", zap.Error(err))
			return res, err
		}

		multiReader := io.MultiReader(reader, clientConn)
		clientConn = &pUtils.Conn{
			Conn:   clientConn,
			Reader: multiReader,
			Logger: logger,
		}

		// handle the TLS connection and get the upgraded client connection
		logger.Debug("[DEBUG] Checking if connection is TLS handshake", zap.Binary("testBuffer", testBuffer))
		isTLS := pTls.IsTLSHandshake(testBuffer)
		if isTLS {
			logger.Debug("[DEBUG] Handling TLS connection")
			clientConn, err = pTls.HandleTLSConnection(ctx, logger, clientConn, opts.Backdate)
			if err != nil {
				utils.LogError(logger, err, "failed to handle TLS conn")
				logger.Debug("[DEBUG] Error handling TLS connection", zap.Error(err))
				return res, err
			}
		}

		// Update this tls connection information in the handshake result
		res.tlsClientConn = clientConn

		// Store (Reset) the last operation for the upgraded client connection, because after ssl request the client will send the handshake response packet again.
		logger.Debug("[DEBUG] Storing last operation for upgraded client connection", zap.Any("clientConn", clientConn))
		decodeCtx.LastOp.Store(clientConn, mysql.HandshakeV10)

		// Store the server greeting packet for the upgraded client connection
		logger.Debug("[DEBUG] Storing server greetings for upgraded client connection", zap.Any("clientConn", clientConn), zap.Any("handshake", handshake))
		decodeCtx.ServerGreetings.Store(clientConn, handshake)

		// read the actual handshake response packet
		logger.Debug("[DEBUG] Reading handshake response from client after TLS upgrade")
		handshakeResponseBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
		if err != nil {
			utils.LogError(logger, err, "failed to read handshake response from client")
			logger.Debug("[DEBUG] Error reading handshake response after TLS upgrade", zap.Error(err))
			return res, err
		}

		// Decode the handshakeResponse
		logger.Debug("[DEBUG] Decoding handshake response after TLS upgrade", zap.Binary("handshakeResponseBuf", handshakeResponseBuf))
		pkt, err = wire.DecodePayload(ctx, logger, handshakeResponseBuf, clientConn, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to decode handshake response from client")
			logger.Debug("[DEBUG] Error decoding handshake response after TLS upgrade", zap.Error(err))
			return res, err
		}
	}

	logger.Debug("[DEBUG] Asserting actual handshake response packet", zap.Any("pkt.Message", pkt.Message))
	_, ok = pkt.Message.(*mysql.HandshakeResponse41Packet)
	if !ok {
		utils.LogError(logger, nil, "failed to assert actual handshake response packet")
		return res, nil
	}

	// Get the handshake response from the mock
	logger.Debug("[DEBUG] Asserting mock handshake response packet", zap.Any("req[reqIdx].Message", req[reqIdx].Message))
	_, ok = req[reqIdx].Message.(*mysql.HandshakeResponse41Packet)
	if !ok {
		utils.LogError(logger, nil, "failed to assert mock handshake response packet")
		return res, nil
	}

	// Match the handshake response from the client with the mock
	logger.Debug("[DEBUG] Matching handshake response", zap.Any("actual", pkt), zap.Any("mock", req[reqIdx].PacketBundle))
	err = matchHanshakeResponse41(ctx, logger, req[reqIdx].PacketBundle, *pkt)
	if err != nil {
		utils.LogError(logger, err, "error while matching handshakeResponse41")
		logger.Debug("[DEBUG] Error matching handshakeResponse41", zap.Error(err))
		return res, err
	}
	reqIdx++ // matched with the mock so increment the index

	// Get the next response in order to find the auth mechanism
	logger.Debug("[DEBUG] Checking for next response for auth mechanism", zap.Int("respIdx", respIdx), zap.Int("respLen", len(resp)))
	if len(resp) < respIdx+1 {
		utils.LogError(logger, nil, "no mysql mocks found for auth mechanism")
		return res, nil
	}

	// Get the next packet to decide the auth mechanism or auth switching
	// For Native password: next packet is Ok/Err
	// For CachingSha2 password: next packet is AuthMoreData

	logger.Debug("[DEBUG] Deciding auth mechanism", zap.Any("nextPacketType", resp[respIdx].Header.Type))
	authDecider := resp[respIdx].Header.Type

	// Check if the next packet is AuthSwitchRequest
	// Server sends AuthSwitchRequest when it wants to switch the auth mechanism
	if authDecider == mysql.AuthStatusToString(mysql.AuthSwitchRequest) {
		logger.Debug("[DEBUG] Auth switch request found, switching the auth mechanism", zap.Any("authDecider", authDecider))

		// Get the AuthSwitchRequest packet
		logger.Debug("[DEBUG] Asserting auth switch request packet", zap.Any("resp[respIdx].Message", resp[respIdx].Message))
		authSwithReqPkt, ok := resp[respIdx].Message.(*mysql.AuthSwitchRequestPacket)
		if !ok {
			utils.LogError(logger, nil, "failed to assert auth switch request packet")
			return res, nil
		}

		// Change the auth plugin name
		logger.Debug("[DEBUG] Changing auth plugin name", zap.String("PluginName", authSwithReqPkt.PluginName))
		decodeCtx.PluginName = authSwithReqPkt.PluginName

		// Encode the AuthSwitchRequest packet
		logger.Debug("[DEBUG] Encoding auth switch request packet", zap.Any("packet", resp[respIdx].PacketBundle))
		buf, err = wire.EncodeToBinary(ctx, logger, &resp[respIdx].PacketBundle, clientConn, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to encode auth switch request packet")
			logger.Debug("[DEBUG] Error encoding auth switch request packet", zap.Error(err))
			return res, err
		}

		// Write the AuthSwitchRequest packet to the client
		logger.Debug("[DEBUG] Writing auth switch request packet to client", zap.Binary("buf", buf))
		_, err = clientConn.Write(buf)
		if err != nil {
			if ctx.Err() != nil {
				logger.Debug("[DEBUG] Context error after writing auth switch request", zap.Error(ctx.Err()))
				return res, ctx.Err()
			}
			utils.LogError(logger, err, "failed to write auth switch request to the client")
			logger.Debug("[DEBUG] Error writing auth switch request to client", zap.Error(err))
			return res, err
		}

		logger.Debug("[DEBUG] Incrementing respIdx after writing auth switch request", zap.Int("respIdx", respIdx))
		respIdx++

		// Read the auth switch response from the client
		logger.Debug("[DEBUG] Reading auth switch response from client")
		authSwitchRespBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
		if err != nil {
			utils.LogError(logger, err, "failed to read auth switch response from the client")
			logger.Debug("[DEBUG] Error reading auth switch response from client", zap.Error(err))
			return res, err
		}

		// Get the packet from the buffer
		logger.Debug("[DEBUG] Converting auth switch response to packet", zap.Binary("authSwitchRespBuf", authSwitchRespBuf))
		authSwitchRespPkt, err := mysqlUtils.BytesToMySQLPacket(authSwitchRespBuf)
		if err != nil {
			utils.LogError(logger, err, "failed to convert auth switch response to packet")
			logger.Debug("[DEBUG] Error converting auth switch response to packet", zap.Error(err))
			return res, err
		}

		logger.Debug("[DEBUG] Checking for auth switch response mock", zap.Int("reqIdx", reqIdx), zap.Int("reqLen", len(req)))
		if len(req) < reqIdx+1 {
			utils.LogError(logger, nil, "no mysql mocks found for auth switch response")
			return res, fmt.Errorf("no mysql mocks found for auth switch response")
		}

		// Get the auth switch response from the mock
		logger.Debug("[DEBUG] Getting auth switch response mock", zap.Any("authSwitchRespMock", req[reqIdx].PacketBundle))
		authSwitchRespMock := req[reqIdx].PacketBundle

		logger.Debug("[DEBUG] Checking auth switch response mock type", zap.Any("expected", mysql.AuthSwithResponse), zap.Any("found", authSwitchRespMock.Header.Type))
		if authSwitchRespMock.Header.Type != mysql.AuthSwithResponse {
			utils.LogError(logger, nil, "expected auth switch response mock not found", zap.Any("found", authSwitchRespMock.Header.Type))
			return res, fmt.Errorf("expected %s but found %s", mysql.AuthSwithResponse, authSwitchRespMock.Header.Type)
		}

		// Since auth switch response data can be different, we should just check the sequence number
		logger.Debug("[DEBUG] Checking sequence number for auth switch response", zap.Any("expected", authSwitchRespMock.Header.Header.SequenceID), zap.Any("actual", authSwitchRespPkt.Header.SequenceID))
		if authSwitchRespMock.Header.Header.SequenceID != authSwitchRespPkt.Header.SequenceID {
			utils.LogError(logger, nil, "sequence number mismatch for auth switch response", zap.Any("expected", authSwitchRespMock.Header.Header.SequenceID), zap.Any("actual", authSwitchRespPkt.Header.SequenceID))
			return res, fmt.Errorf("sequence number mismatch for auth switch response")
		}

		logger.Debug("[DEBUG] Auth mechanism switched successfully")

		logger.Debug("[DEBUG] Incrementing reqIdx after auth switch response", zap.Int("reqIdx", reqIdx))
		reqIdx++

		// Get the next packet to decide the auth mechanism
		logger.Debug("[DEBUG] Checking for next response for auth mechanism after auth switch request", zap.Int("respIdx", respIdx), zap.Int("respLen", len(resp)))
		if len(resp) < respIdx+1 {
			utils.LogError(logger, nil, "no mysql mocks found for auth mechanism after auth switch request")
			return res, nil
		}

		logger.Debug("[DEBUG] Deciding auth mechanism after auth switch request", zap.Any("nextPacketType", resp[respIdx].Header.Type))
		authDecider = resp[respIdx].Header.Type
	}

	logger.Debug("[DEBUG] About to switch on authDecider", zap.Any("authDecider", authDecider))

	switch authDecider {
	case mysql.StatusToString(mysql.OK):
		logger.Debug("[DEBUG] Simulating native password", zap.Any("nativePassMocks.resp", resp[respIdx:]))
		var nativePassMocks reqResp
		nativePassMocks.resp = resp[respIdx:]

		// It means we need to simulate the native password
		err := simulateNativePassword(ctx, logger, clientConn, nativePassMocks, initialHandshakeMock, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate native password")
			logger.Debug("[DEBUG] Error simulating native password", zap.Error(err))
			return res, err
		}

	case mysql.AuthStatusToString(mysql.AuthMoreData):

		logger.Debug("[DEBUG] Simulating caching_sha2_password", zap.Any("cacheSha2PassMock.req", req[reqIdx:]), zap.Any("cacheSha2PassMock.resp", resp[respIdx:]))
		var cacheSha2PassMock reqResp
		cacheSha2PassMock.req = req[reqIdx:]
		cacheSha2PassMock.resp = resp[respIdx:]

		// It means we need to simulate the caching_sha2_password
		err := simulateCacheSha2Password(ctx, logger, clientConn, cacheSha2PassMock, initialHandshakeMock, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate caching_sha2_password")
			logger.Debug("[DEBUG] Error simulating caching_sha2_password", zap.Error(err))
			return res, err
		}
	}

	logger.Debug("[DEBUG] simulateInitialHandshake completed successfully")
	return res, nil
}

func simulateNativePassword(ctx context.Context, logger *zap.Logger, clientConn net.Conn, nativePassMocks reqResp, initialHandshakeMock *models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) error {

	logger.Debug("final response for native password", zap.Any("response", nativePassMocks.resp[0].Header.Type))

	// Send the final response (OK/Err) to the client
	buf, err := wire.EncodeToBinary(ctx, logger, &nativePassMocks.resp[0].PacketBundle, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to encode final response packet for native password")
		return err
	}

	_, err = clientConn.Write(buf)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		utils.LogError(logger, err, "failed to write final response for native password to the client")
		return err
	}

	//update the config mock (since it can be reused in case of more connections compared to record mode)
	ok := updateMock(ctx, logger, initialHandshakeMock, mockDb)
	if !ok {
		utils.LogError(logger, nil, "failed to update the mock unfiltered mock during native password")
	}

	logger.Debug("native password completed successfully")

	return nil
}

func simulateCacheSha2Password(ctx context.Context, logger *zap.Logger, clientConn net.Conn, cacheSha2PassMock reqResp, initialHandshakeMock *models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) error {
	logger.Debug("[DEBUG] Starting simulateCacheSha2Password", zap.Any("cacheSha2PassMock", cacheSha2PassMock))
	resp := cacheSha2PassMock.resp

	// Get the AuthMoreData
	logger.Debug("[DEBUG] Checking auth more data response length", zap.Int("respLen", len(resp)))
	if len(resp) < 1 {
		utils.LogError(logger, nil, "no mysql mocks found for auth more data")
	}

	//check if the response is of type AuthMoreData
	logger.Debug("[DEBUG] Checking if response is AuthMoreData packet", zap.Any("resp[0].Message", resp[0].Message))
	if _, ok := resp[0].Message.(*mysql.AuthMoreDataPacket); !ok {
		utils.LogError(logger, nil, "failed to assert auth more data packet")
		return fmt.Errorf("failed to get auth more data packet, expected %T but got %T", mysql.AuthMoreDataPacket{}, resp[0].Message)
	}

	// Get the auth more data packet
	logger.Debug("[DEBUG] Asserting auth more data packet")
	pkt, ok := resp[0].Message.(*mysql.AuthMoreDataPacket)
	if !ok {
		utils.LogError(logger, nil, "failed to assert auth more data packet")
		return nil
	}

	logger.Debug("[DEBUG] Got auth more data packet", zap.Any("pkt", pkt))
	CachingSha2PasswordMechanism := pkt.Data
	logger.Debug("[DEBUG] CachingSha2PasswordMechanism raw", zap.String("mechanism", CachingSha2PasswordMechanism), zap.Binary("mechanismBytes", []byte(CachingSha2PasswordMechanism)))
	
	// Convert the raw byte value to the proper string representation
	var mechanismString string
	if len(CachingSha2PasswordMechanism) > 0 {
		mechanismByte := CachingSha2PasswordMechanism[0]
		logger.Debug("[DEBUG] CachingSha2PasswordMechanism byte value", zap.Uint8("mechanismByte", mechanismByte))
		mechanismString = mysql.CachingSha2PasswordToString(mysql.CachingSha2Password(mechanismByte))
		logger.Debug("[DEBUG] CachingSha2PasswordMechanism converted", zap.String("mechanismString", mechanismString))
	} else {
		logger.Debug("[DEBUG] CachingSha2PasswordMechanism is empty")
		mechanismString = "UNKNOWN"
	}

	logger.Debug("[DEBUG] Encoding auth more data packet to binary", zap.Any("packet", resp[0].PacketBundle))
	authBuf, err := wire.EncodeToBinary(ctx, logger, &resp[0].PacketBundle, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to encode auth more data packet")
		logger.Debug("[DEBUG] Error encoding auth more data packet", zap.Error(err))
		return err
	}

	// Write the AuthMoreData packet to the client
	logger.Debug("[DEBUG] Writing auth more data packet to client", zap.Binary("authBuf", authBuf))
	_, err = clientConn.Write(authBuf)
	if err != nil {
		if ctx.Err() != nil {
			logger.Debug("[DEBUG] Context error after writing auth more data packet", zap.Error(ctx.Err()))
			return ctx.Err()
		}
		utils.LogError(logger, err, "failed to write auth more data or auth switch request to the client")
		logger.Debug("[DEBUG] Error writing auth more data to client", zap.Error(err))
		return err
	}

	logger.Debug("[DEBUG] Checking if there are enough response mocks", zap.Int("respLen", len(cacheSha2PassMock.resp)))
	if len(cacheSha2PassMock.resp) < 2 {
		utils.LogError(logger, nil, "response mock not found for caching_sha2_password after auth more data")
		return fmt.Errorf("response mock not found for caching_sha2_password after auth more data")
	}

	//update the cacheSha2PassMock resp
	logger.Debug("[DEBUG] Updating cacheSha2PassMock resp, removing first element")
	cacheSha2PassMock.resp = cacheSha2PassMock.resp[1:]
	logger.Debug("[DEBUG] Updated cacheSha2PassMock resp", zap.Any("updatedResp", cacheSha2PassMock.resp))

	//simulate the caching_sha2_password auth mechanism
	logger.Debug("[DEBUG] Switching on mechanismString", zap.String("mechanismString", mechanismString))
	switch mechanismString {
	case mysql.CachingSha2PasswordToString(mysql.PerformFullAuthentication):
		logger.Debug("[DEBUG] Simulating full authentication")
		err := simulateFullAuth(ctx, logger, clientConn, cacheSha2PassMock, initialHandshakeMock, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate full auth")
			logger.Debug("[DEBUG] Error simulating full auth", zap.Error(err))
			return err
		}
	case mysql.CachingSha2PasswordToString(mysql.FastAuthSuccess):
		logger.Debug("[DEBUG] Simulating fast auth success for caching_sha2_password")
		err := simulateFastAuthSuccess(ctx, logger, clientConn, cacheSha2PassMock, initialHandshakeMock, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate fast auth success")
			logger.Debug("[DEBUG] Error simulating fast auth success", zap.Error(err))
			return err
		}
	default:
		logger.Debug("[DEBUG] Unknown CachingSha2PasswordMechanism", zap.String("mechanismString", mechanismString), zap.String("rawMechanism", CachingSha2PasswordMechanism))
	}
	logger.Debug("[DEBUG] simulateCacheSha2Password completed successfully")
	return nil
}

func simulateFastAuthSuccess(ctx context.Context, logger *zap.Logger, clientConn net.Conn, fastAuthMocks reqResp, initialHandshakeMock *models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) error {
	logger.Debug("[DEBUG] Starting simulateFastAuthSuccess", zap.Any("fastAuthMocks", fastAuthMocks))
	resp := fastAuthMocks.resp

	logger.Debug("[DEBUG] Checking fast auth response length", zap.Int("respLen", len(resp)))
	if len(resp) < 1 {
		utils.LogError(logger, nil, "final response mock not found for fast auth success")
		return fmt.Errorf("final response mock not found for fast auth success")
	}

	logger.Debug("[DEBUG] Final response for fast auth success", zap.Any("response", resp[0].Header.Type))

	// Send the final response (OK/Err) to the client
	logger.Debug("[DEBUG] Encoding final response packet for fast auth success", zap.Any("packet", resp[0].PacketBundle))
	buf, err := wire.EncodeToBinary(ctx, logger, &resp[0].PacketBundle, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to encode final response packet for fast auth success")
		logger.Debug("[DEBUG] Error encoding final response packet for fast auth success", zap.Error(err))
		return err
	}

	logger.Debug("[DEBUG] Writing final response packet for fast auth success to client", zap.Binary("buf", buf))
	_, err = clientConn.Write(buf)
	if err != nil {
		if ctx.Err() != nil {
			logger.Debug("[DEBUG] Context error after writing final response for fast auth success", zap.Error(ctx.Err()))
			return ctx.Err()
		}
		utils.LogError(logger, err, "failed to write final response for fast auth success to the client")
		logger.Debug("[DEBUG] Error writing final response for fast auth success to client", zap.Error(err))
		return err
	}

	//update the config mock (since it can be reused in case of more connections compared to record mode)
	//TODO: need to check when updateMock is unsuccessful
	logger.Debug("[DEBUG] Updating mock for fast auth success", zap.Any("initialHandshakeMock", initialHandshakeMock))
	ok := updateMock(ctx, logger, initialHandshakeMock, mockDb)
	if !ok {
		utils.LogError(logger, nil, "failed to update the mock unfiltered mock during fast auth success")
		logger.Debug("[DEBUG] Failed to update mock during fast auth success")
	}

	logger.Debug("[DEBUG] Fast auth success completed successfully")

	return nil
}

func simulateFullAuth(ctx context.Context, logger *zap.Logger, clientConn net.Conn, fullAuthMocks reqResp, initialHandshakeMock *models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) error {
	logger.Debug("[DEBUG] Starting simulateFullAuth", zap.Any("fullAuthMocks", fullAuthMocks))

	resp := fullAuthMocks.resp
	req := fullAuthMocks.req

	logger.Debug("[DEBUG] Checking if SSL is used in full auth", zap.Bool("UseSSL", decodeCtx.UseSSL))
	if decodeCtx.UseSSL {
		logger.Debug("[DEBUG] This is an ssl request, simulating plain password in caching_sha2_password full auth")
		err := simulatePlainPassword(ctx, logger, clientConn, fullAuthMocks, initialHandshakeMock, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate plain password in caching_sha2_password full auth")
			logger.Debug("[DEBUG] Error simulating plain password in full auth", zap.Error(err))
			return err
		}
		return nil
	}

	// read the public key request from the client
	logger.Debug("[DEBUG] Reading public key request from client")
	publicKeyRequestBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read public key request from client")
		logger.Debug("[DEBUG] Error reading public key request from client", zap.Error(err))
		return err
	}

	// decode the public key request
	logger.Debug("[DEBUG] Decoding public key request", zap.Binary("publicKeyRequestBuf", publicKeyRequestBuf))
	pkt, err := wire.DecodePayload(ctx, logger, publicKeyRequestBuf, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode public key request from client")
		logger.Debug("[DEBUG] Error decoding public key request", zap.Error(err))
		return err
	}

	logger.Debug("[DEBUG] Asserting public key request packet", zap.Any("pkt.Message", pkt.Message))
	publicKey, ok := pkt.Message.(string)
	if !ok {
		utils.LogError(logger, nil, "failed to assert public key request packet")
		return nil
	}

	// Get the public key response from the mock
	logger.Debug("[DEBUG] Checking public key request mock length", zap.Int("reqLen", len(req)))
	if len(req) < 1 {
		utils.LogError(logger, nil, "no mysql mocks found for public key response")
		return fmt.Errorf("no mysql mocks found for public key response")
	}

	logger.Debug("[DEBUG] Asserting public key response mock", zap.Any("req[0].Message", req[0].Message))
	publicKeyMock, ok := req[0].Message.(string)
	if !ok {
		utils.LogError(logger, nil, "failed to assert public key response packet")
		return nil
	}

	// Match the header of the public key request
	logger.Debug("[DEBUG] Matching header for public key request", zap.Any("expected", req[0].Header.Header), zap.Any("actual", pkt.Header.Header))
	ok = matchHeader(*req[0].Header.Header, *pkt.Header.Header)
	if !ok {
		utils.LogError(logger, nil, "header mismatch for public key request", zap.Any("expected", req[0].Header.Header), zap.Any("actual", pkt.Header.Header))
		return nil
	}

	// Match the public key response from the client with the mock
	logger.Debug("[DEBUG] Matching public key", zap.String("actual", publicKey), zap.String("expected", publicKeyMock))
	if publicKey != publicKeyMock {
		utils.LogError(logger, nil, "public key mismatch", zap.Any("actual", publicKey), zap.Any("expected", publicKeyMock))
		return fmt.Errorf("public key mismatch")
	}

	// Get the AuthMoreData for sending the public key
	logger.Debug("[DEBUG] Checking auth more data response length for public key", zap.Int("respLen", len(resp)))
	if len(resp) < 1 {
		utils.LogError(logger, nil, "no mysql mocks found for auth more data (public key)")
		return fmt.Errorf("no mysql mocks found for auth more data (public key)")
	}

	// Get the AuthMoreData packet
	logger.Debug("[DEBUG] Asserting auth more data packet for public key", zap.Any("resp[0].Message", resp[0].Message))
	_, ok = resp[0].Message.(*mysql.AuthMoreDataPacket)
	if !ok {
		utils.LogError(logger, nil, "failed to assert auth more data packet (public key)")
		return nil
	}

	// encode the public key response
	logger.Debug("[DEBUG] Encoding public key response packet", zap.Any("packet", resp[0].PacketBundle))
	buf, err := wire.EncodeToBinary(ctx, logger, &resp[0].PacketBundle, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to encode public key response packet")
		logger.Debug("[DEBUG] Error encoding public key response packet", zap.Error(err))
		return err
	}

	// Write the public key response to the client
	logger.Debug("[DEBUG] Writing public key response to client", zap.Binary("buf", buf))
	_, err = clientConn.Write(buf)
	if err != nil {
		if ctx.Err() != nil {
			logger.Debug("[DEBUG] Context error after writing public key response", zap.Error(ctx.Err()))
			return ctx.Err()
		}
		utils.LogError(logger, err, "failed to write public key response to the client")
		logger.Debug("[DEBUG] Error writing public key response to client", zap.Error(err))
		return err
	}

	// Read the encrypted password from the client
	logger.Debug("[DEBUG] Reading encrypted password from client")
	encryptedPasswordBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read encrypted password from client")
		logger.Debug("[DEBUG] Error reading encrypted password from client", zap.Error(err))
		return err
	}

	// Get the packet from the buffer
	logger.Debug("[DEBUG] Converting encrypted password to packet", zap.Binary("encryptedPasswordBuf", encryptedPasswordBuf))
	encryptedPassPkt, err := mysqlUtils.BytesToMySQLPacket(encryptedPasswordBuf)
	if err != nil {
		utils.LogError(logger, err, "failed to convert encrypted password to packet")
		logger.Debug("[DEBUG] Error converting encrypted password to packet", zap.Error(err))
		return err
	}

	logger.Debug("[DEBUG] Checking encrypted password request mock length", zap.Int("reqLen", len(req)))
	if len(req) < 2 {
		utils.LogError(logger, nil, "no mysql mocks found for encrypted password during full auth")
		return fmt.Errorf("no mysql mocks found for encrypted password during full auth")
	}

	// Get the encrypted password from the mock
	logger.Debug("[DEBUG] Getting encrypted password mock", zap.Any("encryptedPassMock", req[1].PacketBundle))
	encryptedPassMock := req[1].PacketBundle

	logger.Debug("[DEBUG] Checking encrypted password mock type", zap.Any("expected", mysql.EncryptedPassword), zap.Any("found", encryptedPassMock.Header.Type))
	if encryptedPassMock.Header.Type != mysql.EncryptedPassword {
		utils.LogError(logger, nil, "expected encrypted password mock not found", zap.Any("found", encryptedPassMock.Header.Type))
		return fmt.Errorf("expected %s but found %s", mysql.EncryptedPassword, encryptedPassMock.Header.Type)
	}

	// Since encrypted password can be different, we should just check the sequence number
	logger.Debug("[DEBUG] Checking sequence number for encrypted password", zap.Any("expected", encryptedPassMock.Header.Header.SequenceID), zap.Any("actual", encryptedPassPkt.Header.SequenceID))
	if encryptedPassMock.Header.Header.SequenceID != encryptedPassPkt.Header.SequenceID {
		utils.LogError(logger, nil, "sequence number mismatch for encrypted password", zap.Any("expected", encryptedPassMock.Header.Header.SequenceID), zap.Any("actual", encryptedPassPkt.Header.SequenceID))
		return fmt.Errorf("sequence number mismatch for encrypted password")
	}

	//Now send the final response (OK/Err) to the client
	logger.Debug("[DEBUG] Checking final response mock length for full auth", zap.Int("respLen", len(resp)))
	if len(resp) < 2 {
		utils.LogError(logger, nil, "final response mock not found for full auth")
		return fmt.Errorf("final response mock not found for full auth")
	}

	logger.Debug("[DEBUG] Final response for full auth", zap.Any("response", resp[1].Header.Type))

	// Get the final response (OK/Err) from the mock
	// Send the final response (OK/Err) to the client
	logger.Debug("[DEBUG] Encoding final response packet for full auth", zap.Any("packet", resp[1].PacketBundle))
	buf, err = wire.EncodeToBinary(ctx, logger, &resp[1].PacketBundle, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to encode final response packet for full auth")
		logger.Debug("[DEBUG] Error encoding final response packet for full auth", zap.Error(err))
		return err
	}

	logger.Debug("[DEBUG] Writing final response for full auth to client", zap.Binary("buf", buf))
	_, err = clientConn.Write(buf)
	if err != nil {
		if ctx.Err() != nil {
			logger.Debug("[DEBUG] Context error after writing final response for full auth", zap.Error(ctx.Err()))
			return ctx.Err()
		}
		utils.LogError(logger, err, "failed to write final response for full auth to the client")
		logger.Debug("[DEBUG] Error writing final response for full auth to client", zap.Error(err))
		return err
	}

	// FullAuth mechanism only comes for the first time unless COM_CHANGE_USER is called (that is not supported for now).
	// Afterwards only fast auth success is expected. So, we can delete this.
	logger.Debug("[DEBUG] Deleting unfiltered mock during full auth", zap.Any("initialHandshakeMock", initialHandshakeMock))
	ok = mockDb.DeleteUnFilteredMock(*initialHandshakeMock)
	// TODO: need to check what to do in this case
	if !ok {
		utils.LogError(logger, nil, "failed to delete unfiltered mock during full auth")
		logger.Debug("[DEBUG] Failed to delete unfiltered mock during full auth")
	}

	logger.Debug("[DEBUG] Full auth completed successfully")

	return nil
}

func simulatePlainPassword(ctx context.Context, logger *zap.Logger, clientConn net.Conn, fullAuthMocks reqResp, initialHandshakeMock *models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) error {
	logger.Debug("[DEBUG] Starting simulatePlainPassword", zap.Any("fullAuthMocks", fullAuthMocks))

	req := fullAuthMocks.req
	resp := fullAuthMocks.resp

	// read the plain password from the client
	logger.Debug("[DEBUG] Reading plain password from client")
	plainPassBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read plain password from client")
		logger.Debug("[DEBUG] Error reading plain password from client", zap.Error(err))
		return err
	}

	// Get the packet from the buffer
	logger.Debug("[DEBUG] Converting plain password to packet", zap.Binary("plainPassBuf", plainPassBuf))
	plainPassPkt, err := mysqlUtils.BytesToMySQLPacket(plainPassBuf)
	if err != nil {
		utils.LogError(logger, err, "failed to convert plain password to packet")
		logger.Debug("[DEBUG] Error converting plain password to packet", zap.Error(err))
		return err
	}

	plainPass := string(intgUtils.EncodeBase64(plainPassPkt.Payload))
	logger.Debug("[DEBUG] Encoded plain password", zap.String("plainPass", plainPass))

	// Get the plain password from the mock
	logger.Debug("[DEBUG] Checking plain password request mock length", zap.Int("reqLen", len(req)))
	if len(req) < 1 {
		utils.LogError(logger, nil, "no mysql mocks found for plain password")
		return fmt.Errorf("no mysql mocks found for plain password")
	}

	logger.Debug("[DEBUG] Asserting plain password mock", zap.Any("req[0].Message", req[0].Message))
	plainPassMock, ok := req[0].Message.(string)
	if !ok {
		utils.LogError(logger, nil, "failed to assert plain password packet")
		return fmt.Errorf("failed to assert plain password packet")
	}

	// Match the header of the plain password
	logger.Debug("[DEBUG] Matching header for plain password", zap.Any("expected", req[0].Header.Header), zap.Any("actual", plainPassPkt.Header))
	ok = matchHeader(*req[0].Header.Header, plainPassPkt.Header)
	if !ok {
		utils.LogError(logger, nil, "header mismatch for plain password", zap.Any("expected", req[0].Header.Header), zap.Any("actual", plainPassPkt.Header))
		return fmt.Errorf("header mismatch for plain password")
	}

	// Match the plain password from the client with the mock
	logger.Debug("[DEBUG] Matching plain password", zap.String("actual", plainPass), zap.String("expected", plainPassMock))
	if plainPass != plainPassMock {
		utils.LogError(logger, nil, "plain password mismatch", zap.Any("actual", plainPass), zap.Any("expected", plainPassMock))
		return fmt.Errorf("plain password mismatch")
	}

	//Now send the final response (OK/Err) to the client
	logger.Debug("[DEBUG] Checking final response mock length for plain password", zap.Int("respLen", len(resp)))
	if len(resp) < 1 {
		utils.LogError(logger, nil, "final response mock not found for full auth (plain password)")
		return fmt.Errorf("final response mock not found for full auth (plain password)")
	}

	logger.Debug("[DEBUG] Final response for full auth(plain password)", zap.Any("response", resp[0].Header.Type))

	// Get the final response (OK/Err) from the mock
	// Send the final response (OK/Err) to the client
	logger.Debug("[DEBUG] Encoding final response packet for plain password", zap.Any("packet", resp[0].PacketBundle))
	buf, err := wire.EncodeToBinary(ctx, logger, &resp[0].PacketBundle, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to encode final response packet for full auth (plain password)")
		logger.Debug("[DEBUG] Error encoding final response packet for plain password", zap.Error(err))
		return err
	}

	logger.Debug("[DEBUG] Writing final response for plain password to client", zap.Binary("buf", buf))
	_, err = clientConn.Write(buf)
	if err != nil {
		if ctx.Err() != nil {
			logger.Debug("[DEBUG] Context error after writing final response for plain password", zap.Error(ctx.Err()))
			return ctx.Err()
		}
		utils.LogError(logger, err, "failed to write final response for full auth (plain password) to the client")
		logger.Debug("[DEBUG] Error writing final response for plain password to client", zap.Error(err))
		return err
	}

	// FullAuth mechanism only comes for the first time unless COM_CHANGE_USER is called (that is not supported for now).
	// Afterwards only fast auth success is expected. So, we can delete this.
	logger.Debug("[DEBUG] Deleting unfiltered mock during plain password", zap.Any("initialHandshakeMock", initialHandshakeMock))
	ok = mockDb.DeleteUnFilteredMock(*initialHandshakeMock)
	// TODO: need to check what to do in this case
	if !ok {
		utils.LogError(logger, nil, "failed to delete unfiltered mock during full auth (plain password) in ssl request")
		logger.Debug("[DEBUG] Failed to delete unfiltered mock during plain password")
	}

	logger.Debug("[DEBUG] Full auth (plain-password) in ssl request completed successfully")
	return nil
}
