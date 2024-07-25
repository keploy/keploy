//go:build linux

package mysql

import (
	"encoding/binary"
	"errors"

	"go.keploy.io/server/v2/pkg/models"
)

// DecodeComStmtFetch decodes the COM_STMT_FETCH packet.
func DecodeComStmtFetch(data []byte) (models.MySQLComStmtFetchPacket, error) {
	if len(data) < 9 {
		return models.MySQLComStmtFetchPacket{}, errors.New("Data too short for COM_STMT_FETCH")
	}

	statementID := binary.LittleEndian.Uint32(data[1:5])
	rowCount := binary.LittleEndian.Uint32(data[5:9])

	// Assuming the info starts at the 10th byte
	infoData := data[9:]
	info := string(infoData)

	return models.MySQLComStmtFetchPacket{
		StatementID: statementID,
		RowCount:    rowCount,
		Info:        info,
	}, nil
}
