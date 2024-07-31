//go:build linux

package mysql

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/operation"
	mysqlUtils "go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func encode(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, mocks chan<- *models.Mock, _ models.OutgoingOptions) error {

	var (
		requests  []mysql.Request
		responses []mysql.Response
	)

	// Helper struct for decoding packets
	decodeCtx := &operation.DecodeContext{
		Mode: models.MODE_RECORD,
		// Map for storing last operation per connection
		LastOp: operation.NewLastOpMap(),
		// Map for storing server greetings (inc capabilities, auth plugin, etc) per initial handshake (per connection)
		ServerGreetings: operation.NewGreetings(),
		// Map for storing prepared statements per connection
		PreparedStatements: make(map[uint32]*mysql.StmtPrepareOkPacket),
	}

	// Read the server greetings
	handshake, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read the handshake from the server")
		return err
	}

	// Write the server handshake response to the client
	_, err = clientConn.Write(handshake)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		utils.LogError(logger, err, "failed to write handshake response to client")

		return err
	}

	reqTimestamp := time.Now()

	// Decode server greetings / handshake packet
	handshakePkt, err := operation.DecodePayload(ctx, logger, handshake, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode handshake packet")
		return err
	}

	responses = append(responses, mysql.Response{
		PacketBundle: *handshakePkt,
	})

	// Get the plugin type from the handshake packet
	pluginName, err := operation.GetPluginName(handshakePkt.Message)
	if err != nil {
		utils.LogError(logger, err, "failed to get the plugin type from the handshake packet")
		return err
	}

	// Set the initial plugin name sent by the server
	decodeCtx.PluginName = pluginName

	// Read the client handshake response
	handshakeResponse, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read the handshake response from the client")
		return err
	}

	// Write the client handshake response to the server
	_, err = destConn.Write(handshakeResponse)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		utils.LogError(logger, err, "failed to write handshake response to server")

		return err
	}

	// Decode client handshake response packet
	handshakeResponsePkt, err := operation.DecodePayload(ctx, logger, handshakeResponse, destConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode handshake response packet")
		return err
	}

	requests = append(requests, mysql.Request{
		PacketBundle: *handshakeResponsePkt,
	})

	// For streaming support, read from both the connections together and write to the respective connections
	clientBuffChan := make(chan []byte)
	destBuffChan := make(chan []byte)
	errChan := make(chan error, 1)

	// get the error group from the context
	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}

	// read requests from client
	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, destConn)
		defer close(clientBuffChan)
		mysqlUtils.ReadPacketStream(ctx, logger, clientConn, clientBuffChan, errChan)
		return nil
	})

	// read responses from destination
	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, destConn)
		defer close(destBuffChan)
		mysqlUtils.ReadPacketStream(ctx, logger, destConn, destBuffChan, errChan)
		return nil
	})

	// used to keep track of the handshake completion
	var handshakeCompleted bool
	// used to keep track of the query response status (preparedStatementOk, textResultSet, BinaryProtocolResultSet,etc)
	queryResponseStatus := &packetCompletionStatus{
		preparedStatementOK:     &queryResponseState{},
		textResultSet:           &queryResponseState{},
		binaryProtocolResultSet: &queryResponseState{},
	}

	var queryResponseStream [][]byte

	// NOTE: we are first decoding the packets then only writing to the respective connections
	// in order to keep the order and previous state of the packet information in helper structs like decodeCtx etc.
	// This is done for streaming support.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errChan:
			if err == io.EOF {
				return nil
			}
			return err

		case clientBuff := <-clientBuffChan:

			if !handshakeCompleted {

				req, err := handleClientHandshake(ctx, logger, clientBuff, clientConn, decodeCtx)
				if err != nil {
					utils.LogError(logger, err, "failed to handle initial handshake")
					return err
				}

				requests = append(requests, req...)

				// write the packet to the server
				_, err = destConn.Write(clientBuff)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					utils.LogError(logger, err, "failed to write packet to server during handshake")
					return err
				}

				continue
			}

			// Handle the client-server interaction (command phase)

			// handle the client queries
			req, err := handleQueries(ctx, logger, clientBuff, clientConn, decodeCtx)
			if err != nil {
				utils.LogError(logger, err, "failed to handle client queries")
				return err
			}

			requests = append(requests, req...)

			// write the command to the destination server
			_, err = destConn.Write(clientBuff)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				utils.LogError(logger, err, "failed to write command to mysql server")
				return err
			}

			// Update the request timestamp for the new command
			reqTimestamp = time.Now()

		case destBuff := <-destBuffChan:

			if !handshakeCompleted {

				handshakeResult, err := handleServerHandshake(ctx, logger, destBuff, clientConn, decodeCtx)
				if err != nil {
					utils.LogError(logger, err, "failed to handle initial handshake")
					return err
				}

				responses = append(responses, handshakeResult.resp...)

				// write the packet to the client
				_, err = clientConn.Write(destBuff)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					utils.LogError(logger, err, "failed to write packet to client during handshake")
					return err
				}

				// Record the mock only when the handshake is completed
				if handshakeResult.saveMock {
					recordMock(ctx, requests, responses, "config", mysql.HandshakeResponse41, handshakeResult.responseOperation, mocks, reqTimestamp)
					// reset the requests and responses
					requests = []mysql.Request{}
					responses = []mysql.Response{}
					handshakeCompleted = true
				}

				continue
			}

			// handle the client-server interaction (command phase)

			//handle the client query response from the server
			result, err := handleQueriesResponse(ctx, logger, clientConn, queryResponseStatus, decodeCtx)
			if err != nil {
				utils.LogError(logger, err, "failed to handle client queries response")
				return err
			}

			queryResponseStream = append(queryResponseStream, destBuff)

			if result.saveMock {
				err := mysqlUtils.WriteStream(ctx, logger, clientConn, queryResponseStream)
				if err != nil {
					utils.LogError(logger, err, "failed to write the query response stream to the client")
					return err
				}

				responses = append(responses, result.resp...)
				recordMock(ctx, requests, responses, "mocks", result.requestOperation, result.responseOperation, mocks, reqTimestamp)
				// reset the requests and responses
				requests = []mysql.Request{}
				responses = []mysql.Response{}
			}

		}
	}
}

