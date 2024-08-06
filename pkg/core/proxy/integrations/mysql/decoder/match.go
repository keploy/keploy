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

func matchHeader(expected, actual mysql.Header) bool {

	// Match the payloadlength
	if actual.PayloadLength != expected.PayloadLength {
		return false
	}

	// Match the sequence number
	if actual.SequenceID != expected.SequenceID {
		return false
	}

	return true
}

func matchHanshakeResponse41(_ context.Context, _ *zap.Logger, expected, actual mysql.PacketBundle) error {
	// Match the type
	if expected.Header.Type != actual.Header.Type {
		return fmt.Errorf("type mismatch for handshake response")
	}

	// Match the header
	ok := matchHeader(*expected.Header.Header, *actual.Header.Header)
	if !ok {
		return fmt.Errorf("header mismatch for handshake response")
	}

	// Match the payload

	return nil
}

func matchEncryptedPassword(expected, actual mysql.Packet) error {

	ok := matchHeader(expected.Header, actual.Header)
	if !ok {
		return fmt.Errorf("header mismatch for encrypted password")
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
