package models

import (
	"time"

	"go.keploy.io/server/v2/config"
)

type HookOptions struct {
}

// TODO: Role of SQLDelay should be mentioned in the comments.
type OutgoingOptions struct {
	Rules         []config.BypassRule
	MongoPassword string
	SQLDelay      time.Duration // This is the same as Application delay.
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
	ServeTest   bool
	DockerDelay time.Duration
}
