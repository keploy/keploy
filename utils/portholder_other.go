//go:build !linux

package utils

// PortHolder names what holds port where the platform says: here it cannot,
// so it is "" (PortTaken then says the holder is unknown).
func PortHolder(uint32) string { return "" }
