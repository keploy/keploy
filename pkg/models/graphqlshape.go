package models

import (
	"encoding/json"
	"net/url"
	"sort"
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
//     GraphQL document (looksLikeGraphQL), or a batch, a JSON array of them;
//   - a JSON body naming a persisted query
//     ({"extensions": {"persistedQuery": {"sha256Hash": ...}}}) and no query;
//   - an application/graphql body;
//   - a GET with the query in the URL (?query=...&operationName=...).
func graphQLRequestOperation(media string, body []byte, doc any, rawQuery string) string {
	if media == "application/graphql" {
		return graphQLOperation(string(body), "")
	}
	if op := graphQLEnvelopeOperation(doc); op != "" {
		return op
	}
	if arr, ok := doc.([]any); ok && len(arr) > 0 {
		ops := make([]string, 0, len(arr))
		for _, v := range arr {
			op := graphQLEnvelopeOperation(v)
			if op == "" {
				return ""
			}
			ops = append(ops, op)
		}
		sort.Strings(ops)
		return strings.Join(ops, ";")
	}
	if rawQuery == "" {
		return ""
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return ""
	}
	if d := q.Get("query"); looksLikeGraphQL(d) {
		return graphQLOperation(d, q.Get("operationName"))
	}
	if ext := q.Get("extensions"); ext != "" {
		var e any
		if json.Unmarshal([]byte(ext), &e) == nil {
			if h := persistedQueryHash(e); h != "" {
				return graphQLPersisted(h, q.Get("operationName"))
			}
		}
	}
	return ""
}

// graphQLEnvelopeOperation is the operation of one JSON GraphQL request
// envelope, "" when v is none.
func graphQLEnvelopeOperation(v any) string {
	obj, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	name, _ := obj["operationName"].(string)
	if d, ok := obj["query"].(string); ok && looksLikeGraphQL(d) {
		return graphQLOperation(d, name)
	}
	if _, has := obj["query"]; !has || obj["query"] == nil || obj["query"] == "" {
		if h := persistedQueryHash(obj["extensions"]); h != "" {
			return graphQLPersisted(h, name)
		}
	}
	return ""
}

// persistedQueryHash is the sha256Hash of an envelope's
// extensions.persistedQuery, "" when it has none.
func persistedQueryHash(ext any) string {
	e, ok := ext.(map[string]any)
	if !ok {
		return ""
	}
	pq, ok := e["persistedQuery"].(map[string]any)
	if !ok {
		return ""
	}
	h, _ := pq["sha256Hash"].(string)
	return h
}

func graphQLPersisted(hash, name string) string {
	op := "graphql:persisted:" + hash
	if name != "" {
		op += "#" + name
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
// canonical form), with "#" and the operation name when the request names
// the one it runs.
func graphQLOperation(doc, operationName string) string {
	p := gqlParser{lx: gqlLexer{src: doc}}
	p.advance()
	canonical := p.document()
	if canonical == "" {
		return ""
	}
	op := "graphql:" + canonical
	if operationName != "" {
		op += "#" + operationName
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
type gqlParser struct {
	lx    gqlLexer
	tok   gqlToken
	depth int // selection sets open
}

// gqlMaxDepth bounds how deep the form follows nested selection sets; a
// deeper one is written "{…}". It keeps a crafted document from exhausting
// the stack.
const gqlMaxDepth = 64

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

// selectionSet is "{" and the set's selections, sorted, then "}".
func (p *gqlParser) selectionSet() string {
	if p.depth >= gqlMaxDepth {
		p.skipSelectionSet()
		return "{…}"
	}
	p.depth++
	defer func() { p.depth-- }()
	p.advance() // {
	var sel []string
	for p.tok.kind != gqlEOF && !p.is("}") {
		s := p.selection()
		if s != "" {
			sel = append(sel, s)
		}
	}
	p.advance() // }
	sort.Strings(sel)
	return "{" + strings.Join(sel, " ") + "}"
}

// selection is a field (its name, not its alias, its argument names, its
// directives and its selections), a fragment spread or an inline fragment.
func (p *gqlParser) selection() string {
	if p.is("...") {
		p.advance()
		if p.tok.kind == gqlName && p.tok.text != "on" {
			return "..." + p.name() + p.directives()
		}
		s := "..."
		if p.tok.kind == gqlName && p.tok.text == "on" {
			p.advance()
			s += "on " + p.name()
		}
		s += p.directives()
		if p.is("{") {
			s += p.selectionSet()
		}
		return s
	}
	if p.tok.kind != gqlName {
		p.advance()
		return ""
	}
	field := p.name()
	if p.is(":") {
		p.advance()
		field = p.name()
	}
	field += p.arguments() + p.directives()
	if p.is("{") {
		field += p.selectionSet()
	}
	return field
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
	var s string
	for p.is("@") {
		p.advance()
		s += "@" + p.name() + p.arguments()
	}
	return s
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
func (p *gqlParser) typeRef() string {
	var t string
	if p.is("[") {
		p.advance()
		t = "[" + p.typeRef() + "]"
		if p.is("]") {
			p.advance()
		}
	} else {
		t = p.name()
	}
	if p.is("!") {
		p.advance()
		t += "!"
	}
	return t
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

// skipSelectionSet reads past a selection set nested deeper than
// gqlMaxDepth, without recursion.
func (p *gqlParser) skipSelectionSet() {
	depth := 0
	for p.tok.kind != gqlEOF {
		switch {
		case p.is("{"):
			depth++
		case p.is("}"):
			depth--
		}
		p.advance()
		if depth == 0 {
			return
		}
	}
}
