package mockrecord

type MockRecorder interface {
	MockRecord(path string, pid uint32, dir string)
}
