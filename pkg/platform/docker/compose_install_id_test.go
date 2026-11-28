package docker

import (
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestGenerateKeployAgentServicePropagatesConfigInstallationID(t *testing.T) {
	t.Setenv("INSTALLATION_ID", "")
	const wantID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	svc, err := (&Impl{
		logger: zap.NewNop(),
		conf:   &config.Config{InstallationID: wantID},
	}).GenerateKeployAgentService(models.SetupOptions{})
	if err != nil {
		t.Fatalf("GenerateKeployAgentService: %v", err)
	}
	wantEnv := "INSTALLATION_ID=" + wantID
	found := false
	for i := 0; i+1 < len(svc.Content); i += 2 {
		if svc.Content[i].Value != "environment" {
			continue
		}
		for _, item := range svc.Content[i+1].Content {
			if item.Value == wantEnv {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected compose agent service environment to contain %q", wantEnv)
	}
}
