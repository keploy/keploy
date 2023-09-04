package serve

type Server interface {
	Serve(path, testReportPath string, Delay uint64, pid, port uint32)
}
