// Package config provides configuration structures for the application.
package config

import (
	"fmt"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

type Config struct {
	Path              string              `json:"path" yaml:"path" mapstructure:"path"`
	StorageFormat     string              `json:"storageFormat" yaml:"storageFormat" mapstructure:"storageFormat"` // serialization format for testcases/mocks/reports: "yaml" (default) or "json"
	AppName           string              `json:"appName" yaml:"appName" mapstructure:"appName"`
	AppID             uint64              `json:"appId" yaml:"appId" mapstructure:"appId"` // deprecated field
	Command           string              `json:"command" yaml:"command" mapstructure:"command"`
	Templatize        Templatize          `json:"templatize" yaml:"templatize" mapstructure:"templatize"`
	Port              uint32              `json:"port" yaml:"port" mapstructure:"port"`
	E2E               bool                `json:"e2e" yaml:"e2e" mapstructure:"e2e"`
	DNSPort           uint32              `json:"dnsPort" yaml:"dnsPort" mapstructure:"dnsPort"`
	ProxyPort         uint32              `json:"proxyPort" yaml:"proxyPort" mapstructure:"proxyPort"`
	IncomingProxyPort uint16              `json:"incomingProxyPort" yaml:"incomingProxyPort" mapstructure:"incomingProxyPort"`
	Debug             bool                `json:"debug" yaml:"debug" mapstructure:"debug"`
	DisableTele       bool                `json:"disableTele" yaml:"disableTele" mapstructure:"disableTele"`
	DisableANSI       bool                `json:"disableANSI" yaml:"disableANSI" mapstructure:"disableANSI"`
	JSONOutput        bool                `json:"jsonOutput" yaml:"jsonOutput" mapstructure:"jsonOutput"`
	InDocker          bool                `json:"inDocker" yaml:"-" mapstructure:"inDocker"`
	ContainerName     string              `json:"containerName" yaml:"containerName" mapstructure:"containerName"`
	NetworkName       string              `json:"networkName" yaml:"networkName" mapstructure:"networkName"`
	BuildDelay        uint64              `json:"buildDelay" yaml:"buildDelay" mapstructure:"buildDelay"`
	Test              Test                `json:"test" yaml:"test" mapstructure:"test"`
	Record            Record              `json:"record" yaml:"record" mapstructure:"record"`
	Report            Report              `json:"report" yaml:"report" mapstructure:"report"`
	Normalize         Normalize           `json:"normalize" yaml:"-" mapstructure:"normalize"`
	DisableMapping    bool                `json:"disableMapping" yaml:"disableMapping" mapstructure:"disableMapping"`
	RetryPassing      bool                `json:"retryPassing" yaml:"retryPassing" mapstructure:"retryPassing"`
	ConfigPath        string              `json:"configPath" yaml:"configPath" mapstructure:"configPath"`
	BypassRules       []models.BypassRule `json:"bypassRules" yaml:"bypassRules" mapstructure:"bypassRules"`
	// MysqlPorts pins extra destination ports to the MySQL parser,
	// skipping auto-detection for them. Rarely needed now that ports are
	// detected automatically (see DisableMysqlAutoDetect); keep it for
	// deployments that want the ~250ms first-connection probe skipped,
	// or that disable detection entirely. Built-in defaults: 3306, 4000.
	MysqlPorts []uint32 `json:"mysqlPorts" yaml:"mysqlPorts" mapstructure:"mysqlPorts"`
	// DisableMysqlAutoDetect turns off automatic MySQL port detection.
	// With detection on (the default), keploy identifies MySQL on any
	// port by reading the server's handshake during record and by
	// recalling the port from recorded mocks during replay. Turn it off
	// to restore the strict port-list behaviour — then MySQL on a port
	// outside MysqlPorts will hang its handshake.
	DisableMysqlAutoDetect bool `json:"disableMysqlAutoDetect" yaml:"disableMysqlAutoDetect" mapstructure:"disableMysqlAutoDetect"`
	// DisableMysqlEndpointDrift stops replay from serving recorded MySQL
	// mocks on a port the recording never saw. Detection still runs; only
	// the inference is turned off.
	//
	// Leave it on (the default) unless a dependency is being misread. The
	// inference fires when a replayed app opens a connection to an unknown
	// port and says nothing, which is the MySQL signature — but a client that
	// stays silent for the whole confirmation window and only then speaks
	// looks the same, and it will be answered with a handshake it did not ask
	// for. Pinning the real mapping with MysqlPorts is the better fix when you
	// know it; this is the escape hatch when you do not, and unlike
	// DisableMysqlAutoDetect it keeps record-time detection working.
	DisableMysqlEndpointDrift bool     `json:"disableMysqlEndpointDrift" yaml:"disableMysqlEndpointDrift" mapstructure:"disableMysqlEndpointDrift"`
	EnableTesting             bool     `json:"enableTesting" yaml:"-" mapstructure:"enableTesting"`
	GenerateGithubActions     bool     `json:"generateGithubActions" yaml:"generateGithubActions" mapstructure:"generateGithubActions"`
	KeployContainer           string   `json:"keployContainer" yaml:"keployContainer" mapstructure:"keployContainer"`
	KeployNetwork             string   `json:"keployNetwork" yaml:"keployNetwork" mapstructure:"keployNetwork"`
	CommandType               string   `json:"cmdType" yaml:"cmdType" mapstructure:"cmdType"`
	Contract                  Contract `json:"contract" yaml:"contract" mapstructure:"contract"`
	Mock                      MockCmd  `json:"mock" yaml:"mock" mapstructure:"mock"`
	Agent                     Agent    `json:"agent" yaml:"agent" mapstructure:"agent"`
	Async                     Async    `json:"async" yaml:"async" mapstructure:"async"`
	InCi                      bool     `json:"inCi" yaml:"inCi" mapstructure:"inCi"`
	InstallationID            string   `json:"-" yaml:"-" mapstructure:"-"`
	ServerPort                uint32   `json:"serverPort" yaml:"serverPort" mapstructure:"serverPort"`
	Version                   string   `json:"-" yaml:"-" mapstructure:"-"`
	APIServerURL              string   `json:"-" yaml:"-" mapstructure:"-"`
	GitHubClientID            string   `json:"-" yaml:"-" mapstructure:"-"`
	// InMemoryCompose holds docker-compose YAML content in memory to avoid writing
	// sensitive environment variables (secrets, tokens) to disk. When set, the
	// compose command uses "-f -" and pipes this content via stdin.
	InMemoryCompose []byte `json:"-" yaml:"-" mapstructure:"-"`
}

type Agent struct {
	models.SetupOptions
	// UpstreamTLSVerifySet / UpstreamTLSCACertSet record that the
	// corresponding --upstream-tls-* flag was PRESENT on this agent's argv,
	// as opposed to merely sitting at its zero value.
	//
	// They exist because a bare bool cannot express the difference between
	// "the orchestrator said false" and "the orchestrator said nothing", and
	// that difference is the entire off switch: the orchestrator forwards its
	// resolved value unconditionally as --upstream-tls-verify=%t, and a native
	// agent ALSO reads the very same keploy.yml through --config-path. Without
	// the marker the agent has to guess, and the only safe-looking guess (OR
	// the two) makes `--upstream-tls-verify=false` a no-op on native while it
	// works under docker. See proxy.resolveUpstreamTLSConfig.
	//
	// Not user configuration — set by the CLI flag parser, never read from
	// keploy.yml, hence the "-" tags.
	UpstreamTLSVerifySet bool `json:"-" yaml:"-" mapstructure:"-"`
	UpstreamTLSCACertSet bool `json:"-" yaml:"-" mapstructure:"-"`
}

// Async configures the async-egress engine. Empty Lanes => feature off,
// record & replay byte-identical to today.
type Async struct {
	Lanes []models.AsyncLane `json:"lanes" yaml:"lanes" mapstructure:"lanes"`
}

type Templatize struct {
	TestSets []string `json:"testSets" yaml:"testSets" mapstructure:"testSets"`
}

type Record struct {
	Filters     []models.Filter `json:"filters" yaml:"filters" mapstructure:"filters"`
	BasePath    string          `json:"basePath" yaml:"basePath" mapstructure:"basePath"`
	RecordTimer time.Duration   `json:"recordTimer" yaml:"recordTimer" mapstructure:"recordTimer"`
	Metadata    string          `json:"metadata" yaml:"metadata" mapstructure:"metadata"`
	// TestCaseNaming controls how default test case filenames are generated.
	// "descriptive" (default) derives a slug from the HTTP method+path or gRPC service/method.
	// "sequential" preserves the legacy `test-N.yaml` numbering.
	TestCaseNaming string `json:"testCaseNaming" yaml:"testCaseNaming" mapstructure:"testCaseNaming"`
	Synchronous    bool   `json:"sync" yaml:"sync" mapstructure:"sync"`
	EnableSampling int    `json:"enableSampling" yaml:"enableSampling"`
	// ChannelBindingShim enables the SCRAM-SHA-256-PLUS channel-binding shim.
	// The shim attaches eBPF uprobes to libcrypto's X509_digest and rewrites the
	// cert-hash libpq folds into the SCRAM proof, so postgres clients running
	// with channel_binding=require still authenticate through keploy's TLS MITM
	// against the REAL upstream postgres at record time. Replay does not forward
	// to the real database — postgres traffic is served from mocks, no SCRAM
	// handshake actually completes against postgres — so the shim is record-only.
	// OSS builds have no implementation registered and ignore this flag entirely;
	// builds with a registered factory respect it. Defaults to false; flip to
	// true in keploy.yml under record: to opt in. Requires CAP_BPF + a kernel
	// that allows bpf_probe_write_user; without those the factory returns an
	// error and the proxy keeps working for non-PLUS clients.
	ChannelBindingShim bool   `json:"channelBindingShim" yaml:"channelBindingShim" mapstructure:"channelBindingShim"`
	MemoryLimit        uint64 `json:"memoryLimit" yaml:"memoryLimit" mapstructure:"memoryLimit"`
	GlobalPassthrough  bool   `json:"globalPassthrough" yaml:"globalPassthrough" mapstructure:"globalPassthrough"`
	TLSPrivateKeyPath  string `json:"tlsPrivateKeyPath" yaml:"tlsPrivateKeyPath" mapstructure:"tlsPrivateKeyPath"`
	// UpstreamTLS controls whether keploy authenticates the REAL upstream
	// server when it dials out on the application's behalf. TLSPrivateKeyPath
	// above is the client half of the same story (upstream mTLS); this is the
	// root half. See the UpstreamTLS type for why it is off by default.
	UpstreamTLS UpstreamTLS `json:"upstreamTls" yaml:"upstreamTls" mapstructure:"upstreamTls"`
	// MockFormat selects the on-disk format for recorded mocks.
	// "" or "yaml" (default) writes mocks.yaml — human-readable, the
	// format all tooling expects. "gob" writes a binary mocks.gob — a
	// ~28% CPU reduction on the record client at high throughput, at
	// the cost of not being grep/diff-friendly and having no cross-
	// Go-version stability contract. The env var KEPLOY_MOCK_FORMAT
	// takes precedence over this field for ad-hoc experimentation.
	MockFormat string `json:"mockFormat,omitempty" yaml:"mockFormat,omitempty" mapstructure:"mockFormat"`
	// CapturePackets toggles raw network packet capture on the proxy ports
	// during recording. When enabled, a pcap file is written into the
	// freshly-created test-set directory under the keploy folder.
	CapturePackets bool `json:"capturePackets" yaml:"capturePackets" mapstructure:"capturePackets"`
	// OpportunisticTLSIntercept enables a "sniff first, hijack if TLS"
	// passthrough mode. The proxy lets the app and upstream talk to
	// each other (relaying bytes verbatim, like GlobalPassthrough)
	// while peeking each chunk for the start of a TLS handshake.
	// As soon as a chunk on the client side starts with a TLS
	// ClientHello, the proxy hijacks: it terminates TLS with the
	// client (presenting keploy's MITM cert, with KeyLogWriter
	// wired so the keylog file populates), opens a fresh tls.Client
	// to the upstream, and then relays cleartext both ways without
	// parser dispatch or mock recording. This gives a decryptable
	// pcap for sessions that would otherwise have been pure
	// passthrough — handy for debugging captured TLS traffic.
	//
	// Independent of GlobalPassthrough — the two flags are
	// alternatives, not a hierarchy. When OpportunisticTLSIntercept
	// is set it takes precedence; the proxy ignores
	// GlobalPassthrough for that connection's outcome.
	//
	// Caveats: cert pinning breaks the same way it does for default
	// record mode (the app must trust keploy's CA); SCRAM-*-PLUS
	// and other channel-binding mechanisms reject the MITM cert and
	// must be disabled client-side; MITM-incompatible workloads
	// should stick with GlobalPassthrough.
	OpportunisticTLSIntercept bool `json:"opportunisticTlsIntercept" yaml:"opportunisticTlsIntercept" mapstructure:"opportunisticTlsIntercept"`

	// RecordBuffer tunes the per-connection record buffer. Defaults
	// suit ~99% of workloads; only touch these if you see "mock
	// incomplete" warnings with reason "per_conn_cap" in the agent logs.
	RecordBuffer RecordBuffer `json:"recordBuffer" yaml:"recordBuffer" mapstructure:"recordBuffer"`

	// PassThroughPorts / PassThroughHosts configure telemetry-egress passthrough:
	// destinations keploy should not record as normal dependencies. Each rule is
	// {port|host, mode} where mode is "skip" (never record; synthesize success on
	// replay) or "recordOne" (record exactly one exchange per host/port/path/method
	// and serve it body-agnostically for every matching call on replay). Hosts are
	// required to catch TLS-encrypted telemetry whose port isn't observable at
	// capture. Built-in telemetry defaults (OTLP /v1/traces, Pyroscope /ingest,
	// Azure App Insights host) are merged in unless overridden. See models.PassThroughRule.
	PassThroughPorts []models.PassThroughRule `json:"passThroughPorts,omitempty" yaml:"passThroughPorts,omitempty" mapstructure:"passThroughPorts"`
	PassThroughHosts []models.PassThroughRule `json:"passThroughHosts,omitempty" yaml:"passThroughHosts,omitempty" mapstructure:"passThroughHosts"`
}

// UpstreamTLS configures the upstream (destination-side) leg of keploy's TLS
// MITM during record. Being a MITM constrains only the CLIENT-facing leg — the
// cert keploy presents to the app is minted by keploy's own CA. The upstream
// leg is an ordinary Go TLS client, so verifying the real server costs nothing
// extra: crypto/tls uses the host's system root pool when RootCAs is nil, and
// the agent additionally embeds the Mozilla NSS roots
// (pkg/agent/proxy/tls/data/mozilla_roots.pem) for images with no trust store.
//
// Verify nevertheless defaults to FALSE, and the reason is fidelity, not a
// missing CA bundle: a recording proxy must never be stricter than the
// application it records. An app connecting with `sslmode=require` (postgres)
// or `tls=skip-verify` (go-sql-driver/mysql) has deliberately chosen to encrypt
// without authenticating its upstream. If keploy authenticated on its behalf it
// would refuse connections the app would happily have made — and the failure is
// silent rather than loud: on a dest-side handshake error the supervisor falls
// through to raw passthrough, so the application keeps working while the mock
// is DROPPED. The user sees a healthy app and a mysteriously empty mocks.yaml.
// Self-signed upstreams and Kubernetes ClusterIP destinations whose cert SAN
// does not match the address keploy dials both land in exactly that hole.
//
// Turn Verify on when the recording itself is security-relevant — e.g. recording
// against public APIs in a regulated environment, where an on-path attacker at
// record time could poison a mock set that later gates CI.
type UpstreamTLS struct {
	// Verify turns on certificate verification for keploy's own outbound TLS
	// dials. False (the default) preserves today's behaviour exactly.
	Verify bool `json:"verify" yaml:"verify" mapstructure:"verify"`
	// CACert is an optional path to a PEM file of extra trust anchors, appended
	// to the system pool (or, on an image with no trust store, to keploy's
	// embedded Mozilla NSS roots). Use it for private/internal CAs instead of
	// installing them into the agent's OS trust store. Only consulted when
	// Verify is true. The path is resolved on the AGENT's filesystem, which for
	// docker/k8s runs is not the host's — bind-mount the file or pass a path
	// that exists inside the agent container.
	CACert string `json:"caCert" yaml:"caCert" mapstructure:"caCert"`
}

// RecordBuffer tunes the per-connection recording queue used by the
// agent's relay.
//
// MaxMemoryPerConnection is the one that bounds recording: the queue is
// bounded by BYTES, and exceeding that budget is the only condition under
// which a chunk is refused (reason "per_conn_cap"), marking the in-flight
// mock incomplete. The forward path is unaffected — user traffic always
// succeeds.
//
// QueueSize sizes the hand-off channel between the recorder and the parser.
// It no longer bounds the recording queue itself, so it is not the knob to
// reach for when mocks come back incomplete; raising MaxMemoryPerConnection
// is. It was previously a slot count on an internal staging channel, and
// running out of slots produced a "channel_full" drop — that failure mode no
// longer exists, because bounding by slots discarded bursts of many small
// chunks that used almost no memory (the boot-time "no mocks" loss).
//
// ConsumerStallGrace bounds teardown, not steady state: it is how long a
// closing connection waits on a parser that has stopped draining before
// giving up on the chunks still queued for it.
//
// Env vars KEPLOY_RECORD_MAX_MEMORY_PER_CONN, KEPLOY_RECORD_QUEUE_SIZE and
// KEPLOY_RECORD_CONSUMER_STALL_GRACE override the yaml/flag values when set.
type RecordBuffer struct {
	// MaxMemoryPerConnection caps the bytes the recorder may hold
	// in the per-connection queue while the parser catches up.
	// Maps to relay.Config.PerConnCap. Zero resolves to the relay's
	// built-in default (64 MiB). Increase if you see drops with
	// reason "per_conn_cap" — usually means responses are larger
	// than the default budget (e.g. >10 MB query results).
	MaxMemoryPerConnection uint64 `json:"maxMemoryPerConnection" yaml:"maxMemoryPerConnection" mapstructure:"maxMemoryPerConnection"`

	// QueueSize is the number of chunk slots in the hand-off channel
	// between the recorder and the parser. Each slot holds one ~32 KiB
	// chunk. Maps to relay.Config.TeeChanBuf. Zero resolves to the
	// relay's built-in default (1024).
	//
	// This does NOT bound how much the recorder may buffer — that is
	// MaxMemoryPerConnection — so raising it will not stop
	// "per_conn_cap" drops.
	QueueSize int `json:"queueSize" yaml:"queueSize" mapstructure:"queueSize"`

	// ConsumerStallGrace bounds how long the recorder waits on a parser
	// that has stopped draining before abandoning the chunks still queued
	// for it. Maps to relay.Config.ConsumerStallGrace. Zero resolves to
	// the relay's built-in default (2s).
	//
	// It bounds STALLED time, not elapsed time: the wait ends the moment
	// the parser takes anything at all, so a merely slow parser still
	// receives every chunk. The bound is consulted only after the
	// connection closes, so it costs nothing on a healthy connection.
	//
	// Raise it if a teardown-time parser is slow enough to look dead and
	// you see drops with reason "consumer_gone"; lower it to cap how long
	// a connection with a genuinely dead parser lingers at teardown.
	ConsumerStallGrace time.Duration `json:"consumerStallGrace" yaml:"consumerStallGrace" mapstructure:"consumerStallGrace"`
}

// MockCmd configures the `keploy mock record|replay` flow — using Keploy as a
// framework-agnostic mocking layer for a user's own test runner (pytest, go
// test, jest/playwright, mobile UI tests). Unlike record/test it captures ONLY
// outgoing dependency calls (no incoming test cases) into a single named mock
// set, and on replay serves that set back to the wrapped runner.
type MockCmd struct {
	// Name is the mock set to record into / replay from (the on-disk directory
	// under keploy/, and the registry key). Defaults to "default". Re-recording
	// the same name overwrites its mocks in place so a CI "re-record on merge to
	// main" job produces a reviewable diff rather than an accumulating pile of
	// test-set-N directories.
	Name string `json:"name" yaml:"name" mapstructure:"name"`
	// OnMiss is the replay-time miss policy: "fail" (default — deterministic,
	// a miss is a hard error), "passthrough" (dial the real dependency, don't
	// persist), or "record" (dial the real dependency AND append the new call
	// to the set — VCR-style new_episodes for incremental refresh).
	OnMiss string `json:"onMiss" yaml:"onMiss" mapstructure:"onMiss"`
	// Strict makes replay exit non-zero if any recorded mock was missed, so a
	// drifted dependency contract fails the build even when the runner passed.
	Strict bool `json:"strict" yaml:"strict" mapstructure:"strict"`
	// Local forces the file-backed store even when a cloud registry is
	// configured (enterprise). Registry-first by default; --local opts out.
	Local bool `json:"local" yaml:"local" mapstructure:"local"`
	// RecordTimer optionally bounds a record session (e.g. "30s"); the wrapped
	// runner exiting on its own ends recording first in almost all cases.
	RecordTimer time.Duration `json:"recordTimer" yaml:"recordTimer" mapstructure:"recordTimer"`
}

type Contract struct {
	Services []string `json:"services" yaml:"services" mapstructure:"services"`
	Tests    []string `json:"tests" yaml:"tests" mapstructure:"tests"`
	Path     string   `json:"path" yaml:"path" mapstructure:"path"`
	Download bool     `json:"download" yaml:"download" mapstructure:"download"`
	Generate bool     `json:"generate" yaml:"generate" mapstructure:"generate"`
	Driven   string   `json:"driven" yaml:"driven" mapstructure:"driven"`
	Mappings Mappings `json:"mappings" yaml:"mappings" mapstructure:"mappings"`
}

type Mappings struct {
	ServicesMapping map[string][]string `json:"servicesMapping" yaml:"servicesMapping" mapstructure:"servicesMapping"`
	Self            string              `json:"self" yaml:"self" mapstructure:"self"`
}

type Normalize struct {
	SelectedTests []SelectedTests `json:"selectedTests" yaml:"selectedTests" mapstructure:"selectedTests"`
	TestRun       string          `json:"testReport" yaml:"testReport" mapstructure:"testReport"`
	AllowHighRisk bool            `json:"allowHighRisk" yaml:"allowHighRisk" mapstructure:"allowHighRisk"`
	EditedBy      string          `json:"-" yaml:"-" mapstructure:"-"`
}

type Test struct {
	SelectedTests               map[string][]string `json:"selectedTests" yaml:"selectedTests" mapstructure:"selectedTests"`
	GlobalNoise                 Globalnoise         `json:"globalNoise" yaml:"globalNoise" mapstructure:"globalNoise"`
	ReplaceWith                 ReplaceWith         `json:"replaceWith" yaml:"replaceWith" mapstructure:"replaceWith"`
	Delay                       uint64              `json:"delay" yaml:"delay" mapstructure:"delay"`
	HealthURL                   string              `json:"healthUrl" yaml:"healthUrl" mapstructure:"healthUrl"`                         // optional HTTP(S) URL polled before firing the first test; empty preserves the fixed --delay behavior
	HealthPollTimeout           time.Duration       `json:"healthPollTimeout" yaml:"healthPollTimeout" mapstructure:"healthPollTimeout"` // ceiling for the pre-test health poll loop before falling back to --delay
	AppReadyProbeAddr           string              `json:"appReadyProbeAddr" yaml:"appReadyProbeAddr" mapstructure:"appReadyProbeAddr"` // optional host:port TCP-polled after the --delay floor (bounded by healthPollTimeout) before firing the first test — the TCP-accept analog of healthUrl for apps with no HTTP health endpoint (e.g. a k8s replay pod's app Service, or a native app on a fixed port). Empty preserves the fixed --delay behavior. Unlike test.port it NEVER affects request routing; it is only a readiness probe.
	Host                        string              `json:"host" yaml:"host" mapstructure:"host"`
	Port                        uint32              `json:"port" yaml:"port" mapstructure:"port"`
	GRPCPort                    uint32              `json:"grpcPort" yaml:"grpcPort" mapstructure:"grpcPort"`
	SSEPort                     uint32              `json:"ssePort" yaml:"ssePort" mapstructure:"ssePort"`
	Protocol                    ProtocolConfig      `json:"protocol" yaml:"protocol" mapstructure:"protocol"`
	APITimeout                  uint64              `json:"apiTimeout" yaml:"apiTimeout" mapstructure:"apiTimeout"`
	SkipCoverage                bool                `json:"skipCoverage" yaml:"skipCoverage" mapstructure:"skipCoverage"`                   // boolean to capture the coverage in test
	CoverageReportPath          string              `json:"coverageReportPath" yaml:"coverageReportPath" mapstructure:"coverageReportPath"` // directory path to store the coverage files
	IgnoreOrdering              bool                `json:"ignoreOrdering" yaml:"ignoreOrdering" mapstructure:"ignoreOrdering"`
	MongoPassword               string              `json:"mongoPassword" yaml:"mongoPassword" mapstructure:"mongoPassword"`
	Language                    models.Language     `json:"language" yaml:"language" mapstructure:"language"`
	RemoveUnusedMocks           bool                `json:"removeUnusedMocks" yaml:"removeUnusedMocks" mapstructure:"removeUnusedMocks"`
	PreserveFailedMocks         bool                `json:"preserveFailedMocks" yaml:"preserveFailedMocks" mapstructure:"preserveFailedMocks"` // skip mock pruning when tests fail (set by k8s-proxy autoreplay)
	FallBackOnMiss              bool                `json:"fallBackOnMiss" yaml:"fallBackOnMiss" mapstructure:"fallBackOnMiss"`                // Deprecated: this flag is ignored. Replay is now always deterministic.
	JacocoAgentPath             string              `json:"jacocoAgentPath" yaml:"jacocoAgentPath" mapstructure:"jacocoAgentPath"`
	BasePath                    string              `json:"basePath" yaml:"basePath" mapstructure:"basePath"`
	Mocking                     bool                `json:"mocking" yaml:"mocking" mapstructure:"mocking"`
	IgnoredTests                map[string][]string `json:"ignoredTests" yaml:"ignoredTests" mapstructure:"ignoredTests"`
	DisableLineCoverage         bool                `json:"disableLineCoverage" yaml:"disableLineCoverage" mapstructure:"disableLineCoverage"`
	UpdateTemplate              bool                `json:"updateTemplate" yaml:"updateTemplate" mapstructure:"updateTemplate"`
	MustPass                    bool                `json:"mustPass" yaml:"mustPass" mapstructure:"mustPass"`
	MaxFailAttempts             uint32              `json:"maxFailAttempts" yaml:"maxFailAttempts" mapstructure:"maxFailAttempts"`
	MaxFlakyChecks              uint32              `json:"maxFlakyChecks" yaml:"maxFlakyChecks" mapstructure:"maxFlakyChecks"`
	ProtoFile                   string              `json:"protoFile" yaml:"protoFile" mapstructure:"protoFile"`
	ProtoDir                    string              `json:"protoDir" yaml:"protoDir" mapstructure:"protoDir"`
	ProtoInclude                []string            `json:"protoInclude" yaml:"protoInclude" mapstructure:"protoInclude"`
	CompareAll                  bool                `json:"compareAll" yaml:"compareAll" mapstructure:"compareAll"`
	SchemaMatch                 bool                `json:"schemaMatch" yaml:"schemaMatch" mapstructure:"schemaMatch"`
	UpdateTestMapping           bool                `json:"updateTestMapping" yaml:"updateTestMapping" mapstructure:"updateTestMapping"`
	DisableAutoHeaderNoise      bool                `json:"disableAutoHeaderNoise" yaml:"disableAutoHeaderNoise" mapstructure:"disableAutoHeaderNoise"`                                    // skip auto-noise for flaky headers (e.g. AWS SigV4)
	SchemaNoiseDetection        bool                `json:"schemaNoiseDetection" yaml:"schemaNoiseDetection" mapstructure:"schemaNoiseDetection"`                                          // detect request-body fields that drift between recording and replay and persist them as field-path noise (req_body_noise) during auto-replay matching; available to any parser implementing the shared schema-noise adapter
	SchemaNoiseStrict           bool                `json:"schemaNoiseStrict" yaml:"schemaNoiseStrict" mapstructure:"schemaNoiseStrict"`                                                   // replay-path enforcement: for a mock that already carries learned req_body_noise, match strictly — every request-body field must match except the learned-noise paths, so a non-noise drift fails the match. Left false on the auto-replay path so it can still learn noise leniently. Available to any parser implementing the shared schema-noise adapter.
	StrictFailure               bool                `json:"strictFailure" yaml:"strictFailure" mapstructure:"strictFailure"`                                                               // when true, a response-failing test (testPass=false) is marked FAILED even if the consumed mock set diverged from the recorded mapping. Default false preserves the historical demotion: response failures with mock-set mismatch are marked OBSOLETE so the user can re-record without seeing the response diff as a hard failure. Set true to surface every response divergence as a real test failure for CI gating; the per-test OBSOLETE label is replaced by FAILED but the mappingDiff (expected vs actual mocks, missing calls) is still written to the report for diagnostics.
	StrictMockWindow            bool                `json:"strictMockWindow" yaml:"strictMockWindow" mapstructure:"strictMockWindow"`                                                      // Strict containment: per-test (LifetimePerTest) mocks whose request timestamp falls outside the outer test window are DROPPED rather than promoted to the cross-test unfiltered pool, which eliminates cross-test mock bleed. Default TRUE now that every stateful-protocol recorder classifies mocks finely enough (session vs per-test for connection-alive commands, per-connection data mocks) that legitimate cross-test sharing is encoded as session/connection lifetime rather than implicit out-of-window reuse. Opt out by setting this to false in keploy.yaml, or export KEPLOY_STRICT_MOCK_WINDOW=0 at process start — the env var wins over config.
	KeepAppAlive                bool                `json:"keepAppAlive" yaml:"keepAppAlive" mapstructure:"keepAppAlive"`                                                                  // Start the user app ONCE on the outer errgroup at Start() time instead of restarting it per test-set. Skips the per-test-set RunApplication spawn + NotifyGracefulShutdown (reuses the existing serveTest gating) and skips the --delay wait on every test-set after the first (the app is already warm after the boundary). Matches the production globality autoreplay shape where a single user-app process serves every test-set back-to-back; required for cross-test-set bugs that need a long-lived TCP connection (asyncpg pool, JDBC HikariCP pool, etc.) to surface — see keploy/integrations#203 for the session-tier staleness case. Works for every cmdType that manages a user application (docker-compose, docker-run, docker-start, native); cmdType == Empty (no -c) short-circuits the one-shot spawn since there's nothing to manage. Default FALSE preserves the historical per-test-set restart behaviour.
	ConnectionPoolIdleRetention time.Duration       `json:"connectionPoolIdleRetention,omitempty" yaml:"connectionPoolIdleRetention,omitempty" mapstructure:"connectionPoolIdleRetention"` // How long a per-connID connection-scoped mock pool survives without activity before the idle sweeper reclaims it. Default 5m — enough for HikariCP-style pooled connections bridging test boundaries without activity. Extend for long-running integration tests that may idle a connection between requests for more than 5 minutes; shorter values make the sweeper more aggressive at cost of potentially reclaiming active connections. Zero / negative reverts to the default.
	CmdUsed                     string              `json:"-" yaml:"-" mapstructure:"-"`                                                                                                   // Full command used for the test run (set at runtime)
	RunLastTestSets             []string            `json:"-" yaml:"-" mapstructure:"-"`
}

type Report struct {
	SelectedTestSets map[string][]string `json:"selectedTestSets" yaml:"selectedTestSets" mapstructure:"selectedTestSets"`
	ShowFullBody     bool                `json:"showFullBody" yaml:"showFullBody" mapstructure:"showFullBody"`
	ReportPath       string              `json:"reportPath" yaml:"reportPath" mapstructure:"reportPath"`
	Summary          bool                `json:"summary" yaml:"summary" mapstructure:"summary"`
	TestCaseIDs      []string            `json:"testCaseIDs" yaml:"testCaseIDs" mapstructure:"testCaseIDs"`
	Format           string              `json:"format" yaml:"format" mapstructure:"format"`
}

type Globalnoise struct {
	Global   GlobalNoise  `json:"global" yaml:"global" mapstructure:"global"`
	Testsets TestsetNoise `json:"test-sets" yaml:"test-sets" mapstructure:"test-sets"`
}

type ReplaceWith struct {
	Global   ReplaceWithMap            `json:"global" yaml:"global" mapstructure:"global"`
	TestSets map[string]ReplaceWithMap `json:"test-sets" yaml:"test-sets" mapstructure:"test-sets"`
}

type ReplaceWithMap struct {
	URL  map[string]string `json:"url" yaml:"url" mapstructure:"url"`
	Port map[uint32]uint32 `json:"port" yaml:"port" mapstructure:"port"`
}

// ProtocolSettings holds per-protocol configuration. Add new fields here
// to extend all protocols without changing the map structure.
type ProtocolSettings struct {
	Port uint32 `json:"port" yaml:"port" mapstructure:"port"`
}

// ProtocolConfig maps protocol names (e.g. "http", "sse", "grpc") to their
// settings. The map schema allows additional protocol names in the config
// without schema changes, but only protocols recognized by the application
// are currently used by the replay and protocol-handling logic.
type ProtocolConfig map[string]ProtocolSettings

type SelectedTests struct {
	TestSet string   `json:"testSet" yaml:"testSet" mapstructure:"testSet"`
	Tests   []string `json:"tests" yaml:"tests" mapstructure:"tests"`
}

type (
	Noise        map[string][]string
	GlobalNoise  map[string]map[string][]string
	TestsetNoise map[string]map[string]map[string][]string
)

func SetByPassPorts(conf *Config, ports []uint) {
	for _, port := range ports {
		conf.BypassRules = append(conf.BypassRules, models.BypassRule{
			Path: "",
			Host: "",
			Port: port,
		})
	}
}

func GetByPassPorts(conf *Config) []uint {
	var ports []uint
	for _, rule := range conf.BypassRules {
		ports = append(ports, rule.Port)
	}
	return ports
}

func SetSelectedTests(conf *Config, testSets []string) {
	conf.Test.SelectedTests = make(map[string][]string)
	for _, testSet := range testSets {
		conf.Test.SelectedTests[testSet] = []string{}
	}
}

func SetSelectedServices(conf *Config, services []string) {
	// string is "s1,s2" so i want to get s1,s2
	conf.Contract.Services = services
}

func SetSelectedContractTests(conf *Config, tests []string) {
	conf.Contract.Tests = tests
}

func SetSelectedTestSets(conf *Config, testSets []string) {
	conf.Report.SelectedTestSets = make(map[string][]string)
	for _, testSet := range testSets {
		conf.Report.SelectedTestSets[testSet] = []string{}
	}
}

func SetSelectedTestsNormalize(conf *Config, value string) error {
	value = strings.TrimSpace(value)

	// No tests provided -> clear selection and return
	if value == "" {
		conf.Normalize.SelectedTests = nil
		return nil
	}

	// Split only on commas: each token represents one test-set specification.
	// Examples:
	//   "ts1, ts2:tc1 tc2" =>
	//      "ts1"
	//      "ts2:tc1 tc2"
	parts := strings.Split(value, ",")

	var selected []SelectedTests

	for _, part := range parts {
		spec := strings.TrimSpace(part)
		if spec == "" {
			continue
		}

		// Check if this spec has an explicit list of test cases, e.g. "ts2:tc1 tc2"
		idx := strings.Index(spec, ":")

		if idx != -1 {
			testSetName := strings.TrimSpace(spec[:idx])
			if testSetName == "" {
				return fmt.Errorf("invalid format (missing test set name): %q", spec)
			}

			testsPart := strings.TrimSpace(spec[idx+1:])
			var testCases []string
			if testsPart != "" {
				for _, tc := range strings.Fields(testsPart) {
					tc = strings.TrimSpace(tc)
					if tc != "" {
						testCases = append(testCases, tc)
					}
				}
			}

			selected = append(selected, SelectedTests{
				TestSet: testSetName,
				// Empty testCases slice means "all tests" in that test set.
				Tests: testCases,
			})
			continue
		}

		// No colon -> entire token is just the test-set name, implies "all tests in this set"
		selected = append(selected, SelectedTests{
			TestSet: spec,
			Tests:   []string{}, // empty slice => all tests in this test set
		})
	}

	conf.Normalize.SelectedTests = selected
	return nil
}
