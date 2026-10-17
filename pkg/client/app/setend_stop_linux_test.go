package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// runNative runs script as keploy runs a native app, and stops it once it has
// written started, with cause: it returns how the run ended.
func runNative(t *testing.T, script string, cause error) models.AppError {
	t.Helper()
	dir := t.TempDir()
	a := NewApp(zap.NewNop(), "cd "+dir+" && "+script, nil, models.SetupOptions{CommandType: string(utils.Native)})
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	done := make(chan models.AppError, 1)
	go func() { done <- a.run(ctx) }()
	for {
		if _, err := os.Stat(filepath.Join(dir, "started")); err == nil {
			break
		}
		select {
		case appErr := <-done:
			t.Fatalf("the app ended before it started: %+v", appErr)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel(cause)
	select {
	case appErr := <-done:
		return appErr
	case <-time.After(2 * time.Minute):
		t.Fatal("the stop did not end")
	}
	return models.AppError{}
}

// The stop at a set's end is the teardown's, and says when it had to kill the
// app, with the grace it gave it (the switch off; under it, the stall rule,
// TestTheSwitchedSetEndStopKillsANativeAppOnlyOnTheStallRule); an app that
// exits on the interrupt is no kill. Another stop (the teardown's own, the
// user's) says nothing.
func TestTheStopAtASetsEndSaysWhenItKilledANativeApp(t *testing.T) {
	ignores := `trap "" INT; touch started; exec sleep 600`
	if appErr := runNative(t, ignores, models.ErrAppStoppedForSetEnd); appErr.AppErrorType != models.ErrCtxCanceled ||
		!strings.Contains(appErr.StopKilled, "so keploy killed it") || !strings.Contains(appErr.StopKilled, utils.TreeInterruptGrace.String()) {
		t.Fatalf("an app that ignores the interrupt: run ended %+v; want a kill named with its grace", appErr)
	}
	if appErr := runNative(t, `touch started; exec sleep 600`, models.ErrAppStoppedForSetEnd); appErr.StopKilled != "" {
		t.Fatalf("an app that exits on the interrupt: run says %q", appErr.StopKilled)
	}
	if appErr := runNative(t, ignores, context.Canceled); appErr.StopKilled != "" {
		t.Fatalf("the teardown's stop: run says %q", appErr.StopKilled)
	}
}

// Under the switch the stop at a set's end waits for the app's exit, not a
// number of seconds: a shutdown longer than the old grace is no kill; one that
// made no progress on its way out for the stall bound of keploy's runnable
// time (here shortened) is, and the stop says so. Another stop (the
// teardown's own, the user's) keeps its grace, and says nothing.
func TestTheSwitchedSetEndStopKillsANativeAppOnlyOnTheStallRule(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	budget := utils.StopStallBudget
	utils.StopStallBudget = 300 * time.Millisecond
	t.Cleanup(func() { utils.StopStallBudget = budget })
	slow := fmt.Sprintf(`trap 'sleep %d; exit 0' INT; touch started; while :; do sleep 0.2; done`, int(utils.TreeInterruptGrace/time.Second)+1)
	utils.StopStallBudget = utils.TreeInterruptGrace + 3*time.Second
	if appErr := runNative(t, slow, models.ErrAppStoppedForSetEnd); appErr.StopKilled != "" {
		t.Fatalf("a shutdown longer than the old grace: run says %q; want it waited for", appErr.StopKilled)
	}
	utils.StopStallBudget = 300 * time.Millisecond
	ignores := `trap "" INT; touch started; exec sleep 600`
	if appErr := runNative(t, ignores, models.ErrAppStoppedForSetEnd); appErr.AppErrorType != models.ErrCtxCanceled ||
		!strings.Contains(appErr.StopKilled, "made no progress on its way out for 300ms of keploy's runnable time after keploy interrupted it, so keploy killed it") {
		t.Fatalf("an app that ignores the interrupt: run ended %+v, says %q; want a kill named with the stall", appErr, appErr.StopKilled)
	}
	// The teardown's stop keeps its grace: with a stall bound of ten
	// minutes, it is over in seconds.
	utils.StopStallBudget = 10 * time.Minute
	start := time.Now()
	if appErr := runNative(t, ignores, context.Canceled); appErr.StopKilled != "" {
		t.Fatalf("the teardown's stop: run says %q", appErr.StopKilled)
	}
	if took := time.Since(start); took > time.Minute {
		t.Fatalf("the teardown's stop took %s: it waited on the app's exit, not its grace", took)
	}
}

// stateDocker answers each container inspect with the next of states (the
// last repeating; nil: no such container), and each kill with killErr,
// counting the kills it is asked for.
type stateDocker struct {
	docker.Client
	states  []*container.State
	inspect *int
	kills   *int
	killErr error
}

func (d stateDocker) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	i := *d.inspect
	if i >= len(d.states) {
		i = len(d.states) - 1
	}
	*d.inspect++
	if d.states[i] == nil {
		return container.InspectResponse{}, errdefs.NotFound(errors.New("no such container"))
	}
	return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{State: d.states[i]}}, nil
}

