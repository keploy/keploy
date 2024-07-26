//go:build linux

package rowscols

import (
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
		case mysql.FieldTypeDate, mysql.FieldTypeTime, mysql.FieldTypeDateTime, mysql.FieldTypeTimestamp:
			data := data[offset+1:]

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
