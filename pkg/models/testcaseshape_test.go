package models

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"runtime"
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

// canonical is a shape's canonical form as its lines.
func canonical(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

// TestCoverHashIsPinned pins the canonical form of one test case of each body
// kind. Stores keep cover hashes, so a change to what a shape holds, or to
// how it is written, must come with a new CoverHashVersion, or stored hashes
// and new ones of one shape stop matching. TestRouteKeepsNamesWithDigits
// pins the route rule.
func TestCoverHashIsPinned(t *testing.T) {
	multipart := httpCase("POST", "/upload", 201, "multipart/form-data; boundary=x", "", "application/json", `{"id":"u1"}`)
	multipart.HTTPReq.Form = []FormData{{Key: "title", Values: []string{"a"}}, {Key: "doc", Paths: []string{"/tmp/x"}}}
	for _, c := range []struct {
		kind string
		tc   *TestCase
		want string
	}{
		{"JSON", httpCase("POST", "http://svc/users/42/orders?expand=items&id=7", 201,
			"application/json", `{"items":[{"sku":"a","qty":1},{"sku":"b","gift":true}],"note":null,"tags":[]}`,
			"application/json; charset=utf-8", `{"id":"o-1","total":12.5}`),
			canonical(`kind "http"`, `method "POST"`, `route "/users/{}/orders"`, `query "expand"`, `query "id"`, `status "201"`,
				`req.media "application/json"`, `resp.media "application/json"`, `operation ""`,
				`req o{items:a[o{gift:b,qty:n,sku:s}],note:z,tags:a}`, `resp o{id:s,total:n}`)},
		{"XML", httpCase("POST", "/ws", 200,
			"text/xml; charset=utf-8", `<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body><GetUser id="1"><Name>x</Name><Name>y</Name></GetUser></soap:Body></soap:Envelope>`,
			"text/xml", `<r><ok/></r>`),
			canonical(`kind "http"`, `method "POST"`, `route "/ws"`, `status "200"`,
				`req.media "text/xml"`, `resp.media "text/xml"`, `operation ""`,
				`req {Envelope:e{Body:e{GetUser:e{@id:s,Name:se}}}}`, `resp {r:e{ok:e}}`)},
		{"url-encoded form", httpCase("POST", "/login", 302, "application/x-www-form-urlencoded", "user=a&pass=b&remember", "", ""),
			canonical(`kind "http"`, `method "POST"`, `route "/login"`, `status "302"`,
				`req.media "application/x-www-form-urlencoded"`, `resp.media ""`, `operation ""`,
				`req {pass:s,remember:s,user:s}`, `resp `)},
		{"multipart form", multipart,
			canonical(`kind "http"`, `method "POST"`, `route "/upload"`, `status "201"`,
				`req.media "multipart/form-data"`, `resp.media "application/json"`, `operation ""`,
				`req {doc:f,title:s}`, `resp o{id:s}`)},
		{"GraphQL", httpCase("POST", "/graphql", 200,
			"application/json", `{"query":"query Q($id: ID!) { u: user(id: $id) { name ...F } } fragment F on User { email }","operationName":"Q","variables":{"id":"1"}}`,
			"application/json", `{"data":{"user":{"name":"a","email":"b"}}}`),
			canonical(`kind "http"`, `method "POST"`, `route "/graphql"`, `status "200"`,
				`req.media "application/json"`, `resp.media "application/json"`,
				`operation "graphql:fragment F on User {email} query Q($id:ID!) {user(id){...F name}}#\"Q\""`,
				`req o{operationName:s,query:s,variables:o{id:s}}`, `resp o{data:o{user:o{email:s,name:s}}}`)},
		{"JSON-RPC", httpCase("POST", "/rpc", 200,
			"application/json", `[{"jsonrpc":"2.0","method":"cart.add","params":{"sku":"a"},"id":1},{"jsonrpc":"2.0","method":"cart.list","id":2}]`,
			"application/json", `[{"jsonrpc":"2.0","result":true,"id":1}]`),
			canonical(`kind "http"`, `method "POST"`, `route "/rpc"`, `status "200"`,
				`req.media "application/json"`, `resp.media "application/json"`, `operation "jsonrpc:\"cart.add\",\"cart.list\""`,
				`req a[o{id:n,jsonrpc:s,method:s,params:o{sku:s}}]`, `resp a[o{id:n,jsonrpc:s,result:b}]`)},
		{"gRPC", grpcCase("/users.v1.Users/Get", "0", []string{`1: {"alice"}`}, []string{"1: {1: {\"a\"} 2: {\"a@x\"} 3: 5}", `4: {"x"}`}),
			canonical(`kind "grpc"`, `method ""`, `route "/users.v1.Users/Get"`, `status "0"`,
				`req.media ""`, `resp.media ""`, `operation ""`,
				`req m{1:l}`, `resp m{1:l{1:l,2:l},4:l}`)},
	} {
		s, _ := ShapeOf(c.tc)
		if got := s.Canonical(); got != c.want {
			t.Errorf("%s: canonical shape changed (bump CoverHashVersion if this is meant):\n%s\nwant:\n%s", c.kind, got, c.want)
		}
		if got, want := CoverHash(c.tc), "c1:"+sha256Hex(c.want)[:32]; got != want {
			t.Errorf("%s: cover hash %s, want %s", c.kind, got, want)
		}
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
		{"/a%20b", "/a%20b", ""},
		{"/files/a%2Fb", "/files/a%2Fb", ""},
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

// allocatedBy is the bytes f allocates.
func allocatedBy(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// requireLinear checks that f costs memory linear in its input: the input
// built from 4n repeats allocates about four times what the one from n does,
// not sixteen, and no more than 1 KiB per input byte.
func requireLinear(t *testing.T, name string, input func(n int) string, f func(string)) {
	t.Helper()
	const n = 4000
	small, large := input(n), input(4*n)
	a := allocatedBy(func() { f(small) })
	b := allocatedBy(func() { f(large) })
	if b > 6*a || b > 1024*uint64(len(large)) {
		t.Errorf("%s: %d bytes allocated for %d bytes of input, %d for %d; want linear", name, a, len(small), b, len(large))
	}
}

// TestShapeCostIsLinear checks that a body's shape costs memory in
// proportion to the body, however deep it nests: a path is never written
// out once per entry.
func TestShapeCostIsLinear(t *testing.T) {
	key := strings.Repeat("k", 200)
	for _, c := range []struct {
		name, media string
		body        func(n int) string
	}{
		{"JSON, nested 200-char keys", "application/json", func(n int) string {
			return strings.Repeat(`{"`+key+`":`, n/4) + "1" + strings.Repeat("}", n/4)
		}},
		{"JSON, nested arrays", "application/json", func(n int) string { return strings.Repeat("[", n) + strings.Repeat("]", n) }},
		{"XML, unclosed elements", "text/xml", func(n int) string { return strings.Repeat("<a>", n) }},
		{"XML, nested 200-char tags", "text/xml", func(n int) string {
			return strings.Repeat("<"+key+">", n/4) + strings.Repeat("</"+key+">", n/4)
		}},
	} {
		requireLinear(t, c.name, c.body, func(body string) { CoverHash(httpCase("POST", "/a", 200, c.media, body, "", "")) })
	}
	requireLinear(t, "gRPC, nested messages", func(n int) string { return strings.Repeat("1: {", n) + strings.Repeat("}", n) },
		func(data string) { CoverHash(grpcCase("/S/M", "0", []string{data}, nil)) })
}

// TestJSONBodyOfAnyDepth checks that JSON nested deeper than encoding/json's
// Unmarshal allows still has its shape, not the invalid body's.
func TestJSONBodyOfAnyDepth(t *testing.T) {
	deep := func(leaf string) *TestCase {
		return httpCase("POST", "/a", 200, "application/json", strings.Repeat("[", 20000)+leaf+strings.Repeat("]", 20000), "", "")
	}
	differentShape(t, "a deep array and an invalid body", deep(""), httpCase("POST", "/a", 200, "application/json", `{"a":`, "", ""))
	differentShape(t, "two deep arrays that differ at the bottom", deep(`1`), deep(`"x"`))
}

// TestJSONNumbersOfAnySize checks that a number too large for a float64 is a
// number, not an invalid body.
func TestJSONNumbersOfAnySize(t *testing.T) {
	sameShape(t, "1e400 and 1", jsonCase("/a", `{"n":1e400}`), jsonCase("/a", `{"n":1}`))
	differentShape(t, "1e400 and a string", jsonCase("/a", `{"n":1e400}`), jsonCase("/a", `{"n":"x"}`))
	differentShape(t, "a trailing value", jsonCase("/a", `{"n":1} {"n":1}`), jsonCase("/a", `{"n":1}`))
}

// TestCanonicalCannotBeForged checks that no value a request carries can
// write a line of the canonical form: a value that holds a newline is quoted.
func TestCanonicalCannotBeForged(t *testing.T) {
	rpc := func(body string) *TestCase {
		return httpCase("POST", "/rpc", 200, "application/json", body, "application/json", `{}`)
	}
	differentShape(t, "a JSON-RPC method that holds a body line",
		rpc(`{"jsonrpc":"2.0","method":"a\nreq $.a:s"}`), rpc(`{"jsonrpc":"2.0","method":"a","a":"x"}`))
	differentShape(t, "a JSON-RPC batch's methods that hold a comma",
		rpc(`[{"jsonrpc":"2.0","method":"a,b"},{"jsonrpc":"2.0","method":"c"}]`),
		rpc(`[{"jsonrpc":"2.0","method":"a"},{"jsonrpc":"2.0","method":"b,c"}]`))
	differentShape(t, "a gRPC :path that holds a status line",
		grpcCase("/S/M\nstatus 5", "0", nil, nil), grpcCase("/S/M", "5\nstatus 0", nil, nil))
}

// TestRouteKeepsNamesWithDigits pins the shape's route rule: a path piece
// is {} when it has a digit and is a number, a date or a version (no
// letters), hex of eight or more characters, or longer than 20 bytes; every
// other piece is a name and is kept, however long.
func TestRouteKeepsNamesWithDigits(t *testing.T) {
	for _, c := range []struct{ path, route string }{
		{"/users/42", "/users/{}"},
		{"/reports/2024-01-02", "/reports/{}"},
		{"/pkg/lodash/4.17.21", "/pkg/lodash/{}"},
		{"/hosts/10.0.0.1/ping", "/hosts/{}/ping"},
		{"/commits/5f3a9c2e", "/commits/{}"},
		{"/users/3f2b8c1e-9a4d-4e6f-8b7a-1c2d3e4f5a6b", "/users/{}"},
		{"/orders/64b7f0c2a1e4d5f6a7b8c9d0", "/orders/{}"},
		{"/tokens/tok_1NffrFeUfNVaHibXkTqPzRm", "/tokens/{}"},
		{"/posts/a-really-long-slug-of-many-words", "/posts/a-really-long-slug-of-many-words"},
		{"/api/v1/notification-preferences", "/api/v1/notification-preferences"},
		{"/caf%C3%A9/menu", "/caf%C3%A9/menu"},
		{"/a%20b/c%41", "/a%20b/cA"},
		{"/integrations/s3", "/integrations/s3"},
		{"/integrations/ec2", "/integrations/ec2"},
		{"/clusters/k8s", "/clusters/k8s"},
		{"/hash/md5", "/hash/md5"},
		{"/hash/sha256", "/hash/sha256"},
		{"/proto/http2", "/proto/http2"},
		{"/enc/base64", "/enc/base64"},
		{"/enc/utf8", "/enc/utf8"},
		{"/i18n/e2e", "/i18n/e2e"},
		{"/media/mp3/h264", "/media/mp3/h264"},
		{"/oauth2/token", "/oauth2/token"},
		{"/v1/users", "/v1/users"},
		{"/blobs/deadbeef", "/blobs/deadbeef"},
		{"/colors/a1b2c3", "/colors/a1b2c3"},
		{"/accounts/acct_42", "/accounts/acct_42"},
		{"/projects:batchGet", "/projects:batchGet"},
		{"/users/", "/users/"},
	} {
		if s, _ := ShapeOf(jsonCase(c.path, `{}`)); s.Route != c.route {
			t.Errorf("route of %q = %q, want %q", c.path, s.Route, c.route)
		}
	}
	differentShape(t, "two integrations", jsonCase("/integrations/s3", `{}`), jsonCase("/integrations/ec2", `{}`))
	differentShape(t, "two hashes", jsonCase("/h/sha256", `{}`), jsonCase("/h/md5", `{}`))
	differentShape(t, "a versioned name and an id", jsonCase("/oauth2/token", `{}`), jsonCase("/abc12/token", `{}`))
	sameShape(t, "two dates", jsonCase("/reports/2024-01-02", `{}`), jsonCase("/reports/2025-12-31", `{}`))
	sameShape(t, "two ObjectIDs", jsonCase("/orders/64b7f0c2a1e4d5f6a7b8c9d0", `{}`), jsonCase("/orders/650a1b2c3d4e5f6a7b8c9d0e", `{}`))
	for _, c := range []struct{ query, names string }{
		{"expand=items&page=2", "expand page"},
		{"utm_source2=x", "utm_source2"},
		{"1697040000", "{}"},
		{"12345=x", "{}"},
		{"a%20b=1", "a+b"},
		{"%7B%7D=1", "%7B%7D"},
	} {
		if s, _ := ShapeOf(jsonCase("/q?"+c.query, `{}`)); strings.Join(s.Query, " ") != c.names {
			t.Errorf("query names of %q = %q, want %q", c.query, s.Query, c.names)
		}
	}
}

// TestRouteOfAnEscapedPath checks that a route is read from the path as sent:
// an empty path is "/", and an escaped '/' does not split a piece.
func TestRouteOfAnEscapedPath(t *testing.T) {
	sameShape(t, "no path and /", jsonCase("http://svc", `{}`), jsonCase("http://svc/", `{}`))
	differentShape(t, "an escaped slash and a slash", jsonCase("/files/a%2Fb", `{}`), jsonCase("/files/a/b", `{}`))
	differentShape(t, "a trailing slash", jsonCase("/users", `{}`), jsonCase("/users/", `{}`))
	differentShape(t, "a literal {} and an id", jsonCase("/a/%7B%7D", `{}`), jsonCase("/a/1", `{}`))
}

// TestBodyEdgeCases covers the bodies the review found merged: invalid
// UTF-8 keys, a url-encoded form that does not parse, and JSON sent as
// text/plain.
func TestBodyEdgeCases(t *testing.T) {
	differentShape(t, "two invalid UTF-8 keys", jsonCase("/a", "{\"\xff\":1}"), jsonCase("/a", "{\"\xfe\":1}"))
	differentShape(t, "an invalid UTF-8 key and U+FFFD", jsonCase("/a", "{\"\xff\":1}"), jsonCase("/a", `{"�":1}`))
	form := func(body string) *TestCase {
		return httpCase("POST", "/f", 200, "application/x-www-form-urlencoded", body, "", "")
	}
	differentShape(t, "forms separated by ';'", form("a=1;b=2"), form("c=1;d=2"))
	differentShape(t, "form names beside a bad escape", form("user=%zz"), form("pass=%zz"))
	plain := func(body string) *TestCase {
		return httpCase("POST", "/track", 200, "text/plain;charset=UTF-8", body, "", "")
	}
	differentShape(t, "JSON sent as text/plain", plain(`{"user":"a"}`), plain(`{"order":1}`))
	sameShape(t, "plain text", plain(`hello`), plain(`goodbye`))
	differentShape(t, "text/plain JSON and plain text", plain(`{"user":"a"}`), plain(`hello`))
}
