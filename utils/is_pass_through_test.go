package utils

import (
	"net/http/httptest"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A rule whose host matches a call and whose path does not compile matches no
// call. It used to match every call to its host when it was the last rule, or
// when a port-only rule for the call's port came after it.
// A port-only rule is the kernel's, never the HTTP parser's.
func TestIsPassThroughMatchesARuleWholly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rules []models.BypassRule
		path  string
		port  uint
		want  bool
	}{
		{"the host and path match", []models.BypassRule{{Host: `^dep\.internal$`, Path: "^/v1/"}}, "/v1/a", 80, true},
		{"the path does not match", []models.BypassRule{{Host: `^dep\.internal$`, Path: "^/v1/"}}, "/v2/a", 80, false},
		{"the path does not compile", []models.BypassRule{{Host: `^dep\.internal$`, Path: "("}}, "/v1/a", 80, false},
		{"the path does not compile, then a port-only rule for the call's port", []models.BypassRule{{Host: `^dep\.internal$`, Path: "("}, {Port: 80}}, "/v1/a", 80, false},
		{"another port", []models.BypassRule{{Host: `^dep\.internal$`, Port: 443}}, "/v1/a", 80, false},
		{"a port-only rule", []models.BypassRule{{Port: 80}}, "/v1/a", 80, false},
	} {
		// As the proxy reads a call: its path in the URL, its host apart.
		req := httptest.NewRequest("GET", tc.path, nil)
		req.Host = "dep.internal"
		if got := IsPassThrough(zap.NewNop(), req, tc.port, models.OutgoingOptions{Rules: tc.rules}); got != tc.want {
			t.Errorf("%s: IsPassThrough = %v, want %v", tc.name, got, tc.want)
		}
	}
}
