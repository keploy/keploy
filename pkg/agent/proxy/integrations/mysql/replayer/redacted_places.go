package replayer

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	lru "github.com/hashicorp/golang-lru/v2"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	// vitesslog must load before any vitess code logs: see that package.
	_ "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/vitesslog"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
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
	// everyParam redacts every bound value of an EXECUTE, whatever its
	// position: the places of the pool could not be read (everyPlace).
	everyParam bool
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
	// outside every literal and comment redacts the statement whole. A
	// comment is read as written, wherever it stands (the template keeps
	// none, and the shape the leading ones alone); a statement whose
	// comments cannot all be read is redacted whole.
	lits, ok := readSQLLiterals(sql)
	if !ok {
		p.statement, p.whole = true, true
		return p
	}
	inComments := false
	for _, t := range lits.tokens {
		inComments = inComments || t.comment && nc.IsNoisyWithin(sql[t.start:t.end])
	}
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
	p.everyParam = p.everyParam || q.everyParam
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
func (p redactedPlaces) redactsAny() bool {
	return p.inStatement() || len(p.params) > 0 || p.everyParam || p.data
}

// liveStatement is the live statement sql as a report or a log shows it:
// sql itself when the recording redacted no place of its statement; else
// sql as the client sent it with the literal at each redacted place
// redacted, every literal (and every comment dropped) when its template is
// not the recorded one or p.statement is set, and sql whole redacted when
// p.whole is set, it cannot be parsed or tokenized, or its literals as
// written do not map one-to-one onto the literals the parser reads
// (sqlLiterals.matched). A literal is redacted where the client wrote it
// (its byte span), so the rest of the statement keeps the client's text:
// its keywords' case, its spacing and its ? markers.
func (p redactedPlaces) liveStatement(sql string) string {
	if !p.inStatement() || sql == "" {
		return sql
	}
	if p.whole {
		return valueRedacted
	}
	lits, ok := readSQLLiterals(sql)
	return p.shownStatement(sql, lits, ok)
}

// shownStatement is liveStatement of sql, read as lits (ok as
// readSQLLiterals returned it).
func (p redactedPlaces) shownStatement(sql string, lits sqlLiterals, ok bool) string {
	if !ok || !lits.matched {
		return valueRedacted
	}
	all := p.statement || extractQueryShape(sql).template != p.template
	// The literal at a redacted place is redacted wherever the client
	// wrote its value: the parser's order of literals need not be the
	// order they are written in (LIMIT n OFFSET m reads m first).
	hidden := map[string]bool{}
	for i, v := range lits.parsed {
		if p.literals[i] {
			hidden[v] = true
		}
	}
	var b strings.Builder
	last := 0
	for _, t := range lits.tokens {
		switch {
		case t.comment:
			if !all {
				continue
			}
			// A space keeps the tokens on either side apart.
			b.WriteString(sql[last:t.start] + " ")
		case all || hidden[t.val]:
			b.WriteString(sql[last:t.start] + "'" + valueRedacted + "'")
		default:
			continue
		}
		last = t.end
	}
	b.WriteString(sql[last:])
	return b.String()
}

// sqlLiterals are the literals of a statement, as the parser reads them
// and as the tokenizer reads them where the client wrote them.
type sqlLiterals struct {
	// parsed are the values of the statement's literals in the parser's
	// walk order (extractQueryShape's positions).
	parsed []string
	// tokens are its literals and comments as written, with their byte
	// spans, in the order written.
	tokens []sqlToken
	// matched is set when the literals as written map one-to-one onto the
	// literals the parser reads: as many of each value, and no literal in
	// a /*! */ comment (the parser reads its text as the statement's, and
	// the tokenizer as a comment). Only then is each literal the parser
	// reads redacted where the client wrote it, and the template keeps no
	// literal; else the statement is shown redacted whole. A literal the
	// walk does not reach (CHAR(10), a column's DEFAULT, an INTO OUTFILE
	// path) and a literal of no token of its own (two strings written
	// side by side, read as one) leave it unset.
	matched bool
}

// sqlToken is a literal or a comment of a statement: sql[start:end] as
// written, and a literal's value as the parser reads it.
type sqlToken struct {
	start, end int
	val        string
	comment    bool
}

// sqlTokenKinds are kinds of token the tokenizer reads: literal ones, and
// text ones that hold no literal (a keyword and a token of one byte, which
// is punctuation or an operator, hold none either).
type sqlTokenKinds struct{ literal, text map[int]bool }

