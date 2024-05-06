// Package config provides configuration structures for the application.
package config

import (
	"fmt"
	"strings"
	"time"
)

type Config struct {
	Path                  string        `json:"path" yaml:"path" mapstructure:"path"`
	ReRecord              string        `json:"rerecord" yaml:"rerecord" mapstructure:"rerecord"`
	Command               string        `json:"command" yaml:"command" mapstructure:"command"`
	Port                  uint32        `json:"port" yaml:"port" mapstructure:"port"`
	DNSPort               uint32        `json:"dnsPort" yaml:"dnsPort" mapstructure:"dnsPort"`
	ProxyPort             uint32        `json:"proxyPort" yaml:"proxyPort" mapstructure:"proxyPort"`
	Debug                 bool          `json:"debug" yaml:"debug" mapstructure:"debug"`
	DisableTele           bool          `json:"disableTele" yaml:"disableTele" mapstructure:"disableTele"`
	DisableANSI           bool          `json:"disableANSI" yaml:"disableANSI" mapstructure:"disableANSI"`
	InDocker              bool          `json:"inDocker" yaml:"inDocker" mapstructure:"inDocker"`
	ContainerName         string        `json:"containerName" yaml:"containerName" mapstructure:"containerName"`
	NetworkName           string        `json:"networkName" yaml:"networkName" mapstructure:"networkName"`
	BuildDelay            time.Duration `json:"buildDelay" yaml:"buildDelay" mapstructure:"buildDelay"`
	Test                  Test          `json:"test" yaml:"test" mapstructure:"test"`
	Record                Record        `json:"record" yaml:"record" mapstructure:"record"`
	Normalize             Normalize     `json:"normalize" yaml:"normalize" mapstructure:"normalize"`
	ConfigPath            string        `json:"configPath" yaml:"configPath" mapstructure:"configPath"`
	BypassRules           []BypassRule  `json:"bypassRules" yaml:"bypassRules" mapstructure:"bypassRules"`
	EnableTesting         bool          `json:"enableTesting" yaml:"enableTesting" mapstructure:"enableTesting"`
	GenerateGithubActions bool          `json:"generateGithubActions" yaml:"generateGithubActions" mapstructure:"generateGithubActions"`
	KeployContainer       string        `json:"keployContainer" yaml:"keployContainer" mapstructure:"keployContainer"`
	KeployNetwork         string        `json:"keployNetwork" yaml:"keployNetwork" mapstructure:"keployNetwork"`
	CommandType           string        `json:"cmdType" yaml:"cmdType" mapstructure:"cmdType"`
}

type Record struct {
	Filters     []Filter      `json:"filters" yaml:"filters" mapstructure:"filters"`
	RecordTimer time.Duration `json:"recordTimer" yaml:"recordTimer" mapstructure:"recordTimer"`
}

type Normalize struct {
	SelectedTests []SelectedTests `json:"selectedTests" yaml:"selectedTests" mapstructure:"selectedTests"`
	TestRun       string          `json:"testReport" yaml:"testReport" mapstructure:"testReport"`
}

type BypassRule struct {
	Path string `json:"path" yaml:"path" mapstructure:"path"`
	Host string `json:"host" yaml:"host" mapstructure:"host"`
	Port uint   `json:"port" yaml:"port" mapstructure:"port"`
}

type Filter struct {
	BypassRule `mapstructure:",squash"`
	URLMethods []string          `json:"urlMethods" yaml:"urlMethods" mapstructure:"urlMethods"`
	Headers    map[string]string `json:"headers" yaml:"headers" mapstructure:"headers"`
}

type Test struct {
	SelectedTests      map[string][]string `json:"selectedTests" yaml:"selectedTests" mapstructure:"selectedTests"`
	GlobalNoise        Globalnoise         `json:"globalNoise" yaml:"globalNoise" mapstructure:"globalNoise"`
	Delay              uint64              `json:"delay" yaml:"delay" mapstructure:"delay"`
	APITimeout         uint64              `json:"apiTimeout" yaml:"apiTimeout" mapstructure:"apiTimeout"`
	Coverage           bool                `json:"coverage" yaml:"coverage" mapstructure:"coverage"`                                // boolean to capture the coverage in test
	CoverageReportPath string              `json:"coverageReportPath" yaml:"coverageReportPath " mapstructure:"coverageReportPath"` // directory path to store the coverage files
	GoCoverage         bool                `json:"goCoverage" yaml:"goCoverage" mapstructure:"goCoverage"`                          // boolean to capture the coverage in test
	IgnoreOrdering     bool                `json:"ignoreOrdering" yaml:"ignoreOrdering" mapstructure:"ignoreOrdering"`
	MongoPassword      string              `json:"mongoPassword" yaml:"mongoPassword" mapstructure:"mongoPassword"`
	Language           string              `json:"language" yaml:"language" mapstructure:"language"`
	RemoveUnusedMocks  bool                `json:"removeUnusedMocks" yaml:"removeUnusedMocks" mapstructure:"removeUnusedMocks"`
	FallBackOnMiss     bool                `json:"fallBackOnMiss" yaml:"fallBackOnMiss" mapstructure:"fallBackOnMiss"`
}

type Globalnoise struct {
	Global   GlobalNoise  `json:"global" yaml:"global" mapstructure:"global"`
	Testsets TestsetNoise `json:"test-sets" yaml:"test-sets" mapstructure:"test-sets"`
}

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
