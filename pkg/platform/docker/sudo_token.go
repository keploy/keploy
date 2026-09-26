package docker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"time"

	"go.keploy.io/server/v3/pkg/agent/token"
)

// sudoKeepingToken is how a command keploy runs through sudo asks it to keep
// the agent's control-plane token: that one variable, and no other.
const sudoKeepingToken = "sudo --preserve-env=" + token.Env

// CheckSudoKeepsAgentToken refuses the `sudo` on PATH when it would start a
// docker or docker compose client without the agent's control-plane token that
// --preserve-env=KEPLOY_AGENT_TOKEN names. Such a client starts the agent
// without a token, and the agent then serves its control plane to anyone who
// can reach it. It is for the commands that hand the token through sudo, and
// runs before they do: the linux `docker run` alias when keploy is not root
// (PrepareDockerCommand), and a compose command under sudo (pkg/client/app).
//
// root is whether keploy runs as root. A root keploy hands the token through
// sudo only when that sudo has options it cannot drop, such as -u, and the
// refusal then says to remove them instead of to run keploy as root.
func CheckSudoKeepsAgentToken(ctx context.Context, root bool) error {
	if runtime.GOOS == "windows" {
		// Refused whatever its version; nothing to ask it.
		return sudoCannotKeepAgentToken(runtime.GOOS, "", root)
	}
	return sudoCannotKeepAgentToken(runtime.GOOS, sudoVersion(ctx), root)
}

// sudoVersionBudget bounds `sudo --version`, which prints and exits without
// asking for a password.
const sudoVersionBudget = 5 * time.Second

// sudoVersion is what `sudo --version` prints on stdout and stderr, or "" if
// there is no sudo on PATH. Classic sudo prints "Sudo version 1.9.15p5" on
// stdout, in the C locale (it translates that line); sudo-rs prints
// "sudo-rs 0.2.5" on stderr.
func sudoVersion(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, sudoVersionBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sudo", "--version")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.WaitDelay = time.Second
	out, _ := cmd.CombinedOutput()
	return string(out)
}

var (
	sudoRsVersionLine = regexp.MustCompile(`(?m)^sudo-rs (\d+)\.(\d+)\.(\d+)`)
	sudoVersionLine   = regexp.MustCompile(`(?m)^Sudo version (\d+)\.(\d+)\.(\d+)`)
)

// sudoCannotKeepAgentToken returns why the sudo that printed version (see
// sudoVersion) would not hand its command the variable
// --preserve-env=KEPLOY_AGENT_TOKEN names, or nil if nothing says it would
// not. goos is the platform keploy runs on, and root whether keploy is root.
//
// Classic sudo from 1.8.21 on, and sudo-rs from 0.2.7 on, either keep the
// variable or refuse to run the command at all. They refuse when the sudoers
// rule has neither SETENV nor the variable in env_keep, and that refusal
// already fails loudly. These are refused here instead, before any agent
// exists:
//   - sudo-rs before 0.2.7 parses --preserve-env=NAME, warns that it "has not
//     yet been implemented and will be ignored", and runs the command without
//     the variable (sudo-rs's CHANGELOG for 0.2.7, and src/sudo/pipeline.rs up
//     to 0.2.6; seen with Debian trixie's sudo-rs 0.2.5). The agent would start
//     without a token.
//   - sudo before 1.8.21 rejects --preserve-env=NAME as a usage error. The list
//     form arrived in 1.8.21, according to sudo's NEWS.
//   - Windows' sudo rejects it too: its --preserve-env is -E, a flag that takes
//     no value, and nothing there keeps a single variable.
//
// Any other sudo is let through. If it drops the token after all, the check
// that the agent enforces one reports it (pkg.verifyControlPlaneGuarded).
func sudoCannotKeepAgentToken(goos, version string, root bool) error {
	remedy := "Run keploy itself as root instead (sudo keploy …): as root, keploy starts docker without sudo, and drops a plain `sudo` or `sudo -E` in front of docker compose, so the token does not have to get through sudo at all"
	if root {
		remedy = "keploy is root already, and drops a plain `sudo` or `sudo -E` in front of docker compose, so that the token does not have to get through sudo at all. It keeps this sudo because of its other options (such as -u), so remove its other options if compose can do without them"
	}
	if goos == "windows" {
		return fmt.Errorf("keploy cannot hand its agent the control-plane token through sudo on Windows: sudo there cannot keep a single environment variable, and it rejects --preserve-env=%s. Docker Desktop does not need sudo: remove it from the command", token.Env)
	}
	if v, ok := parseVersion(sudoRsVersionLine, version); ok && !atLeast(v, [3]int{0, 2, 7}) {
		return fmt.Errorf("keploy cannot hand its agent the control-plane token through this sudo: sudo-rs %d.%d.%d ignores --preserve-env=%s (sudo-rs implements it from 0.2.7 on), so the agent would start without a token and serve its control plane to anyone who can reach it. %s. Or upgrade sudo-rs to 0.2.7 or later",
			v[0], v[1], v[2], token.Env, remedy)
	}
	if v, ok := parseVersion(sudoVersionLine, version); ok && !atLeast(v, [3]int{1, 8, 21}) {
		return fmt.Errorf("keploy cannot hand its agent the control-plane token through this sudo: sudo %d.%d.%d does not accept --preserve-env=%s (sudo accepts it from 1.8.21 on). %s. Or upgrade sudo to 1.8.21 or later",
			v[0], v[1], v[2], token.Env, remedy)
	}
	return nil
}

func parseVersion(line *regexp.Regexp, out string) ([3]int, bool) {
	m := line.FindStringSubmatch(out)
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{}, false
		}
		v[i] = n
	}
	return v, true
}

func atLeast(v, floor [3]int) bool {
	for i := range v {
		if v[i] != floor[i] {
			return v[i] > floor[i]
		}
	}
	return true
}
