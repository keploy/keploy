package record

import (
	"time"

	"go.keploy.io/server/pkg/models"
)

type TrafficCaptureParams struct {
	Path             string
	ProxyPort        uint32
	AppCmd           string
	AppContainer     string
	AppNetwork       string
	Delay            uint64
	BuildDelay       time.Duration
	Ports            []uint
	Filters          *models.TestFilter
	EnableTele       bool
	PassThroughHosts []models.Filters
}
type Recorder interface {
	CaptureTraffic(params TrafficCaptureParams)
}
