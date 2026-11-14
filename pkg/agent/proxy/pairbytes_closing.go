//go:build linux || darwin

package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// The close of a paired connection, on every platform whose proxy pairs them
// (Linux and macOS): the unread count (readUnread), the proxy's side
// (readProxySide), what the application had written as the close began
// (noteWroteAtClose), whether the proxy's side had taken the application's FIN
// or reset (peerEnded) and the data its count holds (dataReceived) are each
// platform's own; what a close with bytes unread, or on their way, charges is
// the same.

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
// Terminate or GOAWAY left unread still is one. Bytes the application had
// written before the close that are still on their way to the socket
// (noteWroteAtClose) are read through the layer the same way: a close_notify
// sent before the close is no loss either, and the read waits for it.
//
// Plaintext still on its way at the close is waited for the same way: no wait
// may hold the connection once the proxy is done with it, so the close itself
// waits for those bytes, by reading the socket (plainOnItsWay). The first byte
// of data that arrives is a loss; the application's FIN, or its reset, with
// none before it, is none. A plaintext close with only the application's FIN
// on its way reads nothing (finAloneOnItsWay): what ends a close's read is an
// event (data, the FIN, a reset or keploy's stop), never a clock, and a FIN
// held off would hold the close until the application's socket gives up on
// it. A bytes wait that holds the connection waits for that FIN instead.
//
// keploy's own stop (ctx, the proxy's connections' context, done) closes
// connections whatever they hold: nothing the application wrote is charged
// then, as nothing more of the run is judged. Nor is anything on a
// connection the proxy serves with no session (pairedConn.noSession): it
// captures nothing on it by design -- closed unanswered, as an agent not
// started in record or test mode closes it, or passed through -- and no test
// is open on it.
func (p *Proxy) pairClosing(ctx context.Context, conn net.Conn, tc *trackedConn) {
	pc, unread, err := p.paired.closing(conn, func(pc *pairedConn) {
		if ctx.Err() == nil && !pc.noSession.Load() {
			noteWroteAtClose(pc)
		}
	})
	tlsLayer := tc != nil && tc.tlsLayer.Load() != nil
	switch {
	case pc == nil, ctx.Err() != nil, pc.noSession.Load():
	case err != nil:
		p.paired.fail(fmt.Errorf("%w: read what the proxy's socket from %s held unread as the proxy closed it: %v", models.ErrProxyConnsUnsupported, pc.app, err))
		p.pairLost(pc, 1, lostTop(pc), "keploy cannot tell whether its proxy closed a connection with bytes the application wrote still unread: from now on it cannot vouch that every call of a test is in its hands", zap.Error(err))
	case unread == 0 && !onItsWayAtClose(pc):
	case unread == 0 && !tlsLayer && finAloneOnItsWay(pc):
		// The application's FIN alone: it carries no call, and a bytes
		// wait that holds the connection waits for it, as an orphan's FIN
		// (pairReached). The close does not wait for it.
	case unread == 0 && !tlsLayer:
		// Data may be on its way. Taken before the read: what the side
		// had received at the close, and what the application had
		// written by then.
		low, top := dataReceived(*pc.atClose.Load())+1, lostTop(pc)
		data, err := plainOnItsWay(ctx, pc.conn)
		ps := pc.readSide()
		pc.atClose.Store(&ps)
		switch {
		case ctx.Err() != nil:
		case err != nil:
			p.pairLost(pc, low, top, "keploy's proxy closed a connection with bytes the application wrote still on their way, and could not read whether they arrived: whether calls of the application's were among them cannot be told", zap.Error(err))
		case data:
			p.pairLost(pc, low, top, unreadLost)
		}
	case !tlsLayer:
		// The bytes tossed are the last unread of what its side had
		// received.
		low := uint64(1)
		if at := pc.atClose.Load(); at != nil && at.err == nil {
			low = dataReceived(*at) - min(uint64(unread), dataReceived(*at)) + 1
		}
		p.pairLost(pc, low, lostTop(pc), unreadLost)
	default:
		// The layer may hold bytes it took from the socket that its reader
		// never read: the bytes lost may begin anywhere (1).
		data, err := tlsUnreadData(ctx, pc.conn, tc.tlsLayer.Load())
		// Its side as the read left it: the application's FIN, if the read
		// waited for it, is in, which the wait counts (pairReached).
		ps := pc.readSide()
		pc.atClose.Store(&ps)
		switch {
		case ctx.Err() != nil:
		case err != nil:
			p.pairLost(pc, 1, lostTop(pc), "keploy's proxy closed a connection with bytes still unread that its TLS layer could not read: whether calls of the application's were among them cannot be told", zap.Error(err))
		case data:
			p.pairLost(pc, 1, lostTop(pc), unreadLost)
		}
	}
}

