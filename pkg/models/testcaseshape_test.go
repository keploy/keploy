package models

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
)

func httpCase(method, url string, status int, reqCT, reqBody, respCT, respBody string) *TestCase {
	return &TestCase{
		Kind:     HTTP,
		HTTPReq:  HTTPReq{Method: Method(method), URL: url, Header: map[string]string{"Content-Type": reqCT}, Body: reqBody},
		HTTPResp: HTTPResp{StatusCode: status, Header: map[string]string{"Content-Type": respCT}, Body: respBody},
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func jsonCase(url, respBody string) *TestCase {
	return httpCase("GET", url, 200, "", "", "application/json", respBody)
}

// sameShape and differentShape compare two test cases by their cover hashes.
func sameShape(t *testing.T, name string, a, b *TestCase) {
	t.Helper()
	if ha, hb := CoverHash(a), CoverHash(b); ha != hb {
		sa, _ := ShapeOf(a)
		sb, _ := ShapeOf(b)
		t.Errorf("%s: want one shape, got two:\n%s---\n%s", name, sa.Canonical(), sb.Canonical())
	}
}

func differentShape(t *testing.T, name string, a, b *TestCase) {
	t.Helper()
	if CoverHash(a) == CoverHash(b) {
		sa, _ := ShapeOf(a)
		t.Errorf("%s: want two shapes, got one:\n%s", name, sa.Canonical())
	}
}

func TestCoverHashFormat(t *testing.T) {
	h := CoverHash(jsonCase("http://svc/users/1", `{"id":1}`))
	if !regexp.MustCompile(`^c1:[0-9a-f]{32}$`).MatchString(h) {
		t.Fatalf("cover hash %q, want c1: and 32 hex digits", h)
	}
}

// TestCoverHashIsPinned pins one hash. Stores keep cover hashes, so a change
// to what a shape holds must come with a new CoverHashVersion, or stored
// hashes and new ones of one shape stop matching.
func TestCoverHashIsPinned(t *testing.T) {
	tc := httpCase("POST", "http://svc/users/42/orders?expand=items", 201,
		"application/json", `{"items":[{"sku":"a","qty":1}],"note":null}`,
		"application/json; charset=utf-8", `{"id":"o-1","total":12.5}`)
	s, _ := ShapeOf(tc)
	wantCanonical := strings.Join([]string{
		"kind http",
		"method POST",
		"route /users/{}/orders",
		"query expand",
		"status 201",
		"req.media application/json",
		"resp.media application/json",
		"operation ",
		"req $.items:a",
		"req $.items[].qty:n",
		"req $.items[].sku:s",
		"req $.items[]:o",
		"req $.note:z",
		"req $:o",
		"resp $.id:s",
		"resp $.total:n",
		"resp $:o",
	}, "\n") + "\n"
	if got := s.Canonical(); got != wantCanonical {
		t.Fatalf("canonical shape changed (bump CoverHashVersion if this is meant):\n%s\nwant:\n%s", got, wantCanonical)
	}
	if got, want := CoverHash(tc), "c1:"+sha256Hex(wantCanonical)[:32]; got != want {
		t.Fatalf("cover hash %s, want %s", got, want)
	}
}

func TestShapeKeyIsReadableAndShort(t *testing.T) {
	s, _ := ShapeOf(jsonCase("http://svc/users/7", `{"id":7}`))
	if !regexp.MustCompile(`^v2\|GET\|/users/\{\}\|200\|[0-9a-f]{16}$`).MatchString(s.Key()) {
		t.Fatalf("key %q, want v2|GET|/users/{}|200|<16 hex>", s.Key())
	}
}

func TestJSONBodyShape(t *testing.T) {
	sameShape(t, "values do not count",
		jsonCase("/a", `{"name":"x","age":3}`), jsonCase("/a", `{"name":"y","age":40}`))
	sameShape(t, "key order does not count",
		jsonCase("/a", `{"name":"x","age":3}`), jsonCase("/a", `{"age":3,"name":"x"}`))
	sameShape(t, "an integer and a fraction are one number type",
		jsonCase("/a", `{"price":10}`), jsonCase("/a", `{"price":10.5}`))
	differentShape(t, "swapped nested keys",
		jsonCase("/a", `{"user":{"data":{"id":1}}}`), jsonCase("/a", `{"data":{"user":{"id":1}}}`))
	differentShape(t, "a field only in a later array item",
		jsonCase("/a", `{"items":[{"id":1},{"id":2,"discount":{"pct":5}}]}`), jsonCase("/a", `{"items":[{"id":1}]}`))
	sameShape(t, "array items are a union, whatever their order",
		jsonCase("/a", `{"items":[{"id":1},{"id":2,"x":1}]}`), jsonCase("/a", `{"items":[{"id":2,"x":1},{"id":1}]}`))
	sameShape(t, "array length does not count",
		jsonCase("/a", `{"items":[{"id":1}]}`), jsonCase("/a", `{"items":[{"id":1},{"id":2},{"id":3}]}`))
	differentShape(t, "an array of arrays holds its items' fields",
		jsonCase("/a", `{"a":[[{"b":1}]]}`), jsonCase("/a", `{"a":[[{"z":"q"}]]}`))
	differentShape(t, "an empty array",
		jsonCase("/a", `{"c":[]}`), jsonCase("/a", `{"c":[1]}`))
	differentShape(t, "null is its own type",
		jsonCase("/a", `{"meta":null}`), jsonCase("/a", `{"meta":{"k":1}}`))
	differentShape(t, "an empty object and an empty body",
		jsonCase("/a", `{}`), jsonCase("/a", ``))
	differentShape(t, "a key with a dot and a nested key",
		jsonCase("/a", `{"a.b":1}`), jsonCase("/a", `{"a":{"b":1}}`))
	differentShape(t, "invalid JSON",
		jsonCase("/a", `{"a":`), jsonCase("/a", `{}`))
	sameShape(t, "an encrypted value keeps the shape of a string",
		jsonCase("/a", `{"token":"sk-live-1234"}`), jsonCase("/a", `{"token":"ENC:aes:k1:QUJD"}`))
}

func TestHTTPShapeParts(t *testing.T) {
	sameShape(t, "IDs in the path",
		jsonCase("http://svc/users/1", `{}`), jsonCase("http://svc/users/2", `{}`))
	differentShape(t, "two long static routes",
		jsonCase("/api/v1/notification-preferences", `{}`), jsonCase("/api/v1/organization-memberships", `{}`))
	sameShape(t, "query values do not count",
		jsonCase("/search?q=a", `{}`), jsonCase("/search?q=b", `{}`))
	sameShape(t, "query order does not count",
		jsonCase("/search?q=a&page=2", `{}`), jsonCase("/search?page=3&q=a", `{}`))
	differentShape(t, "query names count",
		jsonCase("/search?q=a", `{}`), jsonCase("/search?q=a&page=2", `{}`))
	differentShape(t, "status",
		httpCase("GET", "/a", 200, "", "", "application/json", `{}`), httpCase("GET", "/a", 404, "", "", "application/json", `{}`))
	differentShape(t, "method",
		httpCase("GET", "/a", 200, "", "", "", ""), httpCase("DELETE", "/a", 200, "", "", "", ""))
	differentShape(t, "media type",
		httpCase("GET", "/report", 200, "", "", "text/csv", "a,b"), httpCase("GET", "/report", 200, "", "", "application/pdf", "%PDF"))
	sameShape(t, "media type parameters do not count",
		httpCase("GET", "/a", 200, "", "", "application/json; charset=utf-8", `{}`), httpCase("GET", "/a", 200, "", "", "application/json", `{}`))
	sameShape(t, "an absolute URL and a request target",
		jsonCase("http://svc:8080/users/1?x=1", `{}`), jsonCase("/users/9?x=2", `{}`))

	a := jsonCase("/a", `{}`)
	b := jsonCase("/a", `{}`)
	a.HTTPReq.Header["Traceparent"] = "00-1"
	a.HTTPResp.Header["Content-Length"] = "2"
	b.HTTPResp.Header["Transfer-Encoding"] = "chunked"
	sameShape(t, "header names do not count", a, b)

	lower := jsonCase("/a", `{"x":1}`)
	lower.HTTPResp.Header = map[string]string{"content-type": "application/json"}
	sameShape(t, "Content-Type in any case", lower, jsonCase("/a", `{"x":2}`))
}

func TestFormAndXMLBodies(t *testing.T) {
	form := func(body string) *TestCase {
		return httpCase("POST", "/login", 200, "application/x-www-form-urlencoded", body, "", "")
	}
	differentShape(t, "form field names", form("user=a&pass=b"), form("user=a&otp=1"))
	sameShape(t, "form values", form("user=a&pass=b"), form("pass=c&user=d"))

	multipart := func(fields ...FormData) *TestCase {
		tc := httpCase("POST", "/upload", 200, "multipart/form-data; boundary=x", "", "", "")
		tc.HTTPReq.Form = fields
		return tc
	}
	differentShape(t, "multipart fields",
		multipart(FormData{Key: "name", Values: []string{"a"}}),
		multipart(FormData{Key: "name", Values: []string{"a"}}, FormData{Key: "file", Paths: []string{"/tmp/x"}}))
	differentShape(t, "a multipart file and a value",
		multipart(FormData{Key: "doc", Values: []string{"a"}}),
		multipart(FormData{Key: "doc", Paths: []string{"/tmp/x"}}))

	soap := func(op string) *TestCase {
		body := `<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body><` + op + ` id="1"><Name>x</Name></` + op + `></soap:Body></soap:Envelope>`
		return httpCase("POST", "/ws", 200, "text/xml; charset=utf-8", body, "text/xml", "")
	}
	differentShape(t, "SOAP operations on one URL", soap("GetUser"), soap("DeleteUser"))
	sameShape(t, "XML values", soap("GetUser"), func() *TestCase {
		tc := soap("GetUser")
		tc.HTTPReq.Body = strings.ReplaceAll(strings.ReplaceAll(tc.HTTPReq.Body, `id="1"`, `id="2"`), ">x<", ">y<")
		return tc
	}())
}

func TestJSONRPCOperation(t *testing.T) {
	rpc := func(body string) *TestCase {
		return httpCase("POST", "/rpc", 200, "application/json", body, "application/json", `{"jsonrpc":"2.0","result":true,"id":1}`)
	}
	differentShape(t, "JSON-RPC methods",
		rpc(`{"jsonrpc":"2.0","method":"cart.add","params":{"sku":"a"},"id":1}`),
		rpc(`{"jsonrpc":"2.0","method":"cart.remove","params":{"sku":"a"},"id":1}`))
	sameShape(t, "a JSON-RPC batch's methods are a set",
		rpc(`[{"jsonrpc":"2.0","method":"a","id":1},{"jsonrpc":"2.0","method":"b","id":2}]`),
		rpc(`[{"jsonrpc":"2.0","method":"b","id":1},{"jsonrpc":"2.0","method":"a","id":2}]`))
}

func TestShapeOfKinds(t *testing.T) {
	if _, ok := ShapeOf(&TestCase{Kind: GENERIC}); ok {
		t.Fatal("a generic test case has no shape")
	}
	if CoverHash(nil) != "" || CoverHash(&TestCase{Kind: MySQL}) != "" {
		t.Fatal("no cover hash without a shape")
	}
	h2 := jsonCase("/a", `{}`)
	h2.Kind = HTTP2
	sameShape(t, "HTTP/2 reads as HTTP", h2, jsonCase("/a", `{}`))
}

func TestSplitTarget(t *testing.T) {
	for _, tc := range []struct{ in, path, query string }{
		{"http://svc:8080/users/1?x=1&y", "/users/1", "x=1&y"},
		{"/users/1?x=1", "/users/1", "x=1"},
		{"/a%20b", "/a b", ""},
		{"http://svc", "", ""},
	} {
		p, q := splitTarget(tc.in)
		if p != tc.path || q != tc.query {
			t.Errorf("splitTarget(%q) = %q, %q; want %q, %q", tc.in, p, q, tc.path, tc.query)
		}
	}
}

// FuzzHTTPShape checks that any request and response has a shape, the same
// each time, without a panic.
func FuzzHTTPShape(f *testing.F) {
	f.Add("/users/1?x=1", "application/json", `{"a":[{"b":1}]}`, "text/xml", `<a b="1"><c>x</c></a>`)
	f.Add("/graphql", "application/graphql", `{ a(x: 1) { b } }`, "", "")
	f.Add("/f", "application/x-www-form-urlencoded", "a=1&b", "", "%%")
	f.Fuzz(func(t *testing.T, target, reqCT, reqBody, respCT, respBody string) {
		tc := httpCase("POST", target, 200, reqCT, reqBody, respCT, respBody)
		first := CoverHash(tc)
		if again := CoverHash(tc); again != first {
			t.Fatalf("the shape differs between two reads: %s, then %s", first, again)
		}
	})
}
