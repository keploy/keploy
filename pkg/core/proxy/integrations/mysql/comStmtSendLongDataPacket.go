//go:build linux

package mysql

import (
	"encoding/binary"
	"fmt"

	"go.keploy.io/server/v2/pkg/models"
)

func decodeComStmtSendLongData(packet []byte) (models.MySQLCOMSTMTSENDLONGDATA, error) {
	if len(packet) < 7 || packet[0] != 0x18 {
		return models.MySQLCOMSTMTSENDLONGDATA{}, fmt.Errorf("invalid COM_STMT_SEND_LONG_DATA packet")
	}
	stmtID := binary.LittleEndian.Uint32(packet[1:5])
	paramID := binary.LittleEndian.Uint16(packet[5:7])
	data := packet[7:]
	return models.MySQLCOMSTMTSENDLONGDATA{
		StatementID: stmtID,
		ParameterID: paramID,
		Data:        data,
	}, nil
}
