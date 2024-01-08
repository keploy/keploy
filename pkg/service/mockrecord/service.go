package mockrecord

type MockRecorder interface {
	MockRecord(path string, proxyPort uint32, pid uint32, dir string, enableTele bool)
}
