// Package config provides configuration structures for the application.
package config

import (
	"fmt"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

type Config struct {
	Path       string     `json:"path" yaml:"path" mapstructure:"path"`
	AppName    string     `json:"appName" yaml:"appName" mapstructure:"appName"`
	AppID      uint64     `json:"appId" yaml:"appId" mapstructure:"appId"` // deprecated field
	Command    string     `json:"command" yaml:"command" mapstructure:"command"`
	Templatize Templatize `json:"templatize" yaml:"templatize" mapstructure:"templatize"`
	Port       uint32     `json:"port" yaml:"port" mapstructure:"port"`
	E2E        bool       `json:"e2e" yaml:"e2e" mapstructure:"e2e"`
	DNSPort    uint32     `json:"dnsPort" yaml:"dnsPort" mapstructure:"dnsPort"`
	ProxyPort  uint32     `json:"proxyPort" yaml:"proxyPort" mapstructure:"proxyPort"`
	// MySQLPorts lists destination ports that should be routed through the
	// MySQL integration (record/replay). This allows supporting MySQL-wire-compatible
	// databases running on non-standard ports (e.g. TiDB on 4000) without code changes.
	// Defaults to [3306] via the default config; any ports added here are treated as MySQL/DB ports.
	MySQLPorts            []uint32            `json:"mysqlPorts" yaml:"mysqlPorts" mapstructure:"mysqlPorts"`
	IncomingProxyPort     uint16              `json:"incomingProxyPort" yaml:"incomingProxyPort" mapstructure:"incomingProxyPort"`
	Debug                 bool                `json:"debug" yaml:"debug" mapstructure:"debug"`
	DisableTele           bool                `json:"disableTele" yaml:"disableTele" mapstructure:"disableTele"`
	DisableANSI           bool                `json:"disableANSI" yaml:"disableANSI" mapstructure:"disableANSI"`
	JSONOutput            bool                `json:"jsonOutput" yaml:"jsonOutput" mapstructure:"jsonOutput"`
	InDocker              bool                `json:"inDocker" yaml:"-" mapstructure:"inDocker"`
	ContainerName         string              `json:"containerName" yaml:"containerName" mapstructure:"containerName"`
	NetworkName           string              `json:"networkName" yaml:"networkName" mapstructure:"networkName"`
	BuildDelay            uint64              `json:"buildDelay" yaml:"buildDelay" mapstructure:"buildDelay"`
	Test                  Test                `json:"test" yaml:"test" mapstructure:"test"`
	Record                Record              `json:"record" yaml:"record" mapstructure:"record"`
	Report                Report              `json:"report" yaml:"report" mapstructure:"report"`
	Normalize             Normalize           `json:"normalize" yaml:"-" mapstructure:"normalize"`
	DisableMapping        bool                `json:"disableMapping" yaml:"disableMapping" mapstructure:"disableMapping"`
	RetryPassing          bool                `json:"retryPassing" yaml:"retryPassing" mapstructure:"retryPassing"`
	ConfigPath            string              `json:"configPath" yaml:"configPath" mapstructure:"configPath"`
	BypassRules           []models.BypassRule `json:"bypassRules" yaml:"bypassRules" mapstructure:"bypassRules"`
	EnableTesting         bool                `json:"enableTesting" yaml:"-" mapstructure:"enableTesting"`
	GenerateGithubActions bool                `json:"generateGithubActions" yaml:"generateGithubActions" mapstructure:"generateGithubActions"`
	KeployContainer       string              `json:"keployContainer" yaml:"keployContainer" mapstructure:"keployContainer"`
	KeployNetwork         string              `json:"keployNetwork" yaml:"keployNetwork" mapstructure:"keployNetwork"`
	CommandType           string              `json:"cmdType" yaml:"cmdType" mapstructure:"cmdType"`
	Contract              Contract            `json:"contract" yaml:"contract" mapstructure:"contract"`
	Agent                 Agent               `json:"agent" yaml:"agent" mapstructure:"agent"`
	InCi                  bool                `json:"inCi" yaml:"inCi" mapstructure:"inCi"`
	InstallationID        string              `json:"-" yaml:"-" mapstructure:"-"`
	ServerPort            uint32              `json:"serverPort" yaml:"serverPort" mapstructure:"serverPort"`
	Version               string              `json:"-" yaml:"-" mapstructure:"-"`
	APIServerURL          string              `json:"-" yaml:"-" mapstructure:"-"`
	GitHubClientID        string              `json:"-" yaml:"-" mapstructure:"-"`
	// InMemoryCompose holds docker-compose YAML content in memory to avoid writing
	// sensitive environment variables (secrets, tokens) to disk. When set, the
	// compose command uses "-f -" and pipes this content via stdin.
	InMemoryCompose []byte `json:"-" yaml:"-" mapstructure:"-"`
}

type Agent struct {
	models.SetupOptions
}

type Templatize struct {
	TestSets []string `json:"testSets" yaml:"testSets" mapstructure:"testSets"`
}

type Record struct {
	Filters           []models.Filter `json:"filters" yaml:"filters" mapstructure:"filters"`
	BasePath          string          `json:"basePath" yaml:"basePath" mapstructure:"basePath"`
	RecordTimer       time.Duration   `json:"recordTimer" yaml:"recordTimer" mapstructure:"recordTimer"`
	Metadata          string          `json:"metadata" yaml:"metadata" mapstructure:"metadata"`
	Synchronous       bool            `json:"sync" yaml:"sync" mapstructure:"sync"`
	EnableSampling    int             `json:"enableSampling" yaml:"enableSampling"`
	MemoryLimit       uint64          `json:"memoryLimit" yaml:"memoryLimit" mapstructure:"memoryLimit"`
	GlobalPassthrough bool            `json:"globalPassthrough" yaml:"globalPassthrough" mapstructure:"globalPassthrough"`
	TLSPrivateKeyPath string          `json:"tlsPrivateKeyPath" yaml:"tlsPrivateKeyPath" mapstructure:"tlsPrivateKeyPath"`
	// MockFormat selects the on-disk format for recorded mocks.
	// "" or "yaml" (default) writes mocks.yaml — human-readable, the
	// format all tooling expects. "gob" writes a binary mocks.gob — a
	// ~28% CPU reduction on the record client at high throughput, at
	// the cost of not being grep/diff-friendly and having no cross-
	// Go-version stability contract. The env var KEPLOY_MOCK_FORMAT
	// takes precedence over this field for ad-hoc experimentation.
	MockFormat string `json:"mockFormat,omitempty" yaml:"mockFormat,omitempty" mapstructure:"mockFormat"`
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
	AllowHighRisk bool            `json:"allowHighRisk" yaml:"allowHighRisk" mapstructure:"allowHighRisk"`
	EditedBy      string          `json:"-" yaml:"-" mapstructure:"-"`
}

type Test struct {
	SelectedTests               map[string][]string `json:"selectedTests" yaml:"selectedTests" mapstructure:"selectedTests"`
	GlobalNoise                 Globalnoise         `json:"globalNoise" yaml:"globalNoise" mapstructure:"globalNoise"`
	ReplaceWith                 ReplaceWith         `json:"replaceWith" yaml:"replaceWith" mapstructure:"replaceWith"`
	Delay                       uint64              `json:"delay" yaml:"delay" mapstructure:"delay"`
	HealthURL                   string              `json:"healthUrl" yaml:"healthUrl" mapstructure:"healthUrl"`                         // optional HTTP(S) URL polled before firing the first test; empty preserves the fixed --delay behavior
	HealthPollTimeout           time.Duration       `json:"healthPollTimeout" yaml:"healthPollTimeout" mapstructure:"healthPollTimeout"` // ceiling for the pre-test health poll loop before falling back to --delay
	Host                        string              `json:"host" yaml:"host" mapstructure:"host"`
	Port                        uint32              `json:"port" yaml:"port" mapstructure:"port"`
	GRPCPort                    uint32              `json:"grpcPort" yaml:"grpcPort" mapstructure:"grpcPort"`
	SSEPort                     uint32              `json:"ssePort" yaml:"ssePort" mapstructure:"ssePort"`
	Protocol                    ProtocolConfig      `json:"protocol" yaml:"protocol" mapstructure:"protocol"`
	APITimeout                  uint64              `json:"apiTimeout" yaml:"apiTimeout" mapstructure:"apiTimeout"`
	SkipCoverage                bool                `json:"skipCoverage" yaml:"skipCoverage" mapstructure:"skipCoverage"`                   // boolean to capture the coverage in test
	CoverageReportPath          string              `json:"coverageReportPath" yaml:"coverageReportPath" mapstructure:"coverageReportPath"` // directory path to store the coverage files
	IgnoreOrdering              bool                `json:"ignoreOrdering" yaml:"ignoreOrdering" mapstructure:"ignoreOrdering"`
	MongoPassword               string              `json:"mongoPassword" yaml:"mongoPassword" mapstructure:"mongoPassword"`
	Language                    models.Language     `json:"language" yaml:"language" mapstructure:"language"`
	RemoveUnusedMocks           bool                `json:"removeUnusedMocks" yaml:"removeUnusedMocks" mapstructure:"removeUnusedMocks"`
	PreserveFailedMocks         bool                `json:"preserveFailedMocks" yaml:"preserveFailedMocks" mapstructure:"preserveFailedMocks"` // skip mock pruning when tests fail (set by k8s-proxy autoreplay)
	FallBackOnMiss              bool                `json:"fallBackOnMiss" yaml:"fallBackOnMiss" mapstructure:"fallBackOnMiss"`                // Deprecated: this flag is ignored. Replay is now always deterministic.
	JacocoAgentPath             string              `json:"jacocoAgentPath" yaml:"jacocoAgentPath" mapstructure:"jacocoAgentPath"`
	BasePath                    string              `json:"basePath" yaml:"basePath" mapstructure:"basePath"`
	Mocking                     bool                `json:"mocking" yaml:"mocking" mapstructure:"mocking"`
	IgnoredTests                map[string][]string `json:"ignoredTests" yaml:"ignoredTests" mapstructure:"ignoredTests"`
	DisableLineCoverage         bool                `json:"disableLineCoverage" yaml:"disableLineCoverage" mapstructure:"disableLineCoverage"`
	UpdateTemplate              bool                `json:"updateTemplate" yaml:"updateTemplate" mapstructure:"updateTemplate"`
	MustPass                    bool                `json:"mustPass" yaml:"mustPass" mapstructure:"mustPass"`
	MaxFailAttempts             uint32              `json:"maxFailAttempts" yaml:"maxFailAttempts" mapstructure:"maxFailAttempts"`
	MaxFlakyChecks              uint32              `json:"maxFlakyChecks" yaml:"maxFlakyChecks" mapstructure:"maxFlakyChecks"`
	ProtoFile                   string              `json:"protoFile" yaml:"protoFile" mapstructure:"protoFile"`
	ProtoDir                    string              `json:"protoDir" yaml:"protoDir" mapstructure:"protoDir"`
	ProtoInclude                []string            `json:"protoInclude" yaml:"protoInclude" mapstructure:"protoInclude"`
	CompareAll                  bool                `json:"compareAll" yaml:"compareAll" mapstructure:"compareAll"`
	SchemaMatch                 bool                `json:"schemaMatch" yaml:"schemaMatch" mapstructure:"schemaMatch"`
	UpdateTestMapping           bool                `json:"updateTestMapping" yaml:"updateTestMapping" mapstructure:"updateTestMapping"`
	DisableAutoHeaderNoise      bool                `json:"disableAutoHeaderNoise" yaml:"disableAutoHeaderNoise" mapstructure:"disableAutoHeaderNoise"`                                    // skip auto-noise for flaky headers (e.g. AWS SigV4)
	StrictMockWindow            bool                `json:"strictMockWindow" yaml:"strictMockWindow" mapstructure:"strictMockWindow"`                                                      // Strict containment: per-test (LifetimePerTest) mocks whose request timestamp falls outside the outer test window are DROPPED rather than promoted to the cross-test unfiltered pool, which eliminates cross-test mock bleed. Default TRUE now that every stateful-protocol recorder classifies mocks finely enough (session vs per-test for connection-alive commands, per-connection data mocks) that legitimate cross-test sharing is encoded as session/connection lifetime rather than implicit out-of-window reuse. Opt out by setting this to false in keploy.yaml, or export KEPLOY_STRICT_MOCK_WINDOW=0 at process start — the env var wins over config.
	ConnectionPoolIdleRetention time.Duration       `json:"connectionPoolIdleRetention,omitempty" yaml:"connectionPoolIdleRetention,omitempty" mapstructure:"connectionPoolIdleRetention"` // How long a per-connID connection-scoped mock pool survives without activity before the idle sweeper reclaims it. Default 5m — enough for HikariCP-style pooled connections bridging test boundaries without activity. Extend for long-running integration tests that may idle a connection between requests for more than 5 minutes; shorter values make the sweeper more aggressive at cost of potentially reclaiming active connections. Zero / negative reverts to the default.
	CmdUsed                     string              `json:"-" yaml:"-" mapstructure:"-"`                                                                                                   // Full command used for the test run (set at runtime)
}

type Report struct {
	SelectedTestSets map[string][]string `json:"selectedTestSets" yaml:"selectedTestSets" mapstructure:"selectedTestSets"`
	ShowFullBody     bool                `json:"showFullBody" yaml:"showFullBody" mapstructure:"showFullBody"`
	ReportPath       string              `json:"reportPath" yaml:"reportPath" mapstructure:"reportPath"`
	Summary          bool                `json:"summary" yaml:"summary" mapstructure:"summary"`
	TestCaseIDs      []string            `json:"testCaseIDs" yaml:"testCaseIDs" mapstructure:"testCaseIDs"`
	Format           string              `json:"format" yaml:"format" mapstructure:"format"`
}

type Globalnoise struct {
	Global   GlobalNoise  `json:"global" yaml:"global" mapstructure:"global"`
	Testsets TestsetNoise `json:"test-sets" yaml:"test-sets" mapstructure:"test-sets"`
}

type ReplaceWith struct {
	Global   ReplaceWithMap            `json:"global" yaml:"global" mapstructure:"global"`
	TestSets map[string]ReplaceWithMap `json:"test-sets" yaml:"test-sets" mapstructure:"test-sets"`
}

type ReplaceWithMap struct {
	URL  map[string]string `json:"url" yaml:"url" mapstructure:"url"`
	Port map[uint32]uint32 `json:"port" yaml:"port" mapstructure:"port"`
}

// ProtocolSettings holds per-protocol configuration. Add new fields here
// to extend all protocols without changing the map structure.
type ProtocolSettings struct {
	Port uint32 `json:"port" yaml:"port" mapstructure:"port"`
}

// ProtocolConfig maps protocol names (e.g. "http", "sse", "grpc") to their
// settings. The map schema allows additional protocol names in the config
// without schema changes, but only protocols recognized by the application
// are currently used by the replay and protocol-handling logic.
type ProtocolConfig map[string]ProtocolSettings

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
	value = strings.TrimSpace(value)

	// No tests provided -> clear selection and return
	if value == "" {
		conf.Normalize.SelectedTests = nil
		return nil
	}

	// Split only on commas: each token represents one test-set specification.
	// Examples:
	//   "ts1, ts2:tc1 tc2" =>
	//      "ts1"
	//      "ts2:tc1 tc2"
	parts := strings.Split(value, ",")

	var selected []SelectedTests

	for _, part := range parts {
		spec := strings.TrimSpace(part)
		if spec == "" {
			continue
		}

		// Check if this spec has an explicit list of test cases, e.g. "ts2:tc1 tc2"
		idx := strings.Index(spec, ":")

		if idx != -1 {
			testSetName := strings.TrimSpace(spec[:idx])
			if testSetName == "" {
				return fmt.Errorf("invalid format (missing test set name): %q", spec)
			}

			testsPart := strings.TrimSpace(spec[idx+1:])
			var testCases []string
			if testsPart != "" {
				for _, tc := range strings.Fields(testsPart) {
					tc = strings.TrimSpace(tc)
					if tc != "" {
						testCases = append(testCases, tc)
					}
				}
			}

			selected = append(selected, SelectedTests{
				TestSet: testSetName,
				// Empty testCases slice means "all tests" in that test set.
				Tests: testCases,
			})
			continue
		}

		// No colon -> entire token is just the test-set name, implies "all tests in this set"
		selected = append(selected, SelectedTests{
			TestSet: spec,
			Tests:   []string{}, // empty slice => all tests in this test set
		})
	}

	conf.Normalize.SelectedTests = selected
	return nil
}
