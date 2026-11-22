//go:build !linux

package utils

// KernelPidsAreLocal is true where keploy's proxy reports a process by the
// number the system it reads processes from gives it (macOS's sysctl, which
// knows no PID namespaces).
func KernelPidsAreLocal() bool { return true }
