package docker

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The app's resolver settings become the agent container's, each value one
// word of the alias. The agent container's docker run carries them on every
// platform arm through appHostsStr, which each arm takes
// (TestGetAlias_EveryPlatformBranchCarriesTheAppsHostEntries).
func TestAgentDNSFlags(t *testing.T) {
	got, err := agentDNSFlags(models.AppDNS{
		Servers: []string{"10.0.0.2", "fd00::53"},
		Search:  []string{"corp.example"},
		Options: []string{"ndots:2", "rotate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := " --dns '10.0.0.2' --dns 'fd00::53' --dns-search 'corp.example' --dns-option 'ndots:2' --dns-option 'rotate'"
	if runtime.GOOS == "windows" {
		want = strings.ReplaceAll(want, "'", "")
	}
	if got != want {
		t.Fatalf("flags %q, want %q", got, want)
	}
	if got, err := agentDNSFlags(models.AppDNS{}); err != nil || got != "" {
		t.Fatalf("no settings: %q, %v", got, err)
	}
	// A value is one word, whatever it holds: not shell code.
	got, err = agentDNSFlags(models.AppDNS{Options: []string{"ndots:2; touch /tmp/x"}})
	if runtime.GOOS != "windows" && (err != nil || got != ` --dns-option 'ndots:2; touch /tmp/x'`) {
		t.Fatalf("a value with a ; became %q, %v", got, err)
	}
}

// The linux arm's agent `docker run` carries the app's resolver settings, so
// keploy record -c "docker run --dns ..." starts its app, which until then
// docker refused under the agent's network mode, exit 125.
func TestGetAlias_LinuxCarriesTheAppsResolverSettings(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("getAlias switches on runtime.GOOS; the linux arm only executes here")
	}
	alias, err := getAlias(context.Background(), zap.NewNop(), models.SetupOptions{
		KeployContainer: "keploy-test", AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791, ClientNSPID: 4242,
		Mode: models.MODE_RECORD, AppDNS: models.AppDNS{Servers: []string{"10.0.0.2"}, Search: []string{"corp.example"}, Options: []string{"ndots:2"}},
	}, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(alias, " --dns '10.0.0.2' --dns-search 'corp.example' --dns-option 'ndots:2' ") {
		t.Fatalf("the agent's docker run carries no resolver settings:\n%s", alias)
	}
	if i, j := strings.Index(alias, " --dns '10.0.0.2'"), strings.Index(alias, " --rm "); i < 0 || j < 0 || i > j {
		t.Fatalf("the resolver settings are not docker's flags, before the image:\n%s", alias)
	}
}
