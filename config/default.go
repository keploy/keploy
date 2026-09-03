package config

import (
	"fmt"

	"go.keploy.io/server/v3/pkg/models"
	yaml3 "gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/kyaml/yaml"
	"sigs.k8s.io/kustomize/kyaml/yaml/merge2"
	"sigs.k8s.io/kustomize/kyaml/yaml/walk"
)

// defaultConfig is a variable to store the default configuration of the Keploy CLI. It is not a constant because enterprise need update the default configuration.
var defaultConfig = fmt.Sprintf(`
path: ""
storageFormat: "yaml"
appId: 0
appName: ""
command: ""
templatize:
  testSets: []
port: 0
proxyPort: 16789
incomingProxyPort: %d
dnsPort: 26789
debug: false
disableANSI: false
disableTele: false
generateGithubActions: false
containerName: ""
networkName: ""
buildDelay: 30
test:
  selectedTests: {}
  ignoredTests: {}
  globalNoise:
    global: {}
    test-sets: {}
  replaceWith:
    global: {}
    test-sets: {}
  delay: 5
  healthUrl: ""
  healthPath: ""
  healthScheme: ""
  healthPollTimeout: 3m
  host: "localhost"
  port: 0
  grpcPort: 0
  ssePort: 0
  protocol:
    http:
      port: 0
    sse:
      port: 0
    grpc:
      port: 0
  apiTimeout: 5
  skipCoverage: false
  coverageReportPath: ""
  ignoreOrdering: true
  mongoPassword: ""
  language: ""
  removeUnusedMocks: false
  fallBackOnMiss: false
  jacocoAgentPath: ""
  basePath: ""
  mocking: true
  disableLineCoverage: false
  updateTemplate: false
  mustPass: false
  maxFailAttempts: 5
  maxFlakyChecks: 1
  protoFile: ""
  protoDir: ""
  protoInclude: []
  compareAll: false
  updateTestMapping: false
  disableAutoHeaderNoise: false
  # strictMockWindow enforces cross-test bleed prevention. Per-test
  # (LifetimePerTest) mocks whose request timestamp falls outside the
  # outer test window are dropped rather than promoted across tests.
  #
  # Default TRUE now that every stateful-protocol recorder classifies
  # mocks finely enough (per-connection data mocks, session vs per-test
  # distinction for connection-alive commands) that legitimate cross-
  # test sharing is encoded as session/connection lifetime rather than
  # implicit out-of-window reuse. If an older recording relies on the
  # legacy lax behaviour, opt out with strictMockWindow: false here or
  # export KEPLOY_STRICT_MOCK_WINDOW=0 — the env var wins.
  strictMockWindow: true
record:
  recordTimer: 0s
  filters: []
  sync: false
  memoryLimit: 0
  testCaseNaming: descriptive
  # upstreamTls controls whether keploy authenticates the REAL upstream
  # server when it dials out on your application's behalf. Being a TLS
  # MITM constrains only the app-facing leg; the upstream leg is an
  # ordinary Go TLS client, so verification needs no extra CA bundle.
  upstreamTls:
    # Off by default because keploy must never be stricter than the app
    # it records: an app using sslmode=require / tls=skip-verify chose
    # not to authenticate its upstream, and verifying on its behalf
    # would refuse connections the app would have made. The failure is
    # also silent — a dest-side handshake error falls through to raw
    # passthrough, so the app keeps working while the mock is dropped.
    # Turn this on when the recording itself is security-relevant.
    verify: false
    # Optional PEM file of extra trust anchors, appended to the system
    # pool when verify is true. Use it for private/internal CAs. The
    # path is resolved on the AGENT's filesystem, which for docker/k8s
    # runs is not the host's.
    caCert: ""
  # recordBuffer tunes the per-connection recording queue. Touch only
  # if you see "mock incomplete" warnings (reason: per_conn_cap) in
  # the agent logs. Env vars KEPLOY_RECORD_MAX_MEMORY_PER_CONN,
  # KEPLOY_RECORD_QUEUE_SIZE and KEPLOY_RECORD_CONSUMER_STALL_GRACE
  # override these values.
  recordBuffer:
    # Bytes. 67108864 = 64 MiB. Zero falls through to the built-in
    # default. Bump for workloads with large responses (e.g. >10 MB
    # query results).
    maxMemoryPerConnection: 67108864
    # Number of chunk slots (~32 KiB each) in the recorder-to-parser
    # hand-off channel. Zero falls through to the built-in default.
    # This does not bound how much the recorder buffers, so raising it
    # will not stop per_conn_cap drops — raise maxMemoryPerConnection.
    queueSize: 1024
    # How long a closing connection waits on a parser that has stopped
    # draining before abandoning the chunks still queued for it. Bounds
    # stalled time, not elapsed time, and is only consulted after close —
    # a healthy connection never pays it. Zero falls through to the
    # built-in default (2s).
    consumerStallGrace: 2s
    halfCloseGrace: 10s
async:
    lanes: []
configPath: ""
bypassRules: []
# MySQL is detected automatically on any port: at record time keploy
# reads the server's handshake to identify it, and at replay time it
# recalls the port from the recorded mocks. You normally do not need to
# configure anything here.
#
# mysqlPorts pins extra ports to the MySQL parser, skipping detection
# for them (the built-in list is 3306 and 4000). Useful only to avoid
# the ~250ms probe on the first connection to a port, or alongside
# disableMysqlAutoDetect.
mysqlPorts: []
# Set to true to turn detection off and go back to matching mysqlPorts
# strictly. MySQL on any other port will then hang its handshake.
disableMysqlAutoDetect: false
# During replay, keploy serves recorded MySQL mocks even when the app
# connects to a port the recording never saw — an app whose environment
# was rebuilt does not always get the endpoint it had while recording,
# and refusing would leave it waiting for a greeting only keploy can
# send. Set to true if a non-MySQL dependency is being misread as MySQL;
# unlike disableMysqlAutoDetect this keeps record-time detection on.
disableMysqlEndpointDrift: false
disableMapping: false
mock:
  name: "default"
  onMiss: "fail"
  strict: false
  local: false
  recordTimer: 0s
contract:
  driven: "consumer"
  mappings:
    servicesMapping: {}
    self: "s1"
  services: []
  tests: []
  path: ""
  download: false
  generate: false
inCi: false
`, models.DefaultIncomingProxyPort)

