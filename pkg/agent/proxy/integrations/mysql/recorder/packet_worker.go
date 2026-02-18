package recorder

import (
	"context"

	mysqlUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query/rowscols"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// ProcessRawMocks is the async worker that handles all heavy decode work off the hot path.
func ProcessRawMocks(ctx context.Context, logger *zap.Logger, rawMocks <-chan *models.Mock, finalMocks chan<- *models.Mock) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Recovered from panic in ProcessRawMocks", zap.Any("panic", r))
		}
	}()

	for mock := range rawMocks {
		if mock == nil {
			continue
		}

		if err := processMock(ctx, logger, mock); err != nil {
			logger.Error("failed to process mock in async worker", zap.Error(err))
			// Continue will drop the mock if processing failed catastrophically at top level.
			// However, our processMock now handles individual row failures gracefully.
		}

		// Send processed mock (even if partially failed) to final channel
		select {
		case <-ctx.Done():
			return
		case finalMocks <- mock:
		}
	}
}

func processMock(ctx context.Context, logger *zap.Logger, mock *models.Mock) (err error) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Recovered from panic in processMock", zap.Any("panic", r))
			err = getRecoverError(r)
		}
	}()

	// Iterate through responses to process raw data
	for i := range mock.Spec.MySQLResponses {
		resp := &mock.Spec.MySQLResponses[i]
		bundle := &resp.PacketBundle

		// Check if Message is TextResultSet
		if textRes, ok := bundle.Message.(*mysql.TextResultSet); ok {
			processTextResultSet(ctx, logger, textRes)
		}

		// Check if Message is BinaryProtocolResultSet
		if binRes, ok := bundle.Message.(*mysql.BinaryProtocolResultSet); ok {
			processBinaryResultSet(ctx, logger, binRes)
		}

		// Check if Message is StmtPrepareOkPacket
		if prepRes, ok := bundle.Message.(*mysql.StmtPrepareOkPacket); ok {
			processStmtPrepareOk(ctx, logger, prepRes)
		}
	}
	return nil
}

func processTextResultSet(ctx context.Context, logger *zap.Logger, textRes *mysql.TextResultSet) {
	// Decode columns from raw data first (rows depend on columns)
	if len(textRes.RawColumnData) > 0 {
		textRes.Columns = make([]*mysql.ColumnDefinition41, 0, len(textRes.RawColumnData))
		for _, data := range textRes.RawColumnData {
			col := safeDecodeColumn(ctx, logger, data)
			if col != nil {
				textRes.Columns = append(textRes.Columns, col)
			}
			// Free the pooled buffer
			mysqlUtils.PutPacketBuffer(data)
		}
		textRes.RawColumnData = nil
	}

	// Set EOF after columns from raw data
	if len(textRes.RawEOFAfterColumns) > 0 {
		textRes.EOFAfterColumns = textRes.RawEOFAfterColumns
		// Free the pooled buffer
		mysqlUtils.PutPacketBuffer(textRes.RawEOFAfterColumns)
		textRes.RawEOFAfterColumns = nil
	}

	// Decode raw rows (depends on columns being decoded first)
	if len(textRes.RawRowData) > 0 {
		textRes.Rows = make([]*mysql.TextRow, 0, len(textRes.RawRowData))
		for _, data := range textRes.RawRowData {
			row := safeDecodeTextRow(ctx, logger, data, textRes.Columns)
			if row != nil {
				textRes.Rows = append(textRes.Rows, row)
			}
			// Free the pooled buffer
			mysqlUtils.PutPacketBuffer(data)
		}
		textRes.RawRowData = nil
	}
}

func processBinaryResultSet(ctx context.Context, logger *zap.Logger, binRes *mysql.BinaryProtocolResultSet) {
	// Decode columns from raw data first
	if len(binRes.RawColumnData) > 0 {
		binRes.Columns = make([]*mysql.ColumnDefinition41, 0, len(binRes.RawColumnData))
		for _, data := range binRes.RawColumnData {
			col := safeDecodeColumn(ctx, logger, data)
			if col != nil {
				binRes.Columns = append(binRes.Columns, col)
			}
			// Free the pooled buffer
			mysqlUtils.PutPacketBuffer(data)
		}
		binRes.RawColumnData = nil
	}

	// Set EOF after columns
	if len(binRes.RawEOFAfterColumns) > 0 {
		binRes.EOFAfterColumns = binRes.RawEOFAfterColumns
		// Free the pooled buffer
		mysqlUtils.PutPacketBuffer(binRes.RawEOFAfterColumns)
		binRes.RawEOFAfterColumns = nil
	}

	// Decode raw rows
	if len(binRes.RawRowData) > 0 {
		binRes.Rows = make([]*mysql.BinaryRow, 0, len(binRes.RawRowData))
		for _, data := range binRes.RawRowData {
			row := safeDecodeBinaryRow(ctx, logger, data, binRes.Columns)
			if row != nil {
				binRes.Rows = append(binRes.Rows, row)
			}
			// Free the pooled buffer
			mysqlUtils.PutPacketBuffer(data)
		}
		binRes.RawRowData = nil
	}
}

