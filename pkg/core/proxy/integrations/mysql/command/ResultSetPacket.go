//go:build linux

package command

import (
	"context"
	"errors"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/command/rowscols"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.uber.org/zap"
)

type RowType int

// Constants for RowType
const (
	Binary RowType = iota
	Text
)

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_query_response_text_resultset.html

func DecodeTextResultSet(ctx context.Context, logger *zap.Logger, data []byte) (*mysql.TextResultSet, error) {
	result, err := DecodeResultSet(ctx, logger, data, Text)
	if err != nil {
		return nil, err
	}
	return result.(*mysql.TextResultSet), nil
}

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_binary_resultset.html
// (BinaryProtocolResultset)

func DecodeBinaryResultSet(ctx context.Context, logger *zap.Logger, data []byte) (*mysql.BinaryProtocolResultSet, error) {
	result, err := DecodeResultSet(ctx, logger, data, Binary)
	if err != nil {
		return nil, err
	}
	return result.(*mysql.BinaryProtocolResultSet), nil
}

func DecodeResultSet(ctx context.Context, logger *zap.Logger, data []byte, rowType RowType) (interface{}, error) {
	columns := make([]*mysql.ColumnDefinition41, 0)
	var rows []interface{}
	var eofAfterColumns []byte
	var eofAfterRows []byte

	// Parse the column count packet
	columnCount, _, n := utils.ReadLengthEncodedInteger(data)
	if n == 0 {
		return nil, errors.New("invalid column count")
	}

	// Move the buffer forward by the length of the column count packet
	data = data[n:]

	// Parse the columns
	for i := uint64(0); i < columnCount; i++ {
		columnPacket, pos, err := rowscols.DecodeColumn(ctx, logger, data)
		if err != nil {
			return nil, err
		}
		data = data[pos:]
		columns = append(columns, columnPacket)
	}

	// Check for EOF packet after columns
	if utils.IsEOFPacket(data) {
		eofAfterColumns = data[:9]
		data = data[9:]
	}

	// Parse the rows
	for len(data) > 0 {
		if ctx.Err() == context.Canceled {
			return nil, context.Canceled
		}

		// Check for EOF packet after columns
		if utils.IsEOFPacket(data) {
			eofAfterRows = data[:9]
			break
		}

		var row interface{}
		var err error
		if rowType == Binary {
			row, n, err = rowscols.DecodeBinaryRow(ctx, logger, data, columns)
		} else {
			row, n, err = rowscols.DecodeTextRow(ctx, logger, data, columns)
		}

		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
		data = data[n:]
	}

	if rowType == Binary {
		binaryRows := make([]*mysql.BinaryRow, len(rows))
		for i, row := range rows {
			binaryRows[i] = row.(*mysql.BinaryRow)
		}

		return &mysql.BinaryProtocolResultSet{
			Columns:         columns,
			EOFAfterColumns: eofAfterColumns,
			Rows:            binaryRows,
			EOFAfterRows:    eofAfterRows,
		}, nil
	}

	textRows := make([]*mysql.TextRow, len(rows))
	for i, row := range rows {
		textRows[i] = row.(*mysql.TextRow)
	}

	return &mysql.TextResultSet{
		Columns:         columns,
		EOFAfterColumns: eofAfterColumns,
		Rows:            textRows,
		EOFAfterRows:    eofAfterRows,
	}, nil
}
