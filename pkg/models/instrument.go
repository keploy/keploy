package models

import (
	"time"

	"go.keploy.io/server/v2/config"
)

type HookOptions struct {
	Mode          Mode
	EnableTesting bool
}

type OutgoingOptions struct {
	Rules         []config.BypassRule
	MongoPassword string
	// TODO: role of SQLDelay should be mentioned in the comments.
	SQLDelay time.Duration // This is the same as Application delay.
}

type IncomingOptions struct {
	//Filters []config.Filter
}

type SetupOptions struct {
	Container     string
	DockerNetwork string
}

type RunOptions struct {
	//IgnoreErrors bool
	DockerDelay time.Duration
}
