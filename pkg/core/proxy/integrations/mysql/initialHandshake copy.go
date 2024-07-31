//go:build linux

package mysql

import (
	"context"
	"fmt"
	"io"
	"net"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/generic"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/operation"
	mysqlUtils "go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type handshakeRes struct {
	req               []mysql.Request
	resp              []mysql.Response
	requestOperation  string
	responseOperation string
}

func handleInitialHandshake(ctx context.Context, logger *zap.Logger, data []byte, clientConn, destConn net.Conn, decodeCtx *operation.DecodeContext) (handshakeRes, error) {
	var (
		err error
	)

	res := handshakeRes{
		req:  make([]mysql.Request, 0),
		resp: make([]mysql.Response, 0),
	}

	// Initial Handshake from server
	handshake := data
	_, err = clientConn.Write(handshake)
	if err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write handshake response to client")

		return res, err
	}

	// Decode server handshake packet
	handshakePkt, err := operation.DecodePayload(ctx, logger, handshake, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode handshake packet")
		return res, err
	}

	res.resp = append(res.resp, mysql.Response{
		PacketBundle: *handshakePkt,
	})

	// Handshake response from client
	handshakeResponse, err := pUtil.ReadBytes(ctx, logger, clientConn)
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
	handshakeResponsePkt, err := operation.DecodePayload(ctx, logger, handshakeResponse, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode handshake response packet")
		return res, err
	}

	res.req = append(res.req, mysql.Request{
		PacketBundle: *handshakeResponsePkt,
	})

	// Read the auth more data in order to get the auth type
	authMoreData, err := pUtil.ReadBytes(ctx, logger, destConn)
	if err != nil {
		if err == io.EOF {
			logger.Debug("received request buffer is empty in record mode for mysql call")

			return res, err
		}
		utils.LogError(logger, err, "failed to read packet from server after handshake")
		return res, err
	}

	// Tell the auth type to the client
	_, err = clientConn.Write(authMoreData)
	if err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write packet to client after handshake")
		return res, err
	}

	// Decode auth more data packet
	authMorePkt, err := operation.DecodePayload(ctx, logger, authMoreData, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode auth more data packet")
		return res, err
	}

	serverMsg, ok := handshakePkt.Message.(*mysql.HandshakeV10Packet)
	if !ok {
		return res, fmt.Errorf("failed to cast message to HandshakeV10Packet")
	}

	var req []mysql.Request
	var resp []mysql.Response

	switch serverMsg.AuthPluginName {
	case NativePassword:
		return res, fmt.Errorf("Native Password authentication is not supported")
	case CachingSha2Password:
		req, resp, err = handleCachingSha2Password(ctx, logger, authMorePkt, clientConn, destConn, decodeCtx)
		if err != nil {
			return res, err
		}
	case Sha256Password:
		return res, fmt.Errorf("Sha256 Password authentication is not supported")
	default:
		return res, fmt.Errorf("unsupported authentication plugin: %s", serverMsg.AuthPluginName)
	}

	res.req = append(res.req, req...)
	res.resp = append(res.resp, resp...)

	res.requestOperation = mysql.HandshakeResponse41
	res.responseOperation = fmt.Sprintf("%s Authentication", serverMsg.AuthPluginName)

	return res, nil
}

