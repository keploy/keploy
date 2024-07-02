package mysql

import (
	"encoding/binary"
	"fmt"

	"go.keploy.io/server/v2/pkg/models"
)

func decodeComStmtReset(packet []byte) (*models.MySQLCOMSTMTRESET, error) {
	if len(packet) != 5 || packet[0] != 0x1a {
		return nil, fmt.Errorf("invalid COM_STMT_RESET packet")
	}
	stmtID := binary.LittleEndian.Uint32(packet[1:5])
	return &models.MySQLCOMSTMTRESET{
		StatementID: stmtID}, nil
}
