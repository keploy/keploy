package models

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A path keeps its segments that read as names and writes every other one,
// and every query value, {}.
func TestPathShape(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"static", "/api/users", "/api/users"},
		{"root", "/", "/"},
		{"empty", "", ""},
		{"numeric ID", "/users/12345/orders", "/users/{}/orders"},
		{"email", "/users/alice@example.com/orders", "/users/{}/orders"},
		{"email, percent-encoded", "/users/alice%40example.com", "/users/{}"},
		{"UUID", "/sessions/550e8400-e29b-41d4-a716-446655440000", "/sessions/{}"},
		{"UUID without dashes", "/sessions/550e8400e29b41d4a716446655440000", "/sessions/{}"},
		{"hex digest", "/blobs/9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", "/blobs/{}"},
		{"Mongo ObjectId", "/carts/507f1f77bcf86cd799439011", "/carts/{}"},
		{"hex of letters only", "/blobs/deadbeefcafe", "/blobs/{}"},
		{"short word of hex letters", "/feed/add", "/feed/add"},
		{"prefixed ID", "/orders/ord_9f8e7d", "/orders/{}"},
		{"short ID with a digit", "/items/a1b2", "/items/{}"},
		{"SKU", "/products/ABC-12345", "/products/{}"},
		{"date", "/reports/2026-10-14", "/reports/{}"},
		{"phone number", "/calls/+14155550123", "/calls/{}"},
		{"IP address", "/hosts/10.0.0.12", "/hosts/{}"},
		{"base64", "/tokens/dGhpcyBpcyBh+IHNlY3JldA==", "/tokens/{}"},
		{"base64url with a digit", "/invites/V1StGXR8_Z5jdHi6B-myT", "/invites/{}"},
		{"JWT", "/verify/eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk", "/verify/{}"},
		{"random token of letters in both cases", "/customers/cus_NffrFeUfNVaHib", "/customers/{}"},
		{"short random token in both cases", "/invites/XkTqPzRmWb", "/invites/{}"},
		{"long token of letters", "/keys/" + strings.Repeat("abcdxyz", 5), "/keys/{}"},
		{"segment past 64 bytes", "/posts/" + strings.Repeat("a-", 40) + "z", "/posts/{}"},
		{"non-ASCII", "/cities/münchen", "/cities/{}"},
		{"space", "/files/my file", "/files/{}"},
		{"mixed segments", "/v2/accounts/acct_1Hx9/users/alice@example.com/orders/12345/items", "/v2/accounts/{}/users/{}/orders/{}/items"},
		{"API versions are names", "/api/v1/users/v2beta1/V3/v10", "/api/v1/users/v2beta1/V3/v10"},
		{"a long version is an ID", "/items/v123/v1beta123", "/items/{}/{}"},
		{"a name with a digit is shaped", "/oauth2/token", "/{}/token"},
		{"names with separators", "/.well-known/openid-configuration/user_profile/index.html", "/.well-known/openid-configuration/user_profile/index.html"},
		{"camelCase names", "/api/getUserById/XMLHttpRequest/userID", "/api/getUserById/XMLHttpRequest/userID"},
		{"a capital alone is no word", "/OAuth/iPhone", "/{}/{}"},
		{"acronyms", "/API/HTTP", "/API/HTTP"},
		{"gRPC method", "/google.cloud.secretmanager.v1.SecretManagerService/AccessSecretVersion", "/google.cloud.secretmanager.v1.SecretManagerService/AccessSecretVersion"},
		{"custom method", "/v1/projects/my-project:batchGet", "/v1/projects/my-project:batchGet"},
		{"a value with a custom method", "/v1/operations/12345:cancel", "/v1/operations/{}"},
		{"already templated", "/users/{}/orders/{}", "/users/{}/orders/{}"},
		{"template names", "/users/{id}/orders/:orderId", "/users/{}/orders/:orderId"},
		{"query values", "/search?q=alice@example.com&page=2&sort=desc", "/search?q={}&page={}&sort={}"},
		{"query name that is a value", "/x?12345=1&alice@example.com", "/x?{}={}&{}"},
		{"a bare query component is {}", "/svc?wsdl&1697040000", "/svc?{}&{}"},
		{"empty value", "/x?a=&b", "/x?a={}&{}"},
		{"empty query", "/x?", "/x"},
		{"a # is part of its segment (a decoded %23)", "/tags/abc#def/12345", "/tags/{}/{}"},
		{"long camelCase name", "/pkg.v1.SomeVeryLongServiceNameForTesting/AndAnEvenLongerMethodNameHere", "/pkg.v1.SomeVeryLongServiceNameForTesting/AndAnEvenLongerMethodNameHere"},
		{"already templated query", "/search?q={}&page={}", "/search?q={}&page={}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PathShape(c.in)
			require.Equal(t, c.want, got)
			require.Equal(t, got, PathShape(got), "a shape is its own shape")
		})
	}
}