func handleCachingSha2Password(ctx context.Context, logger *zap.Logger, authMorePkt *mysql.PacketBundle, clientConn, destConn net.Conn, decodeCtx *operation.DecodeContext) ([]mysql.Request, []mysql.Response, error) {
	var (
		requests  []mysql.Request
		responses []mysql.Response
	)

	//get the auth type from auth more data packet
	authPktPayload, ok := authMorePkt.Message.(*mysql.AuthMoreDataPacket)
	if !ok {
		return requests, responses, fmt.Errorf("failed to cast message to AuthMoreDataPacket")
	}

	authData := []byte(authPktPayload.Data)

	switch mysql.CachingSha2Password(authData[0]) {
	case mysql.PerformFullAuthentication:
		req, res, err := handleFullAuth(ctx, logger, authMorePkt, clientConn, destConn, decodeCtx)
		if err != nil {
			return requests, responses, fmt.Errorf("failed to handle caching sha2 password full auth: %w", err)
		}

		requests = append(requests, req...)
		responses = append(responses, res...)

	case mysql.FastAuthSuccess:
		req, res, err := handleFastAuthSuccess(ctx, logger, authMorePkt, clientConn, destConn, decodeCtx)
		if err != nil {
			return requests, responses, fmt.Errorf("failed to handle caching sha2 password fast auth success: %w", err)
		}

		requests = append(requests, req...)
		responses = append(responses, res...)
	}
	return requests, responses, nil
}

func handleFastAuthSuccess(ctx context.Context, logger *zap.Logger, authMorePkt *mysql.PacketBundle, clientConn, destConn net.Conn, decodeCtx *operation.DecodeContext) ([]mysql.Request, []mysql.Response, error) {
	var (
		requests  []mysql.Request
		responses []mysql.Response
	)

	//As per wire shark capture, during fast auth success, server sends OK packet along with the auth data

	//get the auth type from auth more data packet
	authPktPayload, ok := authMorePkt.Message.(*mysql.AuthMoreDataPacket)
	if !ok {
		return requests, responses, fmt.Errorf("failed to cast message to AuthMoreDataPacket")
	}

	//debug log
	operation.PrintByteArray("Auth data in fast auth", []byte(authPktPayload.Data))

	authData := []byte(authPktPayload.Data)

	// update the auth more data and separate the ok packet
	// (rather than saving the actual status tag i.e (0x03) saving the string for readability
	authPktPayload.Data = mysql.CachingSha2PasswordToString(mysql.FastAuthSuccess)

	// add auth more data packet to the response
	responses = append(responses, mysql.Response{
		PacketBundle: *authMorePkt,
	})

	okAfterAuth := true
	if len(authData) == 1 {
		okAfterAuth = false
		logger.Debug("auth data length is 1, expected ok packet is not attached with auth data")
	}

	var okData []byte
	var err error
	if okAfterAuth {
		//debug log
		logger.Info("auth data length is more than 1, expected ok packet is attached with auth data")
		okData = authData[1:]
	} else {
		//debug log
		logger.Info("auth data length is 1, expected ok packet is not attached with auth data")

		// read the ok packet from the server as it is not attached with the auth data
		okData, err = pUtil.ReadBytes(ctx, logger, destConn)
		if err != nil {
			if err == io.EOF {
				logger.Debug("received request buffer is empty in record mode for mysql call")
				return requests, responses, err
			}
			utils.LogError(logger, err, "failed to read OK packet from server")
			return requests, responses, err
		}

		// write the ok packet to the client
		_, err = clientConn.Write(okData)
		if err != nil {
			if ctx.Err() != nil {
				return requests, responses, ctx.Err()
			}
			utils.LogError(logger, err, "failed to write ok packet to client after handshake")
			return requests, responses, err
		}

	}

	okPacket, err := mysqlUtils.BytesToMySQLPacket(okData)
	if err != nil {
		return requests, responses, fmt.Errorf("failed to parse MySQL packet: %w", err)
	}

	//get the server greeting to decode the ok packet
	sg, ok := decodeCtx.ServerGreetings.Load(clientConn)
	if !ok {
		return requests, responses, fmt.Errorf("Server Greetings not found")
	}

	okMsg, err := generic.DecodeOk(ctx, okPacket.Payload, sg.CapabilityFlags)
	if err != nil {
		return requests, responses, fmt.Errorf("failed to decode OK packet: %w", err)
	}

	//update the last operation from auth more data packet to ok packet
	decodeCtx.LastOp.Store(clientConn, mysql.OK)

	okPacketBundle := mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &okPacket.Header,
			Type:   mysql.StatusToString(mysql.OK),
		},
		Message: okMsg,
	}

	responses = append(responses, mysql.Response{
		PacketBundle: okPacketBundle,
	})

	return requests, responses, nil
}

