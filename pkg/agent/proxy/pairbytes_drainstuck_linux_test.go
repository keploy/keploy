//go:build linux

package proxy

import (
	"net"
	"testing"
)

// stuckPair is a paired connection the proxy holds whose handler never
// closes it (untoldPair's): the application's end, and the proxy's.
func stuckPair(t *testing.T) (p *Proxy, app *net.TCPConn, served net.Conn) {
	t.Helper()
	p, app, _ = untoldPair(t)
	return p, app, proxyServed(t, p)
}
