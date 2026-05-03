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
  healthPollTimeout: 60s
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
  mongoPassword: "default@123"
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
configPath: ""
bypassRules: []
disableMapping: true
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
