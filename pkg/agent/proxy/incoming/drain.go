package proxy

import (
	"io"
	"net"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"

	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
)

// drainConn is the ingress's connection to the app, as a capture loop reads
// it: it reads the socket for its consumer as the connection itself would,
// counts what has arrived from the app (Arrived), and, as a client's going is
// seen and before the app is told of it, drains the socket (drain).
//
// The drain reads what the socket holds into keploy, without waiting and
// without forwarding it. Its consumer may have left much of an answer there:
// the zero-copy loop reads the app only as fast as it forwards, and the
// read-ahead stops at its bound. Past what the socket takes, the rest waits
// in the app's own send queue, held there by keploy's receive window, where
// no count of keploy's sees it. A read opens the window and the app's kernel
// sends on what it holds, so the drain reads on while the socket holds any:
// an answer the app wrote whole before its client went is read whole before
// the app is told, up to drainMax bytes. Every byte it reads, the app sent
// before it could learn of the going.
//
// Every read of the socket, the consumer's and the drain's, is made under mu
// and without waiting (proxyutil.ReadNow). A consumer that must wait for the
// app waits with mu let go (its RawConn's wait), so the drain never waits on
// it, and the bytes keep their order: what the drain read, the consumer's
// next reads return before any more of the socket.
//
// A conn that is not itself a socket (a wrapper), or a platform without these
// reads, is read as it is, and not drained.
type drainConn struct {
	net.Conn
	rc syscall.RawConn // the socket; nil: read as it is, no drain
	// base is what the socket had received before the wrap and was read
	// then, as proxyutil.Received counts; counted is whether that count
	// can be had.
	base    int64
	counted bool
	// moved is what has been read from the socket since the wrap, by the
	// consumer or the drain, with its close (io.EOF) as one more.
	moved   atomic.Int64
	eofSeen atomic.Bool

	mu      sync.Mutex
	held    []byte // read by the drain, not yet returned to the consumer
	heldErr error  // what ended the drain's reads, returned once held is
	charged int64  // what held is charged in aheadInAll
}

// drainMax is the most a drain holds of one connection (a var, so a test can
// lower it). The read-aheads' bound in all (aheadMaxInAll) holds too.
var drainMax int64 = 1 << 20

// socketReading, when set (tests only), runs as a Read goes to the socket,
// having found nothing the drain held: the instant a drain, and after it the
// socket's close or more of the app's bytes, can come in before the socket's
// read.
var socketReading func()

// newDrainConn wraps conn, the ingress's new connection to the app, before
// anything is read of it.
func newDrainConn(conn net.Conn) *drainConn {
	d := &drainConn{Conn: conn}
	rc, ok := proxyutil.OwnSocket(conn)
	if !ok {
		return d
	}
	d.rc = rc
	// What the socket holds unread first, then its count: what arrives in
	// between moves base up, and Arrived falls short by it, never over.
	unread := proxyutil.UnreadBytes(conn)
	if n, ok := proxyutil.Received(rc); ok {
		d.base, d.counted = n-unread, true
	}
	return d
}

// Arrived is what has arrived from the app since the wrap: its bytes, read or
// not, and its close as one more once it has come. Where the socket gives it,
// it is one count taken by the kernel at once (TCP_INFO), so it can only fall
// short of what has arrived (by what arrived as the conn was wrapped), never
// run past it. Elsewhere it is what has been read, then what the socket holds
// unread (SIOCINQ, Linux only), read in that order: a byte read in between is
// in neither, and a close is counted only once read. It may be read from any
// goroutine.
func (d *drainConn) Arrived() int64 {
	if d.counted {
		if n, ok := proxyutil.Received(d.rc); ok {
			return n - d.base
		}
	}
	n := d.moved.Load()
	return n + proxyutil.UnreadBytes(d.Conn)
}

// drainArrived drains the socket and then counts what has arrived: what a
// watch reads as it sees a client go, before it tells the app.
func (d *drainConn) drainArrived() int64 {
	d.drain()
	return d.Arrived()
}

