//go:build linux

package replayer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/xwb1989/sqlparser"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/wire"
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

func matchSSLRequest(_ context.Context, _ *zap.Logger, expected, actual mysql.PacketBundle) error {
	// Match the type
	if expected.Header.Type != actual.Header.Type {
		return fmt.Errorf("type mismatch for ssl request")
	}

	//Don't match the header, because the payload length can be different.

	// Match the payload
	expectedMessage, _ := expected.Message.(*mysql.SSLRequestPacket)
	actualMessage, _ := actual.Message.(*mysql.SSLRequestPacket)

	// Match the MaxPacketSize
	if expectedMessage.MaxPacketSize != actualMessage.MaxPacketSize {
		return fmt.Errorf("max packet size mismatch for ssl request, expected: %d, actual: %d", expectedMessage.MaxPacketSize, actualMessage.MaxPacketSize)
	}

	// Match the CharacterSet
	if expectedMessage.CharacterSet != actualMessage.CharacterSet {
		return fmt.Errorf("character set mismatch for ssl request, expected: %d, actual: %d", expectedMessage.CharacterSet, actualMessage.CharacterSet)
	}

	// Match the Filler
	if expectedMessage.Filler != actualMessage.Filler {
		return fmt.Errorf("filler mismatch for ssl request, expected: %v, actual: %v", expectedMessage.Filler, actualMessage.Filler)
	}

	return nil
}

func matchHanshakeResponse41(_ context.Context, _ *zap.Logger, expected, actual mysql.PacketBundle) error {
	// Match the type
	if expected.Header.Type != actual.Header.Type {
		return fmt.Errorf("type mismatch for handshake response")
	}

	//Don't match the header, because the payload length can be different.

	// Match the payload

	//Get the packet type from both the packet bundles
	// we don't need to do type assertion because its already done in the caller function

	exp := expected.Message.(*mysql.HandshakeResponse41Packet)
	act := actual.Message.(*mysql.HandshakeResponse41Packet)

	// // Match the CapabilityFlags
	// if exp.CapabilityFlags != act.CapabilityFlags {
	// 	return fmt.Errorf("capability flags mismatch for handshake response, expected: %d, actual: %d", exp.CapabilityFlags, act.CapabilityFlags)
	// }

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
	if !bytes.Equal(exp.AuthResponse, act.AuthResponse) {
		return fmt.Errorf("auth response mismatch for handshake response, expected: %v, actual: %v", exp.AuthResponse, act.AuthResponse)
	}

	// Match the Database
	if exp.Database != act.Database {
		return fmt.Errorf("database mismatch for handshake response, expected: %s, actual: %s", exp.Database, act.Database)
	}

	// Match the AuthPluginName
	if exp.AuthPluginName != act.AuthPluginName {
		return fmt.Errorf("auth plugin name mismatch for handshake response, expected: %s, actual: %s", exp.AuthPluginName, act.AuthPluginName)
	}

	// // Match the ConnectionAttributes
	// if len(exp.ConnectionAttributes) != len(act.ConnectionAttributes) {
	// 	return fmt.Errorf("connection attributes length mismatch for handshake response, expected: %d, actual: %d", len(exp.ConnectionAttributes), len(act.ConnectionAttributes))
	// }

	// for key, value := range exp.ConnectionAttributes {
	// 	if act.ConnectionAttributes[key] != value && key != "_pid" {
	// 		return fmt.Errorf("connection attributes mismatch for handshake response, expected: %s, actual: %s", value, act.ConnectionAttributes[key])
	// 	}
	// }

	// Match the ZstdCompressionLevel
	if exp.ZstdCompressionLevel != act.ZstdCompressionLevel {
		return fmt.Errorf("zstd compression level mismatch for handshake response")
	}

	return nil
}

