package record

type Recorder interface {
	CaptureTraffic(tcsPath, mockPath string, appCmd, appContainer string, Delay uint64)
}
