//go:build linux

package mysql

import (
	"encoding/binary"
	"fmt"
	"io"

	"go.keploy.io/server/v2/pkg/models"
)

func decodeComStmtExecute(data []byte, preparedStmts map[uint32]*models.MySQLStmtPrepareOk) (*models.MySQLComStmtExecute, error) {
	if len(data) < 10 {
		return &models.MySQLComStmtExecute{}, fmt.Errorf("packet length too short for COM_STMT_EXECUTE")
	}

	var pos int

	packet := &models.MySQLComStmtExecute{}

	// Read Status
	if pos+1 > len(data) {
		return nil, io.ErrUnexpectedEOF
	}
	//data[0] is COM_STMT_EXECUTE (0x17)

	pos++

	// Read StatementID
	if pos+4 > len(data) {
		return nil, io.ErrUnexpectedEOF
	}
	packet.StatementID = binary.LittleEndian.Uint32(data[pos : pos+4])
	pos += 4

	packet.ParameterCount = int(preparedStmts[packet.StatementID].NumParams)

	// Read Flags
	if pos+1 > len(data) {
		return nil, io.ErrUnexpectedEOF
	}
	packet.Flags = data[pos]
	pos++

	// Read IterationCount
	if pos+4 > len(data) {
		return nil, io.ErrUnexpectedEOF
	}
	packet.IterationCount = binary.LittleEndian.Uint32(data[pos : pos+4])
	pos += 4

	if packet.ParameterCount > 0 {
		// Read NULL bitmap
		nullBitmapLength := (packet.ParameterCount + 7) / 8
		if pos+nullBitmapLength > len(data) {
			return nil, io.ErrUnexpectedEOF
		}
		packet.NullBitmap = data[pos : pos+int(nullBitmapLength)]
		pos += int(nullBitmapLength)

		// Read NewParamsBindFlag
		if pos+1 > len(data) {
			return nil, io.ErrUnexpectedEOF
		}
		packet.NewParamsBindFlag = data[pos]
		pos++

		// Read Parameters if NewParamsBindFlag is set
		if packet.NewParamsBindFlag == 1 {
			packet.Parameters = make([]models.Parameter, packet.ParameterCount)
			for i := 0; i < packet.ParameterCount; i++ {
				if pos+2 > len(data) {
					return nil, io.ErrUnexpectedEOF
				}
				packet.Parameters[i].Type = binary.LittleEndian.Uint16(data[pos : pos+2])
				packet.Parameters[i].Unsigned = (data[pos+1] & 0x80) != 0 // Check if the highest bit is set
				pos += 2
			}
		}

		// Read Parameter Values
		for i := 0; i < packet.ParameterCount; i++ {
			if pos >= len(data) {
				return nil, io.ErrUnexpectedEOF
			}
			length, _, n := readLengthEncodedInteger(data[pos:])
			pos += n
			if pos+int(length) > len(data) {
				return nil, io.ErrUnexpectedEOF
			}
			packet.Parameters[i].Value = data[pos : pos+int(length)]
			pos += int(length)
		}
	}

	return packet, nil
}
