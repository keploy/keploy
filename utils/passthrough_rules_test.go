package utils

import (
	"net/http"
	"net/url"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A bypass rule that specifies only a port -- what `--pass-through-ports N` and
// `bypassRules: [{port: N}]` both produce via config.SetByPassPorts, which
// builds models.BypassRule{Host: "", Path: "", Port: N} -- was a no-op at the
// proxy layer. IsPassThrough seeded a `passThrough` flag to false, only the Host
// and Path arms ever assigned it, and the port comparison sat inside
// `if passThrough`. A rule with neither host nor path skipped both arms, so the
// port comparison was unreachable and the function returned false.
//
// The port-only cases below fail on unpatched code.
func TestIsPassThrough_PortOnlyRules(t *testing.T) {
	req := &http.Request{Host: "api.example.com", URL: &url.URL{Scheme: "https", Host: "api.example.com", Path: "/v1/users"}}

	cases := []struct {
		name     string
		rules    []models.BypassRule
		destPort uint
		want     bool
	}{
		{"port-only rule, port matches", []models.BypassRule{{Port: 3000}}, 3000, true},
		{"port-only rule, port differs", []models.BypassRule{{Port: 3000}}, 8080, false},
		{"two port-only rules, the second matches", []models.BypassRule{{Port: 43900}, {Port: 3000}}, 3000, true},
		{"host-only rule that matches", []models.BypassRule{{Host: "api\\.example\\.com"}}, 443, true},
		{"host-only rule that does not match", []models.BypassRule{{Host: "other\\.host"}}, 443, false},
		{"host and port, both match", []models.BypassRule{{Host: "api\\.example\\.com", Port: 443}}, 443, true},
		{"host matches but port does not", []models.BypassRule{{Host: "api\\.example\\.com", Port: 8080}}, 443, false},
		{"path-only rule that matches", []models.BypassRule{{Path: "/v1/.*"}}, 443, true},
		{"path and port, both match", []models.BypassRule{{Path: "/v1/.*", Port: 443}}, 443, true},
		{"path matches but port does not", []models.BypassRule{{Path: "/v1/.*", Port: 8080}}, 443, false},
		{"port-only rule against destPort 0", []models.BypassRule{{Port: 3000}}, 0, false},
		// A rule constraining nothing must be inert, not "bypass everything" --
		// a stray empty list entry must never silently disable recording.
		{"rule constraining nothing is inert", []models.BypassRule{{}}, 443, false},
		{"no rules at all", nil, 443, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsPassThrough(zap.NewNop(), req, tc.destPort, models.OutgoingOptions{Rules: tc.rules})
			if got != tc.want {
				t.Errorf("IsPassThrough(destPort=%d, rules=%+v) = %v, want %v", tc.destPort, tc.rules, got, tc.want)
			}
		})
	}
}

// A rule whose path regex does not compile is skipped. Historically the skip
// left a `passThrough` flag set by a PREVIOUS, matching arm still true, so the
// function could return true on the strength of a rule it had just rejected --
// and the leak also carried into later iterations, letting a subsequent rule
// reach a port comparison it was never meant to reach.
//
// That fail-OPEN behaviour is deliberately changed to fail-CLOSED here: a rule
// keploy cannot evaluate must not bypass recording. Recording something the
// user wanted skipped is recoverable; silently skipping something they wanted
// recorded is not. These cases pin that decision -- restoring the old flag
// would make them fail.
func TestIsPassThrough_MalformedRegexFailsClosed(t *testing.T) {
	req := &http.Request{Host: "api.example.com", URL: &url.URL{Scheme: "https", Host: "api.example.com", Path: "/v1/users"}}

	cases := []struct {
		name     string
		rules    []models.BypassRule
		destPort uint
		want     bool
	}{
		{
			name:     "matching host then an uncompilable path must not bypass",
			rules:    []models.BypassRule{{Host: "api\\.example\\.com", Path: "["}},
			destPort: 443,
			want:     false,
		},
		{
			name:     "an uncompilable host must not bypass",
			rules:    []models.BypassRule{{Host: "[", Port: 443}},
			destPort: 443,
			want:     false,
		},
		{
			// The rejected rule must not leak permission into the next one.
			// Here the second rule legitimately matches on port, so the answer
			// is true either way -- but for the right reason only after the fix.
			name:     "a rejected rule does not grant the next one permission",
			rules:    []models.BypassRule{{Host: "api\\.example\\.com", Path: "["}, {Port: 3000}},
			destPort: 3000,
			want:     true,
		},
		{
			name:     "...and the following rule is still evaluated on its own merits",
			rules:    []models.BypassRule{{Host: "api\\.example\\.com", Path: "["}, {Port: 3000}},
			destPort: 9999,
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsPassThrough(zap.NewNop(), req, tc.destPort, models.OutgoingOptions{Rules: tc.rules})
			if got != tc.want {
				t.Errorf("IsPassThrough(destPort=%d, rules=%+v) = %v, want %v", tc.destPort, tc.rules, got, tc.want)
			}
		})
	}
}
