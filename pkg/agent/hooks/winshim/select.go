//go:build windows && amd64

package winshim

import (
	"os"
	"strings"
	"unsafe"

	"go.uber.org/zap"
	"golang.org/x/sys/windows"
)

// Selected reports whether this run should use the unprivileged userspace
// backend instead of the WinDivert one.
//
// The rule is deliberately narrow: WinDivert stays the default wherever it can
// actually work, so an elevated run behaves exactly as it did before. The
// userspace backend is chosen only when
//
//   - the process is not elevated, where WinDivert cannot load its driver at all
//     and the run would otherwise fail with "administrator privileges required";
//     or
//   - the operator asked for it explicitly with KEPLOY_WINDOWS_USERSPACE, which
//     exists so the path can be exercised (and its CI lane run) on a machine
//     that happens to be elevated.
//
// The client and the agent both call this and must agree. They do: on Windows
// the agent is started by the client without elevation (see
// pkg/platform/http/utils/elevate.go, AgentNeedsElevation), so both processes
// observe the same elevation state, and the environment variable is inherited.
// It logs at debug only. Both the agent and the CLI call it, so anything louder
// would tell the user the same thing twice; the CLI announces the choice once,
// where the user is actually looking (see prepareNativeInterception).
func Selected(logger *zap.Logger) bool {
	if forced, ok := forcedByEnv(); ok {
		if logger != nil {
			logger.Debug("Windows interception backend chosen by "+EnvForceUserspace,
				zap.Bool("userspace", forced))
		}
		return forced
	}
	if IsElevated() {
		return false
	}
	if logger != nil {
		logger.Debug("not elevated; using the unprivileged Windows interception backend")
	}
	return true
}

// forcedByEnv reads the KEPLOY_WINDOWS_USERSPACE override. The second return
// reports whether it was set at all, so an explicit "0" can pin WinDivert.
func forcedByEnv() (bool, bool) {
	v, ok := os.LookupEnv(EnvForceUserspace)
	if !ok {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}

// IsElevated reports whether the current process has an elevated token, which is
// what WinDivert needs in order to load its kernel driver.
func IsElevated() bool {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return false
	}
	defer func() { _ = token.Close() }()

	var elevation uint32
	var returnedLen uint32
	err := windows.GetTokenInformation(token, windows.TokenElevation,
		(*byte)(unsafe.Pointer(&elevation)), uint32(unsafe.Sizeof(elevation)), &returnedLen)
	if err != nil {
		return false
	}
	return elevation != 0
}
