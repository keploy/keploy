package docker

import (
	"context"
	"go/ast"
	"go/token"
	"runtime"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A run that learns noise starts its containerised agent settling reads; any
// other run's alias is byte-identical to before.
func TestSettleReadsSuffixForwardsTheOption(t *testing.T) {
	if got := settleReadsSuffix(models.SetupOptions{}); got != "" {
		t.Fatalf("no SettleReads: got %q, want nothing", got)
	}
	if got := settleReadsSuffix(models.SetupOptions{SettleReads: true}); got != " --settle-reads" {
		t.Fatalf("SettleReads: got %q, want \" --settle-reads\"", got)
	}
}

// The docker run agent's alias carries --settle-reads for a run that learns
// noise, and only the flag differs.
func TestGetAlias_LinuxCarriesSettleReads(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("getAlias switches on runtime.GOOS; the linux arm only executes here")
	}
	opts := models.SetupOptions{KeployContainer: "keploy-test", AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791, ClientNSPID: 4242, Mode: models.MODE_TEST}
	off, err := getAlias(context.Background(), zap.NewNop(), opts, &config.Config{})
	if err != nil {
		t.Fatalf("getAlias: %v", err)
	}
	opts.SettleReads = true
	on, err := getAlias(context.Background(), zap.NewNop(), opts, &config.Config{})
	if err != nil {
		t.Fatalf("getAlias: %v", err)
	}
	if strings.Contains(off, " --settle-reads") || strings.Replace(on, " --settle-reads", "", 1) != off {
		t.Fatalf("want only --settle-reads added for a run that learns noise:\noff: %s\non:  %s", off, on)
	}
}

// Every getAlias platform arm takes settleReadsSuffix into the alias it
// returns (as TestGetAlias_EveryPlatformBranchCarriesTheProvenSwitch).
func TestGetAlias_EveryPlatformBranchCarriesSettleReads(t *testing.T) {
	forEachAliasReturningBranch(t, func(t *testing.T, pos token.Position, list []ast.Stmt) {
		t.Helper()
		for _, stmt := range list {
			if stmtAssignsAliasCalling(stmt, "settleReadsSuffix") {
				return
			}
		}
		t.Errorf("%s: this getAlias platform branch returns an alias that does not take settleReadsSuffix(); there a run that learns noise would read its misses unsettled", pos)
	})
}

// The compose agent service carries it too.
func TestGenerateKeployAgentService_CarriesSettleReads(t *testing.T) {
	for _, settles := range []bool{false, true} {
		svc, err := (&Impl{logger: zap.NewNop(), conf: &config.Config{}}).GenerateKeployAgentService(models.SetupOptions{
			AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791, Mode: models.MODE_TEST, SettleReads: settles,
		})
		if err != nil {
			t.Fatalf("GenerateKeployAgentService: %v", err)
		}
		if got := slices.Contains(composeCommand(t, svc), "--settle-reads"); got != settles {
			t.Fatalf("SettleReads %v, yet the compose agent command carries --settle-reads: %v", settles, got)
		}
	}
}
