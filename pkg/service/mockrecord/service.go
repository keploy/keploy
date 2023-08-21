package mockrecord

type MockRecorder interface {
	MockRecord(path string, Delay uint64, pid uint32, dir string)
}
