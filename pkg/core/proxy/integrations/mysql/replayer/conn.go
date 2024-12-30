//go:build linux || windows

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
func simulateInitialHandshake(ctx context.Context, logger *zap.Logger, clientConn net.Conn, mocks []*models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) (handshakeRes, error) {
	// Get the mock for initial handshake
	initialHandshakeMock := mocks[0]

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

	// handle the SSL request
	if decodeCtx.UseSSL {
		_, ok := pkt.Message.(*mysql.SSLRequestPacket)
		if !ok {
			utils.LogError(logger, nil, "failed to assert SSL request packet")
			return res, nil
		}

		// Get the SSL request from the mock
		_, ok = req[reqIdx].Message.(*mysql.SSLRequestPacket)
		if !ok {
			utils.LogError(logger, nil, "failed to assert mock SSL request packet", zap.Any("expected", req[reqIdx].PacketBundle.Header.Type))
			return res, nil
		}

		// Match the SSL request from the client with the mock
		err = matchSSLRequest(ctx, logger, req[reqIdx].PacketBundle, *pkt)
		if err != nil {
			utils.LogError(logger, err, "error while matching SSL request")
			return res, err
		}
		reqIdx++ // matched with the mock so increment the index

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
			clientConn, err = pTls.HandleTLSConnection(ctx, logger, clientConn)
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

	_, ok = pkt.Message.(*mysql.HandshakeResponse41Packet)
	if !ok {
		utils.LogError(logger, nil, "failed to assert actual handshake response packet")
		return res, nil
	}

	// Get the handshake response from the mock
	_, ok = req[reqIdx].Message.(*mysql.HandshakeResponse41Packet)
	if !ok {
		utils.LogError(logger, nil, "failed to assert mock handshake response packet")
		return res, nil
	}

	// Match the handshake response from the client with the mock
	logger.Debug("matching handshake response", zap.Any("actual", pkt), zap.Any("mock", req[reqIdx].PacketBundle))
	err = matchHanshakeResponse41(ctx, logger, req[reqIdx].PacketBundle, *pkt)
	if err != nil {
		utils.LogError(logger, err, "error while matching handshakeResponse41")
		return res, err
	}
	reqIdx++ // matched with the mock so increment the index

	// Get the next response in order to find the auth mechanism
	if len(resp) < respIdx+1 {
		utils.LogError(logger, nil, "no mysql mocks found for auth mechanism")
		return res, nil
	}

	// Get the next packet to decide the auth mechanism or auth switching
	// For Native password: next packet is Ok/Err
	// For CachingSha2 password: next packet is AuthMoreData

	authDecider := resp[respIdx].PacketBundle.Header.Type

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

		authDecider = resp[respIdx].PacketBundle.Header.Type
	}

	switch authDecider {
	case mysql.StatusToString(mysql.OK):
		var nativePassMocks reqResp
		nativePassMocks.resp = resp[respIdx:]

		// It means we need to simulate the native password
		err := simulateNativePassword(ctx, logger, clientConn, nativePassMocks, initialHandshakeMock, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate native password")
			return res, err
		}

	case mysql.AuthStatusToString(mysql.AuthMoreData):

		var cacheSha2PassMock reqResp
		cacheSha2PassMock.req = req[reqIdx:]
		cacheSha2PassMock.resp = resp[respIdx:]

		// It means we need to simulate the caching_sha2_password
		err := simulateCacheSha2Password(ctx, logger, clientConn, cacheSha2PassMock, initialHandshakeMock, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate caching_sha2_password")
			return res, err
		}
	}

	return res, nil
}

func simulateNativePassword(ctx context.Context, logger *zap.Logger, clientConn net.Conn, nativePassMocks reqResp, initialHandshakeMock *models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) error {

	logger.Debug("final response for native password", zap.Any("response", nativePassMocks.resp[0].PacketBundle.Header.Type))

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

	CachingSha2PasswordMechanism := pkt.Data

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
	switch CachingSha2PasswordMechanism {
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
	}
	return nil
}

func simulateFastAuthSuccess(ctx context.Context, logger *zap.Logger, clientConn net.Conn, fastAuthMocks reqResp, initialHandshakeMock *models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) error {
	resp := fastAuthMocks.resp

	if len(resp) < 1 {
		utils.LogError(logger, nil, "final response mock not found for fast auth success")
		return fmt.Errorf("final response mock not found for fast auth success")
	}

	logger.Debug("final response for fast auth success", zap.Any("response", resp[0].PacketBundle.Header.Type))

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
	ok = matchHeader(*req[0].PacketBundle.Header.Header, *pkt.Header.Header)
	if !ok {
		utils.LogError(logger, nil, "header mismatch for public key request", zap.Any("expected", req[0].PacketBundle.Header.Header), zap.Any("actual", pkt.Header.Header))
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
	_, ok = resp[0].PacketBundle.Message.(*mysql.AuthMoreDataPacket)
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

	logger.Debug("final response for full auth", zap.Any("response", resp[1].PacketBundle.Header.Type))

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
