package app

import (
	"context"
	"os"
	"slices"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/platform/docker"
)

// effectiveUID is os.Geteuid, a variable so that tests can run as root.
var effectiveUID = os.Geteuid

// agentTokenCommand returns a compose command as it has to run for compose to
// find the agent's control-plane token in the environment keploy runs it with,
// and whether that then depends on sudo keeping it (viaSudo).
//
// The generated keploy-agent service names the token without a value, and
// compose fills it in from its own environment (App.withAgentToken). A leading
// sudo or doas builds that environment afresh (sudo's env_reset, doas without
// keepenv), so the token has to get through it:
//
//   - When keploy is root, a leading `sudo` or `doas` with no options but those
//     in rootNoOps (`sudo -E`, say) elevates nothing and is dropped: compose
//     inherits the token directly. That covers every Linux compose run whose
//     command keploy is given on its command line, because main re-executes
//     keploy under sudo before it runs one, and it does not depend on which
//     `sudo` is on PATH. A pass-through stand-in in a root CI image would run
//     --preserve-env=… as the command, sudo-rs before 0.2.7 ignores
//     --preserve-env=NAME, and no sudo-rs honours -E. The linux `docker run`
//     alias does the same (docker.linuxDockerClient).
//   - Otherwise a leading sudo is told --preserve-env=KEPLOY_AGENT_TOKEN, which
//     names that one variable and no other, and viaSudo is true. App.Setup
//     then refuses to go on with a sudo that cannot keep it
//     (docker.CheckSudoKeepsAgentToken).
//
// A dropped wrapper leaves compose with keploy's own environment, the one
// keploy's own `docker compose down` runs with, instead of one that sudo
// resets. Compose reaches the daemon that DOCKER_HOST names, as the Engine API
// client does (client.FromEnv), but that client reads no CLI context: a
// DOCKER_CONTEXT, or a currentContext in the docker config, can send compose
// to another daemon. When main re-executed keploy with classic sudo's -E, that
// environment keeps the user's HOME (unless sudoers sets always_set_home), so
// compose reads the user's ~/.docker (logins, credential helpers, contexts)
// where `sudo docker compose` read root's.
//
// Anything else is left as it is: a sudo inside a wrapper script, which keploy
// cannot see, and a doas that has to elevate. doas cannot be told to keep one
// variable, and keploy cannot tell whether doas.conf does it (keepenv, or
// setenv { KEPLOY_AGENT_TOKEN }); refusing it would refuse the doas setups
// that work. If it drops the token, the self-check's ERROR says so and how to
// keep it (pkg.verifyControlPlaneGuarded).
func agentTokenCommand(appCmd string, root bool) (cmd string, viaSudo bool) {
	body := strings.TrimLeft(appCmd, " \t")
	indent := appCmd[:len(appCmd)-len(body)]
	wrapper, args := cutWord(body)
	if (wrapper != "sudo" && wrapper != "doas") || args == "" {
		return appCmd, false
	}
	if root {
		if rest, ok := withoutRootWrapper(wrapper, args); ok {
			return indent + rest, false
		}
	}
	if wrapper != "sudo" {
		return appCmd, false
	}
	return indent + "sudo --preserve-env=" + token.Env + " " + args, true
}

// rootNoOps are the options that leave a leading sudo or doas, run by root,
// doing nothing but resetting the environment: short ones, which can be
// grouped (-En), and long ones. For sudo they are the ones that ask it to keep
// that environment whole (-E), and for both the ones about how to ask for a
// password (-n, and sudo's -S), which a command keploy runs as root anyway has
// no use for. `--`, which ends the options, counts for both.
var rootNoOps = map[string]struct {
	short string
	long  []string
}{
	"sudo": {short: "EnS", long: []string{"--preserve-env", "--non-interactive", "--stdin"}},
	"doas": {short: "n"},
}

// withoutRootWrapper returns args, the words after a leading sudo or doas,
// without the options rootNoOps lists for it. It returns false if any other
// option is there (`sudo -u app docker compose up` still runs compose as
// someone else), or no command.
func withoutRootWrapper(wrapper, args string) (string, bool) {
	noOps := rootNoOps[wrapper]
	for {
		word, rest := cutWord(args)
		switch {
		case word == "":
			return "", false
		case word == "--":
			return rest, rest != ""
		case !strings.HasPrefix(word, "-"):
			return args, true
		case slices.Contains(noOps.long, word):
		case len(word) > 1 && strings.Trim(word[1:], noOps.short) == "":
		default:
			return "", false
		}
		args = rest
	}
}

// cutWord splits s, which starts with no blank, at its first run of spaces or
// tabs.
func cutWord(s string) (word, rest string) {
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimLeft(s[i:], " \t")
}

// checkAgentTokenHandoff refuses a compose command whose sudo would start the
// agent without its control-plane token. It runs before compose does, and so
// before any agent exists (see agentTokenCommand).
func (a *App) checkAgentTokenHandoff(ctx context.Context) error {
	root := effectiveUID() == 0
	if _, viaSudo := agentTokenCommand(a.cmd, root); !viaSudo {
		return nil
	}
	return docker.CheckSudoKeepsAgentToken(ctx, root)
}
