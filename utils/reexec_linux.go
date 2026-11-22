//go:build linux

package utils

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unicode/utf8"

	"go.keploy.io/server/v3/pkg/platform/engine"
	keployLog "go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
)

// execProcess is syscall.Exec, which ReexecWithSudo replaces this process
// with sudo through; a test swaps in one that looks at what the process leaves
// behind at that moment.
var execProcess = syscall.Exec

// sudoVersionOf is SudoVersion; a test swaps in what a given sudo prints.
var sudoVersionOf = SudoVersion

// ReexecWithSudo re-executes the current keploy command with sudo.
// This is used when a Docker/Docker Compose command is detected and keploy is not running as root.
// Uses syscall.Exec to replace the current process entirely - no parent process remains.
// The re-executed keploy gets this process's environment through sudo's
// --preserve-env list (sudoReexecArgs), and a sudo that would not keep it is
// refused before anything runs (sudoReexecRefusal).
// This function never returns on success. On a failure it says why and returns
// the error, for the caller to end the run with exit code 1 through its own
// return: an os.Exit here skipped every deferred call of the run, the removal
// of its temp log among them.
func ReexecWithSudo(logger *zap.Logger) error {
	// Find sudo binary
	sudoPath, err := exec.LookPath("sudo")
	if err != nil {
		logger.Error("sudo not found in PATH, cannot re-execute with elevated privileges", zap.Error(err))
		fmt.Println("Error: sudo is required to run Docker commands. Please install sudo or run as root.")
		return fmt.Errorf("find sudo: %w", err)
	}

	// Find keploy binary - use the current executable
	keployPath, err := os.Executable()
	if err != nil {
		logger.Error("failed to get current executable path", zap.Error(err))
		return fmt.Errorf("find this executable: %w", err)
	}

	if gap, ok := SudoPreserveEnvListGap(sudoVersionOf(context.Background())); ok {
		err := sudoReexecRefusal(gap)
		logger.Error("cannot re-execute with sudo", zap.Error(err))
		return err
	}

	args, unkept := sudoReexecArgs(os.Environ(), os.Getenv("PATH"), keployPath, os.Args[1:])
	if len(unkept) > 0 {
		logger.Warn("these environment variables do not reach the re-executed keploy: sudo's --preserve-env list cannot name a variable whose name has a comma or is not UTF-8", zap.Strings("variables", unkept))
	}

	logger.Info("Re-executing with sudo for elevated privileges as Linux Docker/cloud replay path needs root-level eBPF privileges.")
	logger.Debug("Re-exec command", zap.Strings("args", RedactArgv(args)))

	// The run goes on in the process the exec starts, which creates a log of
	// its own; this one's, a temp file, would be left with no run to remove
	// it. Its name goes now; the file itself, open close-on-exec as Go opens
	// every file, goes at the exec, or with this process if the exec fails.
	if err := keployLog.RemoveLogFile(LogFile); err != nil {
		logger.Debug("failed to remove this process's log before re-executing", zap.Error(err))
	}
	// Use syscall.Exec to replace the current process
	// This means no parent process remains - clean handoff
	err = execProcess(sudoPath, args, os.Environ())
	if err != nil {
		// syscall.Exec only returns on error
		logger.Error("failed to re-execute with sudo", zap.Error(err))
		fmt.Printf("Error: failed to re-execute with sudo: %v\n", err)
		return fmt.Errorf("re-execute with sudo: %w", err)
	}
	return nil
}

// sudoReexecArgs is the command ReexecWithSudo execs: from this process's
// environment, its PATH, its executable and its arguments,
//
//	sudo --preserve-env=NAME,… env PATH=<PATH> <keploy> <arguments>
//
// sudo resets the environment of its command. The re-executed keploy, and the
// app and compose it runs, need the user's: a compose file's ${VAR}, the
// app's settings, keploy's own. `sudo -E` asked for all of it, and sudo-rs
// (Ubuntu 26.04's sudo) ignores -E ("preserving the entire environment
// is not supported"). Both sudo and sudo-rs keep each variable a
// --preserve-env list names wherever the sudoers rule lets the user set
// variables (SETENV, which a rule for ALL implies), and refuse the whole
// command loudly where it does not. So the list names every variable of the
// environment, and their values stay in the environment, off the command line
// that every user on the host can read.
//
// Left out of the list, so that both sudos give what `sudo -E` gave:
//   - PATH: `env` sets it after sudo, where a secure_path would replace it;
//   - USER, LOGNAME, USERNAME and every SUDO_* variable: sudo sets them, to
//     root and to the user who ran sudo;
//   - a name with a comma, which the list cannot carry, and a name that is not
//     UTF-8, for which sudo-rs refuses the whole command line: returned in
//     unkept, for the caller to say so;
//   - a name met before, and an entry with no name.
func sudoReexecArgs(environ []string, path, keploy string, args []string) (argv, unkept []string) {
	var names []string
	seen := map[string]bool{}
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		switch {
		case name == "PATH", name == "USER", name == "LOGNAME", name == "USERNAME", strings.HasPrefix(name, "SUDO_"):
		case strings.Contains(name, ","), !utf8.ValidString(name):
			unkept = append(unkept, name)
		default:
			names = append(names, name)
		}
	}
	argv = []string{"sudo"}
	if len(names) > 0 {
		argv = append(argv, "--preserve-env="+strings.Join(names, ","))
	}
	argv = append(argv, "env", "PATH="+path, keploy)
	return append(argv, args...), unkept
}

