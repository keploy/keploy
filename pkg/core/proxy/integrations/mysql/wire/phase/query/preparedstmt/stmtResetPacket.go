//go:build linux

package preparedstmt

import (
	"context"
	"encoding/binary"
	"fmt"

	"go.keploy.io/server/v2/pkg/models/mysql"
)

//COM_STMT_RESET: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_stmt_reset.html

func DecodeStmtReset(_ context.Context, data []byte) (*mysql.StmtResetPacket, error) {
	if len(data) != 5 {
		return nil, fmt.Errorf("invalid COM_STMT_RESET packet")
	}

	packet := &mysql.StmtResetPacket{
		Status:      data[0],
		StatementID: binary.LittleEndian.Uint32(data[1:5]),
	}
	return packet, nil
}
