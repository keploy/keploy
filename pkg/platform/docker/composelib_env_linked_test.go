//go:build composelib && !darwin

package docker

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/moby/moby/client"
)

// The library path loads keploy's document with the command's --env-file
// (ComposeRunnerOptions.EnvFiles), as compose would, and with none, with the
// project directory's .env. It builds no connection: the client names a
// socket that is not there.
func TestNewComposeRunnerReadsTheEnvFilesComposeReads(t *testing.T) {
	unsetEnv(t, "APP_NAME", "DB_PASS", composeProjectNameEnv, composeEnvFilesEnv, "COMPOSE_DISABLE_ENV_FILE")
	dir := t.TempDir()
	writeEnvFile(t, filepath.Join(dir, ".env"), "APP_NAME=from-dotenv\nCOMPOSE_PROJECT_NAME=dotenvproj\n")
	named := writeEnvFile(t, filepath.Join(t.TempDir(), "named.env"), "APP_NAME=from-named\nCOMPOSE_PROJECT_NAME=namedproj\n")
	api, err := client.New(client.WithHost("unix://" + filepath.Join(t.TempDir(), "no.sock")))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		envFiles              []string
		wantProject, wantName string
	}{
		{nil, "dotenvproj", "from-dotenv"},
		{[]string{named}, "namedproj", "from-named"},
	} {
		r, err := NewComposeRunner(context.Background(), api, ComposeRunnerOptions{Content: []byte(envDoc), WorkingDir: dir, EnvFiles: tc.envFiles})
		if err != nil {
			t.Fatalf("env files %v: %v", tc.envFiles, err)
		}
		if _, name, _ := loadedApp(t, r.project); r.ProjectName() != tc.wantProject || name != tc.wantName {
			t.Fatalf("env files %v: project %q, container %q; want %q, %q", tc.envFiles, r.ProjectName(), name, tc.wantProject, tc.wantName)
		}
	}
}
