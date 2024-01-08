package mysqlparser

import (
	"encoding/binary"
	"errors"
)

type ComStmtFetchPacket struct {
	StatementID uint32 `yaml:"statement_id"`
	RowCount    uint32 `yaml:"row_count"`
	Info        string `yaml:"info"`
}

func decodeComStmtFetch(data []byte) (ComStmtFetchPacket, error) {
	if len(data) < 9 {
		return ComStmtFetchPacket{}, errors.New("Data too short for COM_STMT_FETCH")
	}

	statementID := binary.LittleEndian.Uint32(data[1:5])
	rowCount := binary.LittleEndian.Uint32(data[5:9])

	// Assuming the info starts at the 10th byte
	infoData := data[9:]
	info := string(infoData)

	return ComStmtFetchPacket{
		StatementID: statementID,
		RowCount:    rowCount,
		Info:        info,
	}, nil
}
