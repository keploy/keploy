package replayer

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	lru "github.com/hashicorp/golang-lru/v2"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"vitess.io/vitess/go/vt/sqlparser"
)

// mysqlNoiseAdapter is the MySQL implementation of schemanoise.Adapter, making
// the MySQL parser a client of the same shared schema-noise engine HTTP uses.
// MySQL has no single "request body": the drift-carrying content depends on the
// command packet, so both sides of every comparison are first serialized into a
// canonical JSON document by mysqlRequestBodyJSON and diffed with the shared
// JSON kernel. Field-path vocabulary per packet:
//
//	COM_QUERY               -> body.query.{template,literals.N,comments} [, body.parameters.N.* query attributes]
//	COM_STMT_PREPARE        -> body.query.{template,literals.N,comments}
//	COM_STMT_EXECUTE        -> body.parameters.N.{type,value,unsigned}
//	COM_STMT_SEND_LONG_DATA -> body.data (field-level body.data.* when the chunk is standalone JSON)
//
// SQL texts are literal-split (see queryBodyValue): inline static values
// resolve to per-position paths (body.query.literals.2) and a plain
// body.query entry still ignores the whole subtree. Unparseable SQL falls
// back to whole-text body.query.
//
// Packets whose payload cannot drift (PING, CLOSE, utility commands) or whose
// drift is neutralized elsewhere (handshake auth, statement ids) serialize to
// ok=false, which short-circuits the engine into a no-op for them.
//
// Like HTTP, MySQL does NOT use Engine.Learn on the match path: detected noise
// is merged onto a fresh copy in updateMock so the shared pooled mock is never
// mutated. SetLearnedNoise is implemented for interface completeness.
type mysqlNoiseAdapter struct {
	schemanoise.JSONDiffer
}

// RecordedBody returns the canonical JSON body of the mock's recorded request.
// Command-phase data mocks carry exactly one request; multi-request bundles
// (handshake config mocks) and non-drift-capable commands return ok=false.
func (mysqlNoiseAdapter) RecordedBody(m *models.Mock) ([]byte, bool) {
	if m == nil || len(m.Spec.MySQLRequests) != 1 {
		return nil, false
	}
	bundle := &m.Spec.MySQLRequests[0].PacketBundle
	return mysqlRequestBody(bundle, recordedStatementParams(bundle, nil, bundle.StoredQuery(), "", -1))
}

// recordedStatementParams is how many of a recorded COM_STMT_EXECUTE's
// parameters it is compared on: all of them when its statement (q: the query
// it recorded, mysql.MetaQuery, or its PREPARE's) or the live statement
// (liveQuery) reads its attributes (readsQueryAttributes), else as
// recordedAttributesFrom splits it against the live statement's own count
// (liveParams, -1 when not known), its statement being the live one's when q
// is liveQuery.
func recordedStatementParams(recorded *mysql.PacketBundle, live *mysql.StmtExecutePacket, q, liveQuery string, liveParams int) int {
	exec, _ := recorded.Message.(*mysql.StmtExecutePacket)
	if exec == nil {
		return -1
	}
	if readsQueryAttributes(q) || readsQueryAttributes(liveQuery) {
		return len(exec.Parameters)
	}
	e := sqlStatementIdentity(q)
	return recordedAttributesFrom(exec, live, e != "" && strings.EqualFold(e, sqlStatementIdentity(liveQuery)), liveParams)
}

// StoredNoise returns the noise learned on this mock (kind-agnostic
// MockSpec.ReqBodyNoise, same storage as every other parser).
func (mysqlNoiseAdapter) StoredNoise(m *models.Mock) map[string][]string {
	if m == nil {
		return nil
	}
	return m.Spec.ReqBodyNoise
}

// SetLearnedNoise writes merged noise back onto MockSpec.ReqBodyNoise. Unused
// on the MySQL match path (see updateMock's copy-on-learn); present for
// interface completeness.
func (mysqlNoiseAdapter) SetLearnedNoise(m *models.Mock, merged map[string][]string) {
	if m == nil {
		return
	}
	m.Spec.ReqBodyNoise = merged
}

