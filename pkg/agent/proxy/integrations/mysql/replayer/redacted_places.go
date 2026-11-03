package replayer

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	// vitesslog must load before any vitess code logs: see that package.
	_ "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/vitesslog"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"vitess.io/vitess/go/vt/sqlparser"
)

// valueRedacted stands, in a miss report and in the replay's logs, for the
// live call's value at a place the recording redacted.
const valueRedacted = "(redacted)"

// redactedPlaces are the places of a recorded MySQL command whose value the
// obfuscator redacted: a literal of its statement, a bound value of an
// EXECUTE, its statement whole, or a COM_STMT_SEND_LONG_DATA chunk, where
// the recorded value holds a match of one of the mock's patterns
// (Mock.Noise, util.NoiseChecker.IsNoisyWithin).
//
// The live command's value at such a place is the secret the recording hid,
// and it matches none of those patterns: they are anchored on the value the
// obfuscator wrote. So a miss report, and a log of the miss, redact the live
// side of these places by the place, never by the value.
type redactedPlaces struct {
	// template is the recorded statement's (queryShape.template) whose
	// literals are redacted at the positions in literals.
	template string
	literals map[int]bool
	// statement is set when which literal of the live statement stands at
	// a redacted place cannot be told: the recording redacted a value in
	// its comments, or two recorded statements of different templates hold
	// redacted literals, or whole is set. Every literal of the live
	// statement is redacted then, and its comments dropped.
	statement bool
	// whole is set when the redacted value stands in no literal or comment
	// of the recorded statement (in its template, across its parts) or the
	// parser cannot read it: the live statement is redacted whole then, as
	// its text at that place is no literal the redaction could replace.
	whole bool
	// params are an EXECUTE's bound values by position.
	params map[int]bool
	// data is set when a COM_STMT_SEND_LONG_DATA chunk holds a redacted
	// value. The obfuscator redacts within the chunk's text, which may no
	// longer be the JSON document the live chunk is diffed as, so no place
	// of it is told apart: each diff of a chunk is redacted.
	data bool
}

// placesOf reads the places of m's recorded commands the obfuscator
// redacted; none when m is nil or carries no patterns.
func placesOf(m *models.Mock) redactedPlaces {
	if m == nil {
		return redactedPlaces{}
	}
	nc := util.NewNoiseChecker(m.Noise)
	var p redactedPlaces
	if nc == nil {
		return p
	}
	for i := range m.Spec.MySQLRequests {
		b := &m.Spec.MySQLRequests[i].PacketBundle
		switch msg := b.Message.(type) {
		case *mysql.QueryPacket:
			if msg != nil {
				p = p.or(queryPlaces(msg.Query, nc))
			}
		case *mysql.StmtPreparePacket:
			if msg != nil {
				p = p.or(queryPlaces(msg.Query, nc))
			}
		case *mysql.StmtExecutePacket:
			if msg == nil {
				continue
			}
			p = p.or(queryPlaces(b.StoredQuery(), nc))
			for j, prm := range msg.Parameters {
				if nc.IsNoisyWithin(paramText(prm.Value)) {
					if p.params == nil {
						p.params = map[int]bool{}
					}
					p.params[j] = true
				}
			}
		case *mysql.StmtSendLongDataPacket:
			if msg != nil && nc.IsNoisyWithin(string(msg.Data)) {
				p.data = true
			}
		}
	}
	return p
}

// queryPlaces reads the places of the recorded statement sql the
// obfuscator redacted, by the mock's patterns nc.
func queryPlaces(sql string, nc *util.NoiseChecker) redactedPlaces {
	var p redactedPlaces
	if sql == "" || !nc.IsNoisyWithin(sql) {
		return p
	}
	shape := extractQueryShape(sql)
	if !shape.ok {
		p.statement, p.whole = true, true
		return p
	}
	for i, lit := range shape.literals {
		if nc.IsNoisyWithin(lit) {
			if p.literals == nil {
				p.literals = map[int]bool{}
			}
			p.literals[i] = true
		}
	}
	// A redacted value no literal holds has no literal of the live
	// statement standing for it: its comments are dropped, and a value
	// outside every literal and comment redacts the statement whole.
	inComments := nc.IsNoisyWithin(shape.comments)
	if nc.IsNoisyWithin(shape.template) || len(p.literals) == 0 && !inComments {
		p.statement, p.whole = true, true
	}
	if inComments {
		p.statement = true
	}
	p.template = shape.template
	return p
}

