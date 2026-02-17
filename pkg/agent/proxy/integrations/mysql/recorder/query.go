package recorder

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	mysqlUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// handleClientQueries processes the MySQL command phase using pre-fetching
// pipelines. The pipelines drain the ring buffers asynchronously so packet
// reads never block on parser processing.
func handleClientQueries(ctx context.Context, logger *zap.Logger, clientPipe, destPipe *packetPipeline, mocks chan<- *models.Mock, decodeCtx *wire.DecodeContext, opts models.OutgoingOptions) error {
	var (
		requests  []mysql.Request
		responses []mysql.Response
	)
	// clientConn is the underlying TeeForwardConn, used only as decodeCtx map key
	clientConn := clientPipe.conn

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Read pre-fetched command from client pipeline (instant if buffered)
			command, err := clientPipe.ReadPacket()
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read command packet from client")
				}
				return err
			}

			reqTimestamp := time.Now()

			commandPkt, err := wire.DecodePayload(ctx, logger, command, clientConn, decodeCtx)
			if err != nil {
				utils.LogError(logger, err, "failed to decode the MySQL packet from the client")
				return err
			}

			requests = append(requests, mysql.Request{
				PacketBundle: *commandPkt,
			})

			if wire.IsNoResponseCommand(commandPkt.Header.Type) {
				recordMock(ctx, requests, responses, "mocks", commandPkt.Header.Type, "NO Response Packet", mocks, reqTimestamp, time.Now(), opts)
				requests = []mysql.Request{}
				responses = []mysql.Response{}
				logger.Debug("No response command", zap.Any("packet", commandPkt.Header.Type))
				continue
			}

			commandRespPkt, resTimestamp, err := handleQueryResponse(ctx, logger, clientConn, destPipe, decodeCtx)
			if err != nil {
				if err == io.EOF && commandPkt.Header.Type == mysql.CommandStatusToString(mysql.COM_QUIT) {
					logger.Debug("server closed the connection without any response")
					return err
				}
				utils.LogError(logger, err, "failed to handle the query response")
				return err
			}

			responses = append(responses, mysql.Response{
				PacketBundle: *commandRespPkt,
			})

			recordMock(ctx, requests, responses, "mocks", commandPkt.Header.Type, commandRespPkt.Header.Type, mocks, reqTimestamp, resTimestamp, opts)
			requests = []mysql.Request{}
			responses = []mysql.Response{}
		}
	}
}

// handleQueryResponse reads response packets from the dest pipeline.
// clientConn is the TeeForwardConn used as decodeCtx map key only.
func handleQueryResponse(ctx context.Context, logger *zap.Logger, clientConn net.Conn, destPipe *packetPipeline, decodeCtx *wire.DecodeContext) (*mysql.PacketBundle, time.Time, error) {
	// Read pre-fetched response from dest pipeline
	commandResp, err := destPipe.ReadPacket()
	if err != nil {
		if err != io.EOF {
			utils.LogError(logger, err, "failed to read command response from the server")
		}
		return nil, time.Time{}, err
	}

	commandRespPkt, err := wire.DecodePayload(ctx, logger, commandResp, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode the command response packet")
		return nil, time.Time{}, err
	}

	if commandRespPkt.Header.Type == mysql.StatusToString(mysql.ERR) || commandRespPkt.Header.Type == mysql.StatusToString(mysql.OK) {
		logger.Debug("command response packet", zap.Any("packet", commandRespPkt.Header.Type))
		return commandRespPkt, time.Now(), nil
	}

	lastOp, ok := decodeCtx.LastOp.Load(clientConn)
	if !ok {
		return nil, time.Time{}, fmt.Errorf("failed to get the last operation from the context while handling the query response")
	}

	var queryResponsePkt *mysql.PacketBundle

	switch lastOp {
	case mysql.COM_QUERY:
		logger.Debug("Handling text result set", zap.Any("lastOp", lastOp))
		queryResponsePkt, err = handleTextResultSet(ctx, logger, clientConn, destPipe, commandRespPkt, decodeCtx)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("failed to handle the query response packet: %w", err)
		}
	case mysql.COM_STMT_PREPARE:
		logger.Debug("Handling prepare Statement Response OK", zap.Any("lastOp", lastOp))
		queryResponsePkt, err = handlePreparedStmtResponse(ctx, logger, clientConn, destPipe, commandRespPkt, decodeCtx)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("failed to handle the prepared statement response: %w", err)
		}
	case mysql.COM_STMT_EXECUTE:
		logger.Debug("Handling binary protocol result set", zap.Any("lastOp", lastOp))
		queryResponsePkt, err = handleBinaryResultSet(ctx, logger, clientConn, destPipe, commandRespPkt, decodeCtx)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("failed to handle the statement execute response: %w", err)
		}
	default:
		return nil, time.Time{}, fmt.Errorf("unsupported operation: %x", lastOp)
	}
	return queryResponsePkt, time.Now(), nil
}