// RecordedValueIsNoise excludes recorded values already covered by the mock's
// value-regex noise (Mock.Noise, e.g. enterprise-obfuscated secrets) so they
// are not re-flagged as schema noise — the same values paramValueEqual already
// waves through via NoiseChecker.IsNoisyValue.
func (mysqlNoiseAdapter) RecordedValueIsNoise(m *models.Mock) func(string) bool {
	if m == nil {
		return nil
	}
	nc := util.NewNoiseChecker(m.Noise)
	if nc == nil {
		return nil
	}
	return func(v string) bool { return nc.IsNoisy(v) }
}

// executeBodyNoise is the request-body noise a recorded COM_STMT_EXECUTE is
// compared with the live one under, for the schema-noise engine: noise, plus
// every parameter's type, unsigned flag and name unless both packets sent
// them (typesSentByBoth). Any other request keeps noise as it is.
func executeBodyNoise(noise map[string][]string, recorded *models.Mock, live *mysql.PacketBundle) map[string][]string {
	if recorded == nil || live == nil || len(recorded.Spec.MySQLRequests) != 1 {
		return noise
	}
	act, _ := live.Message.(*mysql.StmtExecutePacket)
	exp, _ := recorded.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket)
	if act == nil || exp == nil || typesSentByBoth(exp, act) {
		return noise
	}
	n := max(len(exp.Parameters), len(act.Parameters))
	out := make(map[string][]string, len(noise)+3*n)
	for k, v := range noise {
		out[k] = v
	}
	for i := 0; i < n; i++ {
		for _, field := range []string{"type", "unsigned", "name"} {
			// An empty list ignores the field whatever its value.
			out["parameters."+strconv.Itoa(i)+"."+field] = []string{}
		}
	}
	return out
}

// strictGate applies schemaNoiseStrict enforcement while matchCommand scans
// candidate mocks, and accumulates the diagnostics the mismatch report needs.
// Consult allows() before a mock may become a match candidate: under
// schemaNoiseStrict every candidate's recorded request body is value-compared
// against the live one and rejected when a field OUTSIDE the known-noise set
// (user's requestBody noise ∪ the mock's learned req_body_noise) drifted.
// Without strict (or with no diffable live body) it always allows, preserving
// the lenient score/FIFO behaviour so the auto-replay detection path can
// still learn.
type strictGate struct {
	engine        *schemanoise.Engine
	logger        *zap.Logger
	requestType   string
	live          *mysql.PacketBundle
	liveBody      []byte
	liveBodyOK    bool
	userBodyNoise map[string][]string

	// liveQuery and liveParams are a live COM_STMT_EXECUTE's statement and
	// its own parameter count (-1 not known): a recording of that statement
	// is compared on as many of its parameters (recordedStatementParams).
	// queries are the candidates' statements as the matcher resolved them
	// (their recorded query, or their PREPARE's in the pool).
	liveQuery  string
	liveParams int
	queries    map[*models.Mock]string

	// Rejection diagnostics for the mismatch report:
	//   rejected    — how many candidates strict enforcement ruled out.
	//   closestMock — the FIRST strict-rejected candidate; it passed the
	//                 lenient match, so it is the closest mock to name in
	//                 the report.
	//   fieldDiffs  — field-level drift vs that first candidate
	//                 (noise-vocabulary "body."-paths with recorded vs live
	//                 values via matcher.JSONFieldDiffs) so the report says
	//                 exactly WHICH request fields drifted outside noise.
	rejected    int
	closestMock string
	fieldDiffs  []models.MockFieldDiff

	// verdicts keeps StrictReject's answer per candidate. The verdict depends
	// only on the candidate and the live body, both fixed for the command, so a
	// candidate the scan meets again (another of its packets, or the full scan
	// after the in-window pass) is not diffed or logged a second time.
	verdicts map[*models.Mock]gateVerdict

	// legacyBody is the live body as an older keploy read the live packet
	// (mysql.StmtExecutePacket's LegacyParameters, mysql.QueryPacket's
	// LegacyQuery), compared with what such a keploy recorded
	// (legacyRecordedMock); read once, when first needed.
	legacyBody                   []byte
	legacyBodyOK, legacyBodyRead bool
}

