package replayer

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	mysqlUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	intgUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	pUtils "go.keploy.io/server/v3/pkg/agent/proxy/util"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

type reqResp struct {
	req  []mysql.Request
	resp []mysql.Response
}

type handshakeRes struct {
	tlsClientConn net.Conn
}

// CAVEAT: We haven't handled the case where clients connect to entirely different MySQL servers.
// However, we do handle scenarios where multiple clients connect to the same server
// but use different databases or usernames.

// Replay mode
func simulateInitialHandshake(ctx context.Context, logger *zap.Logger, clientConn net.Conn, mocks []*models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext, opts models.OutgoingOptions) (handshakeRes, error) {
	// Get the mock for initial handshake
	initialHandshakeMock := mocks[0]

	for i, mock := range mocks {
		if i == 0 {
			logger.Debug("Using initial handshake mock", zap.Int("index", i), zap.String("mock_name", mock.Name), zap.String("conn_id", mock.Spec.Metadata["connID"]))
			continue
		}
		logger.Debug("Config mocks available", zap.Int("index", i), zap.String("mock_name", mock.Name), zap.String("conn_id", mock.Spec.Metadata["connID"]))
	}

	// Read the intial request and response for the handshake from the mocks
	resp := initialHandshakeMock.Spec.MySQLResponses
	req := initialHandshakeMock.Spec.MySQLRequests

	res := handshakeRes{}
	reqIdx, respIdx := 0, 0

	if len(resp) == 0 || len(req) == 0 {
		utils.LogError(logger, nil, "no mysql mocks found for initial handshake")
		return res, nil
	}

	handshake, ok := resp[respIdx].Message.(*mysql.HandshakeV10Packet)
	if !ok {
		utils.LogError(logger, nil, "failed to assert handshake packet")
		return res, nil
	}

	// Store the server greetings
	decodeCtx.ServerGreetings.Store(clientConn, handshake)

	// Set the intial auth plugin
	decodeCtx.PluginName = handshake.AuthPluginName
	decodeCtx.ServerCaps = handshake.CapabilityFlags
	var err error

	// encode the response
	buf, err := wire.EncodeToBinary(ctx, logger, &resp[respIdx].PacketBundle, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to encode handshake packet")
		return res, err
	}

	// Write the initial handshake to the client
	_, err = clientConn.Write(buf)
	if err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write server greetings to the client")

		return res, err
	}

	respIdx++

	// Read the client request, (handshake response or ssl request)
	handshakeResponseBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read handshake response from client")
		return res, err
	}

	// Decode the handshakeResponse or sslRequest
	pkt, err := wire.DecodePayload(ctx, logger, handshakeResponseBuf, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode handshake response from client")
		return res, err
	}

	// NEW: holders for SSL-first-packet and SSL-matched candidates
	var sslFirstPacket *mysql.PacketBundle
	var sslMatchedMocks []*models.Mock

	// handle the SSL request
	if decodeCtx.UseSSL {
		_, ok := pkt.Message.(*mysql.SSLRequestPacket)
		if !ok {
			utils.LogError(logger, nil, "failed to assert SSL request packet")
			return res, nil
		}

		// NEW: Strictly collect all mocks whose requests[0] match this SSLRequest.
		cp := *pkt
		sslFirstPacket = &cp

		for _, m := range mocks {
			mReq := m.Spec.MySQLRequests
			if len(mReq) == 0 {
				continue
			}
			// NEW: direct constant, not ...ToString
			if mReq[0].Header.Type != mysql.SSLRequest {
				continue
			}
			if err := matchSSLRequest(ctx, logger, mReq[0].PacketBundle, *sslFirstPacket); err == nil {
				sslMatchedMocks = append(sslMatchedMocks, m)
			}
		}

		// NEW: If no SSL matches at all, fail immediately (as requested).
		if len(sslMatchedMocks) == 0 {
			utils.LogError(logger, nil, "no mysql mocks matched the SSL request")
			return res, fmt.Errorf("no mysql mocks matched the SSL request")
		}

		reqIdx++ // matched (logically) with the mock so increment the index

		// Upgrade the client connection to TLS
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
		}

		// Update this tls connection information in the handshake result
		res.tlsClientConn = clientConn

		// Store (Reset) the last operation for the upgraded client connection, because after ssl request the client will send the handshake response packet again.
		decodeCtx.LastOp.Store(clientConn, mysql.HandshakeV10)

		// Store the server greeting packet for the upgraded client connection
		decodeCtx.ServerGreetings.Store(clientConn, handshake)

		// read the actual handshake response packet
		handshakeResponseBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
		if err != nil {
			utils.LogError(logger, err, "failed to read handshake response from client")
			return res, err
		}

		// Decode the handshakeResponse
		pkt, err = wire.DecodePayload(ctx, logger, handshakeResponseBuf, clientConn, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to decode handshake response from client")
			return res, err
		}
	}

	// At this point, pkt MUST be HandshakeResponse41 (either SSL path post-decrypt or non-SSL path).
	hr41, ok := pkt.Message.(*mysql.HandshakeResponse41Packet)
	if !ok {
		utils.LogError(logger, nil, "failed to assert actual handshake response packet")
		return res, nil
	}
	decodeCtx.ClientCaps = hr41.CapabilityFlags // live client caps

	// NEW: Build a single candidate list:
	// - If SSL was used: match HR41 within sslMatchedMocks only.
	// - Else: match HR41 across all mocks.
	candidates := mocks
	if sslFirstPacket != nil {
		candidates = sslMatchedMocks
	}

	// NEW: Strictly find the first candidate whose HR41 matches (auth_response ignored in matcher).
	// Try HR41 at index 0 and 1 to be tolerant to recording layout.

	logger.Debug("matching handshake response", zap.Any("actual request", pkt))

	selectedIdx := -1
	hrIdx := -1
	for i, m := range candidates {
		mReq := m.Spec.MySQLRequests
		if len(mReq) > 0 && mReq[0].Header.Type == mysql.HandshakeResponse41 {
			// attempt match
			if err := matchHanshakeResponse41(ctx, logger, mReq[0].PacketBundle, *pkt); err == nil {
				selectedIdx = i
				hrIdx = 0
				break
			}
		}
		if len(mReq) > 1 && mReq[1].Header.Type == mysql.HandshakeResponse41 {
			if err := matchHanshakeResponse41(ctx, logger, mReq[1].PacketBundle, *pkt); err == nil {
				selectedIdx = i
				hrIdx = 1
				break
			}
		}
	}

	// NEW: If nothing matched HR41 strictly, error out.
	if selectedIdx == -1 {
		if sslFirstPacket != nil {
			utils.LogError(logger, nil, "no mysql mocks matched the HandshakeResponse41 within SSL-selected mocks")
			return res, fmt.Errorf("no mysql mocks matched the HandshakeResponse41 within SSL-selected mocks")
		}
		utils.LogError(logger, nil, "no mysql mocks matched the HandshakeResponse41")
		return res, fmt.Errorf("no mysql mocks matched the HandshakeResponse41")
	}

	// NEW: We have a strict HR41 match at candidates[selectedIdx], request index hrIdx.
	handshakeMatchedIdx := selectedIdx
	handshakeMock := candidates[handshakeMatchedIdx]

	// Update both responses and requests from the ultimately picked mock
	resp = handshakeMock.Spec.MySQLResponses
	req = handshakeMock.Spec.MySQLRequests

	// Once successful match of handshakeResponse41 is done the `reqIdx` can continue to increment just like how it is getting incremented.
	reqIdx = hrIdx + 1 // we have consumed HR41 at hrIdx
	respIdx = 1        // next server packet after HandshakeV10 is at responses[1]

	logger.Debug("picked mock for mysql handshake", zap.String("mock_name", handshakeMock.Name))

	// Get the handshake response from the mock (we have advanced past HR41, so look back one)
	if reqIdx-1 < 0 || reqIdx-1 >= len(req) {
		utils.LogError(logger, nil, "handshake response index out of range after selection")
		return res, nil
	}
	hrec, ok := req[reqIdx-1].Message.(*mysql.HandshakeResponse41Packet)
	if !ok {
		utils.LogError(logger, nil, "failed to assert mock handshake response packet")
		return res, nil
	}
	decodeCtx.RecordedClientCaps = hrec.CapabilityFlags

	// Get the next response in order to find the auth mechanism
	if len(resp) < respIdx+1 {
		utils.LogError(logger, nil, "no mysql mocks found for auth mechanism")
		return res, nil
	}

	// Get the next packet to decide the auth mechanism or auth switching
	// For Native password: next packet is Ok/Err
	// For CachingSha2 password: next packet is AuthMoreData

	authDecider := resp[respIdx].Header.Type

	// Check if the next packet is AuthSwitchRequest
	// Server sends AuthSwitchRequest when it wants to switch the auth mechanism
	if authDecider == mysql.AuthStatusToString(mysql.AuthSwitchRequest) {
		logger.Debug("Auth switch request found, switching the auth mechanism")

		// Get the AuthSwitchRequest packet
		authSwithReqPkt, ok := resp[respIdx].Message.(*mysql.AuthSwitchRequestPacket)
		if !ok {
			utils.LogError(logger, nil, "failed to assert auth switch request packet")
			return res, nil
		}

		// Change the auth plugin name
		decodeCtx.PluginName = authSwithReqPkt.PluginName

		// Encode the AuthSwitchRequest packet
		buf, err = wire.EncodeToBinary(ctx, logger, &resp[respIdx].PacketBundle, clientConn, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to encode auth switch request packet")
			return res, err
		}

		// Write the AuthSwitchRequest packet to the client
		_, err = clientConn.Write(buf)
		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			utils.LogError(logger, err, "failed to write auth switch request to the client")
			return res, err
		}

		respIdx++

		// Read the auth switch response from the client
		authSwitchRespBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
		if err != nil {
			utils.LogError(logger, err, "failed to read auth switch response from the client")
			return res, err
		}

		// Get the packet from the buffer
		authSwitchRespPkt, err := mysqlUtils.BytesToMySQLPacket(authSwitchRespBuf)
		if err != nil {
			utils.LogError(logger, err, "failed to convert auth switch response to packet")
			return res, err
		}

		if len(req) < reqIdx+1 {
			utils.LogError(logger, nil, "no mysql mocks found for auth switch response")
			return res, fmt.Errorf("no mysql mocks found for auth switch response")
		}

		// Get the auth switch response from the mock
		authSwitchRespMock := req[reqIdx].PacketBundle

		if authSwitchRespMock.Header.Type != mysql.AuthSwithResponse {
			utils.LogError(logger, nil, "expected auth switch response mock not found", zap.Any("found", authSwitchRespMock.Header.Type))
			return res, fmt.Errorf("expected %s but found %s", mysql.AuthSwithResponse, authSwitchRespMock.Header.Type)
		}

		// Since auth switch response data can be different, we should just check the sequence number
		if authSwitchRespMock.Header.Header.SequenceID != authSwitchRespPkt.Header.SequenceID {
			utils.LogError(logger, nil, "sequence number mismatch for auth switch response", zap.Any("expected", authSwitchRespMock.Header.Header.SequenceID), zap.Any("actual", authSwitchRespPkt.Header.SequenceID))
			return res, fmt.Errorf("sequence number mismatch for auth switch response")
		}

		logger.Debug("auth mechanism switched successfully")

		reqIdx++

		// Get the next packet to decide the auth mechanism
		if len(resp) < respIdx+1 {
			utils.LogError(logger, nil, "no mysql mocks found for auth mechanism after auth switch request")
			return res, nil
		}

		authDecider = resp[respIdx].Header.Type
	}

	switch authDecider {
	case mysql.StatusToString(mysql.OK):
		var nativePassMocks reqResp
		nativePassMocks.resp = resp[respIdx:]

		// It means we need to simulate the native password
		err := simulateNativePassword(ctx, logger, clientConn, nativePassMocks, handshakeMock, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate native password")
			return res, err
		}

	case mysql.AuthStatusToString(mysql.AuthMoreData):

		var cacheSha2PassMock reqResp
		cacheSha2PassMock.req = req[reqIdx:]
		cacheSha2PassMock.resp = resp[respIdx:]

		// It means we need to simulate the caching_sha2_password
		err := simulateCacheSha2Password(ctx, logger, clientConn, cacheSha2PassMock, handshakeMock, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate caching_sha2_password")
			return res, err
		}
	}

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
	resp := cacheSha2PassMock.resp

	// Get the AuthMoreData
	if len(resp) < 1 {
		utils.LogError(logger, nil, "no mysql mocks found for auth more data")
	}

	//check if the response is of type AuthMoreData
	if _, ok := resp[0].Message.(*mysql.AuthMoreDataPacket); !ok {
		utils.LogError(logger, nil, "failed to assert auth more data packet")
		return fmt.Errorf("failed to get auth more data packet, expected %T but got %T", mysql.AuthMoreDataPacket{}, resp[0].Message)
	}

	// Get the auth more data packet
	pkt, ok := resp[0].Message.(*mysql.AuthMoreDataPacket)
	if !ok {
		utils.LogError(logger, nil, "failed to assert auth more data packet")
		return nil
	}

	var mechanismString string
	CachingSha2PasswordMechanism := pkt.Data

	logger.Debug("[DEBUG] CachingSha2PasswordMechanism CachingSha2PasswordMechanism", zap.String("mechanism", CachingSha2PasswordMechanism), zap.Binary("mechanismBytes", []byte(CachingSha2PasswordMechanism)))

	if len(CachingSha2PasswordMechanism) == 1 {
		// CachingSha2PasswordMechanism single byte -> map to symbolic string
		b := CachingSha2PasswordMechanism[0]
		logger.Debug("[DEBUG] CachingSha2PasswordMechanism byte value", zap.Uint8("mechanismByte", b))
		mechanismString = mysql.CachingSha2PasswordToString(mysql.CachingSha2Password(b))
	} else if len(CachingSha2PasswordMechanism) > 1 {
		// already symbolic
		mechanismString = CachingSha2PasswordMechanism
	} else {
		mechanismString = "UNKNOWN"
	}
	logger.Debug("[DEBUG] CachingSha2PasswordMechanism normalized", zap.String("mechanismString", mechanismString))

	authBuf, err := wire.EncodeToBinary(ctx, logger, &resp[0].PacketBundle, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to encode auth more data packet")
		return err
	}

	// Write the AuthMoreData packet to the client
	_, err = clientConn.Write(authBuf)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		utils.LogError(logger, err, "failed to write auth more data or auth switch request to the client")
		return err
	}

	if len(cacheSha2PassMock.resp) < 2 {
		utils.LogError(logger, nil, "response mock not found for caching_sha2_password after auth more data")
		return fmt.Errorf("response mock not found for caching_sha2_password after auth more data")
	}

	//update the cacheSha2PassMock resp
	cacheSha2PassMock.resp = cacheSha2PassMock.resp[1:]

	//simulate the caching_sha2_password auth mechanism
	switch mechanismString {
	case mysql.CachingSha2PasswordToString(mysql.PerformFullAuthentication):
		err := simulateFullAuth(ctx, logger, clientConn, cacheSha2PassMock, initialHandshakeMock, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate full auth")
			return err
		}
	case mysql.CachingSha2PasswordToString(mysql.FastAuthSuccess):
		err := simulateFastAuthSuccess(ctx, logger, clientConn, cacheSha2PassMock, initialHandshakeMock, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate fast auth success")
			return err
		}
	default:
		// return an error
		utils.LogError(logger, nil, "unknown caching_sha2_password mechanism", zap.String("mechanism", mechanismString))
		return fmt.Errorf("unknown caching_sha2_password mechanism: %s", mechanismString)
	}
	return nil
}

