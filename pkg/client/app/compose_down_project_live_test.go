//go:build dockerlive

package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dockertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// keploy's down of a compose app takes down the project the app's up ran,
// however the command named it, and no other. Each case runs a stack of the
// same compose file in the directory's default project (the user's own, say
// their dev stack) beside the app's: keploy's down must leave it running. A
// project named in a way only the up's shell read (an assignment in the
// command) or that keploy's calls do not carry (--env-file) was taken down by
// the down resolving the default project instead: the user's stack, and the
// app's left running.
func TestLiveComposeDownTakesDownTheProjectTheUpRan(t *testing.T) {
	if testing.Short() {
		t.Skip("live docker test skipped under -short")
	}
	dc, err := docker.New(zap.NewNop(), &config.Config{})
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}
	pctx, pcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer pcancel()
	if _, err := dc.Ping(pctx); err != nil {
		t.Skipf("no docker daemon reachable: %v", err)
	}
	const composeYAML = "services:\n  app:\n    image: busybox:1.36\n    command: [\"sleep\", \"600\"]\n"

	for _, tc := range []struct {
		name string
		// up is the user's compose command, the up keploy ran (the shell
		// reads it); file and env are what else sets the project.
		up        func(project string) string
		withName  bool   // the compose file names the project (name:)
		dotEnv    bool   // a .env beside it sets COMPOSE_PROJECT_NAME
		envFile   string // an --env-file that sets it
		assignsIt bool   // the command sets it in an assignment
	}{
		{name: "-p", up: func(p string) string { return "docker compose -p " + p + " -f docker-compose.yml up -d" }},
		{name: "--project-name", up: func(p string) string { return "docker compose --project-name " + p + " -f docker-compose.yml up -d" }},
		{name: "name: in the compose file", up: func(string) string { return "docker compose -f docker-compose.yml up -d" }, withName: true},
		{name: "COMPOSE_PROJECT_NAME in .env", up: func(string) string { return "docker compose -f docker-compose.yml up -d" }, dotEnv: true},
		{name: "--env-file", up: func(string) string { return "docker compose --env-file proj.env -f docker-compose.yml up -d" }, envFile: "proj.env"},
		{name: "an assignment in the command", up: func(p string) string {
			return "COMPOSE_PROJECT_NAME=" + p + " docker compose -f docker-compose.yml up -d"
		}, assignsIt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			project := fmt.Sprintf("keployd2%d", time.Now().UnixNano()%1000000)
			// The directory's default project: its base name, as compose
			// normalizes it.
			userProject := strings.ToLower(filepath.Base(dir))
			yaml := composeYAML
			if tc.withName {
				yaml = "name: " + project + "\n" + yaml
			}
			if err := os.WriteFile("docker-compose.yml", []byte(yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.dotEnv {
				if err := os.WriteFile(".env", []byte("COMPOSE_PROJECT_NAME="+project+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.envFile != "" {
				if err := os.WriteFile(tc.envFile, []byte("COMPOSE_PROJECT_NAME="+project+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			compose := func(args ...string) {
				t.Helper()
				out, err := exec.Command("docker", append([]string{"compose"}, args...)...).CombinedOutput()
				if err != nil {
					t.Fatalf("docker compose %v: %v\n%s", args, err, out)
				}
			}
			t.Cleanup(func() {
				_ = exec.Command("docker", "compose", "-p", project, "-f", filepath.Join(dir, "docker-compose.yml"), "down", "-t", "0").Run()
				_ = exec.Command("docker", "compose", "-p", userProject, "-f", filepath.Join(dir, "docker-compose.yml"), "down", "-t", "0").Run()
			})
			// The user's own stack of the file, in the default project (with
			// the .env or name: present, compose would name it the app's, so
			// it is started explicitly under the default name).
			compose("-p", userProject, "-f", "docker-compose.yml", "up", "-d")
			// The app's up, as keploy runs the command: through a shell.
			up := tc.up(project)
			if out, err := exec.Command("sh", "-c", up).CombinedOutput(); err != nil {
				t.Fatalf("%s: %v\n%s", up, err, out)
			}
			appContainer := project + "-app-1"

			a := &App{
				logger:          zap.NewNop(),
				docker:          dc,
				kind:            utils.DockerCompose,
				composeFile:     "docker-compose.yml",
				cmd:             strings.TrimSuffix(up, " -d"),
				container:       appContainer,
				composeServices: []string{"app"},
			}
			a.ComposeDown()

			if left := liveProjectContainers(t, a, project); len(left) != 0 {
				t.Fatalf("the app's project %s was not taken down: %v", project, left)
			}
			if mine := liveProjectContainers(t, a, userProject); len(mine) != 1 || !strings.Contains(mine[0], "running") {
				t.Fatalf("the user's own stack, project %s, was touched by keploy's down: %v", userProject, mine)
			}
		})
	}
}

// liveProjectContainers describes the containers the daemon holds for a
// compose project: name[state/status].
func liveProjectContainers(t *testing.T, a *App, project string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	list, err := a.docker.ContainerList(ctx, dockertypes.ListOptions{All: true,
		Filters: filters.NewArgs(filters.Arg("label", "com.docker.compose.project="+project))})
	if err != nil {
		t.Fatalf("list project containers: %v", err)
	}
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, fmt.Sprintf("%s[%s/%s]", strings.Join(c.Names, ","), c.State, c.Status))
	}
	return out
}
