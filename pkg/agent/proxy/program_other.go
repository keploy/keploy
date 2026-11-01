//go:build !linux && !darwin

package proxy

// ProgramOf is the program process pid runs; "" here, where keploy cannot
// read it.
func ProgramOf(int) string { return "" }
