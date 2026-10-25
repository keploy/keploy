//go:build !linux

package utils

// OwnPIDNamespace is the --client-pid-ns value of a keploy CLI that runs in
// no Linux pid namespace.
func OwnPIDNamespace() string { return ClientPIDNSNone }

// ClientPIDHere is the said pid as it is: an agent off Linux has no /proc to
// compare it with (keploy's agent runs on Linux, natively or in Docker).
func ClientPIDHere(said uint32, _ string) ClientPID { return ClientPID{PID: said} }
