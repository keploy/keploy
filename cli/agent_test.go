package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"net"
	"net/http"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestIsDaemonSetAgent(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want bool
	}{
		{name: "empty (equivalent to unset)", val: "", want: false},
		{name: "true", val: "true", want: true},
		// Only the exact string "true" gates DaemonSet mode, matching k8s-proxy's
		// canonical daemonsetenv package and pkg/agent/hooks/linux/hooks.go — "1"
		// must NOT be accepted, or this gate would diverge from the rest of the
		// system on a KEPLOY_DAEMONSET_ENABLED=1 pod.
		{name: "one is not accepted", val: "1", want: false},
		{name: "false", val: "false", want: false},
		{name: "zero", val: "0", want: false},
		{name: "uppercase TRUE is not accepted", val: "TRUE", want: false},
		{name: "arbitrary", val: "yes", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// t.Setenv restores the previous value after the test; an empty value
			// is treated the same as unset by isDaemonSetAgent.
			t.Setenv("KEPLOY_DAEMONSET_ENABLED", tc.val)
			if got := isDaemonSetAgent(); got != tc.want {
				t.Errorf("isDaemonSetAgent() with KEPLOY_DAEMONSET_ENABLED=%q = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

// agentSvc is an agent service whose Setup fails, or ends, the way it is told.
// Every other method panics: RunE must not reach them.
type agentSvc struct {
	agent.Service
	setup func(ctx context.Context) error
}

func (s agentSvc) Setup(ctx context.Context, _ chan int) error { return s.setup(ctx) }

type agentSvcFactory struct {
	svc interface{}
	err error
}

func (f agentSvcFactory) GetService(context.Context, string) (interface{}, error) {
	return f.svc, f.err
}

type noFlags struct{}

func (noFlags) AddFlags(*cobra.Command) error                       { return nil }
func (noFlags) ValidateFlags(context.Context, *cobra.Command) error { return nil }
func (noFlags) Validate(context.Context, *cobra.Command) error      { return nil }

// runAgent runs the agent command's RunE against factory and returns the exit
// code the process would end with.
func runAgent(t *testing.T, ctx context.Context, factory ServiceFactory) int {
	t.Helper()
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	cmd := Agent(ctx, zap.NewNop(), &config.Config{}, factory, noFlags{})
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE returned %v; the agent reports failure through its exit code", err)
	}
	return utils.ErrCode
}

// An agent that could not start has to exit non-zero, and with the specific
// code its failure carries: that status is all the launching CLI (and docker,
// and the kubelet) ever sees of it. It exited 0, so a native `keploy mock
// record` whose agent had died reported success without running the tests.
func TestAgentThatCannotStartExitsNonZero(t *testing.T) {
	failing := func(err error) ServiceFactory {
		return agentSvcFactory{svc: agentSvc{setup: func(context.Context) error { return err }}}
	}
	for _, tc := range []struct {
		name    string
		factory ServiceFactory
		want    int
	}{
		{"privileges refused", failing(fmt.Errorf("failed to hook into the app: %w: failed to set memlock rlimit: %w", utils.ErrPrivilegeRequired, syscall.EPERM)), utils.ExitPrivilegeRequired},
		{"environment lacks tracefs", failing(fmt.Errorf("failed to hook into the app: %w: neither debugfs nor tracefs are mounted", utils.ErrEnvironmentUnsupported)), utils.ExitEnvironmentUnsupported},
		{"any other failure", failing(errors.New("failed to hook into the app: address already in use")), utils.ExitKeployError},
		{"no service", agentSvcFactory{err: errors.New("no such service")}, utils.ExitKeployError},
		{"not an agent service", agentSvcFactory{svc: struct{}{}}, utils.ExitKeployError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if got := runAgent(t, ctx, tc.factory); got != tc.want {
				t.Fatalf("exit code %d, want %d", got, tc.want)
			}
		})
	}
}

// The ways a HEALTHY agent ends must stay exit 0, or every clean stop reads as
// a crash: Setup returns context.Canceled when a serving agent is stopped, nil
// when it is stopped before it announced its port, and an error that arrives
// after the process was told to stop is teardown.
func TestAgentThatIsStoppedExitsZero(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(ctx context.Context, stop context.CancelFunc) error
	}{
		{"stopped while serving", func(context.Context, context.CancelFunc) error { return context.Canceled }},
		{"stopped before announcing its port", func(context.Context, context.CancelFunc) error { return nil }},
		{"teardown error after the stop", func(_ context.Context, stop context.CancelFunc) error {
			stop()
			return errors.New("error during agent setup: proxy closed")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			factory := agentSvcFactory{svc: agentSvc{setup: func(ctx context.Context) error { return tc.setup(ctx, cancel) }}}
			if got := runAgent(t, ctx, factory); got != 0 {
				t.Fatalf("a stopped agent exits %d", got)
			}
		})
	}
}

// `keploy agent` dies of a hang-up, as it did before NewCtx made one stop
// keploy gracefully, because its owners count on that (utils.DieOnHangup). The
// agent is a copy of this test binary that builds the agent command on
// NewCtx's context, runs its PreRunE and waits for that context to end.
func TestAgentDiesOfAHangUp(t *testing.T) {
	if dir := os.Getenv("KEPLOY_TEST_AGENT_HANGUP"); dir != "" {
		ctx := utils.NewCtx()
		cmd := Agent(ctx, zap.NewNop(), &config.Config{}, agentSvcFactory{}, noFlags{})
		if err := cmd.PreRunE(cmd, nil); err != nil {
			os.Exit(3)
		}
		if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600); err != nil {
			os.Exit(3)
		}
		<-ctx.Done()
		os.Exit(0)
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no SIGHUP")
	}
	// Go starts a child with the default action for a signal it catches, so
	// the agent starts with SIGHUP's default even when `go test` runs under
	// nohup.
	caught := make(chan os.Signal, 1)
	signal.Notify(caught, syscall.SIGHUP)
	t.Cleanup(func() { signal.Stop(caught) })

	dir := t.TempDir()
	agent := exec.Command(os.Args[0], "-test.run=^TestAgentDiesOfAHangUp$")
	agent.Env = append(os.Environ(), "KEPLOY_TEST_AGENT_HANGUP="+dir)
	var out bytes.Buffer
	agent.Stdout, agent.Stderr = &out, &out
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- agent.Wait() }()
	deadline := time.After(30 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		select {
		case err := <-exited:
			t.Fatalf("the agent ended before it was ready: %v\n%s", err, out.String())
		case <-deadline:
			_ = agent.Process.Kill()
			<-exited
			t.Fatalf("the agent was never ready\n%s", out.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := agent.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(30 * time.Second):
		_ = agent.Process.Kill()
		<-exited
		t.Fatalf("the agent was still running 30s after SIGHUP\n%s", out.String())
	}
	if st := agent.ProcessState.Sys().(syscall.WaitStatus); !st.Signaled() || st.Signal() != syscall.SIGHUP {
		t.Fatalf("the agent ended with %v, want it dead of SIGHUP\n%s", agent.ProcessState, out.String())
	}
}

// A DaemonSet agent binds no control-plane server, so it has no API to
// authenticate, and must not warn that one runs without authentication: that
// warning fired on every DaemonSet start, about a server that never listens.
// Any other agent started without a token still warns.
func TestOnlyAnAgentWithAnAPIWarnsItIsUnauthenticated(t *testing.T) {
	t.Setenv(token.Env, "")
	for _, tc := range []struct {
		name      string
		daemonSet string
		wantWarn  bool
	}{
		{"DaemonSet agent", "true", false},
		{"sidecar or local agent", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KEPLOY_DAEMONSET_ENABLED", tc.daemonSet)
			core, logs := observer.New(zapcore.WarnLevel)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			factory := agentSvcFactory{svc: agentSvc{setup: func(context.Context) error { return nil }}}
			cmd := Agent(ctx, zap.New(core), &config.Config{}, factory, noFlags{})
			if err := cmd.RunE(cmd, nil); err != nil {
				t.Fatalf("RunE returned %v", err)
			}
			warned := logs.FilterMessageSnippet("running WITHOUT authentication").Len() > 0
			if warned != tc.wantWarn {
				t.Fatalf("warned the control-plane API is unauthenticated: %v, want %v", warned, tc.wantWarn)
			}
		})
	}
}