func matchCommand(ctx context.Context, logger *zap.Logger, req mysql.Request, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) (*mysql.Response, bool, error) {

	for {

		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}

		// Get the tcs mocks from the mockDb
		unfiltered, err := mockDb.GetUnFilteredMocks()
		if err != nil {
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}
			utils.LogError(logger, err, "failed to get unfiltered mocks")
			return nil, false, err
		}

		var tcsMocks []*models.Mock
		var hasMySQLMocks bool
		// Filter for non-config MySQL mocks in a single pass.
		for _, mock := range unfiltered {
			if mock.Kind == models.MySQL {
				hasMySQLMocks = true
				if mock.Spec.Metadata["type"] != "config" {
					tcsMocks = append(tcsMocks, mock)
				}
			}
		}

		if !hasMySQLMocks {
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}
			utils.LogError(logger, nil, "no mysql mocks found")
			return nil, false, fmt.Errorf("no mysql mocks found")
		}

		if len(tcsMocks) == 0 {
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}

			// COM_QUIT packet can be handled separately, because there might be no mock for it
			if req.Header.Type == mysql.CommandStatusToString(mysql.COM_QUIT) {
				// If the command is quit, we should return EOF
				logger.Debug("Received quit command, closing the connection by sending EOF")
				return nil, false, io.EOF
			}

			utils.LogError(logger, nil, "no mysql mocks found for handling command phase")
			return nil, false, fmt.Errorf("no mysql mocks found for handling command phase")
		}

		var maxMatchedCount int
		var matchedResp *mysql.Response
		var matchedMock *models.Mock

		queryMatched := false

		// Match the request with the mock
		for _, mock := range tcsMocks {

			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}

			for _, mockReq := range mock.Spec.MySQLRequests {
				if ctx.Err() != nil {
					return nil, false, ctx.Err()
				}

				switch req.Header.Type {
				//utiltiy commands
				case mysql.CommandStatusToString(mysql.COM_QUIT):
					matchCount := matchQuitPacket(ctx, logger, mockReq.PacketBundle, req.PacketBundle)
					if matchCount > maxMatchedCount {
						maxMatchedCount = matchCount
						matchedResp = &mysql.Response{} // in case if server closed the connection without sending any response
						if len(mock.Spec.MySQLResponses) > 0 {
							matchedResp = &mock.Spec.MySQLResponses[0] // if server responded with "error" packet
						}
						matchedMock = mock
					}
				case mysql.CommandStatusToString(mysql.COM_INIT_DB):
					matchCount := matchInitDbPacket(ctx, logger, mockReq.PacketBundle, req.PacketBundle)
					if matchCount > maxMatchedCount {
						maxMatchedCount = matchCount
						matchedResp = &mock.Spec.MySQLResponses[0]
						matchedMock = mock
					}
				case mysql.CommandStatusToString(mysql.COM_STATISTICS):
					matchCount := matchStatisticsPacket(ctx, logger, mockReq.PacketBundle, req.PacketBundle)
					if matchCount > maxMatchedCount {
						maxMatchedCount = matchCount
						matchedResp = &mock.Spec.MySQLResponses[0]
						matchedMock = mock
					}
				case mysql.CommandStatusToString(mysql.COM_DEBUG):
					matchCount := matchDebugPacket(ctx, logger, mockReq.PacketBundle, req.PacketBundle)
					if matchCount > maxMatchedCount {
						maxMatchedCount = matchCount
						matchedResp = &mock.Spec.MySQLResponses[0]
						matchedMock = mock
					}
				case mysql.CommandStatusToString(mysql.COM_PING):
					matchCount := matchPingPacket(ctx, logger, mockReq.PacketBundle, req.PacketBundle)
					if matchCount > maxMatchedCount {
						maxMatchedCount = matchCount
						matchedResp = &mock.Spec.MySQLResponses[0]
						matchedMock = mock
					}
				// case mysql.CommandStatusToString(mysql.COM_CHANGE_USER):
				case mysql.CommandStatusToString(mysql.COM_RESET_CONNECTION):
					matchCount := matchResetConnectionPacket(ctx, logger, mockReq.PacketBundle, req.PacketBundle)
					if matchCount > maxMatchedCount {
						maxMatchedCount = matchCount
						matchedResp = &mock.Spec.MySQLResponses[0]
						matchedMock = mock
					}

				//query commands
				case mysql.CommandStatusToString(mysql.COM_STMT_CLOSE):
					matchCount := matchClosePacket(ctx, logger, mockReq.PacketBundle, req.PacketBundle)
					if matchCount > maxMatchedCount {
						maxMatchedCount = matchCount
						matchedResp = &mysql.Response{}
						matchedMock = mock
					}
				// case mysql.CommandStatusToString(mysql.COM_STMT_SEND_LONG_DATA):
				case mysql.CommandStatusToString(mysql.COM_QUERY):
					matched := matchQueryPacket(ctx, logger, mockReq.PacketBundle, req.PacketBundle)
					// logger.Warn("match count is", zap.Int("matchCount", matchCount))
					if matched {
						matchedResp = &mock.Spec.MySQLResponses[0]
						matchedMock = mock
						queryMatched = true
					}

				case mysql.CommandStatusToString(mysql.COM_STMT_PREPARE):
					matchCount := matchPreparePacket(ctx, logger, mockReq.PacketBundle, req.PacketBundle)
					if matchCount > maxMatchedCount {
						maxMatchedCount = matchCount
						matchedResp = &mock.Spec.MySQLResponses[0]
						matchedMock = mock
					}
				case mysql.CommandStatusToString(mysql.COM_STMT_EXECUTE):
					matchCount := matchStmtExecutePacket(ctx, logger, mockReq.PacketBundle, req.PacketBundle)
					if matchCount > maxMatchedCount {
						maxMatchedCount = matchCount
						matchedResp = &mock.Spec.MySQLResponses[0]
						matchedMock = mock
					}
				}
			}
			if queryMatched {
				break
			}
		}
		if matchedResp == nil {
			logger.Debug("No matching mock found for the command", zap.Any("command", req))

			// COM_QUIT packet can be handled separately, because there might be no mock for it
			if req.Header.Type == mysql.CommandStatusToString(mysql.COM_QUIT) {
				// If the command is quit, we should return EOF
				logger.Debug("Received quit command, closing the connection by sending EOF")
				return nil, false, io.EOF
			}

			return nil, false, nil
		}

		//if the req was prepared statement, we should store the prepared statement response
		if req.Header.Type == mysql.CommandStatusToString(mysql.COM_STMT_PREPARE) {
			prepareOkResp, ok := matchedResp.Message.(*mysql.StmtPrepareOkPacket)
			if !ok {
				logger.Error("failed to type assert the StmtPrepareOkPacket")
				return nil, false, fmt.Errorf("failed to type assert the StmtPrepareOkPacket")
			}
			// This prepared statement will be used in the further execute statement packets
			decodeCtx.PreparedStatements[prepareOkResp.StatementID] = prepareOkResp
		}

		// Delete the matched mock from the mockDb

		okk := updateMock(ctx, logger, matchedMock, mockDb)
		if !okk {
			//TODO: see what to do in case of failed deletion
			logger.Debug("failed to update the matched mock")
			continue
		}
		return matchedResp, true, nil
	}
}

