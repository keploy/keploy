//go:build linux

package decoder

import (
	"context"
	"fmt"
	"math"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/operation"
	intgUtil "go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.keploy.io/server/v2/utils"
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

	//Get the packet type from both the packet bundles
	// we don't need to do type assertion because its already done in the caller function

	exp := expected.Message.(*mysql.HandshakeResponse41Packet)
	act := actual.Message.(*mysql.HandshakeResponse41Packet)

	// Match the CapabilityFlags
	if exp.CapabilityFlags != act.CapabilityFlags {
		return fmt.Errorf("capability flags mismatch for handshake response, expected: %d, actual: %d", exp.CapabilityFlags, act.CapabilityFlags)
	}

	// Match the MaxPacketSize
	if exp.MaxPacketSize != act.MaxPacketSize {
		return fmt.Errorf("max packet size mismatch for handshake response, expected: %d, actual: %d", exp.MaxPacketSize, act.MaxPacketSize)
	}

	// Match the CharacterSet
	if exp.CharacterSet != act.CharacterSet {
		return fmt.Errorf("character set mismatch for handshake response, expected: %d, actual: %d", exp.CharacterSet, act.CharacterSet)
	}

	// Match the Filler
	if exp.Filler != act.Filler {
		return fmt.Errorf("filler mismatch for handshake response, expected: %v, actual: %v", exp.Filler, act.Filler)
	}

	// Match the Username
	if exp.Username != act.Username {
		return fmt.Errorf("username mismatch for handshake response, expected: %s, actual: %s", exp.Username, act.Username)
	}

	// Match the AuthResponse
	if string(exp.AuthResponse) != string(act.AuthResponse) {
		return fmt.Errorf("auth response mismatch for handshake response, expected: %s, actual: %s", string(exp.AuthResponse), string(act.AuthResponse))
	}

	// Match the Database
	if exp.Database != act.Database {
		return fmt.Errorf("database mismatch for handshake response, expected: %s, actual: %s", exp.Database, act.Database)
	}

	// Match the AuthPluginName
	if exp.AuthPluginName != act.AuthPluginName {
		return fmt.Errorf("auth plugin name mismatch for handshake response, expected: %s, actual: %s", exp.AuthPluginName, act.AuthPluginName)
	}

	// Match the ConnectionAttributes
	if len(exp.ConnectionAttributes) != len(act.ConnectionAttributes) {
		return fmt.Errorf("connection attributes length mismatch for handshake response, expected: %d, actual: %d", len(exp.ConnectionAttributes), len(act.ConnectionAttributes))
	}

	for key, value := range exp.ConnectionAttributes {
		if act.ConnectionAttributes[key] != value {
			return fmt.Errorf("connection attributes mismatch for handshake response, expected: %s, actual: %s", value, act.ConnectionAttributes[key])
		}
	}

	// Match the ZstdCompressionLevel
	if exp.ZstdCompressionLevel != act.ZstdCompressionLevel {
		return fmt.Errorf("zstd compression level mismatch for handshake response")
	}

	return nil
}

func matchCommand(ctx context.Context, logger *zap.Logger, req mysql.Request, mockDb integrations.MockMemDb, decodeCtx *operation.DecodeContext) (*mysql.Response, bool, error) {

	// Get the tcs mocks from the mockDb
	mocks, err := mockDb.GetFilteredMocks()
	if err != nil {
		utils.LogError(logger, err, "failed to get filtered mocks")
		return nil, false, err
	}

	// Get the mysql mocks
	tcsMocks := intgUtil.GetMockByKind(mocks, "MySQL")

	if len(tcsMocks) == 0 {
		utils.LogError(logger, nil, "no mysql mocks found")
		return nil, false, fmt.Errorf("no mysql mocks found")
	}

	// Match the request with the mock
	for _, mock := range tcsMocks {

		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		println("mock: ", mock)
		return nil, false, nil
	}

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
