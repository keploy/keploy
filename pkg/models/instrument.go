package models

import (
	"crypto/tls"
	"time"
)

type BypassRule struct {
	Path string `json:"path" yaml:"path" mapstructure:"path"`
	Host string `json:"host" yaml:"host" mapstructure:"host"`
	Port uint   `json:"port" yaml:"port" mapstructure:"port"`
}

type Filter struct {
	BypassRule   `mapstructure:",squash"`
	URLMethods   []string          `json:"urlMethods" yaml:"urlMethods" mapstructure:"urlMethods"`
	Headers      map[string]string `json:"headers" yaml:"headers" mapstructure:"headers"`
	MatchType    MatchType         `json:"matchType" yaml:"matchType" mapstructure:"matchType"`
	FilterPolicy FilterPolicy      `json:"filterPolicy" yaml:"filterPolicy" mapstructure:"filterPolicy"`
}

type MatchType string

const (
	OR  MatchType = "OR"
	AND MatchType = "AND"
)

type FilterPolicy string

const (
	Include FilterPolicy = "include"
	Exclude FilterPolicy = "exclude"
)

type HookOptions struct {
	Rules         []BypassRule
	Mode          Mode
	EnableTesting bool
	Port          uint32 // used for e2e filtering
	IsDocker      bool
	ProxyPort     uint32
	ServerPort    uint32
	CgroupPath    string // optional: explicit cgroupv2 path override (used by DaemonSet agent)
	SkipHooks     bool   // skip eBPF hook and proxy loading (used by DaemonSet orchestrator)
}

type IngressEvent struct {
	PID         uint32
	Family      uint16
	OrigAppPort uint16
	NewAppPort  uint16
	_           uint16 // Padding
}

type OutgoingOptions struct {
	Rules         []BypassRule
	MongoPassword string
	TLSPrivateKey string
	Synchronous   bool
	// TODO: role of SQLDelay should be mentioned in the comments.
	SQLDelay               time.Duration // This is the same as Application delay.
	Mocking                bool          // used to enable/disable mocking
	DstCfg                 *ConditionalDstCfg
	Backdate               time.Time                      // used to set backdate in cacert request
	NoiseConfig            map[string]map[string][]string // noise configuration for mock matching (body, header, etc.)
	DisableAutoHeaderNoise bool                           // when true, skip injecting default flaky headers (e.g. AWS SigV4) into noise
	SkipTLSMITM            bool
	ConnKey                string // connection-level key for TLSHandshakeStore correlation
}

type ConditionalDstCfg struct {
	Addr   string // Destination Addr (ip:port)
	Port   uint
	TLSCfg *tls.Config
}

type IncomingOptions struct {
	Filters  []Filter
	BasePath string
}

type SetupOptions struct {
	ClientNSPID     uint32
	Container       string
	KeployContainer string
	DockerDelay     uint64
	Synchronous     bool
	// Cmd               string
	AgentURI          string
	IsDocker          bool
	CommandType       string
	EnableTesting     bool
	ProxyPort         uint32
	IncomingProxyPort uint16
	DnsPort           uint32
	Mode              Mode
	GlobalPassthrough bool
	AgentPort         uint32
	AppPorts          []string
	AppNetworks       []string
	NetworkAliases    map[string][]string
	BuildDelay        uint64
	PassThroughPorts  []uint
	ConfigPath        string
	ExtraArgs         []string
	EnableSampling    int
	// InMemoryCompose holds docker-compose YAML content to avoid writing sensitive
	// environment variables to disk. When non-nil, SetupCompose uses this content
	// directly instead of reading from a file path extracted from the command.
	InMemoryCompose []byte
	CgroupPath      string // optional: explicit cgroupv2 path override (used by DaemonSet agent)
	SkipHooks       bool   // skip eBPF hook and proxy loading (used by DaemonSet orchestrator)
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
