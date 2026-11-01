package replayer

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	lru "github.com/hashicorp/golang-lru/v2"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	// vitesslog must load before any vitess code logs: see that package.
	_ "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/vitesslog"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"vitess.io/vitess/go/vt/sqlparser"
)

// mysqlNoiseAdapter is the MySQL implementation of mocknoise.Adapter, making
// the MySQL parser a client of the same shared schema-noise engine HTTP uses.
// MySQL has no single "request body": the drift-carrying content depends on the
// command packet, so both sides of every comparison are first serialized into a
// canonical JSON document by mysqlRequestBodyJSON and diffed with the shared
// JSON kernel. Field-path vocabulary per packet:
//
//	COM_QUERY               -> body.query.{template,literals.N,comments} [, body.attributes.<name>.* query attributes]
//	COM_STMT_PREPARE        -> body.query.{template,literals.N,comments}
//	COM_STMT_EXECUTE        -> body.parameters.N.{type,value,unsigned} [, body.attributes.<name>.*]
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
	mocknoise.JSONDiffer
}

// RecordedBody returns the canonical JSON body of the mock's recorded request.
// Command-phase data mocks carry exactly one request; multi-request bundles
// (handshake config mocks) and non-drift-capable commands return ok=false.
func (mysqlNoiseAdapter) RecordedBody(m *models.Mock) ([]byte, bool) {
	if m == nil || len(m.Spec.MySQLRequests) != 1 {
		return nil, false
	}
	bundle := &m.Spec.MySQLRequests[0].PacketBundle
	return mysqlRequestBody(bundle, recordedStatementParams(bundle, nil, bundle.StoredQuery(), "", -1), "")
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
	engine        *mocknoise.Engine
	logger        *zap.Logger
	requestType   string
	live          *mysql.PacketBundle
	userBodyNoise map[string][]string

	// liveBody is the live command's canonical JSON body
	// (mysqlRequestBody, with liveParams and liveQuery), liveBodyOK false
	// for a command with no drift-capable body (utility commands,
	// CLOSE/RESET). Only strict enforcement, the rule of exact serving and
	// detection read it, so it is built on first read (liveRequestBody),
	// never for a replay that runs none of them. bodiesBuilt, when set,
	// counts the builds, so a test can check what a command costs without
	// timing it.
	liveBody                 []byte
	liveBodyOK, liveBodyRead bool
	bodiesBuilt              *int

	// compared, when set, counts the candidates compare compared with the
	// live body (its verdicts, each read once), as bodiesBuilt counts the
	// builds.
	compared *int

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

	// exactRejected is how many of rejected the rule of exact serving ruled
	// out (allowsDrift without strict enforcement, allowsLiterals):
	// recordings of the command's statement whose parameters, or inline
	// literals, drifted outside known noise.
	exactRejected int

	// driftEngine compares a candidate for allowsDrift and allowsLiterals as
	// strict enforcement does, when the engine the gate was given does not
	// (driftVerdicts and literalVerdicts keep their answers, as verdicts
	// keeps the engine's).
	driftEngine     *mocknoise.Engine
	driftVerdicts   map[*models.Mock]gateVerdict
	literalVerdicts map[*models.Mock]gateVerdict

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
		return g.liveRequestBody()
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
		return g.liveRequestBody()
	}
	return g.legacyBody, true
}

// liveRequestBody is the live command's body (liveBody), built on its first
// read.
func (g *strictGate) liveRequestBody() ([]byte, bool) {
	if !g.liveBodyRead {
		g.liveBodyRead = true
		g.liveBody, g.liveBodyOK = mysqlRequestBody(g.live, g.liveParams, g.liveQuery)
		if g.bodiesBuilt != nil {
			*g.bodiesBuilt++
		}
	}
	return g.liveBody, g.liveBodyOK
}

// hasLiveBody reports whether the live command has a drift-capable body.
func (g *strictGate) hasLiveBody() bool {
	_, ok := g.liveRequestBody()
	return ok
}

