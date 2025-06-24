//go:build linux

package rowscols

import (
	"bytes"
	"context"
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

		value, _, n, err := utils.ReadLengthEncodedString(data[offset:])
		if err != nil {
			return nil, offset, fmt.Errorf("failed to read length-encoded string: %w", err)
		}
		row.Values = append(row.Values, mysql.ColumnEntry{
			Type:  mysql.FieldType(col.Type),
			Name:  col.Name,
			Value: string(value),
		})
		offset += n
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

		// Write length-encoded string
		strValue, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("invalid value type for column %s", col.Name)
		}
		if err := utils.WriteLengthEncodedString(buf, strValue); err != nil {
			return nil, fmt.Errorf("failed to write length-encoded string: %w", err)
		}
	}

	if row.Header.PayloadLength != uint32(buf.Len()-4) {
		return nil, fmt.Errorf("PayloadLength mismatch: expected %d, got %d for row: %v",
			row.Header.PayloadLength, buf.Len()-4, row)
	}

	return buf.Bytes(), nil
}
