package replayer

import (
	"encoding/binary"
	"fmt"
	"hash/maphash"
	"maps"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
// redacted; none when m is nil or carries no patterns. They are a function
// of m's patterns' texts and of what readPlaces reads of its recorded
// commands, and are kept by those (placesKept): the agent stages each test's
// pool from copies of the recordings, so a recording is read once a run,
// whichever copy of it a test's pool holds, however many commands miss. A
// recording with no patterns redacted nothing: it is not read and no digest
// is taken. The places returned are shared: read them, never change them
// (or makes new maps).
func placesOf(m *models.Mock) redactedPlaces {
	if m == nil || len(m.Noise) == 0 {
		return redactedPlaces{}
	}
	key := placesKey(m)
	if p, ok := placesKept.get(key); ok {
		return p
	}
	placesReads.Add(1)
	p := readPlaces(m)
	placesKept.put(key, p, p.size())
	return p
}

// placesReads counts the recordings read for their places (readPlaces): one
// a recording with patterns, however many commands miss and however many
// tests' pools hold a copy of it, while placesKept keeps its places.
var placesReads atomic.Int64

// readPlaces is placesOf uncached.
func readPlaces(m *models.Mock) redactedPlaces {
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

// placesKey is the digest of what readPlaces reads of m: its patterns'
// texts, then, in order, the statement of each recorded COM_QUERY and
// COM_STMT_PREPARE, the stored statement and each bound value's text
// (paramText) of each COM_STMT_EXECUTE, and the data of each
// COM_STMT_SEND_LONG_DATA, each command after a tag of its kind and each
// text and count written with its length, so no two different contents
// write the same bytes. A command of another kind, or one with no message,
// adds no place and is not written. It is two 64-bit hashes (hash/maphash)
// under two seeds drawn at start: no two recordings' contents meet by
// chance in 128 bits, and the seeds are not known outside the process.
func placesKey(m *models.Mock) placesDigest {
	var h [2]maphash.Hash
	h[0].SetSeed(placesSeeds[0])
	h[1].SetSeed(placesSeeds[1])
	var n [8]byte
	count := func(c int) {
		binary.BigEndian.PutUint64(n[:], uint64(c))
		for i := range h {
			_, _ = h[i].Write(n[:])
		}
	}
	write := func(s string) {
		count(len(s))
		for i := range h {
			_, _ = h[i].WriteString(s)
		}
	}
	tag := func(t byte) {
		for i := range h {
			_ = h[i].WriteByte(t)
		}
	}
	// param writes a bound value as its text (paramText): bytes as they
	// are, not copied into a string first, so a blob costs no copy each
	// time a recording binding it answers.
	param := func(v any) {
		t, ok := v.([]byte)
		if !ok {
			write(paramText(v))
			return
		}
		count(len(t))
		for i := range h {
			_, _ = h[i].Write(t)
		}
	}
	count(len(m.Noise))
	for _, pat := range m.Noise {
		write(pat)
	}
	for i := range m.Spec.MySQLRequests {
		b := &m.Spec.MySQLRequests[i].PacketBundle
		switch msg := b.Message.(type) {
		case *mysql.QueryPacket:
			if msg != nil {
				tag('q')
				write(msg.Query)
			}
		case *mysql.StmtPreparePacket:
			if msg != nil {
				tag('p')
				write(msg.Query)
			}
		case *mysql.StmtExecutePacket:
			if msg != nil {
				tag('e')
				write(b.StoredQuery())
				count(len(msg.Parameters))
				for _, prm := range msg.Parameters {
					param(prm.Value)
				}
			}
		case *mysql.StmtSendLongDataPacket:
			if msg != nil {
				tag('d')
				count(len(msg.Data))
				for i := range h {
					_, _ = h[i].Write(msg.Data)
				}
			}
		}
	}
	return placesDigest{h[0].Sum64(), h[1].Sum64()}
}

// placesDigest is a recording's digest (placesKey).
type placesDigest [2]uint64

// placesSeeds are placesKey's seeds.
var placesSeeds = [2]maphash.Seed{maphash.MakeSeed(), maphash.MakeSeed()}

// maxKeptPlaces bounds the bytes the places kept hold (size): past it, kept
// places are dropped, any of them, until the new ones fit. A recording's
// places hold its template and a few positions, a few hundred bytes for an
// ORM statement, so it keeps those of some tens of thousands of redacted
// recordings; a pool of more is read in part again at each miss.
const maxKeptPlaces = 32 << 20

// placesKept are the places read, by their recording's digest (placesKey).
var placesKept = keptPlaces{byKey: map[placesDigest]keptEntry{}, max: maxKeptPlaces}

// keptPlaces are places by digest, holding at most max bytes (size).
type keptPlaces struct {
	mu    sync.RWMutex
	byKey map[placesDigest]keptEntry
	bytes int
	max   int
}

type keptEntry struct {
	places redactedPlaces
	size   int
}

func (k *keptPlaces) get(key placesDigest) (redactedPlaces, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	e, ok := k.byKey[key]
	return e.places, ok
}

// put keeps p, of size bytes, by key, dropping kept places, any of them,
// until it fits; places larger than the bound are not kept.
func (k *keptPlaces) put(key placesDigest, p redactedPlaces, size int) {
	if size > k.max {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if old, ok := k.byKey[key]; ok {
		delete(k.byKey, key)
		k.bytes -= old.size
	}
	for other, e := range k.byKey {
		if k.bytes+size <= k.max {
			break
		}
		delete(k.byKey, other)
		k.bytes -= e.size
	}
	k.byKey[key] = keptEntry{places: p, size: size}
	k.bytes += size
}

// size is about the bytes p holds: its template and its positions, each
// with what a map entry costs, and its header.
func (p redactedPlaces) size() int {
	return int(unsafe.Sizeof(p)) + 64 + len(p.template) + 48*(len(p.literals)+len(p.params))
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

// or is the places of p and q together. Its maps are new when it adds to
// them: p's and q's are left as they were.
func (p redactedPlaces) or(q redactedPlaces) redactedPlaces {
	s := placesSum{p: p}
	s.add(q)
	return s.p
}

// placesSum adds places together, as or does, into maps of its own: those
// it starts from are copied before it first adds to them, and then added
// to, so a pool of n recordings' places sum into one map of literals and
// one of bound values, not n of each.
type placesSum struct {
	p redactedPlaces
	// ownLiterals and ownParams are set once p's maps are the sum's own.
	ownLiterals, ownParams bool
}

// add adds q's places to s's.
func (s *placesSum) add(q redactedPlaces) {
	if len(q.literals) > 0 {
		if len(s.p.literals) > 0 && s.p.template != q.template {
			s.p.statement = true
		}
		s.p.template = q.template
		s.p.literals = addPositions(s.p.literals, q.literals, &s.ownLiterals)
	}
	s.p.statement = s.p.statement || q.statement
	s.p.whole = s.p.whole || q.whole
	if len(q.params) > 0 {
		s.p.params = addPositions(s.p.params, q.params, &s.ownParams)
	}
	s.p.everyParam = s.p.everyParam || q.everyParam
	s.p.data = s.p.data || q.data
}

// addPositions adds b's positions to a, copied first unless *own says a is
// the caller's own; *own is set then.
func addPositions(a, b map[int]bool, own *bool) map[int]bool {
	if !*own {
		c := make(map[int]bool, len(a)+len(b))
		maps.Copy(c, a)
		a, *own = c, true
	}
	maps.Copy(a, b)
	return a
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
	var s placesSum
	for _, m := range pool {
		s.add(placesOf(m))
	}
	return s.p
}

// answeredPlaces are the places of the recordings that answered a call of
// the open test of db, the store a command is matched in: a recording that
// answered is in no later read of the pool (the store takes a test's
// recording out of it once it answers), so a call made once more than
// recorded (a retry) would find in the pool no recording that redacted its
// secret. Served exactly, they are the places of the open test's recordings
// the store lists as answered (integrations.ExactConsumedReader); otherwise
// those answered noted in the test's window (answeredNote). start is the
// start of the window the command was matched in (CurrentTestWindow, read
// as its match begins, before its pool): without exact serving, when the
// store tells another window once the places are read, the replay moved
// on while the command was matched, the note may hold another test's
// places or none of its own, and the command is redacted whole
// (everyPlace). Under exact serving no window is told (both reads are
// zero): the store's list is of the view the command's reads are made in,
// the one it was read in, even once that test ended; when the command reads
// the view in force and the store installed another since it was held
// (integrations.CallViewMoved, asked after the list is read), its pool and
// the list may be of two views, and the command is redacted whole.
func answeredPlaces(db integrations.MockMemDb, start time.Time) redactedPlaces {
	p, ok := answeredNote(db).of(db, start)
	if !ok {
		return everyPlace
	}
	if er, ok := db.(integrations.ExactConsumedReader); ok {
		p = p.or(poolPlaces(er.ConsumedTestMocks()))
	}
	if integrations.CallViewMoved(db) {
		return everyPlace
	}
	return p
}

// answeredKey is the slot a store keeps its answered note in
// (integrations.Keep).
type answeredKey struct{}

// answeredNote is the note of the places of the recordings db took out of its
// pool as they answered, by the start of db's open test window
// (CurrentTestWindow): a window of another start, the next test's, starts it
// again. A window whose end alone moved is the same test's, and keeps them.
// It keeps their union, not the recordings, so it holds no more than the
// places a test's recordings name, however many answered. Under exact
// serving the store lists them itself, and no window is open (every test's
// would be one).
//
// The note is kept with the store (integrations.Keep), so it is that store's
// alone: a long-lived agent closes its store at each record session, and a
// later store, whose window can open at the same start (the same recordings
// replayed again), or none, or that serves exactly, reads none of it. A
// connection of the closed store still reads it (it keeps the store it was
// opened with), and it is collected with the store. Within the store it is
// let go of when the store lets go of the recordings it staged
// (integrations.WhenRestaged) at a staging that finds its window is not the
// note's, as a new replay session's staging does (it opens none), so that
// session reads none of it either; a staging lets go before it moves the
// window to the next test, so a test's note is let go of at the staging
// after that one, and no read finds it meanwhile, as the window is
// another's. A store staged with no window (SetFilteredMocks, which lets
// nothing go) keeps its note across its replay sessions, redacting more,
// never less. A store that keeps nothing shares unkeptAnswered with every
// other such store.
func answeredNote(db integrations.MockMemDb) *windowPlaces {
	if w, ok := integrations.Keep(db, answeredKey{}, func() any { return new(windowPlaces) }); ok {
		return w.(*windowPlaces)
	}
	return &unkeptAnswered
}

// unkeptAnswered is the answered note of the stores that keep nothing
// (answeredNote).
var unkeptAnswered windowPlaces

type windowPlaces struct {
	// mu guards the note, and the store's window it is told by is read
	// under it (add, of, released): each takes the note and the window the
	// store tells at one instant, while no other add, read or release of
	// the note runs. An answer of a test that takes mu only after the store
	// moved on to the next test reads the next test's window and adds to
	// its note: it cannot begin the note again over that test's answers.
	// The store's window lock is taken under mu, never mu under it, and a
	// read of the window takes no lock a staging holds as it runs a release
	// (it holds the manager's swap lock).
	mu sync.Mutex
	// held is set while a note is kept: from the first answer that adds to
	// it until its store lets it go (released).
	held   bool
	start  time.Time
	places redactedPlaces
	// gen counts the notes begun, so a store's release lets go of the note
	// it was asked for alone (unkeptAnswered is several stores' note).
	gen uint64
}

// add folds p into the places of db's open test window.
func (w *windowPlaces) add(db integrations.MockMemDb, p redactedPlaces) {
	if !p.redactsAny() {
		return
	}
	w.mu.Lock()
	start, _ := db.CurrentTestWindow()
	begun := !w.held || !start.Equal(w.start)
	if begun {
		w.gen++
		w.held, w.start, w.places = true, start, redactedPlaces{}
	}
	w.places = w.places.or(p)
	gen := w.gen
	w.mu.Unlock()
	if begun {
		w.letGoWith(db, gen, start)
	}
}

// of is the places kept for db's open test window, that of start; none
// when no recording answered in it. ok is false when db's open window is
// not that of start.
func (w *windowPlaces) of(db integrations.WindowAware, start time.Time) (p redactedPlaces, ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now, _ := db.CurrentTestWindow()
	if !now.Equal(start) {
		return redactedPlaces{}, false
	}
	if !now.Equal(w.start) {
		return redactedPlaces{}, true
	}
	return w.places, true
}

// letGoWith asks db to let go of the note gen, begun in its window of start,
// when it lets go of the recordings it staged (released). Outside w.mu: a
// closed store runs the release at once.
func (w *windowPlaces) letGoWith(db integrations.MockMemDb, gen uint64, start time.Time) {
	var asked atomic.Bool
	integrations.WhenRestaged(db, w, func() { w.released(db, gen, start, asked.Load()) })
	asked.Store(true)
}

// released lets go of the note gen when db's window, at a staging or its
// Close, is not the note's start; else it keeps the note and asks db again
// (letGoWith). A Close keeps the window, so a closed store keeps its note
// for its connections. asked is unset when the release ran before
// WhenRestaged returned: at once, as db is closed, or at a staging that
// raced the ask. db is not asked again then, so a closed store does not run
// the release at once without end; a note kept by a staging that raced the
// ask is let go of when the next note of the store begins, and redacts more
// until then, never less.
func (w *windowPlaces) released(db integrations.MockMemDb, gen uint64, start time.Time, asked bool) {
	w.mu.Lock()
	if w.gen != gen {
		// Another note was begun since.
		w.mu.Unlock()
		return
	}
	now, _ := db.CurrentTestWindow()
	gone := !now.Equal(start)
	if gone {
		w.held, w.places = false, redactedPlaces{}
	}
	w.mu.Unlock()
	if !gone && asked {
		w.letGoWith(db, gen, start)
	}
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
