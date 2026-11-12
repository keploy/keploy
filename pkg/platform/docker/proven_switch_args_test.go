package docker

import (
	"context"
	"go/ast"
	"go/token"
	"runtime"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// The agent container is not given KEPLOY_PROVEN, so the switch must reach it
// as an argv flag; with the switch off the alias must not change at all.
func TestGetAlias_LinuxCarriesTheProvenSwitch(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("getAlias switches on runtime.GOOS; the linux arm only executes here")
	}
	opts := models.SetupOptions{KeployContainer: "keploy-test", AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791, ClientNSPID: 4242, Mode: models.MODE_TEST}
	flag := " --" + provenswitch.AgentFlag

	t.Setenv(provenswitch.Env, "")
	off, err := getAlias(context.Background(), zap.NewNop(), opts, &config.Config{})
	if err != nil {
		t.Fatalf("getAlias: %v", err)
	}
	if strings.Contains(off, flag) {
		t.Fatalf("switch off, yet the agent alias carries %q:\n%s", flag, off)
	}

	t.Setenv(provenswitch.Env, "1")
	on, err := getAlias(context.Background(), zap.NewNop(), opts, &config.Config{})
	if err != nil {
		t.Fatalf("getAlias: %v", err)
	}
	if !strings.Contains(on, flag) {
		t.Fatalf("KEPLOY_PROVEN=1, yet the agent alias lacks %q:\n%s", flag, on)
	}
	// Only the flag differs: the rest of the alias is what it was.
	if strings.Replace(on, flag, "", 1) != off {
		t.Fatalf("the switch changed more of the alias than the flag:\noff: %s\non:  %s", off, on)
	}
}

// getAlias rebuilds the alias in each of its five platform arms, and only the
// linux one runs here; every arm must carry the switch the same way. The call
// must be part of an assignment to `alias` in the arm's own statements (not in
// a nested arm, not a stray `_ = provenSuffix()`), so that it is in what the
// arm returns.
func TestGetAlias_EveryPlatformBranchCarriesTheProvenSwitch(t *testing.T) {
	forEachAliasReturningBranch(t, func(t *testing.T, pos token.Position, list []ast.Stmt) {
		t.Helper()
		for _, stmt := range list {
			if stmtAssignsAliasCalling(stmt, "provenSuffix") {
				return
			}
		}
		t.Errorf("%s: this getAlias platform branch returns an alias that does not take provenSuffix(); on that platform an agent started by a CLI with KEPLOY_PROVEN=1 would run the old paths", pos)
	})
}

// stmtAssignsAliasCalling reports whether stmt assigns to `alias` (= or +=)
// from an expression that calls the named function.
func stmtAssignsAliasCalling(stmt ast.Stmt, name string) bool {
	assign, ok := stmt.(*ast.AssignStmt)
	if !ok || len(assign.Lhs) != 1 {
		return false
	}
	if ident, ok := assign.Lhs[0].(*ast.Ident); !ok || ident.Name != "alias" {
		return false
	}
	found := false
	for _, rhs := range assign.Rhs {
		ast.Inspect(rhs, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == name {
					found = true
				}
			}
			return true
		})
	}
	return found
}

// The compose agent service is the third launcher.
func TestGenerateKeployAgentService_CarriesTheProvenSwitch(t *testing.T) {
	flag := "--" + provenswitch.AgentFlag
	command := func(t *testing.T) []string {
		t.Helper()
		svc, err := (&Impl{logger: zap.NewNop(), conf: &config.Config{}}).GenerateKeployAgentService(models.SetupOptions{
			AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791, Mode: models.MODE_TEST,
		})
		if err != nil {
			t.Fatalf("GenerateKeployAgentService: %v", err)
		}
		return composeCommand(t, svc)
	}

	t.Setenv(provenswitch.Env, "")
	for _, arg := range command(t) {
		if arg == flag {
			t.Fatalf("switch off, yet the compose agent command carries %s", flag)
		}
	}

	t.Setenv(provenswitch.Env, "1")
	for _, arg := range command(t) {
		if arg == flag {
			return
		}
	}
	t.Fatalf("KEPLOY_PROVEN=1, yet the compose agent command lacks %s", flag)
}

// composeCommand reads the `command:` sequence of a generated service node.
func composeCommand(t *testing.T, svc *yaml.Node) []string {
	t.Helper()
	for i := 0; i+1 < len(svc.Content); i += 2 {
		if svc.Content[i].Value != "command" {
			continue
		}
		var out []string
		for _, n := range svc.Content[i+1].Content {
			out = append(out, n.Value)
		}
		return out
	}
	t.Fatal("the generated agent service has no command")
	return nil
}
