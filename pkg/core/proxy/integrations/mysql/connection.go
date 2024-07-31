//go:build linux

package mysql

import (
	"context"
	"fmt"
	"net"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/operation"
	mysqlUtils "go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

// This file contains logic for handling the client-server handshake of connection phase

type handshakeResult struct {
	resp              []mysql.Response
	saveMock          bool
	responseOperation string
}

func handleServerHandshake(ctx context.Context, logger *zap.Logger, data []byte, clientConn net.Conn, decodeCtx *operation.DecodeContext) (handshakeResult, error) {

	// For the first iteration
	/*
	 #) This packet can either be authSwitch request (if both client and server auth methods are matched and both support CLIENT_PLUGIN_AUTH capability)
	 Or It can just be auth more data packet to tell the auth type to the client or exchange the auth data.
	*/

	// For the second iteration
	/*
		#) If accepted auth mechanism is "caching_sha2_password",
		1. Full Auth: The server sends the public key data to the client in authMoreData packet.
		2. Fast Auth Success: The server just sends the OK/Err packet to the client.

		#) If the auth mechanism is not accepted by both client and server, client sends the auth switch response packet after receiving the auth switch request packet.
	*/

	// For the Third iteration
	/*
	   1. Full Auth: The server just sends the OK/Err packet to the client.
	*/

	res := handshakeResult{
		resp:              make([]mysql.Response, 0),
		saveMock:          false,
		responseOperation: "",
	}

	pkt, err := operation.DecodePayload(ctx, logger, data, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode handshake related packet")
		return res, err
	}

	res.resp = append(res.resp, mysql.Response{
		PacketBundle: *pkt,
	})

	//Get the last operation
	lastOp, ok := decodeCtx.LastOp.Load(clientConn)
	if !ok {
		utils.LogError(logger, err, "failed to get the last operation")
		return res, fmt.Errorf("failed to handle the initial handshake")
	}

	switch pkt.Header.Type {
	case mysql.AuthStatusToString(mysql.AuthMoreData):

		// Get the AuthMoreData packet
		authMoreDataPkt, ok := pkt.Message.(*mysql.AuthMoreDataPacket)
		if !ok {
			utils.LogError(logger, err, "failed to cast the packet to AuthMoreDataPacket")
			return res, fmt.Errorf("failed to handle the initial handshake")
		}

		switch decodeCtx.PluginName {
		case NativePassword:
			return res, fmt.Errorf("Native Password authentication is not supported")
		case CachingSha2Password:

			switch lastOp {
			case byte(mysql.RequestPublicKey):
				logger.Debug("Received the public key from the server")
				pkt.Meta = map[string]string{
					"auth operation": "public key response",
				}
			default: // It must be related to caching_sha2_password auth mechanism
				logger.Debug("Received the auth more data from the server")

				// If the authMoreData packet has only one byte, then it tells the type of caching_sha2_password auth mechanism
				if len(authMoreDataPkt.Data) == 1 {
					// (rather than saving the actual status tag i.e (0x03/0x04) saving the string for readability
					switch authMoreDataPkt.Data[0] {
					case byte(mysql.PerformFullAuthentication):
						authMoreDataPkt.Data = mysql.CachingSha2PasswordToString(mysql.PerformFullAuthentication)
					case byte(mysql.FastAuthSuccess):
						authMoreDataPkt.Data = mysql.CachingSha2PasswordToString(mysql.FastAuthSuccess)
					default:
						return res, fmt.Errorf("unknown caching_sha2_password auth mechanism")
					}
				}
			}
		case Sha256Password:
			return res, fmt.Errorf("Sha256 Password authentication is not supported")
		default:
			return res, fmt.Errorf("unsupported authentication plugin: %s", decodeCtx.PluginName)
		}

	case mysql.AuthStatusToString(mysql.AuthSwitchRequest):

		pluginName, err := operation.GetPluginName(pkt.Message)
		if err != nil {
			utils.LogError(logger, err, "failed to get the plugin name from auth switch request packet")
		}
		// Change the plugin name because of the auth switch request packet
		decodeCtx.PluginName = pluginName

	case mysql.StatusToString(mysql.OK), mysql.StatusToString(mysql.ERR):
		res.responseOperation = pkt.Header.Type
		//save the mock once final response is received (either OK or ERR)
		res.saveMock = true
	default:
		return res, fmt.Errorf("unsupported server packet type during handshake: %s", pkt.Header.Type)
	}

	return res, nil
}

func handleClientHandshake(ctx context.Context, logger *zap.Logger, data []byte, clientConn net.Conn, decodeCtx *operation.DecodeContext) ([]mysql.Request, error) {
	requests := make([]mysql.Request, 0)
	var err error

	if decodeCtx.PluginName != CachingSha2Password {
		return requests, fmt.Errorf("unsupported authentication plugin, can't handle the client handshake: %s", decodeCtx.PluginName)
	}

	pkt, err := mysqlUtils.BytesToMySQLPacket(data)
	if err != nil {
		utils.LogError(logger, err, "failed to get the mysql packet")
		return requests, err
	}

	var parsedPkt *mysql.PacketBundle

	// Check if the packet is Public Key Request
	if len(pkt.Payload) == 1 && pkt.Payload[0] == byte(mysql.RequestPublicKey) {
		parsedPkt, err = operation.DecodePayload(ctx, logger, data, clientConn, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to decode handshake related packet")
			return requests, err
		}
	} else {
		// If the packet is not Public Key Request, then it must be Encrypted Password during handshake of caching_sha2_password
		encryptPassPkt := &mysql.PacketBundle{
			Header: &mysql.PacketInfo{
				Header: &pkt.Header,
				Type:   EncryptedPassword,
			},
			Message: pkt.Payload,
		}
		parsedPkt = encryptPassPkt
		// there is no status for Encrypted Password, so setting the last operation to RESET
		decodeCtx.LastOp.Store(clientConn, operation.RESET)
	}

	requests = append(requests, mysql.Request{
		PacketBundle: *parsedPkt,
	})

	return requests, nil
}
