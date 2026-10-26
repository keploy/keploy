package http

import (
	"bytes"
	"encoding/json"
	"mime"
	"sort"
	"strings"
)

// graphQLRoot is what a GraphQL request's query selects at its root, each
// root field with its arguments ("branch(branchId=\"b-1\")"), an argument a
// variable reads given its value from the request's variables: the field a
// client's cache files the answer under (Apollo's root field and arguments).
// None for a request that is not one GraphQL query (a mutation, a batch, a
// document keploy cannot read), or whose query selects a fragment at its
// root, whose fields keploy does not read.
func graphQLRoot(contentType string, body []byte) []string {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil || (mt != "application/json" && !strings.HasSuffix(mt, "+json")) {
		return nil
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' {
		return nil
	}
	var r struct {
		Query         *string                    `json:"query"`
		OperationName string                     `json:"operationName"`
		Variables     map[string]json.RawMessage `json:"variables"`
	}
	if json.Unmarshal(body, &r) != nil || r.Query == nil || operationType(*r.Query, r.OperationName) != "query" {
		return nil
	}
	toks, ok := gqlTokens(*r.Query)
	if !ok {
		return nil
	}
	start := operationSelection(toks, r.OperationName)
	if start < 0 {
		return nil
	}
	return rootFields(toks, start, r.Variables)
}

// gqlToken is one GraphQL token: a punctuator ("{", "...", "$"), a name, a
// string (quotes kept) or a number.
type gqlToken struct {
	text string
	name bool
}

// gqlTokens are doc's tokens, without whitespace, commas and comments; ok is
// false for a string that is not closed.
func gqlTokens(doc string) ([]gqlToken, bool) {
	var out []gqlToken
	for i := 0; i < len(doc); {
		c := doc[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(doc) && doc[i] != '\n' && doc[i] != '\r' {
				i++
			}
		case c == '"':
			end := stringEnd(doc, i)
			if end < 0 {
				return nil, false
			}
			out = append(out, gqlToken{text: doc[i:end]})
			i = end
		case strings.HasPrefix(doc[i:], "..."):
			out = append(out, gqlToken{text: "..."})
			i += 3
		case isNameStart(c):
			j := i + 1
			for j < len(doc) && isNameContinue(doc[j]) {
				j++
			}
			out = append(out, gqlToken{text: doc[i:j], name: true})
			i = j
		case c == '-' || (c >= '0' && c <= '9'):
			j := i + 1
			for j < len(doc) && (doc[j] == '.' || doc[j] == 'e' || doc[j] == 'E' || doc[j] == '+' || doc[j] == '-' || (doc[j] >= '0' && doc[j] <= '9')) {
				j++
			}
			out = append(out, gqlToken{text: doc[i:j]})
			i = j
		default:
			out = append(out, gqlToken{text: string(c)})
			i++
		}
	}
	return out, true
}

// operationSelection is the index of the "{" that opens the selection set of
// the operation name names (the document's only one when name is empty);
// -1 when there is none, or several and no name.
func operationSelection(toks []gqlToken, name string) int {
	type op struct {
		name  string
		start int
	}
	var ops []op
	for i := 0; i < len(toks); {
		t := toks[i]
		switch {
		case t.text == "{":
			ops = append(ops, op{start: i}) // a query's shorthand
			i = skipBlock(toks, i)
		case t.name && (t.text == "query" || t.text == "mutation" || t.text == "subscription"):
			o := op{start: -1}
			j := i + 1
			if j < len(toks) && toks[j].name {
				o.name = toks[j].text
				j++
			}
			for depth := 0; j < len(toks); j++ {
				switch toks[j].text {
				case "(":
					depth++
				case ")":
					depth--
				}
				if depth == 0 && toks[j].text == "{" {
					o.start = j
					break
				}
			}
			if o.start < 0 {
				return -1
			}
			ops = append(ops, o)
			i = skipBlock(toks, o.start)
		case t.name && t.text == "fragment":
			j := i + 1
			for j < len(toks) && toks[j].text != "{" {
				j++
			}
			if j == len(toks) {
				return -1
			}
			i = skipBlock(toks, j)
		default:
			return -1
		}
	}
	for _, o := range ops {
		if name != "" && o.name == name {
			return o.start
		}
	}
	if name == "" && len(ops) == 1 {
		return ops[0].start
	}
	return -1
}