func matchClosePacket(_ context.Context, _ *zap.Logger, expected, actual mysql.PacketBundle) int {
	matchCount := 0
	// Match the type and return zero if the types are not equal
	if expected.Header.Type != actual.Header.Type {
		return 0
	}
	// Match the header
	ok := matchHeader(*expected.Header.Header, *actual.Header.Header)
	if ok {
		matchCount += 2
	}
	expectedMessage, _ := expected.Message.(*mysql.StmtClosePacket)
	actualMessage, _ := actual.Message.(*mysql.StmtClosePacket)
	// Match the statementID
	if expectedMessage.StatementID == actualMessage.StatementID {
		matchCount++
	}
	return matchCount
}

func getQueryStructure(sql string) (string, error) {
	stmt, err := sqlparser.Parse(sql)
	if err != nil {
		return "", fmt.Errorf("failed to parse SQL: %w", err)
	}

	var structureParts []string
	// Walk the AST and collect the Go type of each grammatical node.
	err = sqlparser.Walk(func(node sqlparser.SQLNode) (bool, error) {
		structureParts = append(structureParts, reflect.TypeOf(node).String())
		return true, nil
	}, stmt)

	if err != nil {
		return "", fmt.Errorf("failed to walk the AST: %w", err)
	}

	return strings.Join(structureParts, "->"), nil
}

