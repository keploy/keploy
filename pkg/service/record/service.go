package record

type Recorder interface {
	CaptureTraffic(tcsPath, mockPath string)
}