// skipBlock is the index just past the block that opens at toks[open] ("{",
// "(" or "["), or len(toks) when it is not closed.
func skipBlock(toks []gqlToken, open int) int {
	closer := map[string]string{"{": "}", "(": ")", "[": "]"}[toks[open].text]
	depth := 0
	for i := open; i < len(toks); i++ {
		switch toks[i].text {
		case toks[open].text:
			depth++
		case closer:
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(toks)
}

// rootFields are the fields the selection set that opens at toks[start]
// selects, each with its arguments, sorted; none when it selects a fragment
// there or keploy cannot read it.
func rootFields(toks []gqlToken, start int, vars map[string]json.RawMessage) []string {
	seen := map[string]bool{}
	var out []string
	i := start + 1
	for i < len(toks) && toks[i].text != "}" {
		if !toks[i].name {
			return nil // "..." (a fragment) or something keploy does not read
		}
		field := toks[i].text
		i++
		if i < len(toks) && toks[i].text == ":" { // an alias: the field is the next name
			if i+1 >= len(toks) || !toks[i+1].name {
				return nil
			}
			field = toks[i+1].text
			i += 2
		}
		var args []string
		if i < len(toks) && toks[i].text == "(" {
			end := skipBlock(toks, i)
			a, ok := fieldArgs(toks[i+1:end-1], vars)
			if !ok {
				return nil
			}
			args = a
			i = end
		}
		for i < len(toks) && toks[i].text == "@" { // directives
			i += 2
			if i < len(toks) && toks[i].text == "(" {
				i = skipBlock(toks, i)
			}
		}
		if i < len(toks) && toks[i].text == "{" {
			i = skipBlock(toks, i)
		}
		if field == "__typename" {
			continue
		}
		key := field
		if len(args) > 0 {
			sort.Strings(args)
			key += "(" + strings.Join(args, ",") + ")"
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	if i >= len(toks) {
		return nil
	}
	sort.Strings(out)
	return out
}

// fieldArgs are a field's arguments ("name=value"), a variable given its
// value from vars (compact JSON; null when the request gives none).
func fieldArgs(toks []gqlToken, vars map[string]json.RawMessage) ([]string, bool) {
	var out []string
	for i := 0; i < len(toks); {
		if !toks[i].name || i+1 >= len(toks) || toks[i+1].text != ":" {
			return nil, false
		}
		name := toks[i].text
		i += 2
		v, next, ok := argValue(toks, i, vars)
		if !ok {
			return nil, false
		}
		out = append(out, name+"="+v)
		i = next
	}
	return out, true
}

// argValue is the value that starts at toks[i] as text, and the index past
// it.
func argValue(toks []gqlToken, i int, vars map[string]json.RawMessage) (string, int, bool) {
	if i >= len(toks) {
		return "", i, false
	}
	switch t := toks[i]; {
	case t.text == "$":
		if i+1 >= len(toks) || !toks[i+1].name {
			return "", i, false
		}
		raw, ok := vars[toks[i+1].text]
		if !ok {
			return "null", i + 2, true
		}
		var b bytes.Buffer
		if json.Compact(&b, raw) != nil {
			return "", i, false
		}
		return b.String(), i + 2, true
	case t.text == "[" || t.text == "{":
		end := skipBlock(toks, i)
		var b strings.Builder
		for j := i; j < end; {
			if toks[j].text == "$" {
				v, next, ok := argValue(toks, j, vars)
				if !ok {
					return "", i, false
				}
				b.WriteString(v)
				j = next
				continue
			}
			b.WriteString(toks[j].text)
			if toks[j].name && j+1 < end && toks[j+1].name {
				b.WriteByte(' ')
			}
			j++
		}
		return b.String(), end, true
	default:
		return t.text, i + 1, true
	}
}