// A call's summary in its shape: the method and the target's shape, a gRPC
// full method alone, an absolute URL by its path and query names, a target
// that names its host first with its host and port when they read as one.
func TestMethodTargetShape(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"method and path", "GET /users/alice@example.com/orders/12345", "GET /users/{}/orders/{}"},
		{"a decoded space is in its segment", "GET /files/my file/v2", "GET /files/{}/v2"},
		{"a last space too", "GET /files/my ", "GET /files/{}"},
		{"a lone target with a space", "/files/my file", "/files/{}"},
		{"gRPC full method", "/orders.v1.OrderService/PlaceOrder", "/orders.v1.OrderService/PlaceOrder"},
		{"gRPC method word", "gRPC /orders.v1.OrderService/PlaceOrder", "gRPC /orders.v1.OrderService/PlaceOrder"},
		{"absolute URL keeps its path and query names", "GET http://user:pw@api.example.com:8080/v1/users/12345?token=s3cret#frag", "GET /v1/users/{}?token={}"},
		{"absolute URL whose path starts //", "GET http://host//cdn/https://img.example.com/1.png", "GET //cdn/https://img.example.com/{}"},
		{"absolute URL that does not parse", "GET http://user:pw@api.example.com/%zz/12345", "GET {}"},
		{"host first, as a dependency row", "GET pay.internal:8443/cards/4111111111111111/charges", "GET pay.internal:8443/cards/{}/charges"},
		{"credentials are no host", "GET user:pw@pay.internal:8443/cards/4111", "GET {}/cards/{}"},
		{"an email is no host", "GET alice@example.com", "GET {}"},
		{"a dotted value is no host", "GET a1b2c3d4.e5f6/x", "GET {}/x"},
		{"a dotted number is no host", "GET 4111-1111-1111-1111.x/y", "GET {}/y"},
		{"a dotted name is a host", "GET pay.internal/cards/4111", "GET pay.internal/cards/{}"},
		{"host first, an address", "GET 10.0.0.12:8080/cards/4111", "GET 10.0.0.12:8080/cards/{}"},
		{"host and port alone", "CONNECT pay.internal:8443", "CONNECT pay.internal:8443"},
		{"a port past 65535 is no port", "GET pay.internal:99999/x", "GET {}/x"},
		{"a number is no host", "GET 12345/orders", "GET {}/orders"},
		{"a UUID is no host", "GET 550e8400-e29b-41d4-a716-446655440000/orders", "GET {}/orders"},
		{"a value with a colon is no host", "GET alice:s3cr3tT0ken", "GET {}"},
		{"a value before a query is no host", "GET ord_9f8e7d?x=1", "GET {}?x={}"},
		{"a value before a # is no host", "GET 4111111111111111#x", "GET {}"},
		{"a random token with a port is no host", "GET cus_NffrFeUfNVaHib:1/x", "GET {}/x"},
		{"an email before a query is no host", "GET alice@example.com?x", "GET {}?{}"},
		{"a lone word is a segment", "OPTIONS user12345", "OPTIONS {}"},
		{"a method with a dash", "M-SEARCH *", "M-SEARCH {}"},
		{"method alone", "GET", "GET"},
		{"gRPC alone", "gRPC ?", "gRPC"},
		{"a target that shapes to nothing", "GET ?", "GET"},
		{"a first word that is no method", "alice@example.com /x", ""},
		{"a lower-case first word is no method", "get /users/12345", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := methodTargetShape(c.in)
			require.Equal(t, c.want, got)
			require.Equal(t, got, methodTargetShape(got), "a shape is its own shape")
		})
	}
}

