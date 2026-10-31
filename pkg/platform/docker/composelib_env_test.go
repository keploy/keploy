package docker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"
)

// envDoc needs a value only an env file gives: compose refuses it when
// APP_NAME is unset, and takes DB_PASS for the bare entry from the same
// environment.
const envDoc = `services:
  app:
    image: app
    container_name: "${APP_NAME:?set APP_NAME}"
    environment: [DB_PASS]
`

// unsetEnv unsets each of keys for one test, restored after it: an empty
// value is not unset to compose (an env file then cannot set it, and
// COMPOSE_DISABLE_ENV_FILE="" does not parse).
func unsetEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
}

func writeEnvFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// loadedApp is what the document's app service resolved to: its project, its
// container's name and the DB_PASS it gets.
func loadedApp(t *testing.T, p *composetypes.Project) (project, container, dbPass string) {
	t.Helper()
	svc, err := p.GetService("app")
	if err != nil {
		t.Fatal(err)
	}
	if v := svc.Environment["DB_PASS"]; v != nil {
		dbPass = *v
	}
	return p.Name, svc.ContainerName, dbPass
}

// The loader resolves keploy's own document against the environment
// `docker compose` resolves it against, on both paths (NewComposeRunner and
// StartComposeChildOutput load through it): keploy's environment, which
// wins, then the env files: the command's --env-file values, else
// COMPOSE_ENV_FILES, else the project directory's .env, unless
// COMPOSE_DISABLE_ENV_FILE. A .env's COMPOSE_PROJECT_NAME names the project
// below -p. A named env file that is missing fails, as it fails compose.
func TestTheLoaderReadsTheEnvFilesComposeReads(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Proj.Dir_1")
	writeEnvFile(t, filepath.Join(dir, ".env"), "APP_NAME=from-dotenv\nDB_PASS=dotenv-pass\nCOMPOSE_PROJECT_NAME=dotenvproj\n")
	named := writeEnvFile(t, filepath.Join(t.TempDir(), "named.env"), "APP_NAME=from-named\nDB_PASS=named-pass\n")
	cwd := t.TempDir()
	writeEnvFile(t, filepath.Join(cwd, "rel.env"), "APP_NAME=from-relative\n")
	t.Chdir(cwd)
	home := t.TempDir()
	writeEnvFile(t, filepath.Join(home, "home.env"), "APP_NAME=from-home\n")

	for _, tc := range []struct {
		name, given string
		envFiles    []string
		env         map[string]string
		project     string
		container   string
		dbPass      string
	}{
		{name: "the project's .env", project: "dotenvproj", container: "from-dotenv", dbPass: "dotenv-pass"},
		{name: "keploy's environment wins", env: map[string]string{"APP_NAME": "from-env", "DB_PASS": "env-pass", composeProjectNameEnv: "envproj"},
			project: "envproj", container: "from-env", dbPass: "env-pass"},
		{name: "-p beats the .env's project", given: "flagproj", project: "flagproj", container: "from-dotenv", dbPass: "dotenv-pass"},
		{name: "--env-file replaces the .env", envFiles: []string{named}, project: "projdir_1", container: "from-named", dbPass: "named-pass"},
		{name: "a later --env-file wins", envFiles: []string{named, "rel.env"}, project: "projdir_1", container: "from-relative", dbPass: "named-pass"},
		{name: "COMPOSE_ENV_FILES with no --env-file", env: map[string]string{composeEnvFilesEnv: named}, project: "projdir_1", container: "from-named", dbPass: "named-pass"},
		{name: "~ is the home directory", envFiles: []string{"~/home.env"}, env: map[string]string{"HOME": home, "USERPROFILE": home}, project: "projdir_1", container: "from-home"},
		{name: "--env-file beats COMPOSE_ENV_FILES", envFiles: []string{"rel.env"}, env: map[string]string{composeEnvFilesEnv: named}, project: "projdir_1", container: "from-relative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unsetEnv(t, "APP_NAME", "DB_PASS", composeProjectNameEnv, composeEnvFilesEnv, "COMPOSE_DISABLE_ENV_FILE")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			p, err := loadComposeProject(context.Background(), []byte(envDoc), tc.given, dir, tc.envFiles)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			project, container, dbPass := loadedApp(t, p)
			if project != tc.project || container != tc.container || dbPass != tc.dbPass {
				t.Fatalf("loaded (project %q, container %q, DB_PASS %q), want (%q, %q, %q)",
					project, container, dbPass, tc.project, tc.container, tc.dbPass)
			}
		})
	}

	for _, tc := range []struct {
		name     string
		envFiles []string
		env      map[string]string
	}{
		{name: "COMPOSE_DISABLE_ENV_FILE", env: map[string]string{"COMPOSE_DISABLE_ENV_FILE": "1"}},
		{name: "a missing --env-file", envFiles: []string{filepath.Join(cwd, "missing.env")}},
	} {
		t.Run("refused: "+tc.name, func(t *testing.T) {
			unsetEnv(t, "APP_NAME", "DB_PASS", composeProjectNameEnv, composeEnvFilesEnv, "COMPOSE_DISABLE_ENV_FILE")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if p, err := loadComposeProject(context.Background(), []byte(envDoc), "", dir, tc.envFiles); err == nil {
				t.Fatalf("loaded project %q; want compose's refusal", p.Name)
			}
		})
	}
}

// With no PWD in keploy's environment, the document still reads the current
// directory as ${PWD}, as compose adds it.
func TestTheLoaderAddsPWDWhenThereIsNone(t *testing.T) {
	unsetEnv(t, composeEnvFilesEnv, "COMPOSE_DISABLE_ENV_FILE")
	t.Chdir(t.TempDir())
	unsetEnv(t, "PWD")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	doc := []byte("services: {app: {image: app, environment: {WHERE: \"${PWD:?}\"}}}")
	p, err := loadComposeProject(context.Background(), doc, "pwd", t.TempDir(), nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	svc, err := p.GetService("app")
	if err != nil {
		t.Fatal(err)
	}
	if got := svc.Environment["WHERE"]; got == nil || *got != wd {
		t.Fatalf("${PWD} = %v, want the current directory %q", got, wd)
	}
}

// On the shell-out path the tail follows the env files too: a document whose
// container name and project come from the project's .env keeps that
// container's lines under that project, where recentAppLogs reads them.
func TestComposeChildOutputLoadsWithTheProjectsDotEnv(t *testing.T) {
	ForgetAppOutput()
	t.Cleanup(ForgetAppOutput)
	unsetEnv(t, "APP_NAME", "DB_PASS", composeProjectNameEnv, composeEnvFilesEnv, "COMPOSE_DISABLE_ENV_FILE")
	dir := t.TempDir()
	writeEnvFile(t, filepath.Join(dir, ".env"), "APP_NAME=from-dotenv\nCOMPOSE_PROJECT_NAME=dotenvproj\n")
	o, err := StartComposeChildOutput(context.Background(), []byte(envDoc), "", dir, nil)
	if err != nil {
		t.Fatalf("StartComposeChildOutput: %v", err)
	}
	if o.Project() != "dotenvproj" {
		t.Fatalf("Project() = %q, want the .env's", o.Project())
	}
	write(t, o.Stream(), "from-dotenv  | listening\n")
	if got := string(ServiceOutputTail("dotenvproj", "app")); got != "listening\n" {
		t.Fatalf("app's tail = %q, want the line of the container the .env names", got)
	}
}
