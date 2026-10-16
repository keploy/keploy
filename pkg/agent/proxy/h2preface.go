package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	"golang.org/x/net/http2"
)

// Deciding gRPC from h2c by bytes (release-1 row W3). An HTTP/2 client
// connection starts with the client preface and its SETTINGS. The protocol
// matchers tell gRPC from h2c by the content-type of the first request's
// HEADERS, which a client may send at once (h2c clients usually do) or only
// once it has the server's connection preface (a gRPC client waits for the
// server's SETTINGS). On replay keploy is the server, so it answers the
// client preface with its own: then every client sends its HEADERS, and the
// connection is read until they arrive, however long the client takes. No
// silence decides anything.
//
// The parser the connection is handed to answers as a server too: it sends
// its own SETTINGS and acknowledges the client's. The client acknowledges
// both servers' SETTINGS, and the parser must not see the acknowledgement of
// keploy's, which it never sent (Go's HTTP/2 server hangs up on a SETTINGS
// ACK it did not ask for): that one frame is taken out of the stream, before
// the hand-off or after it (ackFilter). A PING the client sends while keploy
// waits for HEADERS is answered by keploy, and taken out too.

const (
	h2FrameHeaderLen = 9
	h2FrameHeaders   = byte(http2.FrameHeaders)
	h2FrameSettings  = byte(http2.FrameSettings)
	h2FramePing      = byte(http2.FramePing)
	h2FlagAck        = byte(http2.FlagSettingsAck)
)

// h2ServerPreface is a server's connection preface (RFC 9113 §3.4): a
// SETTINGS frame with no parameters, every setting at its default.
var h2ServerPreface = []byte{0, 0, 0, h2FrameSettings, 0, 0, 0, 0, 0}

// h2FrameLen is the length of the frame whose header starts hdr (at least
// h2FrameHeaderLen bytes), header included.
func h2FrameLen(hdr []byte) int {
	return h2FrameHeaderLen + (int(hdr[0])<<16 | int(hdr[1])<<8 | int(hdr[2]))
}

// isSettingsAck reports whether the frame header hdr is a SETTINGS ACK.
func isSettingsAck(hdr []byte) bool {
	return hdr[3] == h2FrameSettings && hdr[4]&h2FlagAck != 0
}

// splitH2Frames splits buf, which starts with the client preface, into the
// preface and its complete frames (whole) and the incomplete frame after them
// (part, a copy).
func splitH2Frames(buf []byte) (whole, part []byte) {
	pos := len(http2.ClientPreface)
	for len(buf)-pos >= h2FrameHeaderLen {
		n := h2FrameLen(buf[pos:])
		if len(buf)-pos < n {
			break
		}
		pos += n
	}
	return buf[:pos], append([]byte(nil), buf[pos:]...)
}

// readHTTP2UntilHeaders answers an HTTP/2 client connection's preface with
// the server's (h2ServerPreface) and reads it until its first HEADERS frame
// has arrived. initialBuf is what was read of it so far: the client preface,
// and no HEADERS frame. It returns the bytes the protocol matchers and the
// parser read first, every frame up to and including that HEADERS frame and
// whatever followed it in the last read, and the connection to read the rest
// from. Neither holds the client's acknowledgement of keploy's SETTINGS, nor a
// PING keploy answered. A connection the client closes before its HEADERS
// returns what was read, without an error: the matchers decide on it as they
// would have. One ctx ends while it waits returns ctx's error: the read is
// cut short by a deadline already past, set when ctx ends, never by a budget.
func readHTTP2UntilHeaders(ctx context.Context, conn net.Conn, initialBuf []byte) ([]byte, net.Conn, error) {
	if _, err := conn.Write(h2ServerPreface); err != nil {
		return initialBuf, conn, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetReadDeadline(time.Unix(1, 0)) })
	defer func() {
		if !stop() {
			// ctx ended: the read deadline it set must not outlive this read.
			_ = conn.SetReadDeadline(time.Time{})
		}
	}()
	whole, part := splitH2Frames(initialBuf)
	out := append([]byte(nil), whole...)
	ackDropped, sawHeaders := false, false
	chunk := make([]byte, 16<<10)
	for !sawHeaders {
		n, rerr := conn.Read(chunk)
		part = append(part, chunk[:n]...)
		for len(part) >= h2FrameHeaderLen && len(part) >= h2FrameLen(part) {
			f := part[:h2FrameLen(part)]
			switch {
			case !ackDropped && isSettingsAck(f):
				// The acknowledgement of keploy's SETTINGS.
				ackDropped = true
			case !sawHeaders && f[3] == h2FramePing && f[4]&h2FlagAck == 0 && len(f) == h2FrameHeaderLen+8:
				// Answered here: the parser never sees it.
				ack := append([]byte{0, 0, 8, h2FramePing, h2FlagAck, 0, 0, 0, 0}, f[h2FrameHeaderLen:]...)
				if _, err := conn.Write(ack); err != nil {
					return append(out, part...), conn, err
				}
			default:
				sawHeaders = sawHeaders || f[3] == h2FrameHeaders
				out = append(out, f...)
			}
			part = part[len(f):]
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return append(out, part...), conn, ctx.Err()
			}
			if sawHeaders || errors.Is(rerr, io.EOF) {
				break
			}
			return append(out, part...), conn, rerr
		}
	}
	buf := append(out, part...)
	if ackDropped {
		return buf, conn, nil
	}
	return buf, newAckFilter(conn, part), nil
}