// drain reads what the socket holds into held, without waiting, while it
// holds any, up to drainMax held and the read-aheads' bound in all. Called
// before the app is told of its client's going.
func (d *drainConn) drain() {
	if d.rc == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = d.rc.Control(func(fd uintptr) {
		for d.heldErr == nil {
			room := min(drainMax-int64(len(d.held)), aheadMaxInAll-aheadInAll.Load(), aheadRead)
			if room <= 0 {
				return
			}
			if cap(d.held)-len(d.held) < int(room) {
				d.held = slices.Grow(d.held, int(room))
				d.chargeLocked()
			}
			n, err, wait := proxyutil.ReadNow(fd, d.held[len(d.held):len(d.held)+int(room)])
			if wait {
				return
			}
			d.held = d.held[:len(d.held)+n]
			d.count(n, err)
			if err != nil {
				d.heldErr = d.readErr(err)
			} else if n == 0 {
				d.heldErr = io.EOF
			}
		}
	})
}

// Read returns what the drain holds first, then what the socket brings,
// waiting for it as the connection's own Read does.
func (d *drainConn) Read(p []byte) (int, error) {
	if d.rc == nil {
		n, err := d.Conn.Read(p)
		d.count(n, err)
		return n, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if n, err, ok := d.takeHeld(p); ok {
		return n, err
	}
	if socketReading != nil {
		socketReading()
	}
	var n int
	var err error
	served := false
	rerr := d.rc.Read(func(fd uintptr) bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		if len(d.held) > 0 || d.heldErr != nil { // a drain came in between
			n, err = d.takeHeldLocked(p)
			served = true
			return true
		}
		var wait bool
		n, err, wait = proxyutil.ReadNow(fd, p)
		if wait {
			return false
		}
		d.count(n, err)
		return true
	})
	if served {
		return n, err
	}
	if rerr != nil {
		// The socket was closed (the watch closes it on a client's
		// reset) or its deadline passed: what the drain read before is
		// still the app's.
		if n, err, ok := d.takeHeld(p); ok {
			return n, err
		}
		return 0, rerr
	}
	if err != nil {
		return n, d.readErr(err)
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// count counts a read of the socket: n bytes, and its end (n 0, no error) as
// one more, once.
func (d *drainConn) count(n int, err error) {
	if n > 0 {
		d.moved.Add(int64(n))
	}
	if (n == 0 && err == nil && d.rc != nil) || err == io.EOF {
		if d.eofSeen.CompareAndSwap(false, true) {
			d.moved.Add(1)
		}
	}
}

func (d *drainConn) takeHeld(p []byte) (int, error, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.held) == 0 && d.heldErr == nil {
		return 0, nil, false
	}
	n, err := d.takeHeldLocked(p)
	return n, err, true
}

// takeHeldLocked returns what the drain holds, then what ended its reads.
// Caller holds mu.
func (d *drainConn) takeHeldLocked(p []byte) (int, error) {
	if len(d.held) == 0 {
		return 0, d.heldErr
	}
	n := copy(p, d.held)
	d.held = d.held[n:]
	if len(d.held) == 0 {
		d.held = nil
		d.chargeLocked()
	}
	return n, nil
}

// chargeLocked charges held's buffer in aheadInAll. Caller holds mu.
func (d *drainConn) chargeLocked() {
	c := int64(cap(d.held))
	aheadInAll.Add(c - d.charged)
	d.charged = c
}

// release gives back what the drain holds, once the conn's reader is done
// with it. Idempotent.
func (d *drainConn) release() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.held = nil
	d.chargeLocked()
}

// readErr is a failed read of the socket as the connection's own Read gives
// it.
func (d *drainConn) readErr(err error) error {
	return &net.OpError{Op: "read", Net: d.LocalAddr().Network(), Source: d.LocalAddr(), Addr: d.RemoteAddr(), Err: os.NewSyscallError("read", err)}
}
