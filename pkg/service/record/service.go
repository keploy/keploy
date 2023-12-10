package record

import (
	"go.keploy.io/server/pkg/models"
	"time"
)

type Recorder interface {
	CaptureTraffic(path string, proxyPort uint32, appCmd, appContainer, networkName string, Delay uint64, buildDelay time.Duration, ports []uint, filters *models.Filters)
}
