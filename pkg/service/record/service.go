package record

type Recorder interface {
	CaptureTraffic(path string, appCmd, appContainer, networkName string, Delay uint64, ports []uint)
}
