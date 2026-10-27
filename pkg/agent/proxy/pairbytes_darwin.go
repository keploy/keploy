//go:build darwin

package proxy

import (
	"context"
	"net"
)

// WaitPairedBytes returns nil on darwin: the byte-level tracking of proxy
// connections uses Linux-specific socket options (SO_MEMINFO, FIONREAD) that
// macOS does not have. Returning nil lets the settlement flow proceed without
// waiting for byte counts — the connection-level settlement from
// ProxyConnsReporter is still active.
func (p *Proxy) WaitPairedBytes(context.Context, uint64) error { return nil }

// PairedBytesErr returns nil on darwin: byte counting is skipped, not broken.
func (p *Proxy) PairedBytesErr() error { return nil }

func (p *Proxy) checkPairedBytes(net.Conn, net.Conn) {}

func (p *Proxy) proveAppVisible(*net.TCPConn) {}

// pairClosing notes a paired connection's close.
func (p *Proxy) pairClosing(_ context.Context, conn net.Conn, _ *trackedConn) {
	_, _, _ = p.paired.closing(conn)
}

func readUnread(*net.TCPConn) (int, error) { return 0, nil }

func readProxySide(*net.TCPConn) proxySide { return proxySide{} }