// bodyFor is the live request body mock's recorded one is compared with: the
// live packet as an older keploy read it when that keploy recorded mock
// (legacyRecordedMock), else as this one reads it.
func (g *strictGate) bodyFor(mock *models.Mock) ([]byte, bool) {
	if !legacyRecordedMock(mock) {
		return g.liveBody, g.liveBodyOK
	}
	if !g.legacyBodyRead {
		g.legacyBodyRead = true
		if g.live != nil {
			switch act := g.live.Message.(type) {
			case *mysql.StmtExecutePacket:
				if act != nil && act.LegacyParameters != nil {
					read := *act
					read.Parameters = act.LegacyParameters
					bundle := *g.live
					bundle.Message = &read
					g.legacyBody, g.legacyBodyOK = mysqlRequestBodyJSON(&bundle)
				}
			case *mysql.QueryPacket:
				if readsLegacy(act) {
					read := *act
					read.Query, read.Parameters = act.LegacyQuery, act.LegacyParameters
					bundle := *g.live
					bundle.Message = &read
					g.legacyBody, g.legacyBodyOK = mysqlRequestBodyJSON(&bundle)
				}
			}
		}
	}
	if !g.legacyBodyOK {
		return g.liveBody, g.liveBodyOK
	}
	return g.legacyBody, true
}

type gateVerdict struct {
	allowed bool
	drift   map[string][]string
}

func newStrictGate(engine *schemanoise.Engine, logger *zap.Logger, requestType string, live *mysql.PacketBundle, liveBody []byte, liveBodyOK bool, userBodyNoise map[string][]string) *strictGate {
	return &strictGate{
		engine:        engine,
		logger:        logger,
		requestType:   requestType,
		live:          live,
		liveBody:      liveBody,
		liveBodyOK:    liveBodyOK,
		userBodyNoise: userBodyNoise,
		liveParams:    -1,
	}
}

// recordedQuery tells the gate the statement the matcher resolved for a
// candidate COM_STMT_EXECUTE (empty: none), before it asks allows.
func (g *strictGate) recordedQuery(mock *models.Mock, q string) {
	if q == "" {
		return
	}
	if g.queries == nil {
		g.queries = make(map[*models.Mock]string)
	}
	g.queries[mock] = q
}

// recordedBody is mock's recorded request body as compared with the live one
// (recordedStatementParams).
func (g *strictGate) recordedBody(mock *models.Mock) ([]byte, bool) {
	if mock == nil || len(mock.Spec.MySQLRequests) != 1 {
		return nil, false
	}
	bundle := &mock.Spec.MySQLRequests[0].PacketBundle
	q := bundle.StoredQuery()
	if q == "" {
		q = g.queries[mock]
	}
	var live *mysql.StmtExecutePacket
	if g.live != nil {
		live, _ = g.live.Message.(*mysql.StmtExecutePacket)
	}
	return mysqlRequestBody(bundle, recordedStatementParams(bundle, live, q, g.liveQuery, g.liveParams))
}

// allows reports whether the candidate mock survives strict enforcement,
// recording rejection diagnostics for the mismatch report when it doesn't.
func (g *strictGate) allows(mock *models.Mock) bool {
	if !g.engine.StrictEnabled() || !g.liveBodyOK {
		return true
	}
	noise := executeBodyNoise(g.userBodyNoise, mock, g.live)
	body, _ := g.bodyFor(mock)
	v, seen := g.verdicts[mock]
	recorded, recordedOK := g.recordedBody(mock)
	if !seen {
		v.allowed, v.drift = g.engine.StrictRejectRecorded(mock, recorded, recordedOK, body, noise)
		if g.verdicts == nil {
			g.verdicts = make(map[*models.Mock]gateVerdict)
		}
		g.verdicts[mock] = v
	}
	if v.allowed {
		return true
	}
	g.rejected++
	if g.closestMock == "" {
		g.closestMock = mock.Name
		if recordedOK {
			// KnownNoise is root-relative (user ∪ learned); JSONFieldDiffs
			// skips those paths so the report shows only non-noise drift.
			known := g.engine.KnownNoise(mock, noise)
			g.fieldDiffs = maskAttributeDiffs(matcher.JSONFieldDiffs(string(recorded), string(body), known, "body.", 96), mock, g.live)
		}
	}
	if !seen {
		paths := make([]string, 0, len(v.drift))
		for p := range v.drift {
			paths = append(paths, p)
		}
		g.logger.Debug("schema-noise strict: rejected candidate mock (non-noise request field drifted)",
			zap.String("mock", mock.Name),
			zap.String("request_type", g.requestType),
			zap.Strings("drifted_fields", paths))
	}
	return false
}

