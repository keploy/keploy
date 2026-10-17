//go:build linux

package http

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// standInSudo starts a stand-in for a sudo that has run its command: a shell
// whose child is a sleep, in a process group of its own, killed whole when the
// test ends.
func standInSudo(t *testing.T) *exec.Cmd {
	t.Helper()
	sudo := exec.Command("sh", "-c", "sleep 60; true")
	sudo.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sudo.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-sudo.Process.Pid, syscall.SIGKILL); _ = sudo.Wait() })
	return sudo
}

// Once the sudo that may prompt has run the agent (its prompt answered), the
// agent's process runs, and its first report is due within bootSilence of
// keploy's runnable time: an agent that hangs before it was waited on for ever.
// A command sudo runs that is not the agent (a helper it runs while the user
// is still at its prompt) is no agent run.
func TestWaitForAgentHoldsAnAgentSudoHasRunToItsFirstReport(t *testing.T) {
	look, silence := bootLook, bootSilence
	bootLook, bootSilence = time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { bootLook, bootSilence = look, silence })
	sudo := standInSudo(t)

	r := newAgentRun()
	r.watchBoot(filepath.Join(t.TempDir(), "teardown.json"), true, "/usr/local/bin/keploy")
	r.sudoPid.Store(int64(sudo.Process.Pid))
	for i := 0; i < 50; i++ {
		if r.sudoRanAgent(context.Background()) {
			t.Fatal("a command sudo runs that is not the agent was taken for it")
		}
		time.Sleep(time.Millisecond)
	}

	a := clientFor(newStandIn(t, drops).url)
	a.agentCancel = func() {}
	a.run = newAgentRun()
	a.run.watchBoot(filepath.Join(t.TempDir(), "teardown.json"), true, "/bin/sleep") // the agent, here
	a.run.sudoPid.Store(int64(sudo.Process.Pid))
	err := waitWithin(t, a, context.Background(), nil)
	if !errors.Is(err, pkg.ErrAgentNotReady) || !strings.Contains(err.Error(), "without reporting its start") {
		t.Fatalf("waitForAgent = %v; want a stalled start, its first report not made", err)
	}
}

// startNativeAgent sets, before the agent starts, what its wait watches: the
// report next to the teardown progress file its command line names, whether
// the sudo that starts it may prompt, and, once started, that sudo's process:
// started on a PTY (sudo's prompt on a terminal), or not.
func TestStartNativeAgentSetsWhatItsStartsWaitWatches(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root, keploy starts the agent without sudo, never on a PTY")
	}
	for _, onPTY := range []bool{false, true} {
		t.Run(map[bool]string{false: "without a PTY", true: "on a PTY"}[onPTY], func(t *testing.T) {
			startNativeAgentWatched(t, onPTY)
		})
	}
}

func startNativeAgentWatched(t *testing.T, onPTY bool) {
	cmdFn, ptyCmdFn, cachedFn, ptyFn := newNativeAgentCommand, newNativeAgentPTYCommand, sudoCredentialsCached, needsAgentPTY
	t.Cleanup(func() {
		newNativeAgentCommand, newNativeAgentPTYCommand, sudoCredentialsCached, needsAgentPTY = cmdFn, ptyCmdFn, cachedFn, ptyFn
	})
	var started *exec.Cmd
	var argv []string
	standIn := func(args []string, attr *syscall.SysProcAttr) *exec.Cmd {
		argv = args
		started = exec.Command("sh", "-c", "sleep 60; true")
		started.SysProcAttr = attr
		return started
	}
	newNativeAgentCommand = func(_ string, args []string, _ bool) *exec.Cmd {
		if onPTY {
			t.Error("the agent was started without a PTY")
		}
		return standIn(args, &syscall.SysProcAttr{Setpgid: true})
	}
	newNativeAgentPTYCommand = func(_ string, args []string) *exec.Cmd {
		if !onPTY {
			t.Error("the agent was started on a PTY")
		}
		return standIn(args, &syscall.SysProcAttr{Setsid: true})
	}
	sudoCredentialsCached = func() bool { return false }
	needsAgentPTY = func() bool { return onPTY }

	a := clientFor(newStandIn(t, drops).url)
	a.conf.Agent.AgentURI = "" // its stop is the test's
	grp, gctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(context.WithValue(gctx, models.ErrGroupKey, grp))
	run := newAgentRun()
	exited := make(chan error, 1)
	if err := a.startNativeAgent(ctx, models.SetupOptions{AgentPort: 26795, ProxyPort: 26796, DnsPort: 26797}, exited, run); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-started.Process.Pid, syscall.SIGKILL)
		cancel()
		_ = grp.Wait()
	})
	i := slices.Index(argv, "--teardown-progress-file")
	if i < 0 || i+1 >= len(argv) {
		t.Fatalf("no teardown progress file in %v", argv)
	}
	if want := filepath.Join(filepath.Dir(argv[i+1]), utils.BootProgressFileName); run.bootReport != want {
		t.Fatalf("the wait reads %q; want %q, next to the teardown progress file", run.bootReport, want)
	}
	if !run.bootPrompt {
		t.Fatal("the wait does not wait for the prompt of a sudo with no cached credentials")
	}
	if got := run.sudoPid.Load(); got != int64(started.Process.Pid) {
		t.Fatalf("the wait watches process %d; want the one started, %d", got, started.Process.Pid)
	}
}

// A container runs once its status is running: not while it is created, nor
// paused (its processes frozen: docker says Running of a paused container
// too), restarting or exited.
func TestAPausedContainerIsNotRunning(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	script := "#!/bin/sh\n[ \"$1\" = inspect ] && cat \"" + state + "\" && exit 0\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	running := containerRunning(newEngineCLI(zap.NewNop()), "keploy-v3")
	for _, tc := range []struct {
		status string
		want   bool
	}{{"created", false}, {"running", true}, {"paused", false}, {"restarting", false}, {"exited", false}} {
		if err := os.WriteFile(state, []byte(tc.status+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := running(context.Background()); got != tc.want {
			t.Errorf("a container whose status is %s runs: %v, want %v", tc.status, got, tc.want)
		}
	}
}
