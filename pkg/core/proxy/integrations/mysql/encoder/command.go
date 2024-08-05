// go:build linux

// Package encoder provides the encoding functions for the MySQL integration.
// Binary to Mock Yaml
package encoder

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/operation"
	mysqlUtils "go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func handleClientQueries(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, mocks chan<- *models.Mock, decodeCtx *operation.DecodeContext) error {
	var (
		requests  []mysql.Request
		responses []mysql.Response
	)

	//for keeping conn alive
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// read the command from the client
			command, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read command packet from client")
				}
				return err
			}

			// write the command to the destination server
			_, err = destConn.Write(command)
			if err != nil {
				utils.LogError(logger, err, "failed to write command to the server")
				return err
			}

			// Getting timestamp for the request
			reqTimestamp := time.Now()

			commandPkt, err := operation.DecodePayload(ctx, logger, command, clientConn, decodeCtx)
			if err != nil {
				utils.LogError(logger, err, "failed to decode the MySQL packet from the client")
				return err
			}

			requests = append(requests, mysql.Request{
				PacketBundle: *commandPkt,
			})

			// handle no response commands like COM_STMT_CLOSE, COM_STMT_SEND_LONG_DATA, etc
			if operation.IsNoResponseCommand(commandPkt.Header.Type) {
				recordMock(ctx, requests, responses, "mocks", commandPkt.Header.Type, "NO Response Packet", mocks, reqTimestamp)
				// reset the requests and responses
				requests = []mysql.Request{}
				responses = []mysql.Response{}
				println("No response command", commandPkt.Header.Type)
				continue
			}

			commandRespPkt, err := handleQueryResponse(ctx, logger, clientConn, destConn, decodeCtx)
			if err != nil {
				utils.LogError(logger, err, "failed to handle the query response")
				return err
			}

			responses = append(responses, mysql.Response{
				PacketBundle: *commandRespPkt,
			})

			// record the mock
			recordMock(ctx, requests, responses, "mocks", commandPkt.Header.Type, commandRespPkt.Header.Type, mocks, reqTimestamp)

			// reset the requests and responses
			requests = []mysql.Request{}
			responses = []mysql.Response{}
		}
	}
}

func handleQueryResponse(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, decodeCtx *operation.DecodeContext) (*mysql.PacketBundle, error) {
	// read the command response from the destination server
	commandResp, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
	if err != nil {
		if err != io.EOF {
			utils.LogError(logger, err, "failed to read command response from the server")
		}
		return nil, err
	}

	// write the command response to the client
	_, err = clientConn.Write(commandResp)
	if err != nil {
		utils.LogError(logger, err, "failed to write command response to the client")
		return nil, err
	}

	//decode the command response packet
	commandRespPkt, err := operation.DecodePayload(ctx, logger, commandResp, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode the command response packet")
		return nil, err
	}

	// check if the command response is an error or ok packet
	if commandRespPkt.Header.Type == mysql.StatusToString(mysql.ERR) || commandRespPkt.Header.Type == mysql.StatusToString(mysql.OK) {
		logger.Debug("command response packet", zap.Any("packet", commandRespPkt.Header.Type))
		return commandRespPkt, nil
	}

	// Get the last operation in order to handle current packet if it is not an error or ok packet
	lastOp, ok := decodeCtx.LastOp.Load(clientConn)
	if !ok {
		return nil, fmt.Errorf("failed to get the last operation from the context while handling the query response")
	}

	var queryResponsePkt *mysql.PacketBundle

	switch lastOp {
	case mysql.COM_QUERY:
		//debug log
		logger.Info("Handling text result set", zap.Any("lastOp", lastOp))
		// handle the query response (TextResultSet)
		queryResponsePkt, err = handleTextResultSet(ctx, logger, clientConn, destConn, commandRespPkt, decodeCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to handle the query response packet: %w", err)
		}

	case mysql.COM_STMT_PREPARE:
		//debug
		logger.Info("Handling prepare Statement Response OK", zap.Any("lastOp", lastOp))
		// handle the prepared statement response (COM_STMT_PREPARE_OK)
		queryResponsePkt, err = handlePreparedStmtResponse(ctx, logger, clientConn, destConn, commandRespPkt, decodeCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to handle the prepared statement response: %w", err)
		}
	case mysql.COM_STMT_EXECUTE:
		//debug log
		logger.Info("Handling binary protocol result set", zap.Any("lastOp", lastOp))
		// handle the statment execute response (BinaryProtocolResultSet)
		queryResponsePkt, err = handleBinaryResultSet(ctx, logger, clientConn, destConn, commandRespPkt, decodeCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to handle the statement execute response: %w", err)
		}

	default:
		return nil, fmt.Errorf("unsupported operation: %x", lastOp)
	}

	return queryResponsePkt, nil
}
