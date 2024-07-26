//go:build linux

package preparedstmt

import (
	"context"
	"encoding/binary"
	"errors"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/models/mysql"
)

func DecodeCloseAndQuery(_ context.Context, data []byte) (*mysql.StmtCloseAndQueryPacket, error) {
	if len(data) < 10 {
		return nil, errors.New("data too short for COM_STMT_CLOSE and COM_QUERY with header")
	}

	packet := &mysql.StmtCloseAndQueryPacket{
		StmtClose: mysql.StmtClosePacket{
			Status:      data[0],
			StatementID: binary.LittleEndian.Uint32(data[1:5]),
		},
		QueryHeader: mysql.Header{
			PayloadLength: utils.ReadUint24(data[5:8]),
			SequenceID:    data[8],
		},
		StmtQuery: mysql.QueryPacket{
			Command: data[9],
			Query:   string(data[10:]),
		},
	}

	return packet, nil
}
