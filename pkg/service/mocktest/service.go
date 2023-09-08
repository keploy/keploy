package mocktest

type MockTester interface {
	MockTest(path string, pid uint32, dir string)
}
