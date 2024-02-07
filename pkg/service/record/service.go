package record

import (
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.keploy.io/server/pkg/platform/telemetry"
)

type Recorder interface {
	StartCaptureTraffic(options models.RecordOptions)
  CaptureTraffic(path string, proxyPort uint32, appCmd, appContainer, networkName string, dirName string, Delay uint64, buildDelay time.Duration, ports []uint, filters *models.TestFilter, tcDB platform.TestCaseDB, tele *telemetry.Telemetry, passThroughHosts []models.Filters)
	
}
