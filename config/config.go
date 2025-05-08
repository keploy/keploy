// Package config provides configuration structures for the application.
// Editable: No (core package structure)
package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Config is the main application configuration structure.
// Editable: Yes (primary configuration for users)
type Config struct {
	Path                  string       `json:"path" yaml:"path" mapstructure:"path"` // Editable: Yes (application path)
	AppID                 uint64       `json:"appId" yaml:"appId" mapstructure:"appId"` // Editable: No (auto-generated)
	AppName               string       `json:"appName" yaml:"appName" mapstructure:"appName"` // Editable: Yes (user-defined app name)
	Command               string       `json:"command" yaml:"command" mapstructure:"command"` // Editable: Yes (startup command)
	Templatize            Templatize   `json:"templatize" yaml:"templatize" mapstructure:"templatize"` // Editable: Yes (template settings)
	Port                  uint32       `json:"port" yaml:"port" mapstructure:"port"` // Editable: Yes (application port)
	E2E                   bool         `json:"e2e" yaml:"e2e" mapstructure:"e2e"` // Editable: Yes (E2E testing flag)
	DNSPort               uint32       `json:"dnsPort" yaml:"dnsPort" mapstructure:"dnsPort"` // Editable: No (internal use)
	ProxyPort             uint32       `json:"proxyPort" yaml:"proxyPort" mapstructure:"proxyPort"` // Editable: No (internal use)
	Debug                 bool         `json:"debug" yaml:"debug" mapstructure:"debug"` // Editable: Yes (debug mode)
	DisableTele           bool         `json:"disableTele" yaml:"disableTele" mapstructure:"disableTele"` // Editable: Yes (telemetry setting)
	DisableANSI           bool         `json:"disableANSI" yaml:"disableANSI" mapstructure:"disableANSI"` // Editable: Yes (ANSI colors)
	InDocker              bool         `json:"inDocker" yaml:"-" mapstructure:"inDocker"` // Editable: No (auto-detected)
	ContainerName         string       `json:"containerName" yaml:"containerName" mapstructure:"containerName"` // Editable: Yes (Docker container name)
	NetworkName           string       `json:"networkName" yaml:"networkName" mapstructure:"networkName"` // Editable: Yes (Docker network name)
	BuildDelay            uint64       `json:"buildDelay" yaml:"buildDelay" mapstructure:"buildDelay"` // Editable: Yes (build delay in seconds)
	Test                  Test         `json:"test" yaml:"test" mapstructure:"test"` // Editable: Yes (test settings)
	Record                Record       `json:"record" yaml:"record" mapstructure:"record"` // Editable: Yes (recording settings)
	Gen                   UtGen        `json:"gen" yaml:"-" mapstructure:"gen"` // Editable: No (internal generation)
	Normalize             Normalize    `json:"normalize" yaml:"-" mapstructure:"normalize"` // Editable: No (internal normalization)
	ReRecord              ReRecord     `json:"rerecord" yaml:"-" mapstructure:"rerecord"` // Editable: No (internal re-recording)
	ConfigPath            string       `json:"configPath" yaml:"configPath" mapstructure:"configPath"` // Editable: Yes (config file path)
	BypassRules           []BypassRule `json:"bypassRules" yaml:"bypassRules" mapstructure:"bypassRules"` // Editable: Yes (proxy bypass rules)
	EnableTesting         bool         `json:"enableTesting" yaml:"-" mapstructure:"enableTesting"` // Editable: No (internal flag)
	GenerateGithubActions bool         `json:"generateGithubActions" yaml:"generateGithubActions" mapstructure:"generateGithubActions"` // Editable: Yes (CI/CD generation)
	KeployContainer       string       `json:"keployContainer" yaml:"keployContainer" mapstructure:"keployContainer"` // Editable: Yes (container name)
	KeployNetwork         string       `json:"keployNetwork" yaml:"keployNetwork" mapstructure:"keployNetwork"` // Editable: Yes (network name)
	CommandType           string       `json:"cmdType" yaml:"cmdType" mapstructure:"cmdType"` // Editable: Yes (command type)
	Contract              Contract     `json:"contract" yaml:"contract" mapstructure:"contract"` // Editable: Yes (contract testing)

	InCi           bool   `json:"inCi" yaml:"inCi" mapstructure:"inCi"` // Editable: No (auto-detected)
	InstallationID string `json:"-" yaml:"-" mapstructure:"-"` // Editable: No (internal use)
	Version        string `json:"-" yaml:"-" mapstructure:"-"` // Editable: No (version info)
	APIServerURL   string `json:"-" yaml:"-" mapstructure:"-"` // Editable: No (internal API URL)
	GitHubClientID string `json:"-" yaml:"-" mapstructure:"-"` // Editable: No (internal use)
}

