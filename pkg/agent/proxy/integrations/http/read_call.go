package http

import (
	"bytes"
	"encoding/json"
	"mime"
	"net/http"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// A call is a read when HTTP says it changes nothing at the server: a safe
// method (RFC 9110 §9.2.1: GET, HEAD, OPTIONS, TRACE), or a GraphQL request
// whose operation is a query (GraphQL: "query – a read-only fetch"; over
// HTTP, a POST whose JSON body's "query" is the document, and the operation
// it runs is the one "operationName" names, or its only one). Every other
// call is a write: another method, another body, and a GraphQL document
// keploy cannot read. Nothing is decided by a name (an operation called
// "GetX" is not a read for that): only the method and the parsed document.
func init() {
	models.RegisterReadCall(models.HTTP, recordedRead)
}

// recordedRead reports whether the HTTP call m recorded is a read.
func recordedRead(m *models.Mock) bool {
	if m == nil || m.Kind != models.HTTP || m.Spec.HTTPReq == nil {
		return false
	}
	r := m.Spec.HTTPReq
	return readCall(string(r.Method), headerValue(r.Header, "Content-Type"), []byte(r.Body))
}

// readCall reports whether a call of method, with a body of contentType, is a
// read.
func readCall(method, contentType string, body []byte) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	case http.MethodPost:
		return graphQLQuery(contentType, body)
	}
	return false
}

// graphQLRequest is a GraphQL request's JSON body, as far as a read needs it.
type graphQLRequest struct {
	Query         *string `json:"query"`
	OperationName string  `json:"operationName"`
}

// graphQLQuery reports whether body is a GraphQL request that runs a query
// and nothing else: one request, or a batch of them that are all queries.
func graphQLQuery(contentType string, body []byte) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	if mt == "application/graphql" {
		return operationType(string(body), "") == "query"
	}
	if mt != "application/json" && !strings.HasSuffix(mt, "+json") {
		return false
	}
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' {
		var batch []graphQLRequest
		if json.Unmarshal(body, &batch) != nil || len(batch) == 0 {
			return false
		}
		for _, r := range batch {
			if !r.query() {
				return false
			}
		}
		return true
	}
	var one graphQLRequest
	if json.Unmarshal(body, &one) != nil {
		return false
	}
	return one.query()
}

// query reports whether r runs a query.
func (r graphQLRequest) query() bool {
	return r.Query != nil && operationType(*r.Query, r.OperationName) == "query"
}

// operationType is the type ("query", "mutation" or "subscription") of the
// operation a GraphQL document runs: the one name names, or the document's
// only operation when name is empty. It is "" for a document keploy cannot
// read: an unclosed brace, parenthesis or string, a type system definition,
// no such operation, or several with no name given.
func operationType(doc, name string) string {
	type operation struct{ kind, name string }
	var (
		ops    []operation
		depth  int    // braces open
		parens int    // parentheses open at the top level
		def    string // the top-level definition being read: "", "operation" or "fragment"
		kind   string // the operation's type, while def is "operation"
		opName string
		named  bool // the operation's name, if any, has been read
	)
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
				return ""
			}
			i = end
		case c == '{':
			if depth == 0 {
				switch def {
				case "":
					ops = append(ops, operation{kind: "query"})
				case "operation":
					ops = append(ops, operation{kind: kind, name: opName})
				}
				def, kind, opName, named = "", "", "", false
			}
			depth++
			i++
		case c == '}':
			depth--
			if depth < 0 {
				return ""
			}
			i++
		case c == '(' && depth == 0:
			parens++
			named = true
			i++
		case c == ')' && depth == 0:
			parens--
			if parens < 0 {
				return ""
			}
			i++
		case isNameStart(c):
			j := i + 1
			for j < len(doc) && isNameContinue(doc[j]) {
				j++
			}
			word := doc[i:j]
			i = j
			if depth > 0 || parens > 0 {
				continue
			}
			switch {
			case def == "" && (word == "query" || word == "mutation" || word == "subscription"):
				def, kind = "operation", word
			case def == "" && word == "fragment":
				def = "fragment"
			case def == "":
				// A type system definition or extension: not a request.
				return ""
			case def == "operation" && !named:
				opName, named = word, true
			}
		default:
			if c == '@' || c == '$' {
				named = named || def == "operation"
			}
			i++
		}
	}
	if depth != 0 || parens != 0 || def != "" {
		return ""
	}
	if name != "" {
		for _, op := range ops {
			if op.name == name {
				return op.kind
			}
		}
		return ""
	}
	if len(ops) != 1 {
		return ""
	}
	return ops[0].kind
}

// stringEnd is the index just past the GraphQL string that starts at
// doc[start] (a quote): a block string ("""...""") or a string on one line;
// -1 when it is not closed.
func stringEnd(doc string, start int) int {
	if strings.HasPrefix(doc[start:], `"""`) {
		for i := start + 3; i+2 < len(doc); i++ {
			if doc[i] == '\\' && strings.HasPrefix(doc[i+1:], `"""`) {
				i += 3
				continue
			}
			if strings.HasPrefix(doc[i:], `"""`) {
				return i + 3
			}
		}
		return -1
	}
	for i := start + 1; i < len(doc); i++ {
		switch doc[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		case '\n', '\r':
			return -1
		}
	}
	return -1
}

// isNameStart reports whether c can begin a GraphQL name.
func isNameStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// isNameContinue reports whether c can continue a GraphQL name.
func isNameContinue(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}
