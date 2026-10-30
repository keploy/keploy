//go:build !linux

package util

import "net"

// WatchUntilTaken does nothing here: handshakes are held, and connections
// pre-dialled, on Linux only.
func (p *Predialed) WatchUntilTaken(func()) {}

func closedByPeer(net.Conn) bool { return false }

// ReceivedNothing cannot tell here, so it says the application has sent
// something: nothing is closed on its account.
func ReceivedNothing(net.Conn) bool { return false }

// Waiting cannot look here: it reports nothing waiting.
func Waiting(net.Conn) (data, closed bool) { return false, false }

// ArrivedNothing cannot tell here: known is false.
func ArrivedNothing(net.Conn) (nothing, known bool) { return false, false }

// WatchPeerEnd cannot watch here: its stop reports no end.
func WatchPeerEnd(net.Conn, func(reset bool)) (stop func() (ended bool)) {
	return func() bool { return false }
}
