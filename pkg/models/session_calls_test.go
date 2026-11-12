package models

import "testing"

func statedHTTP(method, url, host string) *Mock {
	return &Mock{Kind: HTTP, Spec: MockSpec{
		Metadata: map[string]string{"type": string(HTTPClient), MetadataLifetimeKey: MetadataLifetimePerTest},
		HTTPReq:  &HTTPReq{Method: Method(method), URL: url, Header: map[string]string{"Host": host}},
	}}
}

// A call declared shared in keploy.yaml (sessionCalls) is served to every
// test: a stated per-test HTTP or HTTP/2 mock it covers derives session.
// Calls it does not cover, and other kinds, keep their lifetime.
func TestDeclaredSessionCallsAreSession(t *testing.T) {
	t.Cleanup(func() { SetSessionCalls(nil) })
	SetSessionCalls([]SessionCallRule{
		{Host: "auth.example.com", Method: "POST", PathPrefix: "/oauth/token"},
		{Host: "config.example.com:8443", PathPrefix: "/v1/"},
		{Host: "flags.example.com"},
	})
	h2 := func(method, url, authority string) *Mock {
		return &Mock{Kind: HTTP2, Spec: MockSpec{
			Metadata: map[string]string{MetadataLifetimeKey: MetadataLifetimePerTest},
			HTTP2Req: &HTTP2Req{Method: Method(method), URL: url, Authority: authority},
		}}
	}
	for _, tc := range []struct {
		name string
		mock *Mock
		want Lifetime
	}{
		{"declared token call", statedHTTP("POST", "/oauth/token", "auth.example.com"), LifetimeSession},
		{"declared, sub-path", statedHTTP("POST", "/oauth/token/refresh?x=1", "AUTH.example.com:443"), LifetimeSession},
		{"declared, absolute URL", statedHTTP("POST", "https://auth.example.com/oauth/token", ""), LifetimeSession},
		{"another method", statedHTTP("GET", "/oauth/token", "auth.example.com"), LifetimePerTest},
		{"a path that only shares letters", statedHTTP("POST", "/oauth/tokens", "auth.example.com"), LifetimePerTest},
		{"another host", statedHTTP("POST", "/oauth/token", "api.example.com"), LifetimePerTest},
		{"declared port", statedHTTP("GET", "/v1/settings", "config.example.com:8443"), LifetimeSession},
		{"another port", statedHTTP("GET", "/v1/settings", "config.example.com:9443"), LifetimePerTest},
		{"outside a trailing-slash prefix", statedHTTP("GET", "/v2/settings", "config.example.com:8443"), LifetimePerTest},
		{"whole host", statedHTTP("GET", "/anything", "flags.example.com"), LifetimeSession},
		{"HTTP/2 declared", h2("GET", "/anything", "flags.example.com:443"), LifetimeSession},
		{"HTTP/2 not declared", h2("GET", "/anything", "api.example.com"), LifetimePerTest},
		{
			"another kind is untouched",
			&Mock{Kind: MySQL, Spec: MockSpec{Metadata: map[string]string{MetadataLifetimeKey: MetadataLifetimePerTest, "connID": "c1"}}},
			LifetimePerTest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.mock.DeriveLifetime()
			if got := tc.mock.TestModeInfo.Lifetime; got != tc.want {
				t.Fatalf("derived %v, want %v", got, tc.want)
			}
		})
	}

	// A mock recorded before lifetimes were stated keeps the lifetime the
	// older rules give it, declared or not: here per-test, as they derive an
	// HTTP_CLIENT mock with the lax kind fallback off.
	prev := laxKindFallbackDisabledCache
	laxKindFallbackDisabledCache = true
	t.Cleanup(func() { laxKindFallbackDisabledCache = prev })
	old := statedHTTP("POST", "/oauth/token", "auth.example.com")
	delete(old.Spec.Metadata, MetadataLifetimeKey)
	old.DeriveLifetime()
	if old.TestModeInfo.Lifetime != LifetimePerTest {
		t.Fatalf("a declared call recorded before lifetimes were stated derived %v, want per-test as before", old.TestModeInfo.Lifetime)
	}

	SetSessionCalls(nil)
	m := statedHTTP("POST", "/oauth/token", "auth.example.com")
	m.DeriveLifetime()
	if m.TestModeInfo.Lifetime != LifetimePerTest {
		t.Fatalf("with no declared calls the token call derived %v, want per-test", m.TestModeInfo.Lifetime)
	}
}

// A rule that can match no call says why, for ValidateFlags to warn of.
func TestSessionCallRuleProblem(t *testing.T) {
	for _, tc := range []struct {
		rule SessionCallRule
		ok   bool
	}{
		{SessionCallRule{Host: "auth.example.com"}, true},
		{SessionCallRule{Host: "auth.example.com:8443", Method: "POST", PathPrefix: "/oauth/"}, true},
		{SessionCallRule{}, false},
		{SessionCallRule{Host: "  ", PathPrefix: "/oauth"}, false},
		{SessionCallRule{Host: "https://auth.example.com"}, false},
		{SessionCallRule{Host: "auth.example.com/oauth/token"}, false},
	} {
		if got := tc.rule.Problem(); (got == "") != tc.ok {
			t.Errorf("%+v: Problem() = %q, want ok %v", tc.rule, got, tc.ok)
		}
	}
}

