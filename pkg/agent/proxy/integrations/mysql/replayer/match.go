package replayer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"vitess.io/vitess/go/vt/sqlparser"
)

var querySigCache sync.Map // map[string]string

// recorded PREP registry per recorded connection
type prepEntry struct { // minimal, enough for lookup
	statementID uint32
	query       string
	mockName    string // for debugging purpose
}

// case-insensitive prefix check without allocation
func hasPrefixFold(s, p string) bool {
	if len(s) < len(p) {
		return false
	}
	return strings.EqualFold(s[:len(p)], p)
}

func getQueryStructureCached(sql string) (string, error) {
	if v, ok := querySigCache.Load(sql); ok {
		return v.(string), nil
	}
	sig, err := getQueryStructure(sql)
	if err == nil {
		querySigCache.Store(sql, sig)
	}
	return sig, err
}

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

	// DO NOT compare AuthResponse (salt-dependent)
	// if !bytes.Equal(exp.AuthResponse, act.AuthResponse) {
	// 	return fmt.Errorf("auth response mismatch for handshake response, expected: %v, actual: %v", exp.AuthResponse, act.AuthResponse)
	// }

	// Match the Database (backward-compatible: ignore old mocks with junk bytes / off-by-one)
	if !dbEqualCompat(exp.Database, act.Database) {
		return fmt.Errorf("database mismatch for handshake response, expected: %s, actual: %s", printable(exp.Database), printable(act.Database))
	}

	// Match the AuthPluginName (tolerate unknown/garbled plugin names in old mocks)
	if !pluginEqualCompat(exp.AuthPluginName, act.AuthPluginName) {
		return fmt.Errorf("auth plugin name mismatch for handshake response, expected: %s, actual: %s", printable(exp.AuthPluginName), printable(act.AuthPluginName))
	}

	// Match the ZstdCompressionLevel
	if exp.ZstdCompressionLevel != act.ZstdCompressionLevel {
		return fmt.Errorf("zstd compression level mismatch for handshake response")
	}

	return nil
}

