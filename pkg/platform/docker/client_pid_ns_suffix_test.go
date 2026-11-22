package docker

import (
	"context"
	"go/ast"
	"go/token"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// ownPIDNamespace is this CLI's --client-pid-ns value; every launch must say
// it, so a test that cannot read it cannot hold a launch to it.
func ownPIDNamespace(t *testing.T) string {
	t.Helper()
	own := utils.OwnPIDNamespace()
	if own == "" {
		t.Fatal("this process cannot read its own pid namespace, so no launch could say it")
	}
	return own
}

// The docker agent runs in a pid namespace of its own, so its --client-pid is
// another namespace's number; the alias says which one. Without it the agent
// takes the pid by its number alone (utils.ClientPIDHere), and in a CI step
// container the CLI's small pid names one of the app's processes.
func TestClientPIDNSSuffixSaysThisCLIsPidNamespace(t *testing.T) {
	if got, want := clientPIDNSSuffix(), " --client-pid-ns "+ownPIDNamespace(t); got != want {
		t.Fatalf("clientPIDNSSuffix() = %q, want %q", got, want)
	}
}

// The linux arm's alias says the namespace right beside the pid, in record and
// in test mode alike.
func TestGetAlias_LinuxSaysTheCLIsPidNamespace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("getAlias switches on runtime.GOOS; the linux arm only executes here")
	}
	want := " --client-pid 4242 --client-pid-ns " + ownPIDNamespace(t) + " "
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		opts := models.SetupOptions{KeployContainer: "keploy-test", AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791, ClientNSPID: 4242, Mode: mode}
		alias, err := getAlias(context.Background(), zap.NewNop(), opts, &config.Config{})
		if err != nil {
			t.Fatalf("%s: getAlias: %v", mode, err)
		}
		if !strings.Contains(alias, want) {
			t.Fatalf("%s: the agent alias does not say %q, so the agent takes the CLI's pid by its number alone:\n%s", mode, want, alias)
		}
	}
}

// The windows and darwin arms run only on their platforms, so each is held to
// the suffix by its source: every arm that returns an alias builds it with
// clientPIDNSSuffix().
func TestGetAlias_EveryPlatformBranchSaysTheCLIsPidNamespace(t *testing.T) {
	forEachAliasReturningBranch(t, func(t *testing.T, pos token.Position, list []ast.Stmt) {
		t.Helper()
		for _, stmt := range list {
			if stmtAssignsAliasCalling(stmt, "clientPIDNSSuffix") {
				return
			}
		}
		t.Errorf("%s: this getAlias platform branch returns an alias that does not take clientPIDNSSuffix(); on that platform the agent takes the CLI's pid by its number alone", pos)
	})
}

// compose's keploy-agent service, which the app's services join (pid:
// service:keploy-agent), says the namespace its --client-pid is this CLI's in,
// in record and in test mode alike.
func TestGenerateKeployAgentService_SaysTheCLIsPidNamespace(t *testing.T) {
	own := ownPIDNamespace(t)
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		svc, err := (&Impl{logger: zap.NewNop(), conf: &config.Config{}}).GenerateKeployAgentService(models.SetupOptions{
			AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791, Mode: mode,
		})
		if err != nil {
			t.Fatalf("%s: GenerateKeployAgentService: %v", mode, err)
		}
		command := composeCommand(t, svc)
		pid, ns := slices.Index(command, "--client-pid"), slices.Index(command, "--client-pid-ns")
		if pid < 0 || pid+1 >= len(command) || command[pid+1] != strconv.Itoa(os.Getpid()) {
			t.Fatalf("%s: the compose agent command does not say this CLI's pid: %v", mode, command)
		}
		if ns < 0 || ns+1 >= len(command) || command[ns+1] != own {
			t.Fatalf("%s: the compose agent command does not say the CLI's pid namespace %q beside its pid, so the agent takes the pid by its number alone: %v", mode, own, command)
		}
	}
}
