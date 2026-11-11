//go:build !linux

package proxy

import "net"

// listenerHeldBy and sameContainer are own_listener_linux.go's; elsewhere
// keploy cannot tell, so no process holds the listener, none shares a
// container, and the call takes the mock path.
func listenerHeldBy(net.IP, uint32, int) bool { return false }

func sameContainer(int, int) bool { return false }
