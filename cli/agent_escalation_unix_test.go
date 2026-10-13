//go:build unix

package cli

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// The agent's CLI forces a stop with utils.EscalationSignal when it may not
// signal the agent itself (sudo's): the agent command's PreRunE makes it kill
// the agent where it stands (utils.DieOnEscalation).
func TestAgentDiesOfTheEscalationSignal(t *testing.T) {
	_, agent, out, exited := startHangupAgent(t, false)
	if err := agent.Process.Signal(utils.EscalationSignal); err != nil {
		t.Fatal(err)
	}
	if st := awaitAgentEnd(t, agent, exited, out); !st.Signaled() || st.Signal() != syscall.SIGKILL {
		t.Fatalf("the agent ended with %v, want it killed (SIGKILL)\n%s", agent.ProcessState, out.String())
	}
}

// The agent reports its teardown's progress where the CLI said
// (--teardown-progress-file) once its root context is cancelled, if that path
// is one a keploy CLI gives; one that is not, it does not touch.
func TestTheAgentReportsItsTeardownWhereTheCLISaid(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keploy-agent-stop-test")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, utils.TeardownProgressFileName)
	ctx, cancel := context.WithCancel(context.Background())
	reportTeardown(ctx, zap.NewNop(), path, false)
	cancel()
	hung := time.After(time.Minute) // only turns a hang into a failure
	for {
		if b, err := os.ReadFile(path); err == nil {
			if _, err := utils.ParseTeardownProgress(b); err != nil {
				t.Fatalf("the agent's report does not parse: %v (%q)", err, b)
			}
			break
		}
		select {
		case <-hung:
			t.Fatal("the agent never reported its teardown where the CLI said")
		case <-time.After(5 * time.Millisecond):
		}
	}

	open := t.TempDir()
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.WarnLevel)
	ctx2, cancel2 := context.WithCancel(context.Background())
	reportTeardown(ctx2, zap.New(core), filepath.Join(open, utils.TeardownProgressFileName), false)
	cancel2()
	if logs.FilterMessageSnippet("not reporting the agent's teardown progress").Len() != 1 {
		t.Fatal("a path others may write to was not refused")
	}
}

// progressFlag is a CmdConfigurator that gives the agent command its
// --teardown-progress-file, set to path.
type progressFlag struct {
	noFlags
	path string
}

func (f progressFlag) AddFlags(cmd *cobra.Command) error {
	cmd.Flags().String("teardown-progress-file", f.path, "")
	return nil
}

// The agent command reports its teardown where its --teardown-progress-file
// says, once it is stopped.
func TestTheAgentCommandReportsItsTeardown(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keploy-agent-stop-cmd")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, utils.TeardownProgressFileName)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	factory := agentSvcFactory{svc: agentSvc{setup: func(context.Context) error {
		cancel() // stopped while serving
		return context.Canceled
	}}}
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	cmd := Agent(ctx, zap.NewNop(), &config.Config{}, factory, progressFlag{path: path})
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	hung := time.After(time.Minute) // only turns a hang into a failure
	for {
		if b, err := os.ReadFile(path); err == nil {
			if _, err := utils.ParseTeardownProgress(b); err != nil {
				t.Fatalf("the agent's report does not parse: %v (%q)", err, b)
			}
			return
		}
		select {
		case <-hung:
			t.Fatal("the agent command never reported its teardown where --teardown-progress-file said")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
