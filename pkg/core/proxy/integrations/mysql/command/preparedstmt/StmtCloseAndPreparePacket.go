//go:build linux

// Package preparedstmt provides utilities for encoding & decoding MySQL prepared statement packets.
package preparedstmt

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/models/mysql"
)

func DecodeCloseAndPrepare(_ context.Context, data []byte) (*mysql.StmtCloseAndPreparePacket, error) {
	if len(data) < 10 {
		return nil, errors.New("data too short for COM_STMT_CLOSE and COM_STMT_PREPARE with header")
	}

	packet := &mysql.StmtCloseAndPreparePacket{
		StmtClose: mysql.StmtClosePacket{
			Status:      data[0],
			StatementID: binary.LittleEndian.Uint32(data[1:5]),
		},
		PrepareHeader: mysql.Header{
			PayloadLength: utils.ReadUint24(data[5:8]),
			SequenceID:    data[8],
		},
		StmtPrepare: mysql.StmtPreparePacket{
			Command: data[9],
		},
	}

	// Get the query string after the header
	query := string(data[10:])
	packet.StmtPrepare.Query = strings.ReplaceAll(query, "\t", "")

	return packet, nil
}
