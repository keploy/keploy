package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// TestRun_OnlyTheComposeCommandStartsAnAgentWithTheToken drives App.run, as
// keploy runs the app, against a `docker` and an app on PATH that report what
// they were given and a `sudo` that does what sudo's env_reset does: runs the
// command with nothing but PATH and the variables --preserve-env names.
//
// Under docker compose the app command is what starts the agent: the generated
// keploy-agent service names the control-plane token, and compose fills it in
// from its own environment. So that command has to carry the token, through a
// leading sudo as well, and so does the `up` run() retries after a dependency
// failed to start. Every run of it has to count as a new agent for the check
// that the agent enforces the token — a compose `keploy test` restarts the
// agent for each test-set, at the same address. Under every other kind the
// command is the application under test, which is handed nothing.
func TestRun_OnlyTheComposeCommandStartsAnAgentWithTheToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-ins are shell scripts; windows runs the app command through cmd.exe")
	}
	// Not root: a root keploy runs compose without its sudo (see
	// TestRun_AsRootComposeDoesNotGoThroughItsSudo).
	setEffectiveUID(t, 1000)
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls.log")
	// Each stand-in logs one line: its name, the token it was given, its argv.
	record := `printf '%s|%s|%s\n' "$(basename "$0")" "${` + token.Env + `-<unset>}" "$*" >> '` + calls + `'`
	// A dependency that fails to start: compose aborts the first `up` before
	// it starts the app, which `ps` then shows as created and never started.
	flake := filepath.Join(bin, "flake")
	for name, body := range map[string]string{
		"docker": record + `
case "$*" in
  *" up") if [ -e '` + flake + `' ]; then rm '` + flake + `'; exit 1; fi ;;
  *"ps -a --format json") printf '%s\n' '{"Service":"app","State":"created"}' '{"Service":"db","State":"exited","ExitCode":1}' ;;
esac`,
		"app": record,
		"sudo": record + `
keep=
while :; do
  case "$1" in
    --preserve-env=*) keep=${1#--preserve-env=}; shift ;;
    -*) shift ;;
    *) break ;;
  esac
done
if [ -n "$keep" ]; then exec env -i PATH="$PATH" "$keep=$(printenv "$keep")" "$@"; fi
exec env -i PATH="$PATH" "$@"`,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	composeFile := filepath.Join(t.TempDir(), "docker-compose-tmp.yaml")

	for _, tc := range []struct {
		name string
		kind utils.CmdType
		cmd  string
		// starts picks out the call that starts the app (and, under compose,
		// the agent) from everything else the run asks docker for.
		starts string
		sudo   bool
		agent  bool
		// ups is how many times compose is brought up in one run.
		ups int
	}{
		{name: "compose under sudo", kind: utils.DockerCompose, cmd: "sudo docker compose -f " + composeFile + " up", starts: "docker|compose -f " + composeFile + " up", sudo: true, agent: true},
		{name: "compose", kind: utils.DockerCompose, cmd: "docker compose -f " + composeFile + " up", starts: "docker|compose -f " + composeFile + " up", agent: true},
		{name: "compose under sudo, retried after a dependency failed", kind: utils.DockerCompose, cmd: "sudo docker compose -f " + composeFile + " up", starts: "docker|compose -f " + composeFile + " up", sudo: true, agent: true, ups: 2},
		{name: "docker run under sudo", kind: utils.DockerRun, cmd: "sudo docker run --rm --name app-under-test app-image", starts: "docker|run ", sudo: true},
		{name: "native", kind: utils.Native, cmd: "app --serve", starts: "app|--serve"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agentURI := "http://localhost:16791/agent/" + strings.ReplaceAll(tc.name, " ", "-")
			a := &App{
				logger:      zap.NewNop(),
				docker:      &noContainers{},
				kind:        tc.kind,
				cmd:         tc.cmd,
				composeFile: composeFile,
				opts:        models.SetupOptions{AgentURI: agentURI},
			}
			if tc.ups == 0 {
				tc.ups = 1
			}

			var launches []uint64
			for run := 1; run <= 2; run++ {
				require.NoError(t, os.RemoveAll(calls))
				if tc.ups > 1 {
					a.composeService = "app"
					require.NoError(t, os.WriteFile(flake, nil, 0o600))
				}
				appErr := a.run(context.Background())
				require.Equal(t, models.ErrAppStopped, appErr.AppErrorType, "run %d: %v", run, appErr.Err)

				log, err := os.ReadFile(calls)
				require.NoError(t, err, "run %d: nothing ran", run)
				var starts []string
				var sudo string
				for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
					name, rest, _ := strings.Cut(line, "|")
					got, argv, _ := strings.Cut(rest, "|")
					require.NotContains(t, argv, token.Session(), "run %d: the token is on a command line, where `ps` shows it to every local user", run)
					switch {
					case name == "sudo":
						sudo = argv
					case strings.HasPrefix(name+"|"+argv, tc.starts):
						starts = append(starts, got)
					}
				}

				require.Len(t, starts, tc.ups, "run %d: %s", run, log)
				for _, got := range starts {
					if tc.agent {
						require.Equal(t, token.Session(), got,
							"run %d: compose started the agent without the token its compose file names, so the agent serves its control plane to anyone", run)
					} else {
						require.Equal(t, "<unset>", got, "run %d: the application under test was handed the agent's token", run)
					}
				}
				switch {
				case !tc.sudo:
					require.Empty(t, sudo, "run %d: the command was put under sudo", run)
				case tc.agent:
					require.True(t, strings.HasPrefix(sudo, "--preserve-env="+token.Env+" "),
						"run %d: sudo was not told to keep the token: sudo %s", run, sudo)
				default:
					require.NotContains(t, sudo, token.Env, "run %d: a command that starts no agent was rewritten: sudo %s", run, sudo)
				}

				launch, launched := token.Launched(agentURI)
				require.Equal(t, tc.agent, launched,
					"run %d: whether the client checks this agent enforces the token does not follow whether this command started it", run)
				launches = append(launches, launch)
			}
			if tc.agent {
				require.NotEqual(t, launches[0], launches[1],
					"a compose restart was not counted as a new agent, so the agent of every later test-set goes unchecked")
			}
		})
	}
}

// noContainers is a daemon with nothing left to show once the run is over: an
// inspect finds no such container and a listing is empty. The docker.Client
// that inspectRecorder embeds is nil, so any other call panics.
type noContainers struct{ inspectRecorder }

func (*noContainers) ContainerList(context.Context, container.ListOptions) ([]container.Summary, error) {
	return nil, nil
}