// or is the places of p and q together.
func (p redactedPlaces) or(q redactedPlaces) redactedPlaces {
	if len(q.literals) > 0 {
		if len(p.literals) > 0 && p.template != q.template {
			p.statement = true
		}
		p.template = q.template
		p.literals = mergePositions(p.literals, q.literals)
	}
	p.statement = p.statement || q.statement
	p.whole = p.whole || q.whole
	p.params = mergePositions(p.params, q.params)
	p.data = p.data || q.data
	return p
}

func mergePositions(a, b map[int]bool) map[int]bool {
	if len(b) == 0 {
		return a
	}
	out := make(map[int]bool, len(a)+len(b))
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// inStatement reports whether p holds a place of a statement.
func (p redactedPlaces) inStatement() bool { return p.statement || len(p.literals) > 0 }

// redactsAny reports whether p holds any place.
func (p redactedPlaces) redactsAny() bool { return p.inStatement() || len(p.params) > 0 || p.data }

// liveStatement is the live statement sql as a report or a log shows it:
// sql itself when the recording redacted no place of its statement; else
// sql with the literal at each redacted place redacted, every literal (and
// its comments dropped) when its template is not the recorded one or
// p.statement is set, and sql whole redacted when p.whole is set or it
// cannot be parsed.
func (p redactedPlaces) liveStatement(sql string) (out string) {
	if !p.inStatement() || sql == "" {
		return sql
	}
	if p.whole {
		return valueRedacted
	}
	// The rewriter panics on AST slots it cannot replace (computeQueryShape).
	defer func() {
		if r := recover(); r != nil {
			out = valueRedacted
		}
	}()
	parser, err := sqlparser.New(sqlparser.Options{})
	if err != nil {
		return valueRedacted
	}
	stmt, err := parser.Parse(sql)
	if err != nil {
		return valueRedacted
	}
	all := p.statement || extractQueryShape(sql).template != p.template
	if c, ok := stmt.(sqlparser.Commented); ok && all {
		c.SetComments(nil)
	}
	n := -1
	rewritten := sqlparser.Rewrite(stmt, func(cursor *sqlparser.Cursor) bool {
		if _, isLit := cursor.Node().(*sqlparser.Literal); isLit {
			n++
			if all || p.literals[n] {
				cursor.Replace(sqlparser.NewStrLiteral(valueRedacted))
			}
		}
		return true
	}, nil)
	if all && literalTokens(parser, sql) > n+1 {
		// A literal the rewriter does not reach (a column's DEFAULT) would
		// be shown as the client sent it.
		return valueRedacted
	}
	return sqlparser.String(rewritten)
}

// literalTokens is how many literals the tokenizer reads in sql (a number,
// a string, a hex or bit value), the most there are when it cannot read it.
func literalTokens(parser *sqlparser.Parser, sql string) int {
	tkn := parser.NewStringTokenizer(sql)
	n := 0
	for {
		switch typ, _ := tkn.Scan(); typ {
		case 0:
			return n
		case sqlparser.LEX_ERROR:
			return math.MaxInt
		case sqlparser.STRING, sqlparser.NCHAR_STRING, sqlparser.INTEGRAL, sqlparser.FLOAT, sqlparser.DECIMAL,
			sqlparser.HEXNUM, sqlparser.HEX, sqlparser.BIT_LITERAL:
			n++
		}
	}
}

// liveParams is params as a report shows them: each value at a redacted
// place redacted. params itself when none is.
func (p redactedPlaces) liveParams(params []mysql.Parameter) []mysql.Parameter {
	if len(p.params) == 0 {
		return params
	}
	out := make([]mysql.Parameter, len(params))
	copy(out, params)
	for i := range out {
		if p.params[i] && out[i].Value != nil {
			out[i].Value = valueRedacted
		}
	}
	return out
}

// fieldDiffs redacts, in place, both values of each of a strict miss's
// field diffs at a place of p, the places of every recording its report
// names, liveQuery being the live statement they were compared with: a
// literal at a redacted position (every literal when the live template is
// not the recorded one), the statement whole and every part of it when
// p.statement, the template when the live statement is redacted whole, the
// value of a bound parameter at a redacted position, and every diff of a
// streamed chunk (body.data) when p holds any place.
func (p redactedPlaces) fieldDiffs(diffs []models.MockFieldDiff, liveQuery string) []models.MockFieldDiff {
	if !p.redactsAny() {
		return diffs
	}
	allLiterals := p.statement || (len(p.literals) > 0 && extractQueryShape(liveQuery).template != p.template)
	for i := range diffs {
		path := diffs[i].Path
		redacted := false
		switch {
		case path == "body.query" || strings.HasPrefix(path, "body.query."):
			// A literal is keyed by its position (queryBodyValue).
			rest, isLiteral := strings.CutPrefix(path, "body.query.literals.")
			n, _ := strconv.Atoi(rest)
			switch {
			case p.statement || path == "body.query" || path == "body.query.literals":
				redacted = p.inStatement()
			case isLiteral:
				redacted = allLiterals || p.literals[n]
			case path == "body.query.template":
				// A template of the same structure (sameQueryStructure)
				// keeps a literal the rewriter does not reach (an INTO
				// OUTFILE path) as the client sent it: the live statement is
				// redacted whole then (liveStatement), and so is this.
				redacted = p.liveStatement(liveQuery) == valueRedacted
			}
		case strings.HasPrefix(path, "body.parameters."):
			idx, field, _ := strings.Cut(strings.TrimPrefix(path, "body.parameters."), ".")
			n, err := strconv.Atoi(idx)
			// Everything under the parameter but its type, unsigned flag
			// and name is its value, an enveloped one ($bin) included.
			redacted = err == nil && p.params[n] && field != "name" && field != "type" && field != "unsigned"
		case path == "body.data" || strings.HasPrefix(path, "body.data."):
			// A chunk's diff: the recording redacted a value somewhere, and
			// which place of the chunk it was is not told (p.data).
			redacted = true
		}
		if !redacted {
			continue
		}
		// The side the field is absent from stays empty.
		if diffs[i].Kind != models.DiffKindMissingInMock {
			diffs[i].Expected = valueRedacted
		}
		if diffs[i].Kind != models.DiffKindMissingInLive {
			diffs[i].Actual = valueRedacted
		}
	}
	return diffs
}

// paramText is a bound value as the obfuscator read it: a string or bytes
// as their text, any other value as fmt prints it.
func paramText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return fmt.Sprint(v)
	}
}

