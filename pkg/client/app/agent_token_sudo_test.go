package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// stubCall is one line a PATH stand-in logged: its name, the agent token its
// environment held, and its argv.
type stubCall struct{ name, token, argv string }

// installStubs puts each stand-in, a /bin/sh body, in a new directory at the
// front of PATH. Each one logs a stubCall before its body runs. The function
// it returns reads back, and forgets, the calls made so far.
func installStubs(t *testing.T, stubs map[string]string) func() []stubCall {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-ins are shell scripts; windows runs the app command through cmd.exe")
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "calls.log")
	record := `printf '%s|%s|%s\n' "$(basename "$0")" "${` + token.Env + `-<unset>}" "$*" >> '` + log + `'`
	for name, body := range stubs {
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+record+"\n"+body+"\n"), 0o755))
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []stubCall {
		data, err := os.ReadFile(log)
		if os.IsNotExist(err) {
			return nil
		}
		require.NoError(t, err)
		require.NoError(t, os.Remove(log))
		var calls []stubCall
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			name, rest, _ := strings.Cut(line, "|")
			tok, argv, _ := strings.Cut(rest, "|")
			calls = append(calls, stubCall{name: name, token: tok, argv: argv})
		}
		return calls
	}
}

func setEffectiveUID(t *testing.T, uid int) {
	t.Helper()
	prev := effectiveUID
	effectiveUID = func() int { return uid }
	t.Cleanup(func() { effectiveUID = prev })
}

// passThroughSudo is the `sudo` of a CI image that already runs everything as
// root: it runs its command as it is, and knows no option but -E.
const passThroughSudo = `while [ "$1" = -E ]; do shift; done
case "$1" in -*) echo "sudo: unknown option $1" >&2; exit 1 ;; esac
exec "$@"`

// sudoRs stands in for sudo-rs at version, as a real sudo-rs 0.2.5 (Debian
// trixie's) was seen to behave: `--version` prints "sudo-rs <version>" on
// stderr, and the command runs with an environment of sudo's making, which
// drops the agent token. From 0.2.7 on, --preserve-env=NAME keeps NAME; before
// that it is parsed, warned about and ignored. -E is ignored in every version.
func sudoRs(version string, preserves bool) string {
	keep := `echo 'warning: --preserve-env has not yet been implemented and will be ignored' >&2`
	if preserves {
		keep = `keep="$keep ${1#--preserve-env=}=$(printenv "${1#--preserve-env=}")"`
	}
	return `if [ "$1" = --version ]; then echo "sudo-rs ` + version + `" >&2; exit 0; fi
keep=
while :; do
  case "$1" in
    --preserve-env=*) ` + keep + `; shift ;;
    -E|--preserve-env) echo "warning: preserving the entire environment is not supported, '$1' is ignored" >&2; shift ;;
    -*) shift ;;
    *) break ;;
  esac
done
exec env -i PATH="$PATH" $keep "$@"`
}

// doasStub is doas without keepenv: the command runs with an environment of
// doas's making.
const doasStub = `case "$1" in -*) echo "doas: unknown option $1" >&2; exit 1 ;; esac
exec env -i PATH="$PATH" "$@"`

