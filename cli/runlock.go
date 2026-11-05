package cli

import (
	"os"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// acquireRunLock takes the keploy folder's run lock for a record or test run and
// returns the func that releases it. ok is false when another run holds the lock;
// the failure is logged and the exit code armed, so the caller only has to return.
//
// The run that docker re-executes inside the keploy container (BINARY_TO_DOCKER)
// skips the lock: the host process that started it already holds it, and with
// --pid=host it would see that process alive and refuse to start.
func acquireRunLock(logger *zap.Logger, conf *config.Config) (release func(), ok bool) {
	if conf == nil || conf.Path == "" || os.Getenv("BINARY_TO_DOCKER") == "true" {
		return func() {}, true
	}
	release, err := utils.AcquireRunLock(conf.Path)
	if err != nil {
		utils.LogError(logger, err, "failed to start")
		utils.SetExitCodeOnce(utils.ExitKeployError)
		return nil, false
	}
	return release, true
}
