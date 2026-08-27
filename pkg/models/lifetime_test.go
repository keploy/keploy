package models

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models/mysql"
)

func httpMock(method string, header map[string]string, meta map[string]string) *Mock {
	return &Mock{
		Kind: HTTP,
		Spec: MockSpec{
			Metadata: meta,
			HTTPReq: &HTTPReq{
				Method: Method(method),
				URL:    "/query",
				Header: header,
			},
			HTTPResp: &HTTPResp{StatusCode: 200},
		},
	}
}

func mysqlMock(cmdType string, meta map[string]string) *Mock {
	return &Mock{
		Kind: MySQL,
		Spec: MockSpec{
			Metadata: meta,
			MySQLRequests: []mysql.Request{{
				PacketBundle: mysql.PacketBundle{
					Header: &mysql.PacketInfo{Type: cmdType},
				},
			}},
		},
	}
}

// preflightHeaders is the header set a browser actually sends on a CORS
// preflight, copied from a recorded OPTIONS /query mock (kind Http,
// metadata type HTTP_CLIENT) in the ci-cd recording that surfaced B28.
func preflightHeaders() map[string]string {
	return map[string]string{
		"Accept":                         "*/*",
		"Access-Control-Request-Headers": "content-type",
		"Access-Control-Request-Method":  "POST",
		"Origin":                         "http://localhost:3000",
		"Sec-Fetch-Mode":                 "cors",
	}
}

// TestDeriveLifetime covers the precedence rules in DeriveLifetime's doc
// comment. Every case builds a fresh mock because DeriveLifetime is
// idempotent via TestModeInfo.LifetimeDerived — reusing one mock across
// sub-tests would short-circuit every call after the first.
func TestDeriveLifetime(t *testing.T) {
	cases := []struct {
		name string
		mock func() *Mock
		want Lifetime
		// requiresLax marks cases that depend on rule 5, which is
		// disabled when KEPLOY_STRICT_MOCK_WINDOW is set to an
		// enabling value. The gate is snapshotted at package init, so
		// the test cannot flip it — it skips instead.
		requiresLax bool
	}{
		{
			// Rule 1(b), the B28 fix: a tagged HTTP_CLIENT preflight is
			// promoted to session so it is neither consumed on match nor
			// restricted to the recording test's own mock pool.
			name: "http tagged preflight OPTIONS is session",
			mock: func() *Mock {
				return httpMock("OPTIONS", preflightHeaders(), map[string]string{"type": HTTPClient})
			},
			want: LifetimeSession,
		},
		{
			// A bare OPTIONS with no Access-Control-Request-Method is not
			// a preflight — it may be a real data endpoint whose response
			// varies per caller, so it must stay consumable per-test.
			name: "http tagged non-preflight OPTIONS stays per-test",
			mock: func() *Mock {
				return httpMock("OPTIONS", map[string]string{"Accept": "*/*"}, map[string]string{"type": HTTPClient})
			},
			want: LifetimePerTest,
		},
		{
			// Proxy-recorded headers are canonicalised, but mocks that
			// arrive as raw JSON/YAML keep the producer's casing, so the
			// header probe is case-insensitive.
			name: "http preflight with lowercase header key is session",
			mock: func() *Mock {
				return httpMock("OPTIONS", map[string]string{"access-control-request-method": "POST"}, map[string]string{"type": HTTPClient})
			},
			want: LifetimeSession,
		},
		{
			// The core fix this branch shipped: tagged HTTP data mocks are
			// per-test, not session, so the matcher consumes them.
			name: "http tagged GET is per-test",
			mock: func() *Mock {
				return httpMock("GET", map[string]string{"Accept": "*/*"}, map[string]string{"type": HTTPClient})
			},
			want: LifetimePerTest,
		},
		{
			// Rule 4: pre-tag recordings have no type metadata at all and
			// must keep replaying as session.
			name: "http untagged is session via kind fallback",
			mock: func() *Mock {
				return httpMock("GET", map[string]string{"Accept": "*/*"}, nil)
			},
			want: LifetimeSession,
		},
		{
			// Rule 2.
			name: "http tagged config is session",
			mock: func() *Mock {
				return httpMock("GET", map[string]string{"Accept": "*/*"}, map[string]string{"type": "config"})
			},
			want: LifetimeSession,
		},
		{
			// Rule 1(a) must not regress: COM_PING is promoted even though
			// the recorder tagged it per-test "mocks".
			name: "mysql tagged mocks COM_PING is session",
			mock: func() *Mock {
				return mysqlMock("COM_PING", map[string]string{"type": "mocks"})
			},
			want: LifetimeSession,
		},
		{
			// Rule 1(a) is deliberately narrow: COM_QUERY depends on
			// input. It is NOT asserted as per-test here because rule 5
			// promotes tagged MySQL mocks to session under lax mode — the
			// narrowness of the allowlist itself is asserted in
			// TestIsMySQLSessionReusableCommandType below.
			name: "mysql tagged mocks COM_QUERY skips rule 1(a) and hits rule 5",
			mock: func() *Mock {
				return mysqlMock("COM_QUERY", map[string]string{"type": "mocks"})
			},
			want:        LifetimeSession,
			requiresLax: true,
		},
		{
			// Rule 5 must not regress for the kinds still on the lax
			// promotion list.
			name: "postgres tagged mocks is session under lax",
			mock: func() *Mock {
				return &Mock{
					Kind: Postgres,
					Spec: MockSpec{Metadata: map[string]string{"type": "mocks"}},
				}
			},
			want:        LifetimeSession,
			requiresLax: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.requiresLax && laxKindFallbackDisabled() {
				t.Skip("rule 5 disabled by KEPLOY_STRICT_MOCK_WINDOW")
			}
			m := tc.mock()
			m.DeriveLifetime()
			if m.TestModeInfo.Lifetime != tc.want {
				t.Fatalf("Lifetime = %v, want %v", m.TestModeInfo.Lifetime, tc.want)
			}
			if !m.TestModeInfo.LifetimeDerived {
				t.Fatal("LifetimeDerived not set")
			}
		})
	}
}