func matchCommand(ctx context.Context, logger *zap.Logger, req mysql.Request, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) (*mysql.Response, bool, error) {
	// Precompute string constants once (avoid frequent map lookups)
	var (
		sCOM_QUIT       = mysql.CommandStatusToString(mysql.COM_QUIT)
		sCOM_QUERY      = mysql.CommandStatusToString(mysql.COM_QUERY)
		sCOM_STMT_PREP  = mysql.CommandStatusToString(mysql.COM_STMT_PREPARE)
		sCOM_STMT_EXEC  = mysql.CommandStatusToString(mysql.COM_STMT_EXECUTE)
		sCOM_STMT_CLOSE = mysql.CommandStatusToString(mysql.COM_STMT_CLOSE)
		sCOM_INIT_DB    = mysql.CommandStatusToString(mysql.COM_INIT_DB)
		sCOM_STATS      = mysql.CommandStatusToString(mysql.COM_STATISTICS)
		sCOM_DEBUG      = mysql.CommandStatusToString(mysql.COM_DEBUG)
		sCOM_PING       = mysql.CommandStatusToString(mysql.COM_PING)
		sCOM_RESET_CONN = mysql.CommandStatusToString(mysql.COM_RESET_CONNECTION)
	)

	// Fast path: QUIT may have no mock
	if req.Header.Type == sCOM_QUIT {
		return nil, false, io.EOF
	}

	// Single fetch; no struct copies (see MockManager changes)
	unfiltered, err := mockDb.GetUnFilteredMocks()
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		utils.LogError(logger, err, "failed to get unfiltered mocks")
		return nil, false, err
	}
	if len(unfiltered) == 0 {
		utils.LogError(logger, nil, "no mysql mocks found")
		return nil, false, fmt.Errorf("no mysql mocks found")
	}

	// remove this block
	// get all the mock names that has type com-exec
	stmtMocks := []string{}
	for _, mock := range unfiltered {
		if mock.Kind != models.MySQL {
			continue
		}
		if mock.Spec.Metadata["type"] == "config" {
			continue // command-phase only wants data mocks
		}
		for _, mockReq := range mock.Spec.MySQLRequests {
			if mockReq.PacketBundle.Header.Type == sCOM_STMT_EXEC {
				stmtMocks = append(stmtMocks, mock.Name)
			}
		}
	}

	// Build recordedPrepByConn once (map[connID][]prepEntry) from recorded mocks
	recordedPrepByConn := buildRecordedPrepIndex(unfiltered)

	if req.Header.Type == sCOM_STMT_PREP || req.Header.Type == sCOM_STMT_EXEC {
		var allEntries []string
		for connID, prepEntries := range recordedPrepByConn {
			for _, entry := range prepEntries {
				allEntries = append(allEntries, fmt.Sprintf("connID=%s stmtID=%d query=%q mock=%s", connID, entry.statementID, entry.query, entry.mockName))
			}
		}
		logger.Debug("recorded prepEntries", zap.String("entries", strings.Join(allEntries, " | ")))
	}

	var (
		maxMatchedCount int
		matchedResp     *mysql.Response
		matchedMock     *models.Mock
		queryMatched    bool
		stmtMatched     bool
	)

	// Single pass: filter & match on the fly.
	for _, mock := range unfiltered {
		if mock.Kind != models.MySQL {
			continue
		}
		if mock.Spec.Metadata["type"] == "config" {
			continue // command-phase only wants data mocks
		}
		for _, mockReq := range mock.Spec.MySQLRequests {
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			default:
			}
			switch req.Header.Type {
			case sCOM_STMT_CLOSE:
				// query-aware CLOSE matching via recordedPrepByConn + runtime map
				var expectedQuery, actualQuery string
				if expClose, _ := mockReq.PacketBundle.Message.(*mysql.StmtClosePacket); expClose != nil {
					expectedQuery = lookupRecordedQuery(recordedPrepByConn, mock.Spec.Metadata["connID"], expClose.StatementID)
				}
				if actClose, _ := req.PacketBundle.Message.(*mysql.StmtClosePacket); actClose != nil && decodeCtx != nil && decodeCtx.StmtIDToQuery != nil {
					actualQuery = strings.TrimSpace(decodeCtx.StmtIDToQuery[actClose.StatementID])
				}
				c := matchCloseWithQuery(mockReq.PacketBundle, req.PacketBundle, expectedQuery, actualQuery)
				if c > maxMatchedCount {
					maxMatchedCount, matchedResp, matchedMock = c, &mysql.Response{}, mock
				}

			case sCOM_QUERY:
				if ok, c := matchQueryPacket(ctx, logger, mockReq.PacketBundle, req.PacketBundle); ok {
					matchedResp, matchedMock, queryMatched = &mock.Spec.MySQLResponses[0], mock, true
				} else if c > maxMatchedCount {
					maxMatchedCount, matchedResp, matchedMock = c, &mock.Spec.MySQLResponses[0], mock
				}

			case sCOM_STMT_PREP:
				if ok, c := matchPreparePacket(ctx, logger, mockReq.PacketBundle, req.PacketBundle); ok {
					matchedResp, matchedMock, queryMatched = &mock.Spec.MySQLResponses[0], mock, true
				} else if c > maxMatchedCount {
					maxMatchedCount, matchedResp, matchedMock = c, &mock.Spec.MySQLResponses[0], mock
				}

			case sCOM_STMT_EXEC:
				// query-aware EXEC matching via recordedPrepByConn + runtime map
				expMsg, eOk := mockReq.PacketBundle.Message.(*mysql.StmtExecutePacket)
				actMsg, aOk := req.PacketBundle.Message.(*mysql.StmtExecutePacket)

				if !eOk || !aOk {
					//  Either mock or actual request is not of type StmtExecutePacket
					continue
				}

				logger.Debug("List of com-stmt-execute mocks to match", zap.Strings("mocks", stmtMocks))

				// remove this log and if block
				if actMsg != nil {
					logger.Debug("Trying to match the mock with com-stmt-execute request", zap.String("mock_name", mock.Name), zap.Any("Req", actMsg))
				}

				var expectedQuery, actualQuery string
				if expMsg != nil {
					expectedQuery = lookupRecordedQuery(recordedPrepByConn, mock.Spec.Metadata["connID"], expMsg.StatementID)
				}
				if actMsg != nil && decodeCtx != nil && decodeCtx.StmtIDToQuery != nil {
					actualQuery = strings.TrimSpace(decodeCtx.StmtIDToQuery[actMsg.StatementID])
				}

				logger.Debug("queries comparison", zap.String("expected_query", expectedQuery), zap.String("actual_query", actualQuery), zap.Uint32("mock_statement_id", expMsg.StatementID), zap.Uint32("actual_statment_id", actMsg.StatementID), zap.Any("connID", mock.Spec.Metadata["connID"]), zap.String("mock_name", mock.Name))

				if ok, c := matchStmtExecutePacketQueryAware(logger, mockReq.PacketBundle, req.PacketBundle, expectedQuery, actualQuery, mock.Name); ok {
					// Query-aware definitive match (exact or structural): pick and stop searching
					matchedResp, matchedMock, stmtMatched = &mock.Spec.MySQLResponses[0], mock, true
				} else if c > maxMatchedCount {
					// fallback score-based candidate (used when no stmt info was available)
					maxMatchedCount, matchedResp, matchedMock = c, &mock.Spec.MySQLResponses[0], mock
				}

			case sCOM_INIT_DB:
				if c := matchInitDbPacket(ctx, logger, mockReq.PacketBundle, req.PacketBundle); c > maxMatchedCount {
					maxMatchedCount, matchedResp, matchedMock = c, &mock.Spec.MySQLResponses[0], mock
				}
			case sCOM_STATS:
				if c := matchStatisticsPacket(ctx, logger, mockReq.PacketBundle, req.PacketBundle); c > maxMatchedCount {
					maxMatchedCount, matchedResp, matchedMock = c, &mock.Spec.MySQLResponses[0], mock
				}
			case sCOM_DEBUG:
				if c := matchDebugPacket(ctx, logger, mockReq.PacketBundle, req.PacketBundle); c > maxMatchedCount {
					maxMatchedCount, matchedResp, matchedMock = c, &mock.Spec.MySQLResponses[0], mock
				}
			case sCOM_PING:
				if c := matchPingPacket(ctx, logger, mockReq.PacketBundle, req.PacketBundle); c > maxMatchedCount {
					maxMatchedCount, matchedResp, matchedMock = c, &mock.Spec.MySQLResponses[0], mock
				}
			case sCOM_RESET_CONN:
				if c := matchResetConnectionPacket(ctx, logger, mockReq.PacketBundle, req.PacketBundle); c > maxMatchedCount {
					maxMatchedCount, matchedResp, matchedMock = c, &mock.Spec.MySQLResponses[0], mock
				}
			}
		}
		if queryMatched || stmtMatched {
			break
		}
	}

	if matchedResp == nil {
		// Graceful generic OK for common control statements (no mocks)
		if req.Header.Type == sCOM_QUERY {
			if qp, ok := req.Message.(*mysql.QueryPacket); ok {
				q := strings.TrimSpace(qp.Query)
				switch {
				case strings.EqualFold(q, "BEGIN"),
					strings.EqualFold(q, "START TRANSACTION"),
					strings.EqualFold(q, "COMMIT"),
					strings.EqualFold(q, "ROLLBACK"),
					hasPrefixFold(q, "SET "),
					// DDL/control that only expects an OK from server
					hasPrefixFold(q, "ALTER "),
					hasPrefixFold(q, "CREATE "),
					hasPrefixFold(q, "DROP "),
					hasPrefixFold(q, "TRUNCATE "),
					hasPrefixFold(q, "RENAME "),
					hasPrefixFold(q, "LOCK TABLES"),
					hasPrefixFold(q, "UNLOCK TABLES"),
					hasPrefixFold(q, "SAVEPOINT "),
					hasPrefixFold(q, "RELEASE SAVEPOINT "),
					hasPrefixFold(q, "USE "):
					// Build a minimal OK; encoder will set length from payload.
					seq := byte(1)
					if req.PacketBundle.Header != nil && req.PacketBundle.Header.Header != nil {
						seq = req.PacketBundle.Header.Header.SequenceID + 1
					}
					generic := &mysql.Response{
						PacketBundle: mysql.PacketBundle{
							Header: &mysql.PacketInfo{
								Header: &mysql.Header{PayloadLength: 7, SequenceID: seq},
								Type:   mysql.StatusToString(mysql.OK),
							},
							Message: &mysql.OKPacket{
								Header:       mysql.OK,
								AffectedRows: 0,
								LastInsertID: 0,
								StatusFlags:  0x0002,
								Warnings:     0,
								Info:         "",
							},
						},
					}
					logger.Debug("Returning synthetic OK for unmocked control/DDL", zap.String("query", q))

					return generic, true, nil
				}
			}
		}
		return nil, false, nil
	}

	// Update the mock in the database BEFORE modifying the response
	// This ensures we update using the original mock state
	if okk := updateMock(ctx, logger, matchedMock, mockDb); !okk {
		logger.Debug("failed to update the matched mock")
		// Re-fetch once to avoid spin
		return nil, false, fmt.Errorf("failed to update matched mock")
	}

	// Create a copy of the response to avoid modifying the original mock
	responseCopy := &mysql.Response{
		PacketBundle: matchedResp.PacketBundle,
		Payload:      matchedResp.Payload,
	}

	// Persist prepared-statement metadata
	if req.Header.Type == sCOM_STMT_PREP {
		if prepareOkResp, ok := responseCopy.Message.(*mysql.StmtPrepareOkPacket); ok && prepareOkResp != nil {
			// Store original statement ID for logging
			originalStmtID := prepareOkResp.StatementID

			// Generate a new unique statement ID for this connection.
			// During record mode, different connections can produce identical statement IDs
			// for the same or different queries. However, during test mode, if both queries
			// execute on the same connection and we reuse those IDs, they would collide.
			// A single connection cannot have two different queries with the same statement ID.
			// To avoid this, we assign a new incremental and unique statement ID for each query.

			newStmtID := decodeCtx.NextStmtID
			decodeCtx.NextStmtID++

			// Create a copy of the StmtPrepareOkPacket and update the statement ID
			prepareOkRespCopy := *prepareOkResp
			prepareOkRespCopy.StatementID = newStmtID
			responseCopy.Message = &prepareOkRespCopy

			if sp, ok := req.Message.(*mysql.StmtPreparePacket); ok && sp != nil {
				// Store in the prepared statements map so that it can be used during EXEC/CLOSE
				decodeCtx.PreparedStatements[prepareOkRespCopy.StatementID] = &prepareOkRespCopy
				// maintain a runtime stmtID -> query map so EXEC/CLOSE can be matched by query.
				decodeCtx.StmtIDToQuery[prepareOkRespCopy.StatementID] = sp.Query
				logger.Debug("Recorded runtime PREP mapping with new statement ID",
					zap.Uint32("original_stmt_id from mock ", originalStmtID),
					zap.Uint32("new_stmt_id", prepareOkRespCopy.StatementID),
					zap.String("query", strings.TrimSpace(sp.Query)))
			}
		}
	}

	logger.Debug("matched command with the mock", zap.Any("mock", matchedMock.Name))
	return responseCopy, true, nil
}

