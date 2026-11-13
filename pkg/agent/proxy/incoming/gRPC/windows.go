package proxy

import (
	"encoding/binary"
	"sync"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"golang.org/x/net/http2"
)

// callWindows holds the windows (syncMock.OpenWindow) of one connection's gRPC
// calls by stream id: each opens as its request's first frame is read, before
// the app can read it (started), and holds every per-test mock the app makes
// from then on that may be the call's, so no reaper drops one while the call
// is in flight (the stale cutoff another request's verdict runs, a duplicate's
// prune). Its capture takes it (take) and decides it as the HTTP/1 capture
// decides a request's (conn.CaptureGRPC); a reset ends it (reset), and the
// connection's end, or a parser that gives up on it, ends what is left (stop).
type callWindows struct {
	mgr *syncMock.SyncMockManager
	// starts follows the client's frames; only its forwarder's goroutine
	// touches it (started).
	starts startScanner
	mu     sync.Mutex
	open   map[uint32]*syncMock.Window
	// stopped: the connection's calls are all handled, or its parser gave
	// up on it; no window opens any more. Under mu.
	stopped bool
}

func newCallWindows(mgr *syncMock.SyncMockManager) *callWindows {
	return &callWindows{mgr: mgr, open: make(map[uint32]*syncMock.Window)}
}

// started opens, at, the window of each call whose request b, the client's
// next bytes, opens: at is b's stamp, taken before b is written to the app.
func (c *callWindows) started(b []byte, at time.Time) {
	for _, id := range c.starts.scan(b) {
		w := c.mgr.OpenWindow(at, nil)
		c.mu.Lock()
		if c.stopped {
			c.mu.Unlock()
			w.Close()
			continue
		}
		c.open[id] = w
		c.mu.Unlock()
	}
}

// take hands the window of stream id's call to its capture, which decides it;
// nil when it has none (a reset ended it, or none opened).
func (c *callWindows) take(id uint32) *syncMock.Window {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.open[id]
	delete(c.open, id)
	return w
}

// reset ends the window of stream id's call, which was reset before it was
// complete: no test case is built of it.
func (c *callWindows) reset(id uint32) {
	c.take(id).Close()
}

// stop ends every window still open and opens none from then on.
func (c *callWindows) stop() {
	c.mu.Lock()
	c.stopped = true
	open := c.open
	c.open = make(map[uint32]*syncMock.Window)
	c.mu.Unlock()
	for _, w := range open {
		w.Close()
	}
}

// startScanner follows the HTTP/2 frames a client sends its app, from the
// connection's preface on, across the reads they arrive in, and finds each
// HEADERS frame that opens a stream: one on a stream id above every stream it
// opened before. A client opens its streams in increasing order (RFC 9113
// §5.1.1); a HEADERS frame on a stream it opened already is that request's
// trailers. A frame opens its call where its header is complete: the app
// cannot read the call's request before that.
type startScanner struct {
	preface int // bytes of the client's preface read
	hdr     [frameHeaderSize]byte
	have    int    // bytes held of the header of the frame in progress
	left    int    // bytes left of the payload of the frame in progress
	last    uint32 // the highest stream id a HEADERS frame opened
}

// scan follows b, the client's next bytes, and returns, in order, the ids of
// the streams the HEADERS frames whose header b completes open.
func (s *startScanner) scan(b []byte) []uint32 {
	var out []uint32
	if k := min(len(http2.ClientPreface)-s.preface, len(b)); k > 0 {
		s.preface += k
		b = b[k:]
	}
	for i := 0; i < len(b); {
		if s.have < frameHeaderSize {
			k := copy(s.hdr[s.have:], b[i:])
			s.have += k
			i += k
			if s.have < frameHeaderSize {
				break
			}
			s.left = int(s.hdr[0])<<16 | int(s.hdr[1])<<8 | int(s.hdr[2])
			id := binary.BigEndian.Uint32(s.hdr[5:]) & (1<<31 - 1)
			if http2.FrameType(s.hdr[3]) == http2.FrameHeaders && id > s.last {
				s.last = id
				out = append(out, id)
			}
		}
		k := min(s.left, len(b)-i)
		s.left -= k
		i += k
		if s.left == 0 {
			s.have = 0
		}
	}
	return out
}
