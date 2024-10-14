package models

import (
	"crypto/tls"
	"time"

	"go.keploy.io/server/v2/config"
)

type HookOptions struct {
	Rules         []config.BypassRule
	Mode          Mode
	EnableTesting bool
}

type OutgoingOptions struct {
	Rules         []config.BypassRule
	MongoPassword string
	// TODO: role of SQLDelay should be mentioned in the comments.
	SQLDelay       time.Duration // This is the same as Application delay.
	FallBackOnMiss bool          // this enables to pass the request to the actual server if no mock is found during test mode.
	Mocking        bool          // used to enable/disable mocking
	DstCfg         *ConditionalDstCfg
}

type ConditionalDstCfg struct {
	Addr   string // Destination Addr (ip:port)
	Port   uint
	TLSCfg *tls.Config
}

type IncomingOptions struct {
	Filters []config.Filter
}

type SetupOptions struct {
	Container     string
	DockerNetwork string
	DockerDelay   uint64
}

type RunOptions struct {
	//IgnoreErrors bool
}

//For test bench

type ModeKey uint32

// These are the keys used to send the keploy record and test ports and pids to the ebpf program when testbench is enabled
const (
	RecordKey ModeKey = 0
	TestKey   ModeKey = 1
)

type TestingOptions struct {
	Mode Mode
}
