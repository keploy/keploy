//go:build linux

// Package rowscols provides encoding and decoding of MySQL row & column packets.
package rowscols

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.uber.org/zap"
)

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_binary_resultset.html#sect_protocol_binary_resultset_row

func DecodeBinaryRow(_ context.Context, _ *zap.Logger, data []byte, columns []*mysql.ColumnDefinition41) (*mysql.BinaryRow, int, error) {

	offset := 0
	row := &mysql.BinaryRow{
		Header: mysql.Header{
			PayloadLength: utils.ReadUint24(data[offset : offset+3]),
			SequenceID:    data[offset+3],
		},
	}
	offset += 4

	if data[offset] != 0x00 {
		return nil, offset, errors.New("malformed binary row packet")
	}
	row.OkAfterRow = true
	offset++

	nullBitmapLen := (len(columns) + 7 + 2) / 8
	nullBitmap := data[offset : offset+nullBitmapLen]

	offset += nullBitmapLen

	for i, col := range columns {
		if isNull(nullBitmap, i) { // This Null doesn't progress the offset
			row.Values = append(row.Values, mysql.ColumnEntry{
				Type:  mysql.FieldType(col.Type),
				Name:  col.Name,
				Value: nil,
			})
			continue
		}

		value, n, err := readBinaryValue(data[offset:], col)
		if err != nil {
			return nil, offset, err
		}

		row.Values = append(row.Values, mysql.ColumnEntry{
			Type:  mysql.FieldType(col.Type),
			Name:  col.Name,
			Value: value,
		})
		offset += n
	}
	return row, offset, nil
}

func isNull(nullBitmap []byte, index int) bool {
	bytePos := (index + 2) / 8
	bitPos := (index + 2) % 8
	return nullBitmap[bytePos]&(1<<bitPos) != 0
}

func readBinaryValue(data []byte, col *mysql.ColumnDefinition41) (interface{}, int, error) {
	isUnsigned := col.Flags&mysql.UNSIGNED_FLAG != 0

	switch models.FieldType(col.Type) {
	case models.FieldTypeLong:
		if len(data) < 4 {
			return nil, 0, errors.New("malformed FieldTypeLong value")
		}
		if isUnsigned {
			return uint32(binary.LittleEndian.Uint32(data[:4])), 4, nil
		}
		return int32(binary.LittleEndian.Uint32(data[:4])), 4, nil

	case models.FieldTypeString, models.FieldTypeVarString, models.FieldTypeVarChar, models.FieldTypeBLOB, models.FieldTypeTinyBLOB, models.FieldTypeMediumBLOB, models.FieldTypeLongBLOB, models.FieldTypeJSON:
		value, _, n, err := utils.ReadLengthEncodedString(data)
		return string(value), n, err

	case models.FieldTypeTiny:
		if isUnsigned {
			return uint8(data[0]), 1, nil
		}
		return int8(data[0]), 1, nil

	case models.FieldTypeShort, models.FieldTypeYear:
		if len(data) < 2 {
			return nil, 0, errors.New("malformed FieldTypeShort value")
		}
		if isUnsigned {
			return uint16(binary.LittleEndian.Uint16(data[:2])), 2, nil
		}
		return int16(binary.LittleEndian.Uint16(data[:2])), 2, nil

	case models.FieldTypeLongLong:
		if len(data) < 8 {
			return nil, 0, errors.New("malformed FieldTypeLongLong value")
		}
		if isUnsigned {
			return uint64(binary.LittleEndian.Uint64(data[:8])), 8, nil
		}
		return int64(binary.LittleEndian.Uint64(data[:8])), 8, nil

	case models.FieldTypeFloat:
		if len(data) < 4 {
			return nil, 0, errors.New("malformed FieldTypeFloat value")
		}
		return float32(binary.LittleEndian.Uint32(data[:4])), 4, nil

	case models.FieldTypeDouble:
		if len(data) < 8 {
			return nil, 0, errors.New("malformed FieldTypeDouble value")
		}
		return float64(binary.LittleEndian.Uint64(data[:8])), 8, nil

	case models.FieldTypeDate, models.FieldTypeNewDate:
		value, n, err := parseBinaryDate(data)
		return value, n, err

	case models.FieldTypeTimestamp, models.FieldTypeDateTime:
		value, n, err := parseBinaryDateTime(data)
		return value, n, err

	case models.FieldTypeTime:
		value, n, err := parseBinaryTime(data)
		return value, n, err

	default:
		return nil, 0, fmt.Errorf("unsupported column type: %v", col.Type)
	}
}

func parseBinaryDate(b []byte) (interface{}, int, error) {
	if len(b) == 0 {
		return nil, 0, nil
	}
	length := b[0]
	if length == 0 {
		return nil, 1, nil
	}
	year := binary.LittleEndian.Uint16(b[1:3])
	month := b[3]
	day := b[4]
	return fmt.Sprintf("%04d-%02d-%02d", year, month, day), int(length) + 1, nil
}

func parseBinaryDateTime(b []byte) (interface{}, int, error) {
	if len(b) == 0 {
		return nil, 0, nil
	}
	length := b[0]
	if length == 0 {
		return nil, 1, nil
	}
	year := binary.LittleEndian.Uint16(b[1:3])
	month := b[3]
	day := b[4]
	hour := b[5]
	minute := b[6]
	second := b[7]
	if length > 7 {
		microsecond := binary.LittleEndian.Uint32(b[8:12])
		return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d.%06d", year, month, day, hour, minute, second, microsecond), int(length) + 1, nil
	}
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d", year, month, day, hour, minute, second), int(length) + 1, nil
}

func parseBinaryTime(b []byte) (interface{}, int, error) {
	if len(b) == 0 {
		return nil, 0, nil
	}
	length := b[0]
	if length == 0 {
		return nil, 1, nil
	}
	isNegative := b[1] == 1
	days := binary.LittleEndian.Uint32(b[2:6])
	hours := b[6]
	minutes := b[7]
	seconds := b[8]
	var microseconds uint32
	if length > 8 {
		microseconds = binary.LittleEndian.Uint32(b[9:13])
	}
	timeString := fmt.Sprintf("%d %02d:%02d:%02d.%06d", days, hours, minutes, seconds, microseconds)
	if isNegative {
		timeString = "-" + timeString
	}
	return timeString, int(length) + 1, nil
}
