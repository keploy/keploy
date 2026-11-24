package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// agentFlags registers the flags the agent's RunE reads straight off the
// command, as the real CmdConfigurator does.
type agentFlags struct{ noFlags }

func (agentFlags) AddFlags(cmd *cobra.Command) error {
	cmd.Flags().Uint32("client-pid", 0, "")
	cmd.Flags().Bool("is-docker", false, "")
	cmd.Flags().String("token-file", "", "")
	cmd.Flags().String("teardown-progress-file", "", "")
	return nil
}

// servingAgent announces port the way a real agent does once it is hooked, and
// then runs until it is stopped. setupCalled records whether it got that far.
type servingAgent struct {
	agentSvc
	port        int
	setupCalled chan struct{}
}

func (s servingAgent) Setup(ctx context.Context, startCh chan int) error {
	close(s.setupCalled)
	select {
	case startCh <- s.port:
	case <-ctx.Done():
		return nil
	}
	<-ctx.Done()
	return context.Canceled
}

// runningAgent is the real `keploy agent` RunE, started in the background.
type runningAgent struct {
	base        string // http://127.0.0.1:<port>/agent
	setupCalled chan struct{}
	done        chan struct{}
}

func startAgent(t *testing.T, logger *zap.Logger, args ...string) *runningAgent {
	t.Helper()
	port := testnet.ReservedPort(t) // no other socket is given it before the agent listens

	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })

	// The agent reports its teardown into this test's own directory -- one
	// only its owner may write to, as a command-line path must be whatever the
	// umask -- the way it would: a native agent where --teardown-progress-file
	// says, a containerised one (--is-docker) where its container's path is,
	// here pointed at that directory rather than the path every process on
	// the host shares. Its reporter is stopped once the agent has returned
	// (cleanups run last first), before the directory is removed.
	dir := filepath.Join(t.TempDir(), "teardown")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	progress := filepath.Join(dir, utils.TeardownProgressFileName)
	t.Cleanup(utils.StopTeardownReporting)
	if slices.Contains(args, "--is-docker") {
		orig := containerTeardownProgressFile
		containerTeardownProgressFile = progress
		t.Cleanup(func() { containerTeardownProgressFile = orig })
		// Its start's progress too, so that no row writes the host's own
		// file (kdocker.AgentBootProgressFile).
		origBoot := containerBootProgressFile
		containerBootProgressFile = filepath.Join(dir, "boot-progress.json")
		t.Cleanup(func() { containerBootProgressFile = origBoot })
	} else {
		args = append([]string{"--teardown-progress-file", progress}, args...)
	}
	ctx, cancel := context.WithCancel(context.Background())
	svc := servingAgent{port: port, setupCalled: make(chan struct{})}
	cmd := Agent(ctx, logger, &config.Config{}, agentSvcFactory{svc: svc}, agentFlags{})
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	a := &runningAgent{base: fmt.Sprintf("http://127.0.0.1:%d/agent", port), setupCalled: svc.setupCalled, done: make(chan struct{})}
	go func() {
		defer close(a.done)
		if err := cmd.RunE(cmd, nil); err != nil {
			t.Errorf("RunE returned %v; the agent reports failure through its exit code", err)
		}
	}()
	t.Cleanup(func() { cancel(); <-a.done })
	return a
}

