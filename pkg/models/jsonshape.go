package models

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// A JSON body's shape (shapeNode) and what a request's JSON body says of the
// GraphQL or JSON-RPC operation it runs (jsonRequest), read in one pass by
// jsonScanner.

// jsonRole is what a JSON value is to the operation a request runs.
type jsonRole uint8

const (
	roleNone       jsonRole = iota
	roleEnvelope            // a GraphQL or JSON-RPC request envelope
	roleBatch               // an array of envelopes
	roleExtensions          // an envelope's "extensions"
	rolePersisted           // its "persistedQuery"
)

// jsonEnvelope is what one request envelope holds of the members
// requestOperation reads, as encoding/json would decode them into a map: the
// last of a repeated member wins.
type jsonEnvelope struct {
	object    bool       // the envelope is an object
	queryType shapeTypes // the "query" member's type, 0 when absent
	query     string     // its text, when a string
	opName    string     // "operationName", when a string
	jsonrpc   bool       // "jsonrpc" is a string
	hasMethod bool       // "method" is a string
	method    string
	persisted string // extensions.persistedQuery.sha256Hash, when a string
}

// member applies an envelope's member, of type t and text s (a string's), to
// what the envelope holds, and is the role of the member's value when it is
// one requestOperation reads into.
func (e *jsonEnvelope) member(parent jsonRole, key string, t shapeTypes, s string) jsonRole {
	str := func() string {
		if t == shapeString {
			return s
		}
		return ""
	}
	switch parent {
	case roleEnvelope:
		switch key {
		case "query":
			e.queryType, e.query = t, str()
		case "operationName":
			e.opName = str()
		case "jsonrpc":
			e.jsonrpc = t == shapeString
		case "method":
			e.hasMethod, e.method = t == shapeString, str()
		case "extensions":
			e.persisted = ""
			if t == shapeObject {
				return roleExtensions
			}
		}
	case roleExtensions:
		if key == "persistedQuery" {
			e.persisted = ""
			if t == shapeObject {
				return rolePersisted
			}
		}
	case rolePersisted:
		if key == "sha256Hash" {
			e.persisted = str()
		}
	}
	return roleNone
}

// jsonRequest is what a JSON request body says of the operation it runs: the
// distinct GraphQL operations and JSON-RPC methods of its envelopes (the body
// itself when it is an object, each item when it is an array).
type jsonRequest struct {
	batch      bool // the body is an array
	envelopes  int
	graphQL    map[string]struct{}
	jsonRPC    map[string]struct{} // each method quoted (strconv.Quote)
	notGraphQL bool                // an envelope runs no GraphQL operation
	notJSONRPC bool                // an envelope is no JSON-RPC request
}

func (r *jsonRequest) add(e *jsonEnvelope) {
	r.envelopes++
	if op := graphQLEnvelopeOperation(e); op != "" {
		r.graphQL[op] = struct{}{}
	} else {
		r.notGraphQL = true
	}
	if e.object && e.jsonrpc && e.hasMethod {
		r.jsonRPC[strconv.Quote(e.method)] = struct{}{}
	} else {
		r.notJSONRPC = true
	}
}

// jsonFrame is an open JSON object or array.
type jsonFrame struct {
	node  *shapeNode
	array bool
	role  jsonRole
}

// jsonScanner reads a JSON document into its shape in one pass, without
// recursion and without encoding/json's depth limit (10000, which its
// Decoder.Token enforces too), so a body of any depth has its full shape. It
// checks the document is exactly one JSON value (RFC 8259, as encoding/json
// reads it), decodes the member names and the strings requestOperation
// reads, and only checks every other value.
type jsonScanner struct {
	rootRole jsonRole
	tree     *shapeNode
	req      *jsonRequest
	env      jsonEnvelope // the envelope being read (envelopes never nest)
	stack    []jsonFrame
	key      string // the member whose value comes next
}

// scanJSON is a JSON document's shape and, when rootRole is roleEnvelope,
// what it says of the operation it runs. It is false when the body is not
// exactly one JSON value.
func scanJSON(body string, rootRole jsonRole) (*shapeNode, *jsonRequest, bool) {
	s := &jsonScanner{rootRole: rootRole, tree: &shapeNode{}}
	if rootRole == roleEnvelope {
		s.req = &jsonRequest{graphQL: map[string]struct{}{}, jsonRPC: map[string]struct{}{}}
	}
	if !s.scan(body) {
		return nil, nil, false
	}
	return s.tree, s.req, true
}