// handlePreparedStmtResponse reads param/column definition packets from the
// dest pipeline and stores raw bytes for async decoding.
func handlePreparedStmtResponse(ctx context.Context, logger *zap.Logger, clientConn net.Conn, destPipe *packetPipeline, commandRespPkt *mysql.PacketBundle, decodeCtx *wire.DecodeContext) (*mysql.PacketBundle, error) {
	responseOk, ok := commandRespPkt.Message.(*mysql.StmtPrepareOkPacket)
	if !ok {
		return nil, fmt.Errorf("expected StmtPrepareOkPacket, got %T", commandRespPkt.Message)
	}

	logger.Debug("Parsing params and columns in prepared statement response", zap.Any("responseOk", responseOk))

	if responseOk.NumParams > 0 {
		for i := uint16(0); i < responseOk.NumParams; i++ {
			colData, err := destPipe.ReadPacket()
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read param definition packet")
				}
				return nil, err
			}
			responseOk.RawParamData = append(responseOk.RawParamData, colData)
		}

		eofData, err := destPipe.ReadPacket()
		if err != nil {
			if err != io.EOF {
				utils.LogError(logger, err, "failed to read EOF packet for param definition")
			}
			return nil, err
		}
		responseOk.RawEOFAfterParamDefs = eofData
	}

	if responseOk.NumColumns > 0 {
		for i := uint16(0); i < responseOk.NumColumns; i++ {
			colData, err := destPipe.ReadPacket()
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read column definition packet")
				}
				return nil, err
			}
			responseOk.RawColumnDefData = append(responseOk.RawColumnDefData, colData)
		}

		eofData, err := destPipe.ReadPacket()
		if err != nil {
			if err != io.EOF {
				utils.LogError(logger, err, "failed to read EOF packet for column definition")
			}
			return nil, err
		}
		responseOk.RawEOFAfterColumnDefs = eofData
	}

	decodeCtx.LastOp.Store(clientConn, mysql.OK)
	return commandRespPkt, nil
}

// handleTextResultSet reads column and row packets from the dest pipeline,
// storing raw bytes for async decoding in ProcessRawMocks.
func handleTextResultSet(ctx context.Context, logger *zap.Logger, clientConn net.Conn, destPipe *packetPipeline, textResultSetPkt *mysql.PacketBundle, decodeCtx *wire.DecodeContext) (*mysql.PacketBundle, error) {
	textResultSet, ok := textResultSetPkt.Message.(*mysql.TextResultSet)
	if !ok {
		return nil, fmt.Errorf("expected TextResultSet, got %T", textResultSetPkt.Message)
	}

	colCount := textResultSet.ColumnCount

	// Read column definition packets — store raw bytes for async decode
	for i := uint64(0); i < colCount; i++ {
		colData, err := destPipe.ReadPacket()
		if err != nil {
			if err != io.EOF {
				utils.LogError(logger, err, "failed to read column definition packet")
			}
			return nil, err
		}
		textResultSet.RawColumnData = append(textResultSet.RawColumnData, colData)
	}

	if decodeCtx.ClientCapabilities&mysql.CLIENT_DEPRECATE_EOF == 0 {
		eofData, err := destPipe.ReadPacket()
		if err != nil {
			if err != io.EOF {
				utils.LogError(logger, err, "failed to read EOF for column definition")
			}
			return nil, err
		}
		textResultSet.RawEOFAfterColumns = eofData
	}

	// Read row data packets until EOF
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			data, err := destPipe.ReadPacket()
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read row data packet")
				}
				return nil, err
			}

			if mysqlUtils.IsEOFPacket(data) {
				textResultSet.FinalResponse = &mysql.GenericResponse{
					Data: data,
					Type: mysql.StatusToString(mysql.EOF),
				}
				decodeCtx.LastOp.Store(clientConn, wire.RESET)
				return textResultSetPkt, nil
			}

			textResultSet.RawRowData = append(textResultSet.RawRowData, data)
		}
	}
}

// handleBinaryResultSet reads column and row packets from the dest pipeline,
// storing raw bytes for async decoding in ProcessRawMocks.
func handleBinaryResultSet(ctx context.Context, logger *zap.Logger, clientConn net.Conn, destPipe *packetPipeline, binaryResultSetPkt *mysql.PacketBundle, decodeCtx *wire.DecodeContext) (*mysql.PacketBundle, error) {
	binaryResultSet, ok := binaryResultSetPkt.Message.(*mysql.BinaryProtocolResultSet)
	if !ok {
		return nil, fmt.Errorf("expected BinaryProtocolResultSet, got %T", binaryResultSetPkt.Message)
	}

	colCount := binaryResultSet.ColumnCount
	logger.Debug("ColCount in handleBinaryResultSet", zap.Any("ColCount", colCount))

	// Read column definition packets — store raw bytes for async decode
	for i := uint64(0); i < colCount; i++ {
		colData, err := destPipe.ReadPacket()
		if err != nil {
			if err != io.EOF {
				utils.LogError(logger, err, "failed to read column definition packet")
			}
			return nil, err
		}
		binaryResultSet.RawColumnData = append(binaryResultSet.RawColumnData, colData)
	}

	eofData, err := destPipe.ReadPacket()
	if err != nil {
		if err != io.EOF {
			utils.LogError(logger, err, "failed to read EOF for column definition")
		}
		return nil, err
	}
	binaryResultSet.RawEOFAfterColumns = eofData

	// Read row data packets until EOF
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			data, err := destPipe.ReadPacket()
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read row data packet")
				}
				return nil, err
			}

			if mysqlUtils.IsEOFPacket(data) {
				binaryResultSet.FinalResponse = &mysql.GenericResponse{
					Data: data,
					Type: mysql.StatusToString(mysql.EOF),
				}
				decodeCtx.LastOp.Store(clientConn, wire.RESET)
				return binaryResultSetPkt, nil
			}

			binaryResultSet.RawRowData = append(binaryResultSet.RawRowData, data)
		}
	}
}
