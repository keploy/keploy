//go:build !linux

package http

import "go.uber.org/zap"

// adoptDaemons keeps a daemon the application starts in the run (adopt_linux.go).
// Linux only: on macOS the shim a daemon inherits keeps it a member, and
// Windows has no reparenting.
func adoptDaemons(*zap.Logger) {}

// stopAdopted ends what adoptDaemons adopted: nothing here.
func stopAdopted(*zap.Logger) {}