func GetDefaultConfig() string {
	return defaultConfig
}

func SetDefaultConfig(cfgStr string) {
	defaultConfig = cfgStr
}

const InternalConfig = `
enableTesting: false
keployContainer: "keploy-v3"
keployNetwork: "keploy-network"
inDocker: false
cmdType: "native"
`

func New() *Config {
	// merge default config with internal config
	mergedConfig, err := Merge(defaultConfig, InternalConfig)
	if err != nil {
		panic(err)
	}
	config := &Config{}
	err = yaml3.Unmarshal([]byte(mergedConfig), config)
	if err != nil {
		panic(err)
	}
	// Defaults for fields whose Go zero value is not the desired default.
	// EnableIPv6Redirect defaults to true so ::1 traffic is redirected to
	// the proxy on modern Linux distros where glibc resolves localhost to
	// ::1 first. Setting it false in config is the opt-in rollback knob.
	config.Agent.EnableIPv6Redirect = true
	return config
}

func Merge(srcStr, destStr string) (string, error) {
	return mergeStrings(srcStr, destStr, false, yaml.MergeOptions{})
}

// Reference: https://github.com/kubernetes-sigs/kustomize/blob/537c4fa5c2bf3292b273876f50c62ce1c81714d7/kyaml/yaml/merge2/merge2.go#L24
// VisitKeysAsScalars is set to true to enable merging comments.
// inferAssociativeLists is set to fasle to disable merging associative lists.
func mergeStrings(srcStr, destStr string, infer bool, mergeOptions yaml.MergeOptions) (string, error) {
	src, err := yaml.Parse(srcStr)
	if err != nil {
		return "", err
	}

	dest, err := yaml.Parse(destStr)
	if err != nil {
		return "", err
	}

	result, err := walk.Walker{
		Sources:               []*yaml.RNode{dest, src},
		Visitor:               merge2.Merger{},
		InferAssociativeLists: infer,
		VisitKeysAsScalars:    true,
		MergeOptions:          mergeOptions,
	}.Walk()
	if err != nil {
		return "", err
	}

	return result.String()
}