func (d stateDocker) ContainerKill(context.Context, string, string) error {
	*d.kills++
	return d.killErr
}

// A docker run app whose container still runs when the grace runs out is
// killed, and the stop says so. At a set's end, a container this stop saw
// that docker is removing, or has removed (the docker client of a --rm
// container waits for the removal), has exited: no kill is said. One it never
// saw says nothing (keploy may not have named the app's container), and its
// kill counts; so does a kill that failed other than for a container seen
// gone. The teardown's stop is as it was.
func TestTheDockerRunStopSaysWhenItKilled(t *testing.T) {
	running := &container.State{Status: "running", Running: true}
	gone := errdefs.NotFound(errors.New("no such container"))
	for _, tc := range []struct {
		name    string
		states  []*container.State
		killErr error
		setEnd  bool
		killed  bool
		kills   int
	}{
		{"running, at a set's end", []*container.State{running}, nil, true, true, 1},
		{"removing, at a set's end", []*container.State{{Status: "removing"}}, nil, true, false, 0},
		{"seen, then removed, at a set's end", []*container.State{running, nil}, nil, true, false, 0},
		{"never seen, at a set's end", []*container.State{nil}, gone, true, true, 1},
		{"removed as it was killed, at a set's end", []*container.State{running}, gone, true, false, 1},
		{"stopped as it was killed, at a set's end", []*container.State{running}, errdefs.Conflict(errors.New("container is not running")), true, false, 1},
		{"a kill that failed otherwise, at a set's end", []*container.State{running}, errors.New("context deadline exceeded"), true, true, 1},
		{"running, the teardown", []*container.State{running}, nil, false, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := exec.Command("sh", "-c", `trap "" INT; exec sleep 600`) // the docker client, which outlives the interrupt
			client.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := client.Start(); err != nil {
				t.Fatal(err)
			}
			reaped := make(chan struct{})
			go func() { _ = client.Wait(); close(reaped) }()
			defer func() { _ = syscall.Kill(-client.Process.Pid, syscall.SIGKILL); <-reaped }()
			inspects, kills := 0, 0
			a := &App{logger: zap.NewNop(), docker: stateDocker{states: tc.states, inspect: &inspects, kills: &kills, killErr: tc.killErr}}
			killed, err := a.stopDockerRun(client, tc.setEnd)
			if killed != tc.killed || kills != tc.kills {
				t.Fatalf("stopDockerRun said killed %v (err %v) after %d kills asked; want %v after %d", killed, err, kills, tc.killed, tc.kills)
			}
		})
	}
	exited := exec.Command("true")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	inspects, kills := 0, 0
	a := &App{logger: zap.NewNop(), docker: stateDocker{states: []*container.State{running}, inspect: &inspects, kills: &kills}}
	if killed, _ := a.stopDockerRun(exited, true); killed || kills != 0 {
		t.Fatalf("a docker client that had exited: killed %v after %d kills", killed, kills)
	}
}
