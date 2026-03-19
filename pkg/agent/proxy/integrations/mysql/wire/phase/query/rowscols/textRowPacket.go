package rowscols

import (
	"bytes"
	"context"
	"fmt"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/models/mysql"
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

// rowscols/textRowPacket.go

func EncodeTextRow(_ context.Context, _ *zap.Logger, row *mysql.TextRow, columns []*mysql.ColumnDefinition41) ([]byte, error) {
	body := new(bytes.Buffer)

	// Write each column's value into the body
	for i := range columns {
		v := row.Values[i].Value
		switch x := v.(type) {
		case nil:
			// NULL is 0xfb
			if err := body.WriteByte(0xfb); err != nil {
				return nil, fmt.Errorf("failed to write NULL: %w", err)
			}

		case string:
			if err := utils.WriteLengthEncodedString(body, x); err != nil {
				return nil, fmt.Errorf("failed to write lenenc string: %w", err)
			}

		case []byte:
			// allow blobs as raw bytes in text rows
			if err := utils.WriteLengthEncodedInteger(body, uint64(len(x))); err != nil {
				return nil, fmt.Errorf("failed to write len for bytes: %w", err)
			}
			if _, err := body.Write(x); err != nil {
				return nil, fmt.Errorf("failed to write bytes: %w", err)
			}

		default:
			// fall back to the textual form (keeps old recordings working)
			s := fmt.Sprint(x)
			if err := utils.WriteLengthEncodedString(body, s); err != nil {
				return nil, fmt.Errorf("failed to write fallback string: %w", err)
			}
		}
	}

	// Now prepend the header using the computed length
	out := new(bytes.Buffer)
	if err := utils.WriteUint24(out, uint32(body.Len())); err != nil {
		return nil, fmt.Errorf("failed to write PayloadLength: %w", err)
	}
	if err := out.WriteByte(row.Header.SequenceID); err != nil {
		return nil, fmt.Errorf("failed to write SequenceID: %w", err)
	}
	if _, err := out.Write(body.Bytes()); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
