//go:build linux

package recorder

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	mysqlUtils "go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/wire/phase/query/rowscols"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func handleClientQueries(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, mocks chan<- *models.Mock, decodeCtx *wire.DecodeContext) error {
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

			commandPkt, err := wire.DecodePayload(ctx, logger, command, clientConn, decodeCtx)
			if err != nil {
				utils.LogError(logger, err, "failed to decode the MySQL packet from the client")
				return err
			}

			requests = append(requests, mysql.Request{
				PacketBundle: *commandPkt,
			})

			// handle no response commands like COM_STMT_CLOSE, COM_STMT_SEND_LONG_DATA, etc
			if wire.IsNoResponseCommand(commandPkt.Header.Type) {
				recordMock(ctx, requests, responses, "mocks", commandPkt.Header.Type, "NO Response Packet", mocks, reqTimestamp)
				// reset the requests and responses
				requests = []mysql.Request{}
				responses = []mysql.Response{}
				logger.Debug("No response command", zap.Any("packet", commandPkt.Header.Type))
				continue
			}

			commandRespPkt, err := handleQueryResponse(ctx, logger, clientConn, destConn, decodeCtx)
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

			// record the mock
			recordMock(ctx, requests, responses, "mocks", commandPkt.Header.Type, commandRespPkt.Header.Type, mocks, reqTimestamp)

			// reset the requests and responses
			requests = []mysql.Request{}
			responses = []mysql.Response{}
		}
	}
}