func processStmtPrepareOk(ctx context.Context, logger *zap.Logger, prepRes *mysql.StmtPrepareOkPacket) {
	// Decode parameter definitions
	if len(prepRes.RawParamData) > 0 {
		prepRes.ParamDefs = make([]*mysql.ColumnDefinition41, 0, len(prepRes.RawParamData))
		for _, data := range prepRes.RawParamData {
			col := safeDecodeColumn(ctx, logger, data)
			if col != nil {
				prepRes.ParamDefs = append(prepRes.ParamDefs, col)
			}
			// Free the pooled buffer
			mysqlUtils.PutPacketBuffer(data)
		}
		prepRes.RawParamData = nil
	}

	if len(prepRes.RawEOFAfterParamDefs) > 0 {
		prepRes.EOFAfterParamDefs = prepRes.RawEOFAfterParamDefs
		// Free the pooled buffer
		mysqlUtils.PutPacketBuffer(prepRes.RawEOFAfterParamDefs)
		prepRes.RawEOFAfterParamDefs = nil
	}

	// Decode column definitions
	if len(prepRes.RawColumnDefData) > 0 {
		prepRes.ColumnDefs = make([]*mysql.ColumnDefinition41, 0, len(prepRes.RawColumnDefData))
		for _, data := range prepRes.RawColumnDefData {
			col := safeDecodeColumn(ctx, logger, data)
			if col != nil {
				prepRes.ColumnDefs = append(prepRes.ColumnDefs, col)
			}
			// Free the pooled buffer
			mysqlUtils.PutPacketBuffer(data)
		}
		prepRes.RawColumnDefData = nil
	}

	if len(prepRes.RawEOFAfterColumnDefs) > 0 {
		prepRes.EOFAfterColumnDefs = prepRes.RawEOFAfterColumnDefs
		// Free the pooled buffer
		mysqlUtils.PutPacketBuffer(prepRes.RawEOFAfterColumnDefs)
		prepRes.RawEOFAfterColumnDefs = nil
	}
}

// safeDecodeColumn wraps DecodeColumn with panic recovery.
func safeDecodeColumn(ctx context.Context, logger *zap.Logger, data []byte) *mysql.ColumnDefinition41 {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Recovered from panic during column decoding", zap.Any("panic", r))
		}
	}()
	col, _, err := rowscols.DecodeColumn(ctx, logger, data)
	if err != nil {
		logger.Error("failed to decode column", zap.Error(err))
		return nil
	}
	return col
}

// safeDecodeTextRow wraps DecodeTextRow with panic recovery.
func safeDecodeTextRow(ctx context.Context, logger *zap.Logger, data []byte, columns []*mysql.ColumnDefinition41) *mysql.TextRow {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Recovered from panic during text row decoding", zap.Any("panic", r))
		}
	}()
	row, _, err := rowscols.DecodeTextRow(ctx, logger, data, columns)
	if err != nil {
		logger.Error("failed to decode text row", zap.Error(err))
		return nil
	}
	return row
}

// safeDecodeBinaryRow wraps DecodeBinaryRow with panic recovery.
func safeDecodeBinaryRow(ctx context.Context, logger *zap.Logger, data []byte, columns []*mysql.ColumnDefinition41) *mysql.BinaryRow {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Recovered from panic during binary row decoding", zap.Any("panic", r))
		}
	}()
	row, _, err := rowscols.DecodeBinaryRow(ctx, logger, data, columns)
	if err != nil {
		logger.Error("failed to decode binary row", zap.Error(err))
		return nil
	}
	return row
}

func getRecoverError(r interface{}) error {
	if err, ok := r.(error); ok {
		return err
	}
	return nil
}
