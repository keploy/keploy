package grpc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

func EqualProtoscope(a, b string) (bool, string, error) {
	na, err := ParseProtoscope(a)
	if err != nil {
		return false, "", fmt.Errorf("parse A: %w", err)
	}
	nb, err := ParseProtoscope(b)
	if err != nil {
		return false, "", fmt.Errorf("parse B: %w", err)
	}
	ca := Canonicalize(na)
	cb := Canonicalize(nb)
	if reflect.DeepEqual(ca, cb) {
		return true, "", nil
	}
	ja := mustPrettyJSON(ca)
	jb := mustPrettyJSON(cb)
	return false, unifiedStringDiff(ja, jb), nil
}

func ParseProtoscope(s string) (any, error) {
	p := newParser(s)
	// top-level message: sequence of fieldNumber ":" value
	obj := map[string]any{}
	for {
		p.skipSpaceAndComments()
		if p.eof() {
			break
		}
		key, ok := p.readFieldNumber()
		if !ok {
			return nil, p.errHere("expected field number")
		}
		p.skipSpaceAndComments()
		if !p.consume(':') {
			return nil, p.errHere("expected ':' after field number")
		}
		p.skipSpaceAndComments()
		val, err := p.readValue()
		if err != nil {
			return nil, err
		}
		addMulti(obj, key, val)
	}
	return obj, nil
}

func Canonicalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = Canonicalize(vv)
		}
		return out
	case []any:
		tmp := make([]any, len(t))
		for i := range t {
			tmp[i] = Canonicalize(t[i])
		}
		sort.Slice(tmp, func(i, j int) bool {
			return mustCompactJSON(tmp[i]) < mustCompactJSON(tmp[j])
		})
		return tmp
	case json.Number, string, bool, nil:
		return t
	default:
		// normalize numeric ints to json.Number
		switch x := t.(type) {
		case int, int32, int64, uint, uint32, uint64, float64:
			return jsonNumber(x)
		}
		var out any
		b, _ := json.Marshal(t)
		_ = json.Unmarshal(b, &out)
		return out
	}
}

// ===================== Parser =====================

type parser struct {
	data []byte
	i    int
	line int
	col  int
}

func newParser(s string) *parser { return &parser{data: []byte(s), line: 1, col: 1} }

func (p *parser) eof() bool { return p.i >= len(p.data) }

func (p *parser) peek() rune {
	if p.eof() {
		return 0
	}
	r, _ := utf8.DecodeRune(p.data[p.i:])
	return r
}

func (p *parser) advance() rune {
	if p.eof() {
		return 0
	}
	r, w := utf8.DecodeRune(p.data[p.i:])
	p.i += w
	if r == '\n' {
		p.line++
		p.col = 1
	} else {
		p.col++
	}
	return r
}

func (p *parser) skipSpaceAndComments() {
loop:
	for !p.eof() {
		r := p.peek()
		if unicode.IsSpace(r) {
			p.advance()
			continue
		}
		// line comments: // ... or # ...
		if r == '/' && p.i+1 < len(p.data) && p.data[p.i+1] == '/' {
			p.i += 2
			p.col += 2
			for !p.eof() && p.advance() != '\n' {
			}
			continue
		}
		if r == '#' {
			for !p.eof() && p.advance() != '\n' {
			}
			continue
		}
		break loop
	}
}

func (p *parser) consume(ch rune) bool {
	if p.peek() == ch {
		p.advance()
		return true
	}
	return false
}

func (p *parser) readFieldNumber() (string, bool) {
	start := p.i
	for !p.eof() && unicode.IsDigit(p.peek()) {
		p.advance()
	}
	if p.i == start {
		return "", false
	}
	return string(p.data[start:p.i]), true
}

func (p *parser) readValue() (any, error) {
	p.skipSpaceAndComments()
	switch p.peek() {
	case '{':
		p.advance()
		inner := p.readUntilMatchingBrace()
		if inner == nil {
			return nil, p.errHere("unclosed '{'")
		}
		text := strings.TrimSpace(*inner)
		if text == "" {
			// empty block -> empty message/map
			return map[string]any{}, nil
		}
		// Decide: nested message (has "digits:" at top-level) vs list
		if hasTopLevelFieldColon(text) {
			return ParseProtoscope(text)
		}
		return parseList(text)
	case '"':
		return p.readQuoted()
	default:
		// number (with optional i32/i64) or bare identifier
		tok := p.readToken()
		if tok == "" {
			return nil, p.errHere("expected value")
		}
		// Try number (allow suffix i32/i64)
		if n, ok := tryParseNumberWithSuffix(tok); ok {
			return n, nil
		}
		// Bare identifier → keep as string
		return tok, nil
	}
}

