//go:build !linux && !darwin

package util

import "net"

// PeerGone cannot ask here: known is false.
func PeerGone(net.Conn) (gone, known bool) { return false, false }
