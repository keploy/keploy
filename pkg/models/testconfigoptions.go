package models

import "time"

type TestConfigOptions struct {
	Path               *string
	ProxyPort          *uint32
	AppCmd             *string
	Tests              *map[string][]string
	AppContainer       *string
	NetworkName        *string
	Delay              *uint64
	BuildDelay         *time.Duration
	PassThroughPorts   *[]uint
	ApiTimeout         *uint64
	GlobalNoise        *GlobalNoise
	TestSetNoise       *TestsetNoise
	CoverageReportPath *string
	WithCoverage       *bool
	ConfigPath         *string
	IgnoreOrdering     *bool
	PassThroughHosts   *[]Filters
}
