//go:build linux

// Package rowscols provides encoding and decoding of MySQL row & column packets.
package rowscols

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"go.keploy.io/server/v2/pkg/agent/proxy/integrations/mysql/utils"
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
	row.RowNullBuffer = nullBitmap

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

		res, n, err := readBinaryValue(data[offset:], col)
		if err != nil {
			return nil, offset, err
		}

		row.Values = append(row.Values, mysql.ColumnEntry{
			Type:     mysql.FieldType(col.Type),
			Name:     col.Name,
			Value:    res.value,
			Unsigned: res.isUnsigned,
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

type binaryValueResult struct {
	value      interface{}
	isUnsigned bool
}

func readBinaryValue(data []byte, col *mysql.ColumnDefinition41) (*binaryValueResult, int, error) {
	isUnsigned := col.Flags&mysql.UNSIGNED_FLAG != 0
	res := &binaryValueResult{
		isUnsigned: isUnsigned,
	}

	switch mysql.FieldType(col.Type) {
	case mysql.FieldTypeLong:
		if len(data) < 4 {
			return nil, 0, errors.New("malformed FieldTypeLong value")
		}
		if isUnsigned {
			res.value = uint32(binary.LittleEndian.Uint32(data[:4]))
			return res, 4, nil
		}
		res.value = int32(binary.LittleEndian.Uint32(data[:4]))
		return res, 4, nil

	case mysql.FieldTypeString,
		mysql.FieldTypeVarString,
		mysql.FieldTypeVarChar,
		mysql.FieldTypeBLOB, mysql.FieldTypeTinyBLOB, mysql.FieldTypeMediumBLOB, mysql.FieldTypeLongBLOB,
		mysql.FieldTypeJSON,
		mysql.FieldTypeNewDecimal, // NEWDECIMAL (0xF6 / 246) is sent as a length-encoded string in binary rows
		mysql.FieldTypeDecimal:    // legacy DECIMAL (0) — treat same as NEWDECIMAL
		value, _, n, err := utils.ReadLengthEncodedString(data)
		res.value = string(value)
		return res, n, err

	case mysql.FieldTypeTiny:
		if isUnsigned {
			res.value = uint8(data[0])
			return res, 1, nil
		}
		res.value = int8(data[0])
		return res, 1, nil

	case mysql.FieldTypeShort, mysql.FieldTypeYear:
		if len(data) < 2 {
			return nil, 0, errors.New("malformed FieldTypeShort value")
		}
		if isUnsigned {
			res.value = uint16(binary.LittleEndian.Uint16(data[:2]))
			return res, 2, nil
		}
		res.value = int16(binary.LittleEndian.Uint16(data[:2]))
		return res, 2, nil

	case mysql.FieldTypeLongLong:
		if len(data) < 8 {
			return nil, 0, errors.New("malformed FieldTypeLongLong value")
		}
		if isUnsigned {
			res.value = uint64(binary.LittleEndian.Uint64(data[:8]))
			return res, 8, nil
		}
		res.value = int64(binary.LittleEndian.Uint64(data[:8]))
		return res, 8, nil

	case mysql.FieldTypeFloat:
		if len(data) < 4 {
			return nil, 0, errors.New("malformed FieldTypeFloat value")
		}
		res.value = float32(binary.LittleEndian.Uint32(data[:4]))
		return res, 4, nil

	case mysql.FieldTypeDouble:
		if len(data) < 8 {
			return nil, 0, errors.New("malformed FieldTypeDouble value")
		}
		res.value = float64(binary.LittleEndian.Uint64(data[:8]))
		return res, 8, nil

	case mysql.FieldTypeDate, mysql.FieldTypeNewDate:
		value, n, err := utils.ParseBinaryDate(data)
		res.value = value
		return res, n, err

	case mysql.FieldTypeTimestamp, mysql.FieldTypeDateTime:
		value, n, err := utils.ParseBinaryDateTime(data)
		res.value = value
		return res, n, err

	case mysql.FieldTypeTime:
		value, n, err := utils.ParseBinaryTime(data)
		res.value = value
		return res, n, err

	default:
		return nil, 0, fmt.Errorf("unsupported column type: %v", col.Type)
	}
}

func EncodeBinaryRow(_ context.Context, _ *zap.Logger, row *mysql.BinaryRow, columns []*mysql.ColumnDefinition41) ([]byte, error) {
	body := new(bytes.Buffer)

	// OK byte
	if err := body.WriteByte(0x00); err != nil {
		return nil, fmt.Errorf("failed to write OK byte: %w", err)
	}
	// NULL bitmap
	if _, err := body.Write(row.RowNullBuffer); err != nil {
		return nil, fmt.Errorf("failed to write NULL bitmap: %w", err)
	}

	// Values
	for i, col := range columns {
		if isNull(row.RowNullBuffer, i) {
			continue
		}

		ce := row.Values[i]

		switch ce.Type {
		case mysql.FieldTypeLong:
			if ce.Unsigned {
				v := uint32(ce.Value.(int))
				if err := binary.Write(body, binary.LittleEndian, v); err != nil {
					return nil, err
				}
			} else {
				v := int32(ce.Value.(int))
				if err := binary.Write(body, binary.LittleEndian, v); err != nil {
					return nil, err
				}
			}

		case mysql.FieldTypeString, mysql.FieldTypeVarString, mysql.FieldTypeVarChar,
			mysql.FieldTypeNewDecimal, mysql.FieldTypeDecimal,
			mysql.FieldTypeJSON:
			s, ok := ce.Value.(string)
			if !ok {
				return nil, fmt.Errorf("string-like field %q not a string", col.Name)
			}
			if err := utils.WriteLengthEncodedString(body, s); err != nil {
				return nil, err
			}

		case mysql.FieldTypeBLOB, mysql.FieldTypeTinyBLOB, mysql.FieldTypeMediumBLOB, mysql.FieldTypeLongBLOB:
			switch v := ce.Value.(type) {
			case []byte:
				if err := writeLenEncBytes(body, v); err != nil {
					return nil, err
				}
			case string:
				// Try base64 (used by YAML !!binary). If that fails, write raw bytes of the string.
				if decoded, err := base64.StdEncoding.DecodeString(v); err == nil {
					if err := writeLenEncBytes(body, decoded); err != nil {
						return nil, err
					}
				} else {
					if err := writeLenEncBytes(body, []byte(v)); err != nil {
						return nil, err
					}
				}
			default:
				return nil, fmt.Errorf("blob-like field %q has unsupported type %T", col.Name, ce.Value)
			}

		case mysql.FieldTypeTiny:
			if ce.Unsigned {
				if err := body.WriteByte(uint8(ce.Value.(int))); err != nil {
					return nil, err
				}
			} else {
				if err := body.WriteByte(byte(int8(ce.Value.(int)))); err != nil {
					return nil, err
				}
			}

		case mysql.FieldTypeShort, mysql.FieldTypeYear:
			if ce.Unsigned {
				v := uint16(ce.Value.(int))
				if err := binary.Write(body, binary.LittleEndian, v); err != nil {
					return nil, err
				}
			} else {
				v := int16(ce.Value.(int))
				if err := binary.Write(body, binary.LittleEndian, v); err != nil {
					return nil, err
				}
			}

		case mysql.FieldTypeLongLong:
			if ce.Unsigned {
				v := uint64(ce.Value.(int))
				if err := binary.Write(body, binary.LittleEndian, v); err != nil {
					return nil, err
				}
			} else {
				v := int64(ce.Value.(int))
				if err := binary.Write(body, binary.LittleEndian, v); err != nil {
					return nil, err
				}
			}

		case mysql.FieldTypeFloat:
			v := float32(ce.Value.(float32))
			if err := binary.Write(body, binary.LittleEndian, v); err != nil {
				return nil, err
			}

		case mysql.FieldTypeDouble:
			v := float64(ce.Value.(float64))
			if err := binary.Write(body, binary.LittleEndian, v); err != nil {
				return nil, err
			}

		case mysql.FieldTypeDate, mysql.FieldTypeNewDate, mysql.FieldTypeTimestamp, mysql.FieldTypeDateTime, mysql.FieldTypeTime:
			dt, err := encodeBinaryDateTime(ce.Type, ce.Value)
			if err != nil {
				return nil, err
			}
			if _, err := body.Write(dt); err != nil {
				return nil, err
			}

		default:
			return nil, fmt.Errorf("unsupported column type: %v", ce.Type)
		}
	}

	// Prepend header with computed payload length
	final := new(bytes.Buffer)
	if err := utils.WriteUint24(final, uint32(body.Len())); err != nil {
		return nil, fmt.Errorf("write header length: %w", err)
	}
	if err := final.WriteByte(row.Header.SequenceID); err != nil {
		return nil, fmt.Errorf("write header seq: %w", err)
	}
	if _, err := final.Write(body.Bytes()); err != nil {
		return nil, err
	}
	return final.Bytes(), nil
}

// small helper used above
func writeLenEncBytes(buf *bytes.Buffer, b []byte) error {
	if err := utils.WriteLengthEncodedInteger(buf, uint64(len(b))); err != nil {
		return err
	}
	_, err := buf.Write(b)
	return err
}

func encodeBinaryDateTime(fieldType mysql.FieldType, value interface{}) ([]byte, error) {
	switch fieldType {
	case mysql.FieldTypeDate, mysql.FieldTypeNewDate:
		// Date format: YYYY-MM-DD
		return encodeDate(value)
	case mysql.FieldTypeTimestamp, mysql.FieldTypeDateTime:
		// DateTime format: YYYY-MM-DD HH:MM:SS[.ffffff]
		return encodeDateTime(value)
	case mysql.FieldTypeTime:
		// Time format: [-]HH:MM:SS[.ffffff]
		return encodeTime(value)
	default:
		return nil, fmt.Errorf("unsupported date/time field type: %v", fieldType)
	}
}

func encodeDate(value interface{}) ([]byte, error) {
	dateStr, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("invalid value type for date field")
	}
	var year, month, day int
	_, err := fmt.Sscanf(dateStr, "%04d-%02d-%02d", &year, &month, &day)
	if err != nil {
		return nil, fmt.Errorf("failed to parse date string: %w", err)
	}
	buf := new(bytes.Buffer)
	err = buf.WriteByte(byte(4))
	if err != nil {
		return nil, fmt.Errorf("failed to write date length: %w", err)
	}
	err = binary.Write(buf, binary.LittleEndian, uint16(year))
	if err != nil {
		return nil, fmt.Errorf("failed to write year: %w", err)
	}
	err = buf.WriteByte(byte(month))
	if err != nil {
		return nil, fmt.Errorf("failed to write month: %w", err)
	}
	err = buf.WriteByte(byte(day))
	if err != nil {
		return nil, fmt.Errorf("failed to write day: %w", err)
	}
	return buf.Bytes(), nil
}

func encodeDateTime(value interface{}) ([]byte, error) {
	dateTimeStr, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("invalid value type for datetime field")
	}
	var (
		year, month, day, hour, minute, second, microsecond int
		length                                              byte
	)
	if strings.Contains(dateTimeStr, ".") {
		_, err := fmt.Sscanf(dateTimeStr, "%04d-%02d-%02d %02d:%02d:%02d.%06d",
			&year, &month, &day, &hour, &minute, &second, &microsecond)
		if err != nil {
			return nil, fmt.Errorf("failed to parse datetime string: %w", err)
		}
		length = 11
	} else {
		_, err := fmt.Sscanf(dateTimeStr, "%04d-%02d-%02d %02d:%02d:%02d",
			&year, &month, &day, &hour, &minute, &second)
		if err != nil {
			return nil, fmt.Errorf("failed to parse datetime string: %w", err)
		}
		length = 7
	}
	buf := new(bytes.Buffer)
	err := buf.WriteByte(length)
	if err != nil {
		return nil, fmt.Errorf("failed to write datetime length: %w", err)
	}
	err = binary.Write(buf, binary.LittleEndian, uint16(year))
	if err != nil {
		return nil, fmt.Errorf("failed to write year: %w", err)
	}
	err = buf.WriteByte(byte(month))
	if err != nil {
		return nil, fmt.Errorf("failed to write month: %w", err)
	}
	err = buf.WriteByte(byte(day))
	if err != nil {
		return nil, fmt.Errorf("failed to write day: %w", err)
	}
	err = buf.WriteByte(byte(hour))
	if err != nil {
		return nil, fmt.Errorf("failed to write hour: %w", err)
	}
	err = buf.WriteByte(byte(minute))
	if err != nil {
		return nil, fmt.Errorf("failed to write minute: %w", err)
	}
	err = buf.WriteByte(byte(second))
	if err != nil {
		return nil, fmt.Errorf("failed to write second: %w", err)
	}
	if length == 11 {
		err = binary.Write(buf, binary.LittleEndian, uint32(microsecond))
		if err != nil {
			return nil, fmt.Errorf("failed to write microseconds: %w", err)
		}
	}
	return buf.Bytes(), nil
}

