//go:build darwin

package proxy

import (
	"net"
	"testing"
)

// stuckPair is a paired connection the proxy holds whose handler never
// closes it (darwinPair's): the application's end, and the proxy's.
func stuckPair(t *testing.T) (p *Proxy, app *net.TCPConn, served net.Conn) {
	t.Helper()
	p, app, proxySide, _ := darwinPair(t)
	return p, app, proxySide
}
