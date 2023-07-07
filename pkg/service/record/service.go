package record

type Recorder interface {
	// CaptureTraffic(tcsPath, mockPath string, appCmd, appContainer, networkName string, Delay uint64)
	CaptureTraffic(path string, appCmd, appContainer, networkName string, Delay uint64)
}
