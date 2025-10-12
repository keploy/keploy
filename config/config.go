// Package config provides configuration structures for the application.
package config

import (
	"fmt"
	"strings"
	"time"

	"go.keploy.io/server/v2/pkg/models"
)

type Config struct {
	Path                  string              `json:"path" yaml:"path" mapstructure:"path"`
	AppName               string              `json:"appName" yaml:"appName" mapstructure:"appName"`
	Command               string              `json:"command" yaml:"command" mapstructure:"command"`
	Templatize            Templatize          `json:"templatize" yaml:"templatize" mapstructure:"templatize"`
	Port                  uint32              `json:"port" yaml:"port" mapstructure:"port"`
	E2E                   bool                `json:"e2e" yaml:"e2e" mapstructure:"e2e"`
	DNSPort               uint32              `json:"dnsPort" yaml:"dnsPort" mapstructure:"dnsPort"`
	ProxyPort             uint32              `json:"proxyPort" yaml:"proxyPort" mapstructure:"proxyPort"`
	Debug                 bool                `json:"debug" yaml:"debug" mapstructure:"debug"`
	DisableTele           bool                `json:"disableTele" yaml:"disableTele" mapstructure:"disableTele"`
	DisableANSI           bool                `json:"disableANSI" yaml:"disableANSI" mapstructure:"disableANSI"`
	InDocker              bool                `json:"inDocker" yaml:"-" mapstructure:"inDocker"`
	ContainerName         string              `json:"containerName" yaml:"containerName" mapstructure:"containerName"`
	NetworkName           string              `json:"networkName" yaml:"networkName" mapstructure:"networkName"`
	BuildDelay            uint64              `json:"buildDelay" yaml:"buildDelay" mapstructure:"buildDelay"`
	Test                  Test                `json:"test" yaml:"test" mapstructure:"test"`
	Record                Record              `json:"record" yaml:"record" mapstructure:"record"`
	Report                Report              `json:"report" yaml:"report" mapstructure:"report"`
	Gen                   UtGen               `json:"gen" yaml:"-" mapstructure:"gen"`
	Normalize             Normalize           `json:"normalize" yaml:"-" mapstructure:"normalize"`
	ReRecord              ReRecord            `json:"rerecord" yaml:"-" mapstructure:"rerecord"`
	DisableMapping        bool                `json:"disableMapping" yaml:"disableMapping" mapstructure:"disableMapping"`
	ConfigPath            string              `json:"configPath" yaml:"configPath" mapstructure:"configPath"`
	BypassRules           []models.BypassRule `json:"bypassRules" yaml:"bypassRules" mapstructure:"bypassRules"`
	EnableTesting         bool                `json:"enableTesting" yaml:"-" mapstructure:"enableTesting"`
	GenerateGithubActions bool                `json:"generateGithubActions" yaml:"generateGithubActions" mapstructure:"generateGithubActions"`
	KeployContainer       string              `json:"keployContainer" yaml:"keployContainer" mapstructure:"keployContainer"`
	KeployNetwork         string              `json:"keployNetwork" yaml:"keployNetwork" mapstructure:"keployNetwork"`
	CommandType           string              `json:"cmdType" yaml:"cmdType" mapstructure:"cmdType"`
	Contract              Contract            `json:"contract" yaml:"contract" mapstructure:"contract"`
	Agent                 models.SetupOptions `json:"agent" yaml:"agent" mapstructure:"agent"`
	InCi                  bool                `json:"inCi" yaml:"inCi" mapstructure:"inCi"`
	InstallationID        string              `json:"-" yaml:"-" mapstructure:"-"`
	ServerPort            uint32              `json:"serverPort" yaml:"serverPort" mapstructure:"serverPort"`
	Version               string              `json:"-" yaml:"-" mapstructure:"-"`
	APIServerURL          string              `json:"-" yaml:"-" mapstructure:"-"`
	GitHubClientID        string              `json:"-" yaml:"-" mapstructure:"-"`
}

