package models

import "go.keploy.io/server/v2/config"

type HookOptions struct {
	// if pid==0, we use keploy's pid. Since keploy is the parent process
	// for all processes started by it.
	Pid uint32
}

type OutgoingOptions struct {
	Rules         []config.BypassRule
	MongoPassword string
}

type IncomingOptions struct {
	//Filters []config.Filter
}