// servingAgentSvc hands the agent a free port, as Agent.Setup does, then asks
// the control plane for an arbitrary path without a token and records how it
// answered: an HTTP status, or 0 when nothing ever listened.
type servingAgentSvc struct {
	agent.Service
	status *int
}

func (s servingAgentSvc) Setup(ctx context.Context, startAgentCh chan int) error {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	select {
	case startAgentCh <- port:
	case <-ctx.Done():
		return ctx.Err()
	}
	client := &http.Client{Timeout: time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/agent/__keploy_auth_probe", port)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := client.Get(url); err == nil {
			*s.status = resp.StatusCode
			_ = resp.Body.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	*s.status = 0
	return nil
}

// The property the DaemonSet branch controls: an agent that serves its control
// plane serves it behind the token check, and a DaemonSet agent serves nothing.
// A request without the token must be refused by a serving agent (401), and
// must find no listener at all on a DaemonSet one.
func TestAgentControlPlaneIsGuardedOrAbsent(t *testing.T) {
	for _, tc := range []struct {
		name       string
		daemonSet  string
		wantStatus int
	}{
		{"sidecar or local agent", "", http.StatusUnauthorized},
		{"DaemonSet agent", "true", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(token.Env, "x")
			t.Setenv("KEPLOY_DAEMONSET_ENABLED", tc.daemonSet)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			status := -1
			factory := agentSvcFactory{svc: servingAgentSvc{status: &status}}
			cmd := Agent(ctx, zap.NewNop(), &config.Config{}, factory, noFlags{})
			if err := cmd.RunE(cmd, nil); err != nil {
				t.Fatalf("RunE returned %v", err)
			}
			if status != tc.wantStatus {
				t.Fatalf("an unauthenticated control-plane request got %d, want %d (0 = nothing listening)", status, tc.wantStatus)
			}
		})
	}
}
