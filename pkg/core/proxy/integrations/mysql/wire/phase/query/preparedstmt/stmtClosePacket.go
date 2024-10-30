//go:build linux || windows

// Package preparedstmt provides functionality for decoding prepared statement packets.
package preparedstmt

import (
	"context"
	"encoding/binary"
	"errors"

	"go.keploy.io/server/v2/pkg/models/mysql"
)

//COM_STMT_CLOSE: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_stmt_close.html

func DecoderStmtClose(_ context.Context, data []byte) (*mysql.StmtClosePacket, error) {
	if len(data) != 5 {
		return nil, errors.New("invalid packet for COM_STMT_CLOSE")
	}

	packet := &mysql.StmtClosePacket{
		Status:      data[0],
		StatementID: binary.LittleEndian.Uint32(data[1:5]),
	}
	return packet, nil
}
