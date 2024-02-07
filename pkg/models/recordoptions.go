package models

import "time"

type RecordOptions struct {
	Path             string
	ProxyPort        uint32
	AppCmd           string
	AppContainer     string
	AppNetwork       string
	Delay            uint64
	BuildDelay       time.Duration
	Ports            []uint
	Filters          *TestFilter
	EnableTele       bool
	PassThroughHosts []Filters
}
