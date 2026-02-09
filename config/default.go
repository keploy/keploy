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
  delay: 5
  host: ""
  port: 0
  grpcPort: 0
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
  disableMockUpload: true
  useLocalMock: false
  updateTemplate: false
  mustPass: false
  maxFailAttempts: 5
  maxFlakyChecks: 1
  protoFile: ""
  protoDir: ""
  protoInclude: []
  compareAll: false
record:
  recordTimer: 0s
  filters: []
  sync: false
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

var config = &Config{}

func New() *Config {
	// merge default config with internal config
	mergedConfig, err := Merge(defaultConfig, InternalConfig)
	if err != nil {
		panic(err)
	}
	err = yaml3.Unmarshal([]byte(mergedConfig), config)
	if err != nil {
		panic(err)
	}
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
