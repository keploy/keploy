package query

import (
	"bytes"
	"context"
	"fmt"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query/rowscols"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// EncodeStmtFetchResponse encodes the row-only response to COM_STMT_FETCH.
func EncodeStmtFetchResponse(ctx context.Context, logger *zap.Logger, response *mysql.StmtFetchResponse) ([]byte, error) {
	if response == nil {
		return nil, fmt.Errorf("encode COM_STMT_FETCH response: response is nil")
	}
	if response.FinalResponse == nil || len(response.FinalResponse.Data) == 0 {
		return nil, fmt.Errorf("encode COM_STMT_FETCH response: final response is missing")
	}

	var out bytes.Buffer
	for i, row := range response.Rows {
		columns, err := columnsFromBinaryRow(row)
		if err != nil {
			return nil, fmt.Errorf("encode COM_STMT_FETCH row %d: %w", i, err)
		}
		packet, err := rowscols.EncodeBinaryRow(ctx, logger, row, columns)
		if err != nil {
			return nil, fmt.Errorf("encode COM_STMT_FETCH row %d: %w", i, err)
		}
		if _, err := out.Write(packet); err != nil {
			return nil, fmt.Errorf("write COM_STMT_FETCH row %d: %w", i, err)
		}
	}
	if _, err := out.Write(response.FinalResponse.Data); err != nil {
		return nil, fmt.Errorf("write COM_STMT_FETCH final response: %w", err)
	}
	return out.Bytes(), nil
}

func columnsFromBinaryRow(row *mysql.BinaryRow) ([]*mysql.ColumnDefinition41, error) {
	if row == nil {
		return nil, fmt.Errorf("row is nil")
	}
	columns := make([]*mysql.ColumnDefinition41, len(row.Values))
	for i, value := range row.Values {
		flags := uint16(0)
		if value.Unsigned {
			flags = mysql.UNSIGNED_FLAG
		}
		columns[i] = &mysql.ColumnDefinition41{
			Name:  value.Name,
			Type:  byte(value.Type),
			Flags: flags,
		}
	}
	return columns, nil
}