// TestRun_AsRootComposeDoesNotGoThroughItsSudo: every Linux compose run keploy
// starts from a command line runs as root, because main re-executes keploy
// under sudo before it runs a docker command. A sudo or doas in front of
// `docker compose` then elevates nothing, and all it can do is reset the
// environment compose fills the agent's token in from. Telling sudo to keep the
// token broke the form keploy's docs use, `sudo docker compose up`, twice over:
// a pass-through sudo ran --preserve-env=… as the command, so compose never
// started, and sudo-rs before 0.2.7 ignores the flag, so the agent started
// without a token. doas has no such flag at all.
func TestRun_AsRootComposeDoesNotGoThroughItsSudo(t *testing.T) {
	setEffectiveUID(t, 0)
	composeFile := filepath.Join(t.TempDir(), "docker-compose-tmp.yaml")
	up := "compose -f " + composeFile + " up"

	for _, tc := range []struct{ name, prefix, sudo string }{
		{name: "sudo, a pass-through stand-in", prefix: "sudo", sudo: passThroughSudo},
		{name: "sudo -E, a pass-through stand-in", prefix: "sudo -E", sudo: passThroughSudo},
		{name: "sudo, sudo-rs 0.2.5", prefix: "sudo", sudo: sudoRs("0.2.5", false)},
		{name: "sudo -E, sudo-rs 0.2.5", prefix: "sudo -E", sudo: sudoRs("0.2.5", false)},
		{name: "sudo --preserve-env, sudo-rs 0.2.5", prefix: "sudo --preserve-env", sudo: sudoRs("0.2.5", false)},
		{name: "sudo -n --, a pass-through stand-in", prefix: "sudo -n --", sudo: passThroughSudo},
		{name: "sudo -En, sudo-rs 0.2.5", prefix: "sudo -En", sudo: sudoRs("0.2.5", false)},
		{name: "sudo, sudo-rs 0.2.13", prefix: "sudo", sudo: sudoRs("0.2.13", true)},
		{name: "doas", prefix: "doas", sudo: passThroughSudo},
		{name: "doas -n", prefix: "doas -n", sudo: passThroughSudo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := installStubs(t, map[string]string{"docker": "", "sudo": tc.sudo, "doas": doasStub})
			a := &App{
				logger:      zap.NewNop(),
				docker:      &noContainers{},
				kind:        utils.DockerCompose,
				cmd:         tc.prefix + " docker " + up,
				composeFile: composeFile,
				opts:        models.SetupOptions{AgentURI: "http://localhost:16791/agent/as-root"},
			}
			appErr := a.run(context.Background())
			require.Equal(t, models.ErrAppStopped, appErr.AppErrorType, "%v", appErr.Err)

			var ups, wrapped []stubCall
			for _, c := range calls() {
				switch {
				case c.name == "sudo" || c.name == "doas":
					wrapped = append(wrapped, c)
				case c.name == "docker" && c.argv == up:
					ups = append(ups, c)
				}
			}
			require.Len(t, ups, 1, "compose was never brought up; %s was run as %v", tc.prefix, wrapped)
			require.Equal(t, token.Session(), ups[0].token,
				"compose started the agent without the token its compose file names, so the agent serves its control plane to anyone")
			require.Empty(t, wrapped, "keploy is root, and still ran compose through %s", tc.prefix)
		})
	}
}

// TestSetup_RefusesASudoThatWouldDropTheAgentToken: when keploy is not root,
// the sudo in front of compose is needed, and it is told
// --preserve-env=KEPLOY_AGENT_TOKEN. sudo-rs before 0.2.7 does not implement
// that: it warns, runs compose anyway, and compose starts an agent with no
// token, which serves its control plane to anyone who can reach it. keploy has
// to refuse before anything starts, and say why.
func TestSetup_RefusesASudoThatWouldDropTheAgentToken(t *testing.T) {
	setEffectiveUID(t, 1000)
	for _, version := range []string{"0.2.5", "0.2.6"} {
		t.Run("sudo-rs "+version, func(t *testing.T) {
			calls := installStubs(t, map[string]string{"docker": "", "sudo": sudoRs(version, false)})
			a := &App{
				logger: zap.NewNop(),
				docker: &noContainers{},
				kind:   utils.DockerCompose,
				cmd:    "sudo docker compose up",
				opts:   models.SetupOptions{AgentURI: "http://localhost:16791/agent/old-sudo-rs"},
			}
			err := a.Setup(context.Background())
			require.ErrorContains(t, err, "sudo-rs "+version, "keploy did not refuse a sudo that starts the agent without its token")
			require.ErrorContains(t, err, "0.2.7", "the refusal does not say which sudo-rs works")
			for _, c := range calls() {
				require.NotEqual(t, "docker", c.name, "docker ran (%s) after keploy refused", c.argv)
				require.NotContains(t, c.argv, "compose", "compose ran after keploy refused")
			}
		})
	}
}

