// Package query provides functions to decode MySQL command phase packets.
package query

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"strings"

	intUtil "go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// COM_QUERY: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_query.html

func DecodeQuery(_ context.Context, logger *zap.Logger, data []byte, clientCapabilities uint32) (*mysql.QueryPacket, error) {

	logger.Debug("Decoding query packet", zap.Int("data_length", len(data)), zap.Any("query buffer", string(data)))

	if len(data) < 2 {
		return nil, fmt.Errorf("query packet is empty")
	}
	packet := &mysql.QueryPacket{
		Command: data[0],
	}

	pos := 1 // Start reading after the command byte

	// Early return if no query attributes to process
	if clientCapabilities&mysql.CLIENT_QUERY_ATTRIBUTES == 0 {
		packet.Query = string(data[pos:])
		packet.Query = replaceTabsWithSpaces(packet.Query)
		logger.Debug("Decoded query packet without attributes", zap.String("query", packet.Query))
		return packet, nil
	}

	if pos >= len(data) {
		return nil, fmt.Errorf("malformed query packet: no data for parameter_count when CLIENT_QUERY_ATTRIBUTES is set")
	}

	// 1. Read parameter_count (lenenc-int).
	paramCount, isNull, n := utils.ReadLengthEncodedInteger(data[pos:])
	if isNull {
		return nil, fmt.Errorf("malformed query packet: got NULL for parameter_count")
	}
	pos += n

	packet.ParameterCount = int(paramCount)

	if pos >= len(data) {
		return nil, fmt.Errorf("malformed query packet: missing parameter_set_count")
	}

	// 2. Read parameter_set_count (lenenc-int).
	paramSetCount, isNull, n := utils.ReadLengthEncodedInteger(data[pos:])
	if isNull || (paramSetCount != 1) {
		return nil, fmt.Errorf("malformed query packet: expected parameter_set_count of 1, got %d", paramSetCount)
	}
	pos += n

	if paramCount > 0 {

		nullBitmapLength := (packet.ParameterCount + 7) / 8
		if pos+nullBitmapLength > len(data) {
			return nil, fmt.Errorf("malformed query packet: data too short for query attribute null_bitmap")
		}
		packet.NullBitmap = data[pos : pos+nullBitmapLength]
		pos += int(nullBitmapLength)

		if pos+1 > len(data) {
			return nil, fmt.Errorf("malformed query packet: data too short for new_params_bind_flag")
		}
		packet.NewParamsBindFlag = data[pos]
		pos++

		if packet.NewParamsBindFlag != 1 {
			return nil, fmt.Errorf("malformed query packet: new_params_bind_flag should be always 1 if parameter_count > 0")
		}

		packet.Parameters = make([]mysql.Parameter, packet.ParameterCount)

		for i := 0; i < packet.ParameterCount; i++ {
			if pos+2 > len(data) {
				return nil, fmt.Errorf("malformed query packet: data too short for parameter types")
			}
			packet.Parameters[i].Type = binary.LittleEndian.Uint16(data[pos : pos+2])
			packet.Parameters[i].Unsigned = (data[pos+1] & 0x80) != 0 // Check if the highest bit is set
			pos += 2
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
	}

	if pos >= len(data) {
		return nil, fmt.Errorf("malformed query packet: no data for query")
	}

	// Trim any trailing null bytes which can sometimes be appended by clients.
	packet.Query = string(data[pos:])
	packet.Query = replaceTabsWithSpaces(packet.Query)

	logger.Debug("Decoded query packet with attributes", zap.String("query", packet.Query))

	return packet, nil
}

// This is required to replace tabs with spaces in the query string, as yaml does not support tabs.
func replaceTabsWithSpaces(query string) string {
	return strings.ReplaceAll(query, "\t", "    ")
}
