//go:build !linux

package proxy

import (
	"context"
	"fmt"
	"net"

	"go.keploy.io/server/v3/pkg/models"
)

// WaitPairedBytes cannot tell here: the kernel's count of the connections to
// the proxy, and the sockets it reads, are Linux's.
func (p *Proxy) WaitPairedBytes(context.Context, uint64) error {
	return fmt.Errorf("%w: the bytes on the proxy's connections are counted on Linux only", models.ErrProxyConnsUnsupported)
}

func (p *Proxy) checkPairedBytes(net.Conn, net.Conn) {}

func (p *Proxy) proveAppVisible(*net.TCPConn) {}

func readProxySide(*net.TCPConn) proxySide {
	return proxySide{err: fmt.Errorf("%w: the proxy's sockets are read on Linux only", models.ErrProxyConnsUnsupported)}
}