func simulateFastAuthSuccess(ctx context.Context, logger *zap.Logger, clientConn net.Conn, fastAuthMocks reqResp, initialHandshakeMock *models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) error {
	resp := fastAuthMocks.resp

	if len(resp) < 1 {
		utils.LogError(logger, nil, "final response mock not found for fast auth success")
		return fmt.Errorf("final response mock not found for fast auth success")
	}

	logger.Debug("final response for fast auth success", zap.Any("response", resp[0].Header.Type))

	// Send the final response (OK/Err) to the client
	buf, err := wire.EncodeToBinary(ctx, logger, &resp[0].PacketBundle, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to encode final response packet for fast auth success")
		return err
	}

	_, err = clientConn.Write(buf)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		utils.LogError(logger, err, "failed to write final response for fast auth success to the client")
		return err
	}

	//update the config mock (since it can be reused in case of more connections compared to record mode)
	//TODO: need to check when updateMock is unsuccessful
	ok := updateMock(ctx, logger, initialHandshakeMock, mockDb)
	if !ok {
		utils.LogError(logger, nil, "failed to update the mock unfiltered mock during fast auth success")
	}

	logger.Debug("fast auth success completed successfully")

	return nil
}

func simulateFullAuth(ctx context.Context, logger *zap.Logger, clientConn net.Conn, fullAuthMocks reqResp, initialHandshakeMock *models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) error {

	resp := fullAuthMocks.resp
	req := fullAuthMocks.req

	if decodeCtx.UseSSL {
		logger.Debug("This is an ssl request, simulating plain password in caching_sha2_password full auth")
		err := simulatePlainPassword(ctx, logger, clientConn, fullAuthMocks, initialHandshakeMock, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate plain password in caching_sha2_password full auth")
			return err
		}
		return nil
	}

	// read the public key request from the client
	publicKeyRequestBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read public key request from client")
		return err
	}

	// decode the public key request
	pkt, err := wire.DecodePayload(ctx, logger, publicKeyRequestBuf, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode public key request from client")
		return err
	}

	publicKey, ok := pkt.Message.(string)
	if !ok {
		utils.LogError(logger, nil, "failed to assert public key request packet")
		return nil
	}

	// Get the public key response from the mock
	if len(req) < 1 {
		utils.LogError(logger, nil, "no mysql mocks found for public key response")
		return fmt.Errorf("no mysql mocks found for public key response")
	}

	publicKeyMock, ok := req[0].Message.(string)
	if !ok {
		utils.LogError(logger, nil, "failed to assert public key response packet")
		return nil
	}

	// Match the header of the public key request
	ok = matchHeader(*req[0].Header.Header, *pkt.Header.Header)
	if !ok {
		utils.LogError(logger, nil, "header mismatch for public key request", zap.Any("expected", req[0].Header.Header), zap.Any("actual", pkt.Header.Header))
		return nil
	}

	// Match the public key response from the client with the mock
	if publicKey != publicKeyMock {
		utils.LogError(logger, nil, "public key mismatch", zap.Any("actual", publicKey), zap.Any("expected", publicKeyMock))
		return fmt.Errorf("public key mismatch")
	}

	// Get the AuthMoreData for sending the public key
	if len(resp) < 1 {
		utils.LogError(logger, nil, "no mysql mocks found for auth more data (public key)")
		return fmt.Errorf("no mysql mocks found for auth more data (public key)")
	}

	// Get the AuthMoreData packet
	_, ok = resp[0].Message.(*mysql.AuthMoreDataPacket)
	if !ok {
		utils.LogError(logger, nil, "failed to assert auth more data packet (public key)")
		return nil
	}

	// encode the public key response
	buf, err := wire.EncodeToBinary(ctx, logger, &resp[0].PacketBundle, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to encode public key response packet")
		return err
	}

	// Write the public key response to the client
	_, err = clientConn.Write(buf)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		utils.LogError(logger, err, "failed to write public key response to the client")
		return err
	}

	// Read the encrypted password from the client

	encryptedPasswordBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read encrypted password from client")
		return err
	}

	// Get the packet from the buffer
	encryptedPassPkt, err := mysqlUtils.BytesToMySQLPacket(encryptedPasswordBuf)
	if err != nil {
		utils.LogError(logger, err, "failed to convert encrypted password to packet")
		return err
	}

	if len(req) < 2 {
		utils.LogError(logger, nil, "no mysql mocks found for encrypted password during full auth")
		return fmt.Errorf("no mysql mocks found for encrypted password during full auth")
	}

	// Get the encrypted password from the mock
	encryptedPassMock := req[1].PacketBundle

	if encryptedPassMock.Header.Type != mysql.EncryptedPassword {
		utils.LogError(logger, nil, "expected encrypted password mock not found", zap.Any("found", encryptedPassMock.Header.Type))
		return fmt.Errorf("expected %s but found %s", mysql.EncryptedPassword, encryptedPassMock.Header.Type)
	}

	// Since encrypted password can be different, we should just check the sequence number
	if encryptedPassMock.Header.Header.SequenceID != encryptedPassPkt.Header.SequenceID {
		utils.LogError(logger, nil, "sequence number mismatch for encrypted password", zap.Any("expected", encryptedPassMock.Header.Header.SequenceID), zap.Any("actual", encryptedPassPkt.Header.SequenceID))
		return fmt.Errorf("sequence number mismatch for encrypted password")
	}

	//Now send the final response (OK/Err) to the client
	if len(resp) < 2 {
		utils.LogError(logger, nil, "final response mock not found for full auth")
		return fmt.Errorf("final response mock not found for full auth")
	}

	logger.Debug("final response for full auth", zap.Any("response", resp[1].Header.Type))

	// Get the final response (OK/Err) from the mock
	// Send the final response (OK/Err) to the client
	buf, err = wire.EncodeToBinary(ctx, logger, &resp[1].PacketBundle, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to encode final response packet for full auth")
		return err
	}

	_, err = clientConn.Write(buf)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		utils.LogError(logger, err, "failed to write final response for full auth to the client")
		return err
	}

	// FullAuth mechanism only comes for the first time unless COM_CHANGE_USER is called (that is not supported for now).
	// Afterwards only fast auth success is expected. So, we can delete this.
	ok = mockDb.DeleteUnFilteredMock(*initialHandshakeMock)
	// TODO: need to check what to do in this case
	if !ok {
		utils.LogError(logger, nil, "failed to delete unfiltered mock during full auth")
	}

	logger.Debug("full auth completed successfully")

	return nil
}

