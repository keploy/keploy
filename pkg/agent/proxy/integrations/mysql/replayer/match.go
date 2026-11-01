package replayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	mysqlutils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	// vitesslog must load before any vitess code logs: see that package.
	_ "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/vitesslog"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"vitess.io/vitess/go/vt/sqlparser"
)

// stmtIdentityCacheSize bounds stmtIdentityCache. Same reasoning as
// querySigCacheSize below: the key is the raw SQL text and that key space is not
// finite (a tracer mints a fresh traceparent per request), while the value is a
// pure function of the key, so an eviction costs one re-scan and can never
// change a match verdict.
const stmtIdentityCacheSize = 2048

var stmtIdentityCache = newStmtIdentityCache()

// newStmtIdentityCache builds the memo as a 2Q cache, not a plain LRU, for the
// reason spelled out on newQuerySigCache: the COM_STMT_EXECUTE and
// COM_STMT_CLOSE comparisons re-derive the LIVE statement's identity once per
// candidate while scanning the pool, so under plain-LRU
// recency a pool holding more distinct texts than the cap would evict it between
// candidates and re-scan it N times per command.
func newStmtIdentityCache() *lru.TwoQueueCache[string, string] {
	c, err := lru.New2Q[string, string](stmtIdentityCacheSize)
	if err != nil {
		// New2Q only errors on a non-positive size and the const is > 0, so this
		// is unreachable — fail loudly rather than leave a nil cache.
		panic(fmt.Sprintf("mysql replayer: invalid stmtIdentityCacheSize %d: %v", stmtIdentityCacheSize, err))
	}
	return c
}

// querySigCacheSize bounds querySigCache.
//
// The cache is keyed by the RAW SQL text, and that key space is NOT finite:
// a client that inlines literals (`... WHERE id = 91827`) mints a brand new
// key for every request, so a sync.Map here grows for as long as the agent
// runs. The value, by contrast, is a pure memo — getQueryStructure builds a
// fresh parser, parses, and joins the AST node type names, with no package or
// closure state — so evicting an entry only costs one re-parse and can never
// change a match verdict.
//
// Entry cost, measured against the pinned vitess: the signature runs 3-13x the
// length of the SQL text (it is the joined reflect type name of every AST
// node), so a realistic entry is 0.5-1.8 KB and this cap is a few MB. Multi-KB
// ORM statements cost proportionally more; the cap is on entries, not bytes.
const querySigCacheSize = 2048

var querySigCache = newQuerySigCache()

// newQuerySigCache builds the memo as a 2Q cache rather than a plain LRU.
// matchCommand rescans the entire candidate mock pool for every live command
// and looks up the live query's signature once per candidate, so a pool with
// more distinct SQL texts than the cap would evict the live query between
// candidates under plain-LRU recency and re-parse it N times per command —
// strictly worse than the unbounded map this replaces. 2Q keeps the
// repeatedly-touched live query in its frequent list while the once-touched
// candidates churn through the smaller recent list.
func newQuerySigCache() *lru.TwoQueueCache[string, string] {
	c, err := lru.New2Q[string, string](querySigCacheSize)
	if err != nil {
		// New2Q only errors on a non-positive size and querySigCacheSize is a
		// const > 0, so this is unreachable — but fail loudly rather than
		// leave a nil cache that panics on the first match.
		panic(fmt.Sprintf("mysql replayer: invalid querySigCacheSize %d: %v", querySigCacheSize, err))
	}
	return c
}

// recorded PREP registry per recorded connection
type prepEntry struct { // minimal, enough for lookup
	statementID uint32
	query       string
	mockName    string // for debugging purpose
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
	if v, ok := querySigCache.Get(sql); ok {
		return v, nil
	}
	sig, err := getQueryStructure(sql)
	if err == nil {
		querySigCache.Add(sql, sig)
	}
	return sig, err
}

// exactDriftNextSteps is what a miss says when, under exact serving,
// recordings of the command's statement were turned away because a parameter
// drifted outside known noise (strictGate.allowsDrift). It names the two ways
// to mark a field that work under exact serving, the field written as
// listed. First configured noise in keploy.yml
// (test.globalNoise.global.requestBody, requestBodyNoise): it covers every
// recording of the statement and survives a re-record. A shipped keploy
// (built with -tags=viper_bind_struct, as goreleaser, the Dockerfile and
// the release script build it) loads a dotted key there whole. Then the
// recording's own noise (the noise.req list of its document in mocks.yaml,
// MockSpec.ReqBodyNoise): one recording, which a re-record discards. Nothing
// learns MySQL noise there: the drifted recording is never served, so
// detect-on-serve never sees it, and the proven switch's learn round does
// not cover MySQL.
const exactDriftNextSteps = "Each recording of this statement was made with other parameters (the listed fields), which are not noise: keploy does not answer a call with another call's rows. If the application sends a value that changes on every run, mark the listed field as noise, written as listed, in keploy.yml, under test.globalNoise.global.requestBody: {\"body.parameters.0.value\": []}. Or, for one recording, add it to the noise.req list of the recording's document in mocks.yaml. Otherwise the call changed since it was recorded: re-record the test."

// exactLiteralNextSteps is the same, for a COM_QUERY whose inline literals,
// or the query attributes its statement reads, drifted
// (strictGate.allowsLiterals).
const exactLiteralNextSteps = "Each recording of this statement was made with other inline literals or query attributes (the listed fields; literals by position), which are not noise: keploy does not answer a call with another call's rows. If the application sends a value that changes on every run, mark the listed field as noise, written as listed, in keploy.yml, under test.globalNoise.global.requestBody: {\"body.query.literals.0\": []}. Or, for one recording, add it to the noise.req list of the recording's document in mocks.yaml. Otherwise the call changed since it was recorded: re-record the test."