// DeclaredSessionCall is whether a mock is a stated per-test HTTP call a
// declared session call names exactly, by its method and its whole path: the
// calls the proven switch serves every test from a recording made outside
// every test. A rule that covers the call by a path prefix only, or with no
// method, does not name it (DeriveLifetime still makes it session); a
// recording's empty path is the root (httpCallOf). A stated session call is session by its recorder, an
// unstated one keeps the older rules, and with no declared calls none is.
// SessionCallsDeclared follows the rules installed last.
func TestDeclaredSessionCallIsTheStatedPerTestCallARuleCovers(t *testing.T) {
	t.Cleanup(func() { SetSessionCalls(nil) })
	SetSessionCalls(nil)
	if SessionCallsDeclared() {
		t.Fatal("no rules installed, but session calls are declared")
	}
	SetSessionCalls([]SessionCallRule{{Host: "127.0.0.1", Method: "GET", PathPrefix: "/echo/rates"}})
	if !SessionCallsDeclared() {
		t.Fatal("a rule is installed, but no session call is declared")
	}
	stated := func(lifetime string) *Mock {
		m := statedHTTP("GET", "/echo/rates", "127.0.0.1:9000")
		if lifetime == "" {
			delete(m.Spec.Metadata, MetadataLifetimeKey)
		} else {
			m.Spec.Metadata[MetadataLifetimeKey] = lifetime
		}
		return m
	}
	for _, tc := range []struct {
		name string
		mock *Mock
		want bool
	}{
		{"a stated per-test call the rule covers", stated(MetadataLifetimePerTest), true},
		{"a stated session call", stated(MetadataLifetimeSession), false},
		{"a call recorded before lifetimes were stated", stated(""), false},
		{"a call the rule does not cover", statedHTTP("GET", "/echo/quote", "127.0.0.1:9000"), false},
		{"a call the rule covers by a path prefix only", statedHTTP("GET", "/echo/rates/eur", "127.0.0.1:9000"), false},
		{"another method on the rule's path", statedHTTP("POST", "/echo/rates", "127.0.0.1:9000"), false},
		{"the rule's call on another host", statedHTTP("GET", "/echo/rates", "10.0.0.1:9000"), false},
		{"no mock", nil, false},
	} {
		if got := tc.mock.DeclaredSessionCall(); got != tc.want {
			t.Errorf("%s: DeclaredSessionCall = %v, want %v", tc.name, got, tc.want)
		}
	}
	for _, tc := range []struct {
		name string
		rule SessionCallRule
		url  string
		want bool
	}{
		{"a rule with no method", SessionCallRule{Host: "127.0.0.1", PathPrefix: "/echo/rates"}, "/echo/rates", false},
		{"a rule with no path", SessionCallRule{Host: "127.0.0.1", Method: "GET"}, "/echo/rates", false},
		{"a rule with no path, a call with an empty path", SessionCallRule{Host: "127.0.0.1", Method: "GET"}, "http://127.0.0.1:9000", false},
		{"the root, a call with an empty path", SessionCallRule{Host: "127.0.0.1", Method: "GET", PathPrefix: "/"}, "http://127.0.0.1:9000", true},
		{"a prefix ending in a slash", SessionCallRule{Host: "127.0.0.1", Method: "GET", PathPrefix: "/echo/"}, "/echo/rates", false},
		{"the method in another case", SessionCallRule{Host: "127.0.0.1", Method: "get", PathPrefix: "/echo/rates"}, "/echo/rates", true},
	} {
		SetSessionCalls([]SessionCallRule{tc.rule})
		if got := statedHTTP("GET", tc.url, "127.0.0.1:9000").DeclaredSessionCall(); got != tc.want {
			t.Errorf("%s: DeclaredSessionCall = %v, want %v", tc.name, got, tc.want)
		}
	}
	SetSessionCalls(nil)
	if SessionCallsDeclared() || stated(MetadataLifetimePerTest).DeclaredSessionCall() {
		t.Fatal("the rules were removed, but a session call is still declared")
	}
}

// SessionCallDeclared says of a live call whether a declared session call
// covers it (by a path prefix too) and whether one names it exactly, by its
// method and whole path: a miss's hint tells the two apart under the proven
// switch, which serves only a named call from a recording made outside
// every test. A live call's empty path is the root, as a recording's is.
func TestSessionCallDeclaredTellsACoveredCallFromANamedOne(t *testing.T) {
	t.Cleanup(func() { SetSessionCalls(nil) })
	SetSessionCalls(nil)
	if covered, named := SessionCallDeclared("GET", "127.0.0.1:9000", "/echo/rates"); covered || named {
		t.Fatalf("no rules installed, but the call is covered %v, named %v", covered, named)
	}
	SetSessionCalls([]SessionCallRule{{Host: "127.0.0.1", Method: "GET", PathPrefix: "/echo/rates"}, {Host: "auth.example.com", PathPrefix: "/oauth"}, {Host: "root.example.com", Method: "GET", PathPrefix: "/"}})
	for _, tc := range []struct {
		method, host, path string
		covered, named     bool
	}{
		{"GET", "127.0.0.1:9000", "/echo/rates", true, true},
		{"GET", "127.0.0.1:9000", "/echo/rates/eur", true, false},
		{"POST", "auth.example.com", "/oauth", true, false},
		{"POST", "auth.example.com", "/oauth/token", true, false},
		{"GET", "127.0.0.1:9000", "/echo/quote", false, false},
		// An empty path is the root, as a recording's is read.
		{"GET", "root.example.com", "", true, true},
	} {
		covered, named := SessionCallDeclared(tc.method, tc.host, tc.path)
		if covered != tc.covered || named != tc.named {
			t.Errorf("%s %s%s: covered %v, named %v; want %v, %v", tc.method, tc.host, tc.path, covered, named, tc.covered, tc.named)
		}
	}
}
