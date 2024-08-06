//go:build linux

package decoder

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/operation"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.uber.org/zap"
)

// func matchHanshakeResponse41(ctx context.Context, logger *zap.Logger, expected, actual mysql.Packet) (bool, error) {
// 	// Match the payloadlength
// 	if actual.Header.PayloadLength != expected.Header.PayloadLength {
// 		return false, fmt.Errorf("payload length mismatch for handshake response41")
// 	}

// 	// Match the sequence number
// 	if actual.Header.SequenceID != expected.Header.SequenceID {
// 		return fmt.Errorf("sequence number mismatch for handshake response41")
// 	}

// 	// Match the payload

// 	return nil
// }

func matchEncryptedPassword(expected, actual mysql.Packet) error {

	// Match the payloadlength
	if actual.Header.PayloadLength != expected.Header.PayloadLength {
		return fmt.Errorf("payload length mismatch for encrypted password")
	}

	// Match the sequence number
	if actual.Header.SequenceID != expected.Header.SequenceID {
		return fmt.Errorf("sequence number mismatch for encrypted password")
	}

	// Match the payload
	// first convert the actual payload to base64 since the expected payload is in base64
	actualPayload := base64.StdEncoding.EncodeToString(actual.Payload)
	if actualPayload != string(expected.Payload) {
		return fmt.Errorf("payload mismatch for encrypted password")
	}
	return nil
} 

func matchCommand(ctx context.Context, logger *zap.Logger, req mysql.Request, mockDb integrations.MockMemDb, decodeCtx *operation.DecodeContext) (*mysql.Response, bool, error) {
	logger.Info("implementing matchCommand")
	return nil, false, nil
}

// The same function is used in http parser as well, If you find this useful you can extract it to a common package
// and delete the duplicate code.
// updateMock processes the matched mock based on its filtered status.
func updateMock(_ context.Context, logger *zap.Logger, matchedMock *models.Mock, mockDb integrations.MockMemDb) bool {
	if matchedMock.TestModeInfo.IsFiltered {
		originalMatchedMock := *matchedMock
		matchedMock.TestModeInfo.IsFiltered = false
		matchedMock.TestModeInfo.SortOrder = math.MaxInt
		//UpdateUnFilteredMock also marks the mock as used
		updated := mockDb.UpdateUnFilteredMock(&originalMatchedMock, matchedMock)
		return updated
	}

	// we don't update the mock if the IsFiltered is false
	err := mockDb.FlagMockAsUsed(*matchedMock)
	if err != nil {
		logger.Error("failed to flag mock as used", zap.Error(err))
	}

	return true
}
