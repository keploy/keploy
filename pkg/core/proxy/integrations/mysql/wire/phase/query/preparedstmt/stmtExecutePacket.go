//go:build linux

package preparedstmt

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	intUtil "go.keploy.io/server/v2/pkg/core/proxy/integrations/util"

	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.uber.org/zap"
)

// COM_STMT_EXECUTE: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_stmt_execute.html

func DecodeStmtExecute(_ context.Context, _ *zap.Logger, data []byte, preparedStmts map[uint32]*mysql.StmtPrepareOkPacket) (*mysql.StmtExecutePacket, error) {
	if len(data) < 10 {
		return &mysql.StmtExecutePacket{}, fmt.Errorf("packet length too short for COM_STMT_EXECUTE")
	}

	pos := 0

	packet := &mysql.StmtExecutePacket{}

	// Read Status
	if pos+1 > len(data) {
		return nil, io.ErrUnexpectedEOF
	}
	//data[0] is COM_STMT_EXECUTE (0x17)
	packet.Status = data[pos]
	pos++

	// Read StatementID
	if pos+4 > len(data) {
		return nil, io.ErrUnexpectedEOF
	}
	packet.StatementID = binary.LittleEndian.Uint32(data[pos : pos+4])
	pos += 4

	numParams, ok := preparedStmts[packet.StatementID]
	if !ok && numParams == nil {
		return nil, fmt.Errorf("prepared statement with ID %d not found", packet.StatementID)
	}

	packet.ParameterCount = int(numParams.NumParams)

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

	if packet.ParameterCount <= 0 {
		return packet, nil
	}

	// Read Parameters only if there are any

	// Read NULL bitmap
	nullBitmapLength := (packet.ParameterCount + 7) / 8
	if pos+nullBitmapLength > len(data) {
		return nil, io.ErrUnexpectedEOF
	}
	packet.NullBitmap = data[pos : pos+nullBitmapLength]
	pos += int(nullBitmapLength)

	// Read NewParamsBindFlag
	if pos+1 > len(data) {
		return nil, io.ErrUnexpectedEOF
	}
	packet.NewParamsBindFlag = data[pos]
	pos++

	// Read Parameters if NewParamsBindFlag is set
	if packet.NewParamsBindFlag == 1 {
		packet.Parameters = make([]mysql.Parameter, packet.ParameterCount)
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

		// Process Parameter based on its type
		param := &packet.Parameters[i]

		// Handle length-encoded values (only for types that require variable-length data)
		switch mysql.FieldType(param.Type) {
		case mysql.FieldTypeString, mysql.FieldTypeVarString, mysql.FieldTypeVarChar, mysql.FieldTypeBLOB, mysql.FieldTypeTinyBLOB, mysql.FieldTypeMediumBLOB, mysql.FieldTypeLongBLOB, mysql.FieldTypeJSON:
			// Read the length of the parameter value
			length, _, n := utils.ReadLengthEncodedInteger(data[pos:])
			pos += n
			if pos+int(length) > len(data) {
				return nil, io.ErrUnexpectedEOF
			}

			if intUtil.IsASCII(string(data[pos : pos+int(length)])) {
				param.Value = string(data[pos : pos+int(length)])
			} else {
				param.Value = intUtil.EncodeBase64(data[pos : pos+int(length)])
			}
			pos += int(length)
		case mysql.FieldTypeLong:
			if len(data[pos:]) < 4 {
				return nil, fmt.Errorf("malformed FieldTypeLong value")
			}
			if param.Unsigned {
				param.Value = uint32(binary.LittleEndian.Uint32(data[pos : pos+4]))
			} else {
				param.Value = int32(binary.LittleEndian.Uint32(data[pos : pos+4]))
			}
			pos += 4

		case mysql.FieldTypeTiny:
			if len(data[pos:]) < 1 {
				return nil, fmt.Errorf("malformed FieldTypeTiny value")
			}
			if param.Unsigned {
				param.Value = uint8(data[pos])
			} else {
				param.Value = int8(data[pos])
			}
			pos += 1

		case mysql.FieldTypeShort, mysql.FieldTypeYear:
			if len(data[pos:]) < 2 {
				return nil, fmt.Errorf("malformed FieldTypeShort value")
			}
			if param.Unsigned {
				param.Value = uint16(binary.LittleEndian.Uint16(data[pos : pos+2]))
			} else {
				param.Value = int16(binary.LittleEndian.Uint16(data[pos : pos+2]))
			}
			pos += 2

		case mysql.FieldTypeLongLong:
			if len(data[pos:]) < 8 {
				return nil, fmt.Errorf("malformed FieldTypeLongLong value")
			}
			if param.Unsigned {
				param.Value = uint64(binary.LittleEndian.Uint64(data[pos : pos+8]))
			} else {
				param.Value = int64(binary.LittleEndian.Uint64(data[pos : pos+8]))
			}
			pos += 8

		case mysql.FieldTypeFloat:
			if len(data[pos:]) < 4 {
				return nil, fmt.Errorf("malformed FieldTypeFloat value")
			}
			param.Value = float32(binary.LittleEndian.Uint32(data[pos : pos+4]))
			pos += 4

		case mysql.FieldTypeDouble:
			if len(data[pos:]) < 8 {
				return nil, fmt.Errorf("malformed FieldTypeDouble value")
			}
			param.Value = float64(binary.LittleEndian.Uint64(data[pos : pos+8]))
			pos += 8

		case mysql.FieldTypeDate, mysql.FieldTypeNewDate:
			value, _, err := utils.ParseBinaryDate(data[pos:])
			if err != nil {
				return nil, err
			}
			param.Value = value
			pos += len(param.Value.(string)) // Assuming date parsing returns a string

		case mysql.FieldTypeTimestamp, mysql.FieldTypeDateTime:
			value, _, err := utils.ParseBinaryDateTime(data[pos:])
			if err != nil {
				return nil, err
			}
			param.Value = value
			pos += len(param.Value.(string)) // Assuming datetime parsing returns a string

		case mysql.FieldTypeTime:
			value, _, err := utils.ParseBinaryTime(data[pos:])
			if err != nil {
				return nil, err
			}
			param.Value = value
			pos += len(param.Value.(string)) // Assuming time parsing returns a string
		default:
			return nil, fmt.Errorf("unsupported parameter type: %d", param.Type)
		}
	}

	return packet, nil
}
