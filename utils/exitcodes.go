package utils

// Keploy's own exit codes.
//
// THE CONTRACT, and the reason it is narrow:
//
//	0        the wrapped test command succeeded (or there was none)
//	<runner> the wrapped test command's OWN exit code, propagated verbatim.
//	         Every CI job depends on this, so Keploy must never overwrite it —
//	         see mock.propagateExit.
//	1        a generic Keploy-side failure
//	3,4      a SPECIFIC Keploy-side failure, listed below
//
// The specific codes exist so a caller can react correctly instead of pattern
// matching log text or guessing from a bare 1. The VS Code extension, for
// example, used to show an eBPF/setcap tutorial on ANY non-zero exit while
// unelevated on Linux — which meant a plainly failing `pytest` (exit 1, the most
// common non-zero code there is) told the user they had a permissions problem.
//
// These are only ever set for failures that ALREADY exited 1, so nothing that
// previously succeeded starts failing. Deliberately kept clear of the shell's
// reserved range (126, 127) and of 128+N signal codes.
const (
	// ExitKeployError is the generic Keploy-side failure.
	ExitKeployError = 1

	// ExitPrivilegeRequired means Keploy could not obtain the kernel privileges
	// it needs (eBPF: CAP_SYS_ADMIN / CAP_BPF / CAP_NET_ADMIN / CAP_PERFMON).
	// The remedy is `setcap` on the binary once, or running elevated — NOT
	// retrying, and not anything to do with the user's tests.
	ExitPrivilegeRequired = 3

	// ExitUnsupportedPlatform means the requested mode cannot work on this
	// OS/arch at all — e.g. a native command where only a container command is
	// supported. Retrying is pointless; the command shape must change.
	ExitUnsupportedPlatform = 4
)

// SetExitCodeOnce records a specific Keploy-side exit code, but never clobbers
// a code already set — in particular the wrapped runner's own code, which
// propagateExit may have installed first and which outranks ours.
func SetExitCodeOnce(code int) {
	if ErrCode == 0 {
		ErrCode = code
	}
}
