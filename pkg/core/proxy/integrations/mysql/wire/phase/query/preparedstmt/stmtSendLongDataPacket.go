//go:build linux

package preparedstmt

import (
	"context"
	"encoding/binary"
	"errors"

	"go.keploy.io/server/v2/pkg/models/mysql"
)

// COM_STMT_SEND_LONG_DATA: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_stmt_send_long_data.html

func DecodeStmtSendLongData(_ context.Context, data []byte) (*mysql.StmtSendLongDataPacket, error) {
	if len(data) < 7 || data[0] != 0x18 {
		return &mysql.StmtSendLongDataPacket{}, errors.New("invalid COM_STMT_SEND_LONG_DATA packet")
	}

	packet := &mysql.StmtSendLongDataPacket{
		Status:      data[0],
		StatementID: binary.LittleEndian.Uint32(data[1:5]),
		ParameterID: binary.LittleEndian.Uint16(data[5:7]),
		Data:        data[7:],
	}

	return packet, nil
}
