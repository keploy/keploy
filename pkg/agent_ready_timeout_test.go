package pkg

import (
	"testing"
	"time"
)

// TestAgentReadyTimeout pins the parsing of KEPLOY_AGENT_READY_TIMEOUT: a Go
// duration or a bare integer (seconds) raises the failsafe ceiling; anything
// empty/invalid/non-positive falls back to the 120s default.
func TestAgentReadyTimeout(t *testing.T) {
	const def = 120 * time.Second
	cases := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"empty -> default", "", def},
		{"whitespace -> default", "   ", def},
		{"go duration seconds", "180s", 180 * time.Second},
		{"go duration minutes", "3m", 3 * time.Minute},
		{"bare integer = seconds", "240", 240 * time.Second},
		{"surrounding whitespace trimmed", "  90s  ", 90 * time.Second},
		{"invalid string -> default", "abc", def},
		{"zero -> default", "0", def},
		{"negative -> default", "-5", def},
		{"negative duration -> default", "-30s", def},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KEPLOY_AGENT_READY_TIMEOUT", tc.env)
			if got := AgentReadyTimeout(nil); got != tc.want {
				t.Fatalf("AgentReadyTimeout()=%s, want %s (env=%q)", got, tc.want, tc.env)
			}
		})
	}
}
