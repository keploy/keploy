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
}

type IngressEvent struct {
	PID         uint32
	Family      uint16
	OrigAppPort uint16
	NewAppPort  uint16
	_           uint16 // Padding
}

// Fuzzy-match policy values for config Test.FuzzyMatch and
// OutgoingOptions.FuzzyMatchPolicy. They govern the similarity-based
// fallbacks in mock matching (HTTP Levenshtein/Jaccard, generic Jaccard,
// MySQL partial-shape scoring):
//
//	FuzzyMatchOn   — legacy behaviour: a similarity fallback may serve a mock silently.
//	FuzzyMatchWarn — similarity fallbacks still run, but every fuzzy-served mock
//	                 logs a default-visible Warn naming the mock and score.
//	FuzzyMatchOff  — deterministic replay: similarity guessing is disabled.
//	                 Recorded-order (FIFO/SortOrder) tiebreaks still apply;
//	                 anything that would need a similarity guess becomes a
//	                 structured mock miss instead.
const (
	FuzzyMatchOn   = "on"
	FuzzyMatchWarn = "warn"
	FuzzyMatchOff  = "off"
)

// NormalizeFuzzyPolicy maps an unset/unknown policy to the shipped default
// (FuzzyMatchWarn). Call it at every consumption point so an option struct
// built without the field (older callers, in-cluster paths) behaves exactly
// like the documented default instead of silently diverging per protocol.
func NormalizeFuzzyPolicy(p string) string {
	switch p {
	case FuzzyMatchOn, FuzzyMatchWarn, FuzzyMatchOff:
		return p
	default:
		return FuzzyMatchWarn
	}
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
	SchemaNoiseDetection   bool                           // when true, detect request-body field drift vs the recorded mock and record it as field-path noise (req_body_noise) on the matched mock
	SchemaNoiseStrict      bool                           // when true (replay/enforcement path), an HTTP mock that carries learned req_body_noise must match strictly: every request-body field must match except the learned-noise paths, so a non-noise drift rejects the mock
	FuzzyMatchPolicy       string                         // FuzzyMatchOn/Warn/Off — policy for similarity-based mock-match fallbacks
	SkipTLSMITM            bool
	ConnKey                string // connection-level key for TLSHandshakeStore correlation
	// CapturePackets toggles raw packet capture on the agent's proxy ports
	// for the duration of a Record() session. The recorder flips this via
	// --capture-packets; the agent then stages traffic.pcap + sslkeys.log
	// under its own scratch dir (typically os.TempDir()) — the recorder
	// MUST NOT pass a path here because agent and recorder usually live
	// in different filesystems (separate containers, separate pods,
	// separate hosts). The recorder pulls the bytes back at session end
	// via the agent's /agent/pcap/{traffic,keylog} endpoints and writes
	// them into the local test-set directory itself. Replay (Mock)
	// sessions ignore this flag.
	CapturePackets bool
	// OpportunisticTLSIntercept turns on the sniff-and-hijack
	// passthrough variant: the proxy lets app and upstream relay
	// bytes verbatim while peeking for a TLS ClientHello, and
	// hijacks both halves into a MITM the moment one appears.
	// Surfaced via --opportunistic-tls-intercept so the agent can
	// pick the right per-connection branch in handleConnection.
	OpportunisticTLSIntercept bool
	// MysqlPorts lists destination ports that the proxy should treat as
	// MySQL (or wire-compatible variants like TiDB) — i.e. dial the
	// upstream eagerly on connection accept so the server's Initial
	// Handshake Packet can be relayed. MySQL is a server-speaks-first
	// protocol; the generic dispatch path waits to peek client bytes
	// before dialing and deadlocks otherwise. When nil/empty, the
	// proxy falls back to the built-in defaults [3306, 4000].
	MysqlPorts []uint32
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
	AgentURI                  string
	IsDocker                  bool
	CommandType               string
	EnableTesting             bool
	ProxyPort                 uint32
	IncomingProxyPort         uint16
	DnsPort                   uint32
	Mode                      Mode
	GlobalPassthrough         bool
	CapturePackets            bool
	OpportunisticTLSIntercept bool
	// ChannelBindingShim mirrors config.Record.ChannelBindingShim. Forwarded
	// from orchestrator → agent via the --channel-binding-shim argv flag, the
	// same propagation channel CapturePackets / OpportunisticTLSIntercept use,
	// so containerised agents honour the user's choice without seeing the
	// host's keploy.yml.
	ChannelBindingShim bool
	AgentPort          uint32
	AppPorts           []string
	AppNetworks        []string
	NetworkAliases     map[string][]string
	BuildDelay         uint64
	PassThroughPorts   []uint
	MemoryLimit        uint64
	ConfigPath         string
	// RecordBufferMaxMemoryPerConn mirrors config.Record.RecordBuffer.MaxMemoryPerConnection.
	// Forwarded from orchestrator → agent so containerised agents (docker-compose,
	// k8s sidecar) honour the user's tuning; the agent's filesystem doesn't have
	// the host's keploy.yml, so this is the propagation channel. Zero falls
	// through to the relay package's default. Users override via the
	// orchestrator's --max-memory-per-conn flag, KEPLOY_RECORD_MAX_MEMORY_PER_CONN
	// env, or keploy.yml record.recordBuffer.maxMemoryPerConnection.
	RecordBufferMaxMemoryPerConn uint64
	// RecordBufferQueueSize mirrors config.Record.RecordBuffer.QueueSize.
	// See RecordBufferMaxMemoryPerConn for the propagation rationale.
	RecordBufferQueueSize int
	ExtraArgs             []string
	EnableSampling        int
	// EnableIPv6Redirect controls whether the non-docker BPF cgroup program
	// redirects IPv6 traffic (connect6/bind6/udp6) to the proxy. When true
	// (the default), GetProxyInfo publishes ::ffff:127.0.0.1 so the BPF
	// program can rewrite ::1 destinations to the v4-mapped proxy address.
	// When false, the v6 proxy address is left as all-zero and v6 traffic
	// falls through unredirected — this preserves the legacy zero-address
	// behaviour as an opt-in rollback knob.
	EnableIPv6Redirect bool
	// CAJavaHome, when non-empty, forces the Keploy MITM CA truststore
	// install (installJavaCAForHome) to target $CAJavaHome/lib/security/
	// cacerts using $CAJavaHome/bin/keytool, instead of the PATH-resolved
	// keytool. This is the manual-override knob for the app-aware
	// java.home detector in pkg/agent/proxy/tls/java_detect.go:
	// auto-detection from /proc/<ClientNSPID>/environ +
	// /proc/<ClientNSPID>/exe covers the common SDKMAN / Maven-wrapper /
	// fat-jar cases, but operators can force a specific JDK with
	// --ca-java-home when the app is launched via an exotic launcher
	// that masks both JAVA_HOME and the exe symlink (e.g. containerised
	// runners that re-exec through a wrapper).
	//
	// Empty string = auto-detect (preferred); non-empty = override.
	CAJavaHome string
	// InMemoryCompose holds docker-compose YAML content to avoid writing sensitive
	// environment variables to disk. When non-nil, SetupCompose uses this content
	// directly instead of reading from a file path extracted from the command.
	InMemoryCompose []byte
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
