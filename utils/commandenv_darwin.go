//go:build darwin

package utils

import (
	"os"

	"go.uber.org/zap"
)

// commandEnviron is the environment keploy gives a command it starts, before
// what the caller adds: keploy's own. (On Linux, what glibc's loader or Go's
// runtime changed in it before keploy's code ran, where a file capability
// started keploy, comes back as keploy was started with it.)
func commandEnviron(_ *zap.Logger) []string { return os.Environ() }