// A shape is its own shape, whatever the summary.
func FuzzMethodTargetShape(f *testing.F) {
	for _, s := range []string{"GET /users/12345", "gRPC ?", "GET http://h//a/https://b/1", "GET aID#x", "A 0#000000", "GET http://{}/x", "GET pay.internal:8443/x", "GET [::1]:80/x", "A:///%"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := methodTargetShape(s)
		if again := methodTargetShape(got); again != got {
			t.Fatalf("methodTargetShape(%q) = %q, which shapes again to %q", s, got, again)
		}
	})
}

// A segment a test variable's place names (url.path[<n>]) is {}, whatever
// its look; any other place names no segment.
func TestHolePathPlaces(t *testing.T) {
	cases := []struct {
		name, shape string
		places      []string
		want        string
	}{
		{"a named segment", "GET /orders/kqzxwv/items", []string{"url.path[1]"}, "GET /orders/{}/items"},
		{"the first segment", "GET /kqzxwv", []string{"url.path[0]"}, "GET /{}"},
		{"with a query", "GET /orders/kqzxwv?expand={}", []string{"url.path[1]"}, "GET /orders/{}?expand={}"},
		{"host first", "GET pay.internal:8443/carts/kqzxwv", []string{"url.path[1]"}, "GET pay.internal:8443/carts/{}"},
		{"a place past the path", "GET /orders", []string{"url.path[5]"}, "GET /orders"},
		{"places that are no segment", "GET /orders/kqzxwv", []string{"body.id", "url.query.id", "header.X-Id", "url.path[x]", "url.path[]", "url.path[01]"}, "GET /orders/kqzxwv"},
		{"no path", "GET", []string{"url.path[0]"}, "GET"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, holePathPlaces(c.shape, c.places))
		})
	}
}

// A miss's summary in its shape: an HTTP or gRPC call's method and target
// shape, a SQL statement without its literals, and nothing of any other
// protocol.
func TestCallShape(t *testing.T) {
	cases := []struct {
		name, protocol, summary, want string
	}{
		{"HTTP", "HTTP", "GET /users/alice@example.com/orders/12345?expand=items", "GET /users/{}/orders/{}?expand={}"},
		{"HTTP2", "HTTP2", "POST /carts/507f1f77bcf86cd799439011/checkout", "POST /carts/{}/checkout"},
		{"HTTP/2", "HTTP/2", "PUT /accounts/acct_42", "PUT /accounts/{}"},
		{"gRPC", "gRPC", "/orders.v1.OrderService/PlaceOrder", "/orders.v1.OrderService/PlaceOrder"},
		{"already templated", "HTTP", "GET /users/{}/orders", "GET /users/{}/orders"},
		{"HTTP with no path", "HTTP", "GET", "GET"},
		{"MySQL", "MySQL", "COM_QUERY SELECT * FROM users WHERE email = 'alice@example.com' AND id = 42", "COM_QUERY SELECT * FROM users WHERE email = ? AND id = ?"},
		{"Postgres", "PostgresV2", "SELECT 1", "SELECT ?"},
		{"SQL withheld from its first byte", "MySQL", "/* ping */ SELECT 1", "…"},
		{"another protocol has no shape", "Redis", "GET session:alice", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CallShape(c.protocol, c.summary)
			require.Equal(t, c.want, got)
			require.Equal(t, got, CallShape(c.protocol, got), "a shape is its own shape")
		})
	}
}