// UtGen contains unit test generation configuration
// Editable: Yes (when using test generation features)
type UtGen struct {
	SourceFilePath     string  `json:"sourceFilePath" yaml:"sourceFilePath" mapstructure:"sourceFilePath"` // Editable: Yes (source file path)
	TestFilePath       string  `json:"testFilePath" yaml:"testFilePath" mapstructure:"testFilePath"` // Editable: Yes (test file path)
	CoverageReportPath string  `json:"coverageReportPath" yaml:"coverageReportPath" mapstructure:"coverageReportPath"` // Editable: Yes (coverage report path)
	TestCommand        string  `json:"testCommand" yaml:"testCommand" mapstructure:"testCommand"` // Editable: Yes (test command)
	CoverageFormat     string  `json:"coverageFormat" yaml:"coverageFormat" mapstructure:"coverageFormat"` // Editable: Yes (coverage format)
	DesiredCoverage    float64 `json:"expectedCoverage" yaml:"expectedCoverage" mapstructure:"expectedCoverage"` // Editable: Yes (target coverage)
	MaxIterations      int     `json:"maxIterations" yaml:"maxIterations" mapstructure:"maxIterations"` // Editable: Yes (max generation attempts)
	TestDir            string  `json:"testDir" yaml:"testDir" mapstructure:"testDir"` // Editable: Yes (test directory)
	APIBaseURL         string  `json:"llmBaseUrl" yaml:"llmBaseUrl" mapstructure:"llmBaseUrl"` // Editable: No (internal API URL)
	Model              string  `json:"model" yaml:"model" mapstructure:"model"` // Editable: Yes (LLM model)
	APIVersion         string  `json:"llmApiVersion" yaml:"llmApiVersion" mapstructure:"llmApiVersion"` // Editable: No (API version)
	AdditionalPrompt   string  `json:"additionalPrompt" yaml:"additionalPrompt" mapstructure:"additionalPrompt"` // Editable: Yes (custom prompts)
	FunctionUnderTest  string  `json:"functionUnderTest" yaml:"-" mapstructure:"functionUnderTest"` // Editable: No (internal use)
	Flakiness          bool    `json:"flakiness" yaml:"flakiness" mapstructure:"flakiness"` // Editable: Yes (flakiness detection)
}

// Templatize contains template configuration
// Editable: Yes (template settings)
type Templatize struct {
	TestSets []string `json:"testSets" yaml:"testSets" mapstructure:"testSets"` // Editable: Yes (test sets to templatize)
}

// Record contains recording configuration
// Editable: Yes (recording settings)
type Record struct {
	Filters     []Filter      `json:"filters" yaml:"filters" mapstructure:"filters"` // Editable: Yes (recording filters)
	BasePath    string        `json:"basePath" yaml:"basePath" mapstructure:"basePath"` // Editable: Yes (base path)
	RecordTimer time.Duration `json:"recordTimer" yaml:"recordTimer" mapstructure:"recordTimer"` // Editable: Yes (recording duration)
}

// ReRecord contains re-recording configuration
// Editable: No (internal use)
type ReRecord struct {
	SelectedTests []string `json:"selectedTests" yaml:"selectedTests" mapstructure:"selectedTests"` // Editable: No (internal test selection)
	Filters       []Filter `json:"filters" yaml:"filters" mapstructure:"filters"` // Editable: No (internal filters)
	Host          string   `json:"host" yaml:"host" mapstructure:"host"` // Editable: No (internal host)
	Port          uint32   `json:"port" yaml:"port" mapstructure:"port"` // Editable: No (internal port)
}