func handleQueryResponse(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, decodeCtx *wire.DecodeContext) (*mysql.PacketBundle, error) {
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
	commandRespPkt, err := wire.DecodePayload(ctx, logger, commandResp, clientConn, decodeCtx)
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
		logger.Debug("Handling text result set", zap.Any("lastOp", lastOp))
		// handle the query response (TextResultSet)
		queryResponsePkt, err = handleTextResultSet(ctx, logger, clientConn, destConn, commandRespPkt, decodeCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to handle the query response packet: %w", err)
		}

	case mysql.COM_STMT_PREPARE:
		logger.Debug("Handling prepare Statement Response OK", zap.Any("lastOp", lastOp))
		// handle the prepared statement response (COM_STMT_PREPARE_OK)
		queryResponsePkt, err = handlePreparedStmtResponse(ctx, logger, clientConn, destConn, commandRespPkt, decodeCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to handle the prepared statement response: %w", err)
		}
	case mysql.COM_STMT_EXECUTE:
		logger.Debug("Handling binary protocol result set", zap.Any("lastOp", lastOp))
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

func handlePreparedStmtResponse(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, commandRespPkt *mysql.PacketBundle, decodeCtx *wire.DecodeContext) (*mysql.PacketBundle, error) {

	//commandRespPkt is the response to prepare, there are parameters, intermediate EOF, columns, and EOF packets to be handled
	//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_stmt_prepare.html#sect_protocol_com_stmt_prepare_response_ok

	responseOk, ok := commandRespPkt.Message.(*mysql.StmtPrepareOkPacket)
	if !ok {
		return nil, fmt.Errorf("expected StmtPrepareOkPacket, got %T", commandRespPkt.Message)
	}

	logger.Debug("Parsing the params and columns in the prepared statement response", zap.Any("responseOk", responseOk))

	//See if there are any parameters
	if responseOk.NumParams > 0 {
		for i := uint16(0); i < responseOk.NumParams; i++ {

			// Read the column definition packet
			colData, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read column data for parameter definition")
				}
				return nil, err
			}

			// Write the column definition packet to the client
			_, err = clientConn.Write(colData)
			if err != nil {
				utils.LogError(logger, err, "failed to write column data for parameter definition")
				return nil, err
			}

			// Decode the column definition packet
			column, _, err := rowscols.DecodeColumn(ctx, logger, colData)
			if err != nil {
				return nil, fmt.Errorf("failed to decode column definition packet: %w", err)
			}

			responseOk.ParamDefs = append(responseOk.ParamDefs, column)
		}

	logger.Debug("ParamsDefs after parsing", zap.Any("ParamDefs", responseOk.ParamDefs))

	// Read the EOF packet for parameter definition
	// Note: Some MySQL/TiDB servers skip EOF between params and columns when both are present
	// Also note: When CLIENT_DEPRECATE_EOF is set and NumColumns == 0, there's no EOF after params
	if responseOk.NumParams > 0 {
		// Only read EOF after params if:
		// 1. CLIENT_DEPRECATE_EOF is NOT set, OR
		// 2. NumColumns > 0 (there will be columns after params, so we need to read something)
		shouldReadEOFAfterParams := (decodeCtx.ClientCapabilities&mysql.CLIENT_DEPRECATE_EOF == 0) || (responseOk.NumColumns > 0)
		
		if shouldReadEOFAfterParams {
			eofData, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read EOF packet for parameter definition")
				}
				return nil, err
			}

			// Write the packet to the client (whether it's EOF or column definition)
			_, err = clientConn.Write(eofData)
			if err != nil {
				utils.LogError(logger, err, "failed to write packet after parameter definition to the client")
				return nil, err
			}

			// Check if it's an EOF packet or column definition
			// Some MySQL/TiDB servers skip EOF between params and columns when both are present
			if mysqlUtils.IsEOFPacket(eofData) {
				responseOk.EOFAfterParamDefs = eofData
				logger.Debug("Eof after param defs", zap.Any("eofData", eofData))
			} else if responseOk.NumColumns > 0 {
				// It's a column definition packet, not EOF - this is valid for some MySQL/TiDB versions
				// We need to decode it and add it to ColumnDefs, then continue reading columns
				logger.Debug("No EOF packet between params and columns, received column definition directly")
				
				// Decode the column definition packet
				column, _, err := rowscols.DecodeColumn(ctx, logger, eofData)
				if err != nil {
					return nil, fmt.Errorf("failed to decode column definition packet: %w", err)
				}
				
				responseOk.ColumnDefs = append(responseOk.ColumnDefs, column)
				// Set EOFAfterParamDefs to nil to indicate it wasn't sent
				responseOk.EOFAfterParamDefs = nil
			} else {
				// Not EOF and no columns expected - this is an error
				return nil, fmt.Errorf("expected EOF packet for parameter definition, got %v", eofData)
			}
		} else {
			// CLIENT_DEPRECATE_EOF is set and NumColumns == 0, so no EOF packet after params
			logger.Debug("CLIENT_DEPRECATE_EOF is set and NumColumns == 0, EOF packet after params is deprecated and not sent by server")
			responseOk.EOFAfterParamDefs = nil
		}
	}
	}

	//See if there are any columns
	if responseOk.NumColumns > 0 {
		// Start from the number of columns we've already read (if any)
		startIdx := len(responseOk.ColumnDefs)
		for i := uint16(startIdx); i < responseOk.NumColumns; i++ {

			// Read the column definition packet
			colData, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read column data for column definition")
				}
				return nil, err
			}

			// Write the column definition packet to the client
			_, err = clientConn.Write(colData)
			if err != nil {
				utils.LogError(logger, err, "failed to write column data for column definition")
				return nil, err
			}

			// Decode the column definition packet
			column, _, err := rowscols.DecodeColumn(ctx, logger, colData)
			if err != nil {
				return nil, fmt.Errorf("failed to decode column definition packet: %w", err)
			}

			responseOk.ColumnDefs = append(responseOk.ColumnDefs, column)
		}

		logger.Debug("ColumnDefs after parsing", zap.Any("ColumnDefs", responseOk.ColumnDefs))

		// Check if CLIENT_DEPRECATE_EOF capability is set
		// If set, EOF packets are deprecated and server won't send them (MySQL 5.7.5+)
		if decodeCtx.ClientCapabilities&mysql.CLIENT_DEPRECATE_EOF == 0 {
			logger.Debug("EOF packet is not deprecated, reading EOF after columns")

			// Read the EOF packet for column definition
			eofData, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read EOF packet for column definition")
				}
				return nil, err
			}

			// Write the EOF packet for column definition to the client
			_, err = clientConn.Write(eofData)
			if err != nil {
				utils.LogError(logger, err, "failed to write EOF packet for column definition to the client")
				return nil, err
			}

			// Validate the EOF packet for column definition
			if !mysqlUtils.IsEOFPacket(eofData) {
				return nil, fmt.Errorf("expected EOF packet for column definition, got %v, while handling prepared statement response", eofData)
			}

			responseOk.EOFAfterColumnDefs = eofData
			logger.Debug("Eof after column defs", zap.Any("eofData", eofData))
		} else {
			logger.Debug("CLIENT_DEPRECATE_EOF is set, EOF packet after columns is deprecated and not sent by server")
			responseOk.EOFAfterColumnDefs = nil
		}
	}

	//set the lastOp to COM_STMT_PREPARE_OK
	decodeCtx.LastOp.Store(clientConn, mysql.OK)

	// commandRespPkt.Message = responseOk // need to check whether this is necessary

	return commandRespPkt, nil
}

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_query_response_text_resultset.html

