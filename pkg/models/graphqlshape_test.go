package models

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func gqlCase(query, operationName string) *TestCase {
	env := map[string]any{"query": query}
	if operationName != "" {
		env["operationName"] = operationName
	}
	b, _ := json.Marshal(env)
	return httpCase("POST", "/graphql", 200, "application/json", string(b), "application/json", `{"data":{}}`)
}

func TestGraphQLOperationShape(t *testing.T) {
	sameShape(t, "argument values",
		gqlCase(`{ user(id: 1) { name } }`, ""), gqlCase(`{ user(id: 2) { name } }`, ""))
	sameShape(t, "string literals and their whitespace",
		gqlCase(`{ search(q: "a  b") { id } }`, ""), gqlCase(`{ search(q: "c") { id } }`, ""))
	sameShape(t, "selection order",
		gqlCase(`{ user { name email } }`, ""), gqlCase(`{ user { email name } }`, ""))
	sameShape(t, "argument order",
		gqlCase(`{ users(first: 10, after: "x") { id } }`, ""), gqlCase(`{ users(after: "y", first: 2) { id } }`, ""))
	sameShape(t, "aliases, commas, comments and whitespace",
		gqlCase("query Q {\n  u: user(id: 1) { name, email } # who\n}", ""), gqlCase(`query Q { user(id: 3) { email name } }`, ""))
	sameShape(t, "variable default values",
		gqlCase(`query Q($n: Int = 5) { users(first: $n) { id } }`, ""), gqlCase(`query Q($n: Int = 9) { users(first: $n) { id } }`, ""))
	sameShape(t, "object and list argument values",
		gqlCase(`mutation { add(input: {a: 1, b: [1, 2]}) { id } }`, ""), gqlCase(`mutation { add(input: {c: "x"}) { id } }`, ""))
	differentShape(t, "selected fields",
		gqlCase(`{ user { name } }`, ""), gqlCase(`{ user { name email } }`, ""))
	differentShape(t, "operations",
		gqlCase(`query { user { id } }`, ""), gqlCase(`mutation { user { id } }`, ""))
	differentShape(t, "argument names",
		gqlCase(`{ user(id: 1) { id } }`, ""), gqlCase(`{ user(email: "a") { id } }`, ""))
	differentShape(t, "variable types",
		gqlCase(`query ($id: ID!) { user(id: $id) { id } }`, ""), gqlCase(`query ($id: String) { user(id: $id) { id } }`, ""))
	differentShape(t, "operation name",
		gqlCase(`query A { a } query B { b }`, "A"), gqlCase(`query A { a } query B { b }`, "B"))
	differentShape(t, "fragments",
		gqlCase(`{ user { ...F } } fragment F on User { name }`, ""), gqlCase(`{ user { ...F } } fragment F on User { email }`, ""))
	sameShape(t, "definition order",
		gqlCase(`{ user { ...F } } fragment F on User { name }`, ""), gqlCase(`fragment F on User { name } { user { ...F } }`, ""))
	sameShape(t, "a block string",
		gqlCase(`{ note(text: """a "quoted" line""") { id } }`, ""), gqlCase(`{ note(text: "b") { id } }`, ""))
}

func TestGraphQLRequestForms(t *testing.T) {
	persisted := func(hash, name string) *TestCase {
		b, _ := json.Marshal(map[string]any{"operationName": name, "extensions": map[string]any{"persistedQuery": map[string]any{"version": 1, "sha256Hash": hash}}})
		return httpCase("POST", "/graphql", 200, "application/json", string(b), "application/json", `{"data":{}}`)
	}
	differentShape(t, "persisted queries", persisted("aaa", "GetUser"), persisted("bbb", "GetOrder"))

	get := func(q string) *TestCase {
		return httpCase("GET", "/graphql?query="+url.QueryEscape(q), 200, "", "", "application/json", `{"data":{}}`)
	}
	sameShape(t, "a GET query's values", get(`{ user(id: 1) { name } }`), get(`{ user(id: 7) { name } }`))
	differentShape(t, "a GET query's fields", get(`{ user { name } }`), get(`{ user { email } }`))

	raw := func(q string) *TestCase {
		return httpCase("POST", "/graphql", 200, "application/graphql", q, "application/json", `{"data":{}}`)
	}
	differentShape(t, "an application/graphql body", raw(`{ a }`), raw(`{ b }`))

	batch := func(qs ...string) *TestCase {
		var envs []map[string]any
		for _, q := range qs {
			envs = append(envs, map[string]any{"query": q})
		}
		b, _ := json.Marshal(envs)
		return httpCase("POST", "/graphql", 200, "application/json", string(b), "application/json", `[]`)
	}
	sameShape(t, "a batch's order", batch(`{ a }`, `{ b }`), batch(`{ b }`, `{ a }`))
	differentShape(t, "a batch's operations", batch(`{ a }`, `{ b }`), batch(`{ a }`, `{ c }`))
}

func TestLooksLikeGraphQL(t *testing.T) {
	for doc, want := range map[string]bool{
		"{ a }":                    true,
		"  # c\nquery Q { a }":     true,
		"mutation { a }":           true,
		"fragment F on T { a }":    true,
		"queryString=foo":          false,
		"shoes size 9":             false,
		"":                         false,
		"\xef\xbb\xbfsubscription": true,
	} {
		if got := looksLikeGraphQL(doc); got != want {
			t.Errorf("looksLikeGraphQL(%q) = %v, want %v", doc, got, want)
		}
	}
	// A search API's "query" string field is no GraphQL operation.
	s, _ := ShapeOf(gqlCase("red shoes", ""))
	if s.Operation != "" {
		t.Fatalf("operation %q for a search query, want none", s.Operation)
	}
}

func TestGraphQLOperationOfMalformedDocuments(t *testing.T) {
	// A malformed document still gives a form, the same each time, and the
	// parser stops at the end of the text.
	for _, doc := range []string{`{ a(`, `{ a { b `, `query ($x: [ { a }`, `{ ...`, `{ a(x: {y: [1, {z: "`, `{ @ }`} {
		first := graphQLOperation(doc, "")
		if again := graphQLOperation(doc, ""); again != first {
			t.Errorf("graphQLOperation(%q) gave %q, then %q", doc, first, again)
		}
	}
}

// FuzzGraphQLOperation checks that any document gives one form, without a
// panic or a loop.
func FuzzGraphQLOperation(f *testing.F) {
	for _, s := range []string{`{ a(x: 1) { b ...F } } fragment F on T { c }`, `query Q($a: [Int!]! = [1]) @d { a }`, `{ a(x: {y: [1, {z: """q"""}]}) }`, `{`, `"`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, doc string) {
		first := graphQLOperation(doc, "")
		if again := graphQLOperation(doc, ""); again != first {
			t.Fatalf("the form differs between two reads: %q, then %q", first, again)
		}
	})
}

func TestGraphQLOperationOfADeepDocument(t *testing.T) {
	deep := strings.Repeat("{ a ", 1<<20) + strings.Repeat("}", 1<<20)
	if op := graphQLOperation(deep, ""); !strings.Contains(op, "{…}") {
		t.Fatalf("a document nested past gqlMaxDepth: %.80q…, want its deep sets written {…}", op)
	}
	deepValue := "{ a(x: " + strings.Repeat("[", 1<<20) + strings.Repeat("]", 1<<20) + ") }"
	if op := graphQLOperation(deepValue, ""); op != "graphql:query {a(x)}" {
		t.Fatalf("a deep argument value: %q", op)
	}
}
