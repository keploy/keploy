package test

type Tester interface {
	// Test(tcsPath, mockPath, testReportPath string, appCmd, appContainer, networkName string, Delay uint64) bool
	Test(path, testReportPath string, appCmd, appContainer, networkName string, Delay uint64) bool
}
