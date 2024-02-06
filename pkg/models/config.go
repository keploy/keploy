package models

import "time"

type Config struct {
	Record Record `json:"record" yaml:"record"`
	Test   Test   `json:"test" yaml:"test"`
}

type Record struct {
	Path          string        `json:"path" yaml:"path"`
	Command       string        `json:"command" yaml:"command"`
	ProxyPort     uint32        `json:"proxyport" yaml:"proxyport"`
	ContainerName string        `json:"containerName" yaml:"containerName"`
	NetworkName   string        `json:"networkName" yaml:"networkName"`
	Delay         uint64        `json:"delay" yaml:"delay"`
	BuildDelay    time.Duration `json:"buildDelay" yaml:"buildDelay"`
	Tests         TestFilter    `json:"tests" yaml:"tests"`
	Stubs         Stubs         `json:"stubs" yaml:"stubs"`
}

type TestFilter struct {
	Filters []Filters `json:"filters" yaml:"filters"`
}

type Stubs struct {
	Filters []Filters `json:"filters" yaml:"filters"`
}
type Filters struct {
	Path       string            `json:"path" yaml:"path"`
	UrlMethods []string          `json:"urlMethods" yaml:"urlMethods"`
	Host       string            `json:"host" yaml:"host"`
	Headers    map[string]string `json:"headers" yaml:"headers"`
	Port       uint              `json:"ports" yaml:"ports"`
}

func (tests *TestFilter) GetKind() string {
	return "Tests"
}

type Test struct {
	Path                    string              `json:"path" yaml:"path"`
	Command                 string              `json:"command" yaml:"command"`
	ProxyPort               uint32              `json:"proxyport" yaml:"proxyport"`
	ContainerName           string              `json:"containerName" yaml:"containerName"`
	NetworkName             string              `json:"networkName" yaml:"networkName"`
	SelectedTests           map[string][]string `json:"selectedTests" yaml:"selectedTests"`
	GlobalNoise             Globalnoise         `json:"globalNoise" yaml:"globalNoise"`
	Delay                   uint64              `json:"delay" yaml:"delay"`
	BuildDelay              time.Duration       `json:"buildDelay" yaml:"buildDelay"`
	ApiTimeout              uint64              `json:"apiTimeout" yaml:"apiTimeout"`
	PassThroughPorts        []uint              `json:"passThroughPorts" yaml:"passThroughPorts"`
	BypassEndpointsRegistry []string            `json:"bypassEndpointsRegistry" yaml:"bypassEndpointsRegistry"`
	WithCoverage            bool                `json:"withCoverage" yaml:"withCoverage"`             // boolean to capture the coverage in test
	CoverageReportPath      string              `json:"coverageReportPath" yaml:"coverageReportPath"` // directory path to store the coverage files
	GenerateTestReport      bool                `json:"generateTestReport" yaml:"generateTestReport"` 
	IgnoreOrdering          bool                `json:"ignoreOrdering" yaml:"ignoreOrdering"`
	Stubs                   Stubs               `json:"stubs" yaml:"stubs"`
}

type Globalnoise struct {
	Global   GlobalNoise  `json:"global" yaml:"global"`
	Testsets TestsetNoise `json:"test-sets" yaml:"test-sets"`
}

type (
	Noise        map[string][]string
	GlobalNoise  map[string]map[string][]string
	TestsetNoise map[string]map[string]map[string][]string
)
