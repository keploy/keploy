package config

import (
    yaml3 "gopkg.in/yaml.v3"
    "sigs.k8s.io/kustomize/kyaml/yaml"
    "sigs.k8s.io/kustomize/kyaml/yaml/merge2"
    "sigs.k8s.io/kustomize/kyaml/yaml/walk"
)

var defaultConfig = `
path: ""
appId: 0
appName: ""
command: ""
port: 0
proxyPort: 16789
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
  apiTimeout: 5
  coverage: false
  goCoverage: false
  coverageReportPath: ""
  ignoreOrdering: true
  mongoPassword: "default@123"
  language: ""
  removeUnusedMocks: false
  basePath: ""
  mocking: true
  disableLineCoverage: false
  fallbackOnMiss: false
  disableMockUpload: true
record:
  recordTimer: 0s
  filters: []
contract:
  driven: "consumer"
  servicesMapping: {}
  self: "s1"
configPath: ""
bypassRules: []
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

func New() *Config {
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
