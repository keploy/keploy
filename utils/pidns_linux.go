//go:build linux

package utils

// KernelPidsAreLocal reports whether a kernel pid names in this process's
// /proc the process it names in the kernel (KernelPidsLocalIn("/proc"), read
// once). keploy names a process by its kernel pid only then: elsewhere it
// names none, and says why.
func KernelPidsAreLocal() bool { return kernelPidsLocal() }
