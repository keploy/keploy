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

// PairedBytesErr says the bytes cannot be counted here (WaitPairedBytes).
func (p *Proxy) PairedBytesErr() error {
	return fmt.Errorf("%w: the bytes on the proxy's connections are counted on Linux only", models.ErrProxyConnsUnsupported)
}

func (p *Proxy) checkPairedBytes(net.Conn, net.Conn) {}

func (p *Proxy) proveAppVisible(*net.TCPConn) {}

// pairClosing notes a paired connection's close; none is paired here.
func (p *Proxy) pairClosing(_ context.Context, conn net.Conn, _ *trackedConn) {
	_, _, _ = p.paired.closing(conn, nil)
}

func readUnread(*net.TCPConn) (int, error) {
	return 0, fmt.Errorf("%w: the proxy's sockets are read on Linux only", models.ErrProxyConnsUnsupported)
}

func readProxySide(*net.TCPConn) proxySide {
	return proxySide{err: fmt.Errorf("%w: the proxy's sockets are read on Linux only", models.ErrProxyConnsUnsupported)}
}
