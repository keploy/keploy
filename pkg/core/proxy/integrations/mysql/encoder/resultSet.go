//go:build linux

package encoder

import (
	"context"
	"fmt"
	"io"
	"net"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/command/rowscols"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/operation"
	mysqlUtils "go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_query_response_text_resultset.html

func handleTextResultSet(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, textResultSetPkt *mysql.PacketBundle, decodeCtx *operation.DecodeContext) (*mysql.PacketBundle, error) {

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
				//debug log
				fmt.Println("Found EOF packet after row data")
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
	decodeCtx.LastOp.Store(clientConn, operation.RESET)

	return textResultSetPkt, nil
}

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_binary_resultset.html
// (BinaryProtocolResultset)

func handleBinaryResultSet(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, binaryResultSetPkt *mysql.PacketBundle, decodeCtx *operation.DecodeContext) (*mysql.PacketBundle, error) {

	// colCountPkt is the first packet of the binary result set, it is followed by column definition packets,intermediate eof, row data packets and final eof

	binaryResultSet, ok := binaryResultSetPkt.Message.(*mysql.BinaryProtocolResultSet)
	if !ok {
		return nil, fmt.Errorf("expected TextResultSet, got %T", binaryResultSetPkt.Message)
	}

	// Read the column count packet
	colCount := binaryResultSet.ColumnCount

	//debug log
	println("ColCount in handleBinaryResultSet: ", colCount)
	// Read the column definition packets
	for i := uint64(0); i < colCount; i++ {
		// Read the column definition packet
		//debug log
		println("Reading column count...")

		colData, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
		if err != nil {
			if err != io.EOF {
				utils.LogError(logger, err, "failed to read column definition packet")
			}
			return nil, err
		}
		//debug log
		println("After reading column count...")

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

	//debug log
	logger.Info("Columns: ", zap.Any("Columns", binaryResultSet.Columns))

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

			//debug log
			println("Trying to read row data...")

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
				//debug log
				fmt.Println("Found EOF packet after row data")
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

	//debug log
	logger.Info("Rows: ", zap.Any("Rows", binaryResultSet.Rows))

	// reset the last OP
	decodeCtx.LastOp.Store(clientConn, operation.RESET)

	return binaryResultSetPkt, nil

}
