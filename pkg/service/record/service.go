package record

import (
	"go.keploy.io/server/pkg/models"
)

type Recorder interface {
	CaptureTraffic(options models.RecordOptions)
}
