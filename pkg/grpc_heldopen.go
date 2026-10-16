package pkg

import (
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"

	"golang.org/x/net/http2"
)

// pingWatch is the client end of the connection keploy replays a gRPC test
// on, under the proven switch. It passes every byte through unchanged and
// follows the frames both ways, to end the drain of a response the app keeps
// open past its recording (row W1): every recorded message came, and then the
// app sent a keepalive (a PING of its own, not the acknowledgement of one)
// beyond them, and no end. A stream held open sends nothing else, so its
// keepalives are what ends the wait, and no clock does.
//
// It also ends the drain of a stream that stalls short of its recording: two
// keepalives in a row with no message between them, the app neither ending
// the stream nor sending the rest of its answer (row W1, round 2). Without it
// a stream that sent fewer messages than recorded, then only keepalives,
// would be waited on for good. A count of the app's own keepalives decides,
// not a clock, so an app idle for two of its keepalive intervals before the
// rest of its answer is not judged.
//
// Not every PING an app sends is a keepalive. A gRPC server also pings to
// size its flow-control window (BDP): on the request's data, with at most one
// unanswered, so once the request's last frame (END_STREAM) has gone out the
// app sends at most one more of those. The first PING after it is therefore
// never taken for a keepalive; the second and later are.
//
// Everything is decided on the wire, in the order the app sent it: a message
// is before a PING when its last byte came before the PING's frame, whatever
// the drain has taken from the transport by then. A connection carries the
// one call.
type pingWatch struct {
	net.Conn

	// The client's frames (Write): its preface, then frames, followed to
	// the one that ends the request (ended).
	wmu      sync.Mutex
	wpreface int
	w        frameWalk
	ended    atomic.Bool

	// The app's frames (Read), on the transport's reader alone.
	r        frameWalk
	afterEnd int  // the app's PINGs since the request ended
	appEnded bool // the app ended the stream (END_STREAM, RST_STREAM)
	begun    bool // the app sent its response headers, and not its end
	// lastKeepalive is how many messages had come at the app's last
	// keepalive, -1 before its first.
	lastKeepalive int
	// The gRPC messages in the app's DATA: came are whole.
	padPending bool
	dataLeft   int
	pfx        [5]byte
	pfxN       int
	msgLeft    int
	came       int

	// complete is how many messages complete the recording; cut ends the
	// stream (arm). held says the app held it open, after heldAt messages
	// (written before held); received is how many the drain has had.
	armed    atomic.Bool
	complete int
	cut      func()
	held     atomic.Bool
	heldAt   int
	stalled  bool // held short of the recording (written before held)
	received atomic.Int64
}

// frameWalk follows HTTP/2 frames across reads or writes: a 9-byte header,
// then left bytes of payload.
type frameWalk struct {
	hdr   [9]byte
	hn    int
	left  int
	typ   http2.FrameType
	flags http2.Flags
}

// next takes b's bytes of one frame header or payload: header reports that it
// completed a header, payload is the payload bytes taken, rest what is left.
func (f *frameWalk) next(b []byte) (header bool, payload, rest []byte) {
	if f.left > 0 {
		k := min(f.left, len(b))
		f.left -= k
		return false, b[:k], b[k:]
	}
	k := copy(f.hdr[f.hn:], b)
	f.hn += k
	if f.hn < len(f.hdr) {
		return false, nil, b[k:]
	}
	f.hn = 0
	f.left = int(f.hdr[0])<<16 | int(f.hdr[1])<<8 | int(f.hdr[2])
	f.typ, f.flags = http2.FrameType(f.hdr[3]), http2.Flags(f.hdr[4])
	return true, nil, b[k:]
}

func newPingWatch(c net.Conn) *pingWatch {
	return &pingWatch{Conn: c, lastKeepalive: -1}
}

// arm starts the watch: once complete response messages have come, the app's
// next keepalive calls cut and marks the stream held open.
func (w *pingWatch) arm(complete int, cut func()) {
	w.complete = complete
	w.cut = cut
	w.armed.Store(true)
}

// heldOpen reports whether the app held the stream open, after how many
// messages (the drain has them all when it does), and whether short of its
// recording: stalled.
func (w *pingWatch) heldOpen() (held bool, at int, stalled bool) {
	if w == nil || !w.held.Load() {
		return false, 0, false
	}
	return true, w.heldAt, w.stalled
}

// gotMessage tells the watch the drain has had n messages, and reports
// whether the drain is done: the app held the stream open after n.
//
// The app's frames are read before the transport hands their messages on, so
// the keepalive that holds a stream open may be read before the drain has
// the messages that came ahead of it. Whichever of the two comes second ends
// the drain: the drain itself, here, leaving the stream (SimulateGRPC then
// cuts it), or the keepalive (ping), which cuts the stream the drain waits
// on when it already had them all. Each side stores its own before it loads
// the other's, so one of them always sees both.
func (w *pingWatch) gotMessage(n int) bool {
	w.received.Store(int64(n))
	return w.held.Load() && n >= w.heldAt
}

