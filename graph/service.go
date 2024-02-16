package graph

import (
	"net/http"
)

type graphInterface interface {
	Serve(path string, proxyPort uint32, mongoPassword, testReportPath string, Delay uint64, pid, port uint32, lang string, passThroughPorts []uint, apiTimeout uint64, appCmd string, enableTele bool) error
	stopGraphqlServer(http *http.Server)
}
