//go:build composelib && !darwin

// Tagged: these drive the compose LIBRARY path (App.useComposeLibrary).
// Without the tag that path is never taken, so they would be failing for
// the absence of the thing they pin rather than for its behaviour.

package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	dockertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// These drive the WHOLE in-memory compose path against a real docker daemon —
// dispatch, bring-up, error classification and teardown together. The unit
// tests above fake the stack so they can run anywhere; this is the only place
// the pieces are proven to fit.
//
// Skipped when no daemon is reachable, and under -short.
func liveApp(t *testing.T, project, composeYAML, cmdSuffix, appContainer string) *App {
	t.Helper()
	if testing.Short() {
		t.Skip("live docker test skipped under -short")
	}
	dc, err := docker.New(zap.NewNop(), &config.Config{})
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := dc.Ping(ctx); err != nil {
		t.Skipf("no docker daemon reachable: %v", err)
	}

	a := &App{
		logger:          zap.NewNop(),
		docker:          dc,
		kind:            utils.DockerCompose,
		composeContent:  []byte(composeYAML),
		cmd:             fmt.Sprintf("docker compose -p %s -f - up %s", project, cmdSuffix),
		composeServices: []string{"app"},
		container:       appContainer,
	}
	t.Cleanup(func() { a.ComposeDown() })
	return a
}

// projectContainers describes what the daemon still holds for a project.
func projectContainers(t *testing.T, a *App, project string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	list, err := a.docker.ContainerList(ctx, dockertypes.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", "com.docker.compose.project="+project)),
	})
	if err != nil {
		t.Fatalf("list project containers: %v", err)
	}
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, fmt.Sprintf("%s[%s/%s]", strings.Join(c.Names, ","), c.State, c.Status))
	}
	return out
}

// TestLiveAppRunsAndTearsDownAnInMemoryStack is the happy path end to end: the
// stack starts, the app runs to completion, the run reports a clean exit, and
// ComposeDown leaves nothing behind.
func TestLiveAppRunsAndTearsDownAnInMemoryStack(t *testing.T) {
	project := fmt.Sprintf("keployapplive%d", time.Now().UnixNano()%100000)
	name := project + "-appc"
	yaml := fmt.Sprintf(`
services:
  app:
    image: busybox:1.36
    container_name: %s
    command: ["sh", "-c", "echo up-and-running; exit 0"]
`, name)

	a := liveApp(t, project, yaml, "--abort-on-container-exit --exit-code-from app", name)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	cmdErr := a.runComposeInProcess(ctx, a.ComposeDown)

	if cmdErr.Err != nil {
		t.Fatalf("clean run reported %s error: %v", cmdErr.Type, cmdErr.Err)
	}
	a.ComposeDown()
	if left := projectContainers(t, a, project); len(left) != 0 {
		t.Fatalf("teardown left %d container(s) behind for project %s: %v", len(left), project, left)
	}
}

// TestLiveAppPropagatesTheAppExitCode pins the contract run() depends on: a
// failing app is a Runtime error whose exit status survives, because that is
// what the mock record/replay flows mirror to their own exit status.
func TestLiveAppPropagatesTheAppExitCode(t *testing.T) {
	project := fmt.Sprintf("keployapplive%d", time.Now().UnixNano()%100000)
	name := project + "-appc"
	yaml := fmt.Sprintf(`
services:
  app:
    image: busybox:1.36
    container_name: %s
    command: ["sh", "-c", "echo failing; exit 3"]
`, name)

	a := liveApp(t, project, yaml, "--abort-on-container-exit --exit-code-from app", name)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	cmdErr := a.runComposeInProcess(ctx, a.ComposeDown)

	if cmdErr.Type != utils.Runtime {
		t.Fatalf("type = %q, want Runtime (run() maps it to ErrUnExpected); err=%v", cmdErr.Type, cmdErr.Err)
	}
	if got := exitCodeFromErr(cmdErr.Err); got != 3 {
		t.Fatalf("exitCodeFromErr = %d, want 3 — the app's status stopped propagating (err=%v)", got, cmdErr.Err)
	}
}