func handleTextResultSet(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, textResultSetPkt *mysql.PacketBundle, decodeCtx *wire.DecodeContext) (*mysql.PacketBundle, error) {

	// colCountPkt is the first packet of the text result set, it is followed by column definition packets, intermediate eof, row data packets and final eof

	textResultSet, ok := textResultSetPkt.Message.(*mysql.TextResultSet)
	if !ok {
		return nil, fmt.Errorf("expected TextResultSet, got %T", textResultSetPkt.Message)
	}

	// Read the column count packet
	colCount := textResultSet.ColumnCount

	// Read the column definition packets
	for i := uint64(0); i < colCount; i++ {
		// Read the column definition packet
		colData, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
		if err != nil {
			if err != io.EOF {
				utils.LogError(logger, err, "failed to read column definition packet")
			}
			return nil, err
		}

		// Write the column definition packet to the client
		_, err = clientConn.Write(colData)
		if err != nil {
			utils.LogError(logger, err, "failed to write column definition packet")
			return nil, err
		}

		// Decode the column definition packet
		column, _, err := rowscols.DecodeColumn(ctx, logger, colData)
		if err != nil {
			return nil, fmt.Errorf("failed to decode column definition packet: %w", err)
		}

		textResultSet.Columns = append(textResultSet.Columns, column)
	}

	if decodeCtx.ClientCapabilities&mysql.CLIENT_DEPRECATE_EOF == 0 {
		logger.Debug("EOF packet is not deprecated while handling textResultSet")

		// Read the EOF packet for column definition
		eofData, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
		if err != nil {
			if err != io.EOF {
				utils.LogError(logger, err, "failed to read EOF packet for column definition")
			}
			return nil, err
		}

		// Write the EOF packet for column definition to the client
		_, err = clientConn.Write(eofData)
		if err != nil {
			utils.LogError(logger, err, "failed to write EOF packet for column definition to the client")
			return nil, err
		}

		// Validate the EOF packet for column definition
		if !mysqlUtils.IsEOFPacket(eofData) {
			return nil, fmt.Errorf("expected EOF packet for column definition, got %v, while handling textResultSet", eofData)
		}

		textResultSet.EOFAfterColumns = eofData

	}
	// Read the row data packets
rowLoop:
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:

			// Read the packet
			data, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read data packet while reading row data")
				}
				return nil, err
			}

			// Write the packet to the client
			_, err = clientConn.Write(data)
			if err != nil {
				utils.LogError(logger, err, "failed to write data packet while reading row data")
				return nil, err
			}

			// // Break if the data packet is a generic response
			// resp, ok := mysqlUtils.IsGenericResponse(data)
			// if ok {
			// 	textResultSet.FinalResponse = &mysql.GenericResponse{
			// 		Data: data,
			// 		Type: resp,
			// 	}
			// 	break rowLoop
			// }

			// Break if the data packet is an EOF packet, But we need to check for generic response
			// Right now we are just checking for EOF packet as we couldn't differentiate between the generic response and row data packet
			if mysqlUtils.IsEOFPacket(data) {
				logger.Debug("Found EOF packet after row data in text resultset")
				textResultSet.FinalResponse = &mysql.GenericResponse{
					Data: data,
					Type: mysql.StatusToString(mysql.EOF),
				}
				break rowLoop
			}

			// It must be a row data packet
			row, _, err := rowscols.DecodeTextRow(ctx, logger, data, textResultSet.Columns)
			if err != nil {
				return nil, fmt.Errorf("failed to decode row data packet: %w", err)
			}
			textResultSet.Rows = append(textResultSet.Rows, row)
		}
	}

	// reset the last OP
	decodeCtx.LastOp.Store(clientConn, wire.RESET)

	return textResultSetPkt, nil
}

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_binary_resultset.html
// (BinaryProtocolResultset)

