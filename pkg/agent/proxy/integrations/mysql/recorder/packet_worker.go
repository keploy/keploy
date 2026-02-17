package recorder

import (
	"context"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query/rowscols"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// ProcessRawMocks consumes mocks from rawMocks channel, decodes any raw row data,
// and sends the fully processed mocks to the output channel.
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

		// Iterate through responses to check for RawRowData
		for i := range mock.Spec.MySQLResponses {
			resp := &mock.Spec.MySQLResponses[i]
			bundle := &resp.PacketBundle

			// Check if Message is TextResultSet
			if textRes, ok := bundle.Message.(*mysql.TextResultSet); ok {
				if len(textRes.RawRowData) > 0 {
					// Decode raw rows
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
				if len(binRes.RawRowData) > 0 {
					// Decode raw rows
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
		}

		// Send processing mock to final channel
		select {
		case <-ctx.Done():
			return
		case finalMocks <- mock:
		}
	}
}