// resetDiagnostics clears the rejection diagnostics for a scan that starts over
// on the same command, keeping the verdicts: they still hold.
func (g *strictGate) resetDiagnostics() {
	g.rejected, g.closestMock, g.fieldDiffs = 0, "", nil
}

// queryShape is the literal-split view of a SQL text: the query with every
// inline literal replaced by a positional placeholder (template), the
// extracted literal values in syntactic order, and any statement comments
// pulled out of the template. ok=false means vitess could not parse the SQL
// and callers must fall back to whole-text comparison.
type queryShape struct {
	template string
	literals []string
	comments string
	ok       bool
}

// queryShapeCacheSize bounds queryShapeCache. Same reasoning as match.go's
// querySigCacheSize: the key is the raw SQL text (unbounded once a client
// inlines literals) while the value is a pure memo of a deterministic parse,
// so an eviction costs one re-parse and can never change a match verdict. Kept
// equal to querySigCacheSize because both caches are fed from exactly the same
// SQL texts on the same code paths.
const queryShapeCacheSize = querySigCacheSize

// queryShapeCache memoizes extractQueryShape per SQL text — the same query is
// re-canonicalized for every candidate mock during a pool scan, and mock-side
// texts repeat across commands. Sibling of match.go's querySigCache, 2Q for
// the same scan-resistance reason documented on newQuerySigCache.
var queryShapeCache = newQueryShapeCache()

func newQueryShapeCache() *lru.TwoQueueCache[string, *queryShape] {
	c, err := lru.New2Q[string, *queryShape](queryShapeCacheSize)
	if err != nil {
		// Unreachable: New2Q only errors on a non-positive size. Fail loudly
		// rather than leave a nil cache that panics on the first lookup.
		panic(fmt.Sprintf("mysql replayer: invalid queryShapeCacheSize %d: %v", queryShapeCacheSize, err))
	}
	return c
}

// extractQueryShape parses sql with the vitess parser (the one the matcher
// already uses for AST-structure signatures) and splits it into template +
// literals + comments — the MySQL equivalent of the "give it a query, get its
// fields" helper the Postgres integration uses. Results are memoized.
func extractQueryShape(sql string) *queryShape {
	if v, ok := queryShapeCache.Get(sql); ok {
		return v
	}
	shape := computeQueryShape(sql)
	queryShapeCache.Add(sql, shape)
	return shape
}

func computeQueryShape(sql string) (shape *queryShape) {
	// The rewriter panics on AST slots it cannot replace; treat any such SQL
	// as unparseable so the caller falls back to whole-text comparison
	// instead of taking the proxy down.
	shape = &queryShape{}
	defer func() {
		if r := recover(); r != nil {
			shape = &queryShape{}
		}
	}()

	parser, err := sqlparser.New(sqlparser.Options{})
	if err != nil {
		return shape
	}
	stmt, err := parser.Parse(sql)
	if err != nil {
		return shape
	}

	// Pull statement comments out of the template so a per-request trace
	// comment (/* traceparent=... */) drifts on its own field instead of
	// polluting the whole template.
	var comments []string
	if c, isCommented := stmt.(sqlparser.Commented); isCommented {
		if pc := c.GetParsedComments(); pc != nil {
			comments = append(comments, pc.GetComments()...)
		}
		c.SetComments(nil)
	}

	// Replace every inline literal with a positional argument, collecting the
	// values in walk order. Walk order is syntactic, so two queries that
	// already passed the matcher's AST-structure gate enumerate their
	// literals at identical positions. The replacement name is prefixed
	// "kp_lit_" so it can NEVER collide with vitess's own `?`-placeholder
	// arguments (named v1, v2, …) — without the distinct prefix,
	// `WHERE a = ? AND b = 5` and `WHERE a = 5 AND b = ?` would both render
	// as `a = :v1 and b = :v1` and false-match as the same template.
	var literals []string
	out := sqlparser.Rewrite(stmt, func(cursor *sqlparser.Cursor) bool {
		if lit, isLit := cursor.Node().(*sqlparser.Literal); isLit {
			literals = append(literals, lit.Val)
			cursor.Replace(sqlparser.NewArgument("kp_lit_" + strconv.Itoa(len(literals))))
		}
		return true
	}, nil)

	return &queryShape{
		template: sqlparser.String(out),
		literals: literals,
		comments: strings.TrimSpace(strings.Join(comments, " ")),
		ok:       true,
	}
}

