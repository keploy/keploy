package mocktest

type MockTester interface {
	MockTest(path string, proxyPort uint32, pid uint32, dir string, enableTele bool)
}
