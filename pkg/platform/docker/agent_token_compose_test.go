package docker

import (
	"context"
	"errors"
	"go/ast"
	gotoken "go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/engine"
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
	}, &config.Config{})
	require.NoError(t, err)

	client := linuxDockerClient(os.Geteuid() == 0)
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

// TestAgentLaunch_EveryRunKindNamesTheRunToken: the compose agent service and
// the linux `docker run` alias name the run's token in a record, a test, and
// keploy mock's record and replay, so every docker and compose agent of a run
// gets the token this process presents (the client fills the value in from
// its own environment: TestPrepareDockerCommand_GivesTheClientTheTokenTheAliasNames,
// and app's TestRun_OnlyTheComposeCommandStartsAnAgentWithTheToken for every
// agent a compose test-set restart starts). An agent handed no token refuses
// to start (routes.Guard), so no run kind may start one without it.
func TestAgentLaunch_EveryRunKindNamesTheRunToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts models.SetupOptions
	}{
		{"record", models.SetupOptions{Mode: models.MODE_RECORD}},
		{"test", models.SetupOptions{Mode: models.MODE_TEST}},
		{"mock record", models.SetupOptions{Mode: models.MODE_RECORD, MockMode: true}},
		{"mock replay", models.SetupOptions{Mode: models.MODE_TEST, MockMode: true}},
	} {
		t.Run("compose "+tc.name, func(t *testing.T) {
			opts := tc.opts
			opts.AgentPort, opts.KeployContainer, opts.AgentURI = 16789, "keploy-agent", "http://localhost:16789/agent"
			serviceNode, err := (&Impl{logger: zap.NewNop(), conf: &config.Config{}}).GenerateKeployAgentService(opts)
			require.NoError(t, err)
			var svc struct {
				Environment []string `yaml:"environment"`
			}
			raw, err := yaml.Marshal(serviceNode)
			require.NoError(t, err)
			require.NoError(t, yaml.Unmarshal(raw, &svc), "the generated node must be valid compose YAML")
			require.Contains(t, svc.Environment, token.Env, "a %s run's compose agent service does not ask compose for the run's token", tc.name)
		})
		t.Run("docker "+tc.name, func(t *testing.T) {
			if runtime.GOOS != "linux" {
				t.Skip("getAlias switches on runtime.GOOS; the linux arm only executes here")
			}
			opts := tc.opts
			opts.AgentPort, opts.KeployContainer, opts.AgentURI = 16790, "keploy-test", "http://localhost:16790/agent"
			alias, err := getAlias(context.Background(), zap.NewNop(), opts, &config.Config{})
			require.NoError(t, err)
			require.Contains(t, strings.Fields(alias), token.Env, "a %s run's docker agent is not given the run's token", tc.name)
		})
	}
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
		// A negative control, not a path keploy builds: a sudo docker
		// that is not told to keep the token drops it, so the rows above
		// prove the --preserve-env is what carries it.
		{name: "control: sudo docker without --preserve-env", wantSudo: true, wantToken: "<unset>"},
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
				dockerClientFor(tc.root, tc.passToken)+" container run --rm -e "+token.Env+" keploy-image")
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

// dockerClientFor is the docker client a `docker run` command starts the agent
// container through: linuxDockerClient's, which always hands the token on, or,
// with passToken unset, a plain one (root's, or `sudo docker`) — no path
// keploy builds, kept as the negative control that shows the stub sudo really
// drops a variable it is not told to keep.
func dockerClientFor(root, passToken bool) string {
	switch {
	case passToken:
		return linuxDockerClient(root)
	case root:
		return engine.Active().CLI
	default:
		return "sudo " + engine.Active().CLI
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
// without a token (which then refuses to start), on a platform where no test
// runs it.
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

// noAgentToken takes the session token away from this package's launchers for
// the rest of the test, as a token that could not be minted does.
func noAgentToken(t *testing.T) {
	t.Helper()
	prev := requireAgentToken
	requireAgentToken = func() (string, error) {
		return "", errors.New("no control-plane token to hand the keploy agent: entropy source unavailable")
	}
	t.Cleanup(func() { requireAgentToken = prev })
}

// TestGenerateKeployAgentService_WithoutATokenGeneratesNone: with no token to
// hand the agent, no compose agent service is generated, and the error says
// why; an agent started without one refuses to start anyway.
func TestGenerateKeployAgentService_WithoutATokenGeneratesNone(t *testing.T) {
	noAgentToken(t)
	serviceNode, err := (&Impl{logger: zap.NewNop(), conf: &config.Config{}}).GenerateKeployAgentService(models.SetupOptions{
		AgentPort:       16789,
		KeployContainer: "keploy-agent",
		AgentURI:        "http://localhost:16789/agent",
	})
	require.Error(t, err)
	require.Nil(t, serviceNode)
	require.Contains(t, err.Error(), "no control-plane token to hand the keploy agent")
}

// TestGetAlias_WithoutATokenStartsNoAgent: the same for the `docker run` arm,
// and the launch is not on record, since nothing was launched.
func TestGetAlias_WithoutATokenStartsNoAgent(t *testing.T) {
	noAgentToken(t)
	const agentURI = "http://localhost:16793/agent"
	alias, err := getAlias(context.Background(), zap.NewNop(), models.SetupOptions{
		KeployContainer: "keploy-test",
		AgentPort:       16793,
		AgentURI:        agentURI,
		Mode:            models.MODE_RECORD,
	}, &config.Config{})
	require.Error(t, err)
	require.Empty(t, alias)
	require.Contains(t, err.Error(), "no control-plane token to hand the keploy agent")
	_, launched := token.Launched(agentURI)
	require.False(t, launched, "an agent container that was never started is on record as launched")
}

// The docker launcher's token check is the real one: every row above replaces
// it, so this pins what production runs.
func TestRequireAgentToken_IsTheSessionsRequire(t *testing.T) {
	require.Equal(t, reflect.ValueOf(token.Require).Pointer(), reflect.ValueOf(requireAgentToken).Pointer(),
		"the docker launchers do not ask token.Require for the token they hand their agents")
}

// TestGetKeployDockerAlias_WithoutATokenSetsNothingUp: with no token to hand
// the agent container, the refusal comes before anything is set up for it.
// The docker client here cannot even be built (an unparsable DOCKER_HOST), so
// a refusal that came after it would say so instead.
func TestGetKeployDockerAlias_WithoutATokenSetsNothingUp(t *testing.T) {
	noAgentToken(t)
	t.Setenv("DOCKER_HOST", "::not-a-docker-host")
	alias, err := GetKeployDockerAlias(context.Background(), zap.NewNop(), &config.Config{}, models.SetupOptions{
		KeployContainer: "keploy-test", AgentPort: 16794, AgentURI: "http://localhost:16794/agent", Mode: models.MODE_RECORD,
	})
	require.Error(t, err)
	require.Empty(t, alias)
	require.Contains(t, err.Error(), "no control-plane token to hand the keploy agent")
	require.NotContains(t, err.Error(), "failed to initialise docker", "the docker client was set up for an agent with no token")
}
