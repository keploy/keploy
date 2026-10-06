package utils

import (
	"errors"
	"strings"
)

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
//	8        a USAGE error — a mistyped or unknown command/verb, a flag that
//	         could not be parsed, or the wrong number of arguments. Not a
//	         Keploy failure and not the runner: the command never ran. Returned straight from main's exitCodeForCmdErr
//	         (it does not flow through ErrCode / SetExitCodeOnce), so it is the
//	         one code here not tied to an ErrCode.
//
// Every code of Keploy's own stays below 10: the enterprise build's `keploy ui`
// contract (ui-capture's spec/reserved-exit-codes.json) gives 10-125 to its
// own codes, and that build carries these.
//
// The specific codes exist so a caller can react correctly instead of pattern
// matching log text or guessing from a bare 1. The VS Code extension, for
// example, used to show an eBPF/setcap tutorial on ANY non-zero exit while
// unelevated on Linux — which meant a plainly failing `pytest` (exit 1, the most
// common non-zero code there is) told the user they had a permissions problem.
//
// Apart from 8 (a usage error, above), these are only ever set for a Keploy-side
// failure, so nothing that succeeded starts failing. Deliberately kept clear of
// the shell's reserved range (126, 127) and of 128+N signal codes.
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

	// ExitUsageError means the command line itself was wrong — a mistyped or
	// unknown command/verb (e.g. `keploy mock bogus`), a flag that could not be
	// parsed (`keploy test --typo`, `--delay abc`), or the wrong number of
	// arguments (`keploy diff a b c`). It is NOT a Keploy-side
	// failure and NOT the wrapped runner failing: the command never ran. A CI
	// job can tell "the invocation was wrong" apart from "Keploy ran and
	// failed" (1) by this code, instead of a mistyped verb silently exiting 0
	// (design §P0b). It is 8 because every lower code is taken: 2 is what Go's
	// runtime exits with on an unrecovered panic or a fatal error, so a crash
	// would read as a mistyped command, and the enterprise build's `keploy ui`
	// contract uses 0-7. Not sysexits' EX_USAGE (64): that lies in 10-125,
	// which the same contract keeps for itself.
	ExitUsageError = 8
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

// ErrUsage is what a usage error is: the command line itself was wrong, and
// the command never ran (see ExitUsageError). Errors are tagged with it by
// UsageError where cobra parses the command line, not recognised afterwards
// by their type or text: pflag returns the same error types when keploy's own
// code misuses its flag API (reading a flag that was never defined), and that
// is a keploy bug, not a mistyped command.
var ErrUsage = errors.New("the command line is not one keploy accepts")

type usageError struct{ err error }

func (e *usageError) Error() string        { return e.err.Error() }
func (e *usageError) Unwrap() error        { return e.err }
func (e *usageError) Is(target error) bool { return target == ErrUsage }

// UsageError tags err as a usage error, keeping its message. nil stays nil.
// cli.Root tags every command's flag-parse and argument-validation errors with
// it (TagUsageErrors).
func UsageError(err error) error {
	if err == nil {
		return nil
	}
	return &usageError{err: err}
}

// IsUsageError reports whether err says the command line itself was wrong: an
// error tagged by UsageError, or an unknown command or verb (cobra's, or a
// group's rejection from cli.HardenUnknownSubcommands, both `unknown command
// "<verb>" for "<path>"`). This repository's main maps it to ExitUsageError,
// and so must any binary with a main of its own.
func IsUsageError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrUsage) || strings.HasPrefix(err.Error(), `unknown command "`)
}

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
