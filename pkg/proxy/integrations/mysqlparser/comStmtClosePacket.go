package mysqlparser

import (
	"encoding/binary"
	"errors"
)

type ComStmtClosePacket struct {
	Status      byte
	StatementID uint32
}

func decodeComStmtClose(data []byte) (*ComStmtClosePacket, error) {
	if len(data) < 5 {
		return nil, errors.New("data too short for COM_STMT_CLOSE")
	}
	status := data[0]

	// Statement ID is 4-byte, little-endian integer after command byte
	statementID := binary.LittleEndian.Uint32(data[1:])
	return &ComStmtClosePacket{
		Status:      status,
		StatementID: statementID,
	}, nil
}