type UtGen struct {
	SourceFilePath     string  `json:"sourceFilePath" yaml:"sourceFilePath" mapstructure:"sourceFilePath"`
	TestFilePath       string  `json:"testFilePath" yaml:"testFilePath" mapstructure:"testFilePath"`
	CoverageReportPath string  `json:"coverageReportPath" yaml:"coverageReportPath" mapstructure:"coverageReportPath"`
	TestCommand        string  `json:"testCommand" yaml:"testCommand" mapstructure:"testCommand"`
	CoverageFormat     string  `json:"coverageFormat" yaml:"coverageFormat" mapstructure:"coverageFormat"`
	DesiredCoverage    float64 `json:"expectedCoverage" yaml:"expectedCoverage" mapstructure:"expectedCoverage"`
	MaxIterations      int     `json:"maxIterations" yaml:"maxIterations" mapstructure:"maxIterations"`
	TestDir            string  `json:"testDir" yaml:"testDir" mapstructure:"testDir"`
	APIBaseURL         string  `json:"llmBaseUrl" yaml:"llmBaseUrl" mapstructure:"llmBaseUrl"`
	Model              string  `json:"model" yaml:"model" mapstructure:"model"`
	APIVersion         string  `json:"llmApiVersion" yaml:"llmApiVersion" mapstructure:"llmApiVersion"`
	AdditionalPrompt   string  `json:"additionalPrompt" yaml:"additionalPrompt" mapstructure:"additionalPrompt"`
	FunctionUnderTest  string  `json:"functionUnderTest" yaml:"-" mapstructure:"functionUnderTest"`
	Flakiness          bool    `json:"flakiness" yaml:"flakiness" mapstructure:"flakiness"`
}
type Templatize struct {
	TestSets []string `json:"testSets" yaml:"testSets" mapstructure:"testSets"`
}

type Record struct {
	Filters           []models.Filter `json:"filters" yaml:"filters" mapstructure:"filters"`
	BasePath          string          `json:"basePath" yaml:"basePath" mapstructure:"basePath"`
	RecordTimer       time.Duration   `json:"recordTimer" yaml:"recordTimer" mapstructure:"recordTimer"`
	Metadata          string          `json:"metadata" yaml:"metadata" mapstructure:"metadata"`
	GlobalPassthrough bool            `json:"globalPassthrough" yaml:"globalPassthrough" mapstructure:"globalPassthrough"`
}

type ReRecord struct {
	SelectedTests []string        `json:"selectedTests" yaml:"selectedTests" mapstructure:"selectedTests"`
	Filters       []models.Filter `json:"filters" yaml:"filters" mapstructure:"filters"`
	Host          string          `json:"host" yaml:"host" mapstructure:"host"`
	Port          uint32          `json:"port" yaml:"port" mapstructure:"port"`
	ShowDiff      bool            `json:"showDiff" yaml:"showDiff" mapstructure:"showDiff"` // show response diff during rerecord (disabled by default)
	GRPCPort      uint32          `json:"grpcPort" yaml:"grpcPort" mapstructure:"grpcPort"`
	APITimeout    uint64          `json:"apiTimeout" yaml:"apiTimeout" mapstructure:"apiTimeout"`
	AmendTestSet  bool            `json:"amendTestSet" yaml:"amendTestSet" mapstructure:"amendTestSet"`
	Branch        string          `json:"branch" yaml:"branch" mapstructure:"branch"`
	Owner         string          `json:"owner" yaml:"owner" mapstructure:"owner"`
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
}

