package serve

import "time"

type Server interface {
	Serve(path string, proxyPort uint32, testReportPath string, Delay uint64, BuildDelay time.Duration, pid, port uint32, lang string, passThroughPorts []uint, apiTimeout uint64, appCmd string)
}
