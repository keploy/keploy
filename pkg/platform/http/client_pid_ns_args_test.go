package http

import (
	"os"
	"slices"
	"strconv"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// A natively started agent is told the pid namespace its --client-pid is the
// CLI's in, right after the pid, in record and in test mode alike (the
// readiness watch, the proven-scope root and the CA install read it in both).
// Without it the agent takes the pid by its number alone
// (utils.ClientPIDHere), and a pid of another namespace would name one of
// this agent's processes.
func TestNativeAgentArgs_SayTheCLIsPidNamespace(t *testing.T) {
	t.Cleanup(func() { _ = token.Cleanup() })
	own := utils.OwnPIDNamespace()
	if own == "" {
		t.Fatal("this process cannot read its own pid namespace, so no launch could say it")
	}
	// Under the proven switch the launcher asks the host's docker CLI for its
	// context unless DOCKER_HOST is set (containerkey.AgentArgs).
	t.Setenv("DOCKER_HOST", "unix:///run/test-docker.sock")
	a := New(zap.NewNop(), nil, &config.Config{})
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		opts := models.SetupOptions{AgentPort: 26795, ProxyPort: 26796, DnsPort: 26797, AgentURI: "http://localhost:26795/agent", Mode: mode}
		args := a.nativeAgentArgs(opts)
		i := slices.Index(args, "--client-pid")
		if i < 0 || i+1 >= len(args) || args[i+1] != strconv.Itoa(os.Getpid()) {
			t.Fatalf("%s: the native agent argv does not say this CLI's pid: %v", mode, args)
		}
		j := slices.Index(args, "--client-pid-ns")
		if j < 0 || j+1 >= len(args) || args[j+1] != own {
			t.Fatalf("%s: the native agent argv does not say the CLI's pid namespace %q beside its pid, so the agent takes the pid by its number alone: %v", mode, own, args)
		}
	}
}
