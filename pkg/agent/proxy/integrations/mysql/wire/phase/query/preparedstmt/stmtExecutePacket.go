package preparedstmt

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	intUtil "go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"

	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// COM_STMT_EXECUTE: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_stmt_execute.html

func DecodeStmtExecute(_ context.Context, logger *zap.Logger, data []byte, preparedStmts map[uint32]*mysql.StmtPrepareOkPacket, clientCapabilities uint32) (*mysql.StmtExecutePacket, error) {
	if len(data) < 10 {
		return &mysql.StmtExecutePacket{}, fmt.Errorf("packet length too short for COM_STMT_EXECUTE")
	}

	pos := 0
	packet := &mysql.StmtExecutePacket{}

	logger.Debug("Decoding COM_STMT_EXECUTE packet", zap.Int("packet_length", len(data)), zap.Uint32("client_capabilities", clientCapabilities))

	// Read Status
	if pos+1 > len(data) {
		logger.Error("unexpected end of data while reading status", zap.Int("position", pos), zap.Int("data_length", len(data)))
		return nil, io.ErrUnexpectedEOF
	}
	// data[0] is COM_STMT_EXECUTE (0x17)
	packet.Status = data[pos]
	pos++

	// Read StatementID
	if pos+4 > len(data) {
		logger.Error("unexpected end of data while reading statement ID", zap.Int("position", pos), zap.Int("data_length", len(data)))
		return nil, io.ErrUnexpectedEOF
	}
	packet.StatementID = binary.LittleEndian.Uint32(data[pos : pos+4])
	pos += 4

	stmtPrepOk, ok := preparedStmts[packet.StatementID]
	if !ok && stmtPrepOk == nil {
		return nil, fmt.Errorf("prepared statement with ID %d not found", packet.StatementID)
	}

	logger.Debug("The stmtPrepOk packet", zap.Any("statement_id", packet.StatementID), zap.Any("stmtPrepOk", stmtPrepOk))

	// Read Flags
	if pos+1 > len(data) {
		logger.Error("unexpected end of data while reading flags", zap.Int("position", pos), zap.Int("data_length", len(data)))
		return nil, io.ErrUnexpectedEOF
	}
	packet.Flags = data[pos]
	pos++

	// Read IterationCount
	if pos+4 > len(data) {
		logger.Error("unexpected end of data while reading iteration count", zap.Int("position", pos), zap.Int("data_length", len(data)))
		return nil, io.ErrUnexpectedEOF
	}
	packet.IterationCount = binary.LittleEndian.Uint32(data[pos : pos+4])
	pos += 4

	// CLIENT_QUERY_ATTRIBUTES is negotiated and PARAMETER_COUNT_AVAILABLE flag is set:
	// mysql2 (Node.js) always takes this path against MySQL 8 — the wire layout
	// between iteration_count and the NULL bitmap is augmented with a
	// length-encoded parameter_count, and each declared parameter type is
	// followed by a length-encoded parameter_name string. See
	// https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_stmt_execute.html
	// (the CLIENT_QUERY_ATTRIBUTES branch).
	queryAttrsExtension := clientCapabilities&mysql.CLIENT_QUERY_ATTRIBUTES > 0 &&
		(packet.Flags&mysql.PARAMETER_COUNT_AVAILABLE > 0)

	if stmtPrepOk.NumParams > 0 || queryAttrsExtension {
		// Set the parameter count from the prepared statement by default.
		packet.ParameterCount = int(stmtPrepOk.NumParams)

		if queryAttrsExtension {
			// The wire carries a length-encoded parameter_count that overrides
			// stmtPrepOk.NumParams. mysql2 always sends this byte (typically 0
			// when num_params==0) and the decoder MUST advance past it,
			// otherwise it is mis-read as null_bitmap[0] downstream — that is
			// the root cause of the `value: /Q==` / `value: null` corruption
			// in mocks.yaml when recording mysql2 traffic.
			paramCount, isNull, n := utils.ReadLengthEncodedInteger(data[pos:])
			if isNull || n == 0 {
				logger.Error("unexpected end of data while reading length-encoded parameter_count", zap.Int("position", pos), zap.Int("data_length", len(data)))
				return nil, io.ErrUnexpectedEOF
			}
			pos += n
			packet.ParameterCount = int(paramCount)
		}

		if packet.ParameterCount <= 0 {
			return packet, nil
		}

		// Read Parameters only if there are any

		// Read NULL bitmap
		nullBitmapLength := (packet.ParameterCount + 7) / 8
		if pos+nullBitmapLength > len(data) {
			logger.Error("unexpected end of data while reading NULL bitmap", zap.Int("position", pos), zap.Int("data_length", len(data)), zap.Int("null_bitmap_length", nullBitmapLength))
			return nil, io.ErrUnexpectedEOF
		}
		packet.NullBitmap = data[pos : pos+nullBitmapLength]
		pos += int(nullBitmapLength)

		// Read NewParamsBindFlag
		if pos+1 > len(data) {
			logger.Error("unexpected end of data while reading NewParamsBindFlag", zap.Int("position", pos), zap.Int("data_length", len(data)))
			return nil, io.ErrUnexpectedEOF
		}
		packet.NewParamsBindFlag = data[pos]
		pos++

		// Initialize Parameters slice regardless of NewParamsBindFlag
		packet.Parameters = make([]mysql.Parameter, packet.ParameterCount)

		// Read Parameters if NewParamsBindFlag is set
		if packet.NewParamsBindFlag == 1 {
			for i := 0; i < packet.ParameterCount; i++ {
				if pos+2 > len(data) {
					logger.Error("unexpected end of data while reading parameter type", zap.Int("position", pos), zap.Int("data_length", len(data)), zap.Int("parameter_index", i))
					return nil, io.ErrUnexpectedEOF
				}
				packet.Parameters[i].Type = binary.LittleEndian.Uint16(data[pos : pos+2])
				packet.Parameters[i].Unsigned = (data[pos+1] & 0x80) != 0 // Check if the highest bit is set
				pos += 2

				if queryAttrsExtension {
					// Each parameter type is followed by a length-encoded
					// string parameter_name (usually empty when the binding
					// is positional, as in mysql2).
					nameLen, isNull, n := utils.ReadLengthEncodedInteger(data[pos:])
					if isNull || n == 0 {
						logger.Error("unexpected end of data while reading parameter_name length", zap.Int("position", pos), zap.Int("data_length", len(data)), zap.Int("parameter_index", i))
						return nil, io.ErrUnexpectedEOF
					}
					pos += n
					if pos+int(nameLen) > len(data) {
						logger.Error("unexpected end of data while reading parameter_name string", zap.Int("position", pos), zap.Int("data_length", len(data)), zap.Int("parameter_index", i), zap.Uint64("name_length", nameLen))
						return nil, io.ErrUnexpectedEOF
					}
					if nameLen > 0 {
						packet.Parameters[i].Name = string(data[pos : pos+int(nameLen)])
					}
					pos += int(nameLen)
				}
			}
		} else {
			// When NewParamsBindFlag is 0, we reuse the previous parameter types
			// For now, we'll set a default type for all parameters
			for i := 0; i < packet.ParameterCount; i++ {
				packet.Parameters[i].Type = uint16(mysql.FieldTypeString) // Default type
				packet.Parameters[i].Unsigned = false
			}
		}

		// Read Parameter Values
		for i := 0; i < packet.ParameterCount; i++ {
			// Check if this parameter is NULL according to the NULL bitmap
			byteIndex := i / 8
			bitIndex := i % 8
			if byteIndex < len(packet.NullBitmap) && (packet.NullBitmap[byteIndex]&(1<<bitIndex)) != 0 {
				// Parameter is NULL, set value to nil and continue
				packet.Parameters[i].Value = nil
				continue
			}

			if pos >= len(data) {
				logger.Error("unexpected end of data while reading parameter value", zap.Int("position", pos), zap.Int("data_length", len(data)), zap.Int("parameter_index", i))
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
					logger.Error("unexpected end of data while reading length-encoded parameter value", zap.Int("position", pos), zap.Int("data_length", len(data)), zap.Int("parameter_index", i), zap.Uint64("length", length))
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
				// IEEE-754 reinterpret, NOT a numeric cast — the wire
				// bytes encode the float's bit pattern. mysql2 binds
				// integer IDs (1, 2, 6, ...) as FieldTypeDouble; casting
				// uint64→float64 makes integer 6 read out as ~4.6e+18
				// (the uint64 of the IEEE-754 bits taken as a float).
				// The captured YAML then carries nonsense values and the
				// matcher can't tell IDs apart at COM_STMT_EXECUTE time.
				param.Value = math.Float64frombits(binary.LittleEndian.Uint64(data[pos : pos+8]))
				pos += 8

			// Fix: Added support for FieldTypeDate and FieldTypeNewDate.
			// Previously this would default to "unsupported parameter type".
			// Uses ParseBinaryDate to correctly decode the binary date format.
			case mysql.FieldTypeDate, mysql.FieldTypeNewDate:
				value, n, err := utils.ParseBinaryDate(data[pos:])
				if err != nil {
					return nil, err
				}
				param.Value = value
				pos += n

			// Fix: Added support for FieldTypeTimestamp and FieldTypeDateTime.
			// Uses ParseBinaryDateTime to correctly decode the binary datetime format.
			case mysql.FieldTypeTimestamp, mysql.FieldTypeDateTime:
				value, n, err := utils.ParseBinaryDateTime(data[pos:])
				if err != nil {
					return nil, err
				}
				param.Value = value
				pos += n

			// Fix: Added support for FieldTypeTime.
			// Uses ParseBinaryTime to correctly decode the binary time format.
			case mysql.FieldTypeTime:
				value, n, err := utils.ParseBinaryTime(data[pos:])
				if err != nil {
					return nil, err
				}
				param.Value = value
				pos += n
			default:
				return nil, fmt.Errorf("unsupported parameter type: %d", param.Type)
			}
		}
	}
	return packet, nil
}