// TestLiveAppTearsDownOnContextCancel is the Ctrl-C / teardown-drain shape: a
// long-running stack is cancelled mid-run, and keploy's OWN bounded teardown
// must bring it down rather than leaving compose's unbounded graceful stop to
// overrun the drain budget.
func TestLiveAppTearsDownOnContextCancel(t *testing.T) {
	project := fmt.Sprintf("keployapplive%d", time.Now().UnixNano()%100000)
	name := project + "-appc"
	yaml := fmt.Sprintf(`
services:
  app:
    image: busybox:1.36
    container_name: %s
    command: ["sh", "-c", "echo serving; sleep 600"]
`, name)

	a := liveApp(t, project, yaml, "", name)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan utils.CmdError, 1)
	go func() { done <- a.runComposeInProcess(ctx, a.ComposeDown) }()

	// Wait until the stack is actually up before cancelling, so this exercises
	// teardown rather than a race with start-up.
	deadline := time.Now().Add(120 * time.Second)
	for len(projectContainers(t, a, project)) == 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if len(projectContainers(t, a, project)) == 0 {
		cancel()
		<-done
		t.Fatal("the stack never came up")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("the run never returned after cancellation; this is the 'goroutine is ignoring context " +
			"cancellation' teardown-drain failure")
	}

	if left := projectContainers(t, a, project); len(left) != 0 {
		t.Fatalf("cancellation left %d container(s) for project %s: %v", len(left), project, left)
	}
}

// TestLiveAppSweepFindsStragglersOnTheLibraryPath proves the teardown sweep can
// still see the project's containers now that it reads the compose library
// rather than a `docker compose ps` subprocess. A sweep that silently finds
// nothing is indistinguishable from a clean teardown.
func TestLiveAppSweepFindsStragglersOnTheLibraryPath(t *testing.T) {
	project := fmt.Sprintf("keployapplive%d", time.Now().UnixNano()%100000)
	name := project + "-appc"
	yaml := fmt.Sprintf(`
services:
  app:
    image: busybox:1.36
    container_name: %s
    command: ["sh", "-c", "exit 0"]
`, name)

	a := liveApp(t, project, yaml, "--abort-on-container-exit --exit-code-from app", name)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if cmdErr := a.runComposeInProcess(ctx, func() {}); cmdErr.Err != nil {
		t.Fatalf("run failed: %v", cmdErr.Err)
	}

	states := a.composeServiceStates(ctx)
	if len(states) == 0 {
		t.Fatal("composeServiceStates saw nothing for a live project; the dependency-failure classifier " +
			"and the teardown sweep both go blind")
	}
	var sawApp bool
	for _, st := range states {
		if st.Service == "app" {
			sawApp = true
			if st.ID == "" {
				t.Error("the state row carries no container ID, so the sweep has nothing to remove")
			}
			if !strings.Contains(st.Labels, "com.docker.compose.oneoff") {
				t.Errorf("the oneoff label did not survive into the state row: %q", st.Labels)
			}
		}
	}
	if !sawApp {
		t.Fatalf("no row for the app service in %+v", states)
	}

	ids := a.sweepStragglingProjectContainers()
	if len(ids) == 0 {
		t.Fatal("the sweep found no straggler for a project whose container is still present")
	}
}

// projectNetworks names the networks the daemon still holds for a project.
func projectNetworks(t *testing.T, a *App, project string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	list, err := a.docker.NetworkList(ctx, network.ListOptions{Filters: filters.NewArgs(filters.Arg("label", "com.docker.compose.project="+project))})
	if err != nil {
		t.Fatalf("list project networks: %v", err)
	}
	out := make([]string, 0, len(list))
	for _, n := range list {
		out = append(out, n.Name)
	}
	return out
}

