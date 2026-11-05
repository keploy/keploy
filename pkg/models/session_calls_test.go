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

// LifetimeUnder reads a mock's lifetime under the rules it is given, not the
// ones the process installed: keploy's mock authoring reads a set apart from
// any run, under that set's keploy.yaml. The mock is not derived.
func TestLifetimeUnderReadsTheGivenRulesOnly(t *testing.T) {
	t.Cleanup(func() { SetSessionCalls(nil) })
	token := []SessionCallRule{{Host: "auth.example.com", Method: "POST", PathPrefix: "/oauth/token"}}
	m := statedHTTP("POST", "/oauth/token", "auth.example.com")
	if got := m.LifetimeUnder(token); got != LifetimeSession {
		t.Fatalf("a stated per-test call the given rules cover reads %v, want session", got)
	}
	if got := m.LifetimeUnder(nil); got != LifetimePerTest {
		t.Fatalf("with no rules given the call reads %v, want per-test", got)
	}
	if got := statedHTTP("GET", "/oauth/token", "auth.example.com").LifetimeUnder(token); got != LifetimePerTest {
		t.Fatalf("a call the given rules do not cover reads %v, want per-test", got)
	}
	SetSessionCalls(token)
	if got := m.LifetimeUnder(nil); got != LifetimePerTest {
		t.Fatalf("with no rules given, the installed ones read the call %v, want per-test", got)
	}
	SetSessionCalls(nil)
	if got := m.LifetimeUnder(token); got != LifetimeSession {
		t.Fatalf("with none installed, the given rules read the call %v, want session", got)
	}
	if m.TestModeInfo.LifetimeDerived {
		t.Fatal("LifetimeUnder derived the mock itself")
	}
	// A lifetime the recorder states as session stays session, and one a
	// mock recorded before lifetimes were stated keeps the older rules'
	// answer, covered or not.
	s := statedHTTP("POST", "/oauth/token", "auth.example.com")
	s.Spec.Metadata[MetadataLifetimeKey] = MetadataLifetimeSession
	if got := s.LifetimeUnder(nil); got != LifetimeSession {
		t.Fatalf("a stated session call reads %v, want session", got)
	}
	c := statedHTTP("POST", "/oauth/token", "auth.example.com")
	c.Spec.Metadata[MetadataLifetimeKey], c.Spec.Metadata["connID"] = MetadataLifetimeConnection, "c1"
	if got := c.LifetimeUnder(token); got != LifetimeConnection {
		t.Fatalf("a covered call stated connection reads %v, want connection: the rules make only a per-test call session", got)
	}
	prev := laxKindFallbackDisabledCache
	laxKindFallbackDisabledCache = true
	t.Cleanup(func() { laxKindFallbackDisabledCache = prev })
	old := statedHTTP("POST", "/oauth/token", "auth.example.com")
	delete(old.Spec.Metadata, MetadataLifetimeKey)
	if got := old.LifetimeUnder(token); got != LifetimePerTest {
		t.Fatalf("a covered call recorded before lifetimes were stated reads %v, want per-test as before", got)
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

// LifetimeUnder reads a mock apart from any run, so a pre-tag recording's
// kind fallback it takes moves no legacy-fallback count, which operators
// read as how many such mocks a replay loaded. The same mocks derived as a
// replay loads them (DeriveLifetime) each count once.
func TestLifetimeUnderCountsNoLegacyKindFallback(t *testing.T) {
	untagged := func() []*Mock {
		return []*Mock{
			{Kind: Postgres, Spec: MockSpec{Metadata: map[string]string{}}},
			{Kind: HTTP, Spec: MockSpec{Metadata: map[string]string{}, HTTPReq: &HTTPReq{Method: "GET", URL: "http://risk.test/score"}}},
		}
	}
	before := LegacyKindFallbackFires()
	for _, m := range untagged() {
		m.LifetimeUnder(nil)
		m.LifetimeUnder([]SessionCallRule{{Host: "risk.test"}})
	}
	if got := LegacyKindFallbackFires() - before; got != 0 {
		t.Fatalf("LifetimeUnder counted %d legacy-fallback fires, want 0", got)
	}
	before = LegacyKindFallbackFires()
	for _, m := range untagged() {
		m.DeriveLifetime()
	}
	if got := LegacyKindFallbackFires() - before; got != 2 {
		t.Fatalf("DeriveLifetime counted %d legacy-fallback fires, want 2", got)
	}
}
