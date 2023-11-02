package record

type Recorder interface {
	CaptureTraffic(path string, proxyPort uint32, appCmd, appContainer, networkName string, Delay uint64, ports []uint, url []string, header []string)
}
