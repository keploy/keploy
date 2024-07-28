//go:build linux

package preparedstmt

import (
	"context"
	"encoding/binary"
	"errors"

	"go.keploy.io/server/v2/pkg/models"
)

//COM_STMT_CLOSE: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_stmt_close.html

func DecoderStmtClose(_ context.Context, data []byte) (*models.ComStmtClosePacket, error) {
	if len(data) != 5 {
		return nil, errors.New("invalid packet for COM_STMT_CLOSE")
	}

	packet := &models.ComStmtClosePacket{
		Status:      data[0],
		StatementID: binary.LittleEndian.Uint32(data[1:5]),
	}
	return packet, nil
}