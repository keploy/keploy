// Package rowscols provides encoding and decoding of MySQL row & column packets.
package rowscols

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/models/mysql"
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

func isZeroDateTimeString(s string) bool {
	s = strings.TrimSpace(s)
	return s == utils.ZeroDateString ||
		s == utils.ZeroDateTimeString ||
		s == utils.ZeroDateTimeString+".000000"
}

func isZeroTimeString(s string) bool {
	s = strings.TrimSpace(s)
	return s == utils.ZeroTimeString ||
		s == utils.ZeroTimeString+".000000" ||
		s == "0 00:00:00" ||
		s == "0 00:00:00.000000" ||
		s == "-00:00:00" ||
		s == "-00:00:00.000000"
}

func isZeroDateString(s string) bool {
	return strings.TrimSpace(s) == utils.ZeroDateString
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

		if ce.Value == nil {
			return nil, fmt.Errorf("encode: value for %q is nil but NULL bit not set", columns[i].Name)
		}

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

// accepts string, []byte, time.Time, or fmt.Stringer; returns a normalized string.
func coerceToString(v interface{}) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case []byte:
		return string(t), nil
	case time.Time:
		// MySQL DATETIME has microsecond precision max; drop to microseconds if needed.
		ts := t.Round(time.Microsecond)
		base := ts.Format("2006-01-02 15:04:05")
		usec := ts.Nanosecond() / 1e3
		if usec == 0 {
			return base, nil
		}
		return fmt.Sprintf("%s.%06d", base, usec), nil
	case fmt.Stringer:
		return t.String(), nil
	default:
		return "", fmt.Errorf("cannot coerce %T to string", v)
	}
}

// accepts string, []byte, time.Time, or fmt.Stringer; returns a normalized date string (YYYY-MM-DD).
func coerceDateToString(v interface{}) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case []byte:
		return string(t), nil
	case time.Time:
		// For dates, we only need the date part (YYYY-MM-DD)
		return t.Format("2006-01-02"), nil
	case fmt.Stringer:
		return t.String(), nil
	default:
		return "", fmt.Errorf("cannot coerce %T to date string", v)
	}
}

// strip ISO sugar that MySQL DATETIME can't carry (T, trailing Z, trailing ±HH:MM)
var tzSuffixRe = regexp.MustCompile(`([+-]\d{2}:\d{2})$`)

func stripISOStuff(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "T", " ")
	if strings.HasSuffix(s, "Z") {
		s = strings.TrimSuffix(s, "Z")
		s = strings.TrimSpace(s)
	}
	if m := tzSuffixRe.FindStringSubmatch(s); len(m) > 1 {
		s = strings.TrimSuffix(s, m[1])
		s = strings.TrimSpace(s)
	}
	// common textual tails that sometimes appear
	s = strings.TrimSuffix(s, " UTC")
	s = strings.TrimSuffix(s, " GMT")
	return s
}

// normalize fractional seconds to exactly 6 digits (trim or pad right with zeros)
func normFrac6(frac string) string {
	if len(frac) > 6 {
		return frac[:6]
	}
	if len(frac) < 6 {
		return frac + strings.Repeat("0", 6-len(frac))
	}
	return frac
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
	dateStr, err := coerceDateToString(value)
	if err != nil {
		return nil, err
	}
	dateStr = stripISOStuff(dateStr)
	// If a time portion leaked in, keep just the date
	if i := strings.IndexByte(dateStr, ' '); i >= 0 {
		dateStr = dateStr[:i]
	}

	if isZeroDateString(dateStr) {
		return []byte{0x00}, nil
	}

	var year, month, day int
	_, err = fmt.Sscanf(dateStr, "%04d-%02d-%02d", &year, &month, &day)
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
	s, err := coerceToString(value)
	if err != nil {
		return nil, err
	}
	s = stripISOStuff(s)

	// Accept both full zero and date-only zero forms.
	if isZeroDateTimeString(s) {
		return []byte{0x00}, nil
	}

	// allow MySQL DATETIME encoded as date-only variant (length=4)
	if !strings.Contains(s, " ") {
		var y, m, d int
		if _, err := fmt.Sscanf(s, "%04d-%02d-%02d", &y, &m, &d); err != nil {
			return nil, fmt.Errorf("failed to parse datetime (date-only) %q: %w", s, err)
		}
		buf := new(bytes.Buffer)
		if err := buf.WriteByte(4); err != nil {
			return nil, fmt.Errorf("failed to write datetime length: %w", err)
		}
		if err := binary.Write(buf, binary.LittleEndian, uint16(y)); err != nil {
			return nil, fmt.Errorf("failed to write year: %w", err)
		}
		if err := buf.WriteByte(byte(m)); err != nil {
			return nil, fmt.Errorf("failed to write month: %w", err)
		}
		if err := buf.WriteByte(byte(d)); err != nil {
			return nil, fmt.Errorf("failed to write day: %w", err)
		}
		return buf.Bytes(), nil
	}

	// has date and time
	var y, m, d, hh, mm, ss, usec int
	hasFrac := strings.Contains(s, ".")
	if hasFrac {
		// normalize to exactly 6 fractional digits before scanning
		// split on last '.' to avoid surprises
		i := strings.LastIndexByte(s, '.')
		if i > 0 && i < len(s)-1 {
			head := s[:i]
			frac := normFrac6(s[i+1:])
			s = head + "." + frac
		}
		if _, err := fmt.Sscanf(s, "%04d-%02d-%02d %02d:%02d:%02d.%06d",
			&y, &m, &d, &hh, &mm, &ss, &usec); err != nil {
			return nil, fmt.Errorf("failed to parse datetime %q: %w", s, err)
		}
	} else {
		if _, err := fmt.Sscanf(s, "%04d-%02d-%02d %02d:%02d:%02d",
			&y, &m, &d, &hh, &mm, &ss); err != nil {
			return nil, fmt.Errorf("failed to parse datetime %q: %w", s, err)
		}
	}

	buf := new(bytes.Buffer)
	// length byte: 7 (no microseconds) or 11 (with microseconds)
	if hasFrac {
		if err := buf.WriteByte(11); err != nil {
			return nil, fmt.Errorf("failed to write datetime length: %w", err)
		}
	} else {
		if err := buf.WriteByte(7); err != nil {
			return nil, fmt.Errorf("failed to write datetime length: %w", err)
		}
	}

	if err := binary.Write(buf, binary.LittleEndian, uint16(y)); err != nil {
		return nil, fmt.Errorf("failed to write year: %w", err)
	}
	if err := buf.WriteByte(byte(m)); err != nil {
		return nil, fmt.Errorf("failed to write month: %w", err)
	}
	if err := buf.WriteByte(byte(d)); err != nil {
		return nil, fmt.Errorf("failed to write day: %w", err)
	}
	if err := buf.WriteByte(byte(hh)); err != nil {
		return nil, fmt.Errorf("failed to write hour: %w", err)
	}
	if err := buf.WriteByte(byte(mm)); err != nil {
		return nil, fmt.Errorf("failed to write minute: %w", err)
	}
	if err := buf.WriteByte(byte(ss)); err != nil {
		return nil, fmt.Errorf("failed to write second: %w", err)
	}
	if hasFrac {
		if err := binary.Write(buf, binary.LittleEndian, uint32(usec)); err != nil {
			return nil, fmt.Errorf("failed to write microseconds: %w", err)
		}
	}
	return buf.Bytes(), nil
}

