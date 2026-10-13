package learn

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// isWord reports whether c continues a token: an ASCII letter or digit.
func isWord(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// tokenAt reports whether v occurs in s at i as a token: with no letter or
// digit right before or right after it.
func tokenAt(s, v string, i int) bool {
	if i > 0 && isWord(s[i-1]) && isWord(v[0]) {
		return false
	}
	if j := i + len(v); j < len(s) && isWord(s[j]) && isWord(v[len(v)-1]) {
		return false
	}
	return true
}

// ContainsToken reports whether s holds v as a token: "/orders/42" holds
// "42", "order-42x" does not.
func ContainsToken(s, v string) bool {
	if v == "" {
		return false
	}
	for i := 0; i <= len(s)-len(v); {
		j := strings.Index(s[i:], v)
		if j < 0 {
			return false
		}
		if tokenAt(s, v, i+j) {
			return true
		}
		i += j + 1
	}
	return false
}

// ReplaceTokens replaces every token occurrence of v in s with w, and
// reports whether it replaced any.
func ReplaceTokens(s, v, w string) (string, bool) {
	if v == "" || v == w {
		return s, false
	}
	var b strings.Builder
	done, changed := 0, false
	for i := 0; i <= len(s)-len(v); {
		j := strings.Index(s[i:], v)
		if j < 0 {
			break
		}
		j += i
		if !tokenAt(s, v, j) {
			i = j + 1
			continue
		}
		b.WriteString(s[done:j])
		b.WriteString(w)
		done, i, changed = j+len(v), j+len(v), true
	}
	if !changed {
		return s, false
	}
	b.WriteString(s[done:])
	return b.String(), true
}

// RenderJSON renders a correlation into a JSON answer: at each leaf whose
// noise path is in at, it writes live where the recorded value was (the
// whole leaf, or a token of a string), keeping the leaf's own type and the
// rest of the document byte for byte. It reports whether it changed
// anything; a body that is not one JSON value comes back unchanged.
func RenderJSON(body string, at map[string]bool, recorded, live string) (string, bool) {
	if recorded == "" || recorded == live || len(at) == 0 {
		return body, false
	}
	type frame struct {
		obj       bool
		risk      string // the container's own path in the noise dialect
		key       string
		expectKey bool
	}
	type edit struct {
		start, end int
		text       string
	}
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	var stack []*frame
	var edits []edit
	// valueRisk is the noise-dialect path of the value the next token is.
	valueRisk := func() string {
		if len(stack) == 0 {
			return ""
		}
		top := stack[len(stack)-1]
		if !top.obj {
			return top.risk
		}
		if top.risk == "" {
			return top.key
		}
		return top.risk + "." + top.key
	}
	afterValue := func() {
		if len(stack) > 0 && stack[len(stack)-1].obj {
			stack[len(stack)-1].expectKey = true
		}
	}
	for {
		start := int(dec.InputOffset())
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return body, false
		}
		end := int(dec.InputOffset())
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{':
				stack = append(stack, &frame{obj: true, risk: valueRisk(), expectKey: true})
			case '[':
				r := valueRisk()
				if r == "" {
					r = "[]"
				} else {
					r += "[]"
				}
				stack = append(stack, &frame{risk: r})
			case '}', ']':
				if len(stack) == 0 {
					return body, false
				}
				stack = stack[:len(stack)-1]
				afterValue()
			}
			continue
		}
		if len(stack) > 0 {
			if top := stack[len(stack)-1]; top.obj && top.expectKey {
				k, _ := tok.(string)
				top.key, top.expectKey = k, false
				continue
			}
		}
		path := "body"
		if r := valueRisk(); r != "" {
			path = "body." + r
		}
		afterValue()
		if !at[path] {
			continue
		}
		raw := body[start:end]
		trimmed := strings.TrimLeft(raw, " \t\r\n,:")
		start += len(raw) - len(trimmed)
		switch t := tok.(type) {
		case string:
			s, ok := ReplaceTokens(t, recorded, live)
			if !ok {
				continue
			}
			var b bytes.Buffer
			enc := json.NewEncoder(&b)
			enc.SetEscapeHTML(false)
			if enc.Encode(s) != nil {
				continue
			}
			edits = append(edits, edit{start, end, strings.TrimSuffix(b.String(), "\n")})
		case json.Number:
			if t.String() == recorded && isJSONNumber(live) {
				edits = append(edits, edit{start, end, live})
			}
		case bool:
			if trimmed == recorded && (live == "true" || live == "false") {
				edits = append(edits, edit{start, end, live})
			}
		}
	}
	if len(edits) == 0 {
		return body, false
	}
	var b strings.Builder
	done := 0
	for _, e := range edits {
		b.WriteString(body[done:e.start])
		b.WriteString(e.text)
		done = e.end
	}
	b.WriteString(body[done:])
	return b.String(), true
}

// isJSONNumber reports whether s is a JSON number literal.
func isJSONNumber(s string) bool {
	var n json.Number
	return json.Unmarshal([]byte(s), &n) == nil && n.String() == s
}