func (p *parser) readToken() string {
	var b strings.Builder
	// optional sign
	if p.peek() == '+' || p.peek() == '-' {
		b.WriteRune(p.advance())
	}
	for !p.eof() {
		r := p.peek()
		// token terminators: space, brace, colon, quote, comment
		if unicode.IsSpace(r) || r == '{' || r == '}' || r == ':' || r == '"' || r == '#' ||
			(r == '/' && p.i+1 < len(p.data) && p.data[p.i+1] == '/') {
			break
		}
		b.WriteRune(p.advance())
	}
	return b.String()
}

func (p *parser) readQuoted() (string, error) {
	if !p.consume('"') {
		return "", p.errHere(`expected '"'`)
	}
	var out strings.Builder
	escaped := false
	for !p.eof() {
		r := p.advance()
		if escaped {
			out.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == '"' {
			return out.String(), nil
		}
		out.WriteRune(r)
	}
	return "", p.errHere("unterminated string")
}

func (p *parser) readUntilMatchingBrace() *string {
	depth := 1
	start := p.i
	for !p.eof() {
		r := p.advance()
		if r == '"' { // skip quoted strings entirely
			// rewind one (we already advanced over the quote)
			p.i -= 1
			p.col -= 1
			if _, err := p.readQuoted(); err != nil {
				return nil
			}
			continue
		}
		if r == '{' {
			depth++
		} else if r == '}' {
			depth--
			if depth == 0 {
				s := string(p.data[start : p.i-1])
				return &s
			}
		}
	}
	return nil
}

func (p *parser) errHere(msg string) error {
	return fmt.Errorf("%s at line %d, col %d (byte %d)", msg, p.line, p.col, p.i)
}

// ===================== List & helpers =====================

func hasTopLevelFieldColon(s string) bool {
	depth := 0
	reader := bufio.NewReader(strings.NewReader(s))
	var i int
	for {
		r, w, err := readRune(reader)
		if err != nil {
			break
		}
		i += w
		switch r {
		case '"':
			// skip quoted
			txt := consumeQuoted(reader)
			i += len(txt) + 2
		case '{':
			depth++
		case '}':
			depth--
		case ':':
			if depth == 0 {
				// look back for digits
				k := i - 2
				for k >= 0 && unicode.IsSpace(rune(s[k])) {
					k--
				}
				did := false
				for k >= 0 && s[k] >= '0' && s[k] <= '9' {
					did = true
					k--
				}
				if did {
					return true
				}
			}
		}
	}
	return false
}

func parseList(s string) (any, error) {
	var items []any
	r := bufio.NewReader(strings.NewReader(s))
	for {
		skipSpacesAndCommentsReader(r)
		rn, _, err := r.ReadRune()
		if err != nil {
			break
		}
		if rn == '"' {
			// read quoted
			str := consumeQuoted(r)
			items = append(items, str)
			continue
		}
		if rn == '{' {
			// nested block within list: read until matching then decide message/list
			inner := readUntilMatchingBraceReader(r)
			if inner == "" {
				items = append(items, map[string]any{})
				continue
			}
			inner = strings.TrimSpace(inner)
			if hasTopLevelFieldColon(inner) {
				obj, err := ParseProtoscope(inner)
				if err != nil {
					return nil, err
				}
				items = append(items, obj)
			} else {
				v, err := parseList(inner)
				if err != nil {
					return nil, err
				}
				items = append(items, v)
			}
			continue
		}
		// token: number/ident possibly starting with +/-
		var tok strings.Builder
		tok.WriteRune(rn)
		for {
			r2, _, err2 := r.ReadRune()
			if err2 != nil {
				break
			}
			if unicode.IsSpace(rune(r2)) || r2 == '{' || r2 == '}' || r2 == '"' || r2 == '#' ||
				(r2 == '/' && peekSlash(r)) {
				r.UnreadRune()
				break
			}
			tok.WriteRune(r2)
		}
		word := tok.String()
		if n, ok := tryParseNumberWithSuffix(word); ok {
			items = append(items, n)
		} else {
			items = append(items, word)
		}
	}
	return items, nil
}

func tryParseNumberWithSuffix(tok string) (json.Number, bool) {
	// Strip trailing i32/i64 if present.
	raw := tok
	l := strings.ToLower(raw)
	if strings.HasSuffix(l, "i32") {
		raw = raw[:len(raw)-3]
	} else if strings.HasSuffix(l, "i64") {
		raw = raw[:len(raw)-3]
	}
	// Must be a valid Go float/int in decimal or scientific notation.
	if _, err := strconv.ParseFloat(raw, 64); err == nil {
		return json.Number(raw), true
	}
	// integers like "90"
	if _, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return json.Number(raw), true
	}
	if _, err := strconv.ParseUint(raw, 10, 64); err == nil {
		return json.Number(raw), true
	}
	return "", false
}

