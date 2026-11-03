package replayer

import (
	"fmt"
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
// EXECUTE, or its statement whole, where the recorded value holds a match
// of one of the mock's patterns (Mock.Noise, util.NoiseChecker.IsNoisyWithin).
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
	return sqlparser.String(rewritten)
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

// fieldDiffs redacts, in a strict miss's field diffs against the recording
// p is read from, both values of each diff at a redacted place, liveQuery
// being the live statement they were compared with: a literal at a
// redacted position (every literal when the live template is not the
// recorded one), the statement whole and every part of it when
// p.statement, and the value of a bound parameter at a redacted position.
func (p redactedPlaces) fieldDiffs(diffs []models.MockFieldDiff, liveQuery string) []models.MockFieldDiff {
	if !p.inStatement() && len(p.params) == 0 {
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
			}
		case strings.HasPrefix(path, "body.parameters."):
			idx, field, _ := strings.Cut(strings.TrimPrefix(path, "body.parameters."), ".")
			n, err := strconv.Atoi(idx)
			// Everything under the parameter but its type, unsigned flag
			// and name is its value, an enveloped one ($bin) included.
			redacted = err == nil && p.params[n] && field != "name" && field != "type" && field != "unsigned"
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

// loggedLive is the live statement actual as a log of its comparison with
// the recorded statement expected shows it: its value at each place of
// expected the obfuscator redacted (by the recording's patterns noise) is
// redacted.
func loggedLive(expected, actual string, noise []string) string {
	return queryPlaces(expected, util.NewNoiseChecker(noise)).liveStatement(actual)
}