// A miss names a URL path segment as a value by a field diff at a test
// variable's place: that segment is {}, whatever its look. The place says
// nothing of a SQL statement.
func TestMissShape(t *testing.T) {
	miss := UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /orders/kqzxwv/items?expand=all",
		FieldDiffs: []MockFieldDiff{{Path: "url.path[1]"}, {Path: "url.query.expand"}}}
	require.Equal(t, "GET /orders/{}/items?expand={}", MissShape(miss))
	require.Equal(t, "GET /orders/kqzxwv/items?expand={}", CallShape(miss.Protocol, miss.ActualSummary), "by its look alone the segment is a name")
	sql := UnmatchedCall{Protocol: "MySQL", ActualSummary: "COM_QUERY SELECT a/b FROM t WHERE x = 1", FieldDiffs: []MockFieldDiff{{Path: "url.path[0]"}}}
	require.Equal(t, "COM_QUERY SELECT a/b FROM t WHERE x = ?", MissShape(sql))
	// A variable's place counts the escaped path's segments, as the miss
	// names its path: an encoded slash stays inside its segment.
	escaped := UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /files/a%2Fb/alice", FieldDiffs: []MockFieldDiff{{Path: "url.path[2]"}}}
	require.Equal(t, "GET /files/{}/{}", MissShape(escaped))
	// The places the closest mock declares are holed too, with no diff.
	declared := UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /orders/kqzxwv/lines/abcd", VariablePlaces: []string{"url.path[1]", "url.path[3]", "body.id"}}
	require.Equal(t, "GET /orders/{}/lines/{}", MissShape(declared))
}

// A mock's declared path places: each variable's bind and use places that
// are URL path segments, once, in the order of the variables' names.
func TestDeclaredPathPlaces(t *testing.T) {
	m := &Mock{Spec: MockSpec{Variables: MockVariables{
		"b": {Bind: "url.path[3]", Use: []string{"header.X-Id", "url.path[1]"}},
		"a": {Use: []string{"url.path[1]", "body.id", "url.path[x]"}},
	}}}
	require.Equal(t, []string{"url.path[1]", "url.path[3]"}, DeclaredPathPlaces(m))
	require.Nil(t, DeclaredPathPlaces(nil))
	require.True(t, IsPathPlace("url.path[0]"))
	for _, p := range []string{"", "url.path[]", "url.path[x]", "url.query.id", "body.id", "url.path[1]x"} {
		require.False(t, IsPathPlace(p), p)
	}
}

// A recorded HTTP mock's URL keeps its host and port and its target's shape,
// and every path segment the mock declares as a variable's place is {},
// whatever its look.
func TestMockURLShape(t *testing.T) {
	mock := func(url string, vars MockVariables) *Mock {
		return &Mock{Kind: HTTP, Spec: MockSpec{HTTPReq: &HTTPReq{Method: "GET", URL: url}, Variables: vars}}
	}
	cases := []struct {
		name string
		m    *Mock
		want string
	}{
		{"absolute URL", mock("http://user:pw@pay.internal:8443/carts/507f1f77bcf86cd799439011?force=true", nil), "pay.internal:8443/carts/{}?force={}"},
		{"a bare host", mock("http://pay.internal", nil), "pay.internal"},
		{"a declared bind place", mock("http://store/orders/kqzxwv", MockVariables{"orderId": {Recorded: "kqzxwv", Bind: "url.path[1]"}}), "store/orders/{}"},
		{"a declared use place", mock("http://store/orders/kqzxwv/items", MockVariables{"orderId": {Recorded: "kqzxwv", Use: []string{"body.id", "url.path[1]"}}}), "store/orders/{}/items"},
		{"a path alone", mock("/orders/12345", nil), "/orders/{}"},
		{"a URL that does not parse", mock("http://h/a%zz/alice", nil), "{}"},
		{"a path that starts //, read as a place reads it", mock("//orders/kqzxwv", MockVariables{"orderId": {Use: []string{"url.path[0]"}}}), "orders/{}"},
		{"an encoded slash before a place", mock("http://store/files/a%2Fb/kqzxwv", MockVariables{"orderId": {Use: []string{"url.path[2]"}}}), "store/files/{}/{}"},
		{"a relative URL, counted as a place counts it", mock("orders/kqzxwv", MockVariables{"orderId": {Use: []string{"url.path[1]"}}}), "orders/{}"},
		{"no HTTP request", &Mock{Kind: HTTP}, ""},
		{"no mock", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, MockURLShape(c.m))
		})
	}
}

