package models

import (
	"go.keploy.io/server/v2/config"
	"time"
)

type HookOptions struct {
	// if pid==0, we use keploy's pid. Since keploy is the parent process
	// for all processes started by it.
	Pid        uint32
	KeployIPv4 string
}

type OutgoingOptions struct {
	Rules         []config.BypassRule
	MongoPassword string
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
