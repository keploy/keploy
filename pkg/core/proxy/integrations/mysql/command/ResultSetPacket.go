//go:build linux

package command

import (
	"bytes"
	"context"
	"fmt"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/command/rowscols"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.uber.org/zap"
)

type RowType int

// Constants for RowType
const (
	Binary RowType = iota
	Text
)

func DecodeResultSetMetadata(ctx context.Context, logger *zap.Logger, data []byte, rowType RowType) (interface{}, error) {

	// Decode the column count (No need to get the header as well as it is already decoded by the caller function)
	colCount, err := rowscols.DecodeColumnCount(ctx, logger, data)
	if err != nil {
		return nil, fmt.Errorf("failed to decode column count packet: %w", err)
	}

	switch rowType {
	case Binary:
		return &mysql.BinaryProtocolResultSet{
			ColumnCount: colCount,
		}, nil
	case Text:
		return &mysql.TextResultSet{
			ColumnCount: colCount,
		}, nil
	}
	return nil, nil
}

func EncodeResultSet(ctx context.Context, logger *zap.Logger, resultSet interface{}, rowType RowType) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Encode the column count packet
	var columnCount uint64
	var columns []*mysql.ColumnDefinition41
	var eofAfterColumns []byte
	var rows []interface{}
	var eofAfterRows []byte

	switch rs := resultSet.(type) {
	case *mysql.TextResultSet:
		columnCount = uint64(len(rs.Columns))
		columns = rs.Columns
		eofAfterColumns = rs.EOFAfterColumns
		rows = make([]interface{}, len(rs.Rows))
		for i, row := range rs.Rows {
			rows[i] = row
		}
		eofAfterRows = rs.EOFAfterRows
	case *mysql.BinaryProtocolResultSet:
		columnCount = uint64(len(rs.Columns))
		columns = rs.Columns
		eofAfterColumns = rs.EOFAfterColumns
		rows = make([]interface{}, len(rs.Rows))
		for i, row := range rs.Rows {
			rows[i] = row
		}
		eofAfterRows = rs.EOFAfterRows
	default:
		return nil, fmt.Errorf("unsupported result set type")
	}

	// Write the column count
	if err := utils.WriteLengthEncodedInteger(buf, columnCount); err != nil {
		return nil, fmt.Errorf("failed to write column count: %w", err)
	}

	// Encode the columns
	for _, column := range columns {
		columnBytes, err := rowscols.EncodeColumn(ctx, logger, column)
		if err != nil {
			return nil, fmt.Errorf("failed to encode column: %w", err)
		}
		if _, err := buf.Write(columnBytes); err != nil {
			return nil, fmt.Errorf("failed to write column: %w", err)
		}
	}

	// Write EOF packet after columns
	if _, err := buf.Write(eofAfterColumns); err != nil {
		return nil, fmt.Errorf("failed to write EOF packet after columns: %w", err)
	}

	// Encode the rows
	for _, row := range rows {
		var rowBytes []byte
		var err error
		if rowType == Binary {
			rowBytes, err = rowscols.EncodeBinaryRow(ctx, logger, row.(*mysql.BinaryRow), columns)
		} else {
			rowBytes, err = rowscols.EncodeTextRow(ctx, logger, row.(*mysql.TextRow), columns)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to encode row: %w", err)
		}
		if _, err := buf.Write(rowBytes); err != nil {
			return nil, fmt.Errorf("failed to write row: %w", err)
		}
	}

	// Write EOF packet after rows
	if _, err := buf.Write(eofAfterRows); err != nil {
		return nil, fmt.Errorf("failed to write EOF packet after rows: %w", err)
	}
	fmt.Printf("response.EOFAfterColumnDefs %d", columnCount, columns)

	return buf.Bytes(), nil
}

