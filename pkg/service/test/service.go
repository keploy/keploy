package test

type Tester interface {
	Test(tcsPath, mockPath, testReportPath string, pid uint32) bool
}