//go:build unix || windows

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// watchStandInEnv, set, makes TestAnAgentEndsWhenTheClientThatStartedItIsKilled
// a stand-in for the keploy client: a process that runs until it is killed.
// watchAgentEnv, set to a directory, makes it the agent that client started:
// a copy of this test binary that runs the agent command's RunE on NewCtx's
// context with --client-pid set to watchAgentClientEnv, marks "ready" in the
// directory once its setup runs, and "ended" once RunE returned.
const (
	watchStandInEnv     = "KEPLOY_TEST_WATCH_STAND_IN"
	watchAgentEnv       = "KEPLOY_TEST_WATCH_AGENT"
	watchAgentClientEnv = "KEPLOY_TEST_WATCH_AGENT_CLIENT"
)

// watchFlags gives the agent command the flags RunE reads to arm the
// watchdog, as `keploy agent` has them.
type watchFlags struct{ noFlags }

func (watchFlags) AddFlags(cmd *cobra.Command) error {
	cmd.Flags().Uint32("client-pid", 0, "")
	cmd.Flags().Bool("is-docker", false, "")
	return nil
}

func runWatchHelper() {
	if os.Getenv(watchStandInEnv) != "" {
		time.Sleep(10 * time.Minute)
		os.Exit(0)
	}
	dir := os.Getenv(watchAgentEnv)
	if dir == "" {
		return
	}
	ctx := utils.NewCtx()
	cmd := Agent(ctx, zap.NewNop(), &config.Config{}, agentSvcFactory{svc: agentSvc{setup: func(ctx context.Context) error {
		if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600); err != nil {
			os.Exit(3)
		}
		<-ctx.Done()
		return ctx.Err()
	}}}, watchFlags{})
	if err := cmd.Flags().Set("client-pid", os.Getenv(watchAgentClientEnv)); err != nil {
		os.Exit(3)
	}
	if err := cmd.RunE(cmd, nil); err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(filepath.Join(dir, "ended"), nil, 0o600); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

// startWatchHelper starts a copy of this test binary as the helper env names,
// and kills it at the test's end if it still runs.
func startWatchHelper(t *testing.T, env ...string) (*exec.Cmd, <-chan error) {
	t.Helper()
	// Not os.Args[0]: a test binary run from its own directory has a
	// relative name there, which exec refuses to run on Windows.
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("finding this test binary: %v", err)
	}
	cmd := exec.Command(self, "-test.run=^TestAnAgentEndsWhenTheClientThatStartedItIsKilled$")
	cmd.Env = append(os.Environ(), env...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting a helper: %v", err)
	}
	exited := make(chan error, 1)
	reaped := make(chan struct{}) // closed once Wait returns, whoever reads exited
	go func() {
		exited <- cmd.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-reaped:
		case <-time.After(10 * time.Second):
		}
	})
	return cmd, exited
}

// An agent ends once the keploy client that started it is killed (a crash,
// Task Manager's End task, `taskkill /F`, `kill -9`), and not before: it does
// not keep keploy's ports, or, on Windows, the run's job object and with it
// the application, for a client that is gone. Its root context is cancelled,
// so its ordinary teardown runs. On Windows the agent's watchdog did nothing,
// and the agent ran on.
func TestAnAgentEndsWhenTheClientThatStartedItIsKilled(t *testing.T) {
	runWatchHelper()

	client, clientExited := startWatchHelper(t, watchStandInEnv+"=1")
	dir := t.TempDir()
	agent, agentExited := startWatchHelper(t, watchAgentEnv+"="+dir, watchAgentClientEnv+"="+strconv.Itoa(client.Process.Pid))

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		select {
		case err := <-agentExited:
			t.Fatalf("the agent ended before its setup ran: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent's setup did not run within 30s")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Over two of the watchdog's looks at a client that runs.
	select {
	case err := <-agentExited:
		t.Fatalf("the agent ended while the client that started it still ran: %v", err)
	case <-time.After(3 * watchParentInterval):
	}

	if err := client.Process.Kill(); err != nil {
		t.Fatalf("killing the client: %v", err)
	}
	select {
	case <-clientExited: // reaped: its pid names no process now
	case <-time.After(10 * time.Second):
		t.Fatal("the killed client did not exit within 10s")
	}

	select {
	case err := <-agentExited:
		if err != nil {
			t.Fatalf("the agent whose client was killed exited with %v; want 0", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("the agent (pid %d) still runs 15s after the keploy client that started it was killed", agent.Process.Pid)
	}
	if _, err := os.Stat(filepath.Join(dir, "ended")); errors.Is(err, os.ErrNotExist) {
		t.Fatal("the agent exited without its RunE returning: its teardown did not run")
	}
}
