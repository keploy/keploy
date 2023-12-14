package serve

type Server interface {
	Serve(path string, proxyPort uint32, testReportPath string, Delay uint64, pid, port uint32, lang string, passThroughPorts []uint, apiTimeout uint64, appCmd string, disableTele bool)
}
