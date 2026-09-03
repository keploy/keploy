package models

import (
	"crypto/tls"
	"crypto/x509"
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

// MissPolicy decides what the proxy does in MODE_TEST when an outgoing call
// matches no recorded mock. It is the VCR-style "record mode" for the
// `keploy mock replay` flow.
type MissPolicy string

const (
	// MissFail is the historical, deterministic behaviour: a miss is a hard
	// failure (HTTP 502 / protocol error, connection torn down) and never
	// reaches the real dependency. This is the default.
	MissFail MissPolicy = "fail"
	// MissPassthrough dials the real upstream on a miss, relays the exchange,
	// and does NOT persist it. The equivalent of VCR's `once` on an existing
	// cassette / go-vcr passthrough — new calls succeed against the real
	// dependency but the cassette is not grown.
	MissPassthrough MissPolicy = "passthrough"
	// MissRecord dials the real upstream on a miss, relays the exchange, AND
	// captures it back into the active mock set (VCR `new_episodes`). Newly
	// observed calls are appended so the next replay serves them from the mock.
	MissRecord MissPolicy = "record"
)

// Valid reports whether p is a recognised policy; the empty string is treated
// as MissFail by callers.
func (p MissPolicy) Valid() bool {
	switch p {
	case "", MissFail, MissPassthrough, MissRecord:
		return true
	}
	return false
}

// RecordsOnMiss reports whether the policy captures newly-seen calls.
func (p MissPolicy) RecordsOnMiss() bool { return p == MissRecord }

// PassesThroughOnMiss reports whether the policy dials the real upstream on a
// miss (both passthrough and record do).
func (p MissPolicy) PassesThroughOnMiss() bool {
	return p == MissPassthrough || p == MissRecord
}

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

type OutgoingOptions struct {
	Rules         []BypassRule
	MongoPassword string
	TLSPrivateKey string
	Synchronous   bool
	// TODO: role of SQLDelay should be mentioned in the comments.
	SQLDelay time.Duration // This is the same as Application delay.
	Mocking  bool          // used to enable/disable mocking
	// OnMiss selects what the proxy does in MODE_TEST when no recorded mock
	// matches an outgoing call: "" / "fail" (deterministic hard miss, the
	// default), "passthrough" (dial the real upstream, don't persist), or
	// "record" (dial the real upstream AND capture the exchange back into the
	// active mock set). Only consulted on the `keploy mock replay` path.
	OnMiss                 MissPolicy
	DstCfg                 *ConditionalDstCfg
	Backdate               time.Time                      // used to set backdate in cacert request
	NoiseConfig            map[string]map[string][]string // noise configuration for mock matching (body, header, etc.)
	DisableAutoHeaderNoise bool                           // when true, skip injecting default flaky headers (e.g. AWS SigV4) into noise
	DisableAutoURLDynamic  bool                           // when true, do NOT auto-wildcard machine-id-looking URL path segments (numeric/uuid/hex/token) on the no-exact-match fallback; URL matching stays exact + url-noise only
	SchemaNoiseDetection   bool                           // when true, detect request-body field drift vs the recorded mock and record it as field-path noise (req_body_noise) on the matched mock
	SchemaNoiseStrict      bool                           // when true (replay/enforcement path), a mock (any parser) that carries learned req_body_noise must match strictly: every request-body field must match except the learned-noise paths, so a non-noise drift rejects the mock
	SkipTLSMITM            bool
	ConnKey                string // connection-level key for TLSHandshakeStore correlation
	// PreferH2, on the REPLAY path, tells the TLS MITM to advertise h2 in ALPN
	// (instead of the default http/1.1 downgrade) so a dual-protocol client
	// stays on HTTP/2 and its request matches a recorded kind:Http2 mock.
	// Callers may set it explicitly; the proxy also auto-detects it at the
	// replay handshake from the loaded mock set (any kind:Http2 mock present),
	// so it usually need not be set by hand. Ignored on the record path.
	PreferH2 bool
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
	//
	// Since automatic detection landed this list is an optimisation and
	// an escape hatch rather than a requirement: a port listed here
	// skips the detection probe entirely. See DisableMysqlAutoDetect.
	MysqlPorts []uint32
	// PassThroughPorts / PassThroughHosts mark telemetry / noisy egress that
	// keploy should not record normally (see PassThroughRule). Ports gate on the
	// destination port; Hosts gate on the destination host/authority (needed for
	// TLS egress where the port is not observable at capture). A matched rule is
	// either "skip" (never record; synth success on replay) or "recordOne"
	// (keep one exchange per (host,port,path,method); serve it body-agnostic on
	// replay). Empty ⇒ built-in telemetry defaults only (see MergePassThroughDefaults).
	PassThroughPorts []PassThroughRule
	PassThroughHosts []PassThroughRule
	// PassThroughScope isolates recordOne de-duplication to one app/session. The
	// HTTP integration (and its ptRecorder) is a single instance shared across
	// apps in a long-lived proxyless/DaemonSet agent, so without a scope the first
	// app's recordOne capture would suppress a second app's identical endpoint.
	// Callers that share a recorder across tenants (the enterprise DaemonSet gate)
	// set this to a per-app/per-session key; the classic sidecar leaves it empty
	// (process-per-session, so the plain key already isolates).
	PassThroughScope string
	// DisableMysqlAutoDetect turns off automatic MySQL port detection,
	// restoring the strict "MysqlPorts or nothing" behaviour. With
	// detection on (the default), a MySQL server on any port is
	// identified from its handshake at record time and recalled from
	// the recorded mocks' destAddr at replay time.
	DisableMysqlAutoDetect bool
	// DisableMysqlEndpointDrift stops replay from serving recorded MySQL
	// mocks on a port the recording never saw. Detection still runs; only the
	// inference that covers a moved endpoint is turned off. See
	// Config.DisableMysqlEndpointDrift.
	DisableMysqlEndpointDrift bool
	// SupportsDroppedRevoke is a capability flag set by the CLI on the
	// /outgoing request: when true, the CLI understands the reserved
	// Kind=RevokedTests control frame (it diverts it into a revoke set and
	// deletes those test cases at finalize) rather than trying to persist it
	// as a mock. The agent emits revoke frames ONLY when this is true, so an
	// older CLI that never sets it never receives one — the version-skew
	// guard for the deferred-orphan revoke protocol. Record path only.
	SupportsDroppedRevoke bool
	// UpstreamTLSVerify mirrors config.Record.UpstreamTLS.Verify: when true,
	// keploy's own dial to the REAL destination validates the server's
	// certificate chain and hostname instead of skipping verification.
	//
	// Default false, and deliberately so — a recording proxy must never be
	// stricter than the application it records. An app on sslmode=require /
	// tls=skip-verify chose not to authenticate its upstream; verifying on its
	// behalf would break connections the app would have made, and the failure
	// is silent (the supervisor falls through to raw passthrough and the mock
	// is dropped). This is NOT a CA-bundle limitation: crypto/tls uses the
	// system pool for free when RootCAs is nil.
	UpstreamTLSVerify bool
	// UpstreamTLSRootCAs is the trust anchor set for those verifying dials.
	// nil means "use Go's default" (crypto/tls falls back to the platform
	// root pool) — it never means "trust nothing".
	//
	// json:"-" is load-bearing, not cosmetic. OutgoingOptions is JSON-encoded
	// on the CLI → agent /outgoing request (pkg/platform/http/agent.go), and
	// x509.CertPool has only unexported fields: it would marshal to `{}` and
	// decode on the agent into a NON-NIL EMPTY pool, i.e. a trust store that
	// trusts nothing, failing every handshake. The pool is therefore built
	// agent-side from UpstreamTLSCACert, which does travel: proxy.New settles
	// only the argv-vs-yaml precedence for the CA PATH, and the pool itself is
	// read from disk lazily, under a sync.Once, on the first record session.
	UpstreamTLSRootCAs *x509.CertPool `json:"-"`
	// SrcPid is the kernel (root-namespace) PID of the process that opened THIS
	// outgoing connection, taken from the eBPF redirect map per connection. It
	// is a runtime, per-connection value — never session config — so it is
	// json:"-" (OutgoingOptions is JSON-marshaled CLI→agent and this must not
	// travel as a rule). The proxy stamps it on its per-connection copy of the
	// options and uses it to pick the worker's scoped mock view (per-PID
	// scoping for parallel test runners). 0 ⇒ unknown ⇒ the global pool.
	SrcPid uint32 `json:"-"`
}

type ConditionalDstCfg struct {
	Addr   string // Destination Addr (ip:port)
	Port   uint
	TLSCfg *tls.Config
	// AddrFabricated marks Addr/Port as a stand-in the capture layer
	// synthesized because it could not resolve the connection's REAL
	// destination (e.g. the proxyless SSL-uprobe path substitutes
	// 127.0.0.1:0 when the pid→dest cache is ambiguous, and content
	// matching later forces the well-known port, yielding
	// "127.0.0.1:3306"). Such an address is good enough for parser
	// selection and mock metadata (grouping), but it does NOT point at
	// the server this connection actually talked to — consumers MUST
	// NOT dial it (the MySQL recorder's fetchServerGreeting fallback
	// would otherwise connect to an unrelated local server, or fail
	// instantly with ECONNREFUSED and abort the capture).
	AddrFabricated bool
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
	// MockMode marks a `keploy mock record|replay` session: the agent must
	// NOT relocate the application's listening ports (no ingress/bind hooks)
	// because the wrapped process is a test runner, not a server whose
	// incoming traffic becomes test cases. Only outgoing calls are captured
	// (record) or served (replay). Forwarded to the agent via --mock-mode.
	MockMode                  bool
	GlobalPassthrough         bool
	CapturePackets            bool
	OpportunisticTLSIntercept bool
	// ChannelBindingShim mirrors config.Record.ChannelBindingShim. Forwarded
	// from orchestrator → agent via the --channel-binding-shim argv flag, the
	// same propagation channel CapturePackets / OpportunisticTLSIntercept use,
	// so containerised agents honour the user's choice without seeing the
	// host's keploy.yml.
	ChannelBindingShim bool
	// UpstreamTLSVerify / UpstreamTLSCACert mirror config.Record.UpstreamTLS.
	// Forwarded orchestrator → agent over the --upstream-tls-verify /
	// --upstream-tls-ca-cert argv flags, the same propagation channel
	// CapturePackets / OpportunisticTLSIntercept / ChannelBindingShim use, so a
	// containerised agent honours the operator's choice without ever seeing the
	// host's keploy.yml.
	//
	// The CA pool itself cannot travel — see OutgoingOptions.UpstreamTLSRootCAs
	// — so the PATH travels and the agent loads it locally. That path is
	// resolved on the AGENT's filesystem; for docker/k8s runs the operator must
	// bind-mount the PEM or point at a path that exists inside the container.
	UpstreamTLSVerify bool
	UpstreamTLSCACert string
	AgentPort         uint32
	AppPorts          []string
	AppNetworks       []string
	NetworkAliases    map[string][]string
	BuildDelay        uint64
	PassThroughPorts  []uint
	MemoryLimit       uint64
	ConfigPath        string
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
	// RecordBufferConsumerStallGrace mirrors
	// config.Record.RecordBuffer.ConsumerStallGrace. See
	// RecordBufferMaxMemoryPerConn for the propagation rationale.
	RecordBufferConsumerStallGrace time.Duration

	// RecordBufferHalfCloseGrace mirrors
	// config.RecordBuffer.HalfCloseGrace. Zero means "unset, use the
	// relay default"; NEGATIVE means "disable half-close", so the value
	// must be forwarded on != 0 rather than > 0.
	RecordBufferHalfCloseGrace time.Duration
	ExtraArgs                  []string
	EnableSampling             int
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
