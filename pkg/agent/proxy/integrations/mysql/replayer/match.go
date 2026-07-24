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
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"vitess.io/vitess/go/vt/sqlparser"
)

var querySigCache sync.Map // map[string]string

// recorded PREP registry per recorded connection
// The prepareOrder and wasClosed fields enable ID reuse handling:
// - prepareOrder: tracks the Nth time a query was prepared (for correlating with runtime)
// - wasClosed: indicates if this statement was closed before a subsequent prepare
// These fields are currently used for:
// 1. markClosedEntries() to track statement lifecycle in recorded mocks
// 2. Future enhancement: correlate recorded and runtime prepare cycles when IDs differ
// The current matching works via query comparison (see matchStmtExecutePacketQueryAware),
// which handles most ID reuse cases by comparing query content rather than statement IDs.
type prepEntry struct {
	statementID  uint32
	query        string
	mockName     string // for debugging purpose
	prepareOrder int    // Order of preparation for this query (1st, 2nd, 3rd time prepared)
	wasClosed    bool   // Whether this statement was closed before another prepare
}

// truncate returns s trimmed to at most maxLen characters (including "..." suffix if truncated).
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
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

	// Match the Username.
	// Some synthetic config mocks (built from SSLRequest-only captures) cannot
	// carry username and store it as empty. Treat empty expected username as a
	// wildcard for backward-compatible replay matching.
	if exp.Username != "" && exp.Username != act.Username {
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

// hasConfigTag returns true when the mock's raw Spec.Metadata["type"]
// equals "config". Nil-map safe. Used as a defensive fallback
// alongside TestModeInfo.Lifetime so mocks that reached the matcher
// without DeriveLifetime having run still classify correctly.
func hasConfigTag(m *models.Mock) bool {
	return m != nil && m.Spec.Metadata != nil && m.Spec.Metadata["type"] == "config"
}

// isSessionReusableCommandMock reports whether a session/config-tagged
// mock is eligible for dispatch at command phase. Returns true for
// any single-request mock whose first packet header is a COM_*
// command type — this covers both the narrow input-independent
// allowlist (COM_PING/STATISTICS/DEBUG/RESET_CONNECTION, tagged as
// "config" by the recorder and routed to session pool for pre-first-
// test survival) AND lax-mode kind-fallback-promoted data queries
// (COM_QUERY etc., promoted to session under 9b18de8d's
// pre-Phase-2-compat branch so they stay reusable across tests).
//
// EXCLUDES multi-request handshake bundles (len > 1) — those are
// matched at handshake time and should not spuriously match at
// command phase.
//
// The Header.Type is whatever the recorder stamped; for command-
// phase packets it's always a COM_* string. Non-command packets
// (OK/ERR/EOF payloads, handshake response elements) never land
// here because they're embedded inside the bundle, not first-
// request headers.
func isSessionReusableCommandMock(mock *models.Mock) bool {
	if mock == nil || len(mock.Spec.MySQLRequests) != 1 {
		return false
	}
	hdr := mock.Spec.MySQLRequests[0].PacketBundle.Header
	if hdr == nil {
		return false
	}
	// Accept any COM_*-prefixed packet type at command phase. Using
	// the string prefix rather than an allowlist lets us cover new
	// commands introduced by the MySQL server without matcher edits,
	// and it keeps the check O(1).
	return strings.HasPrefix(hdr.Type, "COM_")
}

// matchCommand matches one live command-phase request against the mock pool.
// noiseEngine carries the resolved schema-noise flags (nil-safe: a nil engine
// disables both detection and strict). userBodyNoise is the user's
// test.globalNoise.requestBody bucket (root-relative lowercased paths) so
// configured noise participates in strict gating with the same vocabulary as
// learned req_body_noise. miss is non-nil only when ok is false without error.
func matchCommand(ctx context.Context, logger *zap.Logger, req mysql.Request, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext, noiseEngine *schemanoise.Engine, userBodyNoise map[string][]string) (*mysql.Response, bool, *mockMiss, error) {
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
		sCOM_STMT_RESET = mysql.CommandStatusToString(mysql.COM_STMT_RESET)
		sCOM_STMT_SLD   = mysql.CommandStatusToString(mysql.COM_STMT_SEND_LONG_DATA)
	)

	// Fast path: QUIT may have no mock
	if req.Header.Type == sCOM_QUIT {
		return nil, false, nil, io.EOF
	}

	// Fetch THREE pools and merge. Under strict-mode default and the
	// post-Phase-2 Lifetime routing, data mocks (tag="mocks" →
	// LifetimePerTest) land in the per-test pool rather than the
	// session pool — pre-unification the whole unfiltered tree
	// contained everything so GetSessionMocks was enough; now we need
	// to explicitly pull per-test mocks too or COM_PING/data queries
	// disappear from the matcher's view.
	//
	// Order: per-test FIRST, session, connection. Per-test mocks are
	// the most specific for the current test and should win ties;
	// session and connection follow as fallbacks for reusable traffic.
	perTestMocks, err := mockDb.GetPerTestMocksInWindow()
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, nil, ctx.Err()
		}
		utils.LogError(logger, err, "failed to get per-test mocks")
		return nil, false, nil, err
	}
	sessionMocks, err := mockDb.GetSessionMocks()
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, nil, ctx.Err()
		}
		utils.LogError(logger, err, "failed to get session mocks")
		return nil, false, nil, err
	}

	// Unification Phase 2.5: prepared-statement setup mocks are tagged
	// type=connection by the recorder (see
	// pkg/agent/proxy/integrations/mysql/recorder/query.go) and live in
	// their own per-connID pool. Fetch them explicitly here so
	// buildRecordedPrepIndex can include them; GetConnectionMocks
	// returns an empty slice when no connection-scoped mocks exist, so
	// this is a no-op for apps that don't use PREPARE.
	connID := ""
	if v := ctx.Value(models.ClientConnectionIDKey); v != nil {
		if s, ok := v.(string); ok {
			connID = s
		}
	}
	var connectionMocks []*models.Mock
	if connID != "" {
		cm, cerr := mockDb.GetConnectionMocks(connID)
		if cerr != nil {
			// Hard-fail for prepared-statement traffic: without the
			// connection pool we can't resolve PREPARE↔EXECUTE pairs
			// and the later "no matching mock" would mask the real
			// root cause. Other command types tolerate the failure
			// (connection pool is advisory for them) — log + continue.
			if req.Header.Type == sCOM_STMT_PREP || req.Header.Type == sCOM_STMT_EXEC {
				utils.LogError(logger, cerr, "failed to get mysql connection mocks", zap.String("connID", connID))
				return nil, false, nil, fmt.Errorf("failed to get mysql connection mocks for connID %q: %w", connID, cerr)
			}
			logger.Debug("failed to get mysql connection mocks; proceeding without per-connID pool",
				zap.String("connID", connID),
				zap.Error(cerr))
		} else {
			connectionMocks = cm
		}
	}

	// Merge pools with per-test FIRST so a per-test data query wins over
	// a session-level catch-all when both happen to match. Connection-
	// scoped setups come last so buildRecordedPrepIndex / stmtMocks
	// naturally pick them up without needing a new priority order.
	pool := make([]*models.Mock, 0, len(perTestMocks)+len(sessionMocks)+len(connectionMocks))
	pool = append(pool, perTestMocks...)
	pool = append(pool, sessionMocks...)
	pool = append(pool, connectionMocks...)

	// Current outer-test window. The enterprise agent lax-promotes per-test
	// MySQL data mocks into the SESSION pool (agentStrict is false for a
	// WindowedProxy) and relies on MockManager for strict windowing, so the
	// window-scoped per-test getter (GetPerTestMocksInWindow) typically
	// returns nothing for MySQL and the data mocks arrive via sessionMocks
	// with their ReqTimestampMock intact. To distinguish a mock recorded
	// INSIDE the current test from a stale earlier-test row, we compare each
	// candidate's ReqTimestampMock against [winStart, winEnd] directly.
	winStart, winEnd := mockDb.CurrentTestWindow()
	windowActive := !winStart.IsZero() && !winEnd.IsZero()
	// mockInCurrentWindow reports whether a mock's recorded request timestamp
	// lies within the active outer-test window. When no window is active
	// (initial staging / between tests) every mock is treated as in-window
	// so behaviour matches the pre-fix path.
	mockInCurrentWindow := func(mk *models.Mock) bool {
		if !windowActive {
			return true
		}
		req := mk.Spec.ReqTimestampMock
		if req.IsZero() {
			return true
		}
		return !req.Before(winStart) && !req.After(winEnd)
	}

	if len(pool) == 0 {
		utils.LogError(logger, nil, "no mysql mocks found")
		return nil, false, nil, fmt.Errorf("no mysql mocks found")
	}

	// remove this block
	// get all the mock names that has type com-exec
	stmtMocks := []string{}
	for _, mock := range pool {
		if mock.Kind != models.MySQL {
			continue
		}
		// Skip session-tier config mocks at command-phase — they were
		// matched at handshake. Connection-scoped (prepared-statement
		// setup) mocks are KEPT here so the prepared-statement index
		// below picks them up and executes can match their setups
		// across test-window boundaries.
		if mock.TestModeInfo.Lifetime == models.LifetimeSession ||
			(mock.TestModeInfo.Lifetime == models.LifetimePerTest && hasConfigTag(mock)) {
			if !isSessionReusableCommandMock(mock) {
				continue
			}
		}
		for _, mockReq := range mock.Spec.MySQLRequests {
			if mockReq.PacketBundle.Header.Type == sCOM_STMT_EXEC {
				stmtMocks = append(stmtMocks, mock.Name)
			}
		}
	}

	// Build recordedPrepByConn once (map[connID][]prepEntry) from recorded mocks
	recordedPrepByConn := buildRecordedPrepIndex(pool)

	if req.Header.Type == sCOM_STMT_PREP || req.Header.Type == sCOM_STMT_EXEC {
		var allEntries []string
		for connID, prepEntries := range recordedPrepByConn {
			for _, entry := range prepEntries {
				allEntries = append(allEntries, fmt.Sprintf("connID=%s stmtID=%d query=%q mock=%s", connID, entry.statementID, entry.query, entry.mockName))
			}
		}
		logger.Debug("recorded prepEntries", zap.String("entries", strings.Join(allEntries, " | ")))
	}

	// Canonical JSON body of the live request for the schema-noise engine.
	// liveBodyOK is false for packets with no drift-capable body (utility
	// commands, CLOSE/RESET) — the engine no-ops for those.
	liveBody, liveBodyOK := mysqlRequestBodyJSON(&req.PacketBundle)

	// Strict-enforcement gate (see strictGate in schema_noise.go): consulted
	// before a mock may become a match candidate, and accumulates the
	// rejection diagnostics (count / closest mock / field-level diffs) the
	// mismatch report renders on a miss.
	gate := newStrictGate(noiseEngine, logger, req.Header.Type, liveBody, liveBodyOK, userBodyNoise)

	var (
		// Score-based candidate tracking. Every non-definitive matcher returns
		// a similarity score for its candidate; the trio below carries the best
		// one seen so far across the whole pool scan:
		//   maxMatchedCount — highest similarity score so far; a later
		//                     candidate replaces the pick only with a
		//                     STRICTLY higher score.
		//   matchedResp     — the response of that best-scoring candidate;
		//                     this is what gets served when no definitive
		//                     (exact) match is found anywhere in the pool.
		//   matchedMock     — the mock owning matchedResp; consumed/updated
		//                     via updateMock once selected.
		maxMatchedCount  int
		matchedResp      *mysql.Response
		matchedMock      *models.Mock
		queryMatched     bool
		stmtMatched      bool
		bestPartialMock  *models.Mock // closest non-exact match for diff reporting
		bestPartialQuery string       // query of the closest partial match

		// COM_STMT_EXECUTE FIFO fallback: when the live bound parameters
		// match NO recorded mock for the same prepared query (e.g. an
		// INSERT-then-SELECT read-back of a replay-generated uuid that
		// exists in no recorded parameter), we must NOT serve an arbitrary
		// same-shape row by score/first-wins. Instead we serve the next
		// UNCONSUMED per-test mock for that exact query in RECORDED ORDER.
		// Because per-test data mocks are consumed via DeleteFilteredMock
		// and the pool is iterated in recorded SortOrder, the FIRST
		// query-exact per-test mock encountered here is exactly that next
		// unconsumed row. We track the in-window candidate (preferred) and
		// an any-tier candidate (used only if no in-window per-test mock
		// exists for the query) separately so a stale earlier-test read-back
		// mock living in the startup/session tier cannot win over the
		// current test's own in-window row.
		fifoExecResp       *mysql.Response
		fifoExecMock       *models.Mock
		fifoExecRespWindow *mysql.Response
		fifoExecMockWindow *models.Mock

		// defExecResp/defExecMock hold a definitive (query+params exact)
		// COM_STMT_EXECUTE match that is NOT an in-window per-test mock
		// (i.e. it lives in the startup/session/connection tier). Used only
		// when no in-window definitive match exists, so a genuinely unique
		// reusable read still resolves while an in-window row always wins.
		defExecResp *mysql.Response
		defExecMock *models.Mock

		// COM_QUERY in-window preference (parity with the COM_STMT_EXECUTE
		// branch from #4235). A parameterless statement (Spring
		// JdbcTemplate without bind args → COM_QUERY, not a prepared
		// statement) that issues the SAME SQL text across tests but returns
		// a DIFFERENT row each time — e.g. an INSERT read-back
		// "SELECT v FROM kv ORDER BY id DESC LIMIT 1" — records one data
		// mock per call. The matcher used to take the FIRST exact-text
		// match in pool order and (because lax-promoted data mocks live in
		// the reusable session tier and are never consumed by updateMock)
		// served that same first row to every later test. We instead prefer
		// the exact-text match recorded INSIDE the current test window, and
		// keep the first out-of-window exact match only as a fallback for a
		// genuinely reusable single-recording query.
		queryExactResp *mysql.Response
		queryExactMock *models.Mock
	)

	// Single pass: filter & match on the fly. Iterates the merged pool
	// (unfiltered + connection-scoped) so prepared-statement executes
	// find their setups even when the setup was recorded in a
	// different test's window.
	for _, mock := range pool {
		if mock.Kind != models.MySQL {
			continue
		}
		// Session-tier handshake/auth mocks were matched at the
		// command prologue; skip them at command phase. Connection-
		// scoped (prepared-statement setup) mocks ARE retained —
		// they're how COM_STMT_EXEC finds its matching prepare.
		if mock.TestModeInfo.Lifetime == models.LifetimeSession ||
			(mock.TestModeInfo.Lifetime == models.LifetimePerTest && hasConfigTag(mock)) {
			if !isSessionReusableCommandMock(mock) {
				continue // command-phase only wants data + connection mocks + session-reusable commands
			}
		}
		for _, mockReq := range mock.Spec.MySQLRequests {
			select {
			case <-ctx.Done():
				return nil, false, nil, ctx.Err()
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
					// Exact query-text match. Prefer the candidate recorded
					// inside the current test window so a repeated stateful
					// read-back (same SQL, different row per call) resolves to
					// THIS test's row instead of the first one recorded. An
					// out-of-window exact match is kept only as a fallback for
					// a genuinely reusable single-recording query. When no
					// window is active (windowActive==false) this collapses to
					// the previous first-exact-match-wins behaviour.
					//
					// Even an exact-text match is strict-gated: the query
					// attributes (CLIENT_QUERY_ATTRIBUTES) live outside the
					// text and may still drift.
					if !gate.allows(mock) {
						// A strict-rejected exact-text candidate is still the
						// closest mock for the mismatch report (parity with
						// the EXECUTE branch) — without this the miss diff
						// renders an empty "closest" query.
						if bestPartialMock == nil || bestPartialQuery == "" {
							bestPartialMock = mock
							if qp, qok := mockReq.PacketBundle.Message.(*mysql.QueryPacket); qok {
								bestPartialQuery = qp.Query
							}
						}
						continue
					}
					if windowActive && !mockInCurrentWindow(mock) {
						if queryExactMock == nil {
							queryExactResp, queryExactMock = &mock.Spec.MySQLResponses[0], mock
						}
					} else {
						matchedResp, matchedMock, queryMatched = &mock.Spec.MySQLResponses[0], mock, true
					}
				} else if c > maxMatchedCount {
					// Track the closest candidate for the mismatch report even
					// when strict rejects it as a servable match below.
					bestPartialMock = mock
					if qp, qok := mockReq.PacketBundle.Message.(*mysql.QueryPacket); qok {
						bestPartialQuery = qp.Query
					}
					// Structure-matched-but-text-drifted candidate: under
					// strict it may only be served when the drift (body.query
					// / attribute values) is covered by learned/user noise.
					if !gate.allows(mock) {
						continue
					}
					maxMatchedCount, matchedResp, matchedMock = c, &mock.Spec.MySQLResponses[0], mock
				}

			case sCOM_STMT_PREP:
				if ok, c := matchPreparePacket(ctx, logger, mockReq.PacketBundle, req.PacketBundle); ok {
					if !gate.allows(mock) {
						// Same as COM_QUERY above: keep the strict-rejected
						// exact-text PREPARE as the closest candidate so the
						// miss diff names the query instead of rendering empty.
						if bestPartialMock == nil || bestPartialQuery == "" {
							bestPartialMock = mock
							if sp, spOk := mockReq.PacketBundle.Message.(*mysql.StmtPreparePacket); spOk {
								bestPartialQuery = sp.Query
							}
						}
						continue
					}
					matchedResp, matchedMock, queryMatched = &mock.Spec.MySQLResponses[0], mock, true
				} else if c > maxMatchedCount {
					// Track the closest candidate for the mismatch report even
					// when strict rejects it as a servable match below.
					bestPartialMock = mock
					if sp, spOk := mockReq.PacketBundle.Message.(*mysql.StmtPreparePacket); spOk {
						bestPartialQuery = sp.Query
					}
					// Structure-matched-but-text-drifted PREPARE (dynamic SQL:
					// trace comments, generated clauses): strict serves it only
					// when body.query is covered by learned/user noise.
					if !gate.allows(mock) {
						continue
					}
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

				if ok, c, queryExact := matchStmtExecutePacketQueryAware(logger, mockReq.PacketBundle, req.PacketBundle, expectedQuery, actualQuery, mock.Name, util.NewNoiseChecker(mock.Noise)); ok {
					// Query-aware definitive match (query + params exact).
					//
					// Multiple mocks can be definitive matches for the same
					// (query, params) pair — e.g. a startup/session-tier
					// read recorded BEFORE the first test window (admin's
					// empty pre-seed lookup) and the current test's own
					// in-window read with the real row. The startup mock is
					// iterated first (lower SortOrder) and, pre-fix, won by
					// first-definitive-wins, serving a stale/empty row. We
					// must instead prefer the in-window per-test mock.
					//
					// So: if this definitive match is an in-window per-test
					// mock, take it and stop (best possible). Otherwise record
					// it as the out-of-window definitive fallback and keep
					// scanning for an in-window definitive match.
					// Among definitive (query+params exact) matches, prefer the
					// candidate whose recorded request timestamp lies INSIDE the
					// current outer-test window. The enterprise agent lax-
					// promotes per-test MySQL data mocks into the session tier
					// (Lifetime becomes Session by the time the matcher sees
					// them — verified empirically: an in-window type=mocks
					// COM_STMT_EXECUTE arrives with Lifetime==Session), so the
					// Lifetime tier is NOT a reliable "belongs to this test"
					// signal here — the recorded timestamp is. An in-window
					// definitive match is the row the app read at this position
					// during recording; take it and stop. An out-of-window
					// definitive match (e.g. admin's pre-seed empty username
					// lookup recorded before the first test window) is kept only
					// as a last-resort fallback so a genuinely unique reusable
					// read still resolves.
					if !gate.allows(mock) {
						continue
					}
					if windowActive && mockInCurrentWindow(mock) {
						matchedResp, matchedMock, stmtMatched = &mock.Spec.MySQLResponses[0], mock, true
					} else if defExecMock == nil {
						defExecResp, defExecMock = &mock.Spec.MySQLResponses[0], mock
					}
				} else {
					// Not a definitive param-exact match. If the prepared
					// query text matches exactly AND this is a consumable
					// per-test data mock, remember the FIRST such mock in
					// recorded order as the FIFO fallback — used only if no
					// definitive match is found anywhere in the pool. This
					// makes a read-back of a replay-generated id (which
					// matches no recorded parameter) serve the row recorded
					// for that read-back position rather than an arbitrary
					// same-shape row chosen by score/first-wins.
					//
					// Track the closest candidate for the mismatch report even
					// when strict rejects it below — a query-exact candidate
					// beats a score-based one. Pre-strict, EXECUTE mismatches
					// reported an empty closest mock; this fills it.
					if queryExact && (bestPartialMock == nil || bestPartialQuery == "") {
						bestPartialMock, bestPartialQuery = mock, expectedQuery
					} else if bestPartialMock == nil && c > 0 {
						bestPartialMock, bestPartialQuery = mock, expectedQuery
					}
					// Strict gate: a query-exact candidate with drifted bound
					// parameters may only become a FIFO/score candidate when
					// the drifting params (body.parameters.N.value) are
					// covered by learned/user noise. One gate call covers both
					// the FIFO and score branches below.
					if (queryExact || c > maxMatchedCount) && !gate.allows(mock) {
						continue
					}
					if queryExact {
						if windowActive && mockInCurrentWindow(mock) {
							if fifoExecMockWindow == nil {
								fifoExecRespWindow, fifoExecMockWindow = &mock.Spec.MySQLResponses[0], mock
							}
						} else if fifoExecMock == nil {
							fifoExecResp, fifoExecMock = &mock.Spec.MySQLResponses[0], mock
						}
					}
					if c > maxMatchedCount {
						// fallback score-based candidate (used when no stmt info was available)
						maxMatchedCount, matchedResp, matchedMock = c, &mock.Spec.MySQLResponses[0], mock
					}
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

	// COM_QUERY in-window fallback. The scan above takes an in-window
	// exact-text match eagerly (queryMatched=true, loop broken). If it
	// found ONLY an out-of-window exact-text match (a reusable
	// single-recording query, or a stateful read whose matching row was
	// recorded in a different test's window), serve that recorded
	// candidate rather than dropping to the score-based partial pick.
	if req.Header.Type == sCOM_QUERY && !queryMatched && queryExactMock != nil {
		matchedResp, matchedMock, queryMatched = queryExactResp, queryExactMock, true
	}

	// COM_STMT_EXECUTE FIFO fallback. If the scan found no definitive
	// param-exact match (stmtMatched stays false) but did find a per-test
	// data mock whose prepared query matched exactly, prefer that
	// recorded-order candidate over any score-based same-shape pick. The
	// score path (maxMatchedCount) selects on header/flag/partial-param
	// similarity and is order-insensitive, so for repeated identical-shape
	// queries it can serve the wrong recorded row (e.g. admin's row for a
	// freshly-registered alice whose generated uuid matches no recorded
	// parameter). The FIFO candidate is the next UNCONSUMED mock for that
	// query in recorded order, giving a correct 1:1 mapping.
	if req.Header.Type == sCOM_STMT_EXEC && !stmtMatched {
		// Selection priority when no in-window definitive (query+params
		// exact) match was found during the scan:
		//   1. in-window per-test FIFO candidate (query-exact, params not
		//      matched — the read-back-of-generated-id case)
		//   2. any-tier per-test FIFO candidate
		//   3. out-of-window definitive match (query+params exact in a
		//      reusable tier — a genuinely unique reusable read)
		// The definitive out-of-window match ranks BELOW the in-window
		// FIFO so the current test's own recorded row always wins over a
		// stale earlier-test exact match; it ranks above the score-based
		// pick because exact query+params is strictly stronger evidence
		// than partial-shape scoring.
		chosenResp, chosenMock := fifoExecRespWindow, fifoExecMockWindow
		if chosenMock == nil {
			chosenResp, chosenMock = fifoExecResp, fifoExecMock
		}
		if chosenMock == nil && defExecMock != nil {
			chosenResp, chosenMock = defExecResp, defExecMock
		}
		if chosenMock != nil {
			if matchedMock == nil || matchedMock != chosenMock {
				logger.Debug("COM_STMT_EXECUTE FIFO fallback selected next unconsumed mock in recorded order",
					zap.String("mock_name", chosenMock.Name),
					zap.Bool("in_window", chosenMock == fifoExecMockWindow),
					zap.String("score_based_mock", func() string {
						if matchedMock != nil {
							return matchedMock.Name
						}
						return "<none>"
					}()))
			}
			matchedResp, matchedMock = chosenResp, chosenMock
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

					return generic, true, nil, nil
				}
			}
		}

		// COM_STMT_SEND_LONG_DATA streams a single parameter value to the
		// server ahead of COM_STMT_EXECUTE and, per the MySQL protocol, has
		// NO server response. Connector/J emits it for stream-bound
		// parameters (setBinaryStream / setBlob / setCharacterStream / large
		// setBytes), so any Java app writing a BLOB/CLOB hits this path. The
		// matcher has no per-mock comparison for it (the payload is just the
		// streamed bytes, already reflected in the subsequent EXECUTE's
		// recorded response), and the record window may legitimately not hold
		// a mock for it. Without graceful handling matchCommand falls through
		// to matchedResp==nil and query.go drops the connection with
		// "no matching mock" BEFORE its IsNoResponseCommand check — surfacing
		// to the client as SQLSTATE 08S01. Acknowledge it here: query.go sees
		// ok==true, runs no prepared-stmt cleanup, then its
		// IsNoResponseCommand branch continues without sending anything.
		if req.Header.Type == sCOM_STMT_SLD {
			// Consume the first recorded SEND_LONG_DATA mock (in-window
			// preferred, recorded order otherwise) so the recorder's
			// no-response SLD mocks are marked used instead of being flagged
			// unused / pruned. Fall back to plain synthetic acceptance when
			// the record window captured none.
			//
			// Historically the streamed payload was never content-compared at
			// all. Under schemaNoiseStrict each candidate is now gated through
			// the engine: a chunk whose data drifted outside learned/user
			// noise (body.data) cannot be consumed, and when every recorded
			// candidate is rejected that way the command is a real mismatch
			// instead of a silent pass. Detection diffs the consumed chunk and
			// learns the drift, so a later strict replay tolerates it.
			var sldMock, sldMockWindow, sldClosest *models.Mock
			for _, mock := range pool {
				if mock.Kind != models.MySQL {
					continue
				}
				isSLD := false
				for _, mr := range mock.Spec.MySQLRequests {
					if mr.PacketBundle.Header != nil && mr.PacketBundle.Header.Type == sCOM_STMT_SLD {
						isSLD = true
						break
					}
				}
				if !isSLD {
					continue
				}
				if sldClosest == nil {
					sldClosest = mock
				}
				if !gate.allows(mock) {
					continue
				}
				if windowActive && mockInCurrentWindow(mock) {
					if sldMockWindow == nil {
						sldMockWindow = mock
					}
				} else if sldMock == nil {
					sldMock = mock
				}
			}
			chosen := sldMockWindow
			if chosen == nil {
				chosen = sldMock
			}
			if chosen == nil && sldClosest != nil {
				// Recorded SLD mocks exist but strict rejected every one:
				// unmarked payload drift is a real mismatch, not a pass.
				logger.Debug("schema-noise strict: all recorded COM_STMT_SEND_LONG_DATA candidates rejected",
					zap.String("closest_mock", sldClosest.Name))
				return nil, false, &mockMiss{
					closestMock:    sldClosest.Name,
					fieldDiffs:     gate.fieldDiffs,
					strictRejected: gate.rejected,
				}, nil
			}
			if chosen != nil {
				var detected map[string][]string
				if liveBodyOK {
					detected, _ = noiseEngine.Detect(chosen, liveBody, userBodyNoise)
				}
				updateMock(ctx, logger, chosen, mockDb, detected)
			}
			logger.Debug("Accepting COM_STMT_SEND_LONG_DATA (no-response command)",
				zap.Bool("consumed_recorded_mock", chosen != nil))
			return &mysql.Response{}, true, nil, nil
		}

		// COM_STMT_RESET clears the cursor / long-data state of a server
		// prepared statement and is defined to return an OK packet on
		// success (ERR only if the statement ID is unknown). Connector/J
		// emits it opportunistically before re-executing a
		// ServerPreparedStatement when it suspects lingering state — a
		// path that the record run often does not exercise because the
		// recorded driver is single-tenant. Without a mock we used to
		// drop the connection here, which surfaces to the client as
		// SQLSTATE 08S01 (CommunicationsException). Since the packet is
		// stateless from the mock's perspective, synthesizing an OK is
		// correct protocol behavior.
		if req.Header.Type == sCOM_STMT_RESET {
			stmtID := uint32(0)
			if rp, ok := req.Message.(*mysql.StmtResetPacket); ok {
				stmtID = rp.StatementID
			}
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
			logger.Debug("Returning synthetic OK for unmocked COM_STMT_RESET",
				zap.Uint32("statement_id", stmtID))
			return generic, true, nil, nil
		}

		if req.Header.Type == sCOM_STMT_PREP {
			if sp, ok := req.Message.(*mysql.StmtPreparePacket); ok && sp != nil {
				numParams := uint16(strings.Count(sp.Query, "?"))
				newStmtID := decodeCtx.NextStmtID
				decodeCtx.NextStmtID++

				var paramDefs []*mysql.ColumnDefinition41
				if numParams > 0 {
					paramDefs = make([]*mysql.ColumnDefinition41, 0, numParams)
					for i := uint16(0); i < numParams; i++ {
						paramDefs = append(paramDefs, &mysql.ColumnDefinition41{
							Header: mysql.Header{
								PayloadLength: 22,
								SequenceID:    byte(2 + i),
							},
							Catalog:      "def",
							FixedLength:  0x0c,
							CharacterSet: 0,
							ColumnLength: 0,
							Type:         252,
							Flags:        0,
							Decimals:     0,
							Filler:       []byte{0x00, 0x00},
						})
					}
				}

				prepareOk := &mysql.StmtPrepareOkPacket{
					Status:             0,
					StatementID:        newStmtID,
					NumColumns:         0,
					NumParams:          numParams,
					Filler:             0,
					WarningAvailable:   true,
					WarningCount:       0,
					ParamDefs:          paramDefs,
					EOFAfterParamDefs:  []byte{},
					ColumnDefs:         nil,
					EOFAfterColumnDefs: []byte{},
				}

				seq := byte(1)
				if req.PacketBundle.Header != nil && req.PacketBundle.Header.Header != nil {
					seq = req.PacketBundle.Header.Header.SequenceID + 1
				}
				synthetic := &mysql.Response{
					PacketBundle: mysql.PacketBundle{
						Header: &mysql.PacketInfo{
							Header: &mysql.Header{PayloadLength: 12, SequenceID: seq},
							Type:   mysql.COM_STMT_PREPARE_OK,
						},
						Message: prepareOk,
					},
				}

				// Wire the synthetic stmtID into the runtime maps so
				// the subsequent EXECUTE can be resolved by query.
				if decodeCtx.PreparedStatements != nil {
					decodeCtx.PreparedStatements[newStmtID] = prepareOk
				}
				if decodeCtx.StmtIDToQuery != nil {
					decodeCtx.StmtIDToQuery[newStmtID] = sp.Query
				}

				logger.Info("Synthesized PREPARE_OK for unmocked statement (likely TiDB+JDBC cachePrepStmts caching pre-record stmtIDs)",
					zap.String("query", truncate(strings.TrimSpace(sp.Query), 200)),
					zap.Uint32("synthetic_stmt_id", newStmtID),
					zap.Uint16("num_params", numParams))
				return synthetic, true, nil, nil
			}
		}

		actualQuery := ""
		if qp, qok := req.Message.(*mysql.QueryPacket); qok {
			actualQuery = qp.Query
		} else if sp, spOk := req.Message.(*mysql.StmtPreparePacket); spOk {
			actualQuery = sp.Query
		}
		if bestPartialMock != nil {
			logger.Debug("mock miss",
				zap.String("protocol", "MySQL"),
				zap.String("type", req.Header.Type),
				zap.String("actual_query", truncate(actualQuery, 200)),
				zap.String("closest_mock", bestPartialMock.Name),
				zap.String("closest_query", truncate(bestPartialQuery, 200)))
		} else if actualQuery != "" {
			logger.Debug("mock miss",
				zap.String("protocol", "MySQL"),
				zap.String("type", req.Header.Type),
				zap.String("actual_query", truncate(actualQuery, 200)))
		}

		bestPartialMockName := ""
		if bestPartialMock != nil {
			bestPartialMockName = bestPartialMock.Name
		}
		if bestPartialMockName == "" {
			bestPartialMockName = gate.closestMock
		}
		return nil, false, &mockMiss{
			closestQuery:   bestPartialQuery,
			closestMock:    bestPartialMockName,
			fieldDiffs:     gate.fieldDiffs,
			strictRejected: gate.rejected,
		}, nil
	}

	// Schema-noise detection: diff the winning mock's recorded request body
	// against the live one and learn any NEW drifted field paths (beyond
	// user-configured noise and anything already learned) as req_body_noise.
	// A mock served via the lenient score/FIFO fallbacks is exactly a mock
	// whose request drifted — this is where that drift gets named. Detect
	// no-ops when detection is disabled or the winner has no diffable body
	// (utility commands). The learn is carried out on fresh copies inside
	// updateMock, never on the shared pooled mock.
	var detectedNoise map[string][]string
	if liveBodyOK {
		detectedNoise, _ = noiseEngine.Detect(matchedMock, liveBody, userBodyNoise)
		if len(detectedNoise) > 0 {
			paths := make([]string, 0, len(detectedNoise))
			for p := range detectedNoise {
				paths = append(paths, p)
			}
			logger.Debug("schema-noise detection: learned request-body drift on matched mock",
				zap.String("mock", matchedMock.Name),
				zap.String("request_type", req.Header.Type),
				zap.Strings("fields", paths))
		}
	}

	// Update the mock in the database BEFORE modifying the response
	// This ensures we update using the original mock state
	if okk := updateMock(ctx, logger, matchedMock, mockDb, detectedNoise); !okk {
		logger.Debug("failed to update the matched mock")
		// Re-fetch once to avoid spin
		return nil, false, nil, fmt.Errorf("failed to update matched mock")
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

				// Record in history for ID reuse tracking
				if decodeCtx.StmtHistory != nil {
					decodeCtx.StmtHistory.RecordPrepare(prepareOkRespCopy.StatementID, sp.Query)
					logger.Debug("Recorded PREP in history",
						zap.Uint32("stmt_id", prepareOkRespCopy.StatementID),
						zap.String("query", strings.TrimSpace(sp.Query)),
						zap.Uint32("cycle", decodeCtx.StmtHistory.GetCurrentCycle()),
						zap.Int("prepare_count", decodeCtx.StmtHistory.GetPrepareCountForQuery(sp.Query)))
				}

				logger.Debug("Recorded runtime PREP mapping with new statement ID",
					zap.Uint32("original_stmt_id from mock ", originalStmtID),
					zap.Uint32("new_stmt_id", prepareOkRespCopy.StatementID),
					zap.String("query", strings.TrimSpace(sp.Query)))
			}
		}
	}

	logger.Debug("matched command with the mock", zap.Any("mock", matchedMock.Name))
	return responseCopy, true, nil, nil
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

	// Count placeholders in both queries - this is crucial for PREPARE statements
	// to ensure we match mocks with the same number of parameters
	expectedPlaceholders := strings.Count(expectedQuery, "?")
	actualPlaceholders := strings.Count(actualQuery, "?")
	if expectedPlaceholders != actualPlaceholders {
		// log.Debug("placeholder count mismatch",
		// 	zap.String("expected_query", expectedQuery),
		// 	zap.String("actual_query", actualQuery),
		// 	zap.Int("expected_placeholders", expectedPlaceholders),
		// 	zap.Int("actual_placeholders", actualPlaceholders))
		return false, 0
	}

	if actual.Header != nil && actual.Header.Header != nil &&
		expected.Header != nil && expected.Header.Header != nil &&
		actual.Header.Header.PayloadLength == expected.Header.Header.PayloadLength {
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
		log.Debug("failed to get actual query structure",
			zap.String("actual Query", actualQuery),
			zap.Error(err))
		return false, matchCount
	}

	expectedSignature, err := getQueryStructureCached(expectedQuery)
	if err != nil {
		log.Debug("failed to get expected query structure",
			zap.String("expected Query", expectedQuery),
			zap.Error(err))
		return false, matchCount
	}

	if expectedSignature == actualSignature {
		log.Debug("query structure matched",
			zap.String("expected signature", expectedSignature),
			zap.String("actual signature", actualSignature))
		return false, matchCount + 6
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
//   - If both expectedQuery and actualQuery are present, require them to match (exact).
//     If they don't match, return (false, 0) immediately.
//   - If either query is missing, fall back to best-effort scoring (returns (false, score)).
//
// Returns (definitive, score, queryExactMatched). queryExactMatched is true
// when the recorded prepared-statement query text equals the live query text
// (case-insensitive) regardless of whether the bound parameters matched. The
// caller uses this third value to drive a FIFO fallback: when no candidate is
// a definitive param-exact match, the next UNCONSUMED per-test mock for the
// same query (in recorded order) is served, so an INSERT-then-SELECT read-back
// of a replay-generated id returns the row that was read back at record time.
func matchStmtExecutePacketQueryAware(logger *zap.Logger, expected, actual mysql.PacketBundle, expectedQuery, actualQuery string, mockName string, nc *util.NoiseChecker) (bool, int, bool) {
	matchCount := 0

	// Match the type and return zero if the types are not equal
	if expected.Header.Type != actual.Header.Type {
		return false, 0, false
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
				valueEqual = paramValueEqual(ep.Value, ap.Value, nc)
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
	queryExactMatched := false
	eq := strings.TrimSpace(expectedQuery)
	aq := strings.TrimSpace(actualQuery)

	// If both queries are present, require them to match (exact or structural) for a definitive match.
	if eq != "" && aq != "" {
		// If both queries are available, require an exact or structural match to treat this as a definitive match.
		if strings.EqualFold(eq, aq) {
			matchCount += 10
			queryMatched = true
			queryExactMatched = true
			logger.Debug("query matched exactly", zap.String("related stmt-exec mock-name", mockName))
		} else if sigE, errE := getQueryStructureCached(eq); errE == nil {
			if sigA, errA := getQueryStructureCached(aq); errA == nil && sigE == sigA {
				matchCount += 6
				queryMatched = false
				logger.Debug("query structure matched", zap.String("related stmt-exec mock-name", mockName))
			}
		}
	}

	if allParamsMatched && eq == "" && aq == "" {
		return true, matchCount, queryExactMatched
	}

	if allParamsMatched && eq == "" {
		logger.Debug("EXECUTE matched on params alone (mock has no recorded PREPARE)",
			zap.String("mock-name", mockName),
			zap.String("actual_query", truncate(aq, 200)))
		return true, matchCount, queryExactMatched
	}

	if !queryMatched || !allParamsMatched {
		return false, matchCount, queryExactMatched
	}

	// Both queryMatched and allParamsMatched must be true for a definitive match. Otherwise, return the best-effort score.
	return (queryMatched && allParamsMatched), matchCount, queryExactMatched
}

func paramValueEqual(a, b interface{}, nc *util.NoiseChecker) bool {
	if nc.IsNoisyValue(a) {
		return true
	}
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

// updateMock processes the matched mock based on its Lifetime. Per-test
// mocks are CONSUMED on match (DeleteFilteredMock); session / connection
// mocks are RETAINED and updated in place (UpdateUnFilteredMock).
//
// Pre-Phase-2, every MySQL mock lived in the unfiltered/session tree
// (via the legacy kind-fallback) and update-in-place was the only
// correct path. Post-Phase-2, data mocks tagged "mocks" land in the
// per-test tree; calling UpdateUnFilteredMock on them returns false
// because the mock isn't in m.unfiltered — surfacing as a spurious
// "failed to update matched mock" error after a successful match.
//
// Defensive fallback: if Lifetime is still the zero value (a mock that
// somehow reached the matcher without DeriveLifetime having run) AND
// the raw tag says "config", treat as session. This mirrors the
// matcher's session-skip check for consistency.
// Concurrency note: matchedMock is a shared pointer from the mock
// pool. See the HTTP equivalent in pkg/agent/proxy/integrations/http/
// match.go for the rationale — we build a fresh copy and mutate the
// copy rather than the pool pointer, so concurrent goroutines that
// match the same session-lifetime mock don't race on TestModeInfo.
// updateMock processes the matched mock based on its Lifetime. detectedNoise
// carries any request-body drift the schema-noise engine detected this match;
// it is merged onto FRESH copies only (never the shared pooled mock's map —
// see the HTTP updateMock's concurrency note, the same pooled-pointer race
// applies here) and reaches persistence through the same
// DeleteFilteredMock/UpdateUnFilteredMock paths as HTTP's learned noise.
func updateMock(_ context.Context, logger *zap.Logger, matchedMock *models.Mock, mockDb integrations.MockMemDb, detectedNoise map[string][]string) bool {
	updatedMock := *matchedMock
	updatedMock.TestModeInfo.IsFiltered = false
	updatedMock.TestModeInfo.SortOrder = pkg.GetNextSortNum()
	if len(detectedNoise) > 0 {
		updatedMock.Spec.ReqBodyNoise = schemanoise.MergeLearned(updatedMock.Spec.ReqBodyNoise, detectedNoise)
	}

	lifetime := updatedMock.TestModeInfo.Lifetime
	rawConfig := false
	if updatedMock.Spec.Metadata != nil {
		rawConfig = updatedMock.Spec.Metadata["type"] == "config"
	}
	isSessionOrConnection := lifetime == models.LifetimeSession ||
		lifetime == models.LifetimeConnection ||
		(lifetime == models.LifetimePerTest && rawConfig)

	if isSessionOrConnection {
		updated := mockDb.UpdateUnFilteredMock(matchedMock, &updatedMock)
		if !updated {
			logger.Debug("failed to update matched session/connection mock",
				zap.String("mock", updatedMock.Name),
				zap.Stringer("lifetime", lifetime))
		}
		return updated
	}

	// Per-test: consume via DeleteFilteredMock. Fallback to
	// UpdateUnFilteredMock if the mock has been staged into the
	// session pool during the initial pre-first-test window (see
	// SetMocksWithWindow's isInitialStaging branch) — the mock is
	// still classified as LifetimePerTest but physically lives in
	// the session tree until the first real test's re-partition.
	//
	// DeleteFilteredMock keys the tree lookup on TestModeInfo, so the
	// delete-key mock keeps the original (unmutated) TestModeInfo but
	// carries the detected noise on a fresh ReqBodyNoise map — this is how
	// the noise gets reported on the consumed per-test mock (mirrors HTTP).
	deleteMock := *matchedMock
	if len(detectedNoise) > 0 {
		deleteMock.Spec.ReqBodyNoise = schemanoise.MergeLearned(deleteMock.Spec.ReqBodyNoise, detectedNoise)
	}
	if mockDb.DeleteFilteredMock(deleteMock) {
		return true
	}
	if mockDb.UpdateUnFilteredMock(matchedMock, &updatedMock) {
		return true
	}
	logger.Debug("failed to delete or update matched per-test mock",
		zap.String("mock", updatedMock.Name))
	return false
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
	// Track prepare order per connection per query
	queryCountPerConn := make(map[string]map[string]int) // connID → normalized_query → count

	for _, m := range unfiltered {
		if m == nil || m.Kind != models.MySQL {
			continue
		}
		// MySQL matcher now reads the typed Lifetime with a defensive
		// fallback to the raw metadata tag. This handles both the
		// fully-migrated path (DeriveLifetime has run, Lifetime is
		// set) and the edge case where a mock reached the pool
		// without DeriveLifetime having set Lifetime — the raw tag
		// still says config so we skip it correctly.
		if m.TestModeInfo.Lifetime == models.LifetimeSession ||
			(m.TestModeInfo.Lifetime == models.LifetimePerTest && hasConfigTag(m)) {
			continue
		}
		connID := ""
		if m.Spec.Metadata != nil {
			connID = m.Spec.Metadata["connID"]
		}

		// Initialize query counter for this connection
		if _, ok := queryCountPerConn[connID]; !ok {
			queryCountPerConn[connID] = make(map[string]int)
		}

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

		// Track prepare order for this query
		normalizedQuery := strings.ToLower(prepQuery)
		queryCountPerConn[connID][normalizedQuery]++

		out[connID] = append(out[connID], prepEntry{
			statementID:  stmtID,
			query:        prepQuery,
			mockName:     m.Name,
			prepareOrder: queryCountPerConn[connID][normalizedQuery],
			wasClosed:    false, // Will be updated by markClosedEntries
		})
	}

	// Second pass: mark entries that were closed
	markClosedEntries(unfiltered, out)

	return out
}

// markClosedEntries scans CLOSE commands and marks corresponding prepEntries
func markClosedEntries(unfiltered []*models.Mock, idx map[string][]prepEntry) {
	for _, m := range unfiltered {
		if m == nil || m.Kind != models.MySQL {
			continue
		}
		if m.Spec.Metadata["type"] == "config" {
			continue
		}
		connID := m.Spec.Metadata["connID"]

		for _, req := range m.Spec.MySQLRequests {
			if req.Header.Type == mysql.CommandStatusToString(mysql.COM_STMT_CLOSE) {
				if cp, ok := req.Message.(*mysql.StmtClosePacket); ok && cp != nil {
					// Find and mark the corresponding prepEntry as closed
					entries := idx[connID]
					for i := range entries {
						if entries[i].statementID == cp.StatementID && !entries[i].wasClosed {
							entries[i].wasClosed = true
							break
						}
					}
				}
			}
		}
	}
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

// NOTE: Statement ID reuse is handled via PreparedStmtHistory tracking.
// The system maintains full lifecycle history including:
// - Multiple prepare cycles for the same query (prepareOrder field)
// - Closure tracking to disambiguate ID reuse (wasClosed field)
// - Runtime history in DecodeContext.StmtHistory
//
// Supported scenarios:
// 1. Prepare→Close→Prepare→Close→Prepare→Execute (same query, different IDs)
// 2. Multiple prepares without closing (same query gets multiple IDs)
//
// See PreparedStmtHistory in wire/util.go for implementation details.

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
