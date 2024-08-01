//go:build linux

package mysql

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

func handlePreparedStmtResponse(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, commandRespPkt *mysql.PacketBundle, _ *operation.DecodeContext) (*mysql.PacketBundle, error) {

	//commandRespPkt is the response to prepare, there are parameters, intermediate EOF, columns, and EOF packets to be handled
	//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_stmt_prepare.html#sect_protocol_com_stmt_prepare_response_ok

	responseOk, ok := commandRespPkt.Message.(*mysql.StmtPrepareOkPacket)
	if !ok {
		return nil, fmt.Errorf("expected StmtPrepareOkPacket, got %T", commandRespPkt.Message)
	}

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
	}

	// Read the EOF packet for parameter definition
	eofData, err := mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
	if err != nil {
		if err != io.EOF {
			utils.LogError(logger, err, "failed to read EOF packet for parameter definition")
		}
		return nil, err
	}

	// Write the EOF packet for parameter definition to the client
	_, err = clientConn.Write(eofData)
	if err != nil {
		utils.LogError(logger, err, "failed to write EOF packet for parameter definition to the client")
		return nil, err
	}

	// Validate the EOF packet for parameter definition
	if !mysqlUtils.IsEOFPacket(eofData) {
		return nil, fmt.Errorf("expected EOF packet for parameter definition, got %v", eofData)
	}

	responseOk.EOFAfterParamDefs = eofData

	//See if there are any columns
	if responseOk.NumColumns > 0 {
		for i := uint16(0); i < responseOk.NumColumns; i++ {

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
	}

	// Read the EOF packet for column definition
	eofData, err = mysqlUtils.ReadPacketBuffer(ctx, logger, destConn)
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
		return nil, fmt.Errorf("expected EOF packet for column definition, got %v", eofData)
	}

	responseOk.EOFAfterColumnDefs = eofData

	// commandRespPkt.Message = responseOk // need to check whether this is necessary

	return commandRespPkt, nil
}