// ackFilter is a client connection that keploy answered with its own SETTINGS
// (readHTTP2UntilHeaders) before the client acknowledged them: it reads the
// client's frames as they come and takes the first SETTINGS ACK out of the
// stream, then passes every byte through.
type ackFilter struct {
	net.Conn
	done bool
	// hdr holds the header of the frame being read, hn bytes of it;
	// emitted says whether those bytes were passed on already (the frame
	// was started before the filter, so it is no acknowledgement of
	// keploy's).
	hdr     [h2FrameHeaderLen]byte
	hn      int
	emitted bool
	// need is how many bytes of the current frame's payload are left to
	// pass on.
	need int
	// out holds bytes ready for the reader that did not fit its buffer.
	out     []byte
	scratch []byte
}

// newAckFilter filters conn, whose stream continues the incomplete frame part
// already passed on.
func newAckFilter(conn net.Conn, part []byte) *ackFilter {
	f := &ackFilter{Conn: conn, scratch: make([]byte, 16<<10)}
	switch {
	case len(part) >= h2FrameHeaderLen:
		f.need = h2FrameLen(part) - len(part)
	case len(part) > 0:
		f.hn = copy(f.hdr[:], part)
		f.emitted = true
	}
	return f
}

func (f *ackFilter) Read(p []byte) (int, error) {
	for len(f.out) == 0 {
		if f.done {
			return f.Conn.Read(p)
		}
		n, err := f.Conn.Read(f.scratch)
		f.filter(f.scratch[:n])
		if err != nil {
			if len(f.out) == 0 {
				return 0, err
			}
			break
		}
	}
	n := copy(p, f.out)
	f.out = f.out[n:]
	return n, nil
}

// filter appends to f.out what of b the reader reads.
func (f *ackFilter) filter(b []byte) {
	for len(b) > 0 {
		if f.done {
			f.out = append(f.out, b...)
			return
		}
		if f.need > 0 {
			k := min(f.need, len(b))
			f.out = append(f.out, b[:k]...)
			f.need -= k
			b = b[k:]
			continue
		}
		k := copy(f.hdr[f.hn:], b)
		if f.emitted {
			f.out = append(f.out, b[:k]...)
		}
		f.hn += k
		b = b[k:]
		if f.hn < h2FrameHeaderLen {
			return
		}
		switch {
		case !f.emitted && isSettingsAck(f.hdr[:]):
			// The acknowledgement of keploy's SETTINGS: dropped, and
			// nothing more to filter.
			f.done = true
		case !f.emitted:
			f.out = append(f.out, f.hdr[:]...)
			f.need = h2FrameLen(f.hdr[:]) - h2FrameHeaderLen
		default:
			f.need = h2FrameLen(f.hdr[:]) - h2FrameHeaderLen
		}
		f.hn, f.emitted = 0, false
	}
}
