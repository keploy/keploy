//go:build !linux && !darwin

package util

import "net"

// WatchPeerEnd cannot watch here: watched is false, and stop reports no end.
func WatchPeerEnd(net.Conn, func(reset bool)) (stop func() (ended bool), watched bool) {
	return noPeerEnd, false
}

func noPeerEnd() bool { return false }
