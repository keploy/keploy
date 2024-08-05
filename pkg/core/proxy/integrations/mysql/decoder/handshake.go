//go:build linux

package decoder

// import (
// 	"context"
// 	"encoding/base64"
// 	"fmt"
// 	"net"

// 	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
// 	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/operation"
// 	mysqlUtils "go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
// 	"go.keploy.io/server/v2/pkg/models"
// 	"go.keploy.io/server/v2/pkg/models/mysql"
// 	"go.keploy.io/server/v2/utils"
// 	"go.uber.org/zap"
// )

// func simulateInitialHandshake(ctx context.Context, logger *zap.Logger, clientConn net.Conn, mocks []*models.Mock, mockDb integrations.MockMemDb, decodeCtx *operation.DecodeContext) error {
// 	// Get the mock for initial handshake
// 	initialHandshakeMock := mocks[0]

// 	// Read the intial request and response for the handshake from the mocks
// 	resp := initialHandshakeMock.Spec.MySQLResponses
// 	req := initialHandshakeMock.Spec.MySQLRequests

// 	if len(resp) == 0 || len(req) == 0 {
// 		utils.LogError(logger, nil, "no mysql mocks found for initial handshake")
// 		return nil
// 	}

// 	handshake, ok := resp[0].Message.(*mysql.HandshakeV10Packet)
// 	if !ok {
// 		utils.LogError(logger, nil, "failed to assert handshake packet")
// 		return nil
// 	}

// 	// Store the server greetings
// 	decodeCtx.ServerGreetings.Store(clientConn, handshake)

// 	// encode the response
// 	buf, err := operation.EncodeToBinary(ctx, logger, &resp[0].PacketBundle)
// 	if err != nil {
// 		utils.LogError(logger, err, "failed to encode handshake packet")
// 		return err
// 	}

// 	// Write the initial handshake to the client
// 	_, err = clientConn.Write(buf)
// 	if err != nil {
// 		if ctx.Err() != nil {
// 			return ctx.Err()
// 		}
// 		utils.LogError(logger, err, "failed to write server greetings to the client")

// 		return err
// 	}

// 	// Read the client request
// 	handshakeResponseBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
// 	if err != nil {
// 		utils.LogError(logger, err, "failed to read handshake response from client")
// 		return err
// 	}

// 	// Decode the handshakeResponse
// 	pkt, err := operation.DecodePayload(ctx, logger, handshakeResponseBuf, clientConn, decodeCtx)
// 	if err != nil {
// 		utils.LogError(logger, err, "failed to decode handshake response from client")
// 		return err
// 	}

// 	_, ok = pkt.Message.(*mysql.HandshakeResponse41Packet)
// 	if !ok {
// 		utils.LogError(logger, nil, "failed to assert actual handshake response packet")
// 		return nil
// 	}

// 	// Get the handshake response from the mock
// 	_, ok = req[0].Message.(*mysql.HandshakeResponse41Packet)
// 	if !ok {
// 		utils.LogError(logger, nil, "failed to assert mock handshake response packet")
// 		return nil
// 	}

// 	// Match the handshake response from the client with the mock
// 	//debug log
// 	logger.Info("matching handshake response", zap.Any("actual", pkt), zap.Any("mock", req[0].PacketBundle))

// 	// Get the AuthMoreData or AuthSwitchRequest packet
// 	if len(resp) < 2 {
// 		utils.LogError(logger, nil, "no mysql mocks found for auth more data or auth switch request")
// 	}

// 	// Get the AuthMore or AuthSwitchRequest packet
// 	var authBuf []byte
// 	var CachingSha2PasswordMechanism string

// 	switch resp[1].Message.(type) {
// 	case *mysql.AuthSwitchRequestPacket:
// 		// Get the auth switch request packet
// 		pkt, ok := resp[1].Message.(*mysql.AuthSwitchRequestPacket)
// 		if !ok {
// 			utils.LogError(logger, nil, "failed to assert auth switch request packet")
// 			return nil
// 		}