// Rule 1(a)'s allowlist, asserted directly on the predicate so the
// negative cases do not depend on the ambient KEPLOY_STRICT_MOCK_WINDOW
// setting (rule 5 promotes tagged MySQL mocks to session under lax).
func TestIsMySQLSessionReusableCommandType(t *testing.T) {
	for _, cmd := range []string{"COM_PING", "COM_STATISTICS", "COM_DEBUG", "COM_RESET_CONNECTION"} {
		if !IsMySQLSessionReusableCommandType(cmd) {
			t.Errorf("%s should be session-reusable", cmd)
		}
	}
	for _, cmd := range []string{"COM_QUERY", "COM_INIT_DB", "COM_CHANGE_USER", "COM_SET_OPTION", ""} {
		if IsMySQLSessionReusableCommandType(cmd) {
			t.Errorf("%q should not be session-reusable", cmd)
		}
	}
}

func TestIsCORSPreflightRequest(t *testing.T) {
	cases := []struct {
		name   string
		method string
		header map[string]string
		want   bool
	}{
		{"options with preflight header", "OPTIONS", preflightHeaders(), true},
		{"options lowercase method", "options", preflightHeaders(), true},
		{"options without preflight header", "OPTIONS", map[string]string{"Accept": "*/*"}, false},
		{"options nil header", "OPTIONS", nil, false},
		{"post with preflight header", "POST", preflightHeaders(), false},
		{"empty method", "", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCORSPreflightRequest(Method(tc.method), tc.header); got != tc.want {
				t.Fatalf("IsCORSPreflightRequest(%q) = %v, want %v", tc.method, got, tc.want)
			}
		})
	}
}

// httpIsCORSPreflight must not panic on a malformed mock: HTTP mocks whose
// response was elided to an asset still carry a nil HTTPReq on some ingest
// paths, and DeriveLifetime runs on every mock loaded from disk.
func TestHTTPIsCORSPreflightNilSafe(t *testing.T) {
	if httpIsCORSPreflight(nil) {
		t.Fatal("nil mock should not be a preflight")
	}
	m := &Mock{Kind: HTTP, Spec: MockSpec{Metadata: map[string]string{"type": HTTPClient}}}
	if httpIsCORSPreflight(m) {
		t.Fatal("mock with nil HTTPReq should not be a preflight")
	}
	m.DeriveLifetime()
	if m.TestModeInfo.Lifetime != LifetimePerTest {
		t.Fatalf("Lifetime = %v, want per-test", m.TestModeInfo.Lifetime)
	}
}