type gateVerdict struct {
	allowed bool
	drift   map[string][]string
}

// newStrictGate is the gate for the live command live. liveParams and
// liveQuery are a live COM_STMT_EXECUTE's own parameter count (-1 not known)
// and statement ("" not known), as mysqlRequestBody reads them.
func newStrictGate(engine *mocknoise.Engine, logger *zap.Logger, requestType string, live *mysql.PacketBundle, liveParams int, liveQuery string, userBodyNoise map[string][]string) *strictGate {
	return &strictGate{
		engine:        engine,
		logger:        logger,
		requestType:   requestType,
		live:          live,
		userBodyNoise: userBodyNoise,
		liveQuery:     liveQuery,
		liveParams:    liveParams,
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
	// A recording of the live statement, when that reads its query
	// attributes, holds the ones it reads by name, as the live body does
	// (executeBodyParameters): split by the live statement's count.
	if e := sqlStatementIdentity(q); live != nil && g.liveParams >= 0 && readsQueryAttributes(g.liveQuery) && e != "" && strings.EqualFold(e, sqlStatementIdentity(g.liveQuery)) {
		return mysqlRequestBody(bundle, g.liveParams, g.liveQuery)
	}
	return mysqlRequestBody(bundle, recordedStatementParams(bundle, live, q, g.liveQuery, g.liveParams), "")
}

// allows reports whether the candidate mock survives strict enforcement,
// recording rejection diagnostics for the mismatch report when it doesn't.
func (g *strictGate) allows(mock *models.Mock) bool {
	if !g.engine.StrictEnabled() || !g.hasLiveBody() {
		return true
	}
	return g.compare(g.engine, &g.verdicts, mock)
}

// allowsDrift is the rule, under exact serving, for a recording of a live
// COM_STMT_EXECUTE's statement whose parameters are not all equal (the FIFO
// candidate): it answers only when every parameter that differs is noise,
// configured (test.globalNoise.requestBody) or learned on the recording
// (req_body_noise), compared as strict enforcement compares, whether or
// not strict enforcement is on. A MySQL recording that declares variables
// never reaches the matcher there (the agent's followable: the MySQL parser
// follows none). Served with another parameter, the recording answered the
// command with the rows of another call: a read of order 5 got order 7's.
// A command with no diffable body cannot show its drift is noise: no.
func (g *strictGate) allowsDrift(mock *models.Mock) bool {
	if g.engine.StrictEnabled() {
		return g.allows(mock)
	}
	if !g.hasLiveBody() {
		return false
	}
	if g.compare(g.strictEngine(), &g.driftVerdicts, mock) {
		return true
	}
	g.exactRejected++
	return false
}

// literalsLeftOut are the paths of a COM_QUERY's body allowsLiterals does
// not compare: the statement's comments (computeQueryShape keeps a comment
// after the statement's keyword there; the parser drops the others).
var literalsLeftOut = []string{"query.comments"}

// allowsLiterals is the rule, under exact serving, for a recording of a live
// COM_QUERY's statement made with other inline literals (matchQueryLive's
// literal tiers: the same text once its literals are masked): it answers
// only when every literal that differs, compared by position
// (body.query.literals.N), is noise, configured
// (test.globalNoise.requestBody) or learned on the recording
// (req_body_noise), compared as strict enforcement compares, whether or not
// strict enforcement is on. A COM_QUERY's literals are its parameters:
// served with another one, the recording answered the command with the rows
// of another call (a read of id 5 got id 7's). So are the query attributes
// the statement reads, by name (readAttributes: only those are in its body,
// whatever others the client sends with it).
// The statement's comments are left out (a traceparent changes on every
// run). A statement that does not parse is compared whole (body.query).
func (g *strictGate) allowsLiterals(mock *models.Mock) bool {
	if !g.hasLiveBody() {
		return false
	}
	if g.compare(g.strictEngine(), &g.literalVerdicts, mock, literalsLeftOut...) {
		return true
	}
	g.exactRejected++
	return false
}

// strictEngine is the engine allowsDrift and allowsLiterals compare with:
// one that compares as strict enforcement does, made on first use.
func (g *strictGate) strictEngine() *mocknoise.Engine {
	if g.driftEngine == nil {
		g.driftEngine = mocknoise.New(mysqlNoiseAdapter{}, false, true)
	}
	return g.driftEngine
}

// compare is strict enforcement's verdict on mock by engine, cached in
// verdicts, recording the rejection diagnostics when it is no. The paths in
// leftOut are not compared, whatever their values.
func (g *strictGate) compare(engine *mocknoise.Engine, verdicts *map[*models.Mock]gateVerdict, mock *models.Mock, leftOut ...string) bool {
	v, seen := (*verdicts)[mock]
	if seen && v.allowed {
		return true
	}
	noise := executeBodyNoise(g.userBodyNoise, mock, g.live)
	if len(leftOut) > 0 {
		out := make(map[string][]string, len(noise)+len(leftOut))
		for k, v := range noise {
			out[k] = v
		}
		for _, p := range leftOut {
			// An empty list ignores the field whatever its value.
			out[p] = []string{}
		}
		noise = out
	}
	body, _ := g.bodyFor(mock)
	if !seen {
		recorded, recordedOK := g.recordedBody(mock)
		v.allowed, v.drift = engine.StrictRejectRecorded(mock, recorded, recordedOK, body, noise)
		if g.compared != nil {
			*g.compared++
		}
		if *verdicts == nil {
			*verdicts = make(map[*models.Mock]gateVerdict)
		}
		(*verdicts)[mock] = v
	}
	if v.allowed {
		return true
	}
	g.rejected++
	if g.closestMock == "" {
		g.closestMock = mock.Name
		if recorded, recordedOK := g.recordedBody(mock); recordedOK {
			// KnownNoise is root-relative (user ∪ learned); JSONFieldDiffs
			// skips those paths so the report shows only non-noise drift.
			known := engine.KnownNoise(mock, noise)
			g.fieldDiffs = maskAttributeDiffs(matcher.JSONFieldDiffs(string(recorded), string(body), known, "body.", 96), mock, g.live)
		}
	}
	if ce := g.logger.Check(zap.DebugLevel, "schema-noise strict: rejected candidate mock (non-noise request field drifted)"); ce != nil && !seen {
		paths := make([]string, 0, len(v.drift))
		for p := range v.drift {
			paths = append(paths, p)
		}
		ce.Write(zap.String("mock", mock.Name),
			zap.String("request_type", g.requestType),
			zap.Strings("drifted_fields", paths))
	}
	return false
}

// resetDiagnostics clears the rejection diagnostics for a scan that starts over
// on the same command, keeping the verdicts: they still hold.
func (g *strictGate) resetDiagnostics() {
	g.rejected, g.closestMock, g.fieldDiffs, g.exactRejected = 0, "", nil, 0
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
		var attribute bool
		if p := diffs[i].Path; p == "body.attributes" || strings.HasPrefix(p, "body.attributes.") {
			// Every one is an attribute (readAttributes), keyed by its
			// name, which may hold dots: all but a change of its type are
			// masked (one side without it holds it whole, its value too).
			field := p[strings.LastIndexByte(p, '.')+1:]
			attribute = diffs[i].Kind != models.DiffKindValueChanged || (field != "type" && field != "unsigned")
		} else if rest, ok := strings.CutPrefix(diffs[i].Path, "body.parameters."); ok {
			idx, field, _ := strings.Cut(rest, ".")
			n, err := strconv.Atoi(idx)
			// Everything else under the parameter is its value, an
			// enveloped one ($bin, $ts) included.
			attribute = err == nil && field != "name" && field != "type" && field != "unsigned" && (named(live, n) || named(recorded, n))
		}
		if attribute {
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
// (readsQueryAttributes), and then only the ones it reads, by name
// (readAttributes): an EXECUTE's packet does not name its statement, so
// matchCommand and strictGate.recordedBody give the count of its parameters
// to keep, and its statement (mysqlRequestBody, recordedStatementParams).
func mysqlRequestBodyJSON(bundle *mysql.PacketBundle) ([]byte, bool) {
	return mysqlRequestBody(bundle, -1, "")
}

// mysqlRequestBody is mysqlRequestBodyJSON with a COM_STMT_EXECUTE's
// statement's own parameter count when known (a live packet's), -1 when not
// (executeAttributesFrom), and its statement (query, empty when not known):
// when that reads its attributes and the count is known, the body holds
// the statement's own parameters, by position, and the attributes it
// reads, by name (body.attributes, executeBodyParameters).
func mysqlRequestBody(bundle *mysql.PacketBundle, statementParams int, query string) ([]byte, bool) {
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
		if readsQueryAttributes(msg.Query) {
			// A COM_QUERY's parameters are all query attributes.
			if names, ok := attributesRead(msg.Query); ok {
				doc["attributes"] = readAttributes(msg.Parameters, names)
			} else if len(msg.Parameters) > 0 {
				doc["parameters"] = parametersByIndex(msg.Parameters)
			}
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
		params, attrs := executeBodyParameters(msg, statementParams, query)
		doc = map[string]any{"parameters": parametersByIndex(params)}
		if attrs != nil {
			doc["attributes"] = attrs
		}
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
			return strings.Contains(strings.ToLower(query), attributeFunction)
		}
	}
	// ASCII: strings.ToLower lower-cases A-Z only. Compared in place, so the
	// matcher, which asks for every candidate, allocates nothing.
	n := len(attributeFunction)
	for i := 0; i+n <= len(query); i++ {
		j := 0
		for ; j < n; j++ {
			c := query[i+j]
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != attributeFunction[j] {
				break
			}
		}
		if j == n {
			return true
		}
	}
	return false
}

const attributeFunction = "mysql_query_attribute_string"

// attributeCall is a call of mysql_query_attribute_string with a string
// literal for its argument: the name of the attribute it reads.
var attributeCall = regexp.MustCompile(`(?i)mysql_query_attribute_string\s*\(\s*(?:'((?:[^'\\]|\\.|'')*)'|"((?:[^"\\]|\\.|"")*)")\s*\)`)

// attributesRead is the names of the query attributes a statement reads
// (readsQueryAttributes), in the order it first names them. ok is false when
// it reads one whose name it does not spell as a string literal
// (mysql_query_attribute_string(@name)): then which it reads is not known,
// and every attribute sent with it is compared.
func attributesRead(query string) (names []string, ok bool) {
	calls := attributeCall.FindAllStringSubmatchIndex(query, -1)
	if len(calls) != strings.Count(strings.ToLower(query), attributeFunction) {
		return nil, false
	}
	seen := make(map[string]bool, len(calls))
	for _, c := range calls {
		// c[2:4] is a single-quoted name, c[4:6] a double-quoted one.
		var name string
		if c[2] >= 0 {
			name = unquoteSQLString(query[c[2]:c[3]], "'")
		} else {
			name = unquoteSQLString(query[c[4]:c[5]], `"`)
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names, true
}

// unquoteSQLString is the value of a SQL string literal's body s, quoted
// with quote: a doubled quote and a backslash escape stand for the
// character.
func unquoteSQLString(s, quote string) string {
	if !strings.ContainsAny(s, `\`+quote) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s):
			i++
		case s[i] == quote[0] && i+1 < len(s) && s[i+1] == quote[0]:
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// readAttributes is, of the query attributes a client sent (attrs), the
// ones a statement reads (names, attributesRead), keyed by name
// (body.attributes.<name>.value, the name in lower case). MySQL looks a
// name up in the connection's collation, and the first attribute sent with
// it wins. Under its default collations (utf8mb4_0900_ai_ci,
// latin1_swedish_ci; checked on 8.4) that is case-insensitive: a statement
// reading 'tenant' reads one sent as 'Tenant' or 'TENANT', and under
// utf8mb4 one sent as 'ténant'. So for each name it reads, the first one
// sent whose name is the same but for case, or, when none is, a NULL
// (mysql_query_attribute_string reads NULL then); and, under its own name,
// each one sent whose name keploy compares whatever the statement reads
// (foldableAttributeName). Compared with those, an attribute the statement
// does not read can make a call miss, never another tenant's rows answer
// it. The others are left out: a client sends ones that differ on every
// call (Connector/J 8.1+, the caller's OpenTelemetry traceparent), and
// compared, a statement that reads only 'tenant' never matched its
// recording. Keyed by name, the order the client sends them in moves no
// field, and noise written on the layout older versions compared
// (body.parameters.N: every attribute sent, by position, a traceparent sent
// first at 0) unchecks none of them.
//
// A stated limit: under a case-sensitive connection collation (_bin, _cs)
// MySQL reads only the attribute sent with the exact name, while this
// takes the first one sent whose name is the same but for case. So a call
// that sends two case-variants of a read name (Tenant=a, tenant=b), or
// spells it in another case than its recording did, can be answered with
// another value's rows. When every call sends the name once, spelled the
// same, keploy is only stricter than MySQL.
func readAttributes(attrs []mysql.Parameter, names []string) map[string]mysql.Parameter {
	out := make(map[string]mysql.Parameter, len(names))
	for _, name := range names {
		key := strings.ToLower(name)
		if _, dup := out[key]; dup {
			continue
		}
		read := mysql.Parameter{Type: uint16(mysql.FieldTypeNULL)}
		for _, a := range attrs {
			if strings.EqualFold(a.Name, name) {
				read = a
				break
			}
		}
		// The key names it: sent as Tenant or tenant, it is the same read.
		read.Name = key
		out[key] = read
	}
	for _, a := range attrs {
		key := strings.ToLower(a.Name)
		if _, seen := out[key]; seen || !foldableAttributeName(a.Name) {
			continue
		}
		a.Name = key
		out[key] = a
	}
	return out
}

// foldableAttributeName reports whether keploy compares an attribute sent
// as name whatever names the statement reads: one with a byte outside
// printable ASCII, which a collation may make equal to a name it differs
// from in more than case (an accent, which utf8mb4_0900_ai_ci ignores; a
// control character, which the Unicode collations ignore), or one that
// ends in a space. MySQL 8.4's lookup does not ignore a trailing space
// (checked under latin1_swedish_ci); such a name is compared only to be
// safe, since comparing an attribute can only make a call miss.
func foldableAttributeName(name string) bool {
	if strings.HasSuffix(name, " ") {
		return true
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 || name[i] > 0x7e {
			return true
		}
	}
	return false
}

// executeBodyParameters is the parameters of a COM_STMT_EXECUTE (p) its body
// holds: its statement's own (executeOwnParameters), and when the statement
// (query) reads its query attributes, the ones it reads, by name (attrs,
// readAttributes). That needs where its own end, its statement's count
// (statementParams), and the names it reads (attributesRead), and a packet
// sent with its types (the one that sends their names); without them every
// one of its parameters is held, by position, and attrs is nil.
func executeBodyParameters(p *mysql.StmtExecutePacket, statementParams int, query string) (params []mysql.Parameter, attrs map[string]mysql.Parameter) {
	own := executeOwnParameters(p, statementParams)
	if !readsQueryAttributes(query) {
		return own, nil
	}
	names, ok := attributesRead(query)
	if !ok || statementParams < 0 || p.NewParamsBindFlag != 1 {
		return p.Parameters, nil
	}
	return own, readAttributes(p.Parameters[len(own):], names)
}

// parametersByIndex converts a parameter slice to an index-keyed map (see
// mysqlRequestBodyJSON for why an object beats an array here).
func parametersByIndex(params []mysql.Parameter) map[string]mysql.Parameter {
	out := make(map[string]mysql.Parameter, len(params))
	for i, p := range params {
		out[strconv.Itoa(i)] = p
	}
	return out
}