// 		CachingSha2PasswordMechanism = pkt.PluginData

// 		authBuf, err = operation.EncodeToBinary(ctx, logger, &resp[1].PacketBundle)
// 		if err != nil {
// 			utils.LogError(logger, err, "failed to encode auth switch request packet")
// 			return err
// 		}
// 	case *mysql.AuthMoreDataPacket:
// 		// Get the auth more data packet
// 		pkt, ok := resp[1].Message.(*mysql.AuthMoreDataPacket)
// 		if !ok {
// 			utils.LogError(logger, nil, "failed to assert auth more data packet")
// 			return nil
// 		}

// 		CachingSha2PasswordMechanism = pkt.Data

// 		authBuf, err = operation.EncodeToBinary(ctx, logger, &resp[1].PacketBundle)
// 		if err != nil {
// 			utils.LogError(logger, err, "failed to encode auth more data packet")
// 			return err
// 		}
// 	}

// 	// Write the AuthMore or AuthSwitchRequest packet to the client
// 	_, err = clientConn.Write(authBuf)
// 	if err != nil {
// 		if ctx.Err() != nil {
// 			return ctx.Err()
// 		}
// 		utils.LogError(logger, err, "failed to write auth more data or auth switch request to the client")
// 		return err
// 	}

// 	//simulate the caching_sha2_password auth mechanism
// 	switch CachingSha2PasswordMechanism {
// 	case mysql.CachingSha2PasswordToString(mysql.PerformFullAuthentication):
// 		err := simulateFullAuth(ctx, logger, clientConn, initialHandshakeMock, mockDb, decodeCtx)
// 		if err != nil {
// 			utils.LogError(logger, err, "failed to simulate full auth")
// 			return err
// 		}
// 	case mysql.CachingSha2PasswordToString(mysql.FastAuthSuccess):
// 		err := simulateFastAuthSuccess(ctx, logger, clientConn, initialHandshakeMock, mockDb, decodeCtx)
// 		if err != nil {
// 			utils.LogError(logger, err, "failed to simulate fast auth success")
// 			return err
// 		}
// 	}

// 	return nil
// }

// func simulateFastAuthSuccess(ctx context.Context, logger *zap.Logger, clientConn net.Conn, initialHandshakeMock *models.Mock, mockDb integrations.MockMemDb, decodeCtx *operation.DecodeContext) error {
// 	resp := initialHandshakeMock.Spec.MySQLResponses

// 	if len(resp) < 3 {
// 		utils.LogError(logger, nil, "final response mock not found for fast auth success")
// 		return fmt.Errorf("final response mock not found for fast auth success")
// 	}

// 	logger.Debug("final response for fast auth success", zap.Any("response", resp[2].PacketBundle.Header.Type))

// 	// Send the final response (OK/Err) to the client
// 	buf, err := operation.EncodeToBinary(ctx, logger, &resp[2].PacketBundle)
// 	if err != nil {
// 		utils.LogError(logger, err, "failed to encode final response packet for fast auth success")
// 		return err
// 	}

// 	_, err = clientConn.Write(buf)
// 	if err != nil {
// 		if ctx.Err() != nil {
// 			return ctx.Err()
// 		}
// 		utils.LogError(logger, err, "failed to write final response for fast auth success to the client")
// 		return err
// 	}

// 	//update the config mock (since it can be reused in case of more connections compared to record mode)
// 	updateMock(ctx, logger, initialHandshakeMock, mockDb)

// 	return nil
// }

// func simulateFullAuth(ctx context.Context, logger *zap.Logger, clientConn net.Conn, initialHandshakeMock *models.Mock, mockDb integrations.MockMemDb, decodeCtx *operation.DecodeContext) error {

// 	resp := initialHandshakeMock.Spec.MySQLResponses
// 	req := initialHandshakeMock.Spec.MySQLRequests

// 	// read the public key request from the client
// 	publicKeyRequestBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
// 	if err != nil {
// 		utils.LogError(logger, err, "failed to read public key request from client")
// 		return err
// 	}