func addMulti(m map[string]any, k string, v any) {
	if old, ok := m[k]; ok {
		if sl, ok := old.([]any); ok {
			m[k] = append(sl, v)
			return
		}
		m[k] = []any{old, v}
		return
	}
	m[k] = v
}

// ===================== Small utilities =====================

func readRune(r *bufio.Reader) (rune, int, error) {
	b, err := r.ReadByte()
	if err != nil {
		return 0, 0, err
	}
	if b < 0x80 {
		return rune(b), 1, nil
	}
	_ = r.UnreadByte()
	ru, size, err := r.ReadRune()
	return ru, size, err
}

func consumeQuoted(r *bufio.Reader) string {
	var out strings.Builder
	escaped := false
	for {
		ch, _, err := r.ReadRune()
		if err != nil {
			break
		}
		if escaped {
			out.WriteRune(ch)
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			break
		}
		out.WriteRune(ch)
	}
	return out.String()
}

func readUntilMatchingBraceReader(r *bufio.Reader) string {
	depth := 1
	var out strings.Builder
	for {
		ch, _, err := r.ReadRune()
		if err != nil {
			break
		}
		if ch == '"' {
			out.WriteRune(ch)
			out.WriteString(consumeQuoted(r))
			out.WriteRune('"')
			continue
		}
		if ch == '{' {
			depth++
		} else if ch == '}' {
			depth--
			if depth == 0 {
				break
			}
		}
		out.WriteRune(ch)
	}
	return out.String()
}

func skipSpacesAndCommentsReader(r *bufio.Reader) {
	for {
		// peek
		b, err := r.Peek(1)
		if err != nil {
			return
		}
		if unicode.IsSpace(rune(b[0])) {
			r.ReadByte()
			continue
		}
		// // comment
		if b[0] == '/' {
			if bb, err := r.Peek(2); err == nil && len(bb) == 2 && bb[1] == '/' {
				// consume to EOL
				r.ReadByte()
				r.ReadByte()
				for {
					c, err := r.ReadByte()
					if err != nil || c == '\n' {
						break
					}
				}
				continue
			}
		}
		// # comment
		if b[0] == '#' {
			r.ReadByte()
			for {
				c, err := r.ReadByte()
				if err != nil || c == '\n' {
					break
				}
			}
			continue
		}
		return
	}
}

func peekSlash(r *bufio.Reader) bool {
	b, err := r.Peek(1)
	return err == nil && len(b) == 1 && b[0] == '/'
}

func mustCompactJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
func mustPrettyJSON(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

func jsonNumber(v any) json.Number {
	switch x := v.(type) {
	case int:
		return json.Number(strconv.FormatInt(int64(x), 10))
	case int32:
		return json.Number(strconv.FormatInt(int64(x), 10))
	case int64:
		return json.Number(strconv.FormatInt(x, 10))
	case uint:
		return json.Number(strconv.FormatUint(uint64(x), 10))
	case uint32:
		return json.Number(strconv.FormatUint(uint64(x), 10))
	case uint64:
		return json.Number(strconv.FormatUint(x, 10))
	case float64:
		// keep as-is; no forced formatting
		return json.Number(strconv.FormatFloat(x, 'g', -1, 64))
	default:
		return "0"
	}
}

// tiny unified diff for readability
func unifiedStringDiff(a, b string) string {
	if a == b {
		return ""
	}
	var buf bytes.Buffer
	buf.WriteString("--- A\n+++ B\n")
	la := strings.Split(a, "\n")
	lb := strings.Split(b, "\n")
	i, j := 0, 0
	for i < len(la) || j < len(lb) {
		var sa, sb string
		if i < len(la) {
			sa = la[i]
		}
		if j < len(lb) {
			sb = lb[j]
		}
		if sa == sb {
			i++
			j++
			continue
		}
		if i < len(la) {
			fmt.Fprintf(&buf, "-%s\n", sa)
			i++
		}
		if j < len(lb) {
			fmt.Fprintf(&buf, "+%s\n", sb)
			j++
		}
	}
	return buf.String()
}