func matchQueryPacket(_ context.Context, log *zap.Logger, expected, actual mysql.PacketBundle) bool {
	// Match the type and return zero if the types are not equal
	if expected.Header.Type != actual.Header.Type {
		return false
	}

	expectedMessage, _ := expected.Message.(*mysql.QueryPacket)
	actualMessage, _ := actual.Message.(*mysql.QueryPacket)

	if actual.Header.Header.PayloadLength == expected.Header.Header.PayloadLength {
		if expectedMessage.Query == actualMessage.Query {
			return true
		}
	}

	expectedSignature, err := getQueryStructure(expectedMessage.Query)
	if err != nil {
		log.Error("failed to get expected query structure", zap.Error(err))
		return false
	}

	actualSignature, err := getQueryStructure(actualMessage.Query)
	if err != nil {
		log.Error("failed to get actual query structure", zap.Error(err))
		return false
	}

	if expectedSignature == actualSignature {
		log.Debug("query structure matched", zap.String("expected signature", expectedSignature),
			zap.String("actual signature", actualSignature))
		return true
	}

	return false
}

func matchPreparePacket(_ context.Context, _ *zap.Logger, expected, actual mysql.PacketBundle) int {
	matchCount := 0
	// Match the type and return zero if the types are not equal
	if expected.Header.Type != actual.Header.Type {
		return 0
	}
	// Match the header
	ok := matchHeader(*expected.Header.Header, *actual.Header.Header)
	if ok {
		matchCount += 2
	}
	expectedMessage, _ := expected.Message.(*mysql.StmtPreparePacket)
	actualMessage, _ := actual.Message.(*mysql.StmtPreparePacket)

	// Match the query for prepare packet
	if expectedMessage.Query == actualMessage.Query {
		matchCount++
	}
	return matchCount
}

func matchStmtExecutePacket(_ context.Context, _ *zap.Logger, expected, actual mysql.PacketBundle) int {
	matchCount := 0

	// Match the type and return zero if the types are not equal
	if expected.Header.Type != actual.Header.Type {
		return 0
	}
	// Match the header
	if matchHeader(*expected.Header.Header, *actual.Header.Header) {
		matchCount += 2
	}
	expectedMessage, _ := expected.Message.(*mysql.StmtExecutePacket)
	actualMessage, _ := actual.Message.(*mysql.StmtExecutePacket)
	// Match the status
	if expectedMessage.Status == actualMessage.Status {
		matchCount++
	}
	// Match the statementID
	if expectedMessage.StatementID == actualMessage.StatementID {
		matchCount++
	}
	// Match the flags
	if expectedMessage.Flags == actualMessage.Flags {
		matchCount++
	}
	// Match the iteration count
	if expectedMessage.IterationCount == actualMessage.IterationCount {
		matchCount++
	}
	// Match the parameter count
	if expectedMessage.ParameterCount == actualMessage.ParameterCount {
		matchCount++
	}

	// Match the newParamsBindFlag
	if expectedMessage.NewParamsBindFlag == actualMessage.NewParamsBindFlag {
		matchCount++
	}

	// Match the parameters
	if len(expectedMessage.Parameters) == len(actualMessage.Parameters) {
		for i := range expectedMessage.Parameters {
			expectedParam := expectedMessage.Parameters[i]
			actualParam := actualMessage.Parameters[i]
			if expectedParam.Type == actualParam.Type &&
				expectedParam.Name == actualParam.Name &&
				expectedParam.Unsigned == actualParam.Unsigned &&
				reflect.DeepEqual(expectedParam.Value, actualParam.Value) {
				matchCount++
			}
		}
	}

	return matchCount
}

// matching for utility commands
func matchQuitPacket(_ context.Context, _ *zap.Logger, expected, actual mysql.PacketBundle) int {
	matchCount := 0
	// Match the type and return zero if the types are not equal
	if expected.Header.Type != actual.Header.Type {
		return 0
	}
	// Match the header
	if matchHeader(*expected.Header.Header, *actual.Header.Header) {
		matchCount += 2
	}
	expectedMessage, _ := expected.Message.(*mysql.QuitPacket)
	actualMessage, _ := actual.Message.(*mysql.QuitPacket)
	// Match the command for quit packet
	if expectedMessage.Command == actualMessage.Command {
		matchCount++
	}
	return matchCount
}

