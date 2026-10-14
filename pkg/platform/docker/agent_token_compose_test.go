package docker

import (
	"context"
	"go/ast"
	gotoken "go/token"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// TestGenerateKeployAgentService_NamesTheTokenWithoutItsValue pins how the
// compose agent service gets its token. By name: compose fills a bare
// `KEPLOY_AGENT_TOKEN` in from the environment keploy runs it in. Never the
// value, because this compose file is written 0644 into the user's project and
// every local user could read it. And never an env_file: compose opens that
// itself, and a strictly confined docker (the snap) cannot see a host temp
// file from its private /tmp — the agent never started.
func TestGenerateKeployAgentService_NamesTheTokenWithoutItsValue(t *testing.T) {
	serviceNode, err := (&Impl{logger: zap.NewNop(), conf: &config.Config{}}).GenerateKeployAgentService(models.SetupOptions{
		AgentPort:       16789,
		KeployContainer: "keploy-agent",
		AgentURI:        "http://localhost:16789/agent",
	})
	require.NoError(t, err)

	var svc struct {
		EnvFile     []string `yaml:"env_file"`
		Environment []string `yaml:"environment"`
	}
	raw, err := yaml.Marshal(serviceNode)
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(raw, &svc), "the generated node must be valid compose YAML")

	require.Contains(t, svc.Environment, token.Env,
		"the agent service does not ask compose for the token, so the agent starts without one")
	require.Empty(t, svc.EnvFile, "an env_file is read by the docker client, which a confined docker cannot see")
	require.NotContains(t, string(raw), token.Session(),
		"the token is spelled out in the compose file, which is written 0644 into the user's project")
}

// TestGetAlias_LinuxNamesTheToken is the same for the `docker run` arm. The
// docker client it starts runs as root: directly when keploy is root already,
// which it is on every ordinary path here, and through sudo otherwise, told to
// keep the variable that env_reset would drop (see linuxDockerClient).
func TestGetAlias_LinuxNamesTheToken(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("getAlias switches on runtime.GOOS; the linux arm only executes here")
	}
	const agentURI = "http://localhost:16790/agent"

	alias, err := getAlias(context.Background(), zap.NewNop(), models.SetupOptions{
		KeployContainer: "keploy-test",
		AgentPort:       16790,
		AgentURI:        agentURI,
		Mode:            models.MODE_RECORD,
	}, false)
	require.NoError(t, err)

	client := linuxDockerClient(os.Geteuid() == 0, true)
	require.True(t, strings.HasPrefix(alias, client+" container run "),
		"the docker client is not started the way linuxDockerClient says it must be for this user (%q): %s", client, alias)
	require.Contains(t, strings.Fields(alias), token.Env,
		"the agent container is not given the token, so every control-plane call from the CLI is a 401")
	require.NotContains(t, alias, token.Session(),
		"the token is in the docker run command line, where `ps` shows it to every local user")
	require.NotContains(t, alias, "--env-file", "an env file is read by the docker client, which a confined docker cannot see")

	_, launched := token.Launched(agentURI)
	require.True(t, launched, "the client does not know it launched this agent, so it will never check it enforces the token")
}

// TestLinuxDockerClient_TheDockerClientGetsTheToken runs each way the linux
// alias can start docker, through PrepareDockerCommand as keploy does, against
// a `docker` that reports the token it was given and a `sudo` that does what
// sudo's env_reset does: runs the command with nothing but PATH and the
// variables --preserve-env names.
//
// Root never goes through sudo. Whatever `sudo` is on PATH then cannot break
// the handoff: a pass-through stand-in execs --preserve-env as if it were the
// command, and a sudoers rule without SETENV refuses the whole command, so a
// root keploy that still used sudo never started its agent at all.
func TestLinuxDockerClient_TheDockerClientGetsTheToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the alias runs through sh on unix; the windows arm hands it to cmd.exe")
	}
	for _, tc := range []struct {
		name            string
		root, passToken bool
		wantSudo        bool
		wantToken       string
	}{
		{name: "root", root: true, passToken: true, wantToken: token.Session()},
		{name: "not root", passToken: true, wantSudo: true, wantToken: token.Session()},
		{name: "not root, no token", wantSudo: true, wantToken: "<unset>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			writeStub(t, bin, "sudo", `echo "$*" >> '`+bin+`/sudo.log'
keep=
while :; do
  case "$1" in
    --preserve-env=*) keep=${1#--preserve-env=}; shift ;;
    -*) shift ;;
    *) break ;;
  esac