func (a *runningAgent) get(t *testing.T, path, bearer string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, a.base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return -1
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// waitServing waits until the agent answers its health check.
func (a *runningAgent) waitServing(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if a.get(t, "/health", "") == http.StatusOK {
			return
		}
		select {
		case <-a.done:
			t.Fatalf("the agent exited (code %d) instead of serving", utils.ErrCode)
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("the agent never started serving")
}

func writeTokenFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent-token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAgent_GuardsEveryRouteWithTheTokenItWasHanded runs the real agent command
// end to end. Every route test in pkg/agent/routes wraps Authenticate by hand,
// so deleting the one line that installs it on the real router left all of them
// green while the agent served its whole control plane to anyone.
func TestAgent_GuardsEveryRouteWithTheTokenItWasHanded(t *testing.T) {
	t.Setenv(token.Env, "")
	const tok = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tokenFile := writeTokenFile(t, token.Env+"="+tok+"\n")

	a := startAgent(t, zap.NewNop(), "--token-file", tokenFile)
	a.waitServing(t)

	// Wrong and missing tokens are refused before any route is matched: the
	// probe path has no handler, so a 404 here would mean the guard let the
	// request through.
	if got := a.get(t, token.ProbePath, "not-the-token"); got != http.StatusUnauthorized {
		t.Errorf("a wrong token got %d, want 401", got)
	}
	if got := a.get(t, "/mock/stats", ""); got != http.StatusUnauthorized {
		t.Errorf("no token got %d on a registered route, want 401", got)
	}
	// The token the launcher handed over gets past the guard to the router,
	// which has no such route.
	if got := a.get(t, token.ProbePath, tok); got != http.StatusNotFound {
		t.Errorf("the handed-over token got %d, want the router's 404", got)
	}
	if _, err := os.Stat(tokenFile); !os.IsNotExist(err) {
		t.Error("the agent left the session token on disk after reading it")
	}
}

// TestAgent_RefusesToStartWithATokenFileItCannotRead pins failing closed. It
// used to fall back to the environment and, finding nothing there, serve its
// whole control plane open — the outcome a planted file was after.
func TestAgent_RefusesToStartWithATokenFileItCannotRead(t *testing.T) {
	t.Setenv(token.Env, "")
	core, logs := observer.New(zapcore.ErrorLevel)
	a := startAgent(t, zap.New(core), "--token-file", writeTokenFile(t, "garbage\n"))

	select {
	case <-a.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not exit")
	}
	if utils.ErrCode == 0 {
		t.Error("the agent exited 0; the CLI that launched it would read that as a clean finish")
	}
	select {
	case <-a.setupCalled:
		t.Error("the agent went on to hook the application with a token it could not read")
	default:
	}
	if got := a.get(t, "/health", ""); got != -1 {
		t.Errorf("something is serving the agent port (%d)", got)
	}
	found := false
	for _, e := range logs.All() {
		reason, _ := e.ContextMap()["error"].(string)
		found = found || strings.Contains(reason, "will not start")
	}
	if !found {
		t.Error("the agent exited without saying why")
	}
}

// TestAgent_DaemonSetSaysNothingAboutAServerItNeverStarts: a DaemonSet agent
// never binds the control-plane server, so it needs no token: without the
// gate, every node's agent would refuse to start for want of one. It starts,
// and says nothing about a token.
func TestAgent_DaemonSetSaysNothingAboutAServerItNeverStarts(t *testing.T) {
	t.Setenv(token.Env, "")
	t.Setenv("KEPLOY_DAEMONSET_ENABLED", "true")
	core, logs := observer.New(zapcore.InfoLevel)
	a := startAgent(t, zap.New(core))

	deadline := time.Now().Add(10 * time.Second)
	for logs.FilterMessageSnippet("not starting the control-plane HTTP server").Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the DaemonSet agent never reached its server gate")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, said := range []string{"authentication", "control-plane token", token.InsecureNoAuthEnv} {
		if n := logs.FilterMessageSnippet(said).Len(); n != 0 {
			t.Errorf("the DaemonSet agent reported on the token of a server it never starts: %v",
				logs.FilterMessageSnippet(said).All()[0].Message)
		}
	}
	if got := a.get(t, "/health", ""); got != -1 {
		t.Errorf("the DaemonSet agent is serving its control plane (%d)", got)
	}
}

// refusedToStart waits for a started agent to exit and checks it refused to
// start: a non-zero exit, no hooking of the application, nothing serving its
// port, and an error that says why, containing each of want.
func refusedToStart(t *testing.T, a *runningAgent, logs *observer.ObservedLogs, want ...string) {
	t.Helper()
	select {
	case <-a.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not exit")
	}
	if utils.ErrCode == 0 {
		t.Error("the agent exited 0; the CLI, docker or the kubelet that started it would read that as a clean finish")
	}
	select {
	case <-a.setupCalled:
		t.Error("the agent went on to hook the application without a token to enforce")
	default:
	}
	if got := a.get(t, "/health", ""); got != -1 {
		t.Errorf("something is serving the agent port (%d)", got)
	}
	var reasons []string
	for _, e := range logs.All() {
		if reason, _ := e.ContextMap()["error"].(string); reason != "" {
			reasons = append(reasons, reason)
		}
	}
	for _, w := range want {
		if !slices.ContainsFunc(reasons, func(r string) bool { return strings.Contains(r, w) }) {
			t.Errorf("the agent's refusal does not say %q; it said: %q", w, reasons)
		}
	}
}

// TestAgent_WithoutATokenRefusesToStart: an agent started with no token file
// and no KEPLOY_AGENT_TOKEN, as `keploy agent` run by hand is, exits non-zero
// and says how to start it with one.
func TestAgent_WithoutATokenRefusesToStart(t *testing.T) {
	t.Setenv(token.Env, "")
	t.Setenv(token.InsecureNoAuthEnv, "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	core, logs := observer.New(zapcore.ErrorLevel)
	a := startAgent(t, zap.New(core))
	refusedToStart(t, a, logs, "no control-plane token: start the agent through keploy, or set KEPLOY_AGENT_TOKEN",
		token.InsecureNoAuthEnv+"=true")
}

// TestAgent_InClusterAgentWithoutATokenRefusesToStart starts the agent the way
// Kubernetes runs a pod agent that k8s-proxy handed no token: a container
// (--is-docker) in a pod, with no KEPLOY_AGENT_TOKEN and no
// KEPLOY_DAEMONSET_ENABLED. It refuses to start, naming k8s-proxy, so a replay
// under a k8s-proxy release before the one that gives every agent it starts a
// token fails at its agent's start rather than with a guard nothing passes.
func TestAgent_InClusterAgentWithoutATokenRefusesToStart(t *testing.T) {
	t.Setenv(token.Env, "")
	t.Setenv(token.InsecureNoAuthEnv, "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	core, logs := observer.New(zapcore.ErrorLevel)
	a := startAgent(t, zap.New(core), "--is-docker")
	refusedToStart(t, a, logs, "this in-cluster agent was started without KEPLOY_AGENT_TOKEN", "upgrade k8s-proxy")
}

// TestAgent_InsecureOptOutServesWithoutAToken: with the explicit opt-out, an
// agent with no token serves every route to any caller, and says so once, at
// WARN, naming the variable.
func TestAgent_InsecureOptOutServesWithoutAToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		k8s  string
	}{
		{"native", nil, ""},
		{"in-cluster", []string{"--is-docker"}, "10.96.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(token.Env, "")
			t.Setenv(token.InsecureNoAuthEnv, "true")
			t.Setenv("KUBERNETES_SERVICE_HOST", tc.k8s)
			core, logs := observer.New(zapcore.InfoLevel)
			a := startAgent(t, zap.New(core), tc.args...)
			a.waitServing(t)

			// The probe path has no handler: the router's 404 means the guard
			// let a request with no token through.
			if got := a.get(t, token.ProbePath, ""); got != http.StatusNotFound {
				t.Errorf("no token got %d past the opt-out's guard, want the router's 404", got)
			}
			said := logs.FilterMessageSnippet(token.InsecureNoAuthEnv).All()
			if len(said) != 1 || said[0].Level != zapcore.WarnLevel {
				t.Fatalf("the opt-out was announced %d times (want once, at WARN): %v", len(said), said)
			}
		})
	}
}