func handleFullAuth(ctx context.Context, logger *zap.Logger, authMorePkt *mysql.PacketBundle, clientConn, destConn net.Conn, decodeCtx *operation.DecodeContext) ([]mysql.Request, []mysql.Response, error) {
	var (
		requests  []mysql.Request
		responses []mysql.Response
	)

	// rather than saving the actual status tag i.e (0x04) saving the string for readability
	authMorePkt.Message = mysql.CachingSha2PasswordToString(mysql.PerformFullAuthentication)

	// add auth more data packet to the response (authData is just 1 length and contains just the auth type in case of full auth)
	responses = append(responses, mysql.Response{
		PacketBundle: *authMorePkt,
	})

	// read the public key request from the client
	publicKeyRequest, err := pUtil.ReadBytes(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read public key request from client")
		return requests, responses, err
	}
	_, err = destConn.Write(publicKeyRequest)
	if err != nil {
		if ctx.Err() != nil {
			return requests, responses, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write public key request to server")
		return requests, responses, err
	}

	publicKeyReqPkt, err := operation.DecodePayload(ctx, logger, publicKeyRequest, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode public key request packet")
		return requests, responses, err
	}

	requests = append(requests, mysql.Request{
		PacketBundle: *publicKeyReqPkt,
	})

	// read the "public key" as response from the server
	pubKey, err := pUtil.ReadBytes(ctx, logger, destConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read public key from server")
		return requests, responses, err
	}
	_, err = clientConn.Write(pubKey)
	if err != nil {
		if ctx.Err() != nil {
			return requests, responses, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write public key response to client")
		return requests, responses, err
	}

	pubKeyPkt, err := operation.DecodePayload(ctx, logger, pubKey, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode public key packet")
		return requests, responses, err
	}

	pubKeyPkt.Meta = map[string]string{
		"auth operation": "public key response",
	}

	//debug log
	logger.Info("public key response meta", zap.Any("public key meta", pubKeyPkt.Meta))

	responses = append(responses, mysql.Response{
		PacketBundle: *pubKeyPkt,
	})

	// read the encrypted password from the client
	encryptPass, err := pUtil.ReadBytes(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read encrypted password from client")

		return requests, responses, err
	}
	_, err = destConn.Write(encryptPass)
	if err != nil {
		if ctx.Err() != nil {
			return requests, responses, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write encrypted password to server")
		return requests, responses, err
	}

	encPass, err := mysqlUtils.BytesToMySQLPacket(encryptPass)
	if err != nil {
		utils.LogError(logger, err, "failed to parse MySQL packet")
		return requests, responses, err
	}

	encryptPassPkt := &mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &encPass.Header,
			Type:   EncryptedPassword,
		},
		Message: encPass.Payload,
	}

	requests = append(requests, mysql.Request{
		PacketBundle: *encryptPassPkt,
	})

	// read the final response from the server (ok or error)
	finalServerResponse, err := pUtil.ReadBytes(ctx, logger, destConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read final response from server")
		return requests, responses, err
	}
	_, err = clientConn.Write(finalServerResponse)
	if err != nil {
		if ctx.Err() != nil {
			return requests, responses, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write final response to client")

		return requests, responses, err
	}

	finalResPkt, err := operation.DecodePayload(ctx, logger, finalServerResponse, clientConn, decodeCtx)

	if err != nil {
		utils.LogError(logger, err, "failed to decode final response packet during caching sha2 password full auth")
		return requests, responses, err
	}

	responses = append(responses, mysql.Response{
		PacketBundle: *finalResPkt,
	})

	return requests, responses, nil
}
