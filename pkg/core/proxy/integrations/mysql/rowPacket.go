package mysql

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"go.keploy.io/server/v2/pkg/models"
)

func parseTextRow(b []byte, columns []*models.ColumnDefinition) (*models.Row, int, error) {
	offset := 0
	row := &models.Row{
		Header: models.PacketHeader{
			PacketLength:     uint8(readUint24(b[offset : offset+3])),
			PacketSequenceID: b[offset+3],
		},
	}
	offset += 4

	// Check if there is any ok packet after row header
	if len(b) > offset && b[offset] == 0x00 {
		row.OkAfterRow = true
		offset++
	}

	if len(b) > offset && b[offset] == 0x00 {
		row.RowNullBuffer = b[offset : offset+1]
		offset++
	}

	println("Length of column: ", len(columns))
	for i, col := range columns {
		fmt.Printf("b[offset]: %d\n", b[offset])
		dataLength := b[offset]
		println("DataLength for column number(", i, "):", dataLength)
		println("Offset for column number(", i, "):", offset)
		if dataLength == 0xfb {
			println("ColumnType:", col.ColumnType)
			fmt.Printf("Reading NULL value\n")
			row.Columns = append(row.Columns, models.RowCol{
				Type:  models.FieldType(col.ColumnType),
				Name:  col.Name,
				Value: nil,
			})
			offset++
			continue
		}
		println("ColumnType:", col.ColumnType)
		switch models.FieldType(col.ColumnType) {
		case models.FieldTypeDate, models.FieldTypeTime, models.FieldTypeDateTime, models.FieldTypeTimestamp:
			println("Column Type is: ", models.FieldType(col.ColumnType))
			data := b[offset+1:]
			println("length of data: ", len(data))
			println("dataLength: ", dataLength)

			if dataLength < 4 || len(data) < int(dataLength) {
				return nil, 0, fmt.Errorf("invalid timestamp data length")
			}

			dateStr := string(data[:dataLength])
			layout := "2006-01-02 15:04:05"
			t, err := time.Parse(layout, dateStr)
			if err != nil {
				return nil, 0, fmt.Errorf("failed to parse the time string")
			}
			year, month, day := t.Date()
			hour, minute, second := t.Clock()
			row.Columns = append(row.Columns, models.RowCol{
				Type:  models.FieldType(col.ColumnType),
				Name:  col.Name,
				Value: fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d", year, int(month), day, hour, minute, second),
			})
			offset += int(dataLength) + 1
			fmt.Printf("Reading timestamp value: %s\n", row.Columns[len(row.Columns)-1].Value)
		default:
			println("Reading length encoded string")
			value, _, n, err := readLengthEncodedString(b[offset:])
			if err != nil {
				return nil, offset, err
			}
			println("Reading value: ", value)
			row.Columns = append(row.Columns, models.RowCol{
				Type:  models.FieldType(col.ColumnType),
				Name:  col.Name,
				Value: value,
			})
			offset += n
		}
	}
	return row, offset, nil
}

func parseBinaryRow(b []byte, columns []*models.ColumnDefinition) (*models.Row, int, error) {
	offset := 0
	row := &models.Row{
		Header: models.PacketHeader{
			PacketLength:     uint8(readUint24(b[offset : offset+3])),
			PacketSequenceID: b[offset+3],
		},
	}
	offset += 4

	if b[offset] != 0x00 {
		return nil, offset, errors.New("malformed binary row packet")
	}
	row.OkAfterRow = true
	offset++

	println("Length of column: ", len(columns))
	nullBitmapLen := (len(columns) + 7 + 2) / 8
	println("NullBitmapLen: ", nullBitmapLen)
	nullBitmap := b[offset : offset+nullBitmapLen]
	offset += nullBitmapLen

	for i, col := range columns {
		if isNull(nullBitmap, i) {
			row.Columns = append(row.Columns, models.RowCol{
				Type:  models.FieldType(col.ColumnType),
				Name:  col.Name,
				Value: nil,
			})
			continue
		}
		value, n, err := readBinaryValue(b[offset:], col)
		if err != nil {
			return nil, offset, err
		}
		row.Columns = append(row.Columns, models.RowCol{
			Type:  models.FieldType(col.ColumnType),
			Name:  col.Name,
			Value: value,
		})
		fmt.Printf("[BinaryRow] Column(%d) -> Type:%2x || Name:%s || Value:%v\n", i, col.ColumnType, col.Name, value)
		offset += n
	}
	return row, offset, nil
}

func isNull(nullBitmap []byte, index int) bool {
	bytePos := (index + 2) / 8
	bitPos := (index + 2) % 8
	return nullBitmap[bytePos]&(1<<bitPos) != 0
}

func readBinaryValue(b []byte, col *models.ColumnDefinition) (interface{}, int, error) {
	isUnsigned := col.Flags&models.UNSIGNED_FLAG != 0

	switch models.FieldType(col.ColumnType) {
	case models.FieldTypeLong:
		if len(b) < 4 {
			return nil, 0, errors.New("malformed FieldTypeLong value")
		}
		if isUnsigned {
			return uint32(binary.LittleEndian.Uint32(b[:4])), 4, nil
		}
		return int32(binary.LittleEndian.Uint32(b[:4])), 4, nil

	case models.FieldTypeString, models.FieldTypeVarString, models.FieldTypeVarChar, models.FieldTypeBLOB, models.FieldTypeTinyBLOB, models.FieldTypeMediumBLOB, models.FieldTypeLongBLOB, models.FieldTypeJSON:
		value, _, n, err := readLengthEncodedString(b)
		return string(value), n, err

	case models.FieldTypeTiny:
		if isUnsigned {
			return uint8(b[0]), 1, nil
		}
		return int8(b[0]), 1, nil

	case models.FieldTypeShort, models.FieldTypeYear:
		if len(b) < 2 {
			return nil, 0, errors.New("malformed FieldTypeShort value")
		}
		if isUnsigned {
			return uint16(binary.LittleEndian.Uint16(b[:2])), 2, nil
		}
		return int16(binary.LittleEndian.Uint16(b[:2])), 2, nil

	case models.FieldTypeLongLong:
		if len(b) < 8 {
			return nil, 0, errors.New("malformed FieldTypeLongLong value")
		}
		if isUnsigned {
			return uint64(binary.LittleEndian.Uint64(b[:8])), 8, nil
		}
		return int64(binary.LittleEndian.Uint64(b[:8])), 8, nil

	case models.FieldTypeFloat:
		if len(b) < 4 {
			return nil, 0, errors.New("malformed FieldTypeFloat value")
		}
		return float32(binary.LittleEndian.Uint32(b[:4])), 4, nil

	case models.FieldTypeDouble:
		if len(b) < 8 {
			return nil, 0, errors.New("malformed FieldTypeDouble value")
		}
		return float64(binary.LittleEndian.Uint64(b[:8])), 8, nil

	case models.FieldTypeDate, models.FieldTypeNewDate:
		value, n, err := parseBinaryDate(b)
		return value, n, err

	case models.FieldTypeTimestamp, models.FieldTypeDateTime:
		value, n, err := parseBinaryDateTime(b)
		return value, n, err

	case models.FieldTypeTime:
		value, n, err := parseBinaryTime(b)
		return value, n, err

	default:
		return nil, 0, fmt.Errorf("unsupported column type: %v", col.ColumnType)
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
