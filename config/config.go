// Package config provides configuration structures for the application.
package config

import (
	"fmt"
	"strings"
	"time"
)

type Config struct {
	Path                  string       `json:"path" yaml:"path" mapstructure:"path"`
	AppID                 string       `json:"appId" yaml:"appId" mapstructure:"app-id"`
	Command               string       `json:"command" yaml:"command" mapstructure:"command"`
	Port                  uint32       `json:"port" yaml:"port" mapstructure:"port"`
	DNSPort               uint32       `json:"dnsPort" yaml:"dnsPort" mapstructure:"dns-port"`
	ProxyPort             uint32       `json:"proxyPort" yaml:"proxyPort" mapstructure:"proxy-port"`
	Debug                 bool         `json:"debug" yaml:"debug" mapstructure:"debug"`
	DisableTele           bool         `json:"disableTele" yaml:"disableTele" mapstructure:"disable-tele"`
	DisableANSI           bool         `json:"disableANSI" yaml:"disableANSI" mapstructure:"disable-ansi"`
	InDocker              bool         `json:"inDocker" yaml:"inDocker" mapstructure:"in_docker"`
	ContainerName         string       `json:"containerName" yaml:"containerName" mapstructure:"container-name"`
	NetworkName           string       `json:"networkName" yaml:"networkName" mapstructure:"network-name"`
	BuildDelay            uint64       `json:"buildDelay" yaml:"buildDelay" mapstructure:"build-delay"`
	Test                  Test         `json:"test" yaml:"test" mapstructure:"test"`
	Record                Record       `json:"record" yaml:"record" mapstructure:"record"`
	Gen                   UtGen        `json:"gen" yaml:"gen" mapstructure:"gen"`
	Normalize             Normalize    `json:"normalize" yaml:"normalize" mapstructure:"normalize"`
	ConfigPath            string       `json:"configPath" yaml:"configPath" mapstructure:"config-path"`
	BypassRules           []BypassRule `json:"bypassRules" yaml:"bypassRules" mapstructure:"bypassrules"`
	EnableTesting         bool         `json:"enableTesting" yaml:"enableTesting" mapstructure:"enable-testing"`
	GenerateGithubActions bool         `json:"generateGithubActions" yaml:"generateGithubActions" mapstructure:"generate-github-actions"`
	KeployContainer       string       `json:"keployContainer" yaml:"keployContainer" mapstructure:"keploy-container"`
	KeployNetwork         string       `json:"keployNetwork" yaml:"keployNetwork" mapstructure:"keploy-network"`
	CommandType           string       `json:"cmdType" yaml:"cmdType" mapstructure:"cmd-type"`
}

type UtGen struct {
	SourceFilePath     string  `json:"sourceFilePath" yaml:"sourceFilePath" mapstructure:"source-file-path"`
	TestFilePath       string  `json:"testFilePath" yaml:"testFilePath" mapstructure:"test-file-path"`
	CoverageReportPath string  `json:"coverageReportPath" yaml:"coverageReportPath" mapstructure:"coverage-report-path"`
	TestCommand        string  `json:"testCommand" yaml:"testCommand" mapstructure:"test-command"`
	CoverageFormat     string  `json:"coverageFormat" yaml:"coverageFormat" mapstructure:"coverage-format"`
	DesiredCoverage    float64 `json:"expectedCoverage" yaml:"expectedCoverage" mapstructure:"expected-coverage"`
	MaxIterations      int     `json:"maxIterations" yaml:"maxIterations" mapstructure:"max-iterations"`
	TestDir            string  `json:"testDir" yaml:"testDir" mapstructure:"test-dir"`
	APIBaseURL         string  `json:"llmBaseUrl" yaml:"llmBaseUrl" mapstructure:"llm-base-url"`
	Model              string  `json:"model" yaml:"model" mapstructure:"model"`
	APIVersion         string  `json:"llmApiVersion" yaml:"llmApiVersion" mapstructure:"llm-api-version"`
}

type Record struct {
	Filters     []Filter      `json:"filters" yaml:"filters" mapstructure:"filters"`
	RecordTimer time.Duration `json:"recordTimer" yaml:"recordTimer" mapstructure:"record-timer"`
	ReRecord    string        `json:"rerecord" yaml:"rerecord" mapstructure:"rerecord"`
}

