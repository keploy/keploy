//go:build linux

package rowscols

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

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
		case mysql.FieldTypeDate, mysql.FieldTypeTime, mysql.FieldTypeDateTime, mysql.FieldTypeTimestamp:
			if dataLength < 4 || len(data[offset:]) < int(dataLength) {
				return nil, 0, fmt.Errorf("invalid timestamp data length")
			}
			offset++
			var year int
			var month, day, hour, minute, second int

			if dataLength >= 4 {
				year = int(binary.LittleEndian.Uint16(data[offset : offset+2]))
				month = int(data[offset+2])
				day = int(data[offset+3])
				offset += 4
			}
			if dataLength >= 7 {
				hour = int(data[offset])
				minute = int(data[offset+1])
				second = int(data[offset+2])
				offset += 3
			}

			formattedTime := fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d", year, month, day, hour, minute, second)
			row.Values = append(row.Values, mysql.ColumnEntry{
				Type:  mysql.FieldType(col.Type),
				Name:  col.Name,
				Value: formattedTime,
			})
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
		case mysql.FieldTypeDate, mysql.FieldTypeTime, mysql.FieldTypeDateTime, mysql.FieldTypeTimestamp:
			formattedTime, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("invalid value type for date/time field")
			}

			var year, month, day, hour, minute, second int
			if _, err := fmt.Sscanf(formattedTime, "%04d-%02d-%02d %02d:%02d:%02d", &year, &month, &day, &hour, &minute, &second); err != nil {
				return nil, fmt.Errorf("failed to parse formatted time: %w", err)
			}

			// Write the length of the date/time value
			if err := buf.WriteByte(11); err != nil {
				return nil, fmt.Errorf("failed to write date/time length: %w", err)
			}

			// Write the date/time value
			if err := binary.Write(buf, binary.LittleEndian, uint16(year)); err != nil {
				return nil, fmt.Errorf("failed to write year: %w", err)
			}
			if err := buf.WriteByte(byte(month)); err != nil {
				return nil, fmt.Errorf("failed to write month: %w", err)
			}
			if err := buf.WriteByte(byte(day)); err != nil {
				return nil, fmt.Errorf("failed to write day: %w", err)
			}
			if err := buf.WriteByte(byte(hour)); err != nil {
				return nil, fmt.Errorf("failed to write hour: %w", err)
			}
			if err := buf.WriteByte(byte(minute)); err != nil {
				return nil, fmt.Errorf("failed to write minute: %w", err)
			}
			if err := buf.WriteByte(byte(second)); err != nil {
				return nil, fmt.Errorf("failed to write second: %w", err)
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