// TestAgent_InsecureOptOutOnlyTakesTrue: any other value of the opt-out is not
// read as a yes, and the agent says what it was set to.
func TestAgent_InsecureOptOutOnlyTakesTrue(t *testing.T) {
	for _, v := range []string{"1", "TRUE", "yes", " true"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(token.Env, "")
			t.Setenv(token.InsecureNoAuthEnv, v)
			core, logs := observer.New(zapcore.ErrorLevel)
			a := startAgent(t, zap.New(core))
			refusedToStart(t, a, logs, token.InsecureNoAuthEnv+" is set to")
		})
	}
}

// TestAgent_InsecureOptOutLeavesAHandedTokenEnforced: the opt-out is for an
// agent with no token; one that was handed a token enforces it whatever the
// opt-out says.
func TestAgent_InsecureOptOutLeavesAHandedTokenEnforced(t *testing.T) {
	const tok = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	t.Setenv(token.Env, tok)
	t.Setenv(token.InsecureNoAuthEnv, "true")
	a := startAgent(t, zap.NewNop(), "--is-docker")
	a.waitServing(t)
	if got := a.get(t, token.ProbePath, ""); got != http.StatusUnauthorized {
		t.Errorf("no token got %d, want 401: the opt-out switched off a token the agent was handed", got)
	}
	if got := a.get(t, token.ProbePath, tok); got != http.StatusNotFound {
		t.Errorf("the handed-over token got %d, want the router's 404", got)
	}
}

