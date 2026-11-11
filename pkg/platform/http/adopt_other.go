//go:build !linux

package http

import (
	"go.keploy.io/server/v3/pkg/client/app"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// adoptDaemons keeps a daemon the application starts in the run (adopt_linux.go).
// Linux only: on macOS the shim a daemon inherits keeps it a member, and
// Windows has no reparenting.
func adoptDaemons(*zap.Logger) {}

// stopAdopted ends what adoptDaemons adopted: nothing here.
func stopAdopted(*zap.Logger, []utils.Reached, *utils.TeardownScope) (string, string) { return "", "" }

// appRunsOn says whether the application runs on once its command has
// exited: here, where nothing is adopted, whether a process the command
// started is still in the command's own process group (app.App.GroupRunsOn).
// A daemon that left that group is not seen: the run's caller does not wait
// on it.
func appRunsOn(_ *zap.Logger, ap *app.App) bool { return ap.GroupRunsOn() }
