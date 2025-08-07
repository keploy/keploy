// in pkg/core/proxy/integrations/mysql/utils/param_parser.go
//go:build linux

package utils

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	intUtil "go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	"go.keploy.io/server/v2/pkg/models/mysql"
)

// ParseParameterValue reads a single parameter's value from the data slice based on its type.
// It returns the parsed value, the number of bytes read, and an error if any.
func ParseParameterValue(data []byte, paramType mysql.FieldType, isUnsigned bool) (value interface{}, bytesRead int, err error) {
	pos := 0
	switch paramType {
	case mysql.FieldTypeString, mysql.FieldTypeVarString, mysql.FieldTypeVarChar, mysql.FieldTypeBLOB, mysql.FieldTypeTinyBLOB, mysql.FieldTypeMediumBLOB, mysql.FieldTypeLongBLOB, mysql.FieldTypeJSON, mysql.FieldTypeDecimal, mysql.FieldTypeNewDecimal:
		val, _, n := ReadLengthEncodedInteger(data[pos:])
		pos += n
		if pos+int(val) > len(data) {
			return nil, 0, io.ErrUnexpectedEOF
		}
		bytesRead = pos + int(val)
		// Check if the string is ASCII before returning, otherwise encode to base64
		if intUtil.IsASCII(string(data[pos:bytesRead])) {
			return string(data[pos:bytesRead]), bytesRead, nil
		}
		return intUtil.EncodeBase64(data[pos:bytesRead]), bytesRead, nil

	case mysql.FieldTypeLong:
		if len(data[pos:]) < 4 {
			return nil, 0, fmt.Errorf("malformed FieldTypeLong value")
		}
		bytesRead = 4
		if isUnsigned {
			return binary.LittleEndian.Uint32(data[pos : pos+bytesRead]), bytesRead, nil
		}
		return int32(binary.LittleEndian.Uint32(data[pos : pos+bytesRead])), bytesRead, nil

	case mysql.FieldTypeTiny:
		if len(data[pos:]) < 1 {
			return nil, 0, fmt.Errorf("malformed FieldTypeTiny value")
		}
		bytesRead = 1
		if isUnsigned {
			return data[pos], bytesRead, nil
		}
		return int8(data[pos]), bytesRead, nil

	case mysql.FieldTypeShort, mysql.FieldTypeYear:
		if len(data[pos:]) < 2 {
			return nil, 0, fmt.Errorf("malformed FieldTypeShort value")
		}
		bytesRead = 2
		if isUnsigned {
			return binary.LittleEndian.Uint16(data[pos : pos+bytesRead]), bytesRead, nil
		}
		return int16(binary.LittleEndian.Uint16(data[pos : pos+bytesRead])), bytesRead, nil

	case mysql.FieldTypeLongLong:
		if len(data[pos:]) < 8 {
			return nil, 0, fmt.Errorf("malformed FieldTypeLongLong value")
		}
		bytesRead = 8
		if isUnsigned {
			return binary.LittleEndian.Uint64(data[pos : pos+bytesRead]), bytesRead, nil
		}
		return int64(binary.LittleEndian.Uint64(data[pos : pos+bytesRead])), bytesRead, nil

	case mysql.FieldTypeFloat:
		if len(data[pos:]) < 4 {
			return nil, 0, fmt.Errorf("malformed FieldTypeFloat value")
		}
		bytesRead = 4
		return math.Float32frombits(binary.LittleEndian.Uint32(data[pos : pos+bytesRead])), bytesRead, nil

	case mysql.FieldTypeDouble:
		if len(data[pos:]) < 8 {
			return nil, 0, fmt.Errorf("malformed FieldTypeDouble value")
		}
		bytesRead = 8
		return math.Float64frombits(binary.LittleEndian.Uint64(data[pos : pos+bytesRead])), bytesRead, nil

	case mysql.FieldTypeDate, mysql.FieldTypeNewDate:
		val, n, err := ParseBinaryDate(data[pos:])
		return val, n, err

	case mysql.FieldTypeTimestamp, mysql.FieldTypeDateTime:
		val, n, err := ParseBinaryDateTime(data[pos:])
		return val, n, err

	case mysql.FieldTypeTime:
		val, n, err := ParseBinaryTime(data[pos:])
		return val, n, err

	default:
		return nil, 0, fmt.Errorf("unsupported parameter type: %d", paramType)
	}
}
