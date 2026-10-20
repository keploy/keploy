package models

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A URL in its shape keeps the dependency it names, its scheme, host and
// port, and nothing of the values it carries, as CallShape keeps the rest.
func TestURLShape(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"absolute URL", "http://pay.internal:8443/cards/4111111111111111?cvv=123", "http://pay.internal:8443/cards/{}?cvv={}"},
		{"credentials and fragment", "https://admin:hunter2@api.example.com/reset/kq7Zt0x9tok#frag", "https://api.example.com/reset/{}"},
		{"a host that does not read as one", "http://4111.1111/x/42", "http://{}/x/{}"},
		{"a single label is a host after ://", "http://localhost/users/42", "http://localhost/users/{}"},
		{"a cluster's service name", "http://payment-svc/cards/1", "http://payment-svc/cards/{}"},
		{"a service name and its port", "http://payment-svc:8080/cards/1", "http://payment-svc:8080/cards/{}"},
		{"a port out of range", "http://payment-svc:99999/cards/1", "http://{}/cards/{}"},
		{"a port that is no number", "http://payment-svc:k3y/cards/1", "http://{}/cards/{}"},
		{"a port of more than five digits", "http://payment-svc:000080/cards/1", "http://{}/cards/{}"},
		{"a host label with digits", "https://cluster0.a1b2.mongodb.net/db", "https://cluster0.a1b2.mongodb.net/db"},
		{"dotted names", "http://payments.my-ns/cards/1", "http://payments.my-ns/cards/{}"},
		{"a token for a host", "myapp://kq7Zt0x9resettoken", "myapp://{}"},
		{"a UUID for a host", "http://550e8400-e29b-41d4-a716-446655440000/x", "http://{}/x"},
		// A password with an unescaped '#', '?' or '/' ends the authority
		// early, as url.Parse reads it; the '@' that follows says what was
		// read as the host may be credentials.
		{"a '#' in a password", "http://admin:1234#x@db.internal/x", "http://{}"},
		{"a '/' in a password", "postgres://admin:2024/pw@db/orders", "postgres://{}/{}/orders"},
		{"an '@' and a '/' in a password", "http://user:p@ss/w@host/x", "http://{}/{}/x"},
		// A password with more than one '/', or a '/' and then a '?', ends
		// the authority, and the path's segments, before its '@': the host,
		// and every segment before that '@', may be credentials.
		{"two '/' in a password", "http://admin:2024/a/b@host/p", "http://{}/{}/{}/p"},
		{"words between the '/' of a password", "http://bob:hunter/secret/more@host/p", "http://{}/{}/{}/p"},
		{"a '/' and then a '?' in a password", "http://admin:2024/b?c@host/p", "http://{}/{}?{}"},
		{"an '@' further down the path", "http://api.example.com/users/alice@example.com", "http://{}/{}/{}"},
		{"an '@' and a route after it", "http://api.example.com/users/alice@example.com/orders", "http://{}/{}/{}/orders"},
		// A password with a '?' or a '#' in it ends the authority there, and
		// one with an '=' too reads as a query: an '@' anywhere after the
		// host may end credentials, so the host, and every segment, name and
		// value before that '@', are {}. So is a host before an application's
		// own '@', as in an email address: the shape cannot tell them apart.
		{"an '@' in a bare query component", "http://admin:2024?x@host/p", "http://{}?{}"},
		{"a '?' and an '=' in a password", "http://alice:4242?x=y@host/x", "http://{}?{}={}"},
		{"a '#' and an '=' in a password", "http://admin:5432#a=b@host/x", "http://{}"},
		{"a '/', a '?' and an '=' in a password", "http://alice:a/b?c=d@host", "http://{}/{}?{}={}"},
		{"an '@' in a query's value", "https://api.example.com/users?email=alice@example.com&page=2", "https://{}/{}?{}={}&page={}"},
		{"an '@' in a query's value, no path", "https://api.example.com?email=a@b.com", "https://{}?{}={}"},
		{"an npm scope", "https://registry.npmjs.org/@types/node", "https://{}/{}/node"},
		// A target that names its host first is read as the same URL would
		// be with a scheme.
		{"a password with a '/', host first", "admin:2024/pw@db/orders", "{}/{}/orders"},
		{"a method and a password with a '/', host first", "GET admin:2024/pw@db/orders", "GET {}/{}/orders"},
		{"a host first and an '@' in its query", "pay.internal:8443/users?email=a@b.com", "{}/{}?{}={}"},
		{"a word password with a '/', host first", "admin:hunter/pw@db/orders", "{}/{}/orders"},
		{"a method and a word password with a '/', host first", "GET admin:hunter/pw@db/orders", "GET {}/{}/orders"},
		{"a Go MySQL DSN", "root:secret/x@tcp(db:3306)/app", "{}/{}/app"},
		{"a relative path and an '@'", "users/alice@example.com", "{}/{}"},
		// A path names no host, so an '@' in it is the application's, but
		// for an '@' after a URL the path holds: that may end credentials.
		{"an email in a path", "/users/alice@example.com/orders", "/users/{}/orders"},
		{"an email in a query", "GET /users?email=a@b.com", "GET /users?email={}"},
		{"a URL in a path", "/proxy/http://admin:hunter/pw@db/x", "/{}/{}//{}/{}/x"},
		{"a URL in a query", "/login?next=http://u:pa&ss=x@h/x", "/{}?{}={}&{}={}"},
		{"a network-path reference in a path", "/proxy//admin:hunter/pw@db/x", "/{}//{}/{}/x"},
		{"a network-path reference in a query", "/login?next=//admin:hunter/pw@db/x", "/{}?{}={}"},
		// A URL whose "//" a proxy merged, or credentials alone, still say
		// by their ':' that the '@' may end them; so does any other ':' or
		// "//" before an '@', which the shape cannot tell apart.
		{"a URL in a path, its slashes merged", "/proxy/http:/admin:hunter/pw@db/x", "/{}/{}/{}/{}/x"},
		{"credentials in a path", "/proxy/admin:hunter/pw@db/x", "/{}/{}/{}/x"},
		{"a doubled slash and an email", "/api//users/alice@example.com", "/{}//{}/{}"},
		{"a time and an email in a query", "/users?since=10:00&email=a@b.com", "/{}?{}={}&{}={}"},
		// A network-path reference names its authority after "//", as an
		// absolute URL does after its scheme.
		{"a network-path reference", "//api.example.com/users/42", "//api.example.com/users/{}"},
		{"a network-path reference's password with a '/'", "//admin:hunter/pw@db/x", "//{}/{}/x"},
		{"a method and a network-path reference", "GET //admin:hunter/pw@db/x", "GET //{}/{}/x"},
		{"a network-path reference and an '@' in its query", "//api.example.com/users?email=a@b.com", "//{}/{}?{}={}"},
		// A target with no '/' before its query is read as one with none.
		{"no '/' before the query", "admin:2024?x@db/orders", "{}"},
		{"a method and no '/' before the query", "GET users?next=/home", "GET {}"},
		// A host is labels of letters, digits, '-' and '_' that read as
		// names; credentials with no '@' are none.
		{"credentials with no '@'", "http://user:pass:80/x", "http://{}/x"},
		{"a '~' in a host", "http://a~b_c/x", "http://{}/x"},
		{"a host with an API version label", "http://api.v2/x", "http://api.v2/x"},
		{"a host of '-' alone", "http://---/x", "http://{}/x"},
		{"an underscore in a service name", "http://payment_svc:8080/x", "http://payment_svc:8080/x"},
		{"a scheme that does not read as one", "s3cr3tT0kenValue://x/y", "{}://x/y"},
		{"a compound scheme", "mongodb+srv://cluster0.example.net/orders", "mongodb+srv://cluster0.example.net/orders"},
		{"a scheme with a digit", "s3://invoices/2024/inv-4411.pdf", "s3://invoices/{}/{}"},
		{"IPv4 and IPv6", "http://10.0.0.7:5432/db/12", "http://10.0.0.7:5432/db/{}"},
		{"bracketed IPv6", "http://[::1]:8080/users/a1b2c3d4", "http://[::1]:8080/users/{}"},
		{"no path", "ws://events.internal", "ws://events.internal"},
		{"a fragment after a name", "http://pay.internal/docs#section-4111", "http://pay.internal/docs"},
		{"method and URL", "GET http://pay.internal/cards/42?expand=items", "GET http://pay.internal/cards/{}?expand={}"},
		{"method and URL, spaced", "  POST   https://api.example.com/v1/charges/ch_42", "POST https://api.example.com/v1/charges/{}"},
		{"a bracket that is no IPv6 address", "http://[bad/cards/42", "http://{}/cards/{}"},
		{"method and a host that is no host", "GET http://ho st/cards/42", "GET http://{}/cards/{}"},
		{"no host", "file:///etc/passwd", "file:///etc/passwd"},
		{"query without a path", "http://pay.internal?token=kq7Zt0x9", "http://pay.internal?token={}"},
		// Whatever is not an absolute URL is shaped as CallShape shapes it.
		{"path", "/users/12345/orders?x=1", "/users/{}/orders?x={}"},
		{"method and path", "GET /orders/12345", "GET /orders/{}"},
		{"host-first target", "pay.internal:8443/cards/42", "pay.internal:8443/cards/{}"},
		{"a host first and a fragment", "pay.internal:8443/docs#section-4111", "pay.internal:8443/docs"},
		{"gRPC method", "/pkg.v1.Users/GetUser", "/pkg.v1.Users/GetUser"},
		// A target with no '/' is read only as a host, an OPTIONS *, or a
		// method alone; whatever else it holds is {}.
		{"CONNECT's authority", "CONNECT pay.internal:443", "CONNECT pay.internal:443"},
		{"OPTIONS *", "OPTIONS *", "OPTIONS *"},
		{"a method alone", "GET", "GET"},
		{"a gRPC call alone", "gRPC", "gRPC"},
		{"capitals alone that are no method", "ABCD-EFGH-JKLM", "{}"},
		{"capitals alone", "HUNTERPASSWORD", "{}"},
		{"a host alone", "pay.internal:8443", "pay.internal:8443"},
		{"a word", "hunterpassword", "{}"},
		{"credentials", "user:hunter", "{}"},
		{"a token", "opaque_abcdefghijklmnopqrstuvwx", "{}"},
		{"a method and a word", "GET hunterpassword", "GET {}"},
		// A word in capitals before a target is a method only when it is
		// one: it can be a code or a key.
		{"a key in capitals before a path", "SECRETKEY /x", "{} /x"},
		{"a dashed code before a path", "TOKEN-ABC /x", "{} /x"},
		{"a key in capitals before a word", "APIKEYABC foo", "{} {}"},
		{"a key in capitals before a URL", "SECRETKEY http://pay.internal/x", "{} http://pay.internal/x"},
		{"a WebDAV method", "PROPFIND /dav/files", "PROPFIND /dav/files"},
		{"a method that holds a '-'", "VERSION-CONTROL /repo", "VERSION-CONTROL /repo"},
		{"a method of seventeen letters", "UPDATEREDIRECTREF /x", "UPDATEREDIRECTREF /x"},
		{"a method of seventeen letters alone", "UPDATEREDIRECTREF", "UPDATEREDIRECTREF"},
		// Whitespace around a summary is not its target's.
		{"a trailing space", "GET /users/orders ", "GET /users/orders"},
		{"a trailing newline", "GET /users/orders\n", "GET /users/orders"},
		{"a path and a trailing newline", "/users/orders\n", "/users/orders"},
		{"words", "not a request", "{}"},
		{"no shape", "not a request/42", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := URLShape(c.in)
			require.Equal(t, c.want, got)
			require.Equal(t, got, URLShape(got), "a shape is its own shape")
		})
	}
}