// sudoReexecRefusal is why ReexecWithSudo does not re-execute keploy through
// the sudo gap names, which would not keep the user's environment
// (SudoPreserveEnvListGap), and what the user can do instead.
func sudoReexecRefusal(gap SudoListGap) error {
	if gap.Ignored {
		return fmt.Errorf("keploy re-runs itself as root through sudo for this command, and names every variable of your environment for sudo to keep (--preserve-env=NAME,…). %s ignores that list (sudo-rs implements it from %s on), so the run would go on without your environment. Upgrade sudo-rs to %s or later, or run keploy as root yourself, with the variables the run needs: sudo env VAR=value … keploy …",
			gap, gap.SinceString(), gap.SinceString())
	}
	return fmt.Errorf("keploy re-runs itself as root through sudo for this command, and names every variable of your environment for sudo to keep (--preserve-env=NAME,…). %s does not accept that list (sudo accepts it from %s on). Upgrade sudo to %s or later, or run keploy as root yourself: sudo -E keploy … keeps your environment on this sudo",
		gap, gap.SinceString(), gap.SinceString())
}

// ShouldReexecWithSudo checks if keploy should re-execute itself with sudo.
// Returns true if:
// 1. A Docker/Docker Compose command is detected in the -c/--command flag, OR
// 2. The "cloud replay" subcommand is being used (it generates a docker-compose internally)
// AND Keploy is NOT currently running as root.
func ShouldReexecWithSudo() bool {
	// Already running as root - no need to re-exec
	if os.Geteuid() == 0 {
		return false
	}

	// Check if this is a "cloud replay" command, which always needs root
	// because it generates and runs a docker-compose file internally.
	if isCloudReplayCmd(os.Args) {
		return true
	}

	// An explicit --cmd-type wins over sniffing the command string, and has
	// to be read straight out of os.Args: this runs before cobra parses
	// anything and long before any config file is read. Without it,
	// `--cmd-type docker-compose -c "make up"` would take the native path
	// here, start unprivileged, and then fail deep inside the docker branch
	// trying to write /proc/sys/kernel/perf_event_paranoid (#4399).
	// --from-container runs the app as a container, so it needs root for the
	// same reason every other docker kind does. It is checked before --cmd-type
	// because it does not require one: the flag alone settles the kind.
	if ExtractFromContainerFromArgs(os.Args) != "" {
		return true
	}

	// Extract the command from arguments
	cmd := ExtractCommandFromArgs(os.Args)

	// A container engine this build cannot drive is refused by ValidateFlags
	// with a message saying so. Asking for a sudo password first, only to be
	// refused after it, would be the worse experience.
	if cmd != "" && !engine.Supported(engine.Detect(cmd)) {
		return false
	}

	if explicit := ExtractCmdTypeFromArgs(os.Args); explicit != "" {
		switch kind := CmdType(explicit); kind {
		case Native, DockerRun, DockerStart, DockerCompose, FromContainer:
			return needsRoot(kind)
		}
		// Anything else is rejected later by ValidateFlags with a proper
		// message; fall through to sniffing rather than guessing here.
	}

	if cmd == "" {
		return false
	}

	// Check if it's a Docker command
	return needsRoot(FindDockerCmd(cmd))
}

// needsRoot reports whether a command of this kind re-executes keploy under
// sudo: the container kinds, except docker-start, which ValidateFlags refuses
// (it cannot work) and which must not ask for a password first.
func needsRoot(kind CmdType) bool {
	return IsDockerCmd(kind) && kind != DockerStart
}

// isCloudReplayCmd checks if the args represent the "keploy cloud replay" subcommand.
func isCloudReplayCmd(args []string) bool {
	// It is tolerant of flags that may appear before or between subcommands,
	// e.g. `keploy --debug cloud replay` or `keploy cloud --debug replay`.
	if len(args) < 3 {
		return false
	}

	// Skip args[0] (program name) and search for "cloud" followed by "replay".
	for i := 1; i < len(args); i++ {
		if args[i] != "cloud" {
			continue
		}

		// Look ahead for "replay", allowing flags and their separate values between
		// "cloud" and "replay" (e.g. `cloud --config /tmp/cfg replay`).
		for j := i + 1; j < len(args); j++ {
			token := args[j]
			if token == "replay" {
				return true
			}

			// End-of-options marker; next token must be the subcommand.
			if token == "--" {
				if j+1 < len(args) && args[j+1] == "replay" {
					return true
				}
				break
			}

			// If this is not a flag, this isn't "cloud replay".
			if !strings.HasPrefix(token, "-") {
				break
			}

			// Flag with inline value (`--config=/tmp/cfg`) is fully consumed.
			if strings.Contains(token, "=") {
				continue
			}

			// Heuristically treat the next non-flag token as a flag value and skip it.
			// If the next token is "replay", keep it as the subcommand candidate.
			if j+1 < len(args) && args[j+1] != "replay" && !strings.HasPrefix(args[j+1], "-") {
				j++
			}
		}
	}

	return false
}
