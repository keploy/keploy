//go:build linux

package rowscols

import (
	"bytes"
	"context"
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
		case mysql.FieldTypeDate:
			data := data[offset+1:]
			if dataLength < 10 || len(data) < int(dataLength) {
				return nil, 0, fmt.Errorf("invalid date data length")
			}
			dateStr := string(data[:dataLength])
			layout := "2006-01-02"
			t, err := time.Parse(layout, dateStr)
			if err != nil {
				return nil, 0, fmt.Errorf("failed to parse the date string")
			}

			year, month, day := t.Date()
			row.Values = append(row.Values, mysql.ColumnEntry{
				Type:  mysql.FieldType(col.Type),
				Name:  col.Name,
				Value: fmt.Sprintf("%04d-%02d-%02d", year, int(month), day),
			})

			offset += int(dataLength) + 1

		case mysql.FieldTypeTime:
			data := data[offset+1:]
			if dataLength < 8 || len(data) < int(dataLength) {
				return nil, 0, fmt.Errorf("invalid time data length")
			}
			timeStr := string(data[:dataLength])
			layout := "15:04:05"
			t, err := time.Parse(layout, timeStr)
			if err != nil {
				return nil, 0, fmt.Errorf("failed to parse the time string")
			}

			hour, minute, second := t.Clock()
			row.Values = append(row.Values, mysql.ColumnEntry{
				Type:  mysql.FieldType(col.Type),
				Name:  col.Name,
				Value: fmt.Sprintf("%02d:%02d:%02d", hour, minute, second),
			})

			offset += int(dataLength) + 1

		case mysql.FieldTypeDateTime, mysql.FieldTypeTimestamp:
			data := data[offset+1:]
			if dataLength < 19 || len(data) < int(dataLength) {
				return nil, 0, fmt.Errorf("invalid datetime/timestamp data length")
			}
			dateTimeStr := string(data[:dataLength])
			layout := "2006-01-02 15:04:05"
			t, err := time.Parse(layout, dateTimeStr)
			if err != nil {
				return nil, 0, fmt.Errorf("failed to parse the datetime/timestamp string: received %s, expected format %s", dateTimeStr, layout)
			}

			year, month, day := t.Date()
			hour, minute, second := t.Clock()
			row.Values = append(row.Values, mysql.ColumnEntry{
				Type:  mysql.FieldType(col.Type),
				Name:  col.Name,
				Value: fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d", year, int(month), day, hour, minute, second),
			})

			offset += int(dataLength) + 1

		default:
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
	}
	return row, offset, nil
}

func EncodeTextRow(_ context.Context, _ *zap.Logger, row *mysql.TextRow, columns []*mysql.ColumnDefinition41) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Write the packet header
	if err := utils.WriteUint24(buf, row.Header.PayloadLength); err != nil {
		return nil, fmt.Errorf("failed to write PayloadLength: %w", err)
	}
	if err := buf.WriteByte(row.Header.SequenceID); err != nil {
		return nil, fmt.Errorf("failed to write SequenceID: %w", err)
	}

	// Write each column's value
	for i, col := range columns {
		value := row.Values[i].Value
		if value == nil {
			// Write NULL value
			if err := buf.WriteByte(0xfb); err != nil {
				return nil, fmt.Errorf("failed to write NULL value: %w", err)
			}
			continue
		}

		switch row.Values[i].Type {
		case mysql.FieldTypeDate:
			dateValue, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("invalid value type for date field")
			}

			// Write the length of the date value
			if err := buf.WriteByte(byte(len(dateValue))); err != nil {
				return nil, fmt.Errorf("failed to write date length: %w", err)
			}

			// Write the date value
			if _, err := buf.WriteString(dateValue); err != nil {
				return nil, fmt.Errorf("failed to write date value: %w", err)
			}

		case mysql.FieldTypeTime:
			timeValue, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("invalid value type for time field")
			}

			// Write the length of the time value
			if err := buf.WriteByte(byte(len(timeValue))); err != nil {
				return nil, fmt.Errorf("failed to write time length: %w", err)
			}

			// Write the time value
			if _, err := buf.WriteString(timeValue); err != nil {
				return nil, fmt.Errorf("failed to write time value: %w", err)
			}

		case mysql.FieldTypeDateTime, mysql.FieldTypeTimestamp:
			dateTimeValue, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("invalid value type for datetime/timestamp field")
			}

			// Write the length of the datetime/timestamp value
			if err := buf.WriteByte(byte(len(dateTimeValue))); err != nil {
				return nil, fmt.Errorf("failed to write datetime/timestamp length: %w", err)
			}

			// Write the datetime/timestamp value
			if _, err := buf.WriteString(dateTimeValue); err != nil {
				return nil, fmt.Errorf("failed to write datetime/timestamp value: %w", err)
			}

		default:
			// Write length-encoded string for other types
			strValue, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("invalid value type for column %s", col.Name)
			}
			if err := utils.WriteLengthEncodedString(buf, strValue); err != nil {
				return nil, fmt.Errorf("failed to write length-encoded string: %w", err)
			}
		}
	}

	return buf.Bytes(), nil
}
