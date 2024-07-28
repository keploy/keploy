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

func handleInitialHandshake(ctx context.Context, logger *zap.Logger, data []byte, clientConn, destConn net.Conn, decodeCtx *operation.DecodeContext) ([]mysql.Request, []mysql.Response, error) {
	var (
		requests  []mysql.Request
		responses []mysql.Response
	)

	var err error

	// intial Handshake from server
	handshake := data
	_, err = clientConn.Write(handshake)
	if err != nil {
		if ctx.Err() != nil {
			return requests, responses, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write handshake response to client")

		return requests, responses, err
	}

	// decode server handshake packet
	handshakePkt, err := operation.DecodePayload(ctx, logger, handshake, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode handshake packet")
		return requests, responses, err
	}

	responses = append(responses, mysql.Response{
		PacketBundle: *handshakePkt,
	})

	// handshake response from client
	handshakeResponse, err := pUtil.ReadBytes(ctx, logger, clientConn)
	if err != nil {
		if err == io.EOF {
			logger.Debug("recieved request buffer is empty in record mode for mysql call")

			return requests, responses, err
		}
		utils.LogError(logger, err, "failed to read handshake response from client")

		return requests, responses, err
	}

	_, err = destConn.Write(handshakeResponse)
	if err != nil {
		if ctx.Err() != nil {
			return requests, responses, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write handshake response to server")

		return requests, responses, err
	}

	// decode client handshake response packet
	handshakeResponsePkt, err := operation.DecodePayload(ctx, logger, handshakeResponse, destConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode handshake response packet")
		return requests, responses, err
	}

	requests = append(requests, mysql.Request{
		PacketBundle: *handshakeResponsePkt,
	})

	// read the auth more data in order to get the auth type
	authMoreData, err := pUtil.ReadBytes(ctx, logger, destConn)
	if err != nil {
		if err == io.EOF {
			logger.Debug("recieved request buffer is empty in record mode for mysql call")

			return requests, responses, err
		}
		utils.LogError(logger, err, "failed to read packet from server after handshake")
		return requests, responses, err
	}

	// tell the auth type to the client
	_, err = clientConn.Write(authMoreData)
	if err != nil {
		if ctx.Err() != nil {
			return requests, responses, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write packet to client after handshake")
		return requests, responses, err
	}

	// decode auth more data packet
	authMorePkt, err := operation.DecodePayload(ctx, logger, authMoreData, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode auth more data packet")
		return requests, responses, err
	}

	serverMsg, ok := handshakePkt.Message.(*mysql.HandshakeV10Packet)
	if !ok {
		return requests, responses, fmt.Errorf("failed to cast message to HandshakeV10Packet")
	}

	if serverMsg.AuthPluginName == CachingSha2Password {
		req, res, err := handleCachingSha2Password(ctx, logger, authMorePkt, clientConn, destConn, decodeCtx)
		if err != nil {
			return requests, responses, err
		}
		requests = append(requests, req...)
		responses = append(responses, res...)
	}

	return requests, responses, nil
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
		req, res, err := handleFastAuthSuccess(ctx, logger, authMorePkt, clientConn, decodeCtx)
		if err != nil {
			return requests, responses, fmt.Errorf("failed to handle caching sha2 password fast auth success: %w", err)
		}

		requests = append(requests, req...)
		responses = append(responses, res...)
	}
	return requests, responses, nil
}

func handleFastAuthSuccess(ctx context.Context, logger *zap.Logger, authMorePkt *mysql.PacketBundle, clientConn net.Conn, decodeCtx *operation.DecodeContext) ([]mysql.Request, []mysql.Response, error) {
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

	authData := []byte(authPktPayload.Data)
	if len(authData) == 1 {
		return requests, responses, fmt.Errorf("invalid auth data length, expected more data for ok packet")
	}

	// update the auth more data and separate the ok packet
	authPktPayload.Data = string(authData[0])

	// add auth more data packet to the response
	responses = append(responses, mysql.Response{
		PacketBundle: *authMorePkt,
	})

	okData := authData[1:]
	okPacket, err := mysqlUtils.BytesToMySQLPacket(okData)
	if err != nil {
		return requests, responses, fmt.Errorf("failed to parse MySQL packet: %w", err)
	}

	//get the server greeting to decode the ok packet
	sg, ok := decodeCtx.ServerGreetings.Load(decodeCtx.ClientConn)
	if !ok {
		return requests, responses, fmt.Errorf("Server Greetings not found")
	}

	okMsg, err := generic.DecodeOk(ctx, okPacket.Payload, sg.CapabilityFlags)
	if err != nil {
		return requests, responses, fmt.Errorf("failed to decode OK packet: %w", err)
	}

	//update the last operation from auth more data packet to ok packet
	decodeCtx.LastOp.Store(decodeCtx.ClientConn, mysql.OK)

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

	pubKeyPkt, err := operation.DecodePayload(ctx, logger, pubKey, destConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode public key packet")
		return requests, responses, err
	}
	pubKeyPkt.Meta["auth operation"] = "public key response"

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

	finalResPkt, err := operation.DecodePayload(ctx, logger, finalServerResponse, destConn, decodeCtx)

	if err != nil {
		utils.LogError(logger, err, "failed to decode final response packet during caching sha2 password full auth")
		return requests, responses, err
	}

	responses = append(responses, mysql.Response{
		PacketBundle: *finalResPkt,
	})

	return requests, responses, nil
}
