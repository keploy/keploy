package util

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Predialed is the upstream connection the proxy opened for an application's
// connection while it held that connection's handshake (synhold): the
// application's connect completed only because this dial did, so the
// connection's first dial of the same address must use it rather than open a
// second one the application never made.
type Predialed struct {
	mu      sync.Mutex
	addr    string
	aliases []string
	conn    net.Conn
	taken   bool
	// watchDone is closed when WatchUntilTaken's watch ends; nil when the
	// connection is not watched.
	watchDone chan struct{}
	// onClosed is what the watch runs, kept so a connection handed back
	// (Return) is watched again.
	onClosed func()
}

// NewPredialed wraps conn, dialled to addr.
func NewPredialed(addr string, conn net.Conn) *Predialed {
	return &Predialed{addr: addr, conn: conn}
}

// Addr is the address the connection was dialled at; "" for a nil one.
func (p *Predialed) Addr() string {
	if p == nil {
		return ""
	}
	return p.addr
}

// Alias lets a dial of addr take the connection too: the same destination
// spelled another way, as the TLS path spells it with the server name the
// application sent (host:port) where the connection was dialled at the
// address the application connected to. The pre-dialled connection reaches
// the address the application chose; redialling the name would open a second
// connection, possibly to another address the name resolves to.
func (p *Predialed) Alias(addr string) {
	if p == nil || addr == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.aliases = append(p.aliases, addr)
}

// take hands the connection to the first dial of its address, once.
func (p *Predialed) take(network, addr string) net.Conn {
	if p == nil || !strings.HasPrefix(network, "tcp") {
		return nil
	}
	p.mu.Lock()
	if p.taken || (addr != p.addr && !slices.Contains(p.aliases, addr)) {
		p.mu.Unlock()
		return nil
	}
	p.taken = true
	done := p.watchDone
	p.mu.Unlock()
	if done != nil {
		// End the watch before the connection changes hands: a read
		// deadline in the past wakes it, and the connection is handed over
		// with none.
		_ = p.conn.SetReadDeadline(time.Unix(1, 0))
		<-done
		_ = p.conn.SetReadDeadline(time.Time{})
	}
	// A connection the destination has already closed is no use to the
	// dial: it dials afresh, as it did before connections were pre-dialled.
	if closedByPeer(p.conn) {
		_ = p.conn.Close()
		return nil
	}
	return p.conn
}

// CloseIfUnused closes the connection if no dial took it: the application's
// connection ended (or took a path that never dials) before its upstream was
// used.
func (p *Predialed) CloseIfUnused() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.taken {
		p.taken = true
		_ = p.conn.Close()
	}
}

// Is reports whether conn is the pre-dialled connection, taken by a dial:
// the application's own upstream rather than one opened for the proxy's own
// purposes.
func (p *Predialed) Is(conn net.Conn) bool {
	if p == nil || conn == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.taken && p.conn == conn
}

// Return hands the pre-dialled connection back after a dial took it for a
// look and decided it is not its to keep (the MySQL detection probe): the
// next dial of its address takes it again, as if it had never been taken.
// replay, if not nil, is what that dial gets instead of the bare connection —
// it wraps it to replay bytes the look already read. It reports whether
// taken was the pre-dialled connection.
func (p *Predialed) Return(taken, replay net.Conn) bool {
	if p == nil || taken == nil {
		return false
	}
	p.mu.Lock()
	if !p.taken || p.conn != taken {
		p.mu.Unlock()
		return false
	}
	if replay != nil {
		p.conn = replay
	}
	p.taken = false
	p.watchDone = nil
	onClosed := p.onClosed
	p.mu.Unlock()
	p.WatchUntilTaken(onClosed)
	return true
}

// DialRaw dials addr, or returns pre — the connection pre-dialled while the
// application's handshake was held — the first time it is asked for its
// address. Every dial of a connection's upstream goes through here or
// DialTLS; a deliberate extra connection (the MySQL recorder's greeting
// fetch) dials on its own and never takes it.
func DialRaw(ctx context.Context, d *net.Dialer, network, addr string, pre *Predialed) (net.Conn, error) {
	c, _, err := dialRaw(ctx, d, network, addr, pre)
	return c, err
}

// dialRaw is DialRaw, reporting whether the connection was the pre-dialled
// one.
func dialRaw(ctx context.Context, d *net.Dialer, network, addr string, pre *Predialed) (net.Conn, bool, error) {
	if c := pre.take(network, addr); c != nil {
		return c, true, nil
	}
	if d == nil {
		d = &net.Dialer{}
	}
	c, err := d.DialContext(ctx, network, addr)
	return c, false, err
}

// DialTLS is tls.Dialer.DialContext over DialRaw: the same ServerName
// inference and handshake, on a connection that may have been pre-dialled.
func DialTLS(ctx context.Context, d *net.Dialer, network, addr string, cfg *tls.Config, pre *Predialed) (*tls.Conn, error) {
	// The dialer's timeout and deadline bound the handshake too, as they do
	// in tls.Dialer.
	if d != nil && d.Timeout != 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.Timeout)
		defer cancel()
	}
	if d != nil && !d.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, d.Deadline)
		defer cancel()
	}
	raw, wasPre, err := dialRaw(ctx, d, network, addr, pre)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		cfg = &tls.Config{}
	}
	// As crypto/tls does for a dial: with no ServerName, use the host part
	// of the address, as written.
	if cfg.ServerName == "" {
		colon := strings.LastIndex(addr, ":")
		if colon == -1 {
			colon = len(addr)
		}
		c := cfg.Clone()
		c.ServerName = addr[:colon]
		cfg = c
	}
	conn := tls.Client(raw, cfg)
	err = conn.HandshakeContext(ctx)
	if err != nil && wasPre && closedBeforeAnswering(err) {
		// The pre-dialled connection sat idle since the application's
		// connect, and the destination closed it just as the handshake
		// began. A dial made now — as it was before connections were
		// pre-dialled — gets a fresh connection.
		_ = raw.Close()
		if d == nil {
			d = &net.Dialer{}
		}
		if raw, err = d.DialContext(ctx, network, addr); err != nil {
			return nil, err
		}
		conn = tls.Client(raw, cfg)
		err = conn.HandshakeContext(ctx)
	}
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	return conn, nil
}

// closedBeforeAnswering reports whether a handshake failed because the
// destination had closed or reset the connection, not because of anything
// it answered.
func closedBeforeAnswering(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}
