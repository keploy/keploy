package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })

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
// never binds the control-plane server, so the startup warning that its
// control plane is unauthenticated was a false alarm in every node's log.
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
	if n := logs.FilterMessageSnippet("authentication").Len(); n != 0 {
		t.Errorf("the DaemonSet agent reported on the authentication of a server it never starts: %v",
			logs.FilterMessageSnippet("authentication").All()[0].Message)
	}
	if got := a.get(t, "/health", ""); got != -1 {
		t.Errorf("the DaemonSet agent is serving its control plane (%d)", got)
	}
}

// TestAgent_InClusterAgentSaysPlainlyWhyItIsUnauthenticated starts the agent
// the way Kubernetes runs a sidecar: a container (--is-docker) in a pod, handed
// no token, because in-cluster agents do not use token authentication — the
// cluster network is trusted. It still serves, and says so once, plainly — not
// with the local-handoff warning, whose remedies do not exist in a pod.
func TestAgent_InClusterAgentSaysPlainlyWhyItIsUnauthenticated(t *testing.T) {
	t.Setenv(token.Env, "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	core, logs := observer.New(zapcore.InfoLevel)
	a := startAgent(t, zap.New(core), "--is-docker")
	a.waitServing(t)

	said := logs.FilterMessageSnippet("authentication").All()
	if len(said) != 1 {
		t.Fatalf("the in-cluster agent said %d things about its authentication, want exactly one", len(said))
	}
	if said[0].Level != zapcore.InfoLevel || !strings.Contains(said[0].Message, "in-cluster agents do not use token authentication because the cluster network is trusted") {
		t.Errorf("the in-cluster agent said, at %s: %s", said[0].Level, said[0].Message)
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
