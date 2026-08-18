//go:build !windows

package utils

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/keploy/shlex"
)

// shellOperators are the shell control/substitution characters we refuse to run
// without a shell: pipes, sequencing, redirection, subshells, and variable or
// command substitution. Their presence means the command cannot be run by
// exec'ing a single binary. The check is on the raw string, so a quoted
// occurrence (e.g. --regex '^a$') is rejected too — erring on the side of a
// hard, explained error rather than risking a misinterpretation.
//
// It deliberately omits the filename-expansion characters (* ? [ ] { } ~): the
// shell-free fallback does not glob or expand them and passes them to the
// binary literally, matching what most programs already expect when they do
// their own argument globbing.
const shellOperators = "|&;<>()$`"

// CommandContext builds an *exec.Cmd that runs cmdStr on Unix-like systems.
//
// When sh is on PATH (the normal case — local dev, standard CI, any image with
// a shell) it returns `sh -c <cmdStr>`, identical to the previous
// exec.Command("sh", "-c", cmdStr). LookPath is used only to probe for sh; the
// bare "sh" name is kept so cmd.Args is byte-for-byte the previous behaviour.
//
// When no shell is present — e.g. a distroless runtime image that ships only
// the application's own binaries — it falls back to tokenizing cmdStr and
// exec'ing the binary directly, which needs no shell. That fallback cannot run
// commands that rely on shell control operators (see shellOperators), so it
// returns an explanatory error for those instead of mis-executing an operator
// as a literal argument.
//
// The fallback only ever runs when sh is genuinely missing, so it cannot change
// the behaviour of any environment that works today: it can only turn the
// current hard "sh: executable file not found" failure into a working direct
// exec for shell-free commands (e.g. `docker compose -f - up ...`).
func CommandContext(ctx context.Context, cmdStr string) (*exec.Cmd, error) {
	// Presence probe only — keep the bare "sh" so the resulting cmd matches the
	// previous exec.Command("sh", "-c", cmdStr) exactly.
	if _, err := exec.LookPath("sh"); err == nil {
		return exec.CommandContext(ctx, "sh", "-c", cmdStr), nil
	}

	if i := strings.IndexAny(cmdStr, shellOperators); i >= 0 {
		return nil, fmt.Errorf(
			"command needs a shell to interpret %q but no /bin/sh is present in this image: %s",
			string(cmdStr[i]), cmdStr)
	}

	args, err := shlex.Split(cmdStr)
	if err != nil {
		return nil, fmt.Errorf("could not parse command without a shell: %q: %w", cmdStr, err)
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("empty command")
	}

	return exec.CommandContext(ctx, args[0], args[1:]...), nil
}