// No shape keeps a URL's credentials, whatever its password holds, in any
// form a target takes: with a scheme, after "//", host first, in a path or a
// query, after a method. Each user and each piece of a password is a word of
// letters, which a shape keeps wherever it reads as a name, so a shape that
// holds one has kept credentials.
func TestURLShapeNeverKeepsCredentials(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	word := func() string {
		b := []byte("zq")
		for range 4 + r.Intn(4) {
			b = append(b, byte('a'+r.Intn(26)))
		}
		return string(b)
	}
	const delimiters = "/?#=&:"
	for range 20000 {
		user := word()
		pieces := []string{word()}
		pass := pieces[0]
		for range r.Intn(4) {
			p := word()
			pieces = append(pieces, p)
			pass += string(delimiters[r.Intn(len(delimiters))]) + p
		}
		host := []string{"db.internal", "db:5432", "pay.internal:8443"}[r.Intn(3)]
		tail := []string{"", "/orders", "/orders/42?expand=items", "?q=1", "#frag"}[r.Intn(5)]
		creds := user + ":" + pass + "@" + host + tail
		forms := []string{
			"postgres://" + creds, "//" + creds, creds,
			"/proxy/http://" + creds, "/login?next=http://" + creds,
			"/proxy//" + creds, "/login?next=//" + creds,
			"/proxy/http:/" + creds, "/login?next=http:/" + creds,
			"/proxy/" + creds, "/login?next=" + creds,
		}
		target := forms[r.Intn(len(forms))]
		u := []string{"", "GET ", "POST ", "{} "}[r.Intn(4)] + target
		shape := URLShape(u)
		for _, w := range append(pieces, user) {
			if strings.Contains(shape, w) {
				t.Fatalf("URLShape(%q) = %q keeps %q", u, shape, w)
			}
		}
		if again := URLShape(shape); again != shape {
			t.Fatalf("URLShape(%q) = %q, but its shape is %q", u, shape, again)
		}
	}
}
