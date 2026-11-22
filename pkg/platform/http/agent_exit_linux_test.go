package http

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/internal/starvetest"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// agentExitHelperEnv names how an agent stand-in (TestAgentExitHelperProcess)
// stops; agentExitDirEnv, the directory it writes in.
const (
	agentExitHelperEnv = "KEPLOY_TEST_AGENT_EXIT_HELPER"
	agentExitDirEnv    = "KEPLOY_TEST_AGENT_EXIT_DIR"
)

// agentExitWork is how long the exiting agent stand-in spends on its way out,
// once it has detached everything: more than two of the agent's beats
// (utils.TeardownBeat), as the Go runtime's exit of a race-detector build
// spends a second waiting for races at exit (GORACE's atexit_sleep_ms).
const agentExitWork = 5 * utils.TeardownBeat / 2

// TestAgentExitHelperProcess is not a test: it is a keploy agent's stop, with
// the agent's own teardown report (utils.ReportTeardownProgressTo, at
// teardown.json in its directory) and its own end on the escalation signal
// (utils.DieOnEscalation). It writes ready in its directory once that is set
// up, and begins its teardown on SIGTERM:
//
//   - exits takes a step, has detached everything (utils.MarkDetachDone),
//     and spends agentExitWork on its way out before it exits 0.
//   - stuck takes no step, and never ends.
func TestAgentExitHelperProcess(t *testing.T) {
	mode := os.Getenv(agentExitHelperEnv)
	if mode == "" {
		t.Skip("a keploy agent stand-in for the stop tests")
	}
	// It ends with the test's process, however that ends.
	starvetest.EndWithOwner()
	dir := os.Getenv(agentExitDirEnv)
	utils.DeclareTeardownSteps()
	ctx, begin := context.WithCancel(context.Background())
	if err := utils.ReportTeardownProgressTo(ctx, nil, filepath.Join(dir, utils.TeardownProgressFileName)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	utils.DieOnEscalation(ctx)
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	<-terms
	begin()
	switch mode {
	case "exits":
		utils.TeardownStep()
		utils.MarkDetachDone()
		time.Sleep(agentExitWork)
		os.Exit(0)
	case "stuck":
		select {}
	}
	os.Exit(3)
}

// A clean stop forces nothing. An agent that has detached everything and then
// spends longer than a beat on its way out -- the Go runtime's own work there,
// as a race-detector build's second waiting for races at exit -- is waited
// for, and exits 0 with no line saying it was stopped by force. It was killed
// at the first beat of its exit, with a WARN that it "has detached
// everything it attached but has not exited", on every stop of a
// race-detector build. One whose teardown takes no step is still killed, and
// keploy says so at WARN, and why.
func TestAnAgentOnItsWayOutIsNotForced(t *testing.T) {
	// A refused stop request from an agent that answered is one going away,
	// given the grace before its signal; the grace is not what this tests.
	defer func(g time.Duration) { agentStoppingGrace = g }(agentStoppingGrace)
	agentStoppingGrace = time.Millisecond
	defer func(n uint64) { stallChecks = n }(stallChecks)
	stallChecks = 2
	for _, tc := range []struct {
		mode string
		// why: the reason the force line gives; "": no force line.
		why string
	}{
		{mode: "exits"},
		{mode: "stuck", why: "its teardown has made no progress across the agent's last checks"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			utils.ResetInterruptedAgain()
			t.Cleanup(utils.ResetInterruptedAgain)
			// The agent reports only in a directory no one else may write
			// to, as the CLI makes one, whatever the umask.
			dir := filepath.Join(t.TempDir(), "agent")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestAgentExitHelperProcess$")
			cmd.Env = append(os.Environ(), agentExitHelperEnv+"="+tc.mode, agentExitDirEnv+"="+dir, starvetest.Owner())
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			run := newAgentRun()
			waited := make(chan error, 1)
			go func() { err := cmd.Wait(); run.exited(); waited <- err }()
			// Its whole group, while it has not been reaped: a group's id is
			// free for another once its leader is.
			t.Cleanup(func() {
				if !run.isGone() {
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				}
				<-run.gone
			})
			for {
				if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
					break
				}
				if returnsWithin(run.gone, 10*time.Millisecond) {
					t.Fatalf("the agent exited before it was ready: %v: %s", <-waited, stderr.String())
				}
			}
			core, logs := observer.New(zap.InfoLevel)
			a := clientFor(closedAgentURL(t)) // the API cannot be asked
			a.logger = zap.New(core)
			within(t, "stopNativeAgent", func() {
				a.stopNativeAgent(run, cmd.Process.Pid, cmd, filepath.Join(dir, utils.TeardownProgressFileName))
			})
			var err error
			within(t, "the agent's exit", func() { err = <-waited })
			if logs.FilterMessageSnippet("Waiting for the keploy agent to finish shutting down").Len() != 1 {
				t.Fatalf("the stop never read the agent's teardown report: %v", logs.All())
			}
			forced := logs.FilterMessageSnippet("Stopping the keploy agent by force").All()
			if tc.why == "" {
				if len(forced) != 0 || err != nil {
					t.Fatalf("an agent on its way out was forced (%v): exit %v", forced, err)
				}
				return
			}
			if len(forced) != 1 || forced[0].Level != zapcore.WarnLevel || !strings.HasSuffix(forced[0].Message, ": "+tc.why) {
				t.Fatalf("force lines %v, want one WARN saying %q", forced, tc.why)
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || !exit.Sys().(syscall.WaitStatus).Signaled() {
				t.Fatalf("the stuck agent was not killed: exit %v", err)
			}
		})
	}
}
