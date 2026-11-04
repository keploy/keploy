package query

import (
	"bytes"
	"context"
	"fmt"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query/rowscols"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

type RowType int

// Constants for RowType
const (
	Binary RowType = iota
	Text
)

func DecodeResultSetMetadata(ctx context.Context, logger *zap.Logger, data []byte, rowType RowType, framing mysql.MetadataFraming) (interface{}, error) {

	// Decode the column count (No need to get the header as well as it is already decoded by the caller function)
	colCount, err := rowscols.DecodeColumnCount(ctx, logger, data, framing)
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

// ColumnCountPayloadLen is the length of the payload Encode*ResultSet writes
// for a result set's column count packet, the first of its packets, which it
// writes without a header: the length-encoded count, then the
// metadata-follows byte under framing.MetadataFollows.
func ColumnCountPayloadLen(count uint64, framing mysql.MetadataFraming) int {
	n := utils.LengthEncodedIntegerSize(count)
	if framing.MetadataFollows() {
		n++
	}
	return n
}

// EncodeTextResultSet encodes resultSet framed as the connection it is sent on
// negotiated (see mysql.MetadataFraming).
func EncodeTextResultSet(ctx context.Context, logger *zap.Logger, resultSet *mysql.TextResultSet, framing mysql.MetadataFraming) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Encode the column count
	if err := utils.WriteLengthEncodedInteger(buf, resultSet.ColumnCount); err != nil {
		return nil, fmt.Errorf("failed to write column count for text resultset: %w", err)
	}

	// The metadata-follows byte: the definitions always follow here.
	if framing.MetadataFollows() {
		buf.WriteByte(1)
	}

	// Encode the column definition packets
	for _, column := range resultSet.Columns {
		columnBytes, err := rowscols.EncodeColumn(ctx, logger, column, framing)
		if err != nil {
			return nil, fmt.Errorf("failed to encode column for text resultset: %w", err)
		}
		if _, err := buf.Write(columnBytes); err != nil {
			return nil, fmt.Errorf("failed to write column for text resultset: %w", err)
		}
	}

	// Write the EOF packet after columns if present
	if len(resultSet.EOFAfterColumns) != 0 {
		if _, err := buf.Write(resultSet.EOFAfterColumns); err != nil {
			return nil, fmt.Errorf("failed to write EOF packet after columns for text resultset: %w", err)
		}
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
	// Write the final response (EOF or OK-replacing-EOF) if present
	if resultSet.FinalResponse != nil && (utils.IsEOFPacket(resultSet.FinalResponse.Data) || utils.IsOKReplacingEOF(resultSet.FinalResponse.Data)) {
		if _, err := buf.Write(resultSet.FinalResponse.Data); err != nil {
			return nil, fmt.Errorf("failed to write final response packet: %w", err)
		}
	}

	return buf.Bytes(), nil
}

// EncodeBinaryResultSet encodes resultSet framed as the connection it is sent on
// negotiated (see mysql.MetadataFraming).
func EncodeBinaryResultSet(ctx context.Context, logger *zap.Logger, resultSet *mysql.BinaryProtocolResultSet, framing mysql.MetadataFraming) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Encode the column count
	if err := utils.WriteLengthEncodedInteger(buf, resultSet.ColumnCount); err != nil {
		return nil, fmt.Errorf("failed to write column count: %w", err)
	}

	// The metadata-follows byte: the definitions always follow here.
	if framing.MetadataFollows() {
		buf.WriteByte(1)
	}

	// Encode the column definition packets
	for _, column := range resultSet.Columns {
		columnBytes, err := rowscols.EncodeColumn(ctx, logger, column, framing)
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

	// Write the final response (EOF or OK-replacing-EOF) if present
	if resultSet.FinalResponse != nil && (utils.IsEOFPacket(resultSet.FinalResponse.Data) || utils.IsOKReplacingEOF(resultSet.FinalResponse.Data)) {
		if logger.Core().Enabled(zap.DebugLevel) {
			logger.Debug("Writing final response packet for BinaryProtocolResultSet",
				zap.Int("final_response_length", len(resultSet.FinalResponse.Data)),
				zap.String("final_response_hex", fmt.Sprintf("%x", resultSet.FinalResponse.Data)),
				zap.Bool("is_ok_replacing_eof", utils.IsOKReplacingEOF(resultSet.FinalResponse.Data)))
		}
		if _, err := buf.Write(resultSet.FinalResponse.Data); err != nil {
			return nil, fmt.Errorf("failed to write final response packet: %w", err)
		}
	}

	return buf.Bytes(), nil
}
