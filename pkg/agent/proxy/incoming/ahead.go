package proxy

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// aheadReader reads a connection as fast as the app sends on it, ahead of its
// consumer, and remembers when each piece arrived. The synchronous ingress
// loop reads the app's response through it: the loop forwards the body as fast
// as the client takes it, and a read made only as the client takes the bytes
// would stamp a response's last byte with the client's pace, not the app's.
// LastReadTime is when the bytes the consumer last read came from the app (or
// when the app closed the connection, for a body that ends there): the end of
// the exchange's window, which must not run on into the requests made after
// the app finished just because its client is slow.
//
// What it holds is bounded in heap, not only in data: each piece is charged
// what its buffer takes plus aheadPieceCost, and a read is appended to the
// last piece while that stays within aheadRead (stamped with the later
// arrival, which moves no window's end earlier), so a stream of tiny writes to
// a stalled client makes few pieces. One aheadReader holds at most aheadMax,
// and all of them at most aheadMaxInAll, beyond the piece each is reading;
// past that it waits for its consumer, and the app for its client, as without
// it. So a body read whole by the time its client takes it is stamped with the
// app's pace, up to those bounds, and past them with the client's.
type aheadReader struct {
	conn     net.Conn
	lastNano atomic.Int64

	mu      sync.Mutex
	cond    *sync.Cond
	queue   []aheadPiece // arrived, not yet read by the consumer
	charged int64        // what queue is charged, in bytes
	err     error        // what ended the reads; with errAt, when
	errAt   time.Time
	closed  bool
}

// aheadPiece is what one or more reads brought, and when the last of them did.
type aheadPiece struct {
	data   []byte
	readAt time.Time
	// cost is what it is charged: its buffer's capacity when it was
	// allocated (a consumer's partial read frees none of it) plus
	// aheadPieceCost.
	cost int64
}

// The read-ahead's bounds (vars, so a test can lower them).
var (
	// aheadMax is what one aheadReader holds ahead of its consumer.
	aheadMax int64 = 256 << 10
	// aheadMaxInAll is what every aheadReader together holds ahead of their
	// consumers, beyond the piece each is reading.
	aheadMaxInAll int64 = 16 << 20
)

const (
	// aheadRead is the most one read from the connection takes, and the
	// size a piece grows to by appending reads.
	aheadRead = 32 << 10
	// aheadPieceCost is what a piece is charged beyond its buffer: its
	// place in the queue (twice, for the queue's growth) and the rounding
	// of a small allocation.
	aheadPieceCost = 128
)

// aheadInAll is what every aheadReader has queued is charged.
var aheadInAll atomic.Int64

// aheadReadSeen, if set, is told of every read an aheadReader made, once it is
// queued: a test's way to know the app's bytes (or its close) have arrived.
var aheadReadSeen atomic.Pointer[func(n int, err error)]

// aheadBufs are the readers' read buffers, aheadRead bytes each.
var aheadBufs = sync.Pool{New: func() any { b := make([]byte, aheadRead); return &b }}

// newAheadReader starts reading conn. Close it once done with it: that ends
// the reads (with the connection's own close) and gives back what is queued.
func newAheadReader(conn net.Conn) *aheadReader {
	a := &aheadReader{conn: conn}
	a.cond = sync.NewCond(&a.mu)
	go a.run()
	return a
}

func (a *aheadReader) run() {
	bp := aheadBufs.Get().(*[]byte)
	defer aheadBufs.Put(bp)
	buf := *bp
	for {
		a.mu.Lock()
		// One piece is always read, however much the others hold: the
		// consumer waits on it. Beyond that, the bounds.
		for !a.closed && len(a.queue) > 0 && (a.charged >= aheadMax || aheadInAll.Load() >= aheadMaxInAll) {
			a.cond.Wait()
		}
		if a.closed {
			a.mu.Unlock()
			return
		}
		a.mu.Unlock()

		n, err := a.conn.Read(buf)
		at := clockNow()

		a.mu.Lock()
		if n > 0 && !a.closed {
			a.queueLocked(buf[:n], at)
		}
		if err != nil {
			a.err, a.errAt = err, at
		}
		a.cond.Broadcast()
		a.mu.Unlock()
		if seen := aheadReadSeen.Load(); seen != nil {
			(*seen)(n, err)
		}
		if err != nil {
			return
		}
	}
}

// queueLocked adds what a read brought at at: to the last piece while it stays
// within aheadRead, else as a piece of its own. Caller holds mu.
func (a *aheadReader) queueLocked(b []byte, at time.Time) {
	if k := len(a.queue); k > 0 {
		if last := &a.queue[k-1]; len(last.data)+len(b) <= aheadRead {
			c0 := cap(last.data)
			last.data = append(last.data, b...)
			last.readAt = at
			if c1 := cap(last.data); c1 != c0 { // a new buffer: the old one is let go
				a.charge(last, int64(c1)+aheadPieceCost)
			}
			return
		}
	}
	a.queue = append(a.queue, aheadPiece{data: append([]byte(nil), b...), readAt: at})
	p := &a.queue[len(a.queue)-1]
	a.charge(p, int64(cap(p.data))+aheadPieceCost)
}

// charge sets what p is charged to cost. Caller holds mu.
func (a *aheadReader) charge(p *aheadPiece, cost int64) {
	d := cost - p.cost
	p.cost = cost
	a.charged += d
	aheadInAll.Add(d)
}

// Read returns what has arrived, in order, waiting for it if nothing has.
// After it, LastReadTime is when the bytes it returned arrived, or, at the end
// of the stream (io.EOF), when that was found.
func (a *aheadReader) Read(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for len(a.queue) == 0 && a.err == nil && !a.closed {
		a.cond.Wait()
	}
	if len(a.queue) > 0 {
		c := &a.queue[0]
		n := copy(p, c.data)
		a.lastNano.Store(c.readAt.UnixNano())
		c.data = c.data[n:]
		if len(c.data) == 0 {
			a.charge(c, 0)
			a.queue[0] = aheadPiece{}
			a.queue = a.queue[1:]
			if len(a.queue) == 0 {
				a.queue = nil // let go of the queue's array too
			}
			a.cond.Broadcast()
		}
		return n, nil
	}
	if a.closed {
		return 0, net.ErrClosed
	}
	if errors.Is(a.err, io.EOF) {
		a.lastNano.Store(a.errAt.UnixNano())
	}
	return 0, a.err
}

// LastReadTime returns when what the latest Read returned arrived from the
// connection (or when its end was found), the zero time before any.
func (a *aheadReader) LastReadTime() time.Time {
	n := a.lastNano.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// Close stops the reads (a read the connection is blocked in ends with the
// connection's close) and gives back what is queued. Idempotent.
func (a *aheadReader) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	a.closed = true
	aheadInAll.Add(-a.charged)
	a.queue, a.charged = nil, 0
	a.cond.Broadcast()
}