// A miss's shape is its own shape, at whatever places, as the agent shapes
// a miss twice (its evidence, then its reason text).
func FuzzMissShape(f *testing.F) {
	for _, s := range []string{"GET /orders/kqzxwv/items", "GET /files/a%2Fb/alice", "/pkg.v1.Svc/Method", "GET pay.internal:8443/a/b", "GET /x?a=1&b"} {
		f.Add(s, uint8(1), uint8(2))
	}
	f.Fuzz(func(t *testing.T, s string, a, b uint8) {
		m := UnmatchedCall{Protocol: "HTTP", ActualSummary: s, VariablePlaces: []string{fmt.Sprintf("url.path[%d]", a%8)},
			FieldDiffs: []MockFieldDiff{{Path: fmt.Sprintf("url.path[%d]", b%8)}}}
		got := MissShape(m)
		m.ActualSummary = got
		if again := MissShape(m); again != got {
			t.Fatalf("MissShape(%q) = %q, which shapes again to %q", s, got, again)
		}
	})
}

// A compared field's name keeps its names and indexes and writes {} for a
// part that may be a value the app sent, by the segment rule; a header's name
// is kept whole.
func TestFieldShape(t *testing.T) {
	cases := []struct{ in, want string }{
		{"body.amount", "body.amount"},
		{"host", "host"},
		{"path", "path"},
		{"header.Authorization", "header.Authorization"},
		{"header.x-b3-traceid", "header.x-b3-traceid"},
		{"body.users.alice@example.role", "body.users.{}.role"},
		{"body.orders.123456.total", "body.orders.{}.total"},
		{"body.users.kqzx9w.role", "body.users.{}.role"},
		{"body.keys.NffrFeUfNVaHib", "body.keys.{}"},
		{"body.blobs.deadbeefcafe", "body.blobs.{}"},
		{"body.items[].sku9X0001234[]", "body.items[].{}[]"},
		{"body.items[0].id", "body.items[0].id"},
		{"body.items[0][].id", "body.items[0][].id"},
		{"query.ord_9f8e", "query.{}"},
		{"query.page", "query.page"},
		{"body.{}.id", "body.{}.id"},
		{"body.a b", "body.{}"},
		{"body.x[y]", "body.{}"},
		{"body.orders[4111111111111111].total", "body.{}.total"},
		{"body.line1", "body.{}"},
		// An SQL call's place names (a Postgres statement's bind.$N, the
		// Nth constant of its text literal.N) are positions keploy counts,
		// not values the app sent: kept whole. Anything else of their look
		// is shaped as every key is.
		{"bind.$1", "bind.$1"},
		{"bind.$65535", "bind.$65535"},
		{"literal.3", "literal.3"},
		{"literal.12", "literal.12"},
		{"bind.$0", "bind.{}"},
		{"bind.$01", "bind.{}"},
		{"bind.$123456", "bind.{}"},
		{"bind.$1a", "bind.{}"},
		{"bind.1", "bind.{}"},
		{"literal.0", "literal.{}"},
		{"literal.$1", "literal.{}"},
		{"literal.123456", "literal.{}"},
		{"bind.$1.total", "bind.{}.total"},
		{"body.bind.$1", "body.bind.{}"},
		{"body.literal.7", "body.literal.{}"},
		{"header.bind.$1", "header.bind.{}"},
	}
	for _, c := range cases {
		require.Equal(t, c.want, FieldShape(c.in), c.in)
		require.Equal(t, c.want, FieldShape(c.want), "a shape is its own shape: "+c.in)
	}
	require.Nil(t, FieldShapes(nil))
	require.Equal(t, []string{"body.{}", "host"}, FieldShapes([]string{"body.12345", "host", "body.67890"}), "once each")
}

