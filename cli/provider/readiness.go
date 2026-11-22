package provider

import (
	"fmt"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// The readiness ceiling, which the proven model deletes (design v14 U18, row
// test.healthPollTimeout; release-1 row W6): keploy test's first test waits
// for the application's readiness event (it listens where the test dials, or
// its health URL answers), however long its start takes, and a start that
// makes no progress across the watchdog's count is not judged. A ceiling
// after which replay fired its tests at an application that was not ready
// has nothing left to bound. Under the switch the flag is refused, as an
// unknown flag will be at the flip, and the keploy.yml key is ignored, with a
// notice when it was set to anything but the default keploy writes.

// readinessHint is what the refusal and the notice tell the user.
const readinessHint = "the first test waits for the application's readiness event (it listens where the test dials, or its --health-url answers), however long its start takes; a start that makes no progress (no listen, connect, fork or exec, and no CPU time) across keploy's watchdog count is not judged"

// refuseHealthPollTimeout refuses, under the proven switch, --health-poll-timeout.
func refuseHealthPollTimeout(cmd *cobra.Command) error {
	if !provenswitch.On() {
		return nil
	}
	if f := cmd.Flags().Lookup("health-poll-timeout"); f != nil && f.Changed {
		// A usage error (exit 8), as it will be an unknown flag at the flip.
		return utils.UsageError(fmt.Errorf("--health-poll-timeout is not used with %s (U18): %s", provenswitch.Env, readinessHint))
	}
	return nil
}

// dropHealthPollTimeout ignores, under the proven switch, keploy.yml's
// test.healthPollTimeout, saying so when it was set to anything but the
// default.
func dropHealthPollTimeout(logger *zap.Logger, cfg *config.Config) {
	if !provenswitch.On() {
		return
	}
	if t := cfg.Test.HealthPollTimeout; t > 0 && t != config.DefaultHealthPollTimeout {
		logger.Warn(fmt.Sprintf("keploy.yml test.healthPollTimeout is ignored under %s (U18): %s", provenswitch.Env, readinessHint),
			zap.Duration("healthPollTimeout", t))
	}
	cfg.Test.HealthPollTimeout = 0
}
