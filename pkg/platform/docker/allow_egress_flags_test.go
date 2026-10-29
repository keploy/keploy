package docker

import (
	"context"
	"go/ast"
	"go/token"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/keploy/shlex"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The agent container never sees the host's keploy.yml, so test.allowEgress
// reaches it only on its argv. Every getAlias platform branch appends the
// rendered flags before it returns.
func TestGetAlias_EveryPlatformBranchForwardsAllowEgress(t *testing.T) {
	t.Parallel()
	forEachAliasReturningBranch(t, func(t *testing.T, pos token.Position, list []ast.Stmt) {
		t.Helper()
		if !slices.ContainsFunc(list, func(s ast.Stmt) bool { return isAliasAppendOf(s, "egressFlags") }) {
			t.Errorf("%s: this getAlias platform branch returns the alias without appending egressFlags; that platform's agent would never learn test.allowEgress", pos)
		}
	})
}

func TestGetAlias_ForwardsAllowEgress(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("getAlias switches on runtime.GOOS; the linux arm only executes here")
	}
	alias, err := getAlias(context.Background(), zap.NewNop(), models.SetupOptions{
		KeployContainer: "keploy-test", AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791,
		ClientNSPID: 4242, Mode: models.MODE_TEST, AllowEgress: []string{models.AllowEgressDNS},
	}, &config.Config{})
	if err != nil {
		t.Fatalf("getAlias: %v", err)
	}
	words, err := shlex.Split(alias)
	if err != nil {
		t.Fatalf("shlex.Split(alias): %v", err)
	}
	if !slices.Contains(words, "--allow-egress="+models.AllowEgressDNS) {
		t.Fatalf("the alias does not hand the agent --allow-egress=%s: %q", models.AllowEgressDNS, words)
	}
}

// An entry keploy does not know never reaches the alias, which a shell reads.
func TestGetAlias_RefusesAnUnknownAllowEgressEntry(t *testing.T) {
	_, err := getAlias(context.Background(), zap.NewNop(), models.SetupOptions{
		KeployContainer: "keploy-test", Mode: models.MODE_TEST, AllowEgress: []string{"dns $(id)"},
	}, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), "allowEgress") {
		t.Fatalf("getAlias accepted an unknown test.allowEgress entry (err %v)", err)
	}
}

func TestGenerateKeployAgentService_ForwardsAllowEgress(t *testing.T) {
	svc, err := (&Impl{logger: zap.NewNop(), conf: &config.Config{}}).GenerateKeployAgentService(models.SetupOptions{
		AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791, Mode: models.MODE_TEST,
		AllowEgress: []string{models.AllowEgressDNS},
	})
	if err != nil {
		t.Fatalf("GenerateKeployAgentService: %v", err)
	}
	var command []string
	for k := 0; k+1 < len(svc.Content); k += 2 {
		if svc.Content[k].Value == "command" {
			for _, w := range svc.Content[k+1].Content {
				command = append(command, w.Value)
			}
		}
	}
	found := slices.Contains(command, "--allow-egress="+models.AllowEgressDNS)
	if !found {
		t.Fatalf("the compose agent service's command does not carry --allow-egress=%s: %q", models.AllowEgressDNS, command)
	}
}
