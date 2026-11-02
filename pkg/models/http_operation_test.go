package models

import (
	"strings"
	"testing"
)

// readLabels registers, for the test, a reader that gives the label labels
// has for a request's body, and reads no operation in any other.
func readLabels(t *testing.T, labels map[string]string) {
	t.Helper()
	unregister := RegisterHTTPOperation(func(r *HTTPReq) (HTTPOperation, bool) {
		l, ok := labels[r.Body]
		return HTTPOperation{Label: l}, ok
	})
	t.Cleanup(unregister)
}

func gqlMockOf(body string) *Mock {
	return &Mock{Kind: HTTP, Spec: MockSpec{HTTPReq: &HTTPReq{Method: "POST", URL: "http://localhost:8083/query", Body: body}}}
}

// With no reader registered, nothing is read in a body: no call carries an
// operation, and a line names a call by its summary alone.
func TestNoOperationReaderReadsNoOperation(t *testing.T) {
	m := gqlMockOf(`{"operationName":"A","query":"query A { a }"}`)
	if got := MockSummaryFromSpec(m); got != "POST http://localhost:8083/query" {
		t.Fatalf("summary %q", got)
	}
	if _, ok := HTTPOperationOf(m.Spec.HTTPReq); ok {
		t.Fatal("an operation read with no reader")
	}
	if got := MockOperation(m); got != "" {
		t.Fatalf("operation %q", got)
	}
	if got := (UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /query"}).Named(); got != "POST /query" {
		t.Fatalf("named %q", got)
	}
}

// A registered reader's label is a field beside the call: the summary and
// the shape stay what they are with no reader, and a line names the call
// with its label after it. A label the reader gives that could hold a
// value, or a request it reads none in, names no operation.
func TestAnOperationIsAFieldBesideItsCall(t *testing.T) {
	readLabels(t, map[string]string{
		"named":    "query GetSubscription",
		"anon":     "query {getAllGenJobs}",
		"quoted":   `query "x"`,
		"emptylbl": "",
	})
	for _, tc := range []struct{ body, label string }{
		{"named", "query GetSubscription"},
		{"anon", "query {getAllGenJobs}"},
		{"quoted", ""},
		{"emptylbl", ""},
		{"other", ""},
	} {
		m := gqlMockOf(tc.body)
		if got := MockSummaryFromSpec(m); got != "POST http://localhost:8083/query" {
			t.Fatalf("%s: summary %q", tc.body, got)
		}
		if got := CallShape("HTTP", MockSummaryFromSpec(m)); got != "POST /query" {
			t.Fatalf("%s: shape %q", tc.body, got)
		}
		if got := MockOperation(m); got != tc.label {
			t.Fatalf("%s: operation %q; want %q", tc.body, got, tc.label)
		}
	}
	if _, ok := HTTPOperationOf(nil); ok {
		t.Fatal("an operation read in no request")
	}
	if got := MockOperation(nil); got != "" {
		t.Fatalf("no mock: operation %q", got)
	}
	if got := MockOperation(&Mock{Kind: Postgres}); got != "" {
		t.Fatalf("a mock with no HTTP request: operation %q", got)
	}
	u := UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /apps/123/query", Operation: "query GetApp", VariablePlaces: []string{"url.path[1]"}}
	if got := u.Named(); got != "POST /apps/123/query {query GetApp}" {
		t.Fatalf("named %q", got)
	}
	// Evidence keeps the operation as its own field; the shape is the
	// call's alone.
	if s := u.Sanitized(); s.ActualSummary != "POST /apps/{}/query" || s.Operation != "query GetApp" {
		t.Fatalf("sanitized summary %q operation %q", s.ActualSummary, s.Operation)
	}
	if got := MissShape(u); got != "POST /apps/{}/query" {
		t.Fatalf("miss shape %q", got)
	}
	// A call decoded from elsewhere whose operation could hold a value
	// keeps none.
	for _, op := range []string{`query "4111111111111111"`, "query a/b", "query {x"} {
		if s := (UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /query", Operation: op}).Sanitized(); s.Operation != "" {
			t.Fatalf("sanitized operation %q from %q", s.Operation, op)
		}
	}
}

