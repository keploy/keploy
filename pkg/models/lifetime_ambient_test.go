package models

import "testing"

// The former ambient allowlist (/auth/check, /cluster/*, GetAll*Jobs, …) no
// longer promotes HTTP mocks to session lifetime: reuse is decided per spec by
// service/agent/specpool.go. CORS preflight is still session.
func TestFormerAmbientEndpointsArePerTest(t *testing.T) {
	prev := httpPerTestCache
	httpPerTestCache = true // the per-test HTTP model (KEPLOY_MOCK_HTTP_PER_TEST=1)
	t.Cleanup(func() { httpPerTestCache = prev })
	cases := []struct{ method, url, body string }{
		{"GET", "/auth/check", ""},
		{"GET", "/cluster/proxy-origins", ""},
		{"POST", "/query", `{"operationName":"GetAllGenJobs","query":"query GetAllGenJobs { x }"}`},
		{"POST", "/query", `{"operationName":"MyNotifications","query":"query MyNotifications { x }"}`},
	}
	for _, c := range cases {
		m := &Mock{Kind: HTTP, Spec: MockSpec{
			Metadata: map[string]string{"type": "HTTP_CLIENT"},
			HTTPReq:  &HTTPReq{Method: Method(c.method), URL: c.url, Body: c.body},
		}}
		m.DeriveLifetime()
		if m.TestModeInfo.Lifetime != LifetimePerTest {
			t.Errorf("%s %s: got lifetime %v, want per-test", c.method, c.url, m.TestModeInfo.Lifetime)
		}
	}
}

func TestCORSPreflightStillSession(t *testing.T) {
	m := &Mock{Kind: HTTP, Spec: MockSpec{
		Metadata: map[string]string{"type": "HTTP_CLIENT"},
		HTTPReq: &HTTPReq{Method: "OPTIONS", URL: "/query", Header: map[string]string{
			"Origin": "http://localhost:3000", "Access-Control-Request-Method": "POST"}},
	}}
	m.DeriveLifetime()
	if m.TestModeInfo.Lifetime != LifetimeSession {
		t.Fatalf("CORS preflight: got %v, want session", m.TestModeInfo.Lifetime)
	}
}
