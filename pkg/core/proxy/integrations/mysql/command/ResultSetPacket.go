//go:build linux

package command

import (
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
