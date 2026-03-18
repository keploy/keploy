package http

import (
	"testing"

	"go.keploy.io/server/v3/config"
)

func TestDockerAgentConfigPreservesDebug(t *testing.T) {
	client := &AgentClient{
		conf: &config.Config{
			InstallationID: "install-id",
			Debug:          true,
		},
	}

	got := client.dockerAgentConfig()
	if got.InstallationID != "install-id" {
		t.Fatalf("unexpected installation id: %q", got.InstallationID)
	}
	if !got.Debug {
		t.Fatal("expected docker agent config to preserve debug mode")
	}
}

func TestDockerAgentConfigHandlesNilConfig(t *testing.T) {
	client := &AgentClient{}

	got := client.dockerAgentConfig()
	if got == nil {
		t.Fatal("expected non-nil docker agent config")
	}
	if got.Debug {
		t.Fatal("expected debug to be false for nil config")
	}
	if got.InstallationID != "" {
		t.Fatalf("expected empty installation id, got %q", got.InstallationID)
	}
}
