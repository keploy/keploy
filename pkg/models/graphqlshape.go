package models

import (
	"bytes"
	"crypto/sha256"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// A GraphQL request's operation, as a test case's shape keeps it. GraphQL
// sends every operation to one route, so the operation tells the requests
// apart that the route cannot. It is the document without what a request
// varies: argument and variable default values, aliases, the order of
// selections, definitions and arguments, whitespace, commas and comments.
// Two requests that select the same fields with different arguments
// (user(id: 1), user(id: 2)) run one operation.

// graphQLRequestOperation is the GraphQL operation a request runs, "" when it
// is no GraphQL request:
//
//   - a JSON body {"query": ..., "operationName": ...} whose query is a
//     GraphQL document (looksLikeGraphQL), or a batch, a JSON array of them,
//     whose operations are a set: each distinct one, sorted, joined by ';';
//   - a JSON body naming a persisted query
//     ({"extensions": {"persistedQuery": {"sha256Hash": ...}}}) and no query;
//   - an application/graphql body;
//   - a GET with the query in the URL (?query=...&operationName=...), read
//     from the parameters that parse when another one does not.
func graphQLRequestOperation(media string, body []byte, req *jsonRequest, rawQuery string) string {
	if media == "application/graphql" {
		return graphQLOperation(string(body), "")
	}
	if req != nil && req.envelopes > 0 {
		if !req.notGraphQL {
			return strings.Join(sortedKeys(req.graphQL), ";")
		}
		if req.batch {
			return ""
		}
	}
	if rawQuery == "" {
		return ""
	}
	// url.ParseQuery returns the parameters that parse beside the error of
	// one that does not (a bad escape, a ';').
	q, _ := url.ParseQuery(rawQuery)
	if d := q.Get("query"); looksLikeGraphQL(d) {
		return graphQLOperation(d, q.Get("operationName"))
	}
	if ext := q.Get("extensions"); ext != "" {
		if h := persistedQueryHash(ext); h != "" {
			return graphQLPersisted(h, q.Get("operationName"))
		}
	}
	return ""
}

// graphQLEnvelopeOperation is the operation of one JSON GraphQL request
// envelope, "" when e is none.
func graphQLEnvelopeOperation(e *jsonEnvelope) string {
	if !e.object {
		return ""
	}
	if e.queryType == shapeString && looksLikeGraphQL(e.query) {
		return graphQLOperation(e.query, e.opName)
	}
	if e.queryType == 0 || e.queryType == shapeNull || (e.queryType == shapeString && e.query == "") {
		if e.persisted != "" {
			return graphQLPersisted(e.persisted, e.opName)
		}
	}
	return ""
}

// persistedQueryHash is the persistedQuery.sha256Hash of a GET's extensions
// parameter, "" when it has none.
func persistedQueryHash(extensions string) string {
	s := &jsonScanner{rootRole: roleExtensions, tree: &shapeNode{}}
	if !s.scan(strings.TrimSpace(extensions)) {
		return ""
	}
	return s.env.persisted
}

// graphQLPersisted is a persisted query as an operation: its hash and its
// operation name, each quoted, as a request can put any text in them.
func graphQLPersisted(hash, name string) string {
	op := "graphql:persisted:" + strconv.Quote(hash)
	if name != "" {
		op += "#" + strconv.Quote(name)
	}
	return op
}

// looksLikeGraphQL reports whether s reads as a GraphQL document: after
// whitespace and comments, a selection set or an operation or fragment
// keyword ending in a non-name character. A search API's "query" string
// field does not.
func looksLikeGraphQL(s string) bool {
	lx := gqlLexer{src: s}
	t := lx.next()
	switch {
	case t.kind == gqlPunct && t.text == "{":
		return true
	case t.kind == gqlName:
		switch t.text {
		case "query", "mutation", "subscription", "fragment":
			return true
		}
	}
	return false
}

// graphQLOperation is a GraphQL document as an operation ("graphql:" and its
// canonical form), with "#" and the operation name, quoted, when the request
// names the one it runs. The canonical form holds no '#', ';' or '"', so the
// name cannot be read as more of it, nor as another operation of a batch.
func graphQLOperation(doc, operationName string) string {
	p := gqlParser{lx: gqlLexer{src: doc}}
	p.advance()
	canonical := p.document()
	if canonical == "" {
		return ""
	}
	op := "graphql:" + canonical
	if operationName != "" {
		op += "#" + strconv.Quote(operationName)
	}
	return op
}

type gqlKind int

const (
	gqlEOF gqlKind = iota
	gqlName
	gqlPunct
	gqlValue // a number or a string
)

type gqlToken struct {
	kind gqlKind
	text string
}

// gqlLexer reads a GraphQL document's tokens, leaving out whitespace, commas
// and comments.
type gqlLexer struct {
	src string
	i   int
}

// gqlBOM is a Unicode byte order mark, which GraphQL reads as whitespace.
const gqlBOM = "\xef\xbb\xbf"

func (l *gqlLexer) next() gqlToken {
	for l.i < len(l.src) {
		c := l.src[l.i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			l.i++
		case c == '#':
			for l.i < len(l.src) && l.src[l.i] != '\n' && l.src[l.i] != '\r' {
				l.i++
			}
		case strings.HasPrefix(l.src[l.i:], gqlBOM):
			l.i += len(gqlBOM)
		default:
			return l.token()
		}
	}
	return gqlToken{kind: gqlEOF}
}

func (l *gqlLexer) token() gqlToken {
	start := l.i
	c := l.src[l.i]
	switch {
	case c == '_' || isASCIILetter(c):
		for l.i < len(l.src) && (l.src[l.i] == '_' || isASCIILetter(l.src[l.i]) || isDigitByte(l.src[l.i])) {
			l.i++
		}
		return gqlToken{kind: gqlName, text: l.src[start:l.i]}
	case c == '-' || isDigitByte(c):
		l.i++
		for l.i < len(l.src) && (isDigitByte(l.src[l.i]) || strings.IndexByte(".eE+-", l.src[l.i]) >= 0) {
			l.i++
		}
		return gqlToken{kind: gqlValue}
	case strings.HasPrefix(l.src[l.i:], `"""`):
		l.i += 3
		for l.i < len(l.src) && !strings.HasPrefix(l.src[l.i:], `"""`) {
			if strings.HasPrefix(l.src[l.i:], `\"""`) {
				l.i += 4
				continue
			}
			l.i++
		}
		l.i = min(l.i+3, len(l.src))
		return gqlToken{kind: gqlValue}
	case c == '"':
		l.i++
		for l.i < len(l.src) && l.src[l.i] != '"' && l.src[l.i] != '\n' {
			if l.src[l.i] == '\\' {
				l.i++
			}
			l.i++
		}
		l.i = min(l.i+1, len(l.src))
		return gqlToken{kind: gqlValue}
	case strings.HasPrefix(l.src[l.i:], "..."):
		l.i += 3
		return gqlToken{kind: gqlPunct, text: "..."}
	}
	l.i++
	return gqlToken{kind: gqlPunct, text: string(c)}
}

// gqlParser writes a GraphQL document's canonical form. It reads the
// executable grammar (operations, fragments, selections, arguments,
// directives, variable definitions); a token it does not expect is skipped,
// so a malformed document still gives a form, and the same one each time.
// It reads without recursion, and writes the form in full in time and memory
// linear in the document, however deep it nests.
type gqlParser struct {
	lx  gqlLexer
	tok gqlToken
}

func (p *gqlParser) advance() { p.tok = p.lx.next() }

func (p *gqlParser) is(punct string) bool { return p.tok.kind == gqlPunct && p.tok.text == punct }

func (p *gqlParser) name() string {
	if p.tok.kind != gqlName {
		return ""
	}
	n := p.tok.text
	p.advance()
	return n
}

// document is the canonical form of every definition, sorted, joined by ' '.
func (p *gqlParser) document() string {
	var defs []string
	for p.tok.kind != gqlEOF {
		switch {
		case p.is("{"):
			defs = append(defs, "query "+p.selectionSet())
		case p.tok.kind == gqlName && (p.tok.text == "query" || p.tok.text == "mutation" || p.tok.text == "subscription"):
			kind := p.name()
			def := kind
			if n := p.name(); n != "" {
				def += " " + n
			}
			def += p.variableDefinitions() + p.directives()
			if p.is("{") {
				def += " " + p.selectionSet()
			}
			defs = append(defs, def)
		case p.tok.kind == gqlName && p.tok.text == "fragment":
			p.advance()
			def := "fragment " + p.name()
			if p.tok.kind == gqlName && p.tok.text == "on" {
				p.advance()
				def += " on " + p.name()
			}
			def += p.directives()
			if p.is("{") {
				def += " " + p.selectionSet()
			}
			defs = append(defs, def)
		default:
			p.advance()
		}
	}
	sort.Strings(defs)
	return strings.Join(defs, " ")
}

// gqlSelection is a selection as read: its head (a field's name, not its
// alias, its argument names and its directives; a fragment spread; an inline
// fragment's type condition and directives) and its selection set, when it
// has one.
type gqlSelection struct {
	head string
	sels []*gqlSelection
	sum  [16]byte // a hash of its canonical form (canonicalize)
	set  bool
}

// selectionSet is the canonical form of the selection set at '{': "{", its
// selections in canonical order, each its head and its own set, joined by
// ' ', then "}". It reads the set into a tree, puts each set in canonical
// order as it closes (canonicalize), then writes the tree once, so a set
// nested to any depth costs no stack and is copied once.
func (p *gqlParser) selectionSet() string {
	root := &gqlSelection{set: true}
	open := []*gqlSelection{root}
	var buf []byte
	p.advance() // {
	for len(open) > 0 {
		if p.tok.kind == gqlEOF || p.is("}") {
			p.advance() // }
			buf = open[len(open)-1].canonicalize(buf[:0])
			open = open[:len(open)-1]
			continue
		}
		sel, hasSet := p.selection()
		if sel == nil {
			continue
		}
		parent := open[len(open)-1]
		parent.sels = append(parent.sels, sel)
		if hasSet {
			p.advance() // {
			sel.set = true
			open = append(open, sel)
		} else {
			buf = sel.canonicalize(buf[:0])
		}
	}
	return root.write()
}

// canonicalize orders a selection's set, whose selections are already
// canonical, by head and then by hash, so the order is the same whatever
// order the document gives them, and computes its own hash: of its head
// (length-prefixed), whether it has a set, and its selections' hashes in
// that order. buf is scratch space, returned for reuse.
func (s *gqlSelection) canonicalize(buf []byte) []byte {
	sort.Slice(s.sels, func(i, j int) bool {
		a, b := s.sels[i], s.sels[j]
		if a.head != b.head {
			return a.head < b.head
		}
		return bytes.Compare(a.sum[:], b.sum[:]) < 0
	})
	buf = strconv.AppendInt(buf, int64(len(s.head)), 10)
	buf = append(buf, ':')
	buf = append(buf, s.head...)
	if s.set {
		buf = append(buf, '{')
		for _, c := range s.sels {
			buf = append(buf, c.sum[:]...)
		}
	}
	sum := sha256.Sum256(buf)
	copy(s.sum[:], sum[:])
	return buf
}

// write is the selection written out, without recursion.
func (s *gqlSelection) write() string {
	type work struct {
		text string
		sel  *gqlSelection
	}
	var b strings.Builder
	stack := []work{{sel: s}}
	for len(stack) > 0 {
		w := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if w.sel == nil {
			b.WriteString(w.text)
			continue
		}
		b.WriteString(w.sel.head)
		if !w.sel.set {
			continue
		}
		stack = append(stack, work{text: "}"})
		for i := len(w.sel.sels) - 1; i >= 0; i-- {
			stack = append(stack, work{sel: w.sel.sels[i]})
			if i > 0 {
				stack = append(stack, work{text: " "})
			}
		}
		stack = append(stack, work{text: "{"})
	}
	return b.String()
}

// selection reads one selection's head, and reports whether a selection set
// follows it; it is nil for a token that starts no selection, which it
// skips.
func (p *gqlParser) selection() (*gqlSelection, bool) {
	if p.is("...") {
		p.advance()
		if p.tok.kind == gqlName && p.tok.text != "on" {
			return &gqlSelection{head: "..." + p.name() + p.directives()}, false
		}
		head := "..."
		if p.tok.kind == gqlName && p.tok.text == "on" {
			p.advance()
			head += "on " + p.name()
		}
		head += p.directives()
		return &gqlSelection{head: head}, p.is("{")
	}
	if p.tok.kind != gqlName {
		p.advance()
		return nil, false
	}
	field := p.name()
	if p.is(":") {
		p.advance()
		field = p.name()
	}
	field += p.arguments() + p.directives()
	return &gqlSelection{head: field}, p.is("{")
}

// arguments is "(" and the argument names, sorted, then ")", without their
// values; "" when there are none.
func (p *gqlParser) arguments() string {
	if !p.is("(") {
		return ""
	}
	p.advance()
	var names []string
	for p.tok.kind != gqlEOF && !p.is(")") {
		n := p.name()
		if n == "" {
			p.advance()
			continue
		}
		if p.is(":") {
			p.advance()
			p.skipValue()
		}
		names = append(names, n)
	}
	p.advance() // )
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return "(" + strings.Join(names, ",") + ")"
}

// directives is each directive, '@', its name and its argument names, in
// order.
func (p *gqlParser) directives() string {
	var b strings.Builder
	for p.is("@") {
		p.advance()
		b.WriteByte('@')
		b.WriteString(p.name())
		b.WriteString(p.arguments())
	}
	return b.String()
}

// variableDefinitions is "(" and each variable's name and type, sorted, then
// ")", without default values; "" when there are none.
func (p *gqlParser) variableDefinitions() string {
	if !p.is("(") {
		return ""
	}
	p.advance()
	var defs []string
	for p.tok.kind != gqlEOF && !p.is(")") {
		if !p.is("$") {
			p.advance()
			continue
		}
		p.advance()
		def := "$" + p.name()
		if p.is(":") {
			p.advance()
			def += ":" + p.typeRef()
		}
		if p.is("=") {
			p.advance()
			p.skipValue()
		}
		def += p.directives()
		defs = append(defs, def)
	}
	p.advance() // )
	if len(defs) == 0 {
		return ""
	}
	sort.Strings(defs)
	return "(" + strings.Join(defs, ",") + ")"
}

// typeRef is a variable's type: a name or a list of a type, each maybe "!".
// It reads the lists' '[' first, then the name, then each list's ']' and
// '!', so a type nested to any depth costs no stack and is written once.
func (p *gqlParser) typeRef() string {
	var b strings.Builder
	lists := 0
	for p.is("[") {
		p.advance()
		b.WriteByte('[')
		lists++
	}
	b.WriteString(p.name())
	if p.is("!") {
		p.advance()
		b.WriteByte('!')
	}
	for ; lists > 0; lists-- {
		if p.is("]") {
			p.advance()
		}
		b.WriteByte(']')
		if p.is("!") {
			p.advance()
			b.WriteByte('!')
		}
	}
	return b.String()
}

// skipValue reads past one value: a variable, a scalar, a list or an object,
// however deep, without recursion.
func (p *gqlParser) skipValue() {
	depth := 0
	for p.tok.kind != gqlEOF {
		switch {
		case p.is("$"):
			p.advance()
			p.name()
		case p.is("[") || p.is("{"):
			depth++
			p.advance()
			continue
		case p.is("]") || p.is("}"):
			if depth == 0 {
				return
			}
			depth--
			p.advance()
		default:
			p.advance()
		}
		if depth == 0 {
			return
		}
	}
}
