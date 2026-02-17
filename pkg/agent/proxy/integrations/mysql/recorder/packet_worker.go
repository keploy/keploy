package recorder

import (
	"context"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query/rowscols"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// ProcessRawMocks consumes mocks from rawMocks channel, decodes any raw column
// and row data, and sends the fully processed mocks to the output channel.
// This is the async worker that handles all heavy decode work off the hot path.
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

		// Iterate through responses to process raw data
		for i := range mock.Spec.MySQLResponses {
			resp := &mock.Spec.MySQLResponses[i]
			bundle := &resp.PacketBundle

			// Check if Message is TextResultSet
			if textRes, ok := bundle.Message.(*mysql.TextResultSet); ok {
				// Decode columns from raw data first (rows depend on columns)
				if len(textRes.RawColumnData) > 0 {
					for _, data := range textRes.RawColumnData {
						func() {
							defer func() {
								if r := recover(); r != nil {
									logger.Error("Recovered from panic during text column decoding", zap.Any("panic", r))
								}
							}()
							column, _, err := rowscols.DecodeColumn(ctx, logger, data)
							if err != nil {
								logger.Error("failed to decode column in async worker", zap.Error(err))
							} else {
								textRes.Columns = append(textRes.Columns, column)
							}
						}()
					}
					// Clear raw column data to free memory
					textRes.RawColumnData = nil
				}

				// Set EOF after columns from raw data
				if len(textRes.RawEOFAfterColumns) > 0 {
					textRes.EOFAfterColumns = textRes.RawEOFAfterColumns
					textRes.RawEOFAfterColumns = nil
				}

				// Decode raw rows (depends on columns being decoded first)
				if len(textRes.RawRowData) > 0 {
					for _, data := range textRes.RawRowData {
						func() {
							defer func() {
								if r := recover(); r != nil {
									logger.Error("Recovered from panic during text row decoding", zap.Any("panic", r))
								}
							}()
							row, _, err := rowscols.DecodeTextRow(ctx, logger, data, textRes.Columns)
							if err != nil {
								logger.Error("failed to decode text row in async worker", zap.Error(err))
							} else {
								textRes.Rows = append(textRes.Rows, row)
							}
						}()
					}
					// Clear raw data to free memory
					textRes.RawRowData = nil
				}
			}

			// Check if Message is BinaryProtocolResultSet
			if binRes, ok := bundle.Message.(*mysql.BinaryProtocolResultSet); ok {
				// Decode columns from raw data first (rows depend on columns)
				if len(binRes.RawColumnData) > 0 {
					for _, data := range binRes.RawColumnData {
						func() {
							defer func() {
								if r := recover(); r != nil {
									logger.Error("Recovered from panic during binary column decoding", zap.Any("panic", r))
								}
							}()
							column, _, err := rowscols.DecodeColumn(ctx, logger, data)
							if err != nil {
								logger.Error("failed to decode column in async worker", zap.Error(err))
							} else {
								binRes.Columns = append(binRes.Columns, column)
							}
						}()
					}
					// Clear raw column data to free memory
					binRes.RawColumnData = nil
				}

				// Set EOF after columns from raw data
				if len(binRes.RawEOFAfterColumns) > 0 {
					binRes.EOFAfterColumns = binRes.RawEOFAfterColumns
					binRes.RawEOFAfterColumns = nil
				}

				// Decode raw rows (depends on columns being decoded first)
				if len(binRes.RawRowData) > 0 {
					for _, data := range binRes.RawRowData {
						func() {
							defer func() {
								if r := recover(); r != nil {
									logger.Error("Recovered from panic during binary row decoding", zap.Any("panic", r))
								}
							}()
							row, _, err := rowscols.DecodeBinaryRow(ctx, logger, data, binRes.Columns)
							if err != nil {
								logger.Error("failed to decode binary row in async worker", zap.Error(err))
							} else {
								binRes.Rows = append(binRes.Rows, row)
							}
						}()
					}
					// Clear raw data to free memory
					binRes.RawRowData = nil
				}
			}

			// Check if Message is StmtPrepareOkPacket
			if prepRes, ok := bundle.Message.(*mysql.StmtPrepareOkPacket); ok {
				// Decode parameter definitions from raw data
				if len(prepRes.RawParamData) > 0 {
					for _, data := range prepRes.RawParamData {
						func() {
							defer func() {
								if r := recover(); r != nil {
									logger.Error("Recovered from panic during param def decoding", zap.Any("panic", r))
								}
							}()
							column, _, err := rowscols.DecodeColumn(ctx, logger, data)
							if err != nil {
								logger.Error("failed to decode param def in async worker", zap.Error(err))
							} else {
								prepRes.ParamDefs = append(prepRes.ParamDefs, column)
							}
						}()
					}
					prepRes.RawParamData = nil
				}

				// Set EOF after param defs
				if len(prepRes.RawEOFAfterParamDefs) > 0 {
					prepRes.EOFAfterParamDefs = prepRes.RawEOFAfterParamDefs
					prepRes.RawEOFAfterParamDefs = nil
				}

				// Decode column definitions from raw data
				if len(prepRes.RawColumnDefData) > 0 {
					for _, data := range prepRes.RawColumnDefData {
						func() {
							defer func() {
								if r := recover(); r != nil {
									logger.Error("Recovered from panic during column def decoding", zap.Any("panic", r))
								}
							}()
							column, _, err := rowscols.DecodeColumn(ctx, logger, data)
							if err != nil {
								logger.Error("failed to decode column def in async worker", zap.Error(err))
							} else {
								prepRes.ColumnDefs = append(prepRes.ColumnDefs, column)
							}
						}()
					}
					prepRes.RawColumnDefData = nil
				}

				// Set EOF after column defs
				if len(prepRes.RawEOFAfterColumnDefs) > 0 {
					prepRes.EOFAfterColumnDefs = prepRes.RawEOFAfterColumnDefs
					prepRes.RawEOFAfterColumnDefs = nil
				}
			}
		}

		// Send processing mock to final channel
		select {
		case <-ctx.Done():
			return
		case finalMocks <- mock:
		}
	}
}
