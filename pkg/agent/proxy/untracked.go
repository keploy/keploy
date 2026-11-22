package proxy

import (
	"context"
	"net"
	"net/netip"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent"
)

// appEndOf is the application's end of an accepted connection, its remote
// address and port, as the hooks key its redirect.
func appEndOf(conn net.Conn) netip.AddrPort {
	if ta, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		return ta.AddrPort()
	}
	ap, _ := netip.ParseAddrPort(conn.RemoteAddr().String())
	return ap
}

// destFor is where the connection conn was going before the connect hooks
// redirected it: by the application's address and port where the hooks key
// their redirects so (agent.ConnDestInfo, the Linux hooks), by its source port
// alone otherwise.
func (p *Proxy) destFor(ctx context.Context, conn net.Conn, sourcePort int) (*agent.NetworkAddress, error) {
	if cd, ok := p.DestInfo.(agent.ConnDestInfo); ok {
		return cd.GetForConn(ctx, appEndOf(conn))
	}
	return p.DestInfo.Get(ctx, uint16(sourcePort))
}

// dropDest drops conn's redirect once the proxy has it, the same way.
func (p *Proxy) dropDest(ctx context.Context, conn net.Conn, sourcePort int) error {
	if cd, ok := p.DestInfo.(agent.ConnDestInfo); ok {
		return cd.DeleteForConn(ctx, appEndOf(conn))
	}
	return p.DestInfo.Delete(ctx, uint16(sourcePort))
}

// untracked says, and counts, a connection the proxy could not route: the
// hooks hold no destination for it, so it is closed, and whatever call it
// carried is neither recorded nor mocked. Loud: a WARN with the running count
// at the first such connection and at every doubling after it, and the total
// as the proxy stops (untrackedTotal), never only a DEBUG. A connection that
// arrives while the proxy is stopping loses its entry with the hooks: that
// one is a DEBUG, and not counted.
func (p *Proxy) untracked(ctx context.Context, conn net.Conn, err error) {
	fields := []zap.Field{
		zap.String("from", conn.RemoteAddr().String()),
		zap.Error(err),
	}
	if ctx.Err() != nil {
		p.logger.Debug("Untracked connection while the proxy stops, closing it", fields...)
		return
	}
	n := p.untrackedConns.Add(1)
	fields = append(fields, zap.Uint64("untracked_connections", n))
	if n&(n-1) != 0 {
		p.logger.Debug("Untracked connection (eBPF lookup failed), closing gracefully", fields...)
		return
	}
	p.logger.Warn(untrackedMsg, append(fields, untrackedWhy...)...)
}

const untrackedMsg = "Untracked connection: keploy's proxy has no record of where this connection was going, so it closed it, and the call it carried was neither recorded nor mocked"

var untrackedWhy = []zap.Field{
	zap.String("reason", "the kernel kept no redirect for it: the connection was not redirected by keploy's connect hooks, or its redirect was dropped before the proxy read it"),
	zap.String("next_step", "if this repeats, report it with the agent's log: every call on such a connection is missing from the recording"),
}

// untrackedTotal says, as the proxy stops, how many connections it could not
// route in all, when its last WARN did not already say that number.
func (p *Proxy) untrackedTotal() {
	n := p.untrackedConns.Load()
	if n == 0 || n&(n-1) == 0 {
		return
	}
	p.logger.Warn(untrackedMsg+" (the total as the proxy stops)", append([]zap.Field{zap.Uint64("untracked_connections", n)}, untrackedWhy...)...)
}
