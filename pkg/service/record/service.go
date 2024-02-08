package record

import (
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.keploy.io/server/pkg/platform/telemetry"
)

type RecordEnvironment struct {
	options models.RecordOptions
	dirName string
	tcDB    platform.TestCaseDB
	tele    *telemetry.Telemetry
}
type Recorder interface {
	StartCaptureTraffic(options models.RecordOptions)
	CaptureTraffic(RecordEnvironment)
}
