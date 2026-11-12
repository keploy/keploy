package models

import (
	"strings"
	"testing"
)

// A path-only recorded URL is read as a request line is: "//fees/state" is
// the path "//fees/state", not the host "fees" and the path "/state"; any
// other URL is read with url.Parse.
func TestParseRequestURLReadsAPathOnlyURLAsARequestLine(t *testing.T) {
	for _, c := range []struct{ raw, host, path, query string }{
		{"//fees/state", "", "//fees/state", ""},
		{"//fees/state?region=ak", "", "//fees/state", "region=ak"},
		{"/fees/state", "", "/fees/state", ""},
		{"http://pricing//fees/state", "pricing", "//fees/state", ""},
		{"https://pricing:8443/fees?x=1", "pricing:8443", "/fees", "x=1"},
		// As a request line: a "#" and what follows stay in the query.
		{"/a?x=1#y", "", "/a", "x=1#y"},
	} {
		u, err := ParseRequestURL(c.raw)
		if err != nil || u.Host != c.host || u.Path != c.path || u.RawQuery != c.query {
			t.Fatalf("ParseRequestURL(%q) = host %q path %q query %q, %v; want %q %q %q", c.raw, u.Host, u.Path, u.RawQuery, err, c.host, c.path, c.query)
		}
	}
}

// A recorded call whose path begins with "//" is the call to its Host
// header's host at that path (httpCallOf), as a session-call rule reads it.
func TestHTTPCallOfAPathBeginningWithTwoSlashes(t *testing.T) {
	m := &Mock{Kind: HTTP, Spec: MockSpec{HTTPReq: &HTTPReq{Method: "GET", URL: "//fees/state", Header: map[string]string{"Host": "pricing"}}}}
	method, host, path, ok := httpCallOf(m)
	if !ok || method != "GET" || host != "pricing" || path != "//fees/state" {
		t.Fatalf("httpCallOf = %q %q %q %v; want GET pricing //fees/state", method, host, path, ok)
	}
	h2 := &Mock{Kind: HTTP2, Spec: MockSpec{HTTP2Req: &HTTP2Req{Method: "GET", URL: "//fees/state", Authority: "pricing"}}}
	if _, host, path, ok := httpCallOf(h2); !ok || host != "pricing" || path != "//fees/state" {
		t.Fatalf("HTTP/2 httpCallOf = %q %q %v; want pricing //fees/state", host, path, ok)
	}
}

// A recorded call to a path beginning with "//" names no destination of its
// own (the Host header names it), and its target keeps the path.
func TestAPathBeginningWithTwoSlashesNamesNoHost(t *testing.T) {
	m := &Mock{Kind: HTTP, Spec: MockSpec{HTTPReq: &HTTPReq{Method: "GET", URL: "//fees/state"}}}
	if dest, ok := m.RecordedDestination(); ok || dest != "" {
		t.Fatalf("RecordedDestination = %q, %v; want none: \"fees\" is no host", dest, ok)
	}
	if got := MockTargetFromSpec(m); !strings.Contains(got, "//fees/state") {
		t.Fatalf("MockTargetFromSpec = %q; want the path //fees/state in it", got)
	}
}

// A recorded request's parameters are put in order whatever its path, a
// path-only one url.Parse refuses included.
func TestSortedQueryURLOfAPathBeginningWithTwoSlashes(t *testing.T) {
	if got := sortedQueryURL("//projects:list?b=1&a=2"); got != "//projects:list?a=2&b=1" {
		t.Fatalf("sortedQueryURL = %q; want //projects:list?a=2&b=1", got)
	}
}