// Contract contains contract testing configuration
// Editable: Yes (contract testing settings)
type Contract struct {
	Services []string `json:"services" yaml:"services" mapstructure:"services"` // Editable: Yes (services in contract)
	Tests    []string `json:"tests" yaml:"tests" mapstructure:"tests"` // Editable: Yes (tests in contract)
	Path     string   `json:"path" yaml:"path" mapstructure:"path"` // Editable: Yes (contract path)
	Download bool     `json:"download" yaml:"download" mapstructure:"download"` // Editable: Yes (download flag)
	Generate bool     `json:"generate" yaml:"generate" mapstructure:"generate"` // Editable: Yes (generation flag)
	Driven   string   `json:"driven" yaml:"driven" mapstructure:"driven"` // Editable: Yes (test driven approach)
	Mappings Mappings `json:"mappings" yaml:"mappings" mapstructure:"mappings"` // Editable: Yes (service mappings)
}

// Mappings contains service mapping configuration
// Editable: Yes (service mappings)
type Mappings struct {
	ServicesMapping map[string][]string `json:"servicesMapping" yaml:"servicesMapping" mapstructure:"servicesMapping"` // Editable: Yes (service mappings)
	Self            string              `json:"self" yaml:"self" mapstructure:"self"` // Editable: Yes (self reference)
}

// Normalize contains normalization configuration
// Editable: No (internal use)
type Normalize struct {
	SelectedTests []SelectedTests `json:"selectedTests" yaml:"selectedTests" mapstructure:"selectedTests"` // Editable: No (internal test selection)
	TestRun       string          `json:"testReport" yaml:"testReport" mapstructure:"testReport"` // Editable: No (internal test report)
}

// BypassRule contains proxy bypass rules
// Editable: Yes (proxy bypass settings)
type BypassRule struct {
	Path string `json:"path" yaml:"path" mapstructure:"path"` // Editable: Yes (path to bypass)
	Host string `json:"host" yaml:"host" mapstructure:"host"` // Editable: Yes (host to bypass)
	Port uint   `json:"port" yaml:"port" mapstructure:"port"` // Editable: Yes (port to bypass)
}

// Filter contains recording filters
// Editable: Yes (recording filter settings)
type Filter struct {
	BypassRule `mapstructure:",squash"`
	URLMethods []string          `json:"urlMethods" yaml:"urlMethods" mapstructure:"urlMethods"` // Editable: Yes (methods to filter)
	Headers    map[string]string `json:"headers" yaml:"headers" mapstructure:"headers"` // Editable: Yes (headers to filter)
}

// Test contains test configuration
// Editable: Yes (test settings)
type Test struct {
	SelectedTests       map[string][]string `json:"selectedTests" yaml:"selectedTests" mapstructure:"selectedTests"` // Editable: Yes (selected tests)
	GlobalNoise         Globalnoise         `json:"globalNoise" yaml:"globalNoise" mapstructure:"globalNoise"` // Editable: Yes (noise configuration)
	Delay               uint64              `json:"delay" yaml:"delay" mapstructure:"delay"` // Editable: Yes (test delay)
	Host                string              `json:"host" yaml:"host" mapstructure:"host"` // Editable: Yes (test host)
	Port                uint32              `json:"port" yaml:"port" mapstructure:"port"` // Editable: Yes (test port)
	APITimeout          uint64              `json:"apiTimeout" yaml:"apiTimeout" mapstructure:"apiTimeout"` // Editable: Yes (API timeout)
	SkipCoverage        bool                `json:"skipCoverage" yaml:"skipCoverage" mapstructure:"skipCoverage"` // Editable: Yes (coverage skip)
	CoverageReportPath  string              `json:"coverageReportPath" yaml:"coverageReportPath" mapstructure:"coverageReportPath"` // Editable: Yes (coverage path)
	IgnoreOrdering      bool                `json:"ignoreOrdering" yaml:"ignoreOrdering" mapstructure:"ignoreOrdering"` // Editable: Yes (ordering flag)
	MongoPassword       string              `json:"mongoPassword" yaml:"mongoPassword" mapstructure:"mongoPassword"` // Editable: Yes (MongoDB password)
	Language            Language            `json:"language" yaml:"language" mapstructure:"language"` // Editable: Yes (test language)
	RemoveUnusedMocks   bool                `json:"removeUnusedMocks" yaml:"removeUnusedMocks" mapstructure:"removeUnusedMocks"` // Editable: Yes (mock cleanup)
	FallBackOnMiss      bool                `json:"fallBackOnMiss" yaml:"fallBackOnMiss" mapstructure:"fallBackOnMiss"` // Editable: Yes (fallback flag)
	JacocoAgentPath     string              `json:"jacocoAgentPath" yaml:"jacocoAgentPath" mapstructure:"jacocoAgentPath"` // Editable: Yes (Jacoco path)
	BasePath            string              `json:"basePath" yaml:"basePath" mapstructure:"basePath"` // Editable: Yes (base path)
	Mocking             bool                `json:"mocking" yaml:"mocking" mapstructure:"mocking"` // Editable: Yes (mocking flag)
	IgnoredTests        map[string][]string `json:"ignoredTests" yaml:"ignoredTests" mapstructure:"ignoredTests"` // Editable: Yes (ignored tests)
	DisableLineCoverage bool                `json:"disableLineCoverage" yaml:"disableLineCoverage" mapstructure:"disableLineCoverage"` // Editable: Yes (line coverage)
	DisableMockUpload   bool                `json:"disableMockUpload" yaml:"disableMockUpload" mapstructure:"disableMockUpload"` // Editable: Yes (mock upload)
	UseLocalMock        bool                `json:"useLocalMock" yaml:"useLocalMock" mapstructure:"useLocalMock"` // Editable: Yes (local mock flag)
	UpdateTemplate      bool                `json:"updateTemplate" yaml:"updateTemplate" mapstructure:"updateTemplate"` // Editable: Yes (template update)
}