// liveStatementOf is the statement of the live command b: a COM_QUERY's or
// a COM_STMT_PREPARE's text, else executeQuery (an EXECUTE's prepared one).
func liveStatementOf(b *mysql.PacketBundle, executeQuery string) string {
	if b != nil {
		switch m := b.Message.(type) {
		case *mysql.QueryPacket:
			if m != nil {
				return m.Query
			}
		case *mysql.StmtPreparePacket:
			if m != nil {
				return m.Query
			}
		}
	}
	return executeQuery
}

// poolPlaces are the places any recording of pool redacted (placesOf), all
// of them together: the replay's logs show the live statement by these,
// whichever candidate a line compares it with, so a log line carries no
// value the recording it names left in but another recording hid.
func poolPlaces(pool []*models.Mock) redactedPlaces {
	var p redactedPlaces
	for _, m := range pool {
		p = p.or(placesOf(m))
	}
	return p
}

// everyPlace redacts a live statement whole: the places a log reads when
// the pool's cannot be read.
var everyPlace = redactedPlaces{statement: true, whole: true}

// logPlaces reads the places a log line shows the live statement by
// (poolPlaces), once per command.
type logPlaces func() redactedPlaces

// live is the live statement sql as a log line shows it: by the places r
// reads, or whole redacted when there is no r. No statement reads none.
func (r logPlaces) live(sql string) string {
	if sql == "" {
		return sql
	}
	if r == nil {
		return everyPlace.liveStatement(sql)
	}
	return r().liveStatement(sql)
}
