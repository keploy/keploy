package config

import (
	yaml3 "gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/kyaml/yaml"
	"sigs.k8s.io/kustomize/kyaml/yaml/merge2"
	"sigs.k8s.io/kustomize/kyaml/yaml/walk"
)

// DefaultConfig is stored as string because comments are important for the
// user to understand the usage of the config, and it's not possible to have
// comments in the struct.
// A little painful to maintain, but best for the user.
const DefaultConfig = `
record:
 path: ""
 # mandatory
 command: ""
 proxyport: 0
 containerName: ""
 networkName: ""
 delay: 5
 buildDelay: 30s
 tests:
   filters:
     - path: ""
       urlMethods: []
       headers: {}
       host: ""
 stubs:
   filters:
     - path: ""
       host: ""
       ports: 0
test:
 path: ""
 # mandatory
 command: ""
 proxyPort: 0
 containerName: ""
 networkName: ""
 # example: "test-set-1": ["test-1", "test-2", "test-3"]
 # if you want to run all the tests in the testset, use empty array
 # example: "test-set-1": []
 selectedTests:
 # to use globalNoise, please follow the guide at the end of this file.
 globalNoise:
   global:
     body: {}
     header: {}
 delay: 10
 buildDelay: 30s
 apiTimeout: 5
 ignoreOrdering: true
# used if you are using mongo SCRAM authentication
 mongoPassword: "default@123"
 stubs:
   filters:
     - path: ""
       host: ""
       ports: 0
 coverage: false
 coverageReportPath: ""
`

const InternalConfig = `
 keployContainer: "keploy-v2"
 keployNetwork: "keploy-network"
 configPath: "."
 test:
  # keploy unit test server port
  port: 6789
`

var config = &Config{}

func New() *Config {
	// merge default config with internal config
	mergedConfig, err := Merge(DefaultConfig, InternalConfig)
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
	return mergeStrings(srcStr, destStr, true, yaml.MergeOptions{})
}

// MergeStrings merges fields from src YAML into dest YAML.
// This copied from https://github.com/kubernetes-sigs/kustomize/blob/537c4fa5c2bf3292b273876f50c62ce1c81714d7/kyaml/yaml/merge2/merge2.go#L24
// The only change is the VisitKeysAsScalars is set to true to enable merging comments.
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
