//go:build linux

package query

import (
	"bytes"
	"context"
	"fmt"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	mysqlUtils "go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/wire/phase/query/rowscols"
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

func EncodeTextResultSet(ctx context.Context, logger *zap.Logger, resultSet *mysql.TextResultSet) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Encode the column count
	if err := utils.WriteLengthEncodedInteger(buf, resultSet.ColumnCount); err != nil {
		return nil, fmt.Errorf("failed to write column count for text resultset: %w", err)
	}

	// Encode the column definition packets
	for _, column := range resultSet.Columns {
		columnBytes, err := rowscols.EncodeColumn(ctx, logger, column)
		if err != nil {
			return nil, fmt.Errorf("failed to encode column for text resultset: %w", err)
		}
		if _, err := buf.Write(columnBytes); err != nil {
			return nil, fmt.Errorf("failed to write column for text resultset: %w", err)
		}
	}

	// Write the EOF packet after columns
	if _, err := buf.Write(resultSet.EOFAfterColumns); err != nil {
		return nil, fmt.Errorf("failed to write EOF packet after columns for text resultset: %w", err)
	}

	// Encode each row data packet
	for _, row := range resultSet.Rows {
		rowBytes, err := rowscols.EncodeTextRow(ctx, logger, row, resultSet.Columns)
		if err != nil {
			return nil, fmt.Errorf("failed to encode row for text resultset: %w", err)
		}
		if _, err := buf.Write(rowBytes); err != nil {
			return nil, fmt.Errorf("failed to write row for text resultset: %w", err)
		}
	}
	// Write the final EOF packet if present
	if resultSet.FinalResponse != nil && mysqlUtils.IsEOFPacket(resultSet.FinalResponse.Data) {
		if _, err := buf.Write(resultSet.FinalResponse.Data); err != nil {
			return nil, fmt.Errorf("failed to write final EOF packet: %w", err)
		}
	}

	return buf.Bytes(), nil
}

func EncodeBinaryResultSet(ctx context.Context, logger *zap.Logger, resultSet *mysql.BinaryProtocolResultSet) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Encode the column count
	if err := utils.WriteLengthEncodedInteger(buf, resultSet.ColumnCount); err != nil {
		return nil, fmt.Errorf("failed to write column count: %w", err)
	}

	// Encode the column definition packets
	for _, column := range resultSet.Columns {
		columnBytes, err := rowscols.EncodeColumn(ctx, logger, column)
		if err != nil {
			return nil, fmt.Errorf("failed to encode column: %w", err)
		}
		if _, err := buf.Write(columnBytes); err != nil {
			return nil, fmt.Errorf("failed to write column: %w", err)
		}
	}

	// Write the EOF packet after columns
	if _, err := buf.Write(resultSet.EOFAfterColumns); err != nil {
		return nil, fmt.Errorf("failed to write EOF packet after columns: %w", err)
	}

	// Encode each row data packet
	for _, row := range resultSet.Rows {
		rowBytes, err := rowscols.EncodeBinaryRow(ctx, logger, row, resultSet.Columns)
		if err != nil {
			return nil, fmt.Errorf("failed to encode row: %w", err)
		}
		if _, err := buf.Write(rowBytes); err != nil {
			return nil, fmt.Errorf("failed to write row: %w", err)
		}
	}

	// Write the final EOF packet if present
	if resultSet.FinalResponse != nil && mysqlUtils.IsEOFPacket(resultSet.FinalResponse.Data) {
		if _, err := buf.Write(resultSet.FinalResponse.Data); err != nil {
			return nil, fmt.Errorf("failed to write final EOF packet: %w", err)
		}
	}

	return buf.Bytes(), nil
}
