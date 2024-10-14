//go:build linux

package preparedstmt

import (
	"context"
	"encoding/binary"
	"errors"

	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.uber.org/zap"
)

// COM_STMT_FETCH: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_stmt_fetch.html

func DecodeStmtFetch(_ context.Context, _ *zap.Logger, data []byte) (*mysql.StmtFetchPacket, error) {
	if len(data) < 9 {
		return &mysql.StmtFetchPacket{}, errors.New("Data too short for COM_STMT_FETCH")
	}

	packet := &mysql.StmtFetchPacket{
		Status: data[0],
	}

	packet.StatementID = binary.LittleEndian.Uint32(data[1:5])
	packet.NumRows = binary.LittleEndian.Uint32(data[5:9])

	return packet, nil
}