// Language represents supported test languages
// Editable: Yes (when setting language)
type Language string

// String is used both by fmt.Print and by Cobra in help text
// Editable: No (internal method)
func (e *Language) String() string {
	return string(*e)
}

// Set must have pointer receiver so it doesn't change the value of a copy
// Editable: No (internal method)
func (e *Language) Set(v string) error {
	switch v {
	case "go", "java", "python", "javascript":
		*e = Language(v)
		return nil
	default:
		return errors.New(`must be one of "go", "java", "python" or "javascript"`)
	}
}

// Type is only used in help text
// Editable: No (internal method)
func (e *Language) Type() string {
	return "myEnum"
}

// Globalnoise contains noise configuration for tests
// Editable: Yes (noise settings)
type Globalnoise struct {
	Global   GlobalNoise  `json:"global" yaml:"global" mapstructure:"global"` // Editable: Yes (global noise)
	Testsets TestsetNoise `json:"test-sets" yaml:"test-sets" mapstructure:"test-sets"` // Editable: Yes (testset noise)
}

// SelectedTests contains selected test configuration
// Editable: No (internal use)
type SelectedTests struct {
	TestSet string   `json:"testSet" yaml:"testSet" mapstructure:"testSet"` // Editable: No (internal test set)
	Tests   []string `json:"tests" yaml:"tests" mapstructure:"tests"` // Editable: No (internal tests)
}

// Noise types definitions
// Editable: Yes (when configuring noise)
type (
	Noise        map[string][]string
	GlobalNoise  map[string]map[string][]string
	TestsetNoise map[string]map[string]map[string][]string
)

// SetByPassPorts configures bypass ports
// Editable: No (internal helper function)
func SetByPassPorts(conf *Config, ports []uint) {
	for _, port := range ports {
		conf.BypassRules = append(conf.BypassRules, BypassRule{
			Path: "",
			Host: "",
			Port: port,
		})
	}
}

// GetByPassPorts retrieves bypass ports
// Editable: No (internal helper function)
func GetByPassPorts(conf *Config) []uint {
	var ports []uint
	for _, rule := range conf.BypassRules {
		ports = append(ports, rule.Port)
	}
	return ports
}

// SetSelectedTests configures selected tests
// Editable: No (internal helper function)
func SetSelectedTests(conf *Config, testSets []string) {
	conf.Test.SelectedTests = make(map[string][]string)
	for _, testSet := range testSets {
		conf.Test.SelectedTests[testSet] = []string{}
	}
}

// SetSelectedServices configures selected services
// Editable: No (internal helper function)
func SetSelectedServices(conf *Config, services []string) {
	conf.Contract.Services = services
}

// SetSelectedContractTests configures selected contract tests
// Editable: No (internal helper function)
func SetSelectedContractTests(conf *Config, tests []string) {
	conf.Contract.Tests = tests
}

// SetSelectedTestsNormalize configures normalized test selection
// Editable: No (internal helper function)
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