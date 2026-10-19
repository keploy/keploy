package provider

import (
	"os"

	"go.uber.org/zap"
)

// removedAgentReadyTimeout is the variable that once bounded, in wall time
// (330 s by default), keploy's wait for an agent it did not start itself
// (docker compose's). The wait now follows the agent's start: it goes on
// while the start makes progress, or while the engine does not run the
// agent's container, and ends on a start that failed or stopped making
// progress. Nothing reads the variable.
const removedAgentReadyTimeout = "KEPLOY_AGENT_READY_TIMEOUT"

// warnRemovedAgentReadyTimeout says, when it is set, that keploy no longer
// reads removedAgentReadyTimeout: a user who set it to wait longer, or less,
// learns that it changes nothing, and what the wait follows instead.
func warnRemovedAgentReadyTimeout(logger *zap.Logger) {
	v, ok := os.LookupEnv(removedAgentReadyTimeout)
	if !ok {
		return
	}
	logger.Warn(removedAgentReadyTimeout+" is no longer read, and changes nothing: keploy waits for its agent while the agent's start makes progress, however long that takes, and gives up only on a start that failed or stopped making progress",
		zap.String("value", v))
}