func (w *pingWatch) Read(p []byte) (int, error) {
	n, err := w.Conn.Read(p)
	w.scanRead(p[:n])
	return n, err
}

func (w *pingWatch) Write(p []byte) (int, error) {
	// What the request ends with is marked before it goes out, so a PING
	// the app sends on it is read after the mark.
	w.scanWrite(p)
	return w.Conn.Write(p)
}

// scanWrite follows the client's frames and marks the one that ends the
// request: DATA or HEADERS with END_STREAM. The client writes from one
// goroutine at a time (grpc-go's writer); the lock keeps the walk whole if
// not.
func (w *pingWatch) scanWrite(b []byte) {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	if w.wpreface < len(http2.ClientPreface) {
		k := min(len(http2.ClientPreface)-w.wpreface, len(b))
		w.wpreface += k
		b = b[k:]
	}
	for len(b) > 0 {
		var header bool
		header, _, b = w.w.next(b)
		if header && endsStream(w.w.typ, w.w.flags) {
			w.ended.Store(true)
		}
	}
}

// scanRead follows the app's frames (a server sends no preface string: its
// SETTINGS frame comes first): its messages, its end, and its PINGs.
func (w *pingWatch) scanRead(b []byte) {
	for len(b) > 0 {
		var header bool
		var payload []byte
		header, payload, b = w.r.next(b)
		if !header {
			if w.r.typ == http2.FrameData {
				w.data(payload)
			}
			continue
		}
		switch {
		case w.r.typ == http2.FrameData:
			w.padPending = w.r.flags.Has(http2.FlagDataPadded)
			w.dataLeft = w.r.left
		case w.r.typ == http2.FramePing && !w.r.flags.Has(http2.FlagPingAck):
			w.ping()
		case w.r.typ == http2.FrameRSTStream:
			w.appEnded = true
		case w.r.typ == http2.FrameHeaders && !w.r.flags.Has(http2.FlagHeadersEndStream):
			w.begun = true
		}
		if endsStream(w.r.typ, w.r.flags) {
			// Its payload, the stream's last messages, still counts.
			w.appEnded = true
		}
	}
}

// data takes payload bytes of the app's DATA frame: a padded frame starts
// with its padding's length, and ends with the padding; what is between is
// gRPC messages, each a 5-byte prefix (its length) and that many bytes.
func (w *pingWatch) data(p []byte) {
	if w.padPending && len(p) > 0 {
		w.padPending = false
		w.dataLeft -= 1 + int(p[0])
		p = p[1:]
	}
	if len(p) > w.dataLeft {
		p = p[:max(w.dataLeft, 0)]
	}
	w.dataLeft -= len(p)
	for len(p) > 0 {
		if w.pfxN < len(w.pfx) {
			k := copy(w.pfx[w.pfxN:], p)
			w.pfxN += k
			p = p[k:]
			if w.pfxN == len(w.pfx) {
				w.msgLeft = int(binary.BigEndian.Uint32(w.pfx[1:]))
				if w.msgLeft == 0 {
					w.came++
					w.pfxN = 0
				}
			}
			continue
		}
		k := min(w.msgLeft, len(p))
		w.msgLeft -= k
		p = p[k:]
		if w.msgLeft == 0 {
			w.came++
			w.pfxN = 0
		}
	}
}

// ping is a PING of the app's: after the request's end, the first may be the
// app's last flow-control ping, and a later one is a keepalive. On a stream
// the app has not ended, a keepalive holds it open when every recorded
// message has come and the app began its answer (its headers, for a
// recording of none), or when the keepalive before it saw the same messages:
// the app stalled short of its recording. What the app sends after it is not
// waited for, and does not count.
func (w *pingWatch) ping() {
	if !w.ended.Load() {
		return
	}
	w.afterEnd++
	if w.afterEnd < 2 {
		return
	}
	prev := w.lastKeepalive
	w.lastKeepalive = w.came
	if w.appEnded || !w.armed.Load() || w.held.Load() {
		return
	}
	switch {
	case w.came >= w.complete && (w.begun || w.came > 0):
	case prev == w.came:
		w.stalled = true
	default:
		return
	}
	w.heldAt = w.came
	w.held.Store(true)
	if w.received.Load() >= int64(w.heldAt) {
		// Not on the transport's read: the cut cancels the stream, and
		// the transport goes on reading.
		go w.cut()
	}
}

func endsStream(typ http2.FrameType, flags http2.Flags) bool {
	return typ == http2.FrameData && flags.Has(http2.FlagDataEndStream) ||
		typ == http2.FrameHeaders && flags.Has(http2.FlagHeadersEndStream)
}
