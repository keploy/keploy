package record

import (
	"time"

	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.keploy.io/server/pkg/platform/telemetry"
)

type Recorder interface {
	CaptureTraffic(path string, proxyPort uint32, appCmd, appContainer, networkName string, dirName string, Delay uint64, buildDelay time.Duration, ports []uint, filters *models.Filters, tcDB platform.TestCaseDB, tele *telemetry.Telemetry)
	NewStorage(path string, tele *telemetry.Telemetry) (tcDB platform.TestCaseDB, dirName string)
	NewTelemetry() (tele *telemetry.Telemetry)
}