// sameQueryStructure reports whether two statements parse to the same tree
// shape (getQueryStructure: identifier-blind). Neither parsing is no.
func sameQueryStructure(a, b string) bool {
	sigA, errA := getQueryStructureCached(a)
	if errA != nil {
		return false
	}
	sigB, errB := getQueryStructureCached(b)
	return errB == nil && sigA == sigB
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
	expectedMessage, ok := expected.Message.(*mysql.SSLRequestPacket)
	if !ok {
		return fmt.Errorf("the recorded SSL request cannot be read: it holds a %T", expected.Message)
	}
	actualMessage, ok := actual.Message.(*mysql.SSLRequestPacket)
	if !ok {
		return fmt.Errorf("the client's SSL request cannot be read: it holds a %T", actual.Message)
	}

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

	// The header says HandshakeResponse41; a recorded message of another
	// type (a malformed recording) is no match, not a panic.
	exp, ok := expected.Message.(*mysql.HandshakeResponse41Packet)
	if !ok {
		return fmt.Errorf("the recorded handshake response cannot be read: it holds a %T", expected.Message)
	}
	act, ok := actual.Message.(*mysql.HandshakeResponse41Packet)
	if !ok {
		return fmt.Errorf("the client's handshake response cannot be read: it holds a %T", actual.Message)
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
	if h := configTagReadHook.Load(); h != nil {
		(*h)()
	}
	return m != nil && m.Spec.Metadata != nil && m.Spec.Metadata["type"] == "config"
}

// configTagReadHook, when set, is called on every read of a mock's config tag
// (hasConfigTag), so a test can count them. Replay never sets it.
var configTagReadHook atomic.Pointer[func()]

// skippedAtCommandPhase reports whether the command phase skips mock: a
// session-tier or config mock (its handshake and auth were matched at the
// command prologue) that is not a session-reusable command. The shape check
// runs first: a candidate that is one COM_* command, as nearly every data
// mock is, is kept whatever its tier, so its tier is never read. The config
// tag is a map lookup on the mock's metadata, and was the costliest part of
// the filter for a command that walks thousands of candidates.
func skippedAtCommandPhase(mock *models.Mock) bool {
	return !isSessionReusableCommandMock(mock) &&
		(mock.TestModeInfo.Lifetime == models.LifetimeSession ||
			(mock.TestModeInfo.Lifetime == models.LifetimePerTest && hasConfigTag(mock)))
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

// matchOptions adjusts matchCommandWith for tests; the replay path uses the zero
// value, through matchCommand.
type matchOptions struct {
	// fullScan skips the in-window pass, so a test can check that both scans
	// serve, consume and report the same.
	fullScan bool
	// examined, when set, counts the candidates the scan visits, so a test can
	// check what a command costs without timing it.
	examined *int
	// bodiesBuilt, when set, counts the live request bodies the command
	// builds (strictGate.liveRequestBody).
	bodiesBuilt *int
	// compared, when set, counts the candidates the strict gate compares
	// with the live request (strictGate.compare), so a test can check what
	// a command costs without timing it.
	compared *int
	// eagerGate gates each fallback of a COM_STMT_EXECUTE as the walk meets
	// it, as the matcher did before it deferred the gate past the walk
	// (execFallbacks): the reference a test checks that the deferred gate
	// serves, reports and learns the same against.
	eagerGate bool
	// probed, when set, makes the match a dry one (ownRepeats): the scan picks
	// the recording that would answer the command, and the match stops there
	// and writes it to *probed (nil when no recording would), before any
	// synthesized answer, consume, noise learning, prepared-statement
	// bookkeeping or served event.
	probed **models.Mock
	// eagerBody builds the live request body before the scan, whether or not
	// strict enforcement or detection reads it, as the matcher did before it
	// built the body on first read: the reference a test checks that the
	// lazy build serves, reports and learns the same against.
	eagerBody bool
	// refused are the copies whose consume the store refused on this
	// command's earlier matches (matchCommand): the scan passes over them.
	refused map[*models.Mock]struct{}
}

// matchCommand matches one live command-phase request against the mock pool.
// noiseEngine carries the resolved schema-noise flags (nil-safe: a nil engine
// disables both detection and strict). userBodyNoise is the user's
// test.globalNoise.requestBody bucket (root-relative lowercased paths) so
// configured noise participates in strict gating with the same vocabulary as
// learned req_body_noise. miss is non-nil only when ok is false without error.
//
// Under exact serving the command's round is marked before the matcher reads
// the store (integrations.MarkCallRound), and the matcher and its miss read
// the store held to that mark (integrations.HoldToRound): once the replay
// stages a test set again, the command finds nothing of it to be answered
// with, nor compared with, and misses saying so (restagedCommandNote). A
// store that marks the command's round but cannot hold it reads nothing for
// it, and the miss says why (integrations.UnheldWhy).
func matchCommand(ctx context.Context, logger *zap.Logger, req mysql.Request, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext, noiseEngine *schemanoise.Engine, userBodyNoise map[string][]string) (*mysql.Response, bool, *mockMiss, error) {
	call := integrations.MarkCallRound(mockDb)
	db, restaged := integrations.HoldToRound(mockDb, call)
	// A command one of the open test's own recordings is, answered or not,
	// as the matcher judges it (ownRepeats), is matched with the set's
	// background hidden (integrations.OwnRepeatRead) once the background is
	// about to answer it: no pool or worker recording answers a command one
	// more than the test recorded, which then misses as such, its background
	// copy left unspent, even when two of them are run at once (the store
	// refuses the copy, and the retry below reads it hidden). A command the
	// test's own recordings answer never asks.
	var own []*models.Mock
	held := db
	db = integrations.OwnRepeatRead(db, call, func() bool {
		own = ownCommands.Of(held, commandType(req.PacketBundle))
		return len(ownRepeats(ctx, req, held, own, decodeCtx, noiseEngine, userBodyNoise, 1)) > 0
	})
	var opts matchOptions
	resp, ok, miss, err := matchCommandWith(ctx, logger, req, db, decodeCtx, noiseEngine, userBodyNoise, opts)
	// A consume the store refused (integrations.ConsumeRefused: under exact
	// serving, a test mock that already answered, perhaps on another
	// connection, or one its view no longer shows) does not answer: the
	// command is matched again against what the store shows now, without the
	// copies it refused, and is answered or misses. N connections that run
	// one statement with the same parameters at once all read its first
	// recording; one of them is answered with it, and each of the others with
	// the next one it can take. Each match again passes over every copy the
	// store refused before, so no copy is asked twice: the matches end with
	// the copies the store lists. Without exact serving the refusal is failed
	// bookkeeping, and stays the error it was.
	for {
		var refused *refusedConsume
		if !errors.As(err, &refused) || !integrations.ConsumeRefused(db, refused.mock.Name) {
			break
		}
		if opts.refused == nil {
			opts.refused = make(map[*models.Mock]struct{})
		}
		opts.refused[refused.mock] = struct{}{}
		resp, ok, miss, err = matchCommandWith(ctx, logger, req, db, decodeCtx, noiseEngine, userBodyNoise, opts)
	}
	// A command that missed before the background could answer it asks now
	// whether it is one of the open test's own (integrations.OwnRepeat): one
	// is matched again as it reads the store, with the background hidden, so
	// its miss is diagnosed as one command more than the test recorded, never
	// against the background.
	hidden := miss != nil && integrations.OwnRepeat(db)
	if hidden && err == nil {
		resp, ok, miss, err = matchCommandWith(ctx, logger, req, db, decodeCtx, noiseEngine, userBodyNoise, opts)
	}
	// A store that marked the command's round but could not hold it read
	// nothing for it (integrations.HoldToRound fails closed): the miss says
	// why.
	unheld := integrations.UnheldWhy(db)
	if !ok && (err == nil || errors.Is(err, errConsumeRefused)) && (restaged() || unheld != "") {
		// A consume the hold refused is the restage's miss too.
		err = nil
		if miss == nil {
			miss = &mockMiss{matchPhase: models.MatchPhaseExhausted}
		}
		miss.restaged = true
		miss.nextSteps = restagedCommandNote
		if unheld != "" {
			miss.nextSteps = unheld
		}
	}
	if miss != nil {
		miss.call = call
		// One command more than the open test recorded: its own recordings
		// of it answered, and the background was hidden from it.
		if !miss.restaged && hidden {
			miss.extraCall = len(ownRepeats(ctx, req, held, own, decodeCtx, noiseEngine, userBodyNoise, 0))
		}
	}
	return resp, ok, miss, err
}

// errConsumeRefused is the matcher's error when the store refused the
// consume of the mock it matched.
var errConsumeRefused = errors.New("failed to update matched mock")

// refusedConsume is errConsumeRefused for the copy of a mock whose consume
// the store refused.
type refusedConsume struct{ mock *models.Mock }

func (e *refusedConsume) Error() string { return errConsumeRefused.Error() }
func (e *refusedConsume) Unwrap() error { return errConsumeRefused }

// restagedCommandNote is what a miss says when the replay staged a test set
// again (the next set, or the same set for a replaced agent, which the store
// does not tell apart) after the command's first read of the round, and the
// matcher, held to that read, found nothing to answer it with.
const restagedCommandNote = "keploy replay staged a test set again while it decided this command, so keploy answered it with no recording: a command is never answered with, nor compared with, a recording of a staging made after it began."

// matchCommandWith is matchCommand with test options.
func matchCommandWith(ctx context.Context, logger *zap.Logger, req mysql.Request, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext, noiseEngine *schemanoise.Engine, userBodyNoise map[string][]string, opts matchOptions) (*mysql.Response, bool, *mockMiss, error) {
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

	connID := ""
	if v := ctx.Value(models.ClientConnectionIDKey); v != nil {
		if s, ok := v.(string); ok {
			connID = s
		}
	}

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
	// lies within the active outer-test window, or the mock was in flight
	// when the window's test began (sent in the gap before it and answered
	// inside it: integrations.InFlightOwner), which the windowed read serves
	// as the test's own. When no window is active (initial staging / between
	// tests) every mock is treated as in-window so behaviour matches the
	// pre-fix path.
	mockInCurrentWindow := func(mk *models.Mock) bool {
		if !windowActive {
			return true
		}
		req := mk.Spec.ReqTimestampMock
		if req.IsZero() {
			return true
		}
		if !req.Before(winStart) && !req.After(winEnd) {
			return true
		}
		return integrations.InFlightIntoOpenTest(mockDb, mk)
	}

	// testsOwn reports whether an EXECUTE candidate is the open test's own,
	// the one the EXECUTE ranking below prefers to every other recording.
	// While a window is active that is a recording inside it. Under exact
	// serving no window is (CurrentTestWindow is zero: the test's mapping
	// says what it is served, no recorded time), and the test's own are its
	// mapping's (integrations.OwnedByOpenTest). Read by the window alone, no
	// recording was the test's own there, and the first recording of the
	// statement whose parameters differ (the FIFO fallback) outranked the one
	// recorded with the command's own parameters: a statement run twice with
	// different parameters was answered with each other's rows, and N
	// connections running it at once all picked the same recording, so all
	// but one were refused.
	exactServing := integrations.ExactServing(mockDb)
	// sameStatementOnly is the rule of a fallback under exact serving: a
	// recording that is not a match on the command's statement and every
	// parameter answers it only when it ran the command's own statement.
	// For a COM_QUERY or COM_STMT_PREPARE that is the same text once inline
	// literals are masked (maskSQLLiterals: a COM_QUERY's literals are its
	// parameters), and a COM_QUERY's recording answers only when every
	// literal that differs, by position, and every query attribute the
	// statement reads, by name (readAttributes: the others it is sent with
	// are not compared), is configured or learned noise, comments aside
	// (strictGate.allowsLiterals): with another literal it is the rows of
	// another call. That holds for a recording of the same text too when
	// the statement reads its query attributes (liveReadsAttributes): the
	// text does not hold them, and with another value of one it is another
	// call's rows (another tenant's). A PREPARE's recording answers with
	// another literal: its answer is the statement's metadata, and each
	// EXECUTE of the statement is matched on the live text. For a
	// COM_STMT_EXECUTE it is the same prepared statement
	// (queryExact: sqlStatementIdentity, case-insensitive), which the FIFO
	// candidate is, so no EXECUTE is answered by score; and the FIFO
	// candidate answers only when every parameter that differs is
	// configured or learned noise (strictGate.allowsDrift): with another
	// parameter it is the rows of another call. A recording of another
	// statement of the same shape (getQueryStructure is identifier-blind:
	// another table, other columns), or, for an EXECUTE, of a statement
	// unknown on either side, never answers there: it was served by score,
	// and the app read rows its statement never returned. An EXECUTE
	// recorded without its statement answers with every parameter equal
	// (paramsOnlyMock) only when the command's statement is unknown too:
	// against a known one it may be another statement's rows. Without
	// exact serving those still answer, as before: keploy test's lenient
	// tier. A recording of another statement of another shape never
	// scores, in either mode (matchStmtExecutePacketQueryAware,
	// matchQueryLive).
	sameStatementOnly := exactServing
	testsOwn := func(mk *models.Mock) bool {
		if exactServing {
			return integrations.OwnedByOpenTest(mockDb, mk)
		}
		return windowActive && mockInCurrentWindow(mk)
	}

	// recordedInATestsWindow reports whether a recording was made inside the
	// window of some recorded test of the set. One made at start-up or
	// between two tests is no test's; one made inside a test's window that
	// is not the open test's own is another test's, which that test's call
	// will want. With no recorded windows (the replay sent none) it reports
	// false, and no recording is told apart.
	// Without exact serving only: under exact serving the view shows no
	// other test's recordings, and what is not the open test's own ranks
	// in the view's order (#4940).
	testWindows, _ := mockDb.(integrations.TestWindows)
	recordedInATestsWindow := func(mk *models.Mock) bool {
		if testWindows == nil || exactServing {
			return false
		}
		inside, _ := testWindows.InRecordedTestWindow(mk.Spec.ReqTimestampMock)
		return inside
	}

	// outsideGapRank ranks a recording made outside every recorded test's
	// window by the gap it was made in, against the gaps the command may
	// fall in, without exact serving: 0 for ownGap, 1 for otherGap, 2 for
	// any other gap (or with no recorded windows). Gap -1 is the app's
	// start-up, gap i the time after test i's window. keploy test keeps a
	// test's window open until it stages the next test, and sends the next
	// test as soon as the previous one is answered, so a command the app
	// made between two tests is usually replayed inside the next test's
	// window: with a window open, ownGap is the gap before it and otherGap
	// the gap after it. With none open, before the first test, both are the
	// app's start-up. The session pool keeps such a recording after it
	// answers, so a request made between every two tests is answered with
	// its own gap's recording, not an earlier gap's, which may hold other
	// rows. A known limit: a command replayed while the window of the test
	// before it is still open (the replay had not yet sent the next test)
	// takes the recording of its request made in the gap before that
	// window when there is one, an earlier gap's; and a command replayed two
	// or more tests after the gap it was made in takes the gap before the
	// open window's, a later gap's, when there is one. The matcher has no
	// event telling these timings apart. When neither gap holds a recording of
	// the request, another gap's answers (the first in the pool's order),
	// still above another test's and the FIFO candidate.
	windowIndex, _ := mockDb.(integrations.RecordedWindowIndex)
	ownGap, otherGap, gapsKnown := -1, -1, false
	if windowIndex != nil && !exactServing {
		var opened time.Time // zero, before every window: start-up
		if windowActive {
			opened = winStart
		}
		var open int
		open, gapsKnown = windowIndex.RecordedWindowIndex(opened)
		if windowActive {
			ownGap, otherGap = open-1, open
		}
	}
	outsideGapRank := func(mk *models.Mock) int {
		if !gapsKnown {
			return 2
		}
		gap, known := windowIndex.RecordedWindowIndex(mk.Spec.ReqTimestampMock)
		switch {
		case !known:
			return 2
		case gap == ownGap:
			return 0
		case gap == otherGap:
			return 1
		}
		return 2
	}

	// pool is the full merged pool, filled on first need. When the in-window
	// pass has read the per-test and connection tiers, they are kept in
	// perTestTier / connectionTier (tiersRead) and only the session tier is
	// read again.
	var pool, perTestTier, connectionTier []*models.Mock
	tiersRead := false

	// fetchTiers reads the three pools the scan merges. sessionOf is the
	// session-tier read: the whole GetSessionMocks snapshot for the full scan,
	// or its in-window narrowing for the in-window pass.
	//
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
	//
	// With tiersRead set (the in-window pass already holds the per-test and
	// connection tiers), only the session tier is read.
	//
	// fetchConnectionTier is the connection tier's read.
	//
	// Unification Phase 2.5: prepared-statement setup mocks are tagged
	// type=connection by the recorder (see
	// pkg/agent/proxy/integrations/mysql/recorder/query.go) and live in
	// their own per-connID pool. Fetch them explicitly here so
	// buildRecordedPrepIndex can include them; GetConnectionMocks
	// returns an empty slice when no connection-scoped mocks exist, so
	// this is a no-op for apps that don't use PREPARE.
	fetchConnectionTier := func() ([]*models.Mock, error) {
		if connID == "" {
			return nil, nil
		}
		cm, cerr := mockDb.GetConnectionMocks(connID)
		if cerr != nil {
			// Hard-fail for prepared-statement traffic: without the
			// connection pool we can't resolve PREPARE↔EXECUTE pairs
			// and the later "no matching mock" would mask the real
			// root cause. Other command types tolerate the failure
			// (connection pool is advisory for them) — log + continue.
			if req.Header.Type == sCOM_STMT_PREP || req.Header.Type == sCOM_STMT_EXEC {
				utils.LogError(logger, cerr, "failed to get mysql connection mocks", zap.String("connID", connID))
				return nil, fmt.Errorf("failed to get mysql connection mocks for connID %q: %w", connID, cerr)
			}
			logger.Debug("failed to get mysql connection mocks; proceeding without per-connID pool",
				zap.String("connID", connID),
				zap.Error(cerr))
			return nil, nil
		}
		return cm, nil
	}
	fetchTiers := func(sessionOf func() ([]*models.Mock, error)) (perTestMocks, sessionMocks, connectionMocks []*models.Mock, err error) {
		if !tiersRead {
			perTestMocks, err = mockDb.GetPerTestMocksInWindow()
			if err != nil {
				if ctx.Err() != nil {
					return nil, nil, nil, ctx.Err()
				}
				utils.LogError(logger, err, "failed to get per-test mocks")
				return nil, nil, nil, err
			}
		}
		sessionMocks, err = sessionOf()
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, nil, ctx.Err()
			}
			utils.LogError(logger, err, "failed to get session mocks")
			return nil, nil, nil, err
		}

		if !tiersRead {
			if connectionMocks, err = fetchConnectionTier(); err != nil {
				return nil, nil, nil, err
			}
		}
		return perTestMocks, sessionMocks, connectionMocks, nil
	}

	// Merge pools with per-test FIRST so a per-test data query wins over
	// a session-level catch-all when both happen to match. Connection-
	// scoped setups come last so buildRecordedPrepIndex / stmtMocks
	// naturally pick them up without needing a new priority order.
	merge := func(tiers ...[]*models.Mock) []*models.Mock {
		n := 0
		for _, t := range tiers {
			n += len(t)
		}
		out := make([]*models.Mock, 0, n)
		for _, t := range tiers {
			out = append(out, t...)
		}
		return out
	}

	// pool is the full merged pool; loadPool fills it on first need.
	loadPool := func() error {
		var sessionMocks []*models.Mock
		var err error
		if tiersRead {
			_, sessionMocks, _, err = fetchTiers(mockDb.GetSessionMocks)
		} else {
			perTestTier, sessionMocks, connectionTier, err = fetchTiers(mockDb.GetSessionMocks)
		}
		if err != nil {
			return err
		}
		tiersRead = true // recordedQuery reads these tiers, not a second snapshot
		pool = merge(perTestTier, sessionMocks, connectionTier)
		// An empty pool is no error: it is loaded (non-nil, so never read
		// again for this command), the scan below finds no candidate, and the
		// command is answered as it is against a pool holding only mocks it
		// cannot match: a control statement, a SEND_LONG_DATA, a STMT_RESET or
		// an unrecorded PREPARE by its synthesized answer, anything else by
		// the no_mocks miss. It was an error that did not wrap
		// models.ErrNoMockMatched: the connection was dropped, the proxy
		// dropped the error too, and the test that sent the command passed
		// with no miss. The pool is empty, for one, when a connection
		// outlives the mocks its handshake was served from: the pool was
		// replaced by one holding no mock at all.
		if len(pool) == 0 {
			logger.Debug("the mysql mock pool is empty")
		}
		return nil
	}
	// readTiers reads the per-test and connection tiers, once per command.
	readTiers := func() error {
		if tiersRead {
			return nil
		}
		var err error
		perTestTier, _, connectionTier, err = fetchTiers(func() ([]*models.Mock, error) { return nil, nil })
		if err != nil {
			return err
		}
		tiersRead = true
		return nil
	}
	inWindowOnly := func(mocks []*models.Mock) []*models.Mock {
		out := make([]*models.Mock, 0, 8)
		for _, mk := range mocks {
			if mockInCurrentWindow(mk) {
				out = append(out, mk)
			}
		}
		return out
	}

	// liveStatement is the incoming statement stripped of its inert leading
	// comment — the same identity matchQueryLive compares on. It is the
	// yardstick for nearestMock below, and a live PREPARE's key in
	// preparedIndex.
	// liveReadsAttributes is whether a live COM_QUERY's statement reads its
	// query attributes (readsQueryAttributes): then a recording of its very
	// text answers under exact serving only when they are equal, or noise
	// (sameStatementOnly).
	liveStatement, liveReadsAttributes := "", false
	switch m := req.Message.(type) {
	case *mysql.QueryPacket:
		liveStatement = sqlStatementIdentity(m.Query)
		liveReadsAttributes = readsQueryAttributes(m.Query)
	case *mysql.StmtPreparePacket:
		liveStatement = sqlStatementIdentity(m.Query)
	case *mysql.StmtExecutePacket:
		// Its prepared statement, for nearestMock only.
		if decodeCtx != nil {
			liveStatement = sqlStatementIdentity(decodeCtx.StmtIDToQuery[m.StatementID])
		}
	}

	// First pass. A scan can stop at a candidate it may serve without looking
	// further, and nothing it gathered before that candidate is used
	// afterwards. When a command can only stop on a candidate of a known part
	// of the pool, a scan of that part alone, in pool order, stops on the same
	// candidate, and the rest of the pool need not be read:
	//
	//   - COM_QUERY and COM_STMT_EXECUTE, while a test window is active, stop
	//     at the first exact match recorded INSIDE the window (queryMatched /
	//     stmtMatched). Their candidates come from the store's window index
	//     (integrations.SessionWindowReader). A COM_STMT_EXECUTE whose window
	//     holds no definitive match is settled by the window too when it holds
	//     a query-exact one: that one is the FIFO fallback ranked above every
	//     other, and the first in pool order is the window's first.
	//   - COM_STMT_PREPARE stops at the first exact match the strict gate
	//     allows, and a recorded PREPARE is exact only when its statement
	//     identity equals the live one; preparedIndex files the session tier's
	//     PREPAREs under their identity.
	//   - COM_STMT_CLOSE serves the best score, the first on a tie, and only
	//     a recorded CLOSE scores at all; preparedIndex files them. A score as
	//     high as a CLOSE can reach ends the scan.
	//
	// Both indexes come from the store (integrations.SessionKeyReader for the
	// second), so a command costs the same at the end of a set as at its
	// start. Lax mode keeps every test's MySQL data mocks in the session pool,
	// and a walk of it for each command made each test cost more than the one
	// before: the total grew with the square of the test count.
	//
	// When the first pass does not settle the command, the verdict is one of
	// the fallbacks, which rank the WHOLE pool in pool order; the scan then
	// runs over the whole pool from clean state, as it always did, reusing
	// what the first pass already computed for its candidates.
	//
	// While no test window is active, COM_QUERY and COM_STMT_EXECUTE are
	// matched through the store's index too, each through its own
	// (queryIndex, executeIndex). That is under exact serving (keploy
	// mock replay), whose store reports no window, and in keploy test only
	// before a set's first test: a test's window is opened as the test is
	// staged and stays open until the next test is staged. Then:
	//
	//   - a COM_QUERY stops at the first exact match the strict gate allows,
	//     and a recorded COM_QUERY is exact only when its statement identity
	//     equals the live one (matchQueryLive). A live COM_QUERY read as an
	//     older keploy read it (readsLegacy) is compared with some recordings
	//     on another identity, and is left to the full scan.
	//   - a COM_STMT_EXECUTE with no window serves, of the candidates whose
	//     query matches its own ignoring case, the first FIFO one (query-exact,
	//     parameters drifted inside noise), else the first definitive one
	//     (query and parameters exact); only a match on the parameters alone,
	//     or a score, can come from a candidate of another query, and they
	//     rank below both. So a pass over the candidates filed under the live
	//     query settles it when it finds either. A recorded EXECUTE's query is
	//     its own (mysql.PacketBundle.StoredQuery) or its PREPARE's in the
	//     pool, which no key of its own can say: while the staged mocks of
	//     the session tier hold an EXECUTE that recorded no query
	//     (keyExecuteUnnamed, looked up through unnamedExecIndex, which
	//     computes no statement identity), the full scan matches it.
	//
	// Under exact serving the per-test tier leads with every mock the open
	// test owns, and the keyed pass walks that tier before the session tier
	// (whole, or, through a store that hands it over one mock at a time,
	// until the command settles): every index here files the test's own mocks under no key (their
	// OwnedKeys), so the keyed walks of the session tier read the staged mocks
	// only, and no command computes a statement identity of the mocks the test
	// owns. An EXECUTE the test owns that recorded no query is read in that
	// tier too, ahead of every staged mock, as the full scan reads it. The
	// staged trees build each index on the first keyed walk that reaches the
	// session tier through it, so that walk computes the keys of its own kind
	// of command alone, once per staging.
	keyReader, _ := mockDb.(integrations.SessionKeyReader)
	if opts.fullScan {
		keyReader = nil
	}
	windowPass := !opts.fullScan && windowActive &&
		(req.Header.Type == sCOM_QUERY || req.Header.Type == sCOM_STMT_EXEC)
	// liveExec is a live COM_STMT_EXECUTE's statement, as every candidate is
	// compared with it.
	var liveExec liveExecStatement
	if act, _ := req.Message.(*mysql.StmtExecutePacket); act != nil && decodeCtx != nil && decodeCtx.StmtIDToQuery != nil {
		liveExec = newLiveExecStatement(strings.TrimSpace(decodeCtx.StmtIDToQuery[act.StatementID]))
	}
	// scanKey is the key the keyed pass reads the session tier under, and
	// scanIndex the index that files it.
	scanKey, scanIndex := "", preparedIndex
	switch req.Header.Type {
	case sCOM_STMT_PREP:
		scanKey = keyPrepare + liveStatement
	case sCOM_STMT_CLOSE:
		scanKey = keyClose
	case sCOM_QUERY:
		if qp, _ := req.Message.(*mysql.QueryPacket); !windowActive && qp != nil && !readsLegacy(qp) {
			scanKey, scanIndex = keyQuery+liveStatement, queryIndex
		}
	case sCOM_STMT_EXEC:
		if !windowActive && liveExec.identity != "" && keyReader != nil {
			unnamed := false
			if err := keyReader.RangeSessionMocksWithKey(unnamedExecIndex, keyExecuteUnnamed, func(*models.Mock) bool {
				unnamed = true
				return false
			}); err == nil && !unnamed {
				scanKey, scanIndex = keyExecute+foldKey(liveExec.identity), executeIndex
			}
		}
	}
	keyedPass := keyReader != nil && scanKey != ""
	// keyedExec is the keyed pass of a COM_STMT_EXECUTE (no window is active).
	keyedExec := keyedPass && req.Header.Type == sCOM_STMT_EXEC
	firstPass := windowPass || keyedPass
	// walkErr is what the keyed walk of the session tier failed with, if it
	// failed.
	var walkErr error
	var candidates iter.Seq[*models.Mock]
	windowReader, hasWindowReader := mockDb.(integrations.SessionWindowReader)
	switch {
	case keyedPass:
		// A store that hands the per-test tier over one mock at a time
		// (integrations.PerTestRanger) is read that way, and the walk stops
		// where the command settles: under exact serving, at the open test's
		// next recording, so a test that owns thousands of mocks costs each
		// of its commands what a test that owns a few does. Another store's
		// per-test tier is read whole, once per command.
		ranger, ranges := mockDb.(integrations.PerTestRanger)
		if ranges {
			var err error
			if connectionTier, err = fetchConnectionTier(); err != nil {
				return nil, false, nil, err
			}
		} else if err := readTiers(); err != nil {
			return nil, false, nil, err
		}
		key, index := scanKey, scanIndex
		// The per-test and connection tiers are walked whole, unless the
		// command settles first. Under exact serving the per-test tier leads
		// with the open test's unanswered mocks, which the keyed walk of the
		// session tier leaves out.
		candidates = func(yield func(*models.Mock) bool) {
			stopped := false
			if ranges {
				if err := ranger.RangePerTestMocksInWindow(func(mk *models.Mock) bool {
					stopped = !yield(mk)
					return !stopped
				}); err != nil {
					walkErr = err
					return
				}
			} else {
				for _, mk := range perTestTier {
					if stopped = !yield(mk); stopped {
						break
					}
				}
			}
			if stopped {
				return
			}
			if err := keyReader.RangeSessionMocksWithKey(index, key, func(mk *models.Mock) bool {
				stopped = !yield(mk)
				return !stopped
			}); err != nil {
				walkErr = err
				return
			}
			if stopped {
				return
			}
			for _, mk := range connectionTier {
				if !yield(mk) {
					return
				}
			}
		}
	case windowPass && hasWindowReader:
		perTestMocks, sessionMocks, connectionMocks, err := fetchTiers(func() ([]*models.Mock, error) {
			return windowReader.GetSessionMocksInWindow(winStart, winEnd)
		})
		if err != nil {
			return nil, false, nil, err
		}
		perTestTier, connectionTier, tiersRead = perTestMocks, connectionMocks, true
		candidates = slices.Values(merge(inWindowOnly(perTestMocks), sessionMocks, inWindowOnly(connectionMocks)))
	default:
		if err := loadPool(); err != nil {
			return nil, false, nil, err
		}
		if windowPass {
			candidates = slices.Values(inWindowOnly(pool))
		} else {
			candidates = slices.Values(pool)
		}
	}

	debugOn := logger.Core().Enabled(zapcore.DebugLevel)

	// The names of every COM_STMT_EXECUTE mock, for the debug log only. It is a
	// pass over the whole pool, so it runs only when the line will be written,
	// and the line is written once per command, not once per candidate.
	if req.Header.Type == sCOM_STMT_EXEC && debugOn && pool == nil {
		if err := loadPool(); err != nil {
			return nil, false, nil, err
		}
	}
	if req.Header.Type == sCOM_STMT_EXEC && debugOn {
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
			if skippedAtCommandPhase(mock) {
				continue
			}
			for _, mockReq := range mock.Spec.MySQLRequests {
				if mockReq.PacketBundle.Header.Type == sCOM_STMT_EXEC {
					stmtMocks = append(stmtMocks, mock.Name)
				}
			}
		}
		logger.Debug("List of com-stmt-execute mocks to match", zap.Strings("mocks", stmtMocks))
	}

	// recordedQuery resolves a recorded statement (the connID its mock
	// recorded, a statement ID) to the query its PREPARE recorded: the first
	// such entry in pool order, what lookupRecordedQuery finds in
	// buildRecordedPrepIndex(pool). The full scan, and a store without the
	// key index, build that index from the pool. Otherwise the entry is found
	// through the tiers, the session tier's through preparedIndex, so no
	// command indexes the whole pool: the per-test and connection tiers are
	// read once, and each statement is looked up once. Under exact serving
	// the open test's own entries are the per-test tier's, which is read
	// first: preparedIndex files the test's mocks under no entry key.
	var (
		prepByConn         map[string][]prepEntry
		tierPrep, connPrep map[prepKey]string
		prepMemo           map[prepKey]string
	)
	recordedQuery := func(connID string, stmtID uint32) (string, error) {
		if keyReader == nil {
			if prepByConn == nil {
				if pool == nil {
					if err := loadPool(); err != nil {
						return "", err
					}
				}
				prepByConn = buildRecordedPrepIndex(pool)
			}
			return lookupRecordedQuery(prepByConn, connID, stmtID), nil
		}
		k := prepKey{connID, stmtID}
		if q, ok := prepMemo[k]; ok {
			return q, nil
		}
		if prepMemo == nil {
			if err := readTiers(); err != nil {
				return "", err
			}
			tierPrep, connPrep = firstPrepEntries(perTestTier), firstPrepEntries(connectionTier)
			prepMemo = map[prepKey]string{}
		}
		q, found := tierPrep[k]
		if !found {
			if err := keyReader.RangeSessionMocksWithKey(preparedIndex, prepEntryKey(connID, stmtID), func(mk *models.Mock) bool {
				if c, e, ok := recordedPrepEntry(mk); ok && c == connID && e.statementID == stmtID {
					q, found = e.query, true
					return false
				}
				return true
			}); err != nil {
				return "", err
			}
			if !found {
				q = connPrep[k]
			}
		}
		prepMemo[k] = q
		return q, nil
	}
	// statementQuery is the query a recorded command that names a prepared
	// statement ran: the one the recorder kept on the command
	// (mysql.MetaQuery), or else the one its PREPARE recorded
	// (recordedQuery). "" when neither is in the set: the statement is
	// unknown (see unknownStatements).
	statementQuery := func(recorded *mysql.PacketBundle, connID string, stmtID uint32) (string, error) {
		if q := recorded.StoredQuery(); q != "" {
			return q, nil
		}
		return recordedQuery(connID, stmtID)
	}
	// lostStatements are those of unknown (recorded EXECUTEs whose statement
	// the pool cannot name) whose PREPARE the test set held and lost (keploy
	// #4810: an older keploy's mock prune deleted it). The recorder cannot
	// decode an EXECUTE of a statement whose COM_STMT_PREPARE_OK it did not
	// see on that connection, so every recorded EXECUTE had a PREPARE; the
	// question is whether this set held it. It did when the set holds:
	//   - the connection's handshake (its "config" mock, which no prune
	//     deletes): the connection was recorded from its start in this set,
	//     so every statement it ran was prepared in this set. This is the
	//     pooled connection that kept none of its PREPAREs, because another
	//     connection prepared each of its statements first and the old prune
	//     kept only that first one;
	//   - or a PREPARE of the connection with a lower statement ID: a server
	//     numbers a connection's statements in the order it prepares them, so
	//     this one was prepared after a PREPARE the set kept.
	// A statement prepared in an earlier test set, on a connection a long
	// recording kept open across sets, is neither, and is not counted.
	// Evidence holds only when recorded before the unknown EXECUTE: a
	// connection's handshake and its earlier PREPAREs come before its
	// EXECUTEs. A set that gathers several recordings (the k8s-proxy's smart
	// set) holds connections of different agent runs under the same connID,
	// numbered from 0 by each run, and a later run's handshake or PREPARE
	// says nothing of an earlier run's EXECUTE. A zero time says nothing and
	// is taken.
	type preparedAt struct {
		id uint32
		at time.Time
	}
	var (
		evidenceRead bool
		handshakeAt  map[string]time.Time
		preparedOn   map[string][]preparedAt
	)
	precedes := func(evidence, exec time.Time) bool {
		return evidence.IsZero() || exec.IsZero() || !evidence.After(exec)
	}
	lostStatements := func(unknown []*models.Mock) ([]*models.Mock, error) {
		if !evidenceRead {
			if pool == nil {
				if err := loadPool(); err != nil {
					return nil, err
				}
			}
			evidenceRead = true
			handshakeAt = map[string]time.Time{}
			preparedOn = map[string][]preparedAt{}
			for _, m := range pool {
				if c, e, ok := recordedPrepEntry(m); ok {
					preparedOn[c] = append(preparedOn[c], preparedAt{e.statementID, m.Spec.ReqTimestampMock})
				}
				if c := m.Spec.Metadata["connID"]; m.Kind == models.MySQL && m.Spec.Metadata["type"] == "config" && c != "" {
					if at, seen := handshakeAt[c]; !seen || m.Spec.ReqTimestampMock.Before(at) {
						handshakeAt[c] = m.Spec.ReqTimestampMock
					}
				}
			}
		}
		var out []*models.Mock
		for _, m := range unknown {
			c, x := m.Spec.Metadata["connID"], m.Spec.ReqTimestampMock
			if at, ok := handshakeAt[c]; ok && precedes(at, x) {
				out = append(out, m)
				continue
			}
		scan:
			for _, r := range m.Spec.MySQLRequests {
				exec, _ := r.Message.(*mysql.StmtExecutePacket)
				if exec == nil {
					continue
				}
				for _, p := range preparedOn[c] {
					if p.id < exec.StatementID && precedes(p.at, x) {
						out = append(out, m)
						break scan
					}
				}
			}
		}
		return out, nil
	}

	// The whole prepared-statement index, for the debug log only.
	if (req.Header.Type == sCOM_STMT_PREP || req.Header.Type == sCOM_STMT_EXEC) && debugOn {
		if pool == nil {
			if err := loadPool(); err != nil {
				return nil, false, nil, err
			}
		}
		var allEntries []string
		for connID, prepEntries := range buildRecordedPrepIndex(pool) {
			for _, entry := range prepEntries {
				allEntries = append(allEntries, fmt.Sprintf("connID=%s stmtID=%d query=%q mock=%s", connID, entry.statementID, entry.query, entry.mockName))
			}
		}
		logger.Debug("recorded prepEntries", zap.String("entries", strings.Join(allEntries, " | ")))
	}

	// closeBest is the highest score a COM_STMT_CLOSE candidate can reach (see
	// matchCloseWithQuery): the header, and the statement when the live one is
	// known.
	closeBest := 0
	if req.Header.Type == sCOM_STMT_CLOSE {
		closeBest = 2
		if actClose, _ := req.PacketBundle.Message.(*mysql.StmtClosePacket); actClose != nil && decodeCtx != nil && decodeCtx.StmtIDToQuery != nil &&
			sqlStatementIdentity(strings.TrimSpace(decodeCtx.StmtIDToQuery[actClose.StatementID])) != "" {
			closeBest = 12
		}
	}

	// liveStatementParams is how many of a live COM_STMT_EXECUTE's
	// parameters are its statement's own (its PREPARE_OK's num_params), the
	// rest being query attributes (executeAttributesFrom); -1 when not known.
	// Its strict body holds its own and, when its statement (liveStmtQuery)
	// reads its attributes, the ones it reads, by name
	// (executeBodyParameters).
	liveStatementParams, liveStmtQuery := -1, ""
	if act, _ := req.Message.(*mysql.StmtExecutePacket); act != nil && decodeCtx != nil {
		if prep := decodeCtx.PreparedStatements[act.StatementID]; prep != nil {
			liveStatementParams = int(prep.NumParams)
		}
		liveStmtQuery = liveExec.query
	}

	// Strict-enforcement gate (see strictGate in schema_noise.go): consulted
	// before a mock may become a match candidate, and accumulates the
	// rejection diagnostics (count / closest mock / field-level diffs) the
	// mismatch report renders on a miss. It also holds the canonical JSON
	// body of the live request the schema-noise engine compares, built only
	// when strict enforcement, the rule of exact serving or detection first
	// reads it.
	gate := newStrictGate(noiseEngine, logger, req.Header.Type, &req.PacketBundle, liveStatementParams, liveStmtQuery, userBodyNoise)
	gate.bodiesBuilt, gate.compared = opts.bodiesBuilt, opts.compared
	if opts.eagerBody {
		gate.liveRequestBody()
	}
	// fallbacks are a COM_STMT_EXECUTE's FIFO and score candidates, gated
	// after the walk, in the selection's rank (execFallbacks).
	fallbacks := execFallbacks{gate: gate, sameStatementOnly: sameStatementOnly, testsOwn: testsOwn, eager: opts.eagerGate}

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

		// Reporting-only closeness, tracked SEPARATELY from the score above.
		// The score is a correctness signal — a candidate that is not the same
		// statement must score 0 and never be served — but the mismatch report
		// still has to name the nearest recorded query. Without this split, a
		// pool where every candidate scores 0 reports closest_mock="", which
		// query.go renders as "REPLAY-ORPHAN: mock NEVER RECORDED for this
		// query". That message sends the reader looking for a recording bug
		// when the recording is present and merely drifted.
		nearestMock   *models.Mock
		nearestQuery  string
		nearestPrefix int

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
		// exists for the query, and no recording with the command's
		// parameters, defExecMock, either) separately so a stale
		// earlier-test read-back mock living in the startup/session tier
		// cannot win over the current test's own in-window row. The walk
		// lists them in fallbacks, which picks them after it
		// (fallbacks.windowPick, fallbacks.anyPick).

		// defExecResp/defExecMock hold a definitive (query+params exact)
		// COM_STMT_EXECUTE match that is NOT an in-window per-test mock
		// (i.e. it lives in the startup/session/connection tier). Used only
		// when no in-window definitive match exists, so a genuinely unique
		// reusable read still resolves while an in-window row always wins.
		// It ranks above the any-tier FIFO candidate (fallbacks.anyPick).
		// defExecMock is one recorded outside every recorded test's window
		// (at start-up or between two tests): the first of the best
		// outsideGapRank, so the one of the command's own gap. Under exact
		// serving, or with no recorded windows, it is the first one.
		// defExecMockTest is the first one recorded inside another test's
		// window (recordedInATestsWindow), which ranks below it: it is the
		// answer of that test's own call. The first, in recorded order,
		// is an earlier test's left over by its replay, where a later
		// test's is the one its own call, still to come, will want.
		defExecResp     *mysql.Response
		defExecMock     *models.Mock
		defExecRank     int
		defExecRespTest *mysql.Response
		defExecMockTest *models.Mock

		// paramsOnly*: a definitive match on bound parameters alone, of a
		// recorded EXECUTE whose statement the set cannot name (its PREPARE
		// is not in the pool and it records no query; see
		// matchStmtExecutePacketQueryAware). Its statement is unknown, so it
		// ranks below a match on the query and every parameter: the in-window
		// one below an in-window exact match (which the scan goes on looking
		// for), the out-of-window one below defExecMock.
		paramsOnlyRespWindow *mysql.Response
		paramsOnlyMockWindow *models.Mock
		paramsOnlyResp       *mysql.Response
		paramsOnlyMock       *models.Mock

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

		// unknownStatements are the COM_STMT_EXECUTE mocks recorded inside
		// the current test window whose statement the pool cannot name: the
		// recorder kept no query on them, and no PREPARE of theirs is in the
		// pool. Only those whose parameters have the live command's shape
		// (sameParameterShape): the others ran another statement. Which of
		// them lost a PREPARE the set once held is settled after the scan
		// (lostStatements).
		unknownStatements []*models.Mock
	)

	// trackNearest remembers the recorded statement closest to the live one.
	// Reporting only: it never makes a mock servable, it only gives the mismatch
	// report something truthful to name.
	//
	// Ranked by longest shared prefix of the comment-stripped statements. Weak
	// on its own (every SELECT shares "SELECT ") but it is the best available
	// ordering, and it only ever decides which query the report NAMES.
	// recordedIdentity is sqlStatementIdentity(recorded) when the comparison
	// already derived it, empty when it did not.
	trackNearest := func(mock *models.Mock, recorded, recordedIdentity string) {
		if liveStatement == "" || recorded == "" {
			return
		}
		if recordedIdentity == "" {
			recordedIdentity = sqlStatementIdentity(recorded)
		}
		if n := commonPrefixLen(liveStatement, recordedIdentity); n > nearestPrefix {
			nearestPrefix, nearestMock, nearestQuery = n, mock, recorded
		}
	}

	// mysqlCandidates counts the MySQL mocks the command phase had available to
	// compare — those that survived the lifetime filter below. It is a coarse
	// measure: a mock whose recorded command type does not match the live
	// request still counts. Incremented AFTER the filter, not before, because
	// counting pre-filter would pad the number shown to the user with
	// handshake/config mocks that were never candidates and would make the
	// "nothing to compare" phase unreachable (one handshake mock would keep the
	// count non-zero).
	mysqlCandidates := 0

	// The live statement's facts, derived on the first comparison and reused for
	// every other candidate (see liveQuery). The live identity is liveStatement.
	live := liveQuery{identity: liveStatement}
	// liveLegacy is the same for the live statement as a keploy that did not
	// read query attributes' names read it (mysql.QueryPacket's LegacyQuery),
	// for the COM_QUERYs such a keploy recorded (legacyRecordedQuery).
	var liveLegacy liveQuery

	// evaluated keeps what each candidate's comparison returned during the
	// in-window pass, so the full scan that follows a fruitless one reuses it
	// instead of comparing (and logging) the same candidate twice. Only a scan
	// that can run twice keeps it.
	type evalKey struct {
		mock *models.Mock
		req  int
	}
	type evalResult struct {
		ok    bool
		score int
		// recorded is the recorded statement: its identity for COM_QUERY and
		// COM_STMT_PREPARE, the query its PREPARE recorded for COM_STMT_EXECUTE.
		recorded   string
		queryExact bool // COM_STMT_EXECUTE's query-exact signal
	}
	var evaluated map[evalKey]evalResult
	if firstPass {
		evaluated = make(map[evalKey]evalResult)
	}
	compareQuery := func(mock *models.Mock, reqIdx int, recorded mysql.PacketBundle, getQuery func(mysql.PacketBundle) string) (bool, int, string) {
		key := evalKey{mock, reqIdx}
		if ev, seen := evaluated[key]; seen {
			return ev.ok, ev.score, ev.recorded
		}
		lq := &live
		// A COM_QUERY an older keploy recorded with its query attributes
		// read out of step is compared with the live one read the same way.
		if exp, _ := recorded.Message.(*mysql.QueryPacket); legacyRecordedQuery(exp) {
			if act, _ := req.Message.(*mysql.QueryPacket); readsLegacy(act) {
				if !liveLegacy.ready {
					liveLegacy.fill(act.LegacyQuery)
				}
				lq = &liveLegacy
			}
		}
		ok, c, id := matchQueryLive(logger, recorded, req.PacketBundle, lq, getQuery)
		if sameStatementOnly && !ok && c > 0 && maskSQLLiterals(id) != lq.maskedLiterals() {
			c = 0
		}
		if evaluated != nil {
			evaluated[key] = evalResult{ok: ok, score: c, recorded: id}
		}
		return ok, c, id
	}

	// Iterates the candidates in pool order (unfiltered + connection-scoped)
	// so prepared-statement executes find their setups even when the setup was
	// recorded in a different test's window. See firstPass above for the two
	// passes.
	closeSettled := false
	for {
		for mock := range candidates {
			if opts.examined != nil {
				*opts.examined++
			}
			if _, no := opts.refused[mock]; no {
				continue
			}
			if mock.Kind != models.MySQL {
				continue
			}
			// Session-tier handshake/auth mocks were matched at the
			// command prologue; skip them at command phase. Connection-
			// scoped (prepared-statement setup) mocks ARE retained —
			// they're how COM_STMT_EXEC finds its matching prepare.
			if skippedAtCommandPhase(mock) {
				continue // command-phase only wants data + connection mocks + session-reusable commands
			}
			mysqlCandidates++
			for reqIdx, mockReq := range mock.Spec.MySQLRequests {
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
						q, err := statementQuery(&mockReq.PacketBundle, mock.Spec.Metadata["connID"], expClose.StatementID)
						if err != nil {
							return nil, false, nil, err
						}
						expectedQuery = q
					}
					if actClose, _ := req.PacketBundle.Message.(*mysql.StmtClosePacket); actClose != nil && decodeCtx != nil && decodeCtx.StmtIDToQuery != nil {
						actualQuery = strings.TrimSpace(decodeCtx.StmtIDToQuery[actClose.StatementID])
					}
					c := matchCloseWithQuery(mockReq.PacketBundle, req.PacketBundle, expectedQuery, actualQuery)
					if c > maxMatchedCount {
						maxMatchedCount, matchedResp, matchedMock = c, &mysql.Response{}, mock
					}
					// Nothing later can beat the best score a CLOSE reaches.
					// Only the first pass stops on it: the full scan also
					// counts the candidates for a miss report.
					closeSettled = firstPass && maxMatchedCount >= closeBest

				case sCOM_QUERY:
					if ok, c, recordedIdentity := compareQuery(mock, reqIdx, mockReq.PacketBundle, queryPacketText); ok {
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
						// text and may still drift. Under exact serving, with
						// or without strict, a statement that reads them is
						// answered only by a recording made with the same
						// values of the ones it reads, by name (the others
						// are not in its body: readAttributes), or whose
						// differing ones are noise
						// (sameStatementOnly, allowsLiterals; the literals of
						// the same text are equal, its comments left out): it
						// read tenant a's rows for tenant b. One that does not
						// read them answers whatever they are (its rows do not
						// depend on them), so it is not compared: a statement
						// vitess cannot parse is compared whole, and a changed
						// leading comment would refuse it.
						if !gate.allows(mock) || (sameStatementOnly && liveReadsAttributes && !gate.allowsLiterals(mock)) {
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
					} else {
						// Reporting only — see nearestMock. A score-0 candidate is
						// not servable, but it may still be the nearest recording.
						if qp, qok := mockReq.PacketBundle.Message.(*mysql.QueryPacket); qok {
							trackNearest(mock, qp.Query, recordedIdentity)
						}
						if c > maxMatchedCount {
							// Track the closest candidate for the mismatch report even
							// when strict rejects it as a servable match below.
							bestPartialMock = mock
							if qp, qok := mockReq.PacketBundle.Message.(*mysql.QueryPacket); qok {
								bestPartialQuery = qp.Query
							}
							// Under exact serving a recording of the statement
							// made with other inline literals answers only when
							// every literal that differs is noise
							// (sameStatementOnly, allowsLiterals).
							if sameStatementOnly && !gate.allowsLiterals(mock) {
								continue
							}
							// Structure-matched-but-text-drifted candidate: under
							// strict it may only be served when the drift (body.query
							// / attribute values) is covered by learned/user noise.
							if !gate.allows(mock) {
								continue
							}
							maxMatchedCount, matchedResp, matchedMock = c, &mock.Spec.MySQLResponses[0], mock
						}
					}

				case sCOM_STMT_PREP:
					if ok, c, recordedIdentity := compareQuery(mock, reqIdx, mockReq.PacketBundle, preparePacketText); ok {
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
					} else {
						// Reporting only — see nearestMock.
						if sp, spOk := mockReq.PacketBundle.Message.(*mysql.StmtPreparePacket); spOk {
							trackNearest(mock, sp.Query, recordedIdentity)
						}
						if c <= maxMatchedCount {
							continue
						}
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

					key := evalKey{mock, reqIdx}
					ev, seen := evaluated[key]
					if !seen {
						// remove this log and if block
						if ce := logger.Check(zap.DebugLevel, "Trying to match the mock with com-stmt-execute request"); ce != nil && actMsg != nil {
							ce.Write(zap.String("mock_name", mock.Name), zap.Any("Req", executeForLog(actMsg)))
						}

						var expectedQuery, actualQuery string
						if expMsg != nil {
							q, err := statementQuery(&mockReq.PacketBundle, mock.Spec.Metadata["connID"], expMsg.StatementID)
							if err != nil {
								return nil, false, nil, err
							}
							expectedQuery = q
						}
						actualQuery = liveExec.query

						if debugOn {
							logger.Debug("queries comparison", zap.String("expected_query", expectedQuery), zap.String("actual_query", actualQuery), zap.Uint32("mock_statement_id", expMsg.StatementID), zap.Uint32("actual_statment_id", actMsg.StatementID), zap.Any("connID", mock.Spec.Metadata["connID"]), zap.String("mock_name", mock.Name))
						}

						ev.ok, ev.score, ev.queryExact = matchStmtExecuteLive(logger, mockReq.PacketBundle, req.PacketBundle, expectedQuery, liveExec, liveStatementParams, mock.Name, util.NewNoiseChecker(mock.Noise))
						if sameStatementOnly && !ev.ok {
							ev.score = 0
						}
						ev.recorded = expectedQuery
						if evaluated != nil {
							evaluated[key] = ev
						}
					}
					expectedQuery := ev.recorded
					gate.recordedQuery(mock, expectedQuery)
					if expectedQuery == "" && windowActive && mockInCurrentWindow(mock) &&
						sameParameterShape(expMsg, actMsg, liveStatementParams) && !slices.Contains(unknownStatements, mock) {
						unknownStatements = append(unknownStatements, mock)
					}

					if ok, c, queryExact := ev.ok, ev.score, ev.queryExact; ok {
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
						//
						// A match on the parameters alone (the recorded
						// statement unknown) is weaker: it does not stop the
						// scan, see paramsOnlyMockWindow.
						fallbacks.addDefinitive(mock)
						if !gate.allows(mock) {
							continue
						}
						inWindow := testsOwn(mock)
						switch {
						case expectedQuery == "" && sameStatementOnly && liveStmtQuery != "":
							// Under exact serving a recording whose
							// statement is lost does not answer a command
							// whose statement is known: equal parameters
							// do not make it the same statement
							// (sameStatementOnly). It is the nearest for
							// the miss report.
							if bestPartialMock == nil {
								bestPartialMock, bestPartialQuery = mock, ""
							}
						case expectedQuery == "" && inWindow:
							if paramsOnlyMockWindow == nil {
								paramsOnlyRespWindow, paramsOnlyMockWindow = &mock.Spec.MySQLResponses[0], mock
							}
						case expectedQuery == "":
							if paramsOnlyMock == nil {
								paramsOnlyResp, paramsOnlyMock = &mock.Spec.MySQLResponses[0], mock
							}
						case inWindow:
							matchedResp, matchedMock, stmtMatched = &mock.Spec.MySQLResponses[0], mock, true
						case recordedInATestsWindow(mock):
							if defExecMockTest == nil {
								defExecRespTest, defExecMockTest = &mock.Spec.MySQLResponses[0], mock
							}
						default:
							if rank := outsideGapRank(mock); defExecMock == nil || rank < defExecRank {
								defExecResp, defExecMock, defExecRank = &mock.Spec.MySQLResponses[0], mock, rank
							}
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
						// Reporting only: a recording of another statement
						// scores nothing, but may be the nearest recorded.
						if !queryExact {
							trackNearest(mock, expectedQuery, "")
						}
						// A query-exact candidate is a FIFO candidate, and
						// one that scored the score candidate's: a recording
						// of the statement's shape, or one whose statement
						// is unknown, without exact serving
						// (sameStatementOnly). The strict gate decides on
						// them after the walk, when the selection reads its
						// pick (execFallbacks).
						if queryExact || c > 0 {
							fallbacks.add(mock, c, queryExact)
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
			if queryMatched || stmtMatched || closeSettled {
				break
			}
		}
		if walkErr != nil {
			return nil, false, nil, walkErr
		}
		settled := queryMatched || stmtMatched ||
			// See firstPass: the window's FIFO candidate, and its match on the
			// parameters alone, outrank every other fallback.
			(windowPass && req.Header.Type == sCOM_STMT_EXEC && (paramsOnlyMockWindow != nil || fallbacks.windowPick() != nil)) ||
			// With no window, the keyed pass has compared every candidate of
			// the live query and every mock the open test owns (exact
			// serving: the per-test tier is the open test's unanswered mocks,
			// walked whole). A fallback it found outranks every candidate
			// left unread, which can only be another statement's match on
			// its parameters or score, or one unowned: the open test's match
			// on the parameters alone and its FIFO candidate rank above
			// every unowned one, and the FIFO and definitive candidates of
			// the live query above the rest.
			(keyedExec && (defExecMock != nil || paramsOnlyMockWindow != nil ||
				fallbacks.windowPick() != nil || fallbacks.anyPick() != nil)) ||
			// The CLOSE pass scored every recorded CLOSE.
			(req.Header.Type == sCOM_STMT_CLOSE && matchedMock != nil)
		if !firstPass || settled {
			break
		}
		// The first pass did not settle the command: run the scan over the
		// whole pool from clean state. The comparisons already made are in
		// evaluated.
		firstPass, windowPass = false, false
		if pool == nil {
			if err := loadPool(); err != nil {
				return nil, false, nil, err
			}
		}
		candidates = slices.Values(pool)
		maxMatchedCount, matchedResp, matchedMock = 0, nil, nil
		queryMatched, stmtMatched = false, false
		bestPartialMock, bestPartialQuery = nil, ""
		nearestMock, nearestQuery, nearestPrefix = nil, "", 0
		fallbacks.reset()
		defExecResp, defExecMock, defExecRank = nil, nil, 0
		defExecRespTest, defExecMockTest = nil, nil
		paramsOnlyRespWindow, paramsOnlyMockWindow = nil, nil
		paramsOnlyResp, paramsOnlyMock = nil, nil
		queryExactResp, queryExactMock = nil, nil
		unknownStatements = nil
		mysqlCandidates = 0
		gate.resetDiagnostics()
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
		//   1. in-window match on the parameters alone, of a recorded
		//      EXECUTE whose statement is unknown (paramsOnlyMockWindow)
		//   2. in-window per-test FIFO candidate (query-exact, params not
		//      matched — the read-back-of-generated-id case)
		//   3. out-of-window definitive match (query+params exact): without
		//      exact serving one recorded outside every test's window, of
		//      the command's gap first, then one recorded in another test's
		//      window (defExecMock, then defExecMockTest); under exact
		//      serving the first in the view's order
		//   4. out-of-window FIFO candidate (query-exact, params not
		//      matched)
		//   5. out-of-window match on the parameters alone
		// "In-window" is the open test's own (testsOwn): a recording made
		// in the current test window, or under exact serving one of the
		// open test's mapping. The test's own recordings rank first, so its
		// own row wins over a stale exact match recorded elsewhere. Of the
		// rest, a recording with the command's query and parameters ranks
		// above the first one with its query and other parameters (3 above
		// 4), in every mode, whether or not a window is open: no candidate
		// there is the test's own, and an exact query+params match is
		// stronger evidence than one of other parameters. Ranked the other
		// way, a command made with no window open (at start-up, between
		// tests under exact serving, in keploy mock replay), or after a
		// test while its window was still open, was answered with the
		// rows of another call of its statement, perhaps a later test's,
		// which then lost its own. Without exact serving, of two
		// recordings with the command's query and parameters, one made
		// outside every test (at start-up, between tests: where such a
		// command's own was made) ranks above another test's, which that
		// test's own call will want, and of two made outside every test,
		// the one of the gap the command falls in (outsideGapRank). The
		// FIFO candidate answers a command with no recording of its
		// parameters, e.g. a read-back of a value the replay generated.
		//
		// Known limits without exact serving: the open test's FIFO
		// candidate (2) still outranks a recording with the command's
		// parameters made outside every test (3). Whenever the open window
		// holds an unanswered recording of the command's statement with
		// other parameters, a command made outside every test that is
		// replayed in that window takes it (another call's rows), whose own
		// call then takes another: one made between two tests that the
		// replay sends once the next test began, or one made after a test
		// whose window, still open, holds such a recording. And a command
		// replayed while the window of the test before it is still open
		// takes an earlier gap's recording of its request when the gap
		// before that window holds one, and a command replayed two or more
		// tests after its gap a later gap's (outsideGapRank).
		//
		// The FIFO candidates and the score candidate (6, when none of the
		// five is found) are gated here, in this rank, each when it is
		// read (execFallbacks): the walk only listed them.
		chosenResp, chosenMock := paramsOnlyRespWindow, paramsOnlyMockWindow
		inWindow := chosenMock != nil
		if chosenMock == nil {
			chosenMock = fallbacks.windowPick()
			chosenResp, inWindow = recordedResponse(chosenMock), chosenMock != nil
		}
		if chosenMock == nil && defExecMock != nil {
			chosenResp, chosenMock = defExecResp, defExecMock
		}
		if chosenMock == nil && defExecMockTest != nil {
			chosenResp, chosenMock = defExecRespTest, defExecMockTest
		}
		if chosenMock == nil {
			chosenMock = fallbacks.anyPick()
			chosenResp = recordedResponse(chosenMock)
		}
		if chosenMock == nil && paramsOnlyMock != nil {
			chosenResp, chosenMock = paramsOnlyResp, paramsOnlyMock
		}
		if chosenMock != nil {
			if debugOn {
				if scored := fallbacks.scorePick(); scored != chosenMock {
					name := "<none>"
					if scored != nil {
						name = scored.Name
					}
					logger.Debug("COM_STMT_EXECUTE FIFO fallback selected next unconsumed mock in recorded order",
						zap.String("mock_name", chosenMock.Name),
						zap.Bool("in_window", inWindow),
						zap.String("score_based_mock", name))
				}
			}
			matchedResp, matchedMock = chosenResp, chosenMock
		} else if matchedMock = fallbacks.scorePick(); matchedMock != nil {
			matchedResp = recordedResponse(matchedMock)
		} else {
			// A miss: its report reads the gate's diagnostics as the walk
			// gave them.
			fallbacks.diagnose()
		}
	}

	// A COM_STMT_EXECUTE that found no mock of its own test to serve, in a
	// test whose recording holds EXECUTEs that lost their statement (see
	// lostStatements). One of those may be this command's own: its query is
	// lost, and nothing left in the set says which statement it ran. Serving
	// another test's EXECUTE instead, picked by its query alone (FIFO), by
	// its parameters alone, or by score, answers the command with that
	// test's result (keploy #4810: another order's OK in place of this
	// order's duplicate-key error), which can pass the test when it should
	// fail. So the command is a miss that names the mocks it may have been,
	// and why. An exact match on the query and every parameter
	// (defExecMock, defExecMockTest) is still served: it is the answer the matcher gives a
	// genuinely reusable read, whatever else the window holds. (A command no
	// mock answers at all is the miss below, which names them too.)
	var lost []*models.Mock
	if req.Header.Type == sCOM_STMT_EXEC && !stmtMatched && len(unknownStatements) > 0 &&
		(matchedMock == nil || (!mockInCurrentWindow(matchedMock) && matchedMock != defExecMock && matchedMock != defExecMockTest)) {
		var err error
		if lost, err = lostStatements(unknownStatements); err != nil {
			return nil, false, nil, err
		}
	}
	if len(lost) > 0 && matchedMock != nil {
		return nil, false, unknownStatementsMiss(lost, matchedMock, mysqlCandidates), nil
	}

	if opts.probed != nil {
		if matchedResp != nil {
			*opts.probed = matchedMock
		}
		return nil, false, nil, nil
	}

	if matchedResp == nil {
		// System-variable read (SELECT @@[session.|global.]<var>) with no exact
		// mock. Connector/J issues these LIVE during connection setup
		// (useLocalSessionState=false) — e.g. getTransactionIsolation() →
		// "SELECT @@session.transaction_isolation" — and they are frequently
		// absent from the recording (read at replay, not at record). Their value
		// IS recorded, though: the connection-setup probe
		// "SELECT @@... AS <var>, ..." captured every session variable in one
		// result set. Serve <var>'s REAL recorded value as a correctly-framed
		// single-column result set, instead of cross-serving a different var's
		// mock (see rejectsCrossVariableRead). Same spirit as the graceful
		// control-statement OK just below — deterministic, recorded data, no
		// fabrication. Only runs when no exact mock matched, so recorded
		// system-var reads are unaffected.
		if req.Header.Type == sCOM_QUERY {
			if qp, ok := req.Message.(*mysql.QueryPacket); ok {
				// First: an unrecorded single system-variable read is resolved
				// from the connection-setup probe (see the block comment above
				// and rejectsCrossVariableRead). Ordered BEFORE the control-statement
				// OK so the read is answered with its REAL recorded value rather
				// than a bare OK.
				if varName, isVarRead := parseSingleSystemVarRead(qp.Query); isVarRead {
					if resp := buildSessionVarResponse(logger, pool, varName, decodeCtx); resp != nil {
						logger.Debug("served system-variable read from recorded session probe",
							zap.String("var", varName))
						return resp, true, nil, nil
					}
				}

				// Graceful generic OK for common control statements (no mocks)
				// Match on the executable statement: a prologued
				// "/*dde=...*/ SET NAMES utf8mb4" is a SET, and testing the raw
				// text would classify it as an unknown statement and fall
				// through to the mismatch path.
				q := sqlStatementIdentity(qp.Query)
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
					// Candidates existed and strict rejected them; leaving the
					// count at zero would make query.go report this as a mock
					// that was never recorded.
					candidateCount: mysqlCandidates,
					matchPhase:     models.MatchPhaseStrict,
				}, nil
			}
			if chosen != nil {
				var detected map[string][]string
				if noiseEngine.DetectionEnabled() {
					if liveBody, ok := gate.liveRequestBody(); ok {
						detected, _ = noiseEngine.Detect(chosen, liveBody, userBodyNoise)
					}
				}
				// The server answers no SEND_LONG_DATA: F gave the
				// application nothing back (stateServed).
				if updateMock(ctx, logger, chosen, mockDb, detected) {
					stateServed(ctx, chosen, &req.PacketBundle, nil)
				}
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
				// Count placeholders in the statement, not in the prologue —
				// a sqlcommenter comment can legitimately contain a "?".
				numParams := uint16(strings.Count(sqlStatementIdentity(sp.Query), "?"))
				newStmtID := decodeCtx.NextStmtID
				decodeCtx.NextStmtID++

				seq := byte(1)
				if req.Header != nil && req.Header.Header != nil {
					seq = req.Header.Header.SequenceID + 1
				}
				var paramDefs []*mysql.ColumnDefinition41
				// eofAfterParams ends the parameter definitions for a
				// connection that did not negotiate CLIENT_DEPRECATE_EOF:
				// such a client (go-sql-driver) reads them up to an EOF
				// packet. Without one it waited for the rest of the
				// PREPARE_OK forever, so the app hung in PREPARE and never
				// sent the EXECUTE that is the miss to report. A connection
				// that negotiated it reads no EOF there (its next packet is
				// the answer to its next command).
				eofAfterParams := []byte{}
				if numParams > 0 {
					paramDefs = make([]*mysql.ColumnDefinition41, 0, numParams)
					for i := uint16(0); i < numParams; i++ {
						paramDefs = append(paramDefs, &mysql.ColumnDefinition41{
							Header: mysql.Header{
								PayloadLength: 22,
								SequenceID:    seq + 1 + byte(i),
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
					if !decodeCtx.DeprecateEOF() {
						// EOF: 0xfe, no warnings, SERVER_STATUS_AUTOCOMMIT.
						eofAfterParams = []byte{5, 0, 0, seq + 1 + byte(numParams), mysql.EOF, 0x00, 0x00, 0x02, 0x00}
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
					EOFAfterParamDefs:  eofAfterParams,
					ColumnDefs:         nil,
					EOFAfterColumnDefs: []byte{},
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
		// Last resort: no candidate scored and strict rejected nothing, but the
		// pool is not necessarily empty. Name the nearest recorded statement so
		// the report reads "this query drifted from <that one>" instead of
		// claiming the mock was never recorded.
		closestQuery := bestPartialQuery
		if bestPartialMockName == "" && nearestMock != nil {
			bestPartialMockName, closestQuery = nearestMock.Name, nearestQuery
		}
		phase := models.MatchPhaseExhausted
		switch {
		case mysqlCandidates == 0:
			phase = models.MatchPhaseNoMocks
		case gate.rejected > 0:
			phase = models.MatchPhaseStrict
		}
		miss := &mockMiss{
			closestQuery:   closestQuery,
			closestMock:    bestPartialMockName,
			fieldDiffs:     gate.fieldDiffs,
			strictRejected: gate.rejected,
			candidateCount: mysqlCandidates,
			matchPhase:     phase,
		}
		if len(lost) > 0 {
			// The test's own mock may be one whose statement is lost: say so,
			// and, when nothing else explains the miss, what to do about it.
			unknown := unknownStatementsMiss(lost, nil, mysqlCandidates)
			miss.why = unknown.why
			if miss.strictRejected == 0 {
				miss.nextSteps = unknown.nextSteps
			}
		}
		if miss.nextSteps == "" && gate.exactRejected > 0 {
			miss.nextSteps = exactDriftNextSteps
			if req.Header.Type == sCOM_QUERY {
				miss.nextSteps = exactLiteralNextSteps
			}
		}
		return nil, false, miss, nil
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
	if noiseEngine.DetectionEnabled() && gate.hasLiveBody() {
		if body, ok := gate.bodyFor(matchedMock); ok {
			// The recording as strict compares it (strictGate.recordedBody),
			// so what is learned is what a strict replay then excuses.
			recorded, recordedOK := gate.recordedBody(matchedMock)
			detectedNoise, _ = noiseEngine.DetectRecorded(matchedMock, recorded, recordedOK, body, executeBodyNoise(userBodyNoise, matchedMock, &req.PacketBundle))
		}
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
		return nil, false, nil, &refusedConsume{mock: matchedMock}
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
				if debugOn {
					logger.Debug("Recorded runtime PREP mapping with new statement ID",
						zap.Uint32("original_stmt_id from mock ", originalStmtID),
						zap.Uint32("new_stmt_id", prepareOkRespCopy.StatementID),
						zap.String("query", strings.TrimSpace(sp.Query)))
				}
			}
		}
	}

	// Say what the serve gave the application: the command as it sent it,
	// and the answer the command phase writes for it (none to a command the
	// server does not answer: query.go). Without it a later call of the set
	// reads matchedMock as a serve no adapter stated, and refuses every
	// changed value that may stand there.
	written := &responseCopy.PacketBundle
	if wire.IsNoResponseCommand(req.Header.Type) {
		written = nil
	}
	stateServed(ctx, matchedMock, &req.PacketBundle, written)

	if debugOn {
		logger.Debug("matched command with the mock", zap.String("mock", matchedMock.Name))
	}
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

// Scores returned by matchQueryLive. Anything above zero makes the candidate
// servable in matchCommand, so only evidence that the two texts are the SAME
// STATEMENT may score.
const (
	// scoreQueryExact — the executable statements are identical. Returned with
	// ok=true, which makes the caller take the definitive path and ignore the
	// score entirely, so the value is informational only.
	scoreQueryExact = 2
	// scoreQueryLiteralDrift — same statement, drifted inline literal values.
	// Last resort, never definitive, and deliberately below scoreQueryStructure
	// so DML selection is unchanged.
	scoreQueryLiteralDrift = 3
	// scoreQueryStructure — both DML with an identical parse-tree shape.
	// Pre-existing tier, never definitive.
	scoreQueryStructure = 6
)

// liveQuery is what matchQueryLive derives from the LIVE statement. It is the
// same for every candidate one command is compared with, so matchCommand keeps
// one per command and fills it on the first comparison; the comparison used to
// derive it again for every candidate in the pool: the comment strip, the
// placeholder count, the system-variable parse, the DML check, the literal mask
// and the parse-tree signature, each a pass over the whole statement. For a
// large statement (an INSERT inlining a 1 MiB value) those passes were most of
// the cost of the scan.
type liveQuery struct {
	ready        bool
	identity     string
	placeholders int
	varName      string
	isVarRead    bool
	isDML        bool

	maskedReady bool
	masked      string

	sigReady bool
	sig      string
	sigErr   error
}

func (lq *liveQuery) fill(query string) {
	if lq.identity == "" {
		lq.identity = sqlStatementIdentity(query)
	}
	lq.placeholders = strings.Count(lq.identity, "?")
	lq.varName, lq.isVarRead = parseSingleSystemVarRead(lq.identity)
	lq.isDML = sqlparser.IsDML(lq.identity)
	lq.ready = true
}

func (lq *liveQuery) maskedLiterals() string {
	if !lq.maskedReady {
		lq.masked = maskSQLLiterals(lq.identity)
		lq.maskedReady = true
	}
	return lq.masked
}

func (lq *liveQuery) structure() (string, error) {
	if !lq.sigReady {
		lq.sig, lq.sigErr = getQueryStructureCached(lq.identity)
		lq.sigReady = true
	}
	return lq.sig, lq.sigErr
}

// matchQueryLive compares a recorded COM_QUERY or COM_STMT_PREPARE with the live
// one: (exact, score, the recorded statement's identity). The live statement's
// facts are kept in live, so a caller comparing one live statement with many
// candidates derives them once. The identity is empty only when the packet
// types differ, which returns before it is derived.
func matchQueryLive(log *zap.Logger, expected, actual mysql.PacketBundle, live *liveQuery, getQuery func(packet mysql.PacketBundle) string) (bool, int, string) {
	// Match the type and return zero if the types are not equal
	if expected.Header.Type != actual.Header.Type {
		return false, 0, ""
	}

	// Identity is the EXECUTABLE statement, not the bytes on the wire. An
	// observability prologue (Datadog DBM / sqlcommenter) is re-minted per
	// request with a fresh traceparent and names the endpoint that served the
	// call, so raw text is never twice equal for the same statement and its
	// length is not stable either. See stripInertSQLComments.
	if !live.ready {
		live.fill(getQuery(actual))
	}
	expectedQuery := sqlStatementIdentity(getQuery(expected))
	actualQuery := live.identity

	// Count placeholders in both queries - this is crucial for PREPARE statements
	// to ensure we match mocks with the same number of parameters
	expectedPlaceholders := strings.Count(expectedQuery, "?")
	actualPlaceholders := live.placeholders
	if expectedPlaceholders != actualPlaceholders {
		// log.Debug("placeholder count mismatch",
		// 	zap.String("expected_query", expectedQuery),
		// 	zap.String("actual_query", actualQuery),
		// 	zap.Int("expected_placeholders", expectedPlaceholders),
		// 	zap.Int("actual_placeholders", actualPlaceholders))
		return false, 0, expectedQuery
	}

	// Exact match on the statement — deliberately NOT gated on equal
	// PayloadLength. The wire length includes the prologue, so gating on it
	// rejects the very statements this comparison exists to match.
	if expectedQuery == actualQuery {
		if ce := log.Check(zap.DebugLevel, "Query Exact matched"); ce != nil {
			ce.Write(zap.String("expected query", expectedQuery), zap.String("actual query", actualQuery))
		}
		return true, scoreQueryExact, expectedQuery
	}

	// PayloadLength equality is NOT evidence of anything and must never score.
	// It used to award a point, which made it the only surviving signal for a
	// non-DML statement once a trace comment defeated the text comparison — so
	// the first recorded mock of the same byte length won, whatever SQL it held.
	// With a couple of hundred bytes of tracer comment in front of every
	// statement, unrelated queries collide on total length constantly: every
	// "SHOW FULL FIELDS FROM <table>" with an equal-length table name measures
	// the same. Serving one table's column metadata in answer to another's is
	// silent, and downstream it looks like an application bug (an ORM model
	// built with the wrong columns), not a mock mismatch.

	// A PURE single system-variable read (SELECT @@[session.]<var>, no columns
	// list / FROM / expression) is matched by EXACT text ONLY: it is semantically
	// identified by WHICH variable it reads, so a different @@var must never be
	// served as a length/structure "fallback". This is the exact defect behind
	// "Could not map transaction isolation '0'": SELECT @@session.transaction_
	// isolation (unrecorded, issued live by Connector/J) was cross-served the
	// recorded SELECT @@session.transaction_read_only mock's value "0" purely
	// because both are 38-byte non-DML SELECTs (matchCount==1 on equal
	// PayloadLength). Returning score 0 keeps this candidate out of the
	// score-based fallback; an un-matched read is resolved from the recorded
	// session probe in matchCommand. Scoped via parseSingleSystemVarRead (NOT a
	// bare "SELECT @@" prefix) so a real DML query that merely projects a
	// variable — e.g. "SELECT @@version, u.name FROM users u WHERE ..." — keeps
	// its normal DML fuzzy-matching.
	//
	// The cheap string inequality is checked FIRST so parseSingleSystemVarRead
	// is not even called on an equal-query candidate. On non-equal candidates
	// the parse early-exits after a couple of prefix comparisons for anything
	// that is not "SELECT @@...", so it is strictly cheaper than the
	// sqlparser.IsDML parse that already runs per candidate just below.
	//
	// The rejection is narrowed to a DIFFERENT variable rather than applied to any
	// textual difference. matchQueryLive is the SHARED MySQL replay path, used by proxy
	// (MITM) and DaemonSet recordings as well as the proxyless capture this change
	// targets, and rejecting on text alone is subtractive: a recorded read of the
	// SAME variable whose text differs only cosmetically (extra whitespace, a
	// `session.` qualifier on one side, a stripped comment) previously reached the
	// score-based fallback and matched. Hard-rejecting it would make replay depend
	// on buildSessionVarResponse succeeding, which itself bails out when the
	// connection did not negotiate CLIENT_DEPRECATE_EOF or when no recorded result
	// set carries that column — i.e. it could fail an existing recording that
	// passes today, in modes this change is not meant to touch.
	//
	// Comparing the parsed variable NAMES keeps the defect fixed (a different @@var
	// is still never cross-served) while leaving same-variable candidates eligible
	// for normal matching.
	if rejectsCrossVariableRead(expectedQuery, actualQuery, live.varName, live.isVarRead) {
		return false, 0, expectedQuery
	}

	// sqlparser.IsDML parses the statement, so it is the most expensive check in
	// this function and it is deterministic for a given query. On the per-command
	// O(pool) match path the old code parsed each side 3-4x per candidate; compute
	// each side ONCE and reuse. (Profiled: IsDML re-parsing is a measurable slice
	// of replay agent CPU when the candidate pool is large.)
	expectedIsDML := sqlparser.IsDML(expectedQuery)
	actualIsDML := live.isDML

	// check if any of them the query is dml and other is not, then there is no match.
	if expectedIsDML && !actualIsDML {
		log.Debug("expected query is dml but actual is not",
			zap.String("expected query", expectedQuery),
			zap.String("actual query", actualQuery))
		return false, 0, expectedQuery
	} else if !expectedIsDML && actualIsDML {
		log.Debug("actual query is dml but expected is not",
			zap.String("expected query", expectedQuery),
			zap.String("actual query", actualQuery))
		return false, 0, expectedQuery
	}

	if !(expectedIsDML && actualIsDML) {
		// Non-DML. sqlparser.IsDML covers only INSERT/UPDATE/DELETE, so this is
		// where every SELECT and SHOW lands, and the parse-tree tier below never
		// sees them. Last resort: the same statement with a drifted inline
		// literal — a client that interpolates a freshly generated id instead of
		// binding it re-issues exactly this shape on every run, and refusing it
		// tears the connection down.
		//
		// Not definitive: the recorded response belongs to a different row, so it
		// only scores, and only wins when nothing matched exactly.
		if !isSessionControlStatement(expectedQuery) && !isSessionControlStatement(actualQuery) &&
			maskSQLLiterals(expectedQuery) == live.maskedLiterals() {
			log.Debug("query matched with drifted inline literals",
				zap.String("expected query", expectedQuery),
				zap.String("actual query", actualQuery))
			// The payload-length tie-break only ranks candidates already known to
			// share every keyword, identifier and literal type.
			return false, scoreQueryLiteralDrift + equalPayloadLength(expected, actual), expectedQuery
		}
		log.Debug("No Query is dml",
			zap.String("expected query", expectedQuery),
			zap.String("actual query", actualQuery))
		return false, 0, expectedQuery
	}

	// Both are DML: fall back to comparing their parse-tree shape, unchanged
	// from before. This tier is identifier-blind and is deliberately left as it
	// was — it is not what this change is about, and it never returns a
	// definitive match.
	actualSignature, err := live.structure()
	if err != nil {
		log.Debug("failed to get actual query structure",
			zap.String("actual Query", actualQuery),
			zap.Error(err))
		return false, 0, expectedQuery
	}

	expectedSignature, err := getQueryStructureCached(expectedQuery)
	if err != nil {
		log.Debug("failed to get expected query structure",
			zap.String("expected Query", expectedQuery),
			zap.Error(err))
		return false, 0, expectedQuery
	}

	if expectedSignature == actualSignature {
		log.Debug("query structure matched",
			zap.String("expected signature", expectedSignature),
			zap.String("actual signature", actualSignature))
		// The +1 for equal PayloadLength is the DML tier's original tie-break
		// between two structurally identical write statements, and it is kept so
		// this tier behaves exactly as before. It is only ever a tie-break HERE,
		// among candidates already known to share a parse tree — quite unlike its
		// removed use as the sole signal for any statement of the same size.
		return false, scoreQueryStructure + equalPayloadLength(expected, actual), expectedQuery
	}

	return false, 0, expectedQuery
}

func queryPacketText(packet mysql.PacketBundle) string {
	msg, _ := packet.Message.(*mysql.QueryPacket)
	return msg.Query
}

func preparePacketText(packet mysql.PacketBundle) string {
	msg, _ := packet.Message.(*mysql.StmtPreparePacket)
	return msg.Query
}

// maskedAttributeValue stands for a query attribute's value in the debug log
// (executeForLog), the mismatch summary (formatExecParams) and a strict
// miss's field diffs (maskAttributeDiffs).
const maskedAttributeValue = "[masked]"

// executeForLog is a COM_STMT_EXECUTE as the debug log shows it: its query
// attributes' values (its parameters sent with a name; a statement's own are
// sent unnamed) masked. The client names an attribute (a password, a
// tenant's token), and what redacts a log goes by what a value looks like,
// not by such a name: one only its name says is a secret was logged in the
// clear.
func executeForLog(p *mysql.StmtExecutePacket) *mysql.StmtExecutePacket {
	if p == nil {
		return nil
	}
	c := *p
	c.Parameters = slices.Clone(p.Parameters)
	for i := range c.Parameters {
		if c.Parameters[i].Name != "" && c.Parameters[i].Value != nil {
			c.Parameters[i].Value = maskedAttributeValue
		}
	}
	return &c
}

// typesSentByBoth reports whether both COM_STMT_EXECUTEs carry their
// parameters' types, unsigned flags and names on the wire
// (new_params_bind_flag 1). A packet sent without them carries none: its
// decoded ones are filled in, the statement's last sent types (or
// FieldTypeString where none is known, see DecodeStmtExecute), and a released
// recorder stored FieldTypeString for every one. Filled-in types are no
// evidence either way, so they are compared only when both packets sent
// theirs. The ones a flag-0 packet stands for were compared on the
// statement's EXECUTE that sent them.
func typesSentByBoth(expected, actual *mysql.StmtExecutePacket) bool {
	return expected != nil && actual != nil && expected.NewParamsBindFlag == 1 && actual.NewParamsBindFlag == 1
}

// legacyRecorded reports whether a recorded COM_STMT_EXECUTE is one sent
// without its types that a keploy older than the one reading such a packet
// with its statement's types recorded: it typed every parameter
// FieldTypeString, and read every value as a string
// (mysql.StmtExecutePacket's LegacyParameters). One this keploy recorded
// carries the statement's types, so it is legacyRecorded only when every
// type was a string's, whose values both read alike.
func legacyRecorded(p *mysql.StmtExecutePacket) bool {
	if p == nil || p.NewParamsBindFlag == 1 || len(p.Parameters) == 0 {
		return false
	}
	for _, param := range p.Parameters {
		if param.Type != uint16(mysql.FieldTypeString) || param.Unsigned || param.Name != "" {
			return false
		}
	}
	return true
}

// legacyRecordedQuery reports whether a recorded COM_QUERY carrying query
// attributes is one a keploy older than the one reading their names recorded
// (mysql.QueryPacket's LegacyQuery): it read no name. The protocol sends one
// with every attribute, so this keploy records them.
func legacyRecordedQuery(p *mysql.QueryPacket) bool {
	if p == nil || len(p.Parameters) == 0 {
		return false
	}
	for _, param := range p.Parameters {
		if param.Name != "" {
			return false
		}
	}
	return true
}

// readsLegacy reports whether a live COM_QUERY is compared with a recording
// legacyRecordedQuery holds as an older keploy read it (its LegacyQuery): that
// keploy could read it, and it names an attribute, so this keploy would have
// recorded the name. A live one naming none (an attribute named "") is read as
// this keploy reads it: a recording of it holds no name either way.
func readsLegacy(act *mysql.QueryPacket) bool {
	if act == nil || act.LegacyQuery == "" {
		return false
	}
	for _, p := range act.Parameters {
		if p.Name != "" {
			return true
		}
	}
	return false
}

// legacyRecordedMock is legacyRecorded, or legacyRecordedQuery, for a mock's
// one request.
func legacyRecordedMock(m *models.Mock) bool {
	if m == nil || len(m.Spec.MySQLRequests) != 1 {
		return false
	}
	switch exp := m.Spec.MySQLRequests[0].Message.(type) {
	case *mysql.StmtExecutePacket:
		return legacyRecorded(exp)
	case *mysql.QueryPacket:
		return legacyRecordedQuery(exp)
	}
	return false
}

// executeAttributesFrom is the index of the first of a COM_STMT_EXECUTE's
// parameters that is a query attribute, not one of its statement's own: the
// server takes the first num_params (the statement's PREPARE_OK's) as the
// statement's and the rest as attributes, each sent with its name.
// statementParams is that count when known (a live packet's statement, or a
// recording of the same statement), -1 when not (a recording read back): a
// client sends its statement's own parameters unnamed (Connector/J), so such
// a packet's attributes are its trailing run of named parameters. A packet
// whose statement's own are named too is split before them: compared with a
// live one split by its count, its parameters differ, never match as fewer.
// A packet sent without types (new_params_bind_flag 0) sends no names, and of
// attributes only a NULL, the same on every call: it is not split.
func executeAttributesFrom(p *mysql.StmtExecutePacket, statementParams int) int {
	if p.NewParamsBindFlag != 1 {
		return len(p.Parameters)
	}
	if statementParams >= 0 {
		return min(statementParams, len(p.Parameters))
	}
	from := len(p.Parameters)
	for from > 0 && p.Parameters[from-1].Name != "" {
		from--
	}
	return from
}

// recordedAttributesFrom is executeAttributesFrom for a recorded
// COM_STMT_EXECUTE compared with a live one (live) whose statement has
// liveParams parameters of its own (-1 when not known). A recording of the
// same statement (sameStatement: the same query) has as many. Of another, or
// one whose query is not known, the trailing run of named parameters is
// taken for its attributes only when it leaves as many as the live
// statement's and is named as the live packet's attributes are: a client
// that names its statement's own too leaves fewer, or, for a live statement
// with none, leaves none and splits off its statement's own, named as no
// attribute the live packet sent. The recording is then not split at all
// (its attributes compared, so it matches no live EXECUTE whose attributes
// differ), never compared on fewer.
func recordedAttributesFrom(p, live *mysql.StmtExecutePacket, sameStatement bool, liveParams int) int {
	if p.NewParamsBindFlag != 1 {
		return len(p.Parameters)
	}
	if liveParams >= 0 && sameStatement {
		return min(liveParams, len(p.Parameters))
	}
	from := executeAttributesFrom(p, -1)
	if liveParams < 0 {
		return from
	}
	if from != liveParams || live == nil || !sameNames(p.Parameters[from:], live.Parameters[executeAttributesFrom(live, liveParams):]) {
		return len(p.Parameters)
	}
	return from
}

// sameNames reports whether a and b are as many parameters, named alike in
// order.
func sameNames(a, b []mysql.Parameter) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name {
			return false
		}
	}
	return true
}

// executeOwnParameters is p's parameters that are its statement's own, its
// query attributes left out (executeAttributesFrom).
func executeOwnParameters(p *mysql.StmtExecutePacket, statementParams int) []mysql.Parameter {
	return p.Parameters[:executeAttributesFrom(p, statementParams)]
}

// allNamed reports whether every one of params was sent with a name.
func allNamed(params []mysql.Parameter) bool {
	for _, p := range params {
		if p.Name == "" {
			return false
		}
	}
	return true
}

// sameParameterShape reports whether a recorded COM_STMT_EXECUTE (expected)
// may have run the live one's statement (actual), as far as their bound
// parameters show: as many of the statement's own (executeAttributesFrom;
// liveStatementParams is the live statement's count, -1 when not known), and
// each of the same type wherever both are bound to a value (a NULL is sent
// with a type of its own) and both packets sent their types
// (typesSentByBoth).
func sameParameterShape(expected, actual *mysql.StmtExecutePacket, liveStatementParams int) bool {
	if expected == nil || actual == nil {
		return false
	}
	exp := expected.Parameters[:recordedAttributesFrom(expected, actual, false, liveStatementParams)]
	act := executeOwnParameters(actual, liveStatementParams)
	if len(exp) > len(act) && allNamed(exp[len(act):]) {
		// Not split (recordedAttributesFrom), and its parameters past the
		// live statement's count all named: a client that names its
		// statement's own too may have sent the live statement's first,
		// its attributes after them.
		exp = exp[:len(act)]
	}
	if len(exp) != len(act) {
		return false
	}
	if !typesSentByBoth(expected, actual) {
		return true
	}
	for i := range exp {
		e, a := exp[i], act[i]
		if e.Value != nil && a.Value != nil && e.Type != a.Type {
			return false
		}
	}
	return true
}

// query-aware EXEC scoring.
//   - Keep the existing header/flags/params scoring.
//   - Do NOT reward raw StatementID equality.
//   - If both expectedQuery and actualQuery are present, require them to be
//     the same statement (definitive with every parameter equal) or of the
//     same shape (sameQueryStructure: scores, never definitive). Otherwise
//     return (false, 0, false): the recording ran another statement.
//   - If either query is missing, fall back to best-effort scoring (returns (false, score)).
//
// Under exact serving the caller serves by score only a recording of the
// same statement (see sameStatementOnly in matchCommandWith).
//
// Returns (definitive, score, queryExactMatched). queryExactMatched is true
// when the recorded prepared-statement query text equals the live query text
// (case-insensitive) regardless of whether the bound parameters matched. The
// caller uses this third value to drive a FIFO fallback: when no candidate is
// a definitive param-exact match, the next UNCONSUMED per-test mock for the
// same query (in recorded order) is served, so an INSERT-then-SELECT read-back
// of a replay-generated id returns the row that was read back at record time.
func matchStmtExecutePacketQueryAware(logger *zap.Logger, expected, actual mysql.PacketBundle, expectedQuery, actualQuery string, liveStatementParams int, mockName string, nc *util.NoiseChecker) (bool, int, bool) {
	return matchStmtExecuteLive(logger, expected, actual, expectedQuery, newLiveExecStatement(actualQuery), liveStatementParams, mockName, nc)
}

// liveExecStatement is what matchStmtExecuteLive derives from a live
// COM_STMT_EXECUTE's statement: the same for every candidate one command is
// compared with, so matchCommand derives it once per command.
type liveExecStatement struct {
	// query is the statement the live command runs, trimmed ("" unknown).
	query string
	// identity is sqlStatementIdentity(query).
	identity string
	// readsAttributes is readsQueryAttributes(query).
	readsAttributes bool
}

func newLiveExecStatement(query string) liveExecStatement {
	return liveExecStatement{query: query, identity: sqlStatementIdentity(query), readsAttributes: readsQueryAttributes(query)}
}

// matchStmtExecuteLive is matchStmtExecutePacketQueryAware with the live
// statement's facts derived once (liveExecStatement).
func matchStmtExecuteLive(logger *zap.Logger, expected, actual mysql.PacketBundle, expectedQuery string, live liveExecStatement, liveStatementParams int, mockName string, nc *util.NoiseChecker) (bool, int, bool) {
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

	// Match the parameters: their types, unsigned flags and names only when
	// both packets sent them (typesSentByBoth). The statement's own: its
	// query attributes (executeAttributesFrom) are compared only when the
	// statement reads them (readsQueryAttributes), as a COM_QUERY's are
	// (mysqlRequestBodyJSON). A client sends ones that differ on every call
	// (Connector/J 8.1+, the caller's OpenTelemetry traceparent), so compared,
	// no EXECUTE of a traced statement ever matched its recording.
	// A recording is split as recordedAttributesFrom says: by the live
	// statement's count when it recorded the same statement (the same
	// query).
	// eq and aq are the statements' identities: the same identity rule as
	// matchQueryLive, see "Query logic" below.
	eq, aq := sqlStatementIdentity(expectedQuery), live.identity
	expParams, actParams := expectedMessage.Parameters, actualMessage.Parameters
	if !live.readsAttributes && !readsQueryAttributes(expectedQuery) {
		same := eq != "" && strings.EqualFold(eq, aq)
		expParams = expectedMessage.Parameters[:recordedAttributesFrom(expectedMessage, actualMessage, same, liveStatementParams)]
		actParams = executeOwnParameters(actualMessage, liveStatementParams)
	}
	typesSent := typesSentByBoth(expectedMessage, actualMessage)
	var totalMatchedParams int
	var allParamsMatched bool
	if len(expParams) == len(actParams) {
		for i := range expParams {
			ep := expParams[i]
			ap := actParams[i]

			typeEqual := !typesSent || ep.Type == ap.Type
			nameEqual := !typesSent || ep.Name == ap.Name
			unsignedEqual := !typesSent || ep.Unsigned == ap.Unsigned
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
		if len(expParams) == totalMatchedParams {
			allParamsMatched = true
			if ce := logger.Check(zap.DebugLevel, "all parameters matched"); ce != nil {
				ce.Write(zap.String("mock-name", mockName))
			}
		}

		// An older keploy recorded a packet sent without types as it read
		// it: every value a string (legacyRecorded), a small integer's
		// bytes of garbage. Its replayer read the live packet the same way,
		// and so this one does for such a recording (LegacyParameters).
		if !allParamsMatched && legacyRecorded(expectedMessage) && len(actualMessage.LegacyParameters) == len(expectedMessage.Parameters) {
			legacy := 0
			for i, ep := range expectedMessage.Parameters {
				if paramValueEqual(ep.Value, actualMessage.LegacyParameters[i].Value, nc) {
					legacy++
				}
			}
			if legacy > totalMatchedParams {
				matchCount += legacy - totalMatchedParams
				totalMatchedParams = legacy
				if legacy == len(expectedMessage.Parameters) {
					allParamsMatched = true
					if ce := logger.Check(zap.DebugLevel, "all parameters matched as an older keploy read them"); ce != nil {
						ce.Write(zap.String("mock-name", mockName))
					}
				}
			}
		}
	}

	// Query logic:
	queryMatched := false
	queryExactMatched := false
	// Same identity rule as matchQueryLive: a leading observability prologue is not
	// part of the prepared statement. Without this an app that traces its
	// statements would never register a query-exact EXECUTE, and the read-back
	// FIFO in matchCommand — which keys off queryExactMatched — would fall back
	// to an arbitrary same-shape row. eq and aq are derived above.

	// If both queries are present, require them to match (exact or structural) for a definitive match.
	if eq != "" && aq != "" {
		// If both queries are available, require an exact or structural match to treat this as a definitive match.
		switch {
		case strings.EqualFold(eq, aq):
			matchCount += 10
			queryMatched = true
			queryExactMatched = true
			if ce := logger.Check(zap.DebugLevel, "query matched exactly"); ce != nil {
				ce.Write(zap.String("related stmt-exec mock-name", mockName))
			}
		case sameQueryStructure(eq, aq):
			matchCount += 6
			if ce := logger.Check(zap.DebugLevel, "query structure matched"); ce != nil {
				ce.Write(zap.String("related stmt-exec mock-name", mockName))
			}
		default:
			// The recording ran another statement, of another shape. It
			// never answers this one, in any mode. Scored on the header,
			// status, flags and parameters alone, it did: any recorded
			// EXECUTE answered an EXECUTE of a statement the recording
			// never ran ("SELECT code FROM other WHERE ref = ? AND live =
			// 1" with 5 got the rows of "SELECT id, name FROM small WHERE
			// id = ?" with 7).
			return false, 0, false
		}
	}

	if allParamsMatched && eq == "" && aq == "" {
		return true, matchCount, queryExactMatched
	}

	if allParamsMatched && eq == "" {
		if ce := logger.Check(zap.DebugLevel, "EXECUTE matched on params alone (mock has no recorded PREPARE)"); ce != nil {
			ce.Write(zap.String("mock-name", mockName), zap.String("actual_query", truncate(aq, 200)))
		}
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
			// Compare at float32 precision, not float64. One side is
			// genuinely a float32 (FieldTypeFloat off the wire) and the
			// other has been through YAML, which writes a float32 in its
			// shortest 32-bit form ("9.99") and reads it back as
			// float64(9.99). Widening asks the float32 to carry precision
			// it never had — float64(float32(9.99)) is 9.989999771118164,
			// so a correctly recorded FLOAT param never matched itself.
			//
			// This is the same direction the int/uint arms below already
			// take. Narrowing does collide for magnitudes float32 cannot
			// hold (1e-300 compares equal to 0), but a float32 only ever
			// enters here from a FieldTypeFloat decode and a MySQL FLOAT
			// cannot carry those, so no real column reaches the collision.
			return av == float32(bv)
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
			// Narrow, don't widen — see the float32 arm above.
			return float32(av) == bv
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
//
// What it consumes is told to the learn step as served in this connection's
// request (integrations.ServingIn), so a request aloft on another connection
// that reads no recording leaves this one's serve in its order.
func updateMock(ctx context.Context, logger *zap.Logger, matchedMock *models.Mock, mockDb integrations.MockMemDb, detectedNoise map[string][]string) bool {
	mockDb = integrations.ServingIn(ctx, mockDb)
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

	// usedOrRefused settles a consume the update door turned down. Under
	// exact serving UpdateUnFilteredMock's false is no refusal on its own (a
	// mock the view shows that the tree it updates does not hold is one too),
	// and the view refuses at every door what it refuses: MarkMockAsUsed says
	// which it was, as integrations.ConsumeRefused reads it, so a refused
	// consume is matched again (matchCommand) and only a refused one.
	// Without exact serving the false stays failed bookkeeping.
	usedOrRefused := func() bool {
		if !integrations.ExactServing(mockDb) {
			return false
		}
		used := *matchedMock
		if len(detectedNoise) > 0 {
			used.Spec.ReqBodyNoise = schemanoise.MergeLearned(used.Spec.ReqBodyNoise, detectedNoise)
		}
		return mockDb.MarkMockAsUsed(used)
	}

	if isSessionOrConnection {
		updated := mockDb.UpdateUnFilteredMock(matchedMock, &updatedMock) || usedOrRefused()
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
	if mockDb.UpdateUnFilteredMock(matchedMock, &updatedMock) || usedOrRefused() {
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
	for _, m := range unfiltered {
		if connID, e, ok := recordedPrepEntry(m); ok {
			out[connID] = append(out[connID], e)
		}
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
	// Identity ignores inert comments — see stripInertSQLComments.
	eq := sqlStatementIdentity(expectedQuery)
	aq := sqlStatementIdentity(actualQuery)
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

// rejectsCrossVariableRead reports whether a candidate must be excluded from
// matching because it would serve one system variable's recorded value in answer
// to a read of a DIFFERENT variable.
//
// A pure single system-variable read is identified by WHICH variable it reads, so
// it may only be answered by a recorded read of that same variable. Without this,
// two equal-length non-DML SELECTs score as interchangeable and
// "SELECT @@session.transaction_isolation" gets served the recorded
// "SELECT @@session.transaction_read_only" value, which is the defect behind
// "Could not map transaction isolation '0'".
//
// It deliberately compares parsed variable NAMES rather than raw text. matchQueryLive
// is the shared MySQL replay path used by proxy (MITM) and DaemonSet recordings as
// well as proxyless capture, and rejecting on any textual difference would be
// subtractive: a recorded read of the SAME variable differing only cosmetically (a
// `session.` qualifier, extra whitespace, a stripped comment, a trailing
// semicolon) previously remained eligible and could match. Rejecting those would
// make replay depend on buildSessionVarResponse succeeding, and that bails out for
// a connection without CLIENT_DEPRECATE_EOF or a variable absent from every
// recorded result set, so an existing recording that passes today could start
// failing.
//
// actualVar and isPureVarRead are parseSingleSystemVarRead(actualQuery), which
// the caller derives once per command. Returns false for identical text, which is
// the common case and costs one string comparison.
func rejectsCrossVariableRead(expectedQuery, actualQuery, actualVar string, isPureVarRead bool) bool {
	if expectedQuery == actualQuery {
		return false
	}
	if !isPureVarRead {
		return false
	}
	expectedVar, expectedIsVarRead := parseSingleSystemVarRead(expectedQuery)
	return !expectedIsVarRead || !strings.EqualFold(expectedVar, actualVar)
}

// sqlWhitespace is the single definition of "whitespace" used by the
// system-variable parsing below. Keeping one set avoids the class of bug where the
// keyword check accepts \r but the trimming does not, leaving it attached to the
// parsed variable name.
const sqlWhitespace = " \t\r\n"

// stripInertSQLComments removes every comment the server DISCARDS, leaving the
// executable statement. It is the identity used to decide whether a live query
// is the same statement as a recorded one.
//
// Two sources put comments in a statement:
//
//   - Drivers. Connector/J prepends a version banner and a "/* ping */" marker.
//   - Client-side observability. Datadog DBM, Google sqlcommenter and
//     OpenTelemetry inject a "/* key='value',... */" comment carrying a
//     per-request W3C traceparent and the database endpoint that served the
//     call — usually in front of the statement, but a tracer is free to put it
//     anywhere.
//
// None of it reaches the server as SQL, so none of it is part of the
// statement's identity. The observability comment in particular is hostile to
// identity-by-raw-text: the traceparent makes the same statement a different
// byte string on every execution, and the endpoint name changes the comment's
// LENGTH when a cluster hands out its reader endpoint ("...cluster-ro-...")
// instead of its writer ("...cluster-...").
//
// Two comment forms ARE executable and are deliberately left in place, because
// removing them would equate statements the server treats differently:
//
//	/*! ... */   version-gated SQL — the server RUNS the contents
//	/*+ ... */   optimizer hints — they change how the server runs it
//
// The scan is quote-aware: a "/*" or "--" inside a string literal or a
// backquoted identifier is data, not a comment, and is preserved. Removing a
// comment leaves exactly one space so that "SELECT/*c*/1" cannot become
// "SELECT1"; an unterminated comment is left untouched, since guessing where it
// was meant to end could silently delete real SQL.
func stripInertSQLComments(sql string) string {
	// Fast path: nothing that could start a comment.
	if !strings.ContainsAny(sql, "/-#") {
		return strings.TrimSpace(sql)
	}

	var b strings.Builder
	b.Grow(len(sql))

	// writeGap appends a single separating space unless one is already there.
	writeGap := func() {
		out := b.String()
		if len(out) > 0 && !strings.ContainsRune(sqlWhitespace, rune(out[len(out)-1])) {
			b.WriteByte(' ')
		}
	}

	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		// Quoted string or backquoted identifier: copy verbatim to its close.
		case c == '\'' || c == '"' || c == '`':
			j := i + 1
			for j < len(sql) {
				if sql[j] == '\\' && c != '`' {
					// Backslash escapes inside string literals (not identifiers).
					j += 2
					continue
				}
				if sql[j] == c {
					// A doubled quote is an escaped quote, not the close.
					if j+1 < len(sql) && sql[j+1] == c {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			if j > len(sql) {
				j = len(sql)
			}
			b.WriteString(sql[i:j])
			i = j

		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			// Executable comment forms are part of the statement. Copy the WHOLE
			// span verbatim — emitting just "/" and re-entering the scanner would
			// let the generic rules loose on the interior, and a "--" or "#" in
			// there would run past the closing "*/" and swallow the rest of the
			// statement.
			if i+2 < len(sql) && (sql[i+2] == '!' || sql[i+2] == '+') {
				if closeAt := strings.Index(sql[i+2:], "*/"); closeAt >= 0 {
					end := i + 2 + closeAt + 2
					b.WriteString(sql[i:end])
					i = end
					continue
				}
				// Unterminated: the rest is all statement.
				b.WriteString(sql[i:])
				i = len(sql)
				continue
			}
			// MySQL block comments do not nest: the first "*/" closes.
			closeAt := strings.Index(sql[i+2:], "*/")
			if closeAt < 0 {
				// Unterminated: copy the rest verbatim.
				b.WriteString(sql[i:])
				i = len(sql)
				continue
			}
			i = i + 2 + closeAt + 2
			writeGap()
			i = skipSQLWhitespace(sql, i)

		case isLineCommentAt(sql, i):
			if nl := strings.IndexByte(sql[i:], '\n'); nl >= 0 {
				i += nl + 1
			} else {
				i = len(sql)
			}
			writeGap()
			i = skipSQLWhitespace(sql, i)

		default:
			b.WriteByte(c)
			i++
		}
	}

	return strings.TrimSpace(b.String())
}

// equalPayloadLength returns 1 when both packets declare the same wire payload
// length, else 0. It is ONLY a tie-break inside the DML parse-tree tier; it is
// deliberately not a signal anywhere else, because with a couple of hundred
// bytes of tracer comment in front of every statement, unrelated queries share a
// byte count constantly.
func equalPayloadLength(expected, actual mysql.PacketBundle) int {
	if expected.Header == nil || expected.Header.Header == nil ||
		actual.Header == nil || actual.Header.Header == nil {
		return 0
	}
	if expected.Header.Header.PayloadLength == actual.Header.Header.PayloadLength {
		return 1
	}
	return 0
}

// maskSQLLiterals returns the statement with every inline literal VALUE replaced
// by a type-tagged placeholder, leaving keywords, identifiers and punctuation
// exactly as they were.
//
//	SELECT id FROM customers WHERE id = 'a3f...'   ->  SELECT id FROM customers WHERE id = ?s
//	SELECT id FROM customers WHERE id = 'b71...'   ->  SELECT id FROM customers WHERE id = ?s
//
// This is the last-resort tier for a client that inlines its literals instead of
// binding them: the same statement re-issued with a freshly generated id is the
// same statement, and refusing to serve it tears the connection down.
//
// It is emphatically NOT a general similarity measure, and the two things it
// keeps are what make it safe:
//
//   - IDENTIFIERS survive, so "SHOW FULL FIELDS FROM `invoices`" can never be
//     answered by "... FROM `couriers`". This is the failure that byte-length
//     scoring used to cause. Double-quoted tokens count as identifiers here
//     precisely to keep this true under ANSI_QUOTES — see the case below.
//   - The literal's TYPE survives (?s vs ?n), so "x = 1" and "x = '1'" stay
//     distinct — MySQL does not treat them alike.
//
// What it does NOT preserve is WHICH rows the statement selects: "LIMIT 10" and
// "LIMIT 20", "OFFSET 0" and "OFFSET 200", "status = 'active'" and
// "status = 'deleted'" all mask alike. That is inherent to the tier — its whole
// purpose is to serve a statement whose literal drifted — but it means a
// paginated read can be answered with a different page's rows when the exact
// recording is absent. It is why this tier is last, never definitive, and
// scored below every tier that identifies a statement outright.
//
// It is computed lexically, with no SQL parser, deliberately: a parser renders
// statements it does not model to a constant string, which would make unrelated
// statements compare equal. Anything this scanner is unsure of is copied
// verbatim, so ambiguity fails toward "no match" rather than a false one.
func maskSQLLiterals(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))

	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		// Backquoted identifier: a value's name, not a value. Keep it.
		case c == '`':
			j := scanQuoted(sql, i, '`')
			b.WriteString(sql[i:j])
			i = j

		// Double-quoted token: AMBIGUOUS, so it is kept verbatim rather than
		// masked. Under the default sql_mode it is a string literal, but under
		// ANSI_QUOTES it is an IDENTIFIER — and nothing on the wire says which
		// mode the session is in. Masking it would break the guarantee this
		// whole tier rests on: with `"users"` and `"orders"` both collapsing to
		// ?s, `SELECT id FROM "customers"` masks equal to `SELECT id FROM
		// "orders"` and one table's rows get served for another's — the exact
		// cross-serve this matcher exists to prevent, reached through a
		// different quoting style.
		//
		// Keeping it verbatim costs only that a client which BOTH quotes its
		// string literals with " AND interpolates them loses the drift tier for
		// those statements: they stop matching instead of matching wrongly.
		// That is this scanner's stated bias — ambiguity fails toward "no
		// match" — and single-quoted literals, which is what drivers and ORMs
		// actually emit, are unaffected.
		case c == '"':
			j := scanQuoted(sql, i, '"')
			b.WriteString(sql[i:j])
			i = j

		// Single-quoted string literal: unambiguously a value.
		case c == '\'':
			i = scanQuoted(sql, i, c)
			b.WriteString("?s")

		// Executable comment ("/*!", "/*+") — the identity keeps these, so copy
		// the span through untouched rather than masking inside it.
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			if closeAt := strings.Index(sql[i+2:], "*/"); closeAt >= 0 {
				end := i + 2 + closeAt + 2
				b.WriteString(sql[i:end])
				i = end
				continue
			}
			b.WriteString(sql[i:])
			i = len(sql)

		case isNumericLiteralStart(sql, i):
			j := scanNumericLiteral(sql, i)
			// A digit run that runs straight into an identifier character was
			// never a literal (a quirky column name); copy it verbatim.
			if j < len(sql) && isIdentByte(sql[j]) {
				b.WriteByte(c)
				i++
				continue
			}
			b.WriteString("?n")
			i = j

		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// scanQuoted returns the index just past the token opened by quote q at sql[i],
// honouring backslash escapes (not inside backquotes) and doubled quotes. An
// unterminated token runs to the end of the input.
func scanQuoted(sql string, i int, q byte) int {
	j := i + 1
	for j < len(sql) {
		if sql[j] == '\\' && q != '`' {
			j += 2
			continue
		}
		if sql[j] == q {
			if j+1 < len(sql) && sql[j+1] == q {
				j += 2
				continue
			}
			return j + 1
		}
		j++
	}
	return len(sql)
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// isNumericLiteralStart reports whether a numeric literal begins at sql[i]. A
// digit that continues an identifier (the "8" in "utf8mb4") does not.
func isNumericLiteralStart(sql string, i int) bool {
	if sql[i] < '0' || sql[i] > '9' {
		return false
	}
	return i == 0 || !isIdentByte(sql[i-1])
}

// scanNumericLiteral returns the index just past the numeric literal at sql[i],
// covering hex (0x1F), decimals and exponents.
func scanNumericLiteral(sql string, i int) int {
	j := i
	if sql[j] == '0' && j+1 < len(sql) && (sql[j+1] == 'x' || sql[j+1] == 'X') {
		j += 2
		for j < len(sql) && isHexByte(sql[j]) {
			j++
		}
		return j
	}
	for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
		j++
	}
	if j < len(sql) && sql[j] == '.' {
		j++
		for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
			j++
		}
	}
	// Exponent, only when it is actually followed by digits.
	if j < len(sql) && (sql[j] == 'e' || sql[j] == 'E') {
		k := j + 1
		if k < len(sql) && (sql[k] == '+' || sql[k] == '-') {
			k++
		}
		if k < len(sql) && sql[k] >= '0' && sql[k] <= '9' {
			for k < len(sql) && sql[k] >= '0' && sql[k] <= '9' {
				k++
			}
			j = k
		}
	}
	return j
}

func isHexByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// isSessionControlStatement reports whether the statement configures the session
// rather than reading or writing rows. Its literal IS its meaning — "SET NAMES
// utf8mb4" and "SET NAMES latin1" do different things — so it is excluded from
// literal-drift matching.
func isSessionControlStatement(sql string) bool {
	s := strings.TrimLeft(sql, sqlWhitespace)
	return hasPrefixFold(s, "SET") && (len(s) == 3 || strings.ContainsRune(sqlWhitespace, rune(s[3])))
}

// isLineCommentAt reports whether a MySQL line comment starts at sql[i].
//
// "#" always starts one. "--" starts one only when followed by whitespace or
// end-of-input — bare "--x" is the double unary minus operator, so treating it
// as a comment would silently delete half an expression.
func isLineCommentAt(sql string, i int) bool {
	if sql[i] == '#' {
		return true
	}
	if sql[i] != '-' || i+1 >= len(sql) || sql[i+1] != '-' {
		return false
	}
	return i+2 >= len(sql) || strings.ContainsRune(sqlWhitespace, rune(sql[i+2]))
}

func skipSQLWhitespace(sql string, i int) int {
	for i < len(sql) && strings.ContainsRune(sqlWhitespace, rune(sql[i])) {
		i++
	}
	return i
}

// sqlStatementIdentity returns the text that identifies a statement for
// matching: the executable statement with every inert comment removed.
//
// A statement that is NOTHING but comments — Connector/J's "/* ping */" — has
// no executable body, and collapsing every such probe to the empty string would
// make them all interchangeable. Those keep their raw text.
func sqlStatementIdentity(query string) string {
	if query == "" {
		return ""
	}
	if v, ok := stmtIdentityCache.Get(query); ok {
		return v
	}
	id := stripInertSQLComments(query)
	if id == "" {
		id = strings.TrimSpace(query)
	}
	stmtIdentityCache.Add(query, id)
	return id
}

// commonPrefixLen returns the length of the longest shared prefix of a and b.
// It is the closeness measure used to name the nearest recorded query in a
// mismatch report. It is a REPORTING signal only and never feeds the match
// score — two statements sharing a prefix are not thereby the same statement.
func commonPrefixLen(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

// parseSingleSystemVarRead parses "SELECT @@[session.|global.|local.]<var>" and
// returns the bare variable name (e.g. "transaction_isolation"). ok is false for
// anything that isn't a SINGLE bare variable read (multi-column, AS aliases,
// expressions, function calls) — so it only fires for the deterministic
// single-variable probes Connector/J issues at connection setup.
func parseSingleSystemVarRead(query string) (string, bool) {
	s := stripInertSQLComments(query)
	// Require the SELECT keyword followed by at least one whitespace character.
	// Match any whitespace (space, tab, CR, LF) rather than a single literal
	// space, so "SELECT\t@@x" is recognised too.
	const kw = "SELECT"
	if len(s) <= len(kw) || !strings.EqualFold(s[:len(kw)], kw) {
		return "", false
	}
	if !strings.ContainsRune(sqlWhitespace, rune(s[len(kw)])) {
		return "", false
	}
	rest := strings.TrimSpace(s[len(kw):])
	if !strings.HasPrefix(rest, "@@") {
		return "", false
	}
	rest = strings.TrimPrefix(rest, "@@")
	// Single variable only: reject multi-column / AS / expressions / calls.
	// The whitespace set matches sqlWhitespace used for the SELECT keyword check
	// above, \r included, so "SELECT @@a\r@@b" is rejected like its \n counterpart.
	if strings.ContainsAny(rest, ",()"+sqlWhitespace) {
		return "", false
	}
	if low := strings.ToLower(rest); strings.HasPrefix(low, "session.") {
		rest = rest[len("session."):]
	} else if strings.HasPrefix(low, "global.") {
		rest = rest[len("global."):]
	} else if strings.HasPrefix(low, "local.") {
		rest = rest[len("local."):]
	}
	// Trim the statement terminator plus ANY trailing whitespace, using the same
	// set as above: trimming only "; " left "\r" and "\t" attached to the returned
	// variable name, which then failed every lookup.
	rest = strings.TrimRight(rest, ";"+sqlWhitespace)
	if rest == "" {
		return "", false
	}
	return rest, true
}

// resolveVarFromProbe scans the mock pool for a recorded result set carrying a
// column whose label equals varName, returning the REAL recorded column
// definition, value, AND that result set's own terminator (so the caller can
// preserve the recorded server status flags rather than fabricating them).
//
// The connection-setup probe ("SELECT @@... AS <var>, ...") aliases every
// column to its bare variable name, so col.Name is normally the bare name; some
// probe forms omit the alias and leave the raw "@@<var>" label, so both are
// matched.
//
// Assumption: system variables read at connection setup (transaction_isolation,
// transaction_read_only, sql_mode, ...) are session-invariant, so the FIRST
// recorded result set carrying the column is authoritative. The pool is already
// ordered per-test, then session, then connection, so the most specific probe
// is consulted first. If a variable legitimately differed across connections
// this would return the first recorded value; that does not occur for the
// setup-probe variables this path serves.
func resolveVarFromProbe(pool []*models.Mock, varName string) (*mysql.ColumnDefinition41, mysql.ColumnEntry, *mysql.GenericResponse, bool) {
	for _, m := range pool {
		if m == nil {
			continue
		}
		for ri := range m.Spec.MySQLResponses {
			trs, ok := m.Spec.MySQLResponses[ri].PacketBundle.Message.(*mysql.TextResultSet)
			if !ok || len(trs.Rows) == 0 {
				continue
			}
			for ci, col := range trs.Columns {
				if col == nil || ci >= len(trs.Rows[0].Values) {
					continue
				}
				if strings.EqualFold(col.Name, varName) || strings.EqualFold(col.Name, "@@"+varName) {
					return col, trs.Rows[0].Values[ci], trs.FinalResponse, true
				}
			}
		}
	}
	return nil, mysql.ColumnEntry{}, nil, false
}

// fallbackOKReplacingEOFTerminator is the deprecate-EOF result-set terminator at
// sequence 4 used ONLY when the recorded probe carried no reusable terminator:
// 0xFE + affected_rows(0) + last_insert_id(0) +
// status_flags(SERVER_STATUS_AUTOCOMMIT=0x0002) + warnings(0).
var fallbackOKReplacingEOFTerminator = []byte{0x07, 0x00, 0x00, 0x04, 0xfe, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}

// terminatorForSingleColumn returns the result-set terminator to emit for the
// synthesized single-column response. It PREFERS the probe's own recorded
// OK-replacing-EOF terminator (preserving the real recorded status flags,
// warnings and any session-state trailer), only rewriting its sequence-ID byte
// to 4 for the single-column framing — the terminator's structure is
// independent of column count, so this is valid. It falls back to a synthesized
// autocommit terminator only when the probe carried none or it was not an
// OK-replacing-EOF (e.g. a legacy plain EOF), so the status is honest when we
// have it and deterministic when we don't.
func terminatorForSingleColumn(probe *mysql.GenericResponse) *mysql.GenericResponse {
	if probe != nil && mysqlutils.IsOKReplacingEOF(probe.Data) {
		d := make([]byte, len(probe.Data))
		copy(d, probe.Data)
		d[3] = 0x04 // sequence ID of the terminator in a 1-column result set
		return &mysql.GenericResponse{Type: probe.Type, Data: d}
	}
	d := make([]byte, len(fallbackOKReplacingEOFTerminator))
	copy(d, fallbackOKReplacingEOFTerminator)
	return &mysql.GenericResponse{Type: "OK", Data: d}
}

// buildSessionVarResponse builds a single-column text result set carrying the
// recorded value of varName (from resolveVarFromProbe). It reuses the probe's
// real column definition (correct type/charset) and value, framed with the
// sequence IDs of a 1-column result set (col-count=1, column=2, row=3,
// OK-terminator=4). The wire encoder recomputes packet lengths from the encoded
// bodies, so only the sequence IDs must be set here. Returns nil when varName
// isn't present in any recorded result set (caller falls through to its normal
// no-match handling).
func buildSessionVarResponse(logger *zap.Logger, pool []*models.Mock, varName string, decodeCtx *wire.DecodeContext) *mysql.Response {
	// The single-column framing built below is the CLIENT_DEPRECATE_EOF form
	// (no intermediate EOF after the column; an OK-replacing-EOF terminator at
	// sequence 4). Every modern JVM MySQL driver (Connector/J 8.x, MariaDB
	// Connector/J) negotiates deprecate-EOF, which is the only case this
	// proxyless-JSSE path targets. If a connection did NOT negotiate it, DON'T
	// synthesize — fall through to normal no-match handling — rather than emit a
	// mis-sequenced result set. This keeps the fix from being wrong on a
	// non-deprecate-EOF driver instead of silently corrupting its framing.
	if decodeCtx == nil || !decodeCtx.DeprecateEOF() {
		if logger != nil {
			logger.Debug("session-variable resolver skipped: connection did not negotiate CLIENT_DEPRECATE_EOF",
				zap.String("var", varName))
		}
		return nil
	}
	col, val, probeTerminator, found := resolveVarFromProbe(pool, varName)
	if !found || col == nil {
		return nil
	}
	c := *col // copy; we only overwrite header/name, keeping the probe's type/charset/length
	c.Header = mysql.Header{SequenceID: 2}
	c.Name = varName
	c.OrgName = varName
	trs := &mysql.TextResultSet{
		ColumnCount:     1,
		Columns:         []*mysql.ColumnDefinition41{&c},
		EOFAfterColumns: nil, // CLIENT_DEPRECATE_EOF (Connector/J 8.x): no intermediate EOF
		Rows: []*mysql.TextRow{{
			Header: mysql.Header{SequenceID: 3},
			Values: []mysql.ColumnEntry{val},
		}},
		// Result-set terminator at sequence 4. Reuses the probe's own recorded
		// OK-replacing-EOF terminator (real status flags/warnings) when present,
		// falling back to a synthesized autocommit terminator otherwise.
		FinalResponse: terminatorForSingleColumn(probeTerminator),
	}
	if logger != nil {
		logger.Debug("built single-column session-variable result set",
			zap.String("var", varName), zap.Any("value", val.Value))
	}
	return &mysql.Response{
		PacketBundle: mysql.PacketBundle{
			Header: &mysql.PacketInfo{
				// Frames the FIRST sub-packet (column-count = 1 byte) at seq 1.
				Header: &mysql.Header{PayloadLength: 1, SequenceID: 1},
				Type:   mysql.StatusToString(mysql.OK), // cosmetic; encoder dispatches on Message type
			},
			Message: trs,
		},
	}
}
