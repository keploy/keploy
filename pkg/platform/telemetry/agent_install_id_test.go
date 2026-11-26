package telemetry

import (
	"slices"
	"testing"
)

func TestAgentInstallationIDArgs(t *testing.T) {
	if got := AgentInstallationIDArgs(""); got != nil {
		t.Fatalf("empty id: AgentInstallationIDArgs = %q, want nil", got)
	}
	if got := AgentInstallationIDArgs("   \t\n "); got != nil {
		t.Fatalf("whitespace id: AgentInstallationIDArgs = %q, want nil", got)
	}
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	want := []string{"--" + AgentInstallationIDFlag + "=" + id}
	if got := AgentInstallationIDArgs("  " + id + "\n"); !slices.Equal(got, want) {
		t.Fatalf("AgentInstallationIDArgs = %q, want %q", got, want)
	}
}
