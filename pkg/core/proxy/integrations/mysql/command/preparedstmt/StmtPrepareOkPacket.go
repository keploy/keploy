//go:build linux

package preparedstmt

import (
	"context"
	"encoding/binary"
	"errors"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/command/rowscols"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.uber.org/zap"
)

// COM_STMT_PREPARE_OK: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_stmt_prepare.html#sect_protocol_com_stmt_prepare_response_ok

func DecodePrepareOk(ctx context.Context, logger *zap.Logger, data []byte) (*mysql.StmtPrepareOkPacket, error) {
	if len(data) < 12 {
		return nil, errors.New("data length is not enough for COM_STMT_PREPARE_OK")
	}

	offset := 0

	response := &mysql.StmtPrepareOkPacket{}

	response.Status = data[offset]
	offset++

	response.StatementID = binary.LittleEndian.Uint32(data[offset : offset+4])
	offset += 4

	response.NumColumns = binary.LittleEndian.Uint16(data[offset : offset+2])
	offset += 2

	response.NumParams = binary.LittleEndian.Uint16(data[offset : offset+2])
	offset += 2

	//data[10] is reserved byte ([00] filler)
	response.Filler = data[offset]
	offset++

	response.WarningCount = binary.LittleEndian.Uint16(data[offset : offset+2])
	offset += 2

	data = data[offset:]

	if response.NumParams > 0 {
		offset = 0
		for i := uint16(0); i < response.NumParams; i++ {
			column, n, err := rowscols.DecodeColumn(ctx, logger, data)
			if err != nil {
				return nil, err
			}
			response.ParamDefs = append(response.ParamDefs, *column)
			offset += n
		}
		response.EOFAfterParamDefs = data[offset : offset+9]
		offset += 9 //skip EOF packet for Parameter Definition
		data = data[offset:]
	}

	if response.NumColumns > 0 {
		offset = 0
		for i := uint16(0); i < response.NumColumns; i++ {
			column, n, err := rowscols.DecodeColumn(ctx, logger, data)
			if err != nil {
				return nil, err
			}
			response.ColumnDefs = append(response.ColumnDefs, *column)
			offset += n
		}
		response.EOFAfterColumnDefs = data[offset : offset+9]
		// offset += 9 //skip EOF packet for Column Definitions
		// data = data[offset:]
	}

	return response, nil
}
