package mysql

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
)

type ComStmtExecute struct {
	StatementID    uint32           `json:"statement_id,omitempty" yaml:"statement_id,omitempty,flow"`
	Flags          byte             `json:"flags,omitempty" yaml:"flags,omitempty,flow"`
	IterationCount uint32           `json:"iteration_count,omitempty" yaml:"iteration_count,omitempty,flow"`
	NullBitmap     string           `json:"null_bitmap,omitempty" yaml:"null_bitmap,omitempty,flow"`
	ParamCount     uint16           `json:"param_count,omitempty" yaml:"param_count,omitempty,flow"`
	Parameters     []BoundParameter `json:"parameters,omitempty" yaml:"parameters,omitempty,flow"`
}

type BoundParameter struct {
	Type     byte   `json:"type,omitempty" yaml:"type,omitempty,flow"`
	Unsigned byte   `json:"unsigned,omitempty" yaml:"unsigned,omitempty,flow"`
	Value    []byte `json:"value,omitempty" yaml:"value,omitempty,flow"`
}

func decodeComStmtExecute(packet []byte) (ComStmtExecute, error) {
	// removed the print statement for cleanliness
	if len(packet) < 14 { // the minimal size of the packet without parameters should be 14, not 13
		return ComStmtExecute{}, fmt.Errorf("packet length less than 14 bytes")
	}
	var NullBitmapValue []byte

	stmtExecute := ComStmtExecute{}
	stmtExecute.StatementID = binary.LittleEndian.Uint32(packet[1:5])
	stmtExecute.Flags = packet[5]
	stmtExecute.IterationCount = binary.LittleEndian.Uint32(packet[6:10])

	// the next bytes are reserved for the Null-Bitmap, Parameter Bound Flag and Bound Parameters if they exist
	// if the length of the packet is greater than 14, then there are parameters
	if len(packet) > 14 {
		nullBitmapLength := int((stmtExecute.ParamCount + 7) / 8)

		NullBitmapValue = packet[10 : 10+nullBitmapLength]
		stmtExecute.NullBitmap = base64.StdEncoding.EncodeToString(NullBitmapValue)
		stmtExecute.ParamCount = binary.LittleEndian.Uint16(packet[10+nullBitmapLength:])

		// in case new parameters are bound, the new types and values are sent
		if packet[10+nullBitmapLength] == 1 {
			// read the types and values of the new parameters
			stmtExecute.Parameters = make([]BoundParameter, stmtExecute.ParamCount)
			for i := 0; i < int(stmtExecute.ParamCount); i++ {
				index := 10 + nullBitmapLength + 1 + 2*i
				if index+1 >= len(packet) {
					return ComStmtExecute{}, fmt.Errorf("packet length less than expected while reading parameters")
				}
				stmtExecute.Parameters[i].Type = packet[index]
				stmtExecute.Parameters[i].Unsigned = packet[index+1]
			}
		}
	}

	return stmtExecute, nil
}
