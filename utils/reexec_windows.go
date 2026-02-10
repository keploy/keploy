//go:build windows

package utils

import (
	"go.uber.org/zap"
)

// ReexecWithSudo is a no-op on Windows.
// Docker Desktop on Windows handles permissions differently and doesn't require sudo.
// If this is called on Windows, it means there's a logic error - we should never
// try to re-exec with sudo on Windows.
func ReexecWithSudo(logger *zap.Logger) {
	logger.Debug("ReexecWithSudo called on Windows - this is a no-op")
}

// ShouldReexecWithSudo always returns false on Windows.
// Docker Desktop on Windows handles permissions differently and doesn't require sudo.
func ShouldReexecWithSudo() bool {
	return false
}

// ExtractCommandFromArgs parses os.Args to find the value of -c or --command flag.
// Returns empty string if not found.
func ExtractCommandFromArgs(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]

		// Check for -c or --command
		if arg == "-c" || arg == "--command" {
			// Next argument is the command value
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}

		// Check for -c=value or --command=value format
		if len(arg) > 3 && arg[:3] == "-c=" {
			return arg[3:]
		}
		if len(arg) > 10 && arg[:10] == "--command=" {
			return arg[10:]
		}
	}
	return ""
}
