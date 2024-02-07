package record

import (
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.keploy.io/server/pkg/platform/telemetry"
)

type Recorder interface {
	StartCaptureTraffic(options models.RecordOptions)
  	CaptureTraffic(options models.RecordOptions,  dirName string, tcDB platform.TestCaseDB, tele *telemetry.Telemetry)
	
}
