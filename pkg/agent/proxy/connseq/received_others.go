//go:build !linux

package connseq

import "net"

// socketCounterOf asks no socket for a count off Linux (macOS and Windows,
// where keploy's enterprise build records natively): every conn numbers by
// what Read has returned and holds back (Upstream.sentLocked). There, bytes
// the destination sent that wait at the proxy, unread when a client chunk is
// read, are numbered after it, as they were numbered before Upstream.
func socketCounterOf(net.Conn) socketCounter { return nil }
