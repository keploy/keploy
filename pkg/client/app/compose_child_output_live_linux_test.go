package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/platform/docker"
	keployLog "go.keploy.io/server/v3/utils/log"
)

// A real `docker compose` child, against a real daemon, with its containers
// on the log driver none (no `docker logs` to read): what compose prints for
// each container, in its real line format (two names of different lengths, so
// padded), is kept in the containers' tails, and recentAppLogs reads the app
// service's tail cut of the registered value.
//
// It starts containers, so it runs only where KEPLOY_TEST_REAL_DOCKER=1 says
// they may be started (the box and the CI lane), not under -short, and only
// with a `docker compose` that answers.
func TestRecentAppLogsReadsARealComposeChildsTail(t *testing.T) {
	if os.Getenv("KEPLOY_TEST_REAL_DOCKER") != "1" || testing.Short() {
		t.Skip("starts containers: set KEPLOY_TEST_REAL_DOCKER=1, without -short")
	}
	if out, err := exec.Command("docker", "compose", "version").CombinedOutput(); err != nil {
		t.Skipf("no docker compose: %v: %s", err, out)
	}
	shellOutPath(t)
	keployLog.SetRedactor(appCutter{})
	t.Cleanup(func() { keployLog.SetRedactor(nil) })

	project := fmt.Sprintf("keploychildtail%d", os.Getpid())
	doc := fmt.Sprintf(`services:
  app:
    image: busybox:1.36
    command: ["sh", "-c", "sleep 2; echo listening; echo DB_PASSWORD=%[1]s >&2; exit 3"]
    logging: {driver: none}
  db:
    image: busybox:1.36
    container_name: %[2]s-database
    command: ["sh", "-c", "echo db ready; sleep 60"]
    init: true
    logging: {driver: none}
`, plantedAppValue, project)
	t.Cleanup(func() {
		down := exec.Command("docker", "compose", "-p", project, "-f", "-", "down", "-t", "0", "--remove-orphans")
		down.Stdin = strings.NewReader(doc)
		if out, err := down.CombinedOutput(); err != nil {
			t.Logf("compose down: %v: %s", err, out)
		}
	})

	a := childComposeApp()
	a.cmd = "docker compose -p " + project + " -f - up --abort-on-container-exit"
	a.composeContent = []byte(doc)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	killGroup := func(cmd *exec.Cmd) func() error {
		return func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	}
	// The app exits 3, so the child's error is expected; it is said with any
	// failure below, so a failed pull is not read as a missing tail.
	cmdErr := a.executeCommand(ctx, a.cmd, nil, killGroup)

	// The app's stdout and stderr lines are on two streams, compose's and
	// keploy's copies of each; their order across the streams is not kept.
	got := strings.Split(a.recentAppLogs(ctx), "\n")
	sort.Strings(got)
	if want := []string{"DB_PASSWORD=***", "listening"}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("recentAppLogs lines = %q, want %q in any order (the child: %v)", got, want, cmdErr.Err)
	}
	if got := docker.AppOutputTail(project, project+"-database"); !bytes.Equal(got, []byte("db ready\n")) {
		all, _ := docker.AppOutputTails()
		t.Fatalf("the container_name service's tail = %q; tails held: %+v (the child: %v)", got, all, cmdErr.Err)
	}
}
