//go:build !unix

package mockdb

// spillRoom reports false: a file open on Windows cannot be unlinked, so a
// spill a killed replay left would stay in the test set's directory. The
// mocks are read back from the mock file.
func spillRoom(string, int64) bool { return false }
