package proxy

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A rule with a host and no path passes a TLS connection through when its host
// names the server the client asked for, on the rule's port or any port. A
// rule with a path is the HTTP parser's, which sees each call's path, and a
// port-only rule is the kernel's.
func TestABypassRuleWithAHostPassesItsTLSServerThrough(t *testing.T) {
	rules := []models.BypassRule{
		{Port: 7003},
		{Host: `^api\.example\.com$`, Port: 443},
		{Host: "auth.example.com"},
		{Host: "files.example.com", Path: "/upload", Port: 443},
		{Host: "[", Port: 443},
	}
	for _, tc := range []struct {
		server string
		port   uint
		want   bool
	}{
		{"api.example.com", 443, true},
		{"api.example.com", 8443, false},
		{"web.example.com", 443, false},
		{"auth.example.com", 9443, true},
		{"files.example.com", 443, false},
		{"", 443, false},
		{"", 7003, false},
	} {
		if _, got := bypassByServerName(zap.NewNop(), rules, tc.server, tc.port); got != tc.want {
			t.Errorf("bypassByServerName(%q, %d) = %v, want %v", tc.server, tc.port, got, tc.want)
		}
	}
}

// A connection that shows no host cannot apply a rule with a host for its
// port, nor one with no path a rule with a path. keploy warns of each such
// rule once, naming the port-only rule that passes the whole port through.
func TestABypassRuleAConnectionCannotApplyIsWarnedOfOnce(t *testing.T) {
	rules := []models.BypassRule{
		{Port: 5432},
		{Host: "db.internal", Port: 5432},
		{Path: "/v1", Port: 5432},
		{Host: "other.internal", Port: 6432},
		{Host: "any.internal"},
	}
	for _, tc := range []struct {
		name               string
		hostSeen, pathSeen bool
		want               []models.BypassRule
	}{
		{"plain, not HTTP", false, false, []models.BypassRule{rules[1], rules[2]}},
		{"TLS, not HTTP", true, false, []models.BypassRule{rules[2]}},
		{"HTTP", true, true, nil},
	} {
		got := bypassRulesUnseen(rules, 5432, tc.hostSeen, tc.pathSeen)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: unseen = %+v, want %+v", tc.name, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: unseen = %+v, want %+v", tc.name, got, tc.want)
			}
		}
	}

	bypassRulesWarned.Range(func(k, _ any) bool { bypassRulesWarned.Delete(k); return true })
	t.Cleanup(func() { bypassRulesWarned.Range(func(k, _ any) bool { bypassRulesWarned.Delete(k); return true }) })
	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core)
	for i := 0; i < 3; i++ {
		warnBypassRulesUnseen(logger, rules, 5432, "10.0.0.5:5432", false, false)
	}
	entries := logs.All()
	if len(entries) != 2 {
		t.Fatalf("warnings = %d, want 2 (one for each rule): %v", len(entries), entries)
	}
	for i, want := range []string{`{host: "db.internal", path: "", port: 5432}`, `{host: "", path: "/v1", port: 5432}`} {
		msg := entries[i].Message
		if !strings.Contains(msg, want) || !strings.Contains(msg, "{port: 5432}") || !strings.Contains(msg, "10.0.0.5:5432") {
			t.Errorf("warning %d = %q, want it to name %s, the port-only rule and the destination", i, msg, want)
		}
	}
}