// queryBodyValue returns the canonical JSON value for a SQL text. Parseable
// SQL becomes a shape object — template, index-keyed literals, comments — so
// drift resolves to per-literal noise paths (body.query.literals.2) instead
// of the whole text; a learned/user "body.query" entry still covers the whole
// subtree (the JSON differ's noise index ignores prefixed children), so
// whole-query noise keeps working. Unparseable SQL stays a plain string,
// where any drift is the single body.query field.
func queryBodyValue(sql string) any {
	shape := extractQueryShape(sql)
	if !shape.ok {
		return sql
	}
	q := map[string]any{"template": shape.template}
	if len(shape.literals) > 0 {
		lits := make(map[string]string, len(shape.literals))
		for i, v := range shape.literals {
			lits[strconv.Itoa(i)] = v
		}
		q["literals"] = lits
	}
	if shape.comments != "" {
		q["comments"] = shape.comments
	}
	return q
}

// maskAttributeDiffs masks, in a mismatch report's field diffs, the recorded
// and live values of a query attribute (a parameter sent with a name on
// either side): its name, not its value's shape, may make it a secret
// (executeForLog).
func maskAttributeDiffs(diffs []models.MockFieldDiff, mock *models.Mock, live *mysql.PacketBundle) []models.MockFieldDiff {
	named := func(b *mysql.PacketBundle, i int) bool {
		if b == nil {
			return false
		}
		var ps []mysql.Parameter
		switch m := b.Message.(type) {
		case *mysql.StmtExecutePacket:
			if m != nil {
				ps = m.Parameters
			}
		case *mysql.QueryPacket:
			if m != nil {
				ps = m.Parameters
			}
		}
		return i < len(ps) && ps[i].Name != ""
	}
	var recorded *mysql.PacketBundle
	if mock != nil && len(mock.Spec.MySQLRequests) == 1 {
		recorded = &mock.Spec.MySQLRequests[0].PacketBundle
	}
	for i := range diffs {
		rest, ok := strings.CutPrefix(diffs[i].Path, "body.parameters.")
		if !ok {
			continue
		}
		idx, field, _ := strings.Cut(rest, ".")
		n, err := strconv.Atoi(idx)
		if err != nil || field == "name" || field == "type" || field == "unsigned" {
			// Everything else under the parameter is its value, an
			// enveloped one ($bin, $ts) included.
			continue
		}
		if named(live, n) || named(recorded, n) {
			// The side the field is absent from stays empty.
			if diffs[i].Kind != models.DiffKindMissingInMock {
				diffs[i].Expected = maskedAttributeValue
			}
			if diffs[i].Kind != models.DiffKindMissingInLive {
				diffs[i].Actual = maskedAttributeValue
			}
		}
	}
	return diffs
}

// mysqlRequestBodyJSON serializes the drift-carrying content of a command
// packet into a canonical JSON document for the schema-noise engine. ok=false
// means the packet has no drift-capable body (utility commands, statement-id
// only packets, handshake elements) and the engine must no-op.
//
// Parameters are keyed by index as an OBJECT ("0", "1", …), not an array: the
// JSON differ collapses array elements to a single "[]" path, which would
// merge all params into one bucket — an object keeps per-position paths
// (parameters.0.value) so noise on the uuid param never excuses drift on the
// customer param. Parameter's custom MarshalJSON preserves value types
// ($bin/$ts envelopes), so a type flip surfaces as drift, not a false match.
//
// Deliberately EXCLUDED fields: StatementID (unstable across runs, remapped
// query-aware by the matcher), Status/Flags/IterationCount/NullBitmap
// (protocol plumbing already scored by matchStmtExecutePacketQueryAware, not
// user data), and a COM_QUERY's or COM_STMT_EXECUTE's query attributes
// (executeAttributesFrom) unless the statement reads one
// (readsQueryAttributes): an EXECUTE's packet does not name its statement, so
// matchCommand and strictGate.recordedBody give the count of its parameters
// to keep (mysqlRequestBody, recordedStatementParams).
func mysqlRequestBodyJSON(bundle *mysql.PacketBundle) ([]byte, bool) {
	return mysqlRequestBody(bundle, -1)
}