func handleBinaryResultSet(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, binaryResultSetPkt *mysql.PacketBundle, decodeCtx *wire.DecodeContext) (*mysql.PacketBundle, error) {

	// colCountPkt is the first packet of the binary result set, it is followed by column definition packets,intermediate eof, row data packets and final eof

	binaryResultSet, ok := binaryResultSetPkt.Message.(*mysql.BinaryProtocolResultSet)
	if !ok {
		return nil, fmt.Errorf("expected TextResultSet, got %T", binaryResultSetPkt.Message)
	}

	// Read the column count packet
	colCount := binaryResultSet.ColumnCount

	logger.Debug("ColCount in handleBinaryResultSet: ", zap.Any("ColCount", colCount))
	// Read the column definition packets
	for i := uint64(0); i < colCount; i++ {
		// Read the column definition packet
		colData, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
		if err != nil {
			if err != io.EOF {
				utils.LogError(logger, err, "failed to read column definition packet")
			}
			return nil, err
		}

		// Write the column definition packet to the client
		_, err = clientConn.Write(colData)
		if err != nil {
			utils.LogError(logger, err, "failed to write column definition packet")
			return nil, err
		}

		// Decode the column definition packet
		column, _, err := rowscols.DecodeColumn(ctx, logger, colData)
		if err != nil {
			return nil, fmt.Errorf("failed to decode column definition packet: %w", err)
		}

		binaryResultSet.Columns = append(binaryResultSet.Columns, column)
	}

	logger.Debug("Columns: ", zap.Any("Columns", binaryResultSet.Columns))

	// Check if CLIENT_DEPRECATE_EOF capability is set
	// If set, EOF packets are deprecated and server won't send them (MySQL 5.7.5+)
	if decodeCtx.ClientCapabilities&mysql.CLIENT_DEPRECATE_EOF == 0 {
		logger.Debug("EOF packet is not deprecated, reading EOF after columns in binary result set")

		// Read the EOF packet for column definition
		eofData, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
		if err != nil {
			if err != io.EOF {
				utils.LogError(logger, err, "failed to read EOF packet for column definition")
			}
			return nil, err
		}

		// Write the EOF packet for column definition to the client
		_, err = clientConn.Write(eofData)
		if err != nil {
			utils.LogError(logger, err, "failed to write EOF packet for column definition to the client")
			return nil, err
		}

		// Validate the EOF packet for column definition
		if !mysqlUtils.IsEOFPacket(eofData) {
			return nil, fmt.Errorf("expected EOF packet for column definition, got %v, while handling BinaryProtocolResultSet", eofData)
		}

		binaryResultSet.EOFAfterColumns = eofData
		logger.Debug("Eof after column defs in binary result set", zap.Any("eofData", eofData))
	} else {
		logger.Debug("CLIENT_DEPRECATE_EOF is set, EOF packet after columns is deprecated and not sent by server in binary result set")
		binaryResultSet.EOFAfterColumns = nil
	}

	// Read the row data packets
rowLoop:
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:

			// Read the packet
			data, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read data packet while reading row data")
				}
				return nil, err
			}

			// Write the packet to the client
			_, err = clientConn.Write(data)
			if err != nil {
				utils.LogError(logger, err, "failed to write data packet while reading row data")
				return nil, err
			}

			// Break if the data packet is a generic response
			// resp, ok := mysqlUtils.IsGenericResponse(data)
			// if ok {
			// 	binaryResultSet.FinalResponse = &mysql.GenericResponse{
			// 		Data: data,
			// 		Type: resp,
			// 	}
			// 	//debug log
			// 	fmt.Println("Found generic response after row data")
			// 	break rowLoop
			// }

			// Break if the data packet is an EOF packet, But we need to check for generic response
			// Right now we are just checking for EOF packet as we couldn't differentiate between the generic response and row data packet
			if mysqlUtils.IsEOFPacket(data) {
				logger.Debug("Found EOF packet after row data in binary resultset")
				binaryResultSet.FinalResponse = &mysql.GenericResponse{
					Data: data,
					Type: mysql.StatusToString(mysql.EOF),
				}
				break rowLoop
			}

			// It must be a row data packet
			row, _, err := rowscols.DecodeBinaryRow(ctx, logger, data, binaryResultSet.Columns)
			if err != nil {
				return nil, fmt.Errorf("failed to decode row data packet: %w", err)
			}
			binaryResultSet.Rows = append(binaryResultSet.Rows, row)
		}
	}

	logger.Debug("Rows: ", zap.Any("Rows", binaryResultSet.Rows))

	// reset the last OP
	decodeCtx.LastOp.Store(clientConn, wire.RESET)

	return binaryResultSetPkt, nil

}
