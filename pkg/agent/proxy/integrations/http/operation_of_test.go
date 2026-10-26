package http

import "testing"

// operationOf reads a GraphQL body's top-level operationName as its JSON
// reads it, and nothing for a body with none, two, or another shape.
func TestOperationOfReadsTheTopLevelName(t *testing.T) {
	for _, c := range []struct {
		body, want string
		ok         bool
	}{
		{`{"operationName":"GetBranches","variables":{"appId":"a-1"},"query":"query GetBranches { b }"}`, "GetBranches", true},
		{`{"variables":{"x":[1,{"y":2}]},"query":"q","operationName":"Late"}`, "Late", true},
		{`{"operationName":"Escaped"}`, "Escaped", true},
		{`{"variables":{"operationName":"inner"},"operationName":"Outer"}`, "", false},
		{`{"query":"{ a }"}`, "", false},
		{`{"operationName":null}`, "", false},
		{`[{"operationName":"Batch"}]`, "", false},
		{`not json`, "", false},
	} {
		if got, ok := operationOf(c.body); got != c.want || ok != c.ok {
			t.Errorf("%s: %q %v, want %q %v", c.body, got, ok, c.want, c.ok)
		}
	}
}