// 	// decode the public key request
// 	pkt, err := operation.DecodePayload(ctx, logger, publicKeyRequestBuf, clientConn, decodeCtx)
// 	if err != nil {
// 		utils.LogError(logger, err, "failed to decode public key request from client")
// 		return err
// 	}

// 	publicKey, ok := pkt.Message.(string)
// 	if !ok {
// 		utils.LogError(logger, nil, "failed to assert public key request packet")
// 		return nil
// 	}

// 	// Get the public key response from the mock
// 	if len(req) < 2 {
// 		utils.LogError(logger, nil, "no mysql mocks found for public key response")
// 		return fmt.Errorf("no mysql mocks found for public key response")
// 	}

// 	publicKeyMock, ok := req[1].Message.(string)
// 	if !ok {
// 		utils.LogError(logger, nil, "failed to assert public key response packet")
// 		return nil
// 	}

// 	// Match the public key response from the client with the mock
// 	if publicKey != publicKeyMock {
// 		utils.LogError(logger, nil, "public key mismatch")
// 		return fmt.Errorf("public key mismatch")
// 	}

// 	// Get the AuthMoreData for sending the public key
// 	if len(resp) < 3 {
// 		utils.LogError(logger, nil, "no mysql mocks found for auth more data (public key)")
// 		return fmt.Errorf("no mysql mocks found for auth more data (public key)")
// 	}

// 	// Get the AuthMoreData packet
// 	pkt, ok = resp[2].PacketBundle.Message.(*mysql.AuthMoreDataPacket)
// 	if !ok {
// 		utils.LogError(logger, nil, "failed to assert auth more data packet (public key)")
// 		return nil
// 	}

// 	// encode the public key response
// 	buf, err := operation.EncodeToBinary(ctx, logger, &resp[2].PacketBundle)
// 	if err != nil {
// 		utils.LogError(logger, err, "failed to encode public key response packet")
// 		return err
// 	}

// 	// Write the public key response to the client
// 	_, err = clientConn.Write(buf)
// 	if err != nil {
// 		if ctx.Err() != nil {
// 			return ctx.Err()
// 		}
// 		utils.LogError(logger, err, "failed to write public key response to the client")
// 		return err
// 	}

// 	// Read the encrypted password from the client

// 	encryptedPasswordBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
// 	if err != nil {
// 		utils.LogError(logger, err, "failed to read encrypted password from client")
// 		return err
// 	}

// 	// convert it to packet bundle
// 	pkt, err := mysqlUtils.BytesToMySQLPacket(encryptedPasswordBuf)
// 	if err != nil {
// 		utils.LogError(logger, err, "failed to convert encrypted password to packet")
// 		return err
// 	}

// 	encryptedPasswordPkt := pkt.(*my)

// 	// if publicKey

// 	// FullAuth mechanism only comes for the first time unless COM_CHANGE_USER is called (that is not supported for now).
// 	// Afterwards only fast auth success is expected. So, we can delete this.
// 	ok = mockDb.DeleteUnFilteredMock(*initialHandshakeMock)
// 	// need to check what to do in this case
// 	if !ok {
// 		utils.LogError(logger, nil, "failed to delete unfiltered mock")
// 	}

// 	return nil
// }

// func matchEncryptedPassword(actual, expected *mysql.Packet) error {

// 	// Match the payloadlength
// 	if actual.Header.PayloadLength != expected.Header.PayloadLength {
// 		return fmt.Errorf("payload length mismatch for encrypted password")
// 	}

// 	// Match the sequence number
// 	if actual.Header.SequenceID != expected.Header.SequenceID {
// 		return fmt.Errorf("sequence number mismatch for encrypted password")
// 	}

// 	// Match the payload
// 	// first convert the actual payload to base64 since the expected payload is in base64
// 	actualPayload := base64.StdEncoding.EncodeToString(actual.Payload)
// 	if actualPayload != string(expected.Payload) {
// 		return fmt.Errorf("payload mismatch for encrypted password")
// 	}
// 	return nil
// }
