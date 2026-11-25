//go:build linux

package http

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// startInDocker, with and without a PTY, ends a `docker run` the engine
// refused the agent's port on (exit 125) as utils.ErrPortTaken, read from
// what that run printed, which keploy's console gets: a start Setup's caller
// makes again at once on other ports. Its refusal in words keploy does not
// know for a taken port fails closed, the line shown.
func TestStartInDockerTellsATakenAgentPortFromDockerRunsRefusal(t *testing.T) {
	const agentPort = 43902
	known := "docker: Error response from daemon: failed to set up container networking: driver failed programming external connectivity on endpoint keploy-v3-0a1b2c3d4e5f6071 (b7eadbc86d7b): failed to bind host port 127.0.0.1:43902/tcp: address already in use"
	unknown := "docker: Error response from daemon: Ports are not available: exposing port TCP 127.0.0.1:43902 -> 0.0.0.0:0: listen tcp 127.0.0.1:43902: bind: An attempt was made to access a socket in a way forbidden by its access permissions."
	ptyFn := needsAgentPTY
	t.Cleanup(func() { needsAgentPTY = ptyFn })
	for _, onPTY := range []bool{false, true} {
		for _, refusal := range []string{known, unknown} {
			needsAgentPTY = func() bool { return onPTY }
			dir := t.TempDir()
			// The engine refuses the agent's `docker container run`, on
			// stderr, as Docker 29 does, and runs every other command.
			fakeEngineForStartInDocker(t, dir, "#!/bin/sh\nif [ \"$1\" = container ] && [ \"$2\" = run ]; then\n  printf '%s\\n\\n%s\\n' '"+refusal+"' \"Run 'docker run --help' for more information\" >&2\n  exit 125\nfi\nexit 0\n")
			a := &AgentClient{logger: zap.NewNop(), conf: &config.Config{InstallationID: "install-id"}}
			opts := models.SetupOptions{KeployContainer: "keploy-v3-0a1b2c3d4e5f6071", Mode: models.MODE_RECORD, AgentPort: agentPort, ProxyPort: 16791, DnsPort: 26791}
			err := a.startInDocker(context.Background(), zap.NewNop(), opts, newAgentRun(), nil)
			var ee interface{ ExitCode() int }
			if !errors.As(err, &ee) || ee.ExitCode() != engineRunFailed || !strings.Contains(err.Error(), strconv.Quote(refusal)) {
				t.Fatalf("on a PTY %v: startInDocker = %v; want docker run's exit %d, its refusal %q shown", onPTY, err, engineRunFailed, refusal)
			}
			if taken := errors.Is(err, utils.ErrPortTaken); taken != (refusal == known) {
				t.Fatalf("on a PTY %v: startInDocker = %v; a taken port %v, want %v", onPTY, err, taken, refusal == known)
			}
		}
	}
}

// A native agent whose start failed on a taken port, as its report says
// (utils.BootProgress.PortTaken), ends waitForAgent as utils.ErrPortTaken
// whichever reaches the wait first: its exit (its last report read as it
// exits, started on a PTY or not) or its report (it lives on after it).
func TestANativeAgentsTakenPortEndsItsWaitAsATakenPort(t *testing.T) {
	for _, c := range []struct {
		name              string
		onPTY, exitsFirst bool
	}{
		{"its exit first", false, true},
		{"its exit first, on a PTY", true, true},
		{"its report first", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			nativeAgentsTakenPortEndsItsWait(t, c.onPTY, c.exitsFirst)
		})
	}
}

func nativeAgentsTakenPortEndsItsWait(t *testing.T, onPTY, exitsFirst bool) {
	cmdFn, ptyCmdFn, cachedFn, ptyFn, look := newNativeAgentCommand, newNativeAgentPTYCommand, sudoCredentialsCached, needsAgentPTY, bootLook
	t.Cleanup(func() {
		newNativeAgentCommand, newNativeAgentPTYCommand, sudoCredentialsCached, needsAgentPTY, bootLook = cmdFn, ptyCmdFn, cachedFn, ptyFn, look
	})
	// Its exit first: no look at its report comes before it. Its report
	// first: the agent lives on, and a look reads it.
	bootLook = time.Millisecond
	if exitsFirst {
		bootLook = time.Hour
	}
	const failure = "the proxy's port 16789 is taken by pid 7 (x): bind: address already in use"
	report, err := json.Marshal(utils.BootProgress{Failed: true, ExitCode: 1, Failure: failure, PortTaken: true})
	if err != nil {
		t.Fatal(err)
	}
	then := "exec sleep 60"
	if exitsFirst {
		then = "exit 1"
	}
	run := newAgentRun()
	var started *exec.Cmd
	// The stand-in agent writes its report where its start's wait reads it
	// (set before it starts), then exits or lives on.
	standIn := func(attr *syscall.SysProcAttr) *exec.Cmd {
		started = exec.Command("sh", "-c", `printf '%s' "$1" > "$2"; `+then, "sh", string(report), run.bootReport)
		started.SysProcAttr = attr
		return started
	}
	newNativeAgentCommand = func(string, []string, bool) *exec.Cmd {
		return standIn(&syscall.SysProcAttr{Setpgid: true})
	}
	newNativeAgentPTYCommand = func(string, []string) *exec.Cmd {
		return standIn(&syscall.SysProcAttr{Setsid: true})
	}
	// Without a PTY, sudo's credentials are cached: no prompt is in front of
	// the agent, and its process runs.
	sudoCredentialsCached = func() bool { return !onPTY }
	needsAgentPTY = func() bool { return onPTY }

	a := clientFor(newStandIn(t, drops).url)
	a.conf.Agent.AgentURI = "" // its stop is the test's
	a.agentCancel = func() {}
	a.run = run
	grp, gctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(context.WithValue(gctx, models.ErrGroupKey, grp))
	exited := make(chan error, 1)
	if err := a.startNativeAgent(ctx, models.SetupOptions{AgentPort: 26795, ProxyPort: 16789, DnsPort: 26797}, exited, run); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-started.Process.Pid, syscall.SIGKILL)
		cancel()
		_ = grp.Wait()
	})
	if run.bootReport == "" || filepath.Base(run.bootReport) != utils.BootProgressFileName {
		t.Fatalf("the wait reads the report at %q", run.bootReport)
	}
	err = waitWithin(t, a, ctx, exited)
	if !errors.Is(err, utils.ErrPortTaken) || !strings.Contains(err.Error(), failure) {
		t.Fatalf("waitForAgent = %v; want utils.ErrPortTaken saying %q", err, failure)
	}
}