type Normalize struct {
	SelectedTests []SelectedTests `json:"selectedTests" yaml:"selectedTests" mapstructure:"selected-tests"`
	TestRun       string          `json:"testReport" yaml:"testReport" mapstructure:"test-report"`
}

type BypassRule struct {
	Path string `json:"path" yaml:"path" mapstructure:"path"`
	Host string `json:"host" yaml:"host" mapstructure:"host"`
	Port uint   `json:"port" yaml:"port" mapstructure:"port"`
}

type Filter struct {
	BypassRule `mapstructure:",squash"`
	URLMethods []string          `json:"urlMethods" yaml:"urlMethods" mapstructure:"url-methods"`
	Headers    map[string]string `json:"headers" yaml:"headers" mapstructure:"headers"`
}

type Test struct {
	SelectedTests      map[string][]string `json:"selectedTests" yaml:"selectedTests" mapstructure:"selected-tests"`
	GlobalNoise        Globalnoise         `json:"globalNoise" yaml:"globalNoise" mapstructure:"globalNoise"`
	Delay              uint64              `json:"delay" yaml:"delay" mapstructure:"delay"`
	APITimeout         uint64              `json:"apiTimeout" yaml:"apiTimeout" mapstructure:"api-timeout"`
	Coverage           bool                `json:"coverage" yaml:"coverage" mapstructure:"coverage"`                                 // boolean to capture the coverage in test
	CoverageReportPath string              `json:"coverageReportPath" yaml:"coverageReportPath" mapstructure:"coverage-report-path"` // directory path to store the coverage files
	GoCoverage         bool                `json:"goCoverage" yaml:"goCoverage" mapstructure:"go-coverage"`                          // boolean to capture the coverage in test
	IgnoreOrdering     bool                `json:"ignoreOrdering" yaml:"ignoreOrdering" mapstructure:"ignore-ordering"`
	MongoPassword      string              `json:"mongoPassword" yaml:"mongoPassword" mapstructure:"mongo-password"`
	Language           string              `json:"language" yaml:"language" mapstructure:"language"`
	RemoveUnusedMocks  bool                `json:"removeUnusedMocks" yaml:"removeUnusedMocks" mapstructure:"remove-unused-mocks"`
	FallBackOnMiss     bool                `json:"fallBackOnMiss" yaml:"fallBackOnMiss" mapstructure:"fallBack-on-miss"`
	BasePath           string              `json:"basePath" yaml:"basePath" mapstructure:"base-path"`
	Mocking            bool                `json:"mocking" yaml:"mocking" mapstructure:"mocking"`
}

type Globalnoise struct {
	Global   GlobalNoise  `json:"global" yaml:"global" mapstructure:"global"`
	Testsets TestsetNoise `json:"test-sets" yaml:"test-sets" mapstructure:"test-sets"`
}

type SelectedTests struct {
	TestSet string   `json:"testSet" yaml:"testSet" mapstructure:"test-set"`
	Tests   []string `json:"tests" yaml:"tests" mapstructure:"tests"`
}

type (
	Noise        map[string][]string
	GlobalNoise  map[string]map[string][]string
	TestsetNoise map[string]map[string]map[string][]string
)

func SetByPassPorts(conf *Config, ports []uint) {
	for _, port := range ports {
		conf.BypassRules = append(conf.BypassRules, BypassRule{
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
	if conf.Test.SelectedTests == nil {
		conf.Test.SelectedTests = make(map[string][]string)
	}

	for _, testSet := range testSets {
		conf.Test.SelectedTests[testSet] = []string{}
	}
}

func SetSelectedTestsNormalize(conf *Config, value string) error {
	testSets := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ' '
	})
	var tests []SelectedTests
	if len(testSets) == 0 {
		conf.Normalize.SelectedTests = tests
		return nil
	}
	for _, ts := range testSets {
		parts := strings.Split(ts, ":")
		if len(parts) != 2 {
			return fmt.Errorf("invalid format: %s", ts)
		}
		testCases := strings.Split(parts[1], " ")
		tests = append(tests, SelectedTests{
			TestSet: parts[0],
			Tests:   testCases,
		})
	}
	conf.Normalize.SelectedTests = tests
	return nil
}