// sqlKinds are the tokenizer's kinds of a literal, and of the text other
// than keywords and one-byte tokens: a name, a variable, a bind marker and
// an operator of more than one byte.
var sqlKinds = sqlTokenKinds{
	literal: map[int]bool{
		sqlparser.STRING: true, sqlparser.NCHAR_STRING: true, sqlparser.INTEGRAL: true, sqlparser.FLOAT: true,
		sqlparser.DECIMAL: true, sqlparser.HEXNUM: true, sqlparser.HEX: true, sqlparser.BIT_LITERAL: true,
		sqlparser.BITNUM: true,
	},
	text: map[int]bool{
		sqlparser.ID: true, sqlparser.AT_ID: true, sqlparser.AT_AT_ID: true,
		sqlparser.VALUE_ARG: true, sqlparser.LIST_ARG: true, sqlparser.OFFSET_ARG: true,
		sqlparser.ASSIGNMENT_OPT: true, sqlparser.NE: true, sqlparser.LE: true, sqlparser.GE: true,
		sqlparser.NULL_SAFE_EQUAL: true, sqlparser.SHIFT_LEFT: true, sqlparser.SHIFT_RIGHT: true,
		sqlparser.JSON_EXTRACT_OP: true, sqlparser.JSON_UNQUOTE_EXTRACT_OP: true,
	},
}

// sqlLiteralsRead is a statement read by readSQLLiterals.
type sqlLiteralsRead struct {
	lits sqlLiterals
	ok   bool
}

// bytes is about what l holds beside the statement it was read from: its
// literals' values and headers, as read and as written.
func (l sqlLiterals) bytes() int {
	n := int(unsafe.Sizeof(""))*cap(l.parsed) + int(unsafe.Sizeof(sqlToken{}))*cap(l.tokens)
	for _, v := range l.parsed {
		n += len(v)
	}
	for _, t := range l.tokens {
		n += len(t.val)
	}
	return n
}

// sqlLiteralsCache memoizes readSQLLiterals per SQL text, as
// queryShapeCache does extractQueryShape, and keeps what memoKeeps keeps: a
// pool's places are read for every command a debug line or a miss shows,
// and each redacted recording the cache keeps is read once, not once per
// command. Its values are shared: read them, never change them.
var sqlLiteralsCache = func() *lru.TwoQueueCache[string, sqlLiteralsRead] {
	c, err := lru.New2Q[string, sqlLiteralsRead](queryShapeCacheSize)
	if err != nil {
		// Unreachable: New2Q only errors on a non-positive size.
		panic(fmt.Sprintf("mysql replayer: invalid queryShapeCacheSize %d: %v", queryShapeCacheSize, err))
	}
	return c
}()

// readSQLLiterals reads the literals of sql (sqlLiterals); false when the
// parser cannot read it, or the tokenizer reads a token of a kind it does
// not know to be a literal, a comment or text that holds none (a kind the
// parser may read a literal from, which no span would stand for).
func readSQLLiterals(sql string) (sqlLiterals, bool) {
	if v, ok := sqlLiteralsCache.Get(sql); ok {
		return v.lits, v.ok
	}
	lits, ok := scanSQLLiterals(sql, sqlKinds)
	if memoKeeps(sql, lits.bytes()) {
		sqlLiteralsCache.Add(sql, sqlLiteralsRead{lits: lits, ok: ok})
	}
	return lits, ok
}

// scanSQLLiterals is readSQLLiterals uncached, by the kinds of token kinds.
func scanSQLLiterals(sql string, kinds sqlTokenKinds) (out sqlLiterals, ok bool) {
	parser, err := sqlparser.New(sqlparser.Options{})
	if err != nil {
		return out, false
	}
	stmt, err := parser.Parse(sql)
	if err != nil {
		return out, false
	}
	_ = sqlparser.Walk(func(node sqlparser.SQLNode) (bool, error) {
		if lit, isLit := node.(*sqlparser.Literal); isLit {
			out.parsed = append(out.parsed, lit.Val)
		}
		return true, nil
	}, stmt)
	// A /*! */ comment is read as a comment: its text is the statement's
	// to the parser, and no span of it can be told apart.
	tkn := parser.NewStringTokenizer(sql)
	tkn.SkipSpecialComments = true
	written := map[string]int{}
	special := false
	nLits := 0
	for {
		// A token starts past the blanks the scan skips first.
		at := len(sql) - len(strings.TrimLeft(sql[tkn.Pos:], " \n\r\t"))
		typ, val := tkn.Scan()
		switch {
		case typ == 0:
			out.matched = !special && nLits == len(out.parsed)
			for _, v := range out.parsed {
				out.matched = out.matched && written[v] > 0
				written[v]--
			}
			return out, true
		case typ == sqlparser.COMMENT:
			special = special || strings.HasPrefix(sql[at:], "/*!")
			out.tokens = append(out.tokens, sqlToken{start: at, end: tkn.Pos, comment: true})
		case kinds.literal[typ]:
			if typ == sqlparser.BIT_LITERAL {
				// The parser reads b'0101' as the literal 0b0101.
				val = "0b" + val
			}
			nLits++
			written[val]++
			out.tokens = append(out.tokens, sqlToken{start: at, end: tkn.Pos, val: val})
		case typ < 256 || kinds.text[typ] || sqlparser.KeywordString(typ) != "":
		default:
			// A kind not known, a scan error (LEX_ERROR) among them: the
			// scan does not advance past an error.
			return out, false
		}
	}
}