func encodeTime(value interface{}) ([]byte, error) {
	timeStr, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("invalid value type for time field")
	}
	var (
		isNegative                                  bool
		days, hours, minutes, seconds, microseconds int
		length                                      byte
	)
	if timeStr[0] == '-' {
		isNegative = true
		timeStr = timeStr[1:]
	}
	if strings.Contains(timeStr, ".") {
		_, err := fmt.Sscanf(timeStr, "%d %02d:%02d:%02d.%06d",
			&days, &hours, &minutes, &seconds, &microseconds)
		if err != nil {
			return nil, fmt.Errorf("failed to parse time string: %w", err)
		}
		length = 12
	} else {
		_, err := fmt.Sscanf(timeStr, "%d %02d:%02d:%02d",
			&days, &hours, &minutes, &seconds)
		if err != nil {
			return nil, fmt.Errorf("failed to parse time string: %w", err)
		}
		length = 8
	}
	buf := new(bytes.Buffer)
	err := buf.WriteByte(length)
	if err != nil {
		return nil, fmt.Errorf("failed to write time length: %w", err)
	}
	if isNegative {
		buf.WriteByte(1)
	} else {
		buf.WriteByte(0)
	}
	err = binary.Write(buf, binary.LittleEndian, uint32(days))
	if err != nil {
		return nil, fmt.Errorf("failed to write days: %w", err)
	}
	err = buf.WriteByte(byte(hours))
	if err != nil {
		return nil, fmt.Errorf("failed to write hours: %w", err)
	}
	err = buf.WriteByte(byte(minutes))
	if err != nil {
		return nil, fmt.Errorf("failed to write minutes: %w", err)
	}
	err = buf.WriteByte(byte(seconds))
	if err != nil {
		return nil, fmt.Errorf("failed to write seconds: %w", err)
	}
	if length == 12 {
		err = binary.Write(buf, binary.LittleEndian, uint32(microseconds))
		if err != nil {
			return nil, fmt.Errorf("failed to write microseconds: %w", err)
		}
	}
	return buf.Bytes(), nil
}
