//go:build !linux && !darwin

package proxy

import "net"

func listenerOwner(net.IP, uint32) (int, bool) { return 0, true }

func listening(net.IP, uint32) bool { return true }

func descends(int, int) bool { return false }