// onItsWayAtClose says whether, as the proxy began closing pc, bytes the
// application had written to it were still on their way: written, and not yet
// received by the proxy's side.
func onItsWayAtClose(pc *pairedConn) bool {
	w, ps := pc.wroteAtClose.Load(), pc.atClose.Load()
	return w != nil && ps != nil && ps.err == nil && w.want > ps.received
}

// finAloneOnItsWay says, of a close with bytes on their way (onItsWayAtClose,
// which it is asked only after), whether those were the application's FIN
// alone: one byte short of a count that surely holds the FIN (appWrote.fin).
// One byte short of a count that may not hold it may be a byte of data.
func finAloneOnItsWay(pc *pairedConn) bool {
	w, ps := pc.wroteAtClose.Load(), pc.atClose.Load()
	return w.fin && w.want == ps.received+1
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
	return unreadData(ctx, conn, layer)
}

// plainOnItsWay is how a close reads plaintext on its way (readPlainOnItsWay);
// a test replaces it to have keploy stop as the read returns.
var plainOnItsWay = readPlainOnItsWay

// readPlainOnItsWay reads conn, a plaintext connection the proxy is closing
// with nothing unread and bytes the application wrote before the close still
// on their way to it (onItsWayAtClose), until the first of them arrives (data:
// a loss, as unread bytes the close tosses are), or the application's FIN or
// reset ends it first (none), as tlsUnreadData reads through a TLS layer. A
// FIN comes after every byte written before it, so one with none before it
// says the bytes on their way were that FIN. The kernel takes a reset only
// at the sequence number it expects next, the application's snd_nxt (RFC
// 5961): every byte it had sent had arrived, and what its abort dropped
// unsent it would have dropped without keploy. The bytes on their way are
// sent, or dropped by such an abort: the read ends on an event, not a clock.
func readPlainOnItsWay(ctx context.Context, conn *net.TCPConn) (data bool, err error) {
	data, err = unreadData(ctx, conn, conn)
	if errors.Is(err, unix.ECONNRESET) {
		return false, nil
	}
	return data, err
}

// unreadData reads r, conn or a layer over it, for tlsUnreadData and
// plainOnItsWay, until the first byte of data (data), the end of what the
// application sent (none), or what cannot be read (an error).
func unreadData(ctx context.Context, conn *net.TCPConn, r io.Reader) (data bool, err error) {
	_ = conn.CloseWrite()
	_ = conn.SetReadDeadline(time.Time{})
	defer context.AfterFunc(ctx, func() { _ = conn.CloseRead() })()
	var b [1]byte
	var n int
	for {
		n, err = r.Read(b[:])
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
		return false, errors.New("the read gave nothing and said nothing")
	}
	return false, err
}

// lostTop is the last byte the application wrote to pc before the proxy's
// close of it, for a loss at that close: what its side had received when it
// had taken the application's FIN or reset by then (nothing is written after
// either), what the application had written as the close began
// (noteWroteAtClose: bytes written after it are no capture loss), and any
// byte when neither is known.
func lostTop(pc *pairedConn) uint64 {
	if at := pc.atClose.Load(); at != nil && at.err == nil && peerEnded(at.state) {
		return dataReceived(*at)
	}
	if w := pc.wroteAtClose.Load(); w != nil {
		return w.want
	}
	return math.MaxUint64
}

// unreadLost is pairLost's message for a close that tossed bytes unread.
const unreadLost = "keploy's proxy closed a connection with bytes the application wrote still unread: what they carried was neither recorded nor mocked"

// pairLost charges, once per connection, bytes the application wrote to pc
// that the proxy never captured, or that it cannot tell it did, saying which
// (msg): the bytes low..top, counted from 1 as the application wrote them. A
// test whose end was cut short with some of them unsettled owns its share
// (pairedConn.lostBy, TakeOwnedLosses): the loss is that test's, and the test
// open now is charged only when some were written after the last such end.
// With no end cut short on pc, they are the open test's.
func (p *Proxy) pairLost(pc *pairedConn, low, top uint64, msg string, fields ...zap.Field) {
	if pc.noSession.Load() || !pc.lost.CompareAndSwap(false, true) {
		return
	}
	owners, open := pc.lostBy(low, top)
	for _, test := range owners {
		p.paired.own(test, pc.app)
	}
	next := "keploy cannot vouch for the calls of the test that was open (under the proven switch, that test is not verified); check the agent's log for why the connection ended"
	if open {
		p.losses.Add(models.CaptureLossAppBytesLost, "from "+pc.app.String())
	} else {
		where := make([]string, len(owners))
		for i, test := range owners {
			where[i] = agent.WrittenIn(test)
		}
		next = "the bytes were written " + strings.Join(where, ", ") + ", not in the test open now: keploy's wait for them was cut short there (a test whose end was cut short is not verified); check the agent's log for why"
	}
	p.logger.Warn(msg, append([]zap.Field{
		zap.String("from", pc.app.String()),
		zap.Strings("owned_by", owners),
		zap.String("next_step", next),
	}, fields...)...)
}
