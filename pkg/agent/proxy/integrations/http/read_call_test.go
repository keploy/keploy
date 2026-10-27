package http

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// A call is a read by HTTP's own terms only: a safe method, or a GraphQL
// request whose parsed operation is a query. A name decides nothing, and a
// document keploy cannot read is a write.
func TestACallIsAReadOnlyByItsMethodOrItsGraphQLOperation(t *testing.T) {
	const js = "application/json"
	for _, tc := range []struct {
		name, method, ctype, body string
		read                      bool
	}{
		{"a GET", "GET", "", "", true},
		{"a HEAD", "head", "", "", true},
		{"an OPTIONS preflight", "OPTIONS", "", "", true},
		{"a PUT", "PUT", js, `{"query":"query A { a }"}`, false},
		{"a DELETE", "DELETE", "", "", false},
		{"a PATCH", "PATCH", js, `{}`, false},
		{"a POST that is no GraphQL", "POST", js, `{"name":"x"}`, false},
		{"a GraphQL query", "POST", js, `{"operationName":"AppById","query":"query AppById($id: String!) { appById(id: $id) { id } }","variables":{"id":"a"}}`, true},
		{"a GraphQL mutation", "POST", js, `{"operationName":"CreateBranch","query":"mutation CreateBranch($i: In!) { createBranch(input: $i) { id } }"}`, false},
		{"a mutation named like a read", "POST", js, `{"operationName":"GetJobs","query":"mutation GetJobs { start }"}`, false},
		{"a query named like a write", "POST", js, `{"operationName":"DeleteX","query":"query DeleteX { x }"}`, true},
		{"a subscription", "POST", js, `{"query":"subscription S { s }"}`, false},
		{"a shorthand query", "POST", js, `{"query":"{ me { id } }"}`, true},
		{"a query after a fragment", "POST", js, `{"query":"fragment F on App { id }\nquery Q { app { ...F } }"}`, true},
		{"the operation operationName names", "POST", js, `{"operationName":"B","query":"query A { a } mutation B { b }"}`, false},
		{"the other one", "POST", js, `{"operationName":"A","query":"query A { a } mutation B { b }"}`, true},
		{"two operations and no name", "POST", js, `{"query":"query A { a } query B { b }"}`, false},
		{"a name no operation has", "POST", js, `{"operationName":"C","query":"query A { a }"}`, false},
		{"a mutation keyword in a comment", "POST", js, `{"query":"# mutation M { m }\nquery Q { q }"}`, true},
		{"braces in a string", "POST", js, `{"query":"query Q { a(s: \"} mutation M {\") }"}`, true},
		{"braces in a block string", "POST", js, `{"query":"query Q { a(s: \"\"\"} mutation {\"\"\") }"}`, true},
		{"a directive after the keyword", "POST", js, `{"query":"query @live { a }"}`, true},
		{"an unclosed brace", "POST", js, `{"query":"query Q { a"}`, false},
		{"an unclosed string", "POST", js, `{"query":"query Q { a(s: \"x) }"}`, false},
		{"a type definition", "POST", js, `{"query":"type T { a: Int }"}`, false},
		{"a keyword with no body", "POST", js, `{"query":"query Q"}`, false},
		{"no query field", "POST", js, `{"operationName":"Q"}`, false},
		{"a batch of queries", "POST", js, `[{"query":"query A { a }"},{"query":"{ b }"}]`, true},
		{"a batch with a mutation", "POST", js, `[{"query":"query A { a }"},{"query":"mutation B { b }"}]`, false},
		{"an empty batch", "POST", js, `[]`, false},
		{"a +json media type", "POST", "application/graphql+json; charset=utf-8", `{"query":"query A { a }"}`, true},
		{"an application/graphql body", "POST", "application/graphql", `query A { a }`, true},
		{"a form", "POST", "application/x-www-form-urlencoded", `query=query+A+%7B+a+%7D`, false},
		{"no content type", "POST", "", `{"query":"query A { a }"}`, false},
	} {
		require.Equal(t, tc.read, readCall(tc.method, tc.ctype, []byte(tc.body)), tc.name)
	}
}

// The HTTP parser declares a recording a read as it declares the live call
// one, whatever case the recording wrote its Content-Type in.
func TestTheHTTPParserDeclaresARecordedReadAsItsLiveCall(t *testing.T) {
	mock := func(method, ctype, body string) *models.Mock {
		return &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{
			Method: models.Method(method), Header: map[string]string{"content-type": ctype}, Body: body}}}
	}
	require.True(t, models.IsReadCall(mock("POST", "application/json", `{"query":"query A { a }"}`)))
	require.False(t, models.IsReadCall(mock("POST", "application/json", `{"query":"mutation A { a }"}`)))
	require.True(t, models.IsReadCall(mock("GET", "", "")))
	require.False(t, models.IsReadCall(&models.Mock{Kind: models.HTTP}), "a mock with no request is no read")
	require.False(t, models.IsReadCall(&models.Mock{Kind: models.GENERIC}), "only a parser of the kind declares its reads")
}
