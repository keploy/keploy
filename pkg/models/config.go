package models

import "time"

type Config struct {
	Record Record `json:"record" yaml:"record"`
	Test   Test   `json:"test" yaml:"test"`
}

type Record struct {
	Path             string        `json:"path" yaml:"path"`
	Command          string        `json:"command" yaml:"command"`
	ProxyPort        uint32        `json:"proxyport" yaml:"proxyport"`
	ContainerName    string        `json:"containerName" yaml:"containerName"`
	NetworkName      string        `json:"networkName" yaml:"networkName"`
	Delay            uint64        `json:"delay" yaml:"delay"`
	BuildDelay       time.Duration `json:"buildDelay" yaml:"buildDelay"`
	PassThroughPorts []uint        `json:"passThroughPorts" yaml:"passThroughPorts"`
	Filters          Filters       `json:"filters" yaml:"filters"`
}

type Filters struct {
	ReqHeader  []string            `json:"req_header" yaml:"req_header"`
	URLMethods map[string][]string `json:"urlMethods" yaml:"urlMethods"`
}

type Test struct {
	Path             string               `json:"path" yaml:"path"`
	Command          string               `json:"command" yaml:"command"`
	ProxyPort        uint32               `json:"proxyport" yaml:"proxyport"`
	ContainerName    string               `json:"containerName" yaml:"containerName"`
	NetworkName      string               `json:"networkName" yaml:"networkName"`
	Tests            map[string][]string  `json:"tests" yaml:"tests"`
	GlobalNoise      string               `json:"globalNoise" yaml:"globalNoise"`
	Delay            uint64               `json:"delay" yaml:"delay"`
	BuildDelay       time.Duration        `json:"buildDelay" yaml:"buildDelay"`
	ApiTimeout       uint64               `json:"apiTimeout" yaml:"apiTimeout"`
	PassThroughPorts []uint               `json:"passThroughPorts" yaml:"passThroughPorts"`
	WithCoverage       bool               `json:"withCoverage" yaml:"withCoverage"` // boolean to capture the coverage in test
	CoverageReportPath string             `json:"coverageReportPath" yaml:"coverageReportPath"` // directory path to store the coverage files
}

type Globalnoise struct {
	Global     GlobalNoise    `json:"global" yaml:"global"`
	Testsets   TestsetNoise   `json:"test-sets" yaml:"test-sets"`
}

type (
	Noise        map[string][]string
	GlobalNoise  map[string]map[string][]string
	TestsetNoise map[string]map[string]map[string][]string
)
