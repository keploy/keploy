package docker

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"runtime"
	"strings"
	"testing"

	"github.com/keploy/shlex"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// rootSetting is one of the CLI's root-level settings the agent shares
// (agentargs.Root), and how to set it alone in a config.
type rootSetting struct {
	flag string
	set  func(*config.Config)
}

var rootSettings = []rootSetting{
	{"--debug", func(c *config.Config) { c.Debug = true }},
	{"--disable-tele", func(c *config.Config) { c.DisableTele = true }},
	{"--disable-ansi", func(c *config.Config) { c.DisableANSI = true }},
	{"--disable-mapping", func(c *config.Config) { c.DisableMapping = true }},
}

// rootSettingCases is a config with none of the settings set, then one with
// each alone.
func rootSettingCases() []rootSetting {
	return append([]rootSetting{{"", func(*config.Config) {}}}, rootSettings...)
}

// checkRootSettings fails unless words carry each root setting exactly once,
// as --flag=<value>, true for the one setting `on` names ("" for none) and
// false for the rest.
func checkRootSettings(t *testing.T, launcher, on string, words []string) {
	t.Helper()
	for _, s := range rootSettings {
		var got []string
		for _, w := range words {
			if w == s.flag || strings.HasPrefix(w, s.flag+"=") {
				got = append(got, w)
			}
		}
		want := fmt.Sprintf("%s=%t", s.flag, s.flag == on)
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s, CLI with only %q set: the agent gets %q, want one %q:\n%q", launcher, on, got, want, words)
		}
	}
}

// The agent is a keploy process of its own that resolves the root settings
// again from a config with no keploy.yml in the container. So `keploy record
// --disable-tele` still resolved and contacted telemetry.keploy.io from the
// agent, and a docker-run agent never got --debug or --disable-mapping. The
// alias hands it the CLI's value of each, true or false, exactly once.
func TestGetAlias_LinuxCarriesTheCLIsRootSettings(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("getAlias switches on runtime.GOOS; the linux arm only executes here")
	}
	opts := models.SetupOptions{KeployContainer: "keploy-test", AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791, ClientNSPID: 4242, Mode: models.MODE_RECORD}
	for _, s := range rootSettingCases() {
		conf := &config.Config{}
		s.set(conf)
		alias, err := getAlias(context.Background(), zap.NewNop(), opts, conf)
		if err != nil {
			t.Fatalf("getAlias: %v", err)
		}
		words, err := shlex.Split(alias)
		if err != nil {
			t.Fatalf("shlex.Split: %v", err)
		}
		checkRootSettings(t, "docker run alias", s.flag, words)
	}
}

// Only the linux arm runs here; every platform arm must carry the settings the
// same way, in what it returns.
func TestGetAlias_EveryPlatformBranchCarriesTheCLIsRootSettings(t *testing.T) {
	forEachAliasReturningBranch(t, func(t *testing.T, pos token.Position, list []ast.Stmt) {
		t.Helper()
		for _, stmt := range list {
			if stmtAssignsAliasCalling(stmt, "rootSuffix") {
				return
			}
		}
		t.Errorf("%s: this getAlias platform branch returns an alias that does not take rootSuffix(); on that platform the agent holds its own defaults, and `keploy record --disable-tele` still pings telemetry from it", pos)
	})
}

// The compose agent service is the third launcher.
func TestGenerateKeployAgentService_CarriesTheCLIsRootSettings(t *testing.T) {
	for _, s := range rootSettingCases() {
		conf := &config.Config{}
		s.set(conf)
		svc, err := (&Impl{logger: zap.NewNop(), conf: conf}).GenerateKeployAgentService(models.SetupOptions{
			AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791, Mode: models.MODE_RECORD,
		})
		if err != nil {
			t.Fatalf("GenerateKeployAgentService: %v", err)
		}
		checkRootSettings(t, "compose agent command", s.flag, composeCommand(t, svc))
	}
}
