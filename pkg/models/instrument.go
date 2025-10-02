package models

import (
	"context"
	"crypto/tls"
	"time"

	"go.keploy.io/server/v2/config"
)

// TestCasePersister defines the function signature for saving a TestCase.
type TestCasePersister func(ctx context.Context, testCase *TestCase) error

type HookOptions struct {
	Rules         []config.BypassRule
	Mode          Mode
	EnableTesting bool
	E2E           bool
	Port          uint32 // used for e2e filtering
	BigPayload    bool
	IsDocker      bool
	ProxyPort     uint32
	ServerPort    uint32
}

type IngressEvent struct {
	PID         uint32
	Family      uint16
	OrigAppPort uint16
	NewAppPort  uint16
	_           uint16 // Padding
}

type OutgoingOptions struct {
	Rules         []config.BypassRule
	MongoPassword string
	// TODO: role of SQLDelay should be mentioned in the comments.
	SQLDelay       time.Duration // This is the same as Application delay.
	FallBackOnMiss bool          // this enables to pass the request to the actual server if no mock is found during test mode.
	Mocking        bool          // used to enable/disable mocking
	DstCfg         *ConditionalDstCfg
	Backdate       time.Time // used to set backdate in cacert request
}

type ConditionalDstCfg struct {
	Addr   string // Destination Addr (ip:port)
	Port   uint
	TLSCfg *tls.Config
}

type IncomingOptions struct {
	Filters  []config.Filter
	BasePath string
}

type SetupOptions struct {
	ClientPID         uint32
	Container         string
	KeployContainer   string
	DockerNetwork     string
	DockerDelay       uint64
	ClientInode       uint64
	AppInode          uint64
	Cmd               string
	IsDocker          bool
	CommandType       string
	EnableTesting     bool
	ProxyPort         uint32
	DnsPort           uint32
	Mode              Mode
	ClientNsPid       uint32
	ClientID          uint64
	AgentIP           string
	GlobalPassthrough bool
	AgentPort         uint32
	AppPorts          []string
	AppNetwork        string
	AppNetworks       []string
}

type RunOptions struct {
	//IgnoreErrors bool
	AppCommand string // command to run the application
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