func simulatePlainPassword(ctx context.Context, logger *zap.Logger, clientConn net.Conn, fullAuthMocks reqResp, initialHandshakeMock *models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) error {

	req := fullAuthMocks.req
	resp := fullAuthMocks.resp

	// read the plain password from the client
	plainPassBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read plain password from client")
		return err
	}

	// Get the packet from the buffer
	plainPassPkt, err := mysqlUtils.BytesToMySQLPacket(plainPassBuf)
	if err != nil {
		utils.LogError(logger, err, "failed to convert plain password to packet")
		return err
	}

	plainPass := string(intgUtils.EncodeBase64(plainPassPkt.Payload))

	// Get the plain password from the mock
	if len(req) < 1 {
		utils.LogError(logger, nil, "no mysql mocks found for plain password")
		return fmt.Errorf("no mysql mocks found for plain password")
	}

	plainPassMock, ok := req[0].Message.(string)
	if !ok {
		utils.LogError(logger, nil, "failed to assert plain password packet")
		return fmt.Errorf("failed to assert plain password packet")
	}

	// Match the header of the plain password
	ok = matchHeader(*req[0].Header.Header, plainPassPkt.Header)
	if !ok {
		utils.LogError(logger, nil, "header mismatch for plain password", zap.Any("expected", req[0].Header.Header), zap.Any("actual", plainPassPkt.Header))
		return fmt.Errorf("header mismatch for plain password")
	}

	// Match the plain password from the client with the mock
	if plainPass != plainPassMock {
		utils.LogError(logger, nil, "plain password mismatch", zap.Any("actual", plainPass), zap.Any("expected", plainPassMock))
		return fmt.Errorf("plain password mismatch")
	}

	//Now send the final response (OK/Err) to the client
	if len(resp) < 1 {
		utils.LogError(logger, nil, "final response mock not found for full auth (plain password)")
		return fmt.Errorf("final response mock not found for full auth (plain password)")
	}

	logger.Debug("final response for full auth(plain password)", zap.Any("response", resp[0].Header.Type))

	// Get the final response (OK/Err) from the mock
	// Send the final response (OK/Err) to the client
	buf, err := wire.EncodeToBinary(ctx, logger, &resp[0].PacketBundle, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to encode final response packet for full auth (plain password)")
		return err
	}

	_, err = clientConn.Write(buf)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		utils.LogError(logger, err, "failed to write final response for full auth (plain password) to the client")
		return err
	}

	// FullAuth mechanism only comes for the first time unless COM_CHANGE_USER is called (that is not supported for now).
	// Afterwards only fast auth success is expected. So, we can delete this.
	ok = mockDb.DeleteUnFilteredMock(*initialHandshakeMock)
	// TODO: need to check what to do in this case
	if !ok {
		utils.LogError(logger, nil, "failed to delete unfiltered mock during full auth (plain password) in ssl request")
	}

	logger.Debug("full auth (plain-password) in ssl request completed successfully")
	return nil
}