type Test struct {
	SelectedTests       map[string][]string `json:"selectedTests" yaml:"selectedTests" mapstructure:"selectedTests"`
	GlobalNoise         Globalnoise         `json:"globalNoise" yaml:"globalNoise" mapstructure:"globalNoise"`
	Delay               uint64              `json:"delay" yaml:"delay" mapstructure:"delay"`
	Host                string              `json:"host" yaml:"host" mapstructure:"host"`
	Port                uint32              `json:"port" yaml:"port" mapstructure:"port"`
	GRPCPort            uint32              `json:"grpcPort" yaml:"grpcPort" mapstructure:"grpcPort"`
	APITimeout          uint64              `json:"apiTimeout" yaml:"apiTimeout" mapstructure:"apiTimeout"`
	SkipCoverage        bool                `json:"skipCoverage" yaml:"skipCoverage" mapstructure:"skipCoverage"`                   // boolean to capture the coverage in test
	CoverageReportPath  string              `json:"coverageReportPath" yaml:"coverageReportPath" mapstructure:"coverageReportPath"` // directory path to store the coverage files
	IgnoreOrdering      bool                `json:"ignoreOrdering" yaml:"ignoreOrdering" mapstructure:"ignoreOrdering"`
	MongoPassword       string              `json:"mongoPassword" yaml:"mongoPassword" mapstructure:"mongoPassword"`
	Language            models.Language     `json:"language" yaml:"language" mapstructure:"language"`
	RemoveUnusedMocks   bool                `json:"removeUnusedMocks" yaml:"removeUnusedMocks" mapstructure:"removeUnusedMocks"`
	FallBackOnMiss      bool                `json:"fallBackOnMiss" yaml:"fallBackOnMiss" mapstructure:"fallBackOnMiss"`
	JacocoAgentPath     string              `json:"jacocoAgentPath" yaml:"jacocoAgentPath" mapstructure:"jacocoAgentPath"`
	BasePath            string              `json:"basePath" yaml:"basePath" mapstructure:"basePath"`
	Mocking             bool                `json:"mocking" yaml:"mocking" mapstructure:"mocking"`
	IgnoredTests        map[string][]string `json:"ignoredTests" yaml:"ignoredTests" mapstructure:"ignoredTests"`
	DisableLineCoverage bool                `json:"disableLineCoverage" yaml:"disableLineCoverage" mapstructure:"disableLineCoverage"`
	DisableMockUpload   bool                `json:"disableMockUpload" yaml:"disableMockUpload" mapstructure:"disableMockUpload"`
	UseLocalMock        bool                `json:"useLocalMock" yaml:"useLocalMock" mapstructure:"useLocalMock"`
	UpdateTemplate      bool                `json:"updateTemplate" yaml:"updateTemplate" mapstructure:"updateTemplate"`
	MustPass            bool                `json:"mustPass" yaml:"mustPass" mapstructure:"mustPass"`
	MaxFailAttempts     uint32              `json:"maxFailAttempts" yaml:"maxFailAttempts" mapstructure:"maxFailAttempts"`
	MaxFlakyChecks      uint32              `json:"maxFlakyChecks" yaml:"maxFlakyChecks" mapstructure:"maxFlakyChecks"`
	ProtoFile           string              `json:"protoFile" yaml:"protoFile" mapstructure:"protoFile"`
	ProtoDir            string              `json:"protoDir" yaml:"protoDir" mapstructure:"protoDir"`
	ProtoInclude        []string            `json:"protoInclude" yaml:"protoInclude" mapstructure:"protoInclude"`
}

type Report struct {
	SelectedTestSets map[string][]string `json:"selectedTestSets" yaml:"selectedTestSets" mapstructure:"selectedTestSets"`
	ShowFullBody     bool                `json:"showFullBody" yaml:"showFullBody" mapstructure:"showFullBody"`
	ReportPath       string              `json:"reportPath" yaml:"reportPath" mapstructure:"reportPath"`
	Summary          bool                `json:"summary" yaml:"summary" mapstructure:"summary"`
	TestCaseIDs      []string            `json:"testCaseIDs" yaml:"testCaseIDs" mapstructure:"testCaseIDs"`
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