/*
//for keeping conn alive
	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, destConn)
		defer close(errCh)

		for {

			decodeCtx.LastOp.Store(clientConn, operation.RESET) //resetting last command for new loop

			data, source, err := mysqlUtils.ReadFirstBuffer(ctx, logger, clientConn, destConn)
			if len(data) == 0 {
				break
			}
			if err != nil {
				utils.LogError(logger, err, "failed to read initial data")
				errCh <- err
				return nil
			}

			// Getting timestamp for the request
			reqTimestamp := time.Now()

			switch source {
			case "destination":
				// handle the initial client-server handshake (connection phase)
				result, err := handleInitialHandshake(ctx, logger, data, clientConn, destConn, decodeCtx)
				if err != nil {
					utils.LogError(logger, err, "failed to handle initial handshake")
					errCh <- err
					return nil
				}
				requests = append(requests, result.req...)
				responses = append(responses, result.resp...)

				lstOp, _ := decodeCtx.LastOp.Load(clientConn)
				//debug log
				logger.Info("last operation after initial handshake", zap.Any("last operation", lstOp))

				// record the mock
				recordMock(ctx, requests, responses, "config", result.requestOperation, result.responseOperation, mocks, reqTimestamp)

				// reset the requests and responses
				requests = []mysql.Request{}
				responses = []mysql.Response{}

				// handle the client-server interaction (command phase)
				err = handleClientQueries(ctx, logger, clientConn, destConn, mocks, reqTimestamp, decodeCtx)
				if err != nil {
					if err == io.EOF {
						logger.Debug("recieved request buffer is empty in record mode for mysql call")
						errCh <- err
						return nil
					}
					utils.LogError(logger, err, "failed to handle client queries")
					errCh <- err
					return nil
				}
			case "client":
				err := handleClientQueries(ctx, logger, clientConn, destConn, mocks, reqTimestamp, decodeCtx)
				if err != nil {
					utils.LogError(logger, err, "failed to handle client queries")
					errCh <- err
					return nil
				}
			}
		}
		return nil
	})

*/
