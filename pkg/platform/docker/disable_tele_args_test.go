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

// `keploy record --disable-tele` still resolved and contacted
// telemetry.keploy.io: the agent is a keploy process of its own, which pings
// telemetry at its start from its own config, and no launcher handed it the
// CLI's --disable-tele. Each docker launcher must pass the CLI's value, and
// exactly once, so the agent's config is the CLI's.
func countTeleFlags(words []string) (n int, last string) {
	for _, w := range words {
		if w == "--disable-tele" || strings.HasPrefix(w, "--disable-tele=") {
			n++
			last = w
		}
	}
	return n, last
}

func TestGetAlias_LinuxCarriesTheCLIsDisableTele(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("getAlias switches on runtime.GOOS; the linux arm only executes here")
	}
	opts := models.SetupOptions{KeployContainer: "keploy-test", AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791, ClientNSPID: 4242, Mode: models.MODE_RECORD}
	for _, disabled := range []bool{true, false} {
		alias, err := getAlias(context.Background(), zap.NewNop(), opts, &config.Config{DisableTele: disabled})
		if err != nil {
			t.Fatalf("getAlias: %v", err)
		}
		words, err := shlex.Split(alias)
		if err != nil {
			t.Fatalf("shlex.Split: %v", err)
		}
		want := "--disable-tele=false"
		if disabled {
			want = "--disable-tele=true"
		}
		if n, got := countTeleFlags(words); n != 1 || got != want {
			t.Errorf("CLI DisableTele=%t: the agent alias carries %d --disable-tele (last %q), want one %q:\n%s", disabled, n, got, want, alias)
		}
	}
}

// The alias is built from the CLI's own config, so its --debug reaches the
// agent too: startInDocker used to hand it a config holding only the
// installation id, and the agent container never got either flag.
func TestGetAlias_LinuxCarriesTheCLIsDebug(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("getAlias switches on runtime.GOOS; the linux arm only executes here")
	}
	opts := models.SetupOptions{KeployContainer: "keploy-test", AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791, ClientNSPID: 4242, Mode: models.MODE_RECORD}
	for _, debug := range []bool{true, false} {
		alias, err := getAlias(context.Background(), zap.NewNop(), opts, &config.Config{Debug: debug})
		if err != nil {
			t.Fatalf("getAlias: %v", err)
		}
		words, err := shlex.Split(alias)
		if err != nil {
			t.Fatalf("shlex.Split: %v", err)
		}
		if got := slices.Contains(words, "--debug"); got != debug {
			t.Errorf("CLI Debug=%t, yet the agent alias carries --debug: %t:\n%s", debug, got, alias)
		}
	}
}

// Only the linux arm runs here; every platform arm must carry the flag the
// same way, in what it returns.
func TestGetAlias_EveryPlatformBranchCarriesTheCLIsDisableTele(t *testing.T) {
	forEachAliasReturningBranch(t, func(t *testing.T, pos token.Position, list []ast.Stmt) {
		t.Helper()
		for _, stmt := range list {
			if stmtAssignsAliasCalling(stmt, "teleSuffix") {
				return
			}
		}
		t.Errorf("%s: this getAlias platform branch returns an alias that does not take teleSuffix(); on that platform `keploy record --disable-tele` still pings telemetry from the agent", pos)
	})
}

// The compose agent service is the third launcher.
func TestGenerateKeployAgentService_CarriesTheCLIsDisableTele(t *testing.T) {
	for _, disabled := range []bool{true, false} {
		svc, err := (&Impl{logger: zap.NewNop(), conf: &config.Config{DisableTele: disabled}}).GenerateKeployAgentService(models.SetupOptions{
			AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791, Mode: models.MODE_RECORD,
		})
		if err != nil {
			t.Fatalf("GenerateKeployAgentService: %v", err)
		}
		command := composeCommand(t, svc)
		want := "--disable-tele=false"
		if disabled {
			want = "--disable-tele=true"
		}
		if n, got := countTeleFlags(command); n != 1 || got != want {
			t.Errorf("CLI DisableTele=%t: the compose agent command carries %d --disable-tele (last %q), want one %q: %q", disabled, n, got, want, command)
		}
	}
}
