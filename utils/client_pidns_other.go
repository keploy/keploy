//go:build !linux

package utils

// OwnPIDNamespace is the --client-pid-ns value of a keploy CLI that runs in
// no Linux pid namespace.
func OwnPIDNamespace() string { return ClientPIDNSNone }

// ClientPIDHere is the said pid as it is: an agent off Linux shares no pid
// namespace with a Linux CLI and has none to compare, so it takes the pid as
// said.
func ClientPIDHere(said uint32, _ string) ClientPID { return ClientPID{PID: said} }
