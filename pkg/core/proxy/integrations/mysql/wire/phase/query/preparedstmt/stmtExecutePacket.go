//go:build linux

package preparedstmt

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"strings"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	intUtil "go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	"go.keploy.io/server/v2/pkg/models"

	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.uber.org/zap"
)

// COM_STMT_EXECUTE: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_stmt_execute.html
func DecodeStmtExecute(
	ctx context.Context,
	logger *zap.Logger,
	data []byte,
	mode models.Mode,
	recordPrepStmtMap map[uint32]*mysql.StmtPrepareOkPacket, // Record-mode map: stmtID -> packet
	MockPrepStmtMap map[string]map[string]*mysql.StmtPrepareOkPacket, // Runtime map: connID -> (normalizedQuery -> packet)
	runtimeStmtIDToQuery map[uint32]string, // runtime stmtID -> query
	clientCapabilities uint32,
) (*mysql.StmtExecutePacket, error) {
	// keep the original sanity check
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

	// Resolve appropriate StmtPrepareOkPacket based on mode and runtime maps.
	var stmtPrepOk *mysql.StmtPrepareOkPacket

	// Obtain connID from ctx if present (used for runtime map which is conn-scoped)
	var connID string
	if rawConn := ctx.Value(models.ClientConnectionIDKey); rawConn != nil {
		if s, ok := rawConn.(string); ok {
			connID = s
		}
	}

	if mode == models.MODE_RECORD {
		logger.Debug("Decoding in RECORD mode", zap.Uint32("statement_id", packet.StatementID), zap.String("conn_id", connID))
		// RECORD mode - use stmtID -> StmtPrepareOkPacket map
		if recordPrepStmtMap != nil {
			if sp, ok := recordPrepStmtMap[packet.StatementID]; ok && sp != nil {
				stmtPrepOk = sp
			}
		}
	} else {
		logger.Debug("Decoding in TEST mode", zap.Uint32("runtime statement_id", packet.StatementID), zap.String("conn_id", connID))
		// TEST mode - do not rely on the raw stmtID equality.
		// Use runtimeStmtIDToQuery to obtain the runtime query for this stmt id,
		// normalize it and lookup MockPrepStmtMap[connID][normalizedQuery].
		var runtimeQuery string
		if runtimeStmtIDToQuery != nil {
			if q, ok := runtimeStmtIDToQuery[packet.StatementID]; ok {
				runtimeQuery = strings.TrimSpace(q)
			}
		}
		if runtimeQuery != "" && connID != "" && MockPrepStmtMap != nil {
			nq := strings.ToLower(runtimeQuery) // normalization -New
			if inner, ok := MockPrepStmtMap[connID]; ok && inner != nil {
				if sp, found := inner[nq]; found && sp != nil {
					stmtPrepOk = sp //New - found recorded/runtime PREP packet for this conn and query
				}
			}
		}
	}

	var runtimeQuery string
	if runtimeStmtIDToQuery != nil {
		runtimeQuery = runtimeStmtIDToQuery[packet.StatementID]
	}

	if stmtPrepOk == nil {

		return nil, fmt.Errorf("prepared statement metadata not found for statement id %d (mode=%v, connID=%q). runtimeQuery=%q", packet.StatementID, mode, connID, runtimeQuery) //New
	}

	logger.Debug("The stmtPrepOk packet", zap.Any("stmtPrepOk", stmtPrepOk), zap.String("runtimeQuery", runtimeQuery))

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

	if stmtPrepOk.NumParams > 0 || (clientCapabilities&mysql.CLIENT_QUERY_ATTRIBUTES > 0 && (packet.Flags&mysql.PARAMETER_COUNT_AVAILABLE > 0)) {
		// Set the parameter count from the prepared statement
		packet.ParameterCount = int(stmtPrepOk.NumParams)

		if clientCapabilities&mysql.CLIENT_QUERY_ATTRIBUTES > 0 && (packet.Flags&mysql.PARAMETER_COUNT_AVAILABLE > 0) {
			// If query attributes are supported and parameter count is available in the packet,
			// we could potentially override it here, but for now we use the prepared statement count
			packet.ParameterCount = int(stmtPrepOk.NumParams)
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
	}
	return packet, nil
}
