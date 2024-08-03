//go:build linux

package rowscols

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.uber.org/zap"
)

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_query_response_text_resultset_row.html

func DecodeTextRow(_ context.Context, _ *zap.Logger, data []byte, columns []*mysql.ColumnDefinition41) (*mysql.TextRow, int, error) {
	offset := 0
	row := &mysql.TextRow{
		Header: mysql.Header{
			PayloadLength: utils.ReadUint24(data[offset : offset+3]),
			SequenceID:    data[offset+3],
		},
	}

	offset += 4
	if len(data) >= 2 && data[0] == 0x00 && data[1] == 0x00 {
		data = data[2:] // Skip padding
	}
	offset += 2
	for _, col := range columns {
		dataLength := data[offset]
		if dataLength == 0xfb { // NULL
			row.Values = append(row.Values, mysql.ColumnEntry{
				Type:  mysql.FieldType(col.Type),
				Name:  col.Name,
				Value: nil,
			})
			offset++
			continue
		}

		switch mysql.FieldType(col.Type) {
		case mysql.FieldTypeDate, mysql.FieldTypeTime, mysql.FieldTypeDateTime, mysql.FieldTypeTimestamp:
			data := data[offset+1:]
			if dataLength < 4 || len(data) < int(dataLength) {
				return nil, 0, fmt.Errorf("invalid timestamp data length")
			}
			dateStr := string(data[:dataLength])
			var t time.Time
			var err error
			switch mysql.FieldType(col.Type) {
			case mysql.FieldTypeDate:
				t, err = time.Parse("2006-01-02", dateStr)
			case mysql.FieldTypeTime:
				t, err = time.Parse("15:04:05", dateStr)
			case mysql.FieldTypeDateTime, mysql.FieldTypeTimestamp:
				t, err = time.Parse("2006-01-02 15:04:05", dateStr)
			}
			if err != nil {
				return nil, 0, fmt.Errorf("failed to parse the time string: %v", err)
			}
			row.Values = append(row.Values, mysql.ColumnEntry{
				Type:  mysql.FieldType(col.Type),
				Name:  col.Name,
				Value: t,
			})
			offset += int(dataLength) + 1
		case mysql.FieldTypeLongLong:
			// Handle unsigned 64-bit integers
			if col.Flags&mysql.UNSIGNED_FLAG != 0 {
				if offset+8 > len(data) {
					return nil, 0, fmt.Errorf("invalid data length for unsigned long long")
				}
				value := binary.LittleEndian.Uint64(data[offset : offset+8])
				row.Values = append(row.Values, mysql.ColumnEntry{
					Type:  mysql.FieldType(col.Type),
					Name:  col.Name,
					Value: value,
				})
				offset += 8
			} else {
				value, _, n, err := utils.ReadLengthEncodedString(data[offset:])
				if err != nil {
					return nil, offset, err
				}
				row.Values = append(row.Values, mysql.ColumnEntry{
					Type:  mysql.FieldType(col.Type),
					Name:  col.Name,
					Value: string(value),
				})
				offset += n
			}
		default:
			fmt.Print("console", data[offset:])
			value, _, n, err := utils.ReadLengthEncodedString(data[offset:])
			if err != nil {
				return nil, offset, err
			}
			fmt.Print("here", value)

			row.Values = append(row.Values, mysql.ColumnEntry{
				Type:  mysql.FieldType(col.Type),
				Name:  col.Name,
				Value: string(value),
			})
			offset += n
		}
	}
	fmt.Printf("Decoded TextRow",
		zap.Uint32("RowPayloadLength", row.Header.PayloadLength),
		zap.Uint8("RowSequenceID", row.Header.SequenceID),
		zap.Any("RowValues", row.Values),
	)
	return row, offset, nil
}

func EncodeTextRow(_ context.Context, _ *zap.Logger, row *mysql.TextRow, columns []*mysql.ColumnDefinition41) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Write Header
	utils.WriteUint24(buf, row.Header.PayloadLength)
	if err := buf.WriteByte(row.Header.SequenceID); err != nil {
		return nil, fmt.Errorf("failed to write SequenceID: %w", err)
	}

	// Write Values
	for i, col := range columns {
		entry := row.Values[i]
		if entry.Value == nil {
			if err := buf.WriteByte(0xfb); err != nil { // NULL
				return nil, fmt.Errorf("failed to write NULL value: %w", err)
			}
			continue
		}

		switch mysql.FieldType(col.Type) {
		case mysql.FieldTypeDate, mysql.FieldTypeTime, mysql.FieldTypeDateTime, mysql.FieldTypeTimestamp:
			dateStr, ok := entry.Value.(string)
			if !ok {
				return nil, fmt.Errorf("expected string for timestamp value")
			}

			if err := buf.WriteByte(byte(len(dateStr))); err != nil {
				return nil, fmt.Errorf("failed to write length of date string: %w", err)
			}

			if _, err := buf.WriteString(dateStr); err != nil {
				return nil, fmt.Errorf("failed to write date string: %w", err)
			}

		default:
			strValue, ok := entry.Value.(string)
			if !ok {
				return nil, fmt.Errorf("expected string for column value")
			}

			if err := utils.WriteLengthEncodedString(buf, []byte(strValue)); err != nil {
				return nil, fmt.Errorf("failed to write length-encoded string: %w", err)
			}
		}
	}

	return buf.Bytes(), nil
}
