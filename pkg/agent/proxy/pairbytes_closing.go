//go:build linux || darwin

package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
)

// The close of a paired connection, on every platform whose proxy pairs them
// (Linux and macOS): the unread count (readUnread) and the proxy's side
// (readProxySide) are each platform's own; what a close with bytes unread
// charges is the same.

// pairClosing is a paired connection's beforeClose: its side as the proxy
// closes it (closing), and a capture loss when the close tosses bytes the
// application wrote that the proxy never read. Its side counts those
// received, so no wait would find them missing. Bytes held that cannot be
// counted leave every wait unknown, and are a loss of this connection's
// whether or not a wait holds it.
//
// Over TLS (tc's layer) what is unread is read through the layer first: a
// TLS client's goodbye ends with a close_notify -- go-sql-driver/mysql's
// Close (COM_QUIT, then close_notify, then FIN); pgx's Terminate and gRPC's
// GOAWAY once the handler has read them -- which a handler done with the
// connection never reads, and which carries no call. The socket's count
// cannot tell it from data; only plaintext is a loss (tlsUnreadData), so a
// Terminate or GOAWAY left unread still is one.
//
// keploy's own stop (ctx, the proxy's connections' context, done) closes
// connections whatever they hold: nothing the application wrote is charged
// then, as nothing more of the run is judged. Nor is anything on a
// connection the proxy serves with no session (pairedConn.noSession): it
// captures nothing on it by design -- closed unanswered, as an agent not
// started in record or test mode closes it, or passed through -- and no test
// is open on it.
func (p *Proxy) pairClosing(ctx context.Context, conn net.Conn, tc *trackedConn) {
	pc, unread, err := p.paired.closing(conn)
	switch {
	case pc == nil, ctx.Err() != nil, pc.noSession.Load():
	case err != nil:
		p.paired.fail(fmt.Errorf("%w: read what the proxy's socket from %s held unread as the proxy closed it: %v", models.ErrProxyConnsUnsupported, pc.app, err))
		p.pairLost(pc, "keploy cannot tell whether its proxy closed a connection with bytes the application wrote still unread: from now on it cannot vouch that every call of a test is in its hands", zap.Error(err))
	case unread == 0:
	case tc == nil || tc.tlsLayer.Load() == nil:
		p.pairLost(pc, unreadLost)
	default:
		data, err := tlsUnreadData(ctx, pc.conn, tc.tlsLayer.Load())
		// Its side as the read left it: the application's FIN, if the read
		// waited for it, is in, which the wait counts (pairReached).
		ps := readProxySide(pc.conn)
		pc.atClose.Store(&ps)
		switch {
		case ctx.Err() != nil:
		case err != nil:
			p.pairLost(pc, "keploy's proxy closed a connection with bytes still unread that its TLS layer could not read: whether calls of the application's were among them cannot be told", zap.Error(err))
		case data:
			p.pairLost(pc, unreadLost)
		}
	}
}

// tlsUnreadData reads, through layer, the TLS layer the proxy serves conn
// over, what conn's socket still holds as the proxy closes it, and reports
// whether any of it is data (plaintext). It reads until the first byte of data
// (data), the close_notify or the application's FIN (none), or what the layer
// cannot read (an error: the bytes cannot be told to be no data). It never
// reads past the first byte of data: nothing more is needed to charge the
// loss.
//
// Its write side is shut down first: the application, told the proxy is done
// (its close_notify, if the layer's close sent one, then its FIN), ends its
// own side, so a read that waits -- a record not all in yet -- ends at the
// application's FIN, not on a clock. A read deadline its handler left (a
// poll's, or a nudge's in the past) is cleared, and so is one another reader
// of the connection sets while the read waits: either would end the read at
// once, or on a clock, as a loss. If that FIN never comes, the read holds the
// close until keploy's stop (ctx) ends it for good by shutting the read side
// down, which no deadline another reader of the connection sets or clears
// undoes; the stop charges nothing.
func tlsUnreadData(ctx context.Context, conn *net.TCPConn, layer *tls.Conn) (data bool, err error) {
	_ = conn.CloseWrite()
	_ = conn.SetReadDeadline(time.Time{})
	defer context.AfterFunc(ctx, func() { _ = conn.CloseRead() })()
	var b [1]byte
	var n int
	for {
		n, err = layer.Read(b[:])
		// A deadline another reader of the connection set since (a
		// parser's poll, re-armed before its next read, which waits behind
		// this one) is no end of this read either: only the application's
		// end or keploy's stop is. The layer keeps a record a timeout cut
		// short.
		if n == 0 && errors.Is(err, os.ErrDeadlineExceeded) && ctx.Err() == nil {
			_ = conn.SetReadDeadline(time.Time{})
			continue
		}
		break
	}
	switch {
	case n > 0:
		return true, nil
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		// The close_notify, or the application's FIN, at a record's end or
		// in one's middle: the application ended there.
		return false, nil
	case err == nil:
		return false, errors.New("the TLS layer read nothing and said nothing")
	}
	return false, err
}

// unreadLost is pairLost's message for a close that tossed bytes unread.
const unreadLost = "keploy's proxy closed a connection with bytes the application wrote still unread: what they carried was neither recorded nor mocked"

// pairLost charges, once per connection, bytes the application wrote to pc
// that the proxy never captured, or that it cannot tell it did, saying which
// (msg).
func (p *Proxy) pairLost(pc *pairedConn, msg string, fields ...zap.Field) {
	if pc.noSession.Load() || !pc.lost.CompareAndSwap(false, true) {
		return
	}
	p.losses.Add(models.CaptureLossAppBytesLost, "from "+pc.app.String())
	p.logger.Warn(msg, append([]zap.Field{
		zap.String("from", pc.app.String()),
		zap.String("next_step", "keploy cannot vouch for the calls of the test that was open (under the proven switch, it and every later test are not verified); check the agent's log for why the connection ended"),
	}, fields...)...)
}