// TestLiveComposeDownWaitsWhileTheProjectMoves is the down a loaded host
// makes slow, against a real daemon: five services stopped one after another,
// each taking its whole stop grace (a PID 1 that does not stop on SIGTERM).
// The down takes twice its stall bound; the daemon's events for the project
// keep it going to its end, and it leaves nothing behind.
func TestLiveComposeDownWaitsWhileTheProjectMoves(t *testing.T) {
	project := fmt.Sprintf("keploydownlive%d", time.Now().UnixNano()%100000)
	name := project + "-s1"
	var yaml strings.Builder
	yaml.WriteString("services:\n")
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(&yaml, "  s%d:\n    image: busybox:1.36\n    command: [\"sleep\", \"600\"]\n", i)
		if i == 1 {
			fmt.Fprintf(&yaml, "    container_name: %s\n", name)
		}
		if i < 5 {
			fmt.Fprintf(&yaml, "    depends_on: [s%d]\n", i+1)
		}
	}
	a := liveApp(t, project, yaml.String(), "", name)
	a.composeServices = []string{"s1", "s2", "s3", "s4", "s5"}
	core, logs := observer.New(zap.WarnLevel)
	a.logger = zap.New(core)
	stall := 2500 * time.Millisecond
	old := composeDownStall
	composeDownStall = stall
	t.Cleanup(func() { composeDownStall = old })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan utils.CmdError, 1)
	go func() { done <- a.runComposeInProcess(ctx, func() {}) }()
	// All five running, not only created: a down removes a created
	// container at once, and the down would not be slow.
	running := func() int {
		n := 0
		for _, c := range projectContainers(t, a, project) {
			if strings.Contains(c, "[running/") {
				n++
			}
		}
		return n
	}
	deadline := time.Now().Add(120 * time.Second)
	for running() < 5 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if n := running(); n < 5 {
		t.Fatalf("the stack never came up: %d of 5 containers running: %v", n, projectContainers(t, a, project))
	}

	start := time.Now()
	a.ComposeDown()
	took := time.Since(start)
	cancel()
	<-done

	if cut := logs.FilterMessageSnippet("made no progress").All(); len(cut) != 0 {
		t.Fatalf("keploy cut a down whose project was moving (it ran %s against a stall bound of %s): %v", took, stall, cut[0].Message)
	}
	if took < stall {
		t.Fatalf("the down took %s, under its stall bound of %s: the stack did not stop slowly enough to prove anything", took, stall)
	}
	if left := projectContainers(t, a, project); len(left) != 0 {
		t.Fatalf("the down left containers: %v", left)
	}
	if left := projectNetworks(t, a, project); len(left) != 0 {
		t.Fatalf("the down left networks: %v", left)
	}
}

// TestLiveNetworkSweepRemovesTheProjectsNetworks pins the sweep's filter
// against a real daemon: a network under the project's compose label goes,
// another project's stays, and one a running container still uses stays too
// (the daemon refuses it) without stopping the sweep.
func TestLiveNetworkSweepRemovesTheProjectsNetworks(t *testing.T) {
	project := fmt.Sprintf("keploynetlive%d", time.Now().UnixNano()%100000)
	a := liveApp(t, project, "services: {}\n", "", "")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	mk := func(name, proj string) string {
		resp, err := a.docker.NetworkCreate(ctx, name, network.CreateOptions{Labels: map[string]string{"com.docker.compose.project": proj}})
		if err != nil {
			t.Fatalf("create network %s: %v", name, err)
		}
		t.Cleanup(func() { _ = a.docker.NetworkRemove(context.Background(), resp.ID) })
		return resp.ID
	}
	mk(project+"_gone", project)
	inUse := mk(project+"_inuse", project)
	mk(project+"x_other", project+"x")
	c, err := a.docker.ContainerCreate(ctx, &dockertypes.Config{Image: "busybox:1.36", Cmd: []string{"sleep", "600"}},
		&dockertypes.HostConfig{NetworkMode: dockertypes.NetworkMode(inUse)}, nil, nil, project+"-user")
	if err != nil {
		t.Fatalf("create the user's container: %v", err)
	}
	t.Cleanup(func() {
		_ = a.docker.ContainerRemove(context.Background(), c.ID, dockertypes.RemoveOptions{Force: true})
	})
	if err := a.docker.ContainerStart(ctx, c.ID, dockertypes.StartOptions{}); err != nil {
		t.Fatalf("start the user's container: %v", err)
	}

	a.sweepProjectNetworks(project)

	got := strings.Join(projectNetworks(t, a, project), ",")
	if got != project+"_inuse" {
		t.Fatalf("the project's networks after the sweep: %q; want only the one in use, %s_inuse", got, project)
	}
	if other := projectNetworks(t, a, project+"x"); len(other) != 1 {
		t.Fatalf("the sweep touched another project's network: %v left", other)
	}
}