done
if [ -n "$keep" ]; then exec env -i PATH="$PATH" "$keep=$(printenv "$keep")" "$@"; fi
exec env -i PATH="$PATH" "$@"`)
			writeStub(t, bin, "docker", `printf %s "${`+token.Env+`-<unset>}" > '`+bin+`/docker.token'`)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

			cmd, err := PrepareDockerCommand(context.Background(),
				linuxDockerClient(tc.root, tc.passToken)+" container run --rm -e "+token.Env+" keploy-image")
			require.NoError(t, err)
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "the agent container never started: %s", out)

			got, err := os.ReadFile(filepath.Join(bin, "docker.token"))
			require.NoError(t, err, "docker never ran")
			require.Equal(t, tc.wantToken, string(got), "the docker client did not get the token it copies into the agent container")

			sudoLog, err := os.ReadFile(filepath.Join(bin, "sudo.log"))
			if !tc.wantSudo {
				require.ErrorIs(t, err, os.ErrNotExist, "root went through sudo: %s", sudoLog)
				return
			}
			require.NoError(t, err, "a process that is not root started docker without sudo, which it may not reach")
			if tc.passToken {
				require.Contains(t, string(sudoLog), "--preserve-env="+token.Env)
			}
		})
	}
}

// writeStub puts an executable shell script called name in dir.
func writeStub(t *testing.T, dir, name, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755))
}

// TestGetAlias_EveryPlatformBranchCarriesTheToken covers the arms that cannot
// execute here. The token rides `envs`, which every arm must splice into its
// alias; an arm that rebuilt its env flags without it would start its agent
// unauthenticated, on a platform where no test runs it.
func TestGetAlias_EveryPlatformBranchCarriesTheToken(t *testing.T) {
	forEachAliasReturningBranch(t, func(t *testing.T, pos gotoken.Position, list []ast.Stmt) {
		t.Helper()
		for _, stmt := range list {
			if stmtAssignsAliasUsing(stmt, "envs") {
				return
			}
		}
		t.Errorf("%s: this getAlias platform branch builds its alias without envs, which carries the control-plane token", pos)
	})
}

// stmtAssignsAliasUsing reports whether stmt assigns `alias` from an expression
// that mentions the named identifier.
func stmtAssignsAliasUsing(stmt ast.Stmt, name string) bool {
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
			if ident, ok := n.(*ast.Ident); ok && ident.Name == name {
				found = true
			}
			return !found
		})
	}
	return found
}

// TestPrepareDockerCommand_GivesTheClientTheTokenTheAliasNames runs a prepared
// alias for real. The alias only names the variable, so the process running it
// is the one place the value has to be — and only there: it is not exported to
// keploy's own environment, which every other child would inherit.
func TestPrepareDockerCommand_GivesTheClientTheTokenTheAliasNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the alias runs through sh on unix; the windows arm hands it to cmd.exe")
	}
	tok := token.Session()
	t.Setenv(token.Env, "") // restored afterwards; lets the check below see an export
	cmd, err := PrepareDockerCommand(context.Background(), `printf %s "$`+token.Env+`"`)
	require.NoError(t, err)

	out, err := cmd.Output()
	require.NoError(t, err)
	require.Equal(t, tok, string(out), "the docker client would copy an empty token into the agent container")
	require.False(t, slices.ContainsFunc(cmd.Args, func(a string) bool { return strings.Contains(a, tok) }),
		"the token is on the command line")
	require.Empty(t, os.Getenv(token.Env), "the token was exported to keploy's own environment, and so to every process it starts")
}