// TestAgent_InClusterAgentEnforcesTheTokenK8sProxyHandedIt starts the agent the
// way Kubernetes runs a sidecar k8s-proxy injected with a token: a container
// (--is-docker) in a pod, with KEPLOY_AGENT_TOKEN in its environment. It must
// guard every route with that token and say nothing about authentication.
func TestAgent_InClusterAgentEnforcesTheTokenK8sProxyHandedIt(t *testing.T) {
	const tok = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	t.Setenv(token.Env, tok)
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	core, logs := observer.New(zapcore.InfoLevel)
	a := startAgent(t, zap.New(core), "--is-docker")
	a.waitServing(t)

	if got := a.get(t, "/mock/stats", ""); got != http.StatusUnauthorized {
		t.Errorf("no token got %d on a registered route, want 401", got)
	}
	if got := a.get(t, token.ProbePath, "not-the-token"); got != http.StatusUnauthorized {
		t.Errorf("a wrong token got %d, want 401", got)
	}
	if got := a.get(t, token.ProbePath, tok); got != http.StatusNotFound {
		t.Errorf("the token k8s-proxy handed over got %d, want the router's 404", got)
	}
	for _, snippet := range []string{"authentication", "control-plane token", token.InsecureNoAuthEnv} {
		if said := logs.FilterMessageSnippet(snippet).All(); len(said) != 0 {
			t.Errorf("the in-cluster agent enforcing its token said, at %s: %s", said[0].Level, said[0].Message)
		}
	}
}

// TestRoot_RemovesTheAgentTokenDirectoryWhenTheCommandFinishes: the private
// directory token.WriteFile creates must not outlive the command. Removal used
// to be a defer in this repository's main, which the enterprise binary — with
// a main of its own, but commands built by this same Root — never runs.
func TestRoot_RemovesTheAgentTokenDirectoryWhenTheCommandFinishes(t *testing.T) {
	root := Root(context.Background(), zap.NewNop(), agentSvcFactory{}, noFlags{})
	var tokenFile string
	root.AddCommand(&cobra.Command{
		Use: "launch-an-agent",
		RunE: func(*cobra.Command, []string) error {
			var err error
			tokenFile, err = token.WriteFile()
			if err != nil {
				return err
			}
			// However the command ends, error included.
			return fmt.Errorf("the run failed")
		},
	})
	root.SetArgs([]string{"launch-an-agent"})
	root.SilenceErrors, root.SilenceUsage = true, true

	if err := root.Execute(); err == nil {
		t.Fatal("the command's error was lost")
	}
	if tokenFile == "" {
		t.Fatal("the command never wrote a token file")
	}
	if _, err := os.Stat(filepath.Dir(tokenFile)); !os.IsNotExist(err) {
		t.Errorf("the agent token directory %s outlived the command", filepath.Dir(tokenFile))
	}
}