// What scan reads next.
const (
	jsonWantValue      = iota // a value: the root, an array's item, a member's value
	jsonWantItem              // after '[': an item or ']'
	jsonWantFirstKey          // after '{': a member's name or '}'
	jsonWantKey               // after ',' in an object: a member's name
	jsonWantColon             // after a member's name
	jsonWantCommaOrEnd        // after a value in an array or an object
	jsonDone                  // after the root value: nothing but whitespace
)

// scan reads b; a member name or a string it decodes is a part of b, unless
// it holds an escape, so a name costs no copy.
func (s *jsonScanner) scan(b string) bool {
	i, state := 0, jsonWantValue
	for {
		for i < len(b) && isJSONSpace(b[i]) {
			i++
		}
		if state == jsonDone {
			return i == len(b)
		}
		if i == len(b) {
			return false
		}
		c := b[i]
		switch state {
		case jsonWantItem, jsonWantValue:
			if state == jsonWantItem && c == ']' {
				i++
				state = s.close()
				continue
			}
			var ok bool
			if i, ok = s.readValue(b, i); !ok {
				return false
			}
			switch c {
			case '{':
				state = jsonWantFirstKey
			case '[':
				state = jsonWantItem
			default:
				state = s.after()
			}
		case jsonWantFirstKey, jsonWantKey:
			if state == jsonWantFirstKey && c == '}' {
				i++
				state = s.close()
				continue
			}
			if c != '"' {
				return false
			}
			key, end, ok := readJSONString(b, i, true)
			if !ok {
				return false
			}
			s.key, i, state = key, end, jsonWantColon
		case jsonWantColon:
			if c != ':' {
				return false
			}
			i++
			state = jsonWantValue
		case jsonWantCommaOrEnd:
			top := &s.stack[len(s.stack)-1]
			switch {
			case c == ',' && top.array:
				i, state = i+1, jsonWantValue
			case c == ',':
				i, state = i+1, jsonWantKey
			case (c == ']' && top.array) || (c == '}' && !top.array):
				i++
				state = s.close()
			default:
				return false
			}
		}
	}
}

// readValue reads the value that starts at b[i], opening it when it is an
// object or an array, and returns the position after what it read.
func (s *jsonScanner) readValue(b string, i int) (int, bool) {
	switch c := b[i]; {
	case c == '{':
		s.open(shapeObject)
		return i + 1, true
	case c == '[':
		s.open(shapeArray)
		return i + 1, true
	case c == '"':
		text, end, ok := readJSONString(b, i, s.readsText())
		if ok {
			s.begin(shapeString, text)
		}
		return end, ok
	case c == 't':
		return s.literal(b, i, "true", shapeBool)
	case c == 'f':
		return s.literal(b, i, "false", shapeBool)
	case c == 'n':
		return s.literal(b, i, "null", shapeNull)
	case c == '-' || isDigitByte(c):
		end, ok := readJSONNumber(b, i)
		if ok {
			s.begin(shapeNumber, "")
		}
		return end, ok
	}
	return i, false
}

func (s *jsonScanner) literal(b string, i int, word string, t shapeTypes) (int, bool) {
	if !strings.HasPrefix(b[i:], word) {
		return i, false
	}
	s.begin(t, "")
	return i + len(word), true
}

// readsText reports whether the string that comes next is one
// requestOperation reads, so its text is decoded.
func (s *jsonScanner) readsText() bool {
	return len(s.stack) > 0 && s.stack[len(s.stack)-1].role != roleNone
}

// after is what comes after a value: the end of the document, or more of the
// container it is in.
func (s *jsonScanner) after() int {
	if len(s.stack) == 0 {
		return jsonDone
	}
	return jsonWantCommaOrEnd
}

// begin places a value of type t (text str, a string's) in its parent and is
// its node and the role it has.
func (s *jsonScanner) begin(t shapeTypes, str string) (*shapeNode, jsonRole) {
	var parent *jsonFrame
	var n *shapeNode
	switch {
	case len(s.stack) == 0:
		n = s.tree
	default:
		parent = &s.stack[len(s.stack)-1]
		if parent.array {
			n = parent.node.item()
		} else {
			n = parent.node.field(s.key)
		}
	}
	n.types |= t
	role := roleNone
	switch {
	case parent == nil:
		switch {
		case t == shapeObject:
			role = s.rootRole // an envelope, a GET's extensions, or nothing
			s.env = jsonEnvelope{object: true}
		case t == shapeArray && s.rootRole == roleEnvelope:
			role = roleBatch
			s.req.batch = true
		}
	case parent.role == roleBatch:
		if t == shapeObject {
			role = roleEnvelope
			s.env = jsonEnvelope{object: true}
		} else {
			s.req.add(&jsonEnvelope{}) // an item that is no object is no request
		}
	case parent.role != roleNone:
		role = s.env.member(parent.role, s.key, t, str)
	}
	return n, role
}

