package mocktest

type MockTester interface {
	MockTest(path string, Delay uint64, pid uint32, dir string)
}
