//go:build linux

package mysql

import (
	"encoding/binary"
	"fmt"

	"go.keploy.io/server/v2/pkg/models"
)

type BoundParameter struct {
	Type     byte   `yaml:"type"`
	Unsigned byte   `yaml:"unsigned"`
	Value    []byte `yaml:"value,omitempty,flow"`
}

func decodeComStmtExecute(packet []byte) (models.MySQLComStmtExecute, error) {
	// removed the print statement for cleanliness
	if len(packet) < 14 { // the minimal size of the packet without parameters should be 14, not 13
		return models.MySQLComStmtExecute{}, fmt.Errorf("packet length less than 14 bytes")
	}

	stmtExecute := models.MySQLComStmtExecute{}
	stmtExecute.StatementID = binary.LittleEndian.Uint32(packet[1:5])
	stmtExecute.Flags = packet[5]
	stmtExecute.IterationCount = binary.LittleEndian.Uint32(packet[6:10])

	// the next bytes are reserved for the Null-Bitmap, Parameter Bound Flag and Bound Parameters if they exist
	// if the length of the packet is greater than 14, then there are parameters
	if len(packet) > 14 {
		nullBitmapLength := int((stmtExecute.ParamCount + 7) / 8)

		stmtExecute.NullBitmap = packet[10 : 10+nullBitmapLength]
		stmtExecute.ParamCount = binary.LittleEndian.Uint16(packet[10+nullBitmapLength:])

		// in case new parameters are bound, the new types and values are sent
		if packet[10+nullBitmapLength] == 1 {
			// read the types and values of the new parameters
			stmtExecute.Parameters = make([]models.BoundParameter, stmtExecute.ParamCount)
			for i := 0; i < int(stmtExecute.ParamCount); i++ {
				index := 10 + nullBitmapLength + 1 + 2*i
				if index+1 >= len(packet) {
					return models.MySQLComStmtExecute{}, fmt.Errorf("packet length less than expected while reading parameters")
				}
				stmtExecute.Parameters[i].Type = packet[index]
				stmtExecute.Parameters[i].Unsigned = packet[index+1]
			}
		}
	}

	return stmtExecute, nil
}