func (s *jsonScanner) open(t shapeTypes) {
	n, role := s.begin(t, "")
	s.stack = append(s.stack, jsonFrame{node: n, array: t == shapeArray, role: role})
}

// close closes the innermost open object or array and is what comes next.
func (s *jsonScanner) close() int {
	f := s.stack[len(s.stack)-1]
	s.stack = s.stack[:len(s.stack)-1]
	if f.role == roleEnvelope && s.req != nil {
		s.req.add(&s.env)
	}
	return s.after()
}

func isJSONSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// readJSONString reads the string literal at b[i] and returns its text, when
// decode is set, and the position after it. Its text keeps a byte that is
// not valid UTF-8 as it is, where encoding/json writes U+FFFD, so two member
// names that differ only there stay apart; a lone surrogate escape is U+FFFD,
// as there.
func readJSONString(b string, i int, decode bool) (string, int, bool) {
	i++ // "
	start, plain := i, true
	for i < len(b) {
		switch c := b[i]; {
		case c == '"':
			if !decode {
				return "", i + 1, true
			}
			if plain {
				return b[start:i], i + 1, true
			}
			return decodeJSONString(b[start:i]), i + 1, true
		case c == '\\':
			plain = false
			if i+1 >= len(b) {
				return "", i, false
			}
			switch b[i+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				i += 2
			case 'u':
				if i+6 > len(b) || !isHex4(b[i+2:i+6]) {
					return "", i, false
				}
				i += 6
			default:
				return "", i, false
			}
		case c < 0x20:
			return "", i, false
		default:
			i++
		}
	}
	return "", i, false
}

// decodeJSONString is the text of a string literal's content, which
// readJSONString has checked.
func decodeJSONString(raw string) string {
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); {
		if raw[i] != '\\' {
			out = append(out, raw[i])
			i++
			continue
		}
		switch e := raw[i+1]; e {
		case 'u':
			r := rune(hex4(raw[i+2 : i+6]))
			i += 6
			if utf16.IsSurrogate(r) {
				r2 := unicode.ReplacementChar
				if i+6 <= len(raw) && raw[i] == '\\' && raw[i+1] == 'u' {
					r2 = rune(hex4(raw[i+2 : i+6]))
				}
				if dec := utf16.DecodeRune(r, r2); dec != unicode.ReplacementChar {
					r = dec
					i += 6
				} else {
					r = unicode.ReplacementChar
				}
			}
			out = utf8.AppendRune(out, r)
		default:
			out = append(out, jsonEscapes[e])
			i += 2
		}
	}
	return string(out)
}

// jsonEscapes is the byte each one-letter escape stands for.
var jsonEscapes = [256]byte{'"': '"', '\\': '\\', '/': '/', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t'}

func isHex4(b string) bool {
	for _, c := range []byte(b) {
		if !isDigitByte(c) && !strings.ContainsRune("abcdefABCDEF", rune(c)) {
			return false
		}
	}
	return len(b) == 4
}

func hex4(b string) uint16 {
	var v uint16
	for _, c := range []byte(b) {
		v <<= 4
		switch {
		case isDigitByte(c):
			v |= uint16(c - '0')
		case c >= 'a':
			v |= uint16(c - 'a' + 10)
		default:
			v |= uint16(c - 'A' + 10)
		}
	}
	return v
}

// readJSONNumber reads the number at b[i] and returns the position after it:
// '-'? ('0' | [1-9][0-9]*) ('.' [0-9]+)? ([eE] [+-]? [0-9]+)?, of any size.
func readJSONNumber(b string, i int) (int, bool) {
	digits := func() bool {
		start := i
		for i < len(b) && isDigitByte(b[i]) {
			i++
		}
		return i > start
	}
	if b[i] == '-' {
		i++
	}
	switch {
	case i < len(b) && b[i] == '0':
		i++
	case !digits():
		return i, false
	}
	if i < len(b) && b[i] == '.' {
		i++
		if !digits() {
			return i, false
		}
	}
	if i < len(b) && (b[i] == 'e' || b[i] == 'E') {
		i++
		if i < len(b) && (b[i] == '+' || b[i] == '-') {
			i++
		}
		if !digits() {
			return i, false
		}
	}
	return i, true
}
