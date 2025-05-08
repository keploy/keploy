package config

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
	"sigs.k8s.io/kustomize/kyaml/yaml"
	"sigs.k8s.io/kustomize/kyaml/yaml/merge2"
	"sigs.k8s.io/kustomize/kyaml/yaml/walk"
)

// defaultConfig is a variable to store the default configuration of the Keploy CLI. It is not a constant because enterprise need update the default configuration.
var defaultConfig = `
path: ""
appId: 0
appName: ""
command: ""
templatize:
  testSets: []
port: 0
proxyPort: 16789
dnsPort: 26789
debug: false
disableANSI: false
disableTele: false
generateGithubActions: false
containerName: ""
networkName: ""
buildDelay: 30s
test:
  selectedTests: {}
  ignoredTests: {}
  globalNoise:
    global: {}
    test-sets: {}
  delay: 5s
  host: ""
  port: 0
  apiTimeout: 5s
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
record:
  recordTimer: 0s
  filters: []
configPath: ""
bypassRules: []
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
`

func GetDefaultConfig() string {
	return defaultConfig
}

func SetDefaultConfig(cfgStr string) {
	defaultConfig = cfgStr
}

const InternalConfig = `
enableTesting: false
keployContainer: "keploy-v2"
keployNetwork: "keploy-network"
inDocker: false
cmdType: "native"
`

var config = &Config{}

func New() (*Config, error) {
	defaultCfg := &Config{
		BuildDelay: 30 * time.Second,
		Record:     Record{},
		Test: Test{
			Delay:      5 * time.Second,
			APITimeout: 5 * time.Second,
		},
	}

	v := viper.New()
	v.SetConfigType("yaml")

	if err := v.MergeConfigMap(map[string]any{
		"record": map[string]any{},
		"test": map[string]any{
			"delay":      defaultCfg.Test.Delay.String(),
			"apiTimeout": defaultCfg.Test.APITimeout.String(),
		},
		"buildDelay": defaultCfg.BuildDelay.String(),
	}); err != nil {
		return nil, fmt.Errorf("failed to merge default config map: %w", err)
	}

	var finalCfg Config
	if err := v.Unmarshal(&finalCfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	return &finalCfg, nil
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