// classicSudo stands in for sudo at version, which prints "Sudo version …" on
// stdout for --version, and translates it outside the C locale.
func classicSudo(version string) string {
	return `if [ "$1" = --version ]; then
  if [ "$LC_ALL" = C ]; then echo "Sudo version ` + version + `"; else echo "Sudo-Version ` + version + `"; fi
  echo "Sudoers policy plugin version ` + version + `"; exit 0
fi
exec "$@"`
}

// TestCheckAgentTokenHandoff: keploy refuses exactly the sudo that would start
// the agent without its token, and asks nothing of sudo when compose does not
// run under it.
func TestCheckAgentTokenHandoff(t *testing.T) {
	for _, tc := range []struct {
		name, cmd, sudo string
		uid             int
		// refused is what the refusal names; empty when there is none.
		refused string
		// asked is whether keploy ran `sudo --version`.
		asked bool
	}{
		{name: "sudo-rs 0.2.7", cmd: "sudo docker compose up", sudo: sudoRs("0.2.7", true), uid: 1000, asked: true},
		{name: "sudo-rs 0.2.13", cmd: "sudo docker compose up", sudo: sudoRs("0.2.13", true), uid: 1000, asked: true},
		{name: "sudo-rs 0.2.6", cmd: "sudo docker compose up", sudo: sudoRs("0.2.6", false), uid: 1000, refused: "sudo-rs 0.2.6", asked: true},
		{name: "sudo-rs 0.1.0", cmd: "sudo -E docker compose up", sudo: sudoRs("0.1.0", false), uid: 1000, refused: "sudo-rs 0.1.0", asked: true},
		{name: "sudo 1.9.15p5", cmd: "sudo docker compose up", sudo: classicSudo("1.9.15p5"), uid: 1000, asked: true},
		{name: "sudo 1.8.21", cmd: "sudo docker compose up", sudo: classicSudo("1.8.21"), uid: 1000, asked: true},
		{name: "sudo 1.8.20p2", cmd: "sudo docker compose up", sudo: classicSudo("1.8.20p2"), uid: 1000, refused: "sudo 1.8.20", asked: true},
		{name: "a sudo that does not know --version", cmd: "sudo docker compose up", sudo: passThroughSudo, uid: 1000, asked: true},
		{name: "no sudo in the command", cmd: "docker compose up", sudo: sudoRs("0.2.5", false), uid: 1000},
		{name: "sudo-rs 0.2.5, keploy root", cmd: "sudo -E docker compose up", sudo: sudoRs("0.2.5", false), uid: 0},
		{name: "sudo-rs 0.2.5, keploy root, sudo -u", cmd: "sudo -u app docker compose up", sudo: sudoRs("0.2.5", false), uid: 0, refused: "sudo-rs 0.2.5", asked: true},
		{name: "sudo 1.8.20, keploy root, sudo -u", cmd: "sudo -u app docker compose up", sudo: classicSudo("1.8.20"), uid: 0, refused: "sudo 1.8.20", asked: true},
		{name: "sudo-rs 0.2.5, keploy root, sudo -n --", cmd: "sudo -n -- docker compose up", sudo: sudoRs("0.2.5", false), uid: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setEffectiveUID(t, tc.uid)
			// classic sudo translates its version line: keploy has to ask for C.
			t.Setenv("LC_ALL", "de_DE.UTF-8")
			calls := installStubs(t, map[string]string{"sudo": tc.sudo})
			err := (&App{kind: utils.DockerCompose, cmd: tc.cmd}).checkAgentTokenHandoff(context.Background())
			if tc.refused == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.refused)
				// The way out it offers has to be open to this keploy: a root
				// keploy cannot be told to run as root.
				if tc.uid == 0 {
					require.NotContains(t, err.Error(), "as root instead", "keploy is root already")
					require.ErrorContains(t, err, "remove its other options")
				} else {
					require.ErrorContains(t, err, "Run keploy itself as root instead")
				}
			}
			var asked bool
			for _, c := range calls() {
				require.Equal(t, "--version", c.argv, "keploy ran sudo for more than its version")
				asked = true
			}
			require.Equal(t, tc.asked, asked, "whether keploy asked sudo for its version")
		})
	}
}