func matchInitDbPacket(_ context.Context, _ *zap.Logger, expected, actual mysql.PacketBundle) int {
	matchCount := 0
	// Match the type and return zero if the types are not equal
	if expected.Header.Type != actual.Header.Type {
		return 0
	}
	// Match the header
	if matchHeader(*expected.Header.Header, *actual.Header.Header) {
		matchCount += 2
	}
	expectedMessage, _ := expected.Message.(*mysql.InitDBPacket)
	actualMessage, _ := actual.Message.(*mysql.InitDBPacket)
	// Match the command for init db packet
	if expectedMessage.Command == actualMessage.Command {
		matchCount++
	}
	// Match the schema for init db packet
	if expectedMessage.Schema == actualMessage.Schema {
		matchCount++
	}
	return matchCount
}

func matchStatisticsPacket(_ context.Context, _ *zap.Logger, expected, actual mysql.PacketBundle) int {
	matchCount := 0
	// Match the type and return zero if the types are not equal
	if expected.Header.Type != actual.Header.Type {
		return 0
	}
	// Match the header
	if matchHeader(*expected.Header.Header, *actual.Header.Header) {
		matchCount += 2
	}
	expectedMessage, _ := expected.Message.(*mysql.StatisticsPacket)
	actualMessage, _ := actual.Message.(*mysql.StatisticsPacket)
	// Match the command for statistics packet
	if expectedMessage.Command == actualMessage.Command {
		matchCount++
	}
	return matchCount
}

func matchDebugPacket(_ context.Context, _ *zap.Logger, expected, actual mysql.PacketBundle) int {
	matchCount := 0
	// Match the type and return zero if the types are not equal
	if expected.Header.Type != actual.Header.Type {
		return 0
	}
	// Match the header
	if matchHeader(*expected.Header.Header, *actual.Header.Header) {
		matchCount += 2
	}
	expectedMessage, _ := expected.Message.(*mysql.DebugPacket)
	actualMessage, _ := actual.Message.(*mysql.DebugPacket)
	// Match the command for debug packet
	if expectedMessage.Command == actualMessage.Command {
		matchCount++
	}
	return matchCount
}

func matchPingPacket(_ context.Context, _ *zap.Logger, expected, actual mysql.PacketBundle) int {
	matchCount := 0
	// Match the type and return zero if the types are not equal
	if expected.Header.Type != actual.Header.Type {
		return 0
	}
	// Match the header
	if matchHeader(*expected.Header.Header, *actual.Header.Header) {
		matchCount += 2
	}
	expectedMessage, _ := expected.Message.(*mysql.PingPacket)
	actualMessage, _ := actual.Message.(*mysql.PingPacket)
	// Match the command for ping packet
	if expectedMessage.Command == actualMessage.Command {
		matchCount++
	}
	return matchCount
}

func matchResetConnectionPacket(_ context.Context, _ *zap.Logger, expected, actual mysql.PacketBundle) int {
	matchCount := 0
	// Match the type and return zero if the types are not equal
	if expected.Header.Type != actual.Header.Type {
		return 0
	}
	// Match the header
	if matchHeader(*expected.Header.Header, *actual.Header.Header) {
		matchCount += 2
	}
	expectedMessage, _ := expected.Message.(*mysql.ResetConnectionPacket)
	actualMessage, _ := actual.Message.(*mysql.ResetConnectionPacket)
	// Match the command for reset connection packet
	if expectedMessage.Command == actualMessage.Command {
		matchCount++
	}
	return matchCount
}

// The same function is used in http parser as well, If you find this useful you can extract it to a common package
// and delete the duplicate code.
// updateMock processes the matched mock based on its filtered status.
func updateMock(_ context.Context, logger *zap.Logger, matchedMock *models.Mock, mockDb integrations.MockMemDb) bool {
	originalMatchedMock := *matchedMock
	matchedMock.TestModeInfo.IsFiltered = false
	matchedMock.TestModeInfo.SortOrder = pkg.GetNextSortNum()
	updated := mockDb.UpdateUnFilteredMock(&originalMatchedMock, matchedMock)
	if !updated {
		logger.Debug("failed to update matched mock")
	}
	return updated
}
