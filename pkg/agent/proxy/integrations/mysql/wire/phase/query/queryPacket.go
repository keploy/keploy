// Package query provides functions to decode MySQL command phase packets.
package query

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"strings"

	intUtil "go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// COM_QUERY: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_query.html

// DecodeQuery decodes a COM_QUERY. withLegacy also reads its query attributes
// as a keploy that did not read their names read them (QueryPacket's
// LegacyQuery and LegacyParameters), which only a replay compares.
func DecodeQuery(_ context.Context, logger *zap.Logger, data []byte, clientCapabilities uint32, withLegacy bool) (*mysql.QueryPacket, error) {

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

	if pos >= len(data) {
		return nil, fmt.Errorf("malformed query packet: missing parameter_set_count")
	}

	// 2. Read parameter_set_count (lenenc-int).
	paramSetCount, isNull, n := utils.ReadLengthEncodedInteger(data[pos:])
	if isNull || (paramSetCount != 1) {
		return nil, fmt.Errorf("malformed query packet: expected parameter_set_count of 1, got %d", paramSetCount)
	}
	pos += n

	// A count the packet cannot hold is malformed: every query attribute
	// takes at least its 2-byte type (new_params_bind_flag is always 1). Compared
	// unconverted: near MaxInt64, (count+7)/8 below overflowed negative and
	// the slice panicked, past MaxInt64 the count converted negative, and a
	// count of 8 per byte left allocated parameters far past the packet.
	if paramCount > uint64((len(data)-pos)/2) {
		return nil, fmt.Errorf("malformed query packet: parameter_count %d is more than the packet can hold", paramCount)
	}
	packet.ParameterCount = int(paramCount)

	if paramCount > 0 {
		params, bitmap, flag, end, err := readAttributes(data, pos, packet.ParameterCount, true)
		if err != nil {
			return nil, err
		}
		packet.NullBitmap, packet.NewParamsBindFlag, packet.Parameters = bitmap, flag, params
		// What a keploy that did not read the attributes' names read from the
		// same bytes (QueryPacket.LegacyQuery), for a recording it made.
		if withLegacy {
			if old, _, _, oldEnd, oldErr := readAttributes(data, pos, packet.ParameterCount, false); oldErr == nil && oldEnd < len(data) {
				packet.LegacyParameters = old
				packet.LegacyQuery = replaceTabsWithSpaces(string(data[oldEnd:]))
			}
		}
		pos = end
	}

	if pos >= len(data) {
		return nil, fmt.Errorf("malformed query packet: no data for query")
	}

	// Trim any trailing null bytes which can sometimes be appended by clients.
	packet.Query = string(data[pos:])
	packet.Query = replaceTabsWithSpaces(packet.Query)

	return packet, nil
}

