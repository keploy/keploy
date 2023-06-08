package record

type Recorder interface {
	CaptureTraffic(tcsPath, mockPath string, pid uint32, appCmd, appContainer string)
}
