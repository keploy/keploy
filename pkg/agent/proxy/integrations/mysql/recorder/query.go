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

// handleClientQueries processes the MySQL command phase.
// It reads directly from the TeeForwardConn (which uses a ring buffer) to avoid
// extra channel overhead.
func handleClientQueries(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, mocks chan<- *models.Mock, decodeCtx *wire.DecodeContext, opts models.OutgoingOptions) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Read command from client connection (TeeForwardConn ring buffer)
			command, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read command packet from client")
				}
				return err
			}

			reqTimestamp := time.Now()

			// Create NEW slices for each iteration to avoid race conditions with async mock recording
			requests := make([]mysql.Request, 0, 1)
			responses := make([]mysql.Response, 0, 1)

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
				continue
			}

			commandRespPkt, resTimestamp, err := handleQueryResponse(ctx, logger, clientConn, destConn, decodeCtx)
			if err != nil {
				if err == io.EOF && commandPkt.Header.Type == mysql.CommandStatusToString(mysql.COM_QUIT) {
					return err
				}
				utils.LogError(logger, err, "failed to handle the query response")
				return err
			}

			responses = append(responses, mysql.Response{
				PacketBundle: *commandRespPkt,
			})

			recordMock(ctx, requests, responses, "mocks", commandPkt.Header.Type, commandRespPkt.Header.Type, mocks, reqTimestamp, resTimestamp, opts)
		}
	}
}

// handleQueryResponse reads response packets from the dest connection.
// clientConn is the TeeForwardConn used as decodeCtx map key only.
func handleQueryResponse(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, decodeCtx *wire.DecodeContext) (*mysql.PacketBundle, time.Time, error) {
	// Read response from dest connection
	commandResp, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
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
		return commandRespPkt, time.Now(), nil
	}

	lastOp, ok := decodeCtx.LastOp.Load(clientConn)
	if !ok {
		return nil, time.Time{}, fmt.Errorf("failed to get the last operation from the context while handling the query response")
	}

	var queryResponsePkt *mysql.PacketBundle

	switch lastOp {
	case mysql.COM_QUERY:
		queryResponsePkt, err = handleTextResultSet(ctx, logger, clientConn, destConn, commandRespPkt, decodeCtx)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("failed to handle the query response packet: %w", err)
		}
	case mysql.COM_STMT_PREPARE:
		queryResponsePkt, err = handlePreparedStmtResponse(ctx, logger, clientConn, destConn, commandRespPkt, decodeCtx)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("failed to handle the prepared statement response: %w", err)
		}
	case mysql.COM_STMT_EXECUTE:
		queryResponsePkt, err = handleBinaryResultSet(ctx, logger, clientConn, destConn, commandRespPkt, decodeCtx)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("failed to handle the statement execute response: %w", err)
		}
	default:
		return nil, time.Time{}, fmt.Errorf("unsupported operation: %x", lastOp)
	}
	return queryResponsePkt, time.Now(), nil
}

// handlePreparedStmtResponse reads param/column definition packets from the
// dest connection and stores raw bytes for async decoding.
func handlePreparedStmtResponse(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, commandRespPkt *mysql.PacketBundle, decodeCtx *wire.DecodeContext) (*mysql.PacketBundle, error) {
	responseOk, ok := commandRespPkt.Message.(*mysql.StmtPrepareOkPacket)
	if !ok {
		return nil, fmt.Errorf("expected StmtPrepareOkPacket, got %T", commandRespPkt.Message)
	}

	if responseOk.NumParams > 0 {
		for i := uint16(0); i < responseOk.NumParams; i++ {
			colData, err := mysqlUtils.ReadPacketBufferPooled(ctx, logger, destConn)
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read param definition packet")
				}
				return nil, err
			}
			responseOk.RawParamData = append(responseOk.RawParamData, colData)
		}

		eofData, err := mysqlUtils.ReadPacketBufferPooled(ctx, logger, destConn)
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
			colData, err := mysqlUtils.ReadPacketBufferPooled(ctx, logger, destConn)
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read column definition packet")
				}
				return nil, err
			}
			responseOk.RawColumnDefData = append(responseOk.RawColumnDefData, colData)
		}

		eofData, err := mysqlUtils.ReadPacketBufferPooled(ctx, logger, destConn)
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

// handleTextResultSet reads column and row packets from the dest connection,
// storing raw bytes for async decoding in ProcessRawMocks.
func handleTextResultSet(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, textResultSetPkt *mysql.PacketBundle, decodeCtx *wire.DecodeContext) (*mysql.PacketBundle, error) {
	textResultSet, ok := textResultSetPkt.Message.(*mysql.TextResultSet)
	if !ok {
		return nil, fmt.Errorf("expected TextResultSet, got %T", textResultSetPkt.Message)
	}

	colCount := textResultSet.ColumnCount

	// Read column definition packets — store raw bytes for async decode
	for i := uint64(0); i < colCount; i++ {
		colData, err := mysqlUtils.ReadPacketBufferPooled(ctx, logger, destConn)
		if err != nil {
			if err != io.EOF {
				utils.LogError(logger, err, "failed to read column definition packet")
			}
			return nil, err
		}
		textResultSet.RawColumnData = append(textResultSet.RawColumnData, colData)
	}

	if decodeCtx.ClientCapabilities&mysql.CLIENT_DEPRECATE_EOF == 0 {
		eofData, err := mysqlUtils.ReadPacketBufferPooled(ctx, logger, destConn)
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
			// Read packet directly from the connection using pooled buffer
			data, err := mysqlUtils.ReadPacketBufferPooled(ctx, logger, destConn)
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

// handleBinaryResultSet reads column and row packets from the dest connection,
// storing raw bytes for async decoding in ProcessRawMocks.
func handleBinaryResultSet(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, binaryResultSetPkt *mysql.PacketBundle, decodeCtx *wire.DecodeContext) (*mysql.PacketBundle, error) {
	binaryResultSet, ok := binaryResultSetPkt.Message.(*mysql.BinaryProtocolResultSet)
	if !ok {
		return nil, fmt.Errorf("expected BinaryProtocolResultSet, got %T", binaryResultSetPkt.Message)
	}

	colCount := binaryResultSet.ColumnCount

	// Read column definition packets — store raw bytes for async decode
	for i := uint64(0); i < colCount; i++ {
		colData, err := mysqlUtils.ReadPacketBufferPooled(ctx, logger, destConn)
		if err != nil {
			if err != io.EOF {
				utils.LogError(logger, err, "failed to read column definition packet")
			}
			return nil, err
		}
		binaryResultSet.RawColumnData = append(binaryResultSet.RawColumnData, colData)
	}

	eofData, err := mysqlUtils.ReadPacketBufferPooled(ctx, logger, destConn)
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
			// Read packet directly from the connection using pooled buffer
			data, err := mysqlUtils.ReadPacketBufferPooled(ctx, logger, destConn)
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