// What SQLShape cannot read for certain it withholds, and it never keeps a
// literal or a bound parameter.
func TestSQLShapeWithholdsWhatItCannotRead(t *testing.T) {
	require.Equal(t, "SELECT ? …", SQLShape("SELECT 'it''s"), "a literal never closed withholds the rest")
	require.Equal(t, "INSERT INTO t VALUES (?, ?, ?, ?)", SQLShape("INSERT INTO t VALUES (0x1F, 3.5e2, 'x', 'it''s')"))
	for in, want := range map[string]string{
		// MySQL's bound parameters are values the app sent.
		"COM_STMT_EXECUTE SELECT id FROM users WHERE email = ? AND pw = ? params=[alice@example.com, s3cr3t]": "COM_STMT_EXECUTE SELECT id FROM users WHERE email = ? AND pw = ?",
		// Where a literal with a backslash ends depends on sql_mode.
		`UPDATE users SET bio = 'it\'s my secret diary' WHERE id = 7`:        "UPDATE users SET bio = ? …",
		`SELECT 1 WHERE a = 'x\' OR pw = ' AND token = 'abc' AND ssn = 'zz'`: "SELECT ? WHERE a = ? …",
		"SELECT $tag$secret$tag$":                  "SELECT …",
		"SELECT 1 -- secret":                       "SELECT ? …",
		"SELECT 1 /* secret */":                    "SELECT ? …",
		"SELECT 1 # secret":                        "SELECT ? …",
		"SELECT * FROM t WHERE id = $1 AND n = $2": "SELECT * FROM t WHERE id = $1 AND n = $2",
		"SELECT col1, t2.c3 FROM t2 WHERE x = 42":  "SELECT col1, t2.c3 FROM t2 WHERE x = ?",
		// A quote inside a backtick identifier would flip what is a literal.
		"UPDATE `user's` SET password = 'secret' WHERE id = 7":                "UPDATE …",
		"SELECT name AS `Customer\"s Name` FROM c WHERE email = 'alice@x.io'": "SELECT name AS …",
		"SELECT `users`.`email` FROM `users` WHERE `id` = 7":                  "SELECT `users`.`email` FROM `users` WHERE `id` = ?",
		"SELECT `unclosed FROM t WHERE p = 'secret'":                          "SELECT …",
		"COM_STMT_EXECUTE SELECT 1 params=[secret]":                           "COM_STMT_EXECUTE SELECT ?",
		"params=[secret]": "",
		// Withheld from its first byte: a leading comment.
		"/* ping */ SELECT 1": "…",
		"-- secret":           "…",
	} {
		require.Equal(t, want, SQLShape(in), in)
		for _, secret := range []string{"alice", "s3cr3t", "secret", "abc", "zz", "diary"} {
			require.NotContains(t, SQLShape(in), secret, in)
		}
	}
}

// No secret written as a literal survives SQLShape, whatever surrounds it:
// every combination of a prefix, a literal form and a suffix is shaped.
func TestSQLShapeKeepsNoLiteral(t *testing.T) {
	prefixes := []string{"", "SELECT * FROM `t` WHERE a = ", "UPDATE `x'y` SET b = ", "SELECT `a\"b`, ", "SELECT 1 -- ", "SELECT $q$ ", "INSERT INTO t VALUES (", "SELECT E", "SELECT _utf8mb4", "SELECT X"}
	literals := []string{"'SeCrEt'", "\"SeCrEt\"", "'Se''CrEt'", "'Se\\'CrEt'", "'SeCrEt", "`SeCrEt`'SeCrEt'", "$t$SeCrEt$t$", "/* SeCrEt */", "'a' 'SeCrEt'"}
	suffixes := []string{"", ")", " AND c = 'SeCrEt'", " params=[SeCrEt]", " `z`"}
	for _, p := range prefixes {
		for _, l := range literals {
			for _, sfx := range suffixes {
				q := p + l + sfx
				got := SQLShape(q)
				if strings.Contains(strings.ToLower(strings.ReplaceAll(got, "`SeCrEt`", "")), "secret") {
					t.Errorf("SQLShape(%q) = %q keeps the literal", q, got)
				}
			}
		}
	}
}

// Digits inside a kept backtick span are written "?", in case the span was
// misread (a Postgres operator, a multi-byte character set's trail byte).
func TestSQLShapeMasksDigitsInNames(t *testing.T) {
	require.Equal(t, "SELECT a `~ ? AND b ~` c", SQLShape("SELECT a `~ 999 AND b ~` c"))
	require.Equal(t, "SELECT `t?` FROM x", SQLShape("SELECT `t1` FROM x"))
}
