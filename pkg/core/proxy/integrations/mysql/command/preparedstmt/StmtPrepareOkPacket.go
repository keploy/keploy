//go:build linux

package preparedstmt

import (
	"context"
	"encoding/binary"
	"errors"

	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.uber.org/zap"
)

// COM_STMT_PREPARE_OK: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_stmt_prepare.html#sect_protocol_com_stmt_prepare_response_ok

func DecodePrepareOk(_ context.Context, _ *zap.Logger, data []byte) (*mysql.StmtPrepareOkPacket, error) {
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
	// offset += 2

	return response, nil
}