// mysqlRequestBody is mysqlRequestBodyJSON with a COM_STMT_EXECUTE's
// statement's own parameter count when known (a live packet's), -1 when not
// (executeAttributesFrom).
func mysqlRequestBody(bundle *mysql.PacketBundle, statementParams int) ([]byte, bool) {
	if bundle == nil || bundle.Header == nil {
		return nil, false
	}
	var doc map[string]any
	switch msg := bundle.Message.(type) {
	case *mysql.QueryPacket:
		if msg == nil {
			return nil, false
		}
		doc = map[string]any{"query": queryBodyValue(msg.Query)}
		if len(msg.Parameters) > 0 && readsQueryAttributes(msg.Query) {
			doc["parameters"] = parametersByIndex(msg.Parameters)
		}
	case *mysql.StmtPreparePacket:
		if msg == nil {
			return nil, false
		}
		doc = map[string]any{"query": queryBodyValue(msg.Query)}
	case *mysql.StmtExecutePacket:
		if msg == nil {
			return nil, false
		}
		doc = map[string]any{"parameters": parametersByIndex(executeOwnParameters(msg, statementParams))}
	case *mysql.StmtSendLongDataPacket:
		if msg == nil {
			return nil, false
		}
		// A standalone-JSON chunk is embedded raw so drift resolves to
		// field-level paths (body.data.updated_at); anything else (binary,
		// or a JSON document split across chunks) becomes a base64 string,
		// where any drift is the whole body.data field.
		if json.Valid(msg.Data) {
			doc = map[string]any{"data": json.RawMessage(msg.Data)}
		} else {
			doc = map[string]any{"data": base64.StdEncoding.EncodeToString(msg.Data)}
		}
	default:
		return nil, false
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, false
	}
	return out, true
}

// readsQueryAttributes reports whether a COM_QUERY's statement reads its query
// attributes: the server answers with them only through
// mysql_query_attribute_string(). Any other statement's answer is the same
// whatever attributes it carries, and a client sends ones that differ on
// every call: Connector/J 8.1+ sends the caller's OpenTelemetry trace
// (traceparent) with every statement. So they are compared, and drift as a
// request field, only for a statement that reads one; for any other they
// would make every call drift, and be learned as noise. A statement that
// reads one without naming the function is not seen: a stored procedure
// (CALL) or function (SELECT f()), a view, a trigger, or EXECUTE of a
// statement prepared from a variable.
func readsQueryAttributes(query string) bool {
	for i := 0; i < len(query); i++ {
		if query[i] >= utf8.RuneSelf {
			// A rune outside ASCII may lower-case to an ASCII letter (the
			// Kelvin sign to 'k'): read it as strings.ToLower does.
			return strings.Contains(strings.ToLower(query), queryAttributeFunction)
		}
	}
	// ASCII: strings.ToLower lower-cases A-Z only. Compared in place, so the
	// matcher, which asks for every candidate, allocates nothing.
	n := len(queryAttributeFunction)
	for i := 0; i+n <= len(query); i++ {
		j := 0
		for ; j < n; j++ {
			c := query[i+j]
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != queryAttributeFunction[j] {
				break
			}
		}
		if j == n {
			return true
		}
	}
	return false
}

// queryAttributeFunction is the function that reads a query attribute, lower
// case (readsQueryAttributes).
const queryAttributeFunction = "mysql_query_attribute_string"

// parametersByIndex converts a parameter slice to an index-keyed map (see
// mysqlRequestBodyJSON for why an object beats an array here).
func parametersByIndex(params []mysql.Parameter) map[string]mysql.Parameter {
	out := make(map[string]mysql.Parameter, len(params))
	for i, p := range params {
		out[strconv.Itoa(i)] = p
	}
	return out
}