// liveParams is params as a report shows them: each value at a redacted
// place redacted. params itself when none is.
func (p redactedPlaces) liveParams(params []mysql.Parameter) []mysql.Parameter {
	if len(p.params) == 0 && !p.everyParam {
		return params
	}
	out := make([]mysql.Parameter, len(params))
	copy(out, params)
	for i := range out {
		if (p.everyParam || p.params[i]) && out[i].Value != nil {
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
				// keeps a literal the parser's walk does not reach (an
				// INTO OUTFILE path) as the client sent it: it is redacted
				// whenever the statement's literals as written do not map
				// one-to-one onto those the parser reads, as it is when the
				// statement cannot be read.
				lits, ok := readSQLLiterals(liveQuery)
				redacted = !ok || !lits.matched
			}
		case strings.HasPrefix(path, "body.parameters."):
			idx, field, _ := strings.Cut(strings.TrimPrefix(path, "body.parameters."), ".")
			n, err := strconv.Atoi(idx)
			// Everything under the parameter but its type, unsigned flag
			// and name is its value, an enveloped one ($bin) included.
			redacted = err == nil && (p.everyParam || p.params[n]) && field != "name" && field != "type" && field != "unsigned"
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
// of them together: the replay's logs and its miss reports show the live
// command by these, whichever candidate a line compares it with and
// whichever recording a report names, so neither carries a value the
// recording it names left in but another recording hid.
func poolPlaces(pool []*models.Mock) redactedPlaces {
	var p redactedPlaces
	for _, m := range pool {
		p = p.or(placesOf(m))
	}
	return p
}

// answeredPlaces are the places of the recordings that answered a call of
// the open test of db, the store a command is matched in: a recording that
// answered is in no later read of the pool (the store takes a test's
// recording out of it once it answers), so a call made once more than
// recorded (a retry) would find in the pool no recording that redacted its
// secret. Served exactly, they are the places of the open test's recordings
// the store lists as answered (integrations.ExactConsumedReader); otherwise
// those answered noted in the test's window (answeredInWindow).
func answeredPlaces(db integrations.MockMemDb) redactedPlaces {
	p := answeredInWindow.of(db)
	if er, ok := db.(integrations.ExactConsumedReader); ok {
		p = p.or(poolPlaces(er.ConsumedTestMocks()))
	}
	return p
}

// answeredInWindow keeps the places of the recordings a store took out of
// its pool as they answered, by the start of the store's open test window
// (CurrentTestWindow): a window of another start, the next test's, starts
// it again. A window whose end alone moved is the same test's, and keeps
// them. It keeps their union, not the recordings, so it holds no more than
// the places a test's recordings name, however many answered. Under exact
// serving the store lists them itself, and no window is open (every test's
// would be one).
var answeredInWindow windowPlaces

type windowPlaces struct {
	mu     sync.Mutex
	start  time.Time
	places redactedPlaces
}

// add folds p into the places of db's open test window.
func (w *windowPlaces) add(db integrations.WindowAware, p redactedPlaces) {
	if !p.redactsAny() {
		return
	}
	start, _ := db.CurrentTestWindow()
	w.mu.Lock()
	defer w.mu.Unlock()
	if !start.Equal(w.start) {
		w.start, w.places = start, redactedPlaces{}
	}
	w.places = w.places.or(p)
}

// of is the places kept for db's open test window; none when no recording
// answered in it.
func (w *windowPlaces) of(db integrations.WindowAware) redactedPlaces {
	start, _ := db.CurrentTestWindow()
	w.mu.Lock()
	defer w.mu.Unlock()
	if !start.Equal(w.start) {
		return redactedPlaces{}
	}
	return w.places
}

// everyPlace redacts a live command whole: its statement, every bound
// value and every streamed chunk. A log line and a miss report read it when
// the pool's places cannot be read.
var everyPlace = redactedPlaces{statement: true, whole: true, everyParam: true, data: true}

// logPlaces reads the places a log line shows the live statement by
// (poolPlaces), once per command, and shows each text of the statement by
// them once too. A command's lines show its statement for each candidate
// they compare it with, as written or as its identity (sqlStatementIdentity),
// and a statement the caches do not keep (memoKeeps) would be read again for
// each line. The texts a command's lines show are its own: a few at most.
type logPlaces struct {
	read func() redactedPlaces
	// held are the places read, once read.
	held *redactedPlaces
	// shown is each text live showed, by the text.
	shown map[string]string
}

// places are the places r reads, read once; every place when there is no
// r.
func (r *logPlaces) places() redactedPlaces {
	if r == nil {
		return everyPlace
	}
	if r.held == nil {
		p := r.read()
		r.held = &p
	}
	return *r.held
}

// live is the live statement sql as a log line shows it: by the places r
// reads, or whole redacted when there is no r. No statement reads none.
func (r *logPlaces) live(sql string) string {
	if sql == "" {
		return sql
	}
	if r == nil {
		return everyPlace.liveStatement(sql)
	}
	shown, ok := r.shown[sql]
	if !ok {
		shown = r.places().liveStatement(sql)
		if r.shown == nil {
			r.shown = map[string]string{}
		}
		r.shown[sql] = shown
	}
	return shown
}