func encodeTime(value interface{}) ([]byte, error) {
	s, err := coerceToString(value)
	if err != nil {
		return nil, err
	}
	s = strings.TrimSpace(s)

	if isZeroTimeString(s) {
		return []byte{0x00}, nil
	}

	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}

	days, hh, mm, ss, usec := 0, 0, 0, 0, 0
	hasFrac := strings.Contains(s, ".")

	switch strings.Count(s, " ") {
	case 0:
		if hasFrac {
			// normalize fractional part to 6 digits
			i := strings.LastIndexByte(s, '.')
			if i > 0 && i < len(s)-1 {
				head := s[:i]
				frac := normFrac6(s[i+1:])
				s = head + "." + frac
			}
			if _, err := fmt.Sscanf(s, "%02d:%02d:%02d.%06d", &hh, &mm, &ss, &usec); err != nil {
				return nil, fmt.Errorf("failed to parse time %q: %w", s, err)
			}
		} else {
			if _, err := fmt.Sscanf(s, "%02d:%02d:%02d", &hh, &mm, &ss); err != nil {
				return nil, fmt.Errorf("failed to parse time %q: %w", s, err)
			}
		}
	default: // "D HH:MM:SS[.uuuuuu]"
		if hasFrac {
			i := strings.LastIndexByte(s, '.')
			if i > 0 && i < len(s)-1 {
				head := s[:i]
				frac := normFrac6(s[i+1:])
				s = head + "." + frac
			}
			if _, err := fmt.Sscanf(s, "%d %02d:%02d:%02d.%06d", &days, &hh, &mm, &ss, &usec); err != nil {
				return nil, fmt.Errorf("failed to parse time %q: %w", s, err)
			}
		} else {
			if _, err := fmt.Sscanf(s, "%d %02d:%02d:%02d", &days, &hh, &mm, &ss); err != nil {
				return nil, fmt.Errorf("failed to parse time %q: %w", s, err)
			}
		}
	}

	buf := new(bytes.Buffer)
	if hasFrac {
		if err := buf.WriteByte(12); err != nil {
			return nil, fmt.Errorf("failed to write time length: %w", err)
		}
	} else {
		if err := buf.WriteByte(8); err != nil {
			return nil, fmt.Errorf("failed to write time length: %w", err)
		}
	}

	if neg {
		if err := buf.WriteByte(1); err != nil {
			return nil, fmt.Errorf("failed to write sign: %w", err)
		}
	} else {
		if err := buf.WriteByte(0); err != nil {
			return nil, fmt.Errorf("failed to write sign: %w", err)
		}
	}

	if err := binary.Write(buf, binary.LittleEndian, uint32(days)); err != nil {
		return nil, fmt.Errorf("failed to write days: %w", err)
	}
	if err := buf.WriteByte(byte(hh)); err != nil {
		return nil, fmt.Errorf("failed to write hours: %w", err)
	}
	if err := buf.WriteByte(byte(mm)); err != nil {
		return nil, fmt.Errorf("failed to write minutes: %w", err)
	}
	if err := buf.WriteByte(byte(ss)); err != nil {
		return nil, fmt.Errorf("failed to write seconds: %w", err)
	}
	if hasFrac {
		if err := binary.Write(buf, binary.LittleEndian, uint32(usec)); err != nil {
			return nil, fmt.Errorf("failed to write microseconds: %w", err)
		}
	}
	return buf.Bytes(), nil
}
