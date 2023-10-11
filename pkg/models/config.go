package models

type Config struct {
	Record  Record    `json:"record" yaml:"record"`
	Test    Test      `json:"test" yaml:"test"`
}

type Record struct {
	Path string  `json:"path" yaml:"path"`
	Command string `json:"command" yaml:"command"`
	ContainerName string `json:"containerName" yaml:"containerName"`
	NetworkName string `json:"networkName" yaml:"networkName"`
	Delay uint64 `json:"delay" yaml:"delay"`
	PassThroughPorts []uint `json:"passThroughPorts" yaml:"passThroughPorts"`
}

type Test struct {
	Path string  `json:"path" yaml:"path"`
	Command string `json:"command" yaml:"command"`
	ContainerName string `json:"containerName" yaml:"containerName"`
	NetworkName string `json:"networkName" yaml:"networkName"`
	TestSets []string `json:"testSets" yaml:"testSets"`
	Delay uint64 `json:"delay" yaml:"delay"`
	ApiTimeout uint64 `json:"apiTimeout" yaml:"apiTimeout"`
	PassThroughPorts []uint `json:"passThroughPorts" yaml:"passThroughPorts"`
}