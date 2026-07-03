package replayer

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"sync"

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
	return mysqlRequestBodyJSON(&m.Spec.MySQLRequests[0].PacketBundle)
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
	liveBody      []byte
	liveBodyOK    bool
	userBodyNoise map[string][]string

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
}

func newStrictGate(engine *schemanoise.Engine, logger *zap.Logger, requestType string, liveBody []byte, liveBodyOK bool, userBodyNoise map[string][]string) *strictGate {
	return &strictGate{
		engine:        engine,
		logger:        logger,
		requestType:   requestType,
		liveBody:      liveBody,
		liveBodyOK:    liveBodyOK,
		userBodyNoise: userBodyNoise,
	}
}

// allows reports whether the candidate mock survives strict enforcement,
// recording rejection diagnostics for the mismatch report when it doesn't.
func (g *strictGate) allows(mock *models.Mock) bool {
	if !g.engine.StrictEnabled() || !g.liveBodyOK {
		return true
	}
	allowed, drift := g.engine.StrictReject(mock, g.liveBody, g.userBodyNoise)
	if allowed {
		return true
	}
	g.rejected++
	if g.closestMock == "" {
		g.closestMock = mock.Name
		if recorded, ok := (mysqlNoiseAdapter{}).RecordedBody(mock); ok {
			// KnownNoise is root-relative (user ∪ learned); JSONFieldDiffs
			// skips those paths so the report shows only non-noise drift.
			known := g.engine.KnownNoise(mock, g.userBodyNoise)
			g.fieldDiffs = matcher.JSONFieldDiffs(string(recorded), string(g.liveBody), known, "body.", 96)
		}
	}
	paths := make([]string, 0, len(drift))
	for p := range drift {
		paths = append(paths, p)
	}
	g.logger.Debug("schema-noise strict: rejected candidate mock (non-noise request field drifted)",
		zap.String("mock", mock.Name),
		zap.String("request_type", g.requestType),
		zap.Strings("drifted_fields", paths))
	return false
}

// queryShape is the literal-split view of a SQL text: the query with every
// inline literal replaced by a positional placeholder (template), the
// extracted literal values in syntactic order, any statement comments pulled
// out of the template, and the count of REAL `?` bind placeholders the
// statement carried (parsed as arguments — a '?' inside a string literal is
// value content and is NOT counted, unlike a raw strings.Count). ok=false
// means vitess could not parse the SQL and callers must fall back to
// whole-text comparison.
type queryShape struct {
	template     string
	literals     []string
	comments     string
	placeholders int
	ok           bool
}

// queryShapeCache memoizes extractQueryShape per SQL text — the same query is
// re-canonicalized for every candidate mock during a pool scan, and mock-side
// texts repeat across commands. Sibling of match.go's querySigCache.
var queryShapeCache sync.Map // map[string]*queryShape

// extractQueryShape parses sql with the vitess parser (the one the matcher
// already uses for AST-structure signatures) and splits it into template +
// literals + comments — the MySQL equivalent of the "give it a query, get its
// fields" helper the Postgres integration uses. Results are memoized.
func extractQueryShape(sql string) *queryShape {
	if v, ok := queryShapeCache.Load(sql); ok {
		return v.(*queryShape)
	}
	shape := computeQueryShape(sql)
	queryShapeCache.Store(sql, shape)
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

	// Count the statement's REAL `?` bind placeholders before any rewriting:
	// vitess tokenizes each `?` into an *Argument node, so this count is
	// immune to '?' bytes inside string literals (the flaw of a raw
	// strings.Count on the SQL text).
	placeholders := 0
	_ = sqlparser.Walk(func(node sqlparser.SQLNode) (bool, error) {
		if _, isArg := node.(*sqlparser.Argument); isArg {
			placeholders++
		}
		return true, nil
	}, stmt)

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
		template:     sqlparser.String(out),
		literals:     literals,
		comments:     strings.TrimSpace(strings.Join(comments, " ")),
		placeholders: placeholders,
		ok:           true,
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
// user data).
func mysqlRequestBodyJSON(bundle *mysql.PacketBundle) ([]byte, bool) {
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
		if len(msg.Parameters) > 0 {
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
		doc = map[string]any{"parameters": parametersByIndex(msg.Parameters)}
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

// parametersByIndex converts a parameter slice to an index-keyed map (see
// mysqlRequestBodyJSON for why an object beats an array here).
func parametersByIndex(params []mysql.Parameter) map[string]mysql.Parameter {
	out := make(map[string]mysql.Parameter, len(params))
	for i, p := range params {
		out[strconv.Itoa(i)] = p
	}
	return out
}
