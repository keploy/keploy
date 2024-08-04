//go:build linux

package encoder

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/constant"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/operation"
	mysqlUtils "go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	intgUtils "go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type handshakeRes struct {
	req               []mysql.Request
	resp              []mysql.Response
	requestOperation  string
	responseOperation string
	reqTimestamp      time.Time
}

func handleInitialHandshake(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, decodeCtx *operation.DecodeContext) (handshakeRes, error) {

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
	handshakePkt, err := operation.DecodePayload(ctx, logger, handshake, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode handshake packet")
		return res, err
	}

	// Set the intial request operation
	res.requestOperation = handshakePkt.Header.Type

	// Get the initial Plugin Name
	pluginName, err := operation.GetPluginName(handshakePkt.Message)
	if err != nil {
		utils.LogError(logger, err, "failed to get initial plugin name")
		return res, err
	}

	// Set the initial plugin name
	decodeCtx.PluginName = pluginName

	res.resp = append(res.resp, mysql.Response{
		PacketBundle: *handshakePkt,
	})

	// Handshake response from client
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
	handshakeResponsePkt, err := operation.DecodePayload(ctx, logger, handshakeResponse, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode handshake response packet")
		return res, err
	}

	res.req = append(res.req, mysql.Request{
		PacketBundle: *handshakeResponsePkt,
	})

	// Read the next auth packet, It can be either auth more data or auth switch request
	authData, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
	if err != nil {
		if err == io.EOF {
			logger.Debug("received request buffer is empty in record mode for mysql call")

			return res, err
		}
		utils.LogError(logger, err, "failed to read auth packet from server during handshake")
		return res, err
	}

	// AuthSwitchPacket: If the server sends an AuthSwitchRequest, then there must be a diff auth type with its data
	// AuthMoreData: If the server sends an AuthMoreData, then it tells the auth mechanism type for the initial plugin name
	_, err = clientConn.Write(authData)
	if err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write auth packet to client during handshake")
		return res, err
	}

	// Decode auth packet
	authPkt, err := operation.DecodePayload(ctx, logger, authData, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode auth more data packet")
		return res, err
	}

	var authRes handshakeRes
	switch authPkt.Message.(type) {
	case *mysql.AuthSwitchRequestPacket:
		pkt := authPkt.Message.(*mysql.AuthSwitchRequestPacket)

		// Change the plugin name due to auth switch request
		decodeCtx.PluginName = pkt.PluginName

		authRes, err = handleAuth(ctx, logger, authPkt, clientConn, destConn, decodeCtx)
		if err != nil {
			return res, fmt.Errorf("failed to handle auth switch request: %w", err)
		}
	case *mysql.AuthMoreDataPacket:
		authRes, err = handleAuth(ctx, logger, authPkt, clientConn, destConn, decodeCtx)
		if err != nil {
			return res, fmt.Errorf("failed to handle auth more data: %w", err)
		}
	}

	setHandshakeResult(&res, authRes)

	return res, nil
}

func setHandshakeResult(res *handshakeRes, authRes handshakeRes) {
	res.req = append(res.req, authRes.req...)
	res.resp = append(res.resp, authRes.resp...)
	res.responseOperation = authRes.responseOperation
}

func handleAuth(ctx context.Context, logger *zap.Logger, authPkt *mysql.PacketBundle, clientConn, destConn net.Conn, decodeCtx *operation.DecodeContext) (handshakeRes, error) {
	res := handshakeRes{
		req:  make([]mysql.Request, 0),
		resp: make([]mysql.Response, 0),
	}

	switch decodeCtx.PluginName {
	case constant.NativePassword:
		return res, fmt.Errorf("Native Password authentication is not supported")
	case constant.CachingSha2Password:
		result, err := handleCachingSha2Password(ctx, logger, authPkt, clientConn, destConn, decodeCtx)
		if err != nil {
			return res, fmt.Errorf("failed to handle caching sha2 password: %w", err)
		}
		setHandshakeResult(&res, result)
	case constant.Sha256Password:
		return res, fmt.Errorf("Sha256 Password authentication is not supported")
	default:
		return res, fmt.Errorf("unsupported authentication plugin: %s", decodeCtx.PluginName)
	}

	return res, nil
}

func handleCachingSha2Password(ctx context.Context, logger *zap.Logger, authPkt *mysql.PacketBundle, clientConn, destConn net.Conn, decodeCtx *operation.DecodeContext) (handshakeRes, error) {
	res := handshakeRes{
		req:  make([]mysql.Request, 0),
		resp: make([]mysql.Response, 0),
	}

	var authMechanism string
	var err error
	switch authPkt.Message.(type) {
	case *mysql.AuthSwitchRequestPacket:
		pkt := authPkt.Message.(*mysql.AuthSwitchRequestPacket)
		//Change the plugin data to a string for better readability as it is expected to be either "full auth" or "fast auth success"
		authMechanism, err = operation.GetCachingSha2PasswordMechanism(pkt.PluginData[0])
		if err != nil {
			return res, fmt.Errorf("failed to get caching sha2 password mechanism: %w", err)
		}
		pkt.PluginData = authMechanism

	case *mysql.AuthMoreDataPacket:
		authMorePkt := authPkt.Message.(*mysql.AuthMoreDataPacket)
		// Getting the string value of the caching_sha2_password mechanism
		authMechanism, err = operation.GetCachingSha2PasswordMechanism(authMorePkt.Data[0])
		if err != nil {
			return res, fmt.Errorf("failed to get caching sha2 password mechanism: %w", err)
		}
		authMorePkt.Data = authMechanism
	}

	// save the auth more data or auth switch request packet
	res.resp = append(res.resp, mysql.Response{
		PacketBundle: *authPkt,
	})

	auth, err := operation.StringToCachingSha2PasswordMechanism(authMechanism)
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

func handleFastAuthSuccess(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, decodeCtx *operation.DecodeContext) (handshakeRes, error) {
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

	finalPkt, err := operation.DecodePayload(ctx, logger, finalResp, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode final response packet after auth data packet")
		return res, err
	}

	res.resp = append(res.resp, mysql.Response{
		PacketBundle: *finalPkt,
	})

	// Set the final response operation of the handshake
	res.responseOperation = finalPkt.Header.Type

	return res, nil
}

func handleFullAuth(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, decodeCtx *operation.DecodeContext) (handshakeRes, error) {
	res := handshakeRes{
		req:  make([]mysql.Request, 0),
		resp: make([]mysql.Response, 0),
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

	publicKeyReqPkt, err := operation.DecodePayload(ctx, logger, publicKeyRequest, clientConn, decodeCtx)
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

	pubKeyPkt, err := operation.DecodePayload(ctx, logger, pubKey, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode public key packet")
		return res, err
	}

	pubKeyPkt.Meta = map[string]string{
		"auth operation": "public key response",
	}

	//debug log
	logger.Info("public key response meta", zap.Any("public key meta", pubKeyPkt.Meta))

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
			Type:   constant.EncryptedPassword,
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

	finalResPkt, err := operation.DecodePayload(ctx, logger, finalServerResponse, clientConn, decodeCtx)

	if err != nil {
		utils.LogError(logger, err, "failed to decode final response packet during caching sha2 password full auth")
		return res, err
	}

	res.resp = append(res.resp, mysql.Response{
		PacketBundle: *finalResPkt,
	})

	// Set the final response operation of the handshake
	res.responseOperation = finalResPkt.Header.Type

	return res, nil
}
