package docker

import (
	"context"
	"fmt"
	"runtime"

	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/utils"
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
	return sudoCannotKeepAgentToken(runtime.GOOS, utils.SudoVersion(ctx), root)
}

// sudoCannotKeepAgentToken returns why the sudo that printed version (see
// utils.SudoVersion) would not hand its command the variable
// --preserve-env=KEPLOY_AGENT_TOKEN names, or nil if nothing says it would
// not. goos is the platform keploy runs on, and root whether keploy is root.
//
// These are refused here, before any agent exists:
//   - a sudo that ignores or rejects the list (utils.SudoPreserveEnvListGap):
//     sudo-rs before 0.2.7 would start the agent without a token, and sudo
//     before 1.8.21 fails as a usage error;
//   - Windows' sudo, which rejects it too: its --preserve-env is -E, a flag
//     that takes no value, and nothing there keeps a single variable.
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
	gap, ok := utils.SudoPreserveEnvListGap(version)
	switch {
	case !ok:
		return nil
	case gap.Ignored:
		return fmt.Errorf("keploy cannot hand its agent the control-plane token through this sudo: %s ignores --preserve-env=%s (sudo-rs implements it from %s on), so the agent would start without a token and serve its control plane to anyone who can reach it. %s. Or upgrade sudo-rs to %s or later",
			gap, token.Env, gap.SinceString(), remedy, gap.SinceString())
	default:
		return fmt.Errorf("keploy cannot hand its agent the control-plane token through this sudo: %s does not accept --preserve-env=%s (sudo accepts it from %s on). %s. Or upgrade sudo to %s or later",
			gap, token.Env, gap.SinceString(), remedy, gap.SinceString())
	}
}
