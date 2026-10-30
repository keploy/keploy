//go:build composelib && !darwin

package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/moby/client"
	"go.uber.org/zap"
)

// The library path hands compose the command's --env-file: keploy's document
// needs a value only that file gives, and its project is the one the file
// names. It builds no connection: the client names a socket that is not there.
func TestLibraryComposeStackTakesTheCommandsEnvFile(t *testing.T) {
	unsetEnvVars(t, "COMPOSE_PROJECT_NAME", "COMPOSE_ENV_FILES", "COMPOSE_DISABLE_ENV_FILE", "APP_NAME")
	envFile := filepath.Join(t.TempDir(), "keploy.env")
	if err := os.WriteFile(envFile, []byte("APP_NAME=from-env-file\nCOMPOSE_PROJECT_NAME=envfileproj\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	api, err := client.New(client.WithHost("unix://" + filepath.Join(t.TempDir(), "no.sock")))
	if err != nil {
		t.Fatal(err)
	}
	a := &App{
		logger:         zap.NewNop(),
		cmd:            "docker compose --env-file " + envFile + " --project-directory " + t.TempDir() + " -f - up",
		composeContent: []byte(`services: {app: {image: app, container_name: "${APP_NAME:?}"}}`),
		composeAPI:     api,
	}
	stack, err := a.newLibraryComposeStack(context.Background())
	if err != nil {
		t.Fatalf("a document that needs the command's --env-file did not load: %v", err)
	}
	if got := stack.ProjectName(); got != "envfileproj" {
		t.Fatalf("ProjectName() = %q, want the env file's", got)
	}
}
