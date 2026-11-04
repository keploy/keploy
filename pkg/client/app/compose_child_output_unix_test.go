//go:build linux || darwin

package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	keployLog "go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// shellOutPath has recentAppLogs and executeCommand take the decisions of a
// build without the compose library, -tags composelib or not, and gives each
// row fresh tails.
func shellOutPath(t *testing.T) {
	t.Helper()
	was := composeLibrarySupported
	composeLibrarySupported = false
	t.Cleanup(func() { composeLibrarySupported = was })
	docker.ForgetAppOutput()
	t.Cleanup(docker.ForgetAppOutput)
	unsetEnvVars(t, "COMPOSE_ENV_FILES", "COMPOSE_DISABLE_ENV_FILE")
}

func noCancel(_ *exec.Cmd) func() error { return func() error { return nil } }

// childComposeApp is keploy's own compose stack in a build without the
// compose library: its document is run by a `docker compose` child, here a
// script that prints as compose prints.
func childComposeApp() *App {
	return &App{
		logger:         zap.NewNop(),
		kind:           utils.DockerCompose,
		cmd:            "docker compose -p childapp -f - up",
		composeContent: []byte("services: {app: {image: app}, db: {image: db, container_name: my-db}}"),
		composeService: "app",
		container:      "app",
		docker:         noDaemon{},
	}
}

// In a build without the compose library, keploy's own compose stack runs
// as a `docker compose` child, and recentAppLogs reads the app service's tail
// of what that child printed, cut, in the command's project, and never asks
// the daemon (noDaemon panics on any call). Each run is one Up: a set whose
// app printed nothing reads nothing, and the next set's one panic line is all
// it reads. A new document drops the last Up's tail.
func TestRecentAppLogsReadsTheComposeChildsTail(t *testing.T) {
	shellOutPath(t)
	keployLog.SetRedactor(appCutter{})
	t.Cleanup(func() { keployLog.SetRedactor(nil) })
	a := childComposeApp()
	ctx := context.Background()
	if got := a.recentAppLogs(ctx); got != "" {
		t.Fatalf("with no Up yet, recentAppLogs = %q; want nothing", got)
	}

	set1 := `printf 'Attaching to app-1, my-db\n'; printf 'my-db  | ready\n'; printf 'app-1  | listening on :8080\n'; ` +
		`printf 'app-1  | panic: DB_PASSWORD=%s\n' '` + plantedAppValue + `' >&2; printf ' Container childapp-app-1  Exited\n' >&2`
	if cmdErr := a.executeCommand(ctx, set1, nil, noCancel); cmdErr.Err != nil {
		t.Fatalf("set 1: %v", cmdErr.Err)
	}
	if got, want := a.recentAppLogs(ctx), "listening on :8080\npanic: DB_PASSWORD=***"; got != want {
		t.Fatalf("set 1: recentAppLogs = %q, want %q", got, want)
	}
	if got := string(docker.AppOutputTail("childapp", "my-db")); got != "ready\n" {
		t.Fatalf("my-db's tail in the command's project = %q", got)
	}

	if cmdErr := a.executeCommand(ctx, `printf 'Attaching to app-1\n'`, nil, noCancel); cmdErr.Err != nil {
		t.Fatalf("set 2: %v", cmdErr.Err)
	}
	if got := a.recentAppLogs(ctx); got != "" {
		t.Fatalf("set 2 printed nothing, but recentAppLogs = %q", got)
	}
	if cmdErr := a.executeCommand(ctx, `printf 'app-1  | panic: set 3\n'`, nil, noCancel); cmdErr.Err != nil {
		t.Fatalf("set 3: %v", cmdErr.Err)
	}
	if got := a.recentAppLogs(ctx); got != "panic: set 3" {
		t.Fatalf("set 3: recentAppLogs = %q, want its one panic line", got)
	}

	a.setComposeSource("", a.composeContent, nil)
	if got := a.recentAppLogs(ctx); got != "" {
		t.Fatalf("a new document, not yet run: recentAppLogs = %q; want nothing", got)
	}
}

// The child's project is the one its compose takes: the command's
// COMPOSE_PROJECT_NAME assignment, normalized, when it gives no -p.
func TestComposeChildTeeTakesTheCommandsProjectAssignment(t *testing.T) {
	shellOutPath(t)
	a := childComposeApp()
	a.cmd = "COMPOSE_PROJECT_NAME=Assigned docker compose -f - up"
	if opts := a.composeChildTee(context.Background()); len(opts) != 1 {
		t.Fatalf("composeChildTee gave %d options, want the tee", len(opts))
	}
	if got := a.composeOutputProject(); got != "assigned" {
		t.Fatalf("project = %q, want the command's assignment, normalized", got)
	}
}