// A path that decodes to text shaped like a label is shaped as it is with no
// reader: its segments that look like values are written {}.
func TestAPathShapedLikeALabelIsShapedAsWithNoReader(t *testing.T) {
	summaries := []string{"GET /pay/x {card 4111111111111111}", "GET /users/u {ssn 123 45 6789}", "POST /query {query A}"}
	var before []string
	for _, s := range summaries {
		before = append(before, (UnmatchedCall{Protocol: "HTTP", ActualSummary: s}).Sanitized().ActualSummary)
	}
	readLabels(t, nil)
	for i, s := range summaries {
		got := (UnmatchedCall{Protocol: "HTTP", ActualSummary: s}).Sanitized()
		if got.ActualSummary != before[i] || got.Operation != "" {
			t.Fatalf("Sanitized(%q) = %q, operation %q; with no reader %q", s, got.ActualSummary, got.Operation, before[i])
		}
		if strings.Contains(got.ActualSummary, "4111") || strings.Contains(got.ActualSummary, "6789") {
			t.Fatalf("Sanitized(%q) keeps a value: %q", s, got.ActualSummary)
		}
	}
}

// A reader that says it read none names none, whatever label it gave.
func TestAnOperationTheReaderRefusesIsNotNamed(t *testing.T) {
	unregister := RegisterHTTPOperation(func(*HTTPReq) (HTTPOperation, bool) {
		return HTTPOperation{Label: "query A"}, false
	})
	t.Cleanup(unregister)
	if got := MockOperation(gqlMockOf("x")); got != "" {
		t.Fatalf("operation %q", got)
	}
}

// A label is names and the words around them: it holds no value.
func TestValidOperationLabel(t *testing.T) {
	for _, tc := range []struct {
		l  string
		ok bool
	}{
		{"query GetSubscription", true},
		{"mutation create_app2", true},
		{"query {a, b, c, ...}", true},
		{"batch: query A, mutation B", true},
		{"persisted query 0a1b2c3d4e5f", true},
		{"Query A", true},
		{"zquery A", true},
		{"Zquery A", true},
		{"", false},
		{"1query", false},
		{"_query", false},
		{"{query}", false},
		{"@query", false},
		{"[query", false},
		{"`query", false},
		{"query ", false},
		{"query {a", false},
		{"query }{", false},
		{"query a}", false},
		{`query "x"`, false},
		{"query a/b", false},
		{"query a-b", false},
		{"query a\nb", false},
		{"q" + strings.Repeat("a", maxOperationLabel-1), true},
		{"q" + strings.Repeat("a", maxOperationLabel), false},
	} {
		if got := validOperationLabel(tc.l); got != tc.ok {
			t.Fatalf("validOperationLabel(%q) = %v; want %v", tc.l, got, tc.ok)
		}
	}
}

// Inside a label, every byte but a letter, a digit, '_', ' ', ',', ':' and
// '.' (and braces that pair up) refuses it: ';', '=', '"', '/', a control
// byte or a non-ASCII byte could carry a value the request sent.
// Mutation: validOperationLabel allows one more byte.
func TestValidOperationLabelRefusesEveryOtherByte(t *testing.T) {
	const allowed = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_ ,:."
	for c := 0; c < 256; c++ {
		if c == '{' || c == '}' {
			continue // a lone brace does not pair up: TestValidOperationLabel
		}
		l := "query a" + string([]byte{byte(c)}) + "b"
		if want := strings.IndexByte(allowed, byte(c)) >= 0; validOperationLabel(l) != want {
			t.Fatalf("validOperationLabel(%q) = %v; want %v", l, !want, want)
		}
	}
}

// One reader: a nil one, or a second, is refused; once removed, none reads.
func TestRegisterHTTPOperation(t *testing.T) {
	mustPanic := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Fatalf("%s did not panic", name)
			}
		}()
		f()
	}
	mustPanic("a nil reader", func() { RegisterHTTPOperation(nil) })
	reader := func(*HTTPReq) (HTTPOperation, bool) { return HTTPOperation{Label: "query A"}, true }
	unregister := RegisterHTTPOperation(reader)
	mustPanic("a second reader", func() { RegisterHTTPOperation(reader) })
	if got := HTTPOperationLabel(&HTTPReq{}); got != "query A" {
		t.Fatalf("label %q", got)
	}
	unregister()
	if got := HTTPOperationLabel(&HTTPReq{}); got != "" {
		t.Fatalf("label after removal %q", got)
	}
	// A removal already made removes no later reader.
	later := RegisterHTTPOperation(reader)
	unregister()
	if got := HTTPOperationLabel(&HTTPReq{}); got != "query A" {
		t.Fatalf("a stale removal removed the later reader: label %q", got)
	}
	later()
}
