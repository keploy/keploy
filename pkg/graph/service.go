package graph

import (
	"net/http"
)

type graphInterface interface {
	Serve(path string, proxyPort uint32, testReportPath string, disableReportFile bool, Delay uint64, pid, port uint32, lang string, passThroughPorts []uint, apiTimeout uint64, appCmd string, enableTele bool)
	stopGraphqlServer(http *http.Server)
}