// The child's project follows compose's precedence in the command: -p over
// an assignment (in quotes, as the shell passes it), and with neither, the
// --project-directory's name, normalized as compose normalizes it, or the
// project an --env-file names. A document that needs a value only that env
// file gives loads, and keeps its tail.
func TestComposeChildTeeTakesTheProjectComposeTakes(t *testing.T) {
	shellOutPath(t)
	unsetEnvVars(t, "COMPOSE_PROJECT_NAME", "COMPOSE_ENV_FILES", "COMPOSE_DISABLE_ENV_FILE", "APP_NAME")
	projDir := filepath.Join(t.TempDir(), "My.Proj_Dir")
	if err := os.Mkdir(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(t.TempDir(), "keploy.env")
	if err := os.WriteFile(envFile, []byte("APP_NAME=from-env-file\nCOMPOSE_PROJECT_NAME=envfileproj\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	needsEnvFile := []byte(`services: {app: {image: app, container_name: "${APP_NAME:?}"}}`)
	for _, tc := range []struct {
		name, cmd string
		doc       []byte
		want      string
	}{
		{"-p over an assignment", "COMPOSE_PROJECT_NAME=assigned docker compose -p flagproj -f - up", nil, "flagproj"},
		{"quoted -p", `docker compose -p "quoted_proj" -f - up`, nil, "quoted_proj"},
		{"--project-directory", "docker compose --project-directory " + projDir + " -f - up", nil, "myproj_dir"},
		{"--env-file", "docker compose --env-file " + envFile + " -f - up", needsEnvFile, "envfileproj"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := childComposeApp()
			a.cmd = tc.cmd
			if tc.doc != nil {
				a.composeContent = tc.doc
			}
			if opts := a.composeChildTee(context.Background()); len(opts) != 1 {
				t.Fatalf("composeChildTee gave %d options, want the tee", len(opts))
			}
			if got := a.composeOutputProject(); got != tc.want {
				t.Fatalf("project = %q, want %q", got, tc.want)
			}
		})
	}
}

// A document the loader refuses is said once per document, not at every Up
// (each test set's), and said again for the next document.
func TestComposeChildTeeSaysARefusedDocumentOncePerDocument(t *testing.T) {
	shellOutPath(t)
	core, logs := observer.New(zap.WarnLevel)
	a := childComposeApp()
	a.logger = zap.New(core)
	a.composeContent = []byte("services: [not, a, mapping]")
	for range 3 {
		if opts := a.composeChildTee(context.Background()); opts != nil {
			t.Fatal("a document the loader refuses got a tee")
		}
	}
	if logs.Len() != 1 {
		t.Fatalf("three Ups of one refused document said it %d times, want once: %v", logs.Len(), logs.All())
	}
	a.setComposeSource("", []byte("services: [still, not, a, mapping]"), nil)
	a.composeChildTee(context.Background())
	if logs.Len() != 2 {
		t.Fatalf("a new refused document was said %d times in all, want once more (2): %v", logs.Len(), logs.All())
	}
}

// A user's own command (no compose document of keploy's) gets no tee and
// starts no run, and keeps recentAppLogs on `docker logs`, in a build without
// the library as with it. A document the loader refuses gets no tee either.
func TestComposeChildTeeIsOnlyForKeploysOwnDocument(t *testing.T) {
	shellOutPath(t)
	ctx := context.Background()
	core, logs := observer.New(zap.WarnLevel)
	for _, kind := range []utils.CmdType{utils.DockerRun, utils.DockerStart, utils.DockerCompose, utils.FromContainer} {
		d := &logsDaemon{}
		a := &App{logger: zap.New(core), kind: kind, cmd: "docker compose up", container: "app", docker: d}
		if opts := a.composeChildTee(ctx); opts != nil || a.composeChildOut.Load() != nil || logs.Len() != 0 {
			t.Fatalf("%s: a user's own command got a tee, or a warning: %v", kind, logs.All())
		}
		if got := a.recentAppLogs(ctx); got != "from docker logs" || len(d.asked) != 1 {
			t.Errorf("%s: recentAppLogs = %q, asking docker logs of %v; want docker logs of app", kind, got, d.asked)
		}
	}
	a := childComposeApp()
	a.logger = zap.New(core)
	a.composeContent = []byte("services: [not, a, mapping]")
	if opts := a.composeChildTee(ctx); opts != nil || a.composeOutputProject() != "" || logs.Len() != 1 {
		t.Fatalf("a document the loader refuses got a tee (project %q), or not one warning: %v", a.composeOutputProject(), logs.All())
	}
}
