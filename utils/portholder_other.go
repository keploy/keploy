//go:build !linux

package utils

// PortHolder names what holds port where the platform says: here it cannot,
// so it is "" (PortTaken then says the holder is unknown).
func PortHolder(uint32) string { return "" }

// unnamedHolder is why PortTaken names no holder: keploy reads a port's
// holder only on Linux.
const unnamedHolder = "keploy cannot read what holds a port on this platform"
