//go:build linux

package command

import (
	"bytes"
	"context"
	"errors"
	"fmt"

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
	fmt.Printf("before column %d", data)

	// Parse the columns
	for i := uint64(0); i < columnCount; i++ {
		columnPacket, pos, err := rowscols.DecodeColumn(ctx, logger, data)
		if err != nil {
			return nil, err
		}
		data = data[pos:]
		columns = append(columns, columnPacket)
	}
	fmt.Printf("After column %d", data)

	// Check for EOF packet after columns
	if utils.IsEOFPacket(data) {
		eofAfterColumns = data[:9]
		data = data[9:]
	}

	fmt.Printf("eofAfterColumns %d", eofAfterColumns)
	fmt.Printf("dataAfter %d", data)
	fmt.Printf("rowsDebug %d", rows)
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
		fmt.Printf("eofAfterRows %d", eofAfterRows)

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

// EncodeTextResultSet encodes a TextResultSet into a byte slice.
func EncodeTextResultSet(ctx context.Context, logger *zap.Logger, resultSet *mysql.TextResultSet) ([]byte, error) {
	return EncodeResultSet(ctx, logger, resultSet, Text)
}

// EncodeBinaryResultSet encodes a BinaryProtocolResultSet into a byte slice.
func EncodeBinaryResultSet(ctx context.Context, logger *zap.Logger, resultSet *mysql.BinaryProtocolResultSet) ([]byte, error) {
	return EncodeResultSet(ctx, logger, resultSet, Binary)
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

func TestDecodeEncode(ctx context.Context, logger *zap.Logger, original []byte, decodeFunc func(context.Context, *zap.Logger, []byte) (interface{}, error), encodeFunc func(context.Context, *zap.Logger, interface{}) ([]byte, error)) bool {
	// Decode the original data
	decoded, err := decodeFunc(ctx, logger, original)
	if err != nil {
		fmt.Printf("Decoding failed: %v\n", err)
		return false
	}

	// Encode the decoded data
	encoded, err := encodeFunc(ctx, logger, decoded)
	if err != nil {
		fmt.Printf("Encoding failed: %v\n", err)
		return false
	}

	// Compare the original and encoded data
	if bytes.Equal(original, encoded) {
		fmt.Println("Test passed: Decoded and encoded values match")
		return true
	} else {
		fmt.Println("Test failed: Decoded and encoded values do not match")
		fmt.Printf("Original: %v\nEncoded: %v\n", original, encoded)
		return false
	}
}


