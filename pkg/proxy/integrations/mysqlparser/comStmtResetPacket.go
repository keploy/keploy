package mysqlparser

import (
	"encoding/binary"
	"fmt"
)

type COM_STMT_RESET_PACKET struct {
	StatementID uint32
}

func decodeComStmtReset(packet []byte) (*COM_STMT_RESET_PACKET, error) {
	if len(packet) != 5 || packet[0] != 0x1a {
		return nil, fmt.Errorf("invalid COM_STMT_RESET packet")
	}
	stmtID := binary.LittleEndian.Uint32(packet[1:5])
	return &COM_STMT_RESET_PACKET{
		StatementID: stmtID}, nil
}
