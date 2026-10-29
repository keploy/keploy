//go:build linux || darwin

package app

import (
	"context"
	"os/exec"
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
