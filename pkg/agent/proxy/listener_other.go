//go:build !linux

package proxy

import "net"

func listenerOwner(net.IP, uint32) (int, bool) { return 0, true }

func descends(int, int) bool { return false }
