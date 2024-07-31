// go:build linux

package mysql

import (
	"context"
	"fmt"
	"net"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/operation"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

/*
func handleClientQueries(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, mocks chan<- *models.Mock, reqTimestamp time.Time, decodeCtx *operation.DecodeContext) error {
	var (
		requests  []mysql.Request
		responses []mysql.Response
	)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// read the command from the client
			command, err := pUtil.ReadBytes(ctx, logger, clientConn)
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read command from the mysql client")
				}
				return err
			}

			// write the command to the destination server
			_, err = destConn.Write(command)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				utils.LogError(logger, err, "failed to write command to mysql server")
				return err
			}

			if len(command) == 0 {
				break
			}

			commandPkt, err := operation.DecodePayload(ctx, logger, command, clientConn, decodeCtx)
			if err != nil {
				utils.LogError(logger, err, "failed to decode the MySQL packet from the client")
				return err
			}

			//TODO: why directly requests is not used as the first argument
			requests = append([]mysql.Request{}, mysql.Request{
				PacketBundle: *commandPkt,
			})

			// read the command response from the destination server
			commandResp, err := pUtil.ReadBytes(ctx, logger, destConn)
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read command response from mysql server")
				}
				return err
			}
			_, err = clientConn.Write(commandResp)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				utils.LogError(logger, err, "failed to write command response to mysql client")
				return err
			}

			if len(commandResp) == 0 {
				break
			}

			commandRespPkt, err := operation.DecodePayload(ctx, logger, commandResp, clientConn, decodeCtx)
			if err != nil {
				utils.LogError(logger, err, "failed to decode the MySQL packet from the server")
				return err
			}

			responses = append([]mysql.Response{}, mysql.Response{
				PacketBundle: *commandRespPkt,
			})

			recordMock(ctx, requests, responses, "mocks", commandPkt.Header.Type, commandRespPkt.Header.Type, mocks, reqTimestamp)
		}
	}
}
*/

// packetCompletionStatus is used to check whether the query response packet is completed or not.
type packetCompletionStatus struct {
	preparedStatementOK     *queryResponseState // used to check whether preparedStatementOk packet is completed or not.
	textResultSet           *queryResponseState // used to check whether textResultSet packet is completed or not.
	binaryProtocolResultSet *queryResponseState // used to check whether BinaryProtocolResultSet packet is completed or not.
}

type queryResponseState struct {
	isCompleted bool
	data        []byte
}

type preparedStmtOkTracker struct {
	numParams           uint16
	numColumns          uint16
	eofParamsCompleted  bool
	eofColumnsCompleted bool
}

// handleQueries is used to decode the client queries.

func handleQueries(ctx context.Context, logger *zap.Logger, data []byte, clientConn net.Conn, decodeCtx *operation.DecodeContext) ([]mysql.Request, error) {

	var requests []mysql.Request

	// decode the command
	commandPkt, err := operation.DecodePayload(ctx, logger, data, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode the command packet from the client")
		return requests, err
	}

	requests = append(requests, mysql.Request{
		PacketBundle: *commandPkt,
	})

	return requests, nil
}

type QueryResponseResult struct {
	resp              []mysql.Response
	saveMock          bool
	requestOperation  string
	responseOperation string
}


// handleQueriesResponse is used to handle the response of the client query sent by the server.
func handleQueriesResponse(ctx context.Context, logger *zap.Logger, clientConn net.Conn, status *packetCompletionStatus, decodeCtx *operation.DecodeContext) (*QueryResponseResult, error) {
	result := &QueryResponseResult{
		resp:     make([]mysql.Response, 0),
		saveMock: false,
	}

	//Get the last operation
	lastOp, ok := decodeCtx.LastOp.Load(clientConn)
	if !ok {
		utils.LogError(logger, nil, "failed to get the last operation")
		return result, fmt.Errorf("failed to handle the query response")
	}

	switch lastOp {
	case mysql.COM_STMT_PREPARE:
		handlePrepareStatementOk(ctx, logger, clientConn, status, decodeCtx)
	case mysql.COM_QUERY:

	case mysql.COM_STMT_EXECUTE:

	default:

	}

	return result, nil
}
