//go:build linux

package mysql

import (
	"encoding/binary"
	"errors"

	"go.keploy.io/server/v2/pkg/models"
)

func decodeComStmtClose(data []byte) (*models.ComStmtClosePacket, error) {
	if len(data) < 5 {
		return nil, errors.New("data too short for COM_STMT_CLOSE")
	}
	status := data[0]

	// Statement ID is 4-byte, little-endian integer after command byte
	statementID := binary.LittleEndian.Uint32(data[1:])
	return &models.ComStmtClosePacket{
		Status:      status,
		StatementID: statementID,
	}, nil
}
