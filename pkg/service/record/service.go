package record

import "go.keploy.io/server/pkg/models"

type Recorder interface {
	CaptureTraffic(path string, proxyPort uint32, appCmd, appContainer, networkName string, Delay uint64, ports []uint, filters *models.Filters)
}