// readAttributes reads the query attributes of a COM_QUERY from pos (the NULL
// bitmap onward), count of them, and returns them, the bitmap, the
// new_params_bind_flag and where the query begins.
//
// With protocol, it reads them as the protocol lays them out: each
// attribute's type, then its name (string<lenenc>), and then the values, a
// NULL one (in the bitmap) sending none. Without it, it reads them as keploy
// did before it read names: the types alone, then a value for every
// attribute, NULL or not. A valid packet with an attribute read so came out
// of step: the attribute's name taken for its value and its value glued to
// the head of the query (a per-request traceparent made every query differ);
// a NULL attribute or several failed or panicked.
func readAttributes(data []byte, pos, count int, protocol bool) (params []mysql.Parameter, bitmap []byte, flag byte, end int, err error) {
	nullBitmapLength := (count + 7) / 8
	if pos+nullBitmapLength > len(data) {
		return nil, nil, 0, 0, fmt.Errorf("malformed query packet: data too short for query attribute null_bitmap")
	}
	bitmap = data[pos : pos+nullBitmapLength]
	pos += nullBitmapLength

	if pos+1 > len(data) {
		return nil, nil, 0, 0, fmt.Errorf("malformed query packet: data too short for new_params_bind_flag")
	}
	flag = data[pos]
	pos++
	if flag != 1 {
		return nil, nil, 0, 0, fmt.Errorf("malformed query packet: new_params_bind_flag should be always 1 if parameter_count > 0")
	}

	params = make([]mysql.Parameter, count)
	for i := 0; i < count; i++ {
		if pos+2 > len(data) {
			return nil, nil, 0, 0, fmt.Errorf("malformed query packet: data too short for parameter types")
		}
		params[i].Type = binary.LittleEndian.Uint16(data[pos : pos+2])
		params[i].Unsigned = (data[pos+1] & 0x80) != 0 // Check if the highest bit is set
		pos += 2
		if !protocol {
			continue
		}
		nameLen, isNull, n := utils.ReadLengthEncodedInteger(data[pos:])
		if isNull {
			return nil, nil, 0, 0, fmt.Errorf("malformed query packet: parameter %d has no name length", i)
		}
		pos += n
		// Compared unconverted, as a value's length below.
		if nameLen > uint64(len(data)-pos) {
			return nil, nil, 0, 0, io.ErrUnexpectedEOF
		}
		params[i].Name = string(data[pos : pos+int(nameLen)])
		pos += int(nameLen)
	}

	// Read Parameter Values
	for i := 0; i < count; i++ {
		if protocol && bitmap[i/8]&(1<<(i%8)) != 0 {
			continue // NULL: no value sent
		}
		if pos >= len(data) {
			return nil, nil, 0, 0, io.ErrUnexpectedEOF
		}

		// Process Parameter based on its type
		param := &params[i]

		// Handle length-encoded values (only for types that require variable-length data)
		switch mysql.FieldType(param.Type) {
		case mysql.FieldTypeString, mysql.FieldTypeVarString, mysql.FieldTypeVarChar, mysql.FieldTypeBLOB, mysql.FieldTypeTinyBLOB, mysql.FieldTypeMediumBLOB, mysql.FieldTypeLongBLOB, mysql.FieldTypeJSON:
			// Read the length of the parameter value. Compared
			// unconverted: a length past MaxInt64 turned negative as an
			// int, passed the check, and the slice panicked. Read by the
			// protocol, a NULL marker, or a length cut short, is no
			// length (a NULL value is in the bitmap): it read as an
			// empty value.
			length, isNull, n := utils.ReadLengthEncodedInteger(data[pos:])
			if isNull && protocol {
				return nil, nil, 0, 0, fmt.Errorf("malformed query packet: parameter %d has no value length", i)
			}
			pos += n
			if length > uint64(len(data)-pos) {
				return nil, nil, 0, 0, io.ErrUnexpectedEOF
			}

			if intUtil.IsASCII(string(data[pos : pos+int(length)])) {
				param.Value = string(data[pos : pos+int(length)])
			} else {
				param.Value = intUtil.EncodeBase64(data[pos : pos+int(length)])
			}
			pos += int(length)
		case mysql.FieldTypeLong:
			if len(data[pos:]) < 4 {
				return nil, nil, 0, 0, fmt.Errorf("malformed FieldTypeLong value")
			}
			if param.Unsigned {
				param.Value = uint32(binary.LittleEndian.Uint32(data[pos : pos+4]))
			} else {
				param.Value = int32(binary.LittleEndian.Uint32(data[pos : pos+4]))
			}
			pos += 4

		case mysql.FieldTypeTiny:
			if len(data[pos:]) < 1 {
				return nil, nil, 0, 0, fmt.Errorf("malformed FieldTypeTiny value")
			}
			if param.Unsigned {
				param.Value = uint8(data[pos])
			} else {
				param.Value = int8(data[pos])
			}
			pos += 1

		case mysql.FieldTypeShort, mysql.FieldTypeYear:
			if len(data[pos:]) < 2 {
				return nil, nil, 0, 0, fmt.Errorf("malformed FieldTypeShort value")
			}
			if param.Unsigned {
				param.Value = uint16(binary.LittleEndian.Uint16(data[pos : pos+2]))
			} else {
				param.Value = int16(binary.LittleEndian.Uint16(data[pos : pos+2]))
			}
			pos += 2

		case mysql.FieldTypeLongLong:
			if len(data[pos:]) < 8 {
				return nil, nil, 0, 0, fmt.Errorf("malformed FieldTypeLongLong value")
			}
			if param.Unsigned {
				param.Value = uint64(binary.LittleEndian.Uint64(data[pos : pos+8]))
			} else {
				param.Value = int64(binary.LittleEndian.Uint64(data[pos : pos+8]))
			}
			pos += 8

		case mysql.FieldTypeFloat:
			if len(data[pos:]) < 4 {
				return nil, nil, 0, 0, fmt.Errorf("malformed FieldTypeFloat value")
			}
			// IEEE-754 reinterpret, NOT a numeric cast — the wire bytes
			// encode the float's bit pattern. Same defect as the one
			// fixed in stmtExecutePacket.go; query attributes carry it
			// on both FLOAT and DOUBLE.
			param.Value = math.Float32frombits(binary.LittleEndian.Uint32(data[pos : pos+4]))
			pos += 4

		case mysql.FieldTypeDouble:
			if len(data[pos:]) < 8 {
				return nil, nil, 0, 0, fmt.Errorf("malformed FieldTypeDouble value")
			}
			param.Value = math.Float64frombits(binary.LittleEndian.Uint64(data[pos : pos+8]))
			pos += 8

		// Support DATE types in query attributes.
		// These are encoded in binary format when sent as query parameters/attributes.
		case mysql.FieldTypeDate, mysql.FieldTypeNewDate:
			value, n, err := utils.ParseBinaryDate(data[pos:])
			if err != nil {
				return nil, nil, 0, 0, err
			}
			param.Value = value
			pos += n

		//Support TIMESTAMP and DATETIME types in query attributes.
		case mysql.FieldTypeTimestamp, mysql.FieldTypeDateTime:
			value, n, err := utils.ParseBinaryDateTime(data[pos:])
			if err != nil {
				return nil, nil, 0, 0, err
			}
			param.Value = value
			pos += n

		// Fix: Support TIME types in query attributes.
		case mysql.FieldTypeTime:
			value, n, err := utils.ParseBinaryTime(data[pos:])
			if err != nil {
				return nil, nil, 0, 0, err
			}
			param.Value = value
			pos += n
		default:
			return nil, nil, 0, 0, fmt.Errorf("unsupported parameter type: %d", param.Type)
		}
	}
	return params, bitmap, flag, pos, nil
}

// This is required to replace tabs with spaces in the query string, as yaml does not support tabs.
func replaceTabsWithSpaces(query string) string {
	return strings.ReplaceAll(query, "\t", "    ")
}
