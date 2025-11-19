//go:build linux

package replayer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"vitess.io/vitess/go/vt/sqlparser"
)

var querySigCache sync.Map // map[string]string

// recorded PREP registry per recorded connection
type prepEntry struct { // minimal, enough for lookup
	StatementID uint32
	Query       string
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

	// Build recordedPrepByConn once (map[connID][]prepEntry) from recorded mocks
	recordedPrepByConn := buildRecordedPrepIndex(unfiltered)

	var (
		maxMatchedCount int
		matchedResp     *mysql.Response
		matchedMock     *models.Mock
		queryMatched    bool
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
				expMsg, _ := mockReq.PacketBundle.Message.(*mysql.StmtExecutePacket)
				actMsg, _ := req.PacketBundle.Message.(*mysql.StmtExecutePacket)

				var expectedQuery, actualQuery string
				if expMsg != nil {
					expectedQuery = lookupRecordedQuery(recordedPrepByConn, mock.Spec.Metadata["connID"], expMsg.StatementID)
				}
				if actMsg != nil && decodeCtx != nil && decodeCtx.StmtIDToQuery != nil {
					actualQuery = strings.TrimSpace(decodeCtx.StmtIDToQuery[actMsg.StatementID])
				}

				if c := matchStmtExecutePacketQueryAware(mockReq.PacketBundle, req.PacketBundle, expectedQuery, actualQuery); c > maxMatchedCount {
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
		if queryMatched {
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

	// Persist prepared-statement metadata
	if req.Header.Type == sCOM_STMT_PREP {
		if prepareOkResp, ok := matchedResp.Message.(*mysql.StmtPrepareOkPacket); ok && prepareOkResp != nil {

			if sp, ok := req.Message.(*mysql.StmtPreparePacket); ok && sp != nil {
				// maintain a runtime stmtID -> query map so EXEC/CLOSE can be matched by query.

				decodeCtx.StmtIDToQuery[prepareOkResp.StatementID] = sp.Query
				logger.Debug("Recorded runtime PREP mapping",
					zap.Uint32("stmt_id", prepareOkResp.StatementID),
					zap.String("query", strings.TrimSpace(sp.Query)))

				//also index the prepare packet keyed by connection id and normalized query
				// get connID from ctx
				if rawConn := ctx.Value(models.ClientConnectionIDKey); rawConn != nil {
					if connID, ok := rawConn.(string); ok && connID != "" {
						// ensure inner map exists
						if _, ok := decodeCtx.MockPrepStmts[connID]; !ok {
							decodeCtx.MockPrepStmts[connID] = make(map[string]*mysql.StmtPrepareOkPacket)
						}
						nq := strings.ToLower(strings.TrimSpace(sp.Query)) // normalized query key
						decodeCtx.MockPrepStmts[connID][nq] = prepareOkResp
						logger.Debug("Inserted runtime PREP OK Response into MockPrepStmtMap",
							zap.String("connID", connID),
							zap.String("normalized_query", nq),
							zap.Uint32("stmt_id", prepareOkResp.StatementID))
					}
				}
			}
		}
	}

	if okk := updateMock(ctx, logger, matchedMock, mockDb); !okk {
		logger.Debug("failed to update the matched mock")
		// Re-fetch once to avoid spin
		return nil, false, fmt.Errorf("failed to update matched mock")
	}
	logger.Debug("matched command with the mock", zap.Any("mock", matchedMock.Name))
	return matchedResp, true, nil
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
//   - Reward query equality (or structural equality) resolved via:
//     recorded (expected) -> recordedPrepByConn[connID] -> query
//     runtime  (actual)   -> decodeCtx.StmtIDToQuery(stmtID) set during COM_STMT_PREP
func matchStmtExecutePacketQueryAware(expected, actual mysql.PacketBundle, expectedQuery, actualQuery string) int {
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
	if len(expectedMessage.Parameters) == len(actualMessage.Parameters) {
		for i := range expectedMessage.Parameters {
			ep := expectedMessage.Parameters[i]
			ap := actualMessage.Parameters[i]
			if ep.Type == ap.Type &&
				ep.Name == ap.Name &&
				ep.Unsigned == ap.Unsigned &&
				paramValueEqual(ep.Value, ap.Value) {
				matchCount++
			}
		}
	}

	// Query bonus
	eq := strings.TrimSpace(expectedQuery)
	aq := strings.TrimSpace(actualQuery)
	if eq != "" && aq != "" {
		if strings.EqualFold(eq, aq) {
			matchCount += 10
		} else if sigE, errE := getQueryStructureCached(eq); errE == nil {
			if sigA, errA := getQueryStructureCached(aq); errA == nil && sigE == sigA {
				matchCount += 6
			}
		}
	}

	return matchCount
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
		bv, ok := b.(int)
		return ok && av == bv
	case int32:
		bv, ok := b.(int32)
		return ok && av == bv
	case int64:
		switch bv := b.(type) {
		case int64:
			return av == bv
		case int:
			return av == int64(bv)
		}
	case uint32:
		bv, ok := b.(uint32)
		return ok && av == bv
	case uint64:
		bv, ok := b.(uint64)
		return ok && av == bv
	case float32:
		bv, ok := b.(float32)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
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

		// collect all prepare OK stmtIDs in this mock
		stmtIDs := make(map[uint32]struct{})
		for _, r := range m.Spec.MySQLResponses {
			if spok, ok := r.Message.(*mysql.StmtPrepareOkPacket); ok && spok != nil {
				stmtIDs[spok.StatementID] = struct{}{}
			}
		}
		if len(stmtIDs) == 0 {
			continue
		}

		// try to find a (nearest) PREP query; fall back to the last PREP in the mock
		// this is heuristic but robust across our recording layout
		var lastPrepQuery string
		for i := len(m.Spec.MySQLRequests) - 1; i >= 0; i-- {
			if sp, ok := m.Spec.MySQLRequests[i].Message.(*mysql.StmtPreparePacket); ok && sp != nil {
				lastPrepQuery = strings.TrimSpace(sp.Query)
				break
			}
		}
		if lastPrepQuery == "" {
			continue
		}

		for stmtID := range stmtIDs {
			out[connID] = append(out[connID], prepEntry{
				StatementID: stmtID,
				Query:       lastPrepQuery,
			})
		}
	}
	return out
}

// lookup helper on recordedPrepByConn
func lookupRecordedQuery(index map[string][]prepEntry, connID string, stmtID uint32) string {
	list := index[connID]
	for _, e := range list {
		if e.StatementID == stmtID {
			return e.Query
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
