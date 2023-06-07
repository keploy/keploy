package test

type Tester interface {
	Test(tcsPath, mockPath string, pid uint32) bool
}