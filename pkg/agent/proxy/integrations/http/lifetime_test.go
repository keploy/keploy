package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// TestHTTPOutboundLifetime pins the safe-vs-non-safe method classification
// the HTTP recorder uses to tier outbound mocks. Safe methods (GET / HEAD /
// OPTIONS per RFC 7231 §4.2.1) tier as LifetimeSession so apps that cache
// upstream responses can survive strictMockWindow when the cache expires
// mid-replay and re-fetches the same URL in a later test's window than the
// one the mock was originally recorded under. Non-safe methods stay
// LifetimePerTest because their responses can encode test-specific side
// effects.
//
// Regression for the sap-demo-java-postgres lane failure on
// keploy/integrations#196: an idempotent SAP BusinessPartner GET captured
// during recording testcase #6 was dropped at replay testcase #49 of the
// same test-set because the mock's request timestamp fell outside the
// later test's window — the strict-window pre-filter saw it as stale
// previous-test bleed even though the same URL legitimately re-fetches
// after the app's HTTP cache TTL.
func TestHTTPOutboundLifetime(t *testing.T) {
	t.Parallel()
	cases := []struct {
		method string
		want   models.Lifetime
	}{
		// Safe methods → session.
		{"GET", models.LifetimeSession},
		{"HEAD", models.LifetimeSession},
		{"OPTIONS", models.LifetimeSession},
		// Case-insensitive — Go's net/http normalises but be defensive.
		{"get", models.LifetimeSession},
		{"Head", models.LifetimeSession},
		{"options", models.LifetimeSession},
		// Non-safe methods → per-test.
		{"POST", models.LifetimePerTest},
		{"PUT", models.LifetimePerTest},
		{"PATCH", models.LifetimePerTest},
		{"DELETE", models.LifetimePerTest},
		// TRACE / CONNECT / unknown → defensive per-test.
		// (TRACE is idempotent but rarely exercised; CONNECT is for
		// tunnel setup, not a normal outbound exchange. Keeping them
		// per-test avoids accidental cross-test reuse on a method an
		// app might genuinely use as test-stateful.)
		{"TRACE", models.LifetimePerTest},
		{"CONNECT", models.LifetimePerTest},
		{"WEIRD", models.LifetimePerTest},
		{"", models.LifetimePerTest},
	}
	for _, tc := range cases {
		got := httpOutboundLifetime(tc.method)
		if got != tc.want {
			t.Errorf("httpOutboundLifetime(%q) = %v, want %v", tc.method, got, tc.want)
		}
	}
}
