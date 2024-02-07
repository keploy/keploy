package record

import (
	"time"

	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.keploy.io/server/pkg/platform/telemetry"
)

type Recorder interface {
	CaptureTraffic(path string, proxyPort uint32, appCmd, appContainer, networkName string, dirName string, Delay uint64, buildDelay time.Duration, ports []uint, filters *models.TestFilter, tcDB platform.TestCaseDB, tele *telemetry.Telemetry, passThroughHosts []models.Filters, keployRecorderStopTimer time.Duration)
	StartCaptureTraffic(path string, proxyPort uint32, appCmd, appContainer, networkName string, Delay uint64, buildDelay time.Duration, ports []uint, filters *models.TestFilter, enableTele bool, passThroughHosts []models.Filters, keployRecorderStopTimer time.Duration)
}