// func matchClosePacket(_ context.Context, _ *zap.Logger, expected, actual mysql.PacketBundle) int {
// 	matchCount := 0
// 	// Match the type and return zero if the types are not equal
// 	if expected.Header.Type != actual.Header.Type {
// 		return 0
// 	}
// 	// Match the header
// 	ok := matchHeader(*expected.Header.Header, *actual.Header.Header)
// 	if ok {
// 		matchCount += 2
// 	}
// 	expectedMessage, _ := expected.Message.(*mysql.StmtClosePacket)
// 	actualMessage, _ := actual.Message.(*mysql.StmtClosePacket)
// 	// Match the statementID
// 	if expectedMessage.StatementID == actualMessage.StatementID {
// 		matchCount++
// 	}
// 	return matchCount
// }

func getQueryStructure(sql string) (string, error) {

	opts := sqlparser.Options{}
	parser, err := sqlparser.New(opts)
	if err != nil {
		return "", fmt.Errorf("failed to create MYSQL query parser: %w", err)
	}

	stmt, err := parser.Parse(sql)
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

func matchQuery(_ context.Context, log *zap.Logger, expected, actual mysql.PacketBundle, getQuery func(packet mysql.PacketBundle) string) (bool, int) {
	matchCount := 0

	// Match the type and return zero if the types are not equal
	if expected.Header.Type != actual.Header.Type {
		return false, 0
	}

	expectedQuery := getQuery(expected)
	actualQuery := getQuery(actual)

	if actual.Header.Header.PayloadLength == expected.Header.Header.PayloadLength {
		matchCount++
		if expectedQuery == actualQuery {
			matchCount++
			log.Debug("Query Exact matched",
				zap.String("expected query", expectedQuery),
				zap.String("actual query", actualQuery))
			return true, matchCount
		}
	}

	// check if any of them the query is dml and other is not, then there is no match.
	if sqlparser.IsDML(expectedQuery) && !sqlparser.IsDML(actualQuery) {
		log.Debug("expected query is dml but actual is not",
			zap.String("expected query", expectedQuery),
			zap.String("actual query", actualQuery))
		return false, 0
	} else if !sqlparser.IsDML(expectedQuery) && sqlparser.IsDML(actualQuery) {
		log.Debug("actual query is dml but expected is not",
			zap.String("expected query", expectedQuery),
			zap.String("actual query", actualQuery))
		return false, 0
	}

	if !(sqlparser.IsDML(expectedQuery) && sqlparser.IsDML(actualQuery)) {
		log.Debug("No Query is dml",
			zap.String("expected query", expectedQuery),
			zap.String("actual query", actualQuery))
		return false, matchCount
	}

	// Here we can compare the structure of the queries, as both are DML queries.
	log.Debug("Both queries are DML",
		zap.String("expected query", expectedQuery),
		zap.String("actual query", actualQuery))

	actualSignature, err := getQueryStructureCached(actualQuery)
	if err != nil {
		log.Warn("failed to get actual query structure",
			zap.String("actual Query", actualQuery),
			zap.Error(err))
		return false, matchCount
	}

	expectedSignature, err := getQueryStructureCached(expectedQuery)
	if err != nil {
		log.Warn("failed to get expected query structure",
			zap.String("expected Query", expectedQuery),
			zap.Error(err))
		return false, matchCount
	}

	if expectedSignature == actualSignature {
		log.Debug("query structure matched",
			zap.String("expected signature", expectedSignature),
			zap.String("actual signature", actualSignature))
		return true, matchCount
	}

	return false, matchCount
}

func matchQueryPacket(ctx context.Context, log *zap.Logger, expected, actual mysql.PacketBundle) (bool, int) {
	getQuery := func(packet mysql.PacketBundle) string {
		msg, _ := packet.Message.(*mysql.QueryPacket)
		return msg.Query
	}
	return matchQuery(ctx, log, expected, actual, getQuery)
}

func matchPreparePacket(ctx context.Context, log *zap.Logger, expected, actual mysql.PacketBundle) (bool, int) {
	getQuery := func(packet mysql.PacketBundle) string {
		msg, _ := packet.Message.(*mysql.StmtPreparePacket)
		return msg.Query
	}
	return matchQuery(ctx, log, expected, actual, getQuery)
}

// query-aware EXEC scoring.
//   - Keep the existing header/flags/params scoring.
//   - Do NOT reward raw StatementID equality.
//   - If both expectedQuery and actualQuery are present, require them to match (exact or structural).
//     If they don't match, return (false, 0) immediately.
//   - If either query is missing, fall back to best-effort scoring (returns (false, score)).
func matchStmtExecutePacketQueryAware(logger *zap.Logger, expected, actual mysql.PacketBundle, expectedQuery, actualQuery string, mockName string) (bool, int) {
	matchCount := 0

	// Match the type and return zero if the types are not equal
	if expected.Header.Type != actual.Header.Type {
		return false, 0
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

	// DO NOT score StatementID equality (unstable across runs)
	// if expectedMessage.StatementID == actualMessage.StatementID { matchCount++ }

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
	var totalMatchedParams int
	var allParamsMatched bool
	if len(expectedMessage.Parameters) == len(actualMessage.Parameters) {
		for i := range expectedMessage.Parameters {
			ep := expectedMessage.Parameters[i]
			ap := actualMessage.Parameters[i]

			typeEqual := ep.Type == ap.Type
			nameEqual := ep.Name == ap.Name
			unsignedEqual := ep.Unsigned == ap.Unsigned
			valueEqual := false
			if unsignedEqual { // initial check to avoid comparing signed vs unsigned values
				valueEqual = paramValueEqual(ep.Value, ap.Value)
			}
			if typeEqual && nameEqual && unsignedEqual && valueEqual {
				matchCount++
				totalMatchedParams++
			}
		}

		// All parameters matched
		if len(expectedMessage.Parameters) == totalMatchedParams {
			allParamsMatched = true
			logger.Debug("all parameters matched", zap.String("mock-name", mockName))
		}
	}

	// Query logic:
	queryMatched := false
	eq := strings.TrimSpace(expectedQuery)
	aq := strings.TrimSpace(actualQuery)

	// If both queries are present, require them to match (exact or structural) for a definitive match.
	if eq != "" && aq != "" {
		// If both queries are available, require an exact or structural match to treat this as a definitive match.
		if strings.EqualFold(eq, aq) {
			matchCount += 10
			queryMatched = true
			logger.Debug("query matched exactly", zap.String("related stmt-exec mock-name", mockName))
		} else if sigE, errE := getQueryStructureCached(eq); errE == nil {
			if sigA, errA := getQueryStructureCached(aq); errA == nil && sigE == sigA {
				matchCount += 6
				queryMatched = true
				logger.Debug("query structure matched", zap.String("related stmt-exec mock-name", mockName))
			}
		}
	}

	if !queryMatched || !allParamsMatched {
		return false, matchCount
	}

	// Both queryMatched and allParamsMatched must be true for a definitive match. Otherwise, return the best-effort score.
	return (queryMatched && allParamsMatched), matchCount
}

func paramValueEqual(a, b interface{}) bool {
	switch av := a.(type) {
	case []byte:
		bv, ok := b.([]byte)
		return ok && bytes.Equal(av, bv)
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case int:
		switch bv := b.(type) {
		case int:
			return av == bv
		case int64:
			return int64(av) == bv
		case int32:
			return av == int(bv)
		case float32:
			return float32(av) == bv
		case float64:
			return float64(av) == bv
		}
	case int32:
		switch bv := b.(type) {
		case int32:
			return av == bv
		case int:
			return int(av) == bv
		case int64:
			return int64(av) == bv
		case float32:
			return float32(av) == bv
		case float64:
			return float64(av) == bv
		}
	case int64:
		switch bv := b.(type) {
		case int64:
			return av == bv
		case int:
			return av == int64(bv)
		case int32:
			return av == int64(bv)
		case float32:
			return float32(av) == bv
		case float64:
			return float64(av) == bv
		}
	case uint32:
		switch bv := b.(type) {
		case uint32:
			return av == bv
		case uint64:
			return uint64(av) == bv
		case float32:
			return float32(av) == bv
		case float64:
			return float64(av) == bv
		}
	case uint64:
		switch bv := b.(type) {
		case uint64:
			return av == bv
		case uint32:
			return av == uint64(bv)
		case float32:
			return float32(av) == bv
		case float64:
			return float64(av) == bv
		}
	case float32:
		switch bv := b.(type) {
		case float32:
			return av == bv
		case float64:
			return float64(av) == bv
		case int:
			return av == float32(bv)
		case int32:
			return av == float32(bv)
		case int64:
			return av == float32(bv)
		case uint32:
			return av == float32(bv)
		case uint64:
			return av == float32(bv)
		}
	case float64:
		switch bv := b.(type) {
		case float64:
			return av == bv
		case float32:
			return av == float64(bv)
		case int:
			return av == float64(bv)
		case int32:
			return av == float64(bv)
		case int64:
			return av == float64(bv)
		case uint32:
			return av == float64(bv)
		case uint64:
			return av == float64(bv)
		}
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	}
	// Fallback (rare)
	return reflect.DeepEqual(a, b)
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

// printable strips non-printable bytes (common in legacy mocks)
func printable(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 32 && r <= 126 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Back-compat database compare:
// - Strip junk bytes
// - Treat empty on either side as OK
// - Allow suffix match (e.g., "...uss" vs "ss") to accommodate off-by-one legacy encodes
func dbEqualCompat(exp, act string) bool {
	ex := printable(strings.TrimSpace(exp))
	ac := printable(strings.TrimSpace(act))
	if ex == ac {
		return true
	}
	if ex == "" || ac == "" {
		return true
	}
	return strings.HasSuffix(ex, ac) || strings.HasSuffix(ac, ex)
}

var knownPlugins = map[string]struct{}{
	"caching_sha2_password": {},
	"mysql_native_password": {},
	"mysql_clear_password":  {},
}

// Back-compat plugin compare:
// - Strip junk
// - If both are known plugin names and differ -> mismatch
// - Otherwise (unknown/garbled on either side) -> tolerate
func pluginEqualCompat(exp, act string) bool {
	ex := printable(strings.TrimSpace(exp))
	ac := printable(strings.TrimSpace(act))
	if ex == ac {
		return true
	}
	_, exKnown := knownPlugins[ex]
	_, acKnown := knownPlugins[ac]
	if exKnown && acKnown {
		return false
	}
	return true
}

// Build recorded PREP index per connection from recorded mocks.
// We map each connID to the list of (stmtID,query) pairs found by pairing
// StmtPrepareOkPacket(stmtID) with the nearest COM_STMT_PREPARE query.
// Assumes each mock has exactly one MySQLRequest and one MySQLResponse.
func buildRecordedPrepIndex(unfiltered []*models.Mock) map[string][]prepEntry {
	out := make(map[string][]prepEntry)
	for _, m := range unfiltered {
		if m == nil || m.Kind != models.MySQL {
			continue
		}
		if m.Spec.Metadata["type"] == "config" {
			continue
		}
		connID := m.Spec.Metadata["connID"]

		// Check if we have at least one response
		if len(m.Spec.MySQLResponses) == 0 {
			continue
		}

		// Get the statement ID from the first response (if it's a StmtPrepareOkPacket)
		spok, ok := m.Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket)
		if !ok || spok == nil {
			continue
		}
		stmtID := spok.StatementID

		// Check if we have at least one request
		if len(m.Spec.MySQLRequests) == 0 {
			continue
		}

		// Get the query from the first request (if it's a StmtPreparePacket)
		sp, ok := m.Spec.MySQLRequests[0].Message.(*mysql.StmtPreparePacket)
		if !ok || sp == nil {
			continue
		}
		prepQuery := strings.TrimSpace(sp.Query)
		if prepQuery == "" {
			continue
		}

		out[connID] = append(out[connID], prepEntry{
			statementID: stmtID,
			query:       prepQuery,
			mockName:    m.Name,
		})
	}
	return out
}

// Caveat: There can be a condition where for the same connId, for the same query there can be different statementIds,
// this can happen either when client closes the prepared statement and creates a new one for the same query
// or if the client prepares the same query multiple times without closing it.

//TODO: Conditions to handle

// 1. On connection conn-1
// -> preparedStatement with query-1 comes and returns statement-id=1
// -> closeStmt for stmt-id=1
// -> preparedStatement with query-1 comes and returns statement-id=2
// -> closeStmt for stmt-id=2
// -> preparedStatement with query-1 comes and returns statement-id=3
// -> stmtExecute on stmt-id=3

// 2. On connection conn-1
// -> preparedStatement with query-1 comes and returns statement-id=1
// -> preparedStatement with query-1 comes and returns statement-id=2
// -> preparedStatement with query-1 comes and returns statement-id=3
// -> Client will usually use stmt-id-3 but not necessary.

// lookup helper on recordedPrepByConn
func lookupRecordedQuery(index map[string][]prepEntry, connID string, stmtID uint32) string {
	list := index[connID]
	for _, e := range list {
		if e.statementID == stmtID {
			return e.query
		}
	}
	return ""
}

// Query-aware CLOSE scoring (header + query bonus; no raw stmt-id equality)
func matchCloseWithQuery(expected, actual mysql.PacketBundle, expectedQuery, actualQuery string) int {
	score := 0
	if expected.Header.Type != actual.Header.Type {
		return 0
	}
	if matchHeader(*expected.Header.Header, *actual.Header.Header) {
		score += 2
	}
	eq := strings.TrimSpace(expectedQuery)
	aq := strings.TrimSpace(actualQuery)
	if eq == "" || aq == "" {
		return score
	}
	if strings.EqualFold(eq, aq) {
		return score + 10
	}
	if sigE, errE := getQueryStructureCached(eq); errE == nil {
		if sigA, errA := getQueryStructureCached(aq); errA == nil && sigE == sigA {
			return score + 6
		}
	}
	return score
}
