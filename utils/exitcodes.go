package utils

import "errors"

// Keploy's own exit codes.
//
// THE CONTRACT, and the reason it is narrow:
//
//	0        the wrapped test command succeeded (or there was none)
//	<runner> the wrapped test command's OWN exit code, propagated verbatim.
//	         Every CI job depends on this, so Keploy must never overwrite it —
//	         see mock.propagateExit.
//	1        a generic Keploy-side failure
//	3,4,6    a SPECIFIC Keploy-side failure, listed below (5 is the enterprise
//	         build's: a command that needs a session it cannot use)
//
// The specific codes exist so a caller can react correctly instead of pattern
// matching log text or guessing from a bare 1. The VS Code extension, for
// example, used to show an eBPF/setcap tutorial on ANY non-zero exit while
// unelevated on Linux — which meant a plainly failing `pytest` (exit 1, the most
// common non-zero code there is) told the user they had a permissions problem.
//
// These are only ever set for a Keploy-side failure, so nothing that succeeded
// starts failing. Deliberately kept clear of the shell's reserved range (126,
// 127) and of 128+N signal codes.
const (
	// ExitKeployError is the generic Keploy-side failure.
	ExitKeployError = 1

	// ExitPrivilegeRequired means Keploy could not obtain the kernel privileges
	// it needs (eBPF: CAP_SYS_ADMIN / CAP_BPF / CAP_NET_ADMIN / CAP_PERFMON).
	// NOT retrying, and not anything to do with the user's tests. Where Keploy
	// ran unelevated, the remedy is `setcap` on the binary once, or running
	// elevated. Where it already ran as root — the Linux agent always does —
	// only the container or the Docker daemon it runs under can grant them.
	ExitPrivilegeRequired = 3

	// ExitUnsupportedPlatform means the requested mode cannot work on this
	// OS/arch at all — e.g. a native command where only a container command is
	// supported. Retrying is pointless; the command shape must change.
	ExitUnsupportedPlatform = 4

	// ExitEnvironmentUnsupported means Keploy had the privileges it asked for,
	// but the machine or container it runs in lacks something the
	// instrumentation cannot start without: tracefs/debugfs not mounted, or,
	// for an agent started with --is-docker, no non-loopback IPv4 address for
	// the applications it serves to reach it at. The remedy is to change that
	// environment (mount tracefs, give the agent's container a network) — not
	// setcap, not sudo, and nothing to do with the tests. Not 5: the
	// enterprise build uses 5 for a command that needs a session it cannot
	// use.
	ExitEnvironmentUnsupported = 6
)

// The errors the exit codes above are derived from. A failure is tagged with
// one where it happens, and the tag has to survive every layer it crosses —
// wrap with %w, never replace the error — because the process that exits may
// not be the one that failed: the agent is a separate process, and it hands
// its verdict to the CLI that launched it as its own exit status.
var (
	// ErrPrivilegeRequired: Keploy lacks a privilege it needs, or the kernel
	// refused it an operation that needs one (the eBPF load, the memlock
	// rlimit; in docker mode, the capabilities the CLI checks for and the
	// perf_event_paranoid it lowers).
	ErrPrivilegeRequired = errors.New("keploy does not have the kernel privileges it needs")

	// ErrEnvironmentUnsupported: the operation was allowed, but the
	// environment lacks what it depends on.
	ErrEnvironmentUnsupported = errors.New("this environment lacks something keploy needs")
)

// ExitCodeFor is the exit code a Keploy-side failure carrying err should end
// the process with: the specific code err was tagged with, else the generic 1.
func ExitCodeFor(err error) int {
	switch {
	case errors.Is(err, ErrPrivilegeRequired):
		return ExitPrivilegeRequired
	case errors.Is(err, ErrEnvironmentUnsupported):
		return ExitEnvironmentUnsupported
	default:
		return ExitKeployError
	}
}

// SetExitCodeOnce records a specific Keploy-side exit code, but never clobbers
// a code already set — in particular the wrapped runner's own code, which
// propagateExit may have installed first and which outranks ours.
func SetExitCodeOnce(code int) {
	if ErrCode == 0 {
		ErrCode = code
	}
}
