package generic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// timedOut is what a read whose deadline passed returns, without reading.
var timedOut = &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}

// until waits, a millisecond at a time, for cond to hold, at most 10 s: false
// when it never did.
func until(cond func() bool) bool {
	for end := time.Now().Add(10 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			return false
		}
	}
	return true
}

// heldPast is the proxy's end of a TCP connection whose parser is held past a
// request's window once it has read cut bytes of it: the next read waits
// until three windows have passed since the cut's read and the socket holds
// rest bytes unread, then times out without reading, as a read whose deadline
// passed does. The deadline the parser sets next has passed too, so the
// socket's own read times out alike. With more, the first read after the
// parser's look at what the socket holds (UnreadBytes, which asks through
// NetConn) and the deadline it sets then is held too, until the socket holds
// more than four reads (4 x ReadChunkSize) beyond what it held at that
// deadline (seen), then times out without reading: those bytes arrived as
// that read took. after counts the bytes read from there. idle counts the
// reads after the look, before the parser's next answer, that waited and
// timed out with nothing in the socket (the round after that answer waits
// on an empty socket for the next request, as it should; a read whose
// deadline had passed when it began did not wait), and early the reads
// before the parser's first answer (its first Write). With deadlineErr, no
// read deadline can be set once the parser looked; with pastOnly too, only
// one that has passed when it is set (the one the parser reads what the
// layers above the socket hold with). With eof, the read past the
// hold that empties the socket returns its bytes with io.EOF, as a read that
// meets the application's close does; reads after it return io.EOF, and
// deadlinesAfterEOF counts the read deadlines set after it. With wrote, the
// bytes the application has written, writtenAtMore is what it had written
// when the second hold ended. With piece, the reads past the hold go through
// a layer above the socket that reads ahead, as TLS does (readBeneath).
type heldPast struct {
	net.Conn
	cut, rest   int
	more        bool
	deadlineErr error
	pastOnly    bool
	eof         bool
	wrote       func() int64
	piece       int
	// layer: what the layer read of the socket and has not given yet; only
	// the parser's reads touch it.
	layer []byte

	mu sync.Mutex
	// phase: 0 to the cut, 1 the hold, 2 after it, 3 looked, 4 the second
	// hold, 5 after it.
	phase int
	read  int
	cutAt time.Time
	seen  int64
	after int
	idle  int
	early int
	// eofRead: the read that empties the socket returned io.EOF.
	eofRead           bool
	deadlinesAfterEOF int
	writtenAtMore     int64
	// deadline: the read deadline last set.
	deadline time.Time
	// answered: the parser has written its first answer; repliedAfterLook:
	// an answer since its look.
	answered, repliedAfterLook bool
}

func (c *heldPast) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.mu.Lock()
	c.answered = true
	if c.phase >= 3 {
		c.repliedAfterLook = true
	}
	c.mu.Unlock()
	return n, err
}

func (c *heldPast) NetConn() net.Conn {
	c.mu.Lock()
	if c.phase == 2 {
		c.phase = 3
	}
	c.mu.Unlock()
	return c.Conn
}

func (c *heldPast) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	refused := c.phase >= 3 && c.deadlineErr != nil && (!c.pastOnly || !t.After(time.Now()))
	if c.eofRead {
		c.deadlinesAfterEOF++
	}
	c.mu.Unlock()
	if refused {
		return c.deadlineErr
	}
	err := c.Conn.SetReadDeadline(t)
	c.mu.Lock()
	c.deadline = t
	if c.phase == 3 && c.more && t.After(time.Now()) {
		c.seen, c.phase = pUtil.UnreadBytes(c.Conn), 4
	}
	c.mu.Unlock()
	return err
}

func (c *heldPast) to(phase int) {
	c.mu.Lock()
	c.phase = phase
	c.mu.Unlock()
}

func (c *heldPast) Read(p []byte) (int, error) {
	c.mu.Lock()
	phase, cutAt, seen, eofRead := c.phase, c.cutAt, c.seen, c.eofRead
	if phase == 0 && len(p) > c.cut-c.read {
		p = p[:c.cut-c.read]
	}
	c.mu.Unlock()
	if eofRead {
		return 0, io.EOF
	}
	switch phase {
	case 1:
		if !until(func() bool {
			return time.Since(cutAt) > 3*quietWindow && pUtil.UnreadBytes(c.Conn) >= int64(c.rest)
		}) {
			return 0, errors.New("the rest never reached the socket")
		}
		c.to(2)
		return 0, timedOut
	case 4:
		if !until(func() bool { return pUtil.UnreadBytes(c.Conn) > seen+4*pUtil.ReadChunkSize }) {
			return 0, errors.New("the socket never held more")
		}
		c.mu.Lock()
		if c.wrote != nil {
			c.writtenAtMore = c.wrote()
		}
		c.phase = 5
		c.mu.Unlock()
		return 0, timedOut
	}
	c.mu.Lock()
	expired := !c.deadline.After(time.Now())
	c.mu.Unlock()
	n, err := c.readBeneath(p, phase)
	if c.eof && phase >= 2 && n > 0 && pUtil.UnreadBytes(c.Conn) == 0 {
		c.mu.Lock()
		c.eofRead = true
		c.mu.Unlock()
		err = io.EOF
	}
	var ne net.Error
	waited := !expired && errors.As(err, &ne) && ne.Timeout() && pUtil.UnreadBytes(c.Conn) == 0
	c.mu.Lock()
	if !c.answered {
		c.early++
	}
	switch c.phase {
	case 0:
		c.read += n
		if c.read == c.cut {
			c.phase, c.cutAt = 1, time.Now()
		}
	case 5:
		c.after += n
	}
	if c.phase >= 3 && waited && !c.repliedAfterLook {
		c.idle++
	}
	c.mu.Unlock()
	return n, err
}

// readBeneath is the read beneath the parser's. With piece, past the hold, it
// is a layer that reads ahead, as TLS does: while the layer holds bytes it
// gives at most piece of them a read without the socket, whatever the
// deadline, as TLS gives a whole record it read ahead; holding none, it
// reads all the socket holds, which times out on a deadline that passed.
// So once the socket has given what it held, the layer may hold most of the
// request.
func (c *heldPast) readBeneath(p []byte, phase int) (int, error) {
	if c.piece == 0 || phase < 2 {
		return c.Conn.Read(p)
	}
	if len(c.layer) == 0 {
		buf := make([]byte, 1<<20)
		n, err := c.Conn.Read(buf)
		if n == 0 {
			return 0, err
		}
		c.layer = buf[:n]
	}
	n := copy(p[:min(len(p), c.piece)], c.layer)
	c.layer = c.layer[n:]
	return n, nil
}

// A request whose parser is held past its window after it read part of it,
// the rest in the socket, is answered whole, and nothing more is waited for
// once the parser has read what the socket held when it looked. Held only
// past the round's deadline, the parser read on to the window's end; held
// past that too, it cut the request there, and the part it read missed.
// Through a layer above the socket that reads ahead, as TLS does, the parser
// reads what the layer holds once the socket has given what it held, every
// piece of it, not one.
func TestARequestWhoseParserIsHeldPastItsWindowIsAnsweredWhole(t *testing.T) {
	req := varied(40<<10, true)
	for name, store := range bigExchanges(req) {
		for _, cut := range []int{1000, 32 << 10, 40<<10 - 1} {
			for _, piece := range []int{0, 1 << 10} {
				t.Run(fmt.Sprintf("%s, cut at %d, %s", name, cut, map[bool]string{false: "tcp", true: "through a layer that reads ahead"}[piece > 0]), func(t *testing.T) {
					eachSwitch(t, func(t *testing.T) {
						app, proxySide := tcpPair(t)
						conn := &heldPast{Conn: proxySide, cut: cut, rest: len(req) - cut, piece: piece}
						done := make(chan error, 1)
						go func() {
							done <- decodeGeneric(context.Background(), zap.NewNop(), []byte("PING"), conn, nil, store, models.OutgoingOptions{})
						}()
						readAnswer(t, app, "PONG")
						if _, err := app.Write(req); err != nil {
							t.Fatal(err)
						}
						readAnswer(t, app, "BIGOK")
						conn.mu.Lock()
						idle, early := conn.idle, conn.early
						conn.mu.Unlock()
						if idle != 0 {
							t.Fatalf("the parser waited on an empty socket %d times after it read what the socket held", idle)
						}
						// PING, whole, is answered at its round's first timeout.
						if early != 1 {
							t.Fatalf("the parser read %d times before it answered a whole request; want 1", early)
						}
						_ = app.Close()
						awaitEnd(t, done, "a connection the application closed")
					})
				})
			}
		}
	}
}

// The application's close, read with the rest of a request as the parser
// reads on past the request's window, ends the parser once the request is
// answered: it sets no deadline and reads no more of a connection whose end
// it read, past its look as before it.
func TestAnEndReadAsTheParserReadsOnEndsTheParser(t *testing.T) {
	req := varied(40<<10, true)
	eachSwitch(t, func(t *testing.T) {
		app, proxySide := tcpPair(t)
		conn := &heldPast{Conn: proxySide, cut: 1000, rest: len(req) - 1000, eof: true}
		done := make(chan error, 1)
		go func() {
			done <- decodeGeneric(context.Background(), zap.NewNop(), []byte("PING"), conn, nil, bigExchanges(req)["recorded whole"], models.OutgoingOptions{})
		}()
		readAnswer(t, app, "PONG")
		if _, err := app.Write(req); err != nil {
			t.Fatal(err)
		}
		readAnswer(t, app, "BIGOK")
		awaitEnd(t, done, "a connection whose end the parser read")
		conn.mu.Lock()
		eofRead, after := conn.eofRead, conn.deadlinesAfterEOF
		conn.mu.Unlock()
		if !eofRead {
			t.Fatal("the parser never read the connection's end as it read on")
		}
		if after != 0 {
			t.Fatalf("the parser set %d read deadlines after it read the connection's end", after)
		}
	})
}

// Bytes that never pause end, though the parser was held past their window,
// and held again after it looked at what the socket holds: it reads on until
// the socket has given as many bytes as it held at the parser's first look,
// less what arrived as a read took (at most what the application wrote as
// the parser read on), within one read of them, and the request misses. A
// parser that looked again would read on to what the socket held then, and
// one that counted what arrived as its held read took, more than four reads
// of bytes, as making the request longer would read on through them.
func TestAHeldRequestWhoseBytesNeverPauseEndsAfterWhatTheSocketHeld(t *testing.T) {
	eachSwitch(t, func(t *testing.T) {
		app, proxySide := tcpPair(t)
		if tc, ok := proxySide.(*net.TCPConn); ok {
			_ = tc.SetReadBuffer(4 << 20)
		}
		var written atomic.Int64
		conn := &heldPast{Conn: proxySide, cut: 1000, rest: 1, more: true, wrote: written.Load}
		done := make(chan error, 1)
		go func() {
			done <- decodeGeneric(context.Background(), zap.NewNop(), []byte("PING"), conn, nil, bigStore(), models.OutgoingOptions{})
		}()
		readAnswer(t, app, "PONG")
		stop, wrote := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(wrote)
			chunk := bytes.Repeat([]byte("Z"), 2<<10)
			for {
				select {
				case <-stop:
					return
				default:
				}
				n, err := app.Write(chunk)
				written.Add(int64(n))
				if err != nil {
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()
		t.Cleanup(func() { close(stop); _ = app.Close(); <-wrote })
		awaitMiss(t, done)
		ended := written.Load()
		conn.mu.Lock()
		phase, after, seen, atMore := conn.phase, conn.after, conn.seen, conn.writtenAtMore
		conn.mu.Unlock()
		if phase != 5 {
			t.Fatalf("the parser's holds ended at phase %d; want both (5)", phase)
		}
		if during := ended - atMore; int64(after) > seen+during+pUtil.ReadChunkSize {
			t.Fatalf("the parser read %d bytes after its look; the socket held %d then, and %d were written as it read on", after, seen, during)
		}
	})
}

// heldAtARoundsEnd is the proxy's end of a TCP connection whose parser is held
// past a round's deadline before it read anything of a request: once the
// parser has answered (its first Write), the next read waits until the
// deadline then set has passed and the socket holds the request's first part
// (first bytes), then times out without reading. The next reads return that
// part in halves, so the socket still holds some of it as the first returns.
// The read after them waits until the socket holds the rest (rest bytes),
// which the row writes once that part was read (gotFirst).
type heldAtARoundsEnd struct {
	net.Conn
	first, rest int
	gotFirst    chan struct{}

	mu sync.Mutex
	// phase: 0 to the answer, 1 the hold, 2 to the first part, 3 the wait
	// for the rest, 4 after it.
	phase    int
	deadline time.Time
	read     int
}

func (c *heldAtARoundsEnd) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.mu.Lock()
	if c.phase == 0 {
		c.phase = 1
	}
	c.mu.Unlock()
	return n, err
}

// NetConn is the connection beneath, as the proxy's own wrappers give it, so
// the parser's look at what the socket holds sees through this one too.
func (c *heldAtARoundsEnd) NetConn() net.Conn { return c.Conn }

func (c *heldAtARoundsEnd) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return c.Conn.SetReadDeadline(t)
}

func (c *heldAtARoundsEnd) Read(p []byte) (int, error) {
	c.mu.Lock()
	phase, deadline := c.phase, c.deadline
	c.mu.Unlock()
	switch phase {
	case 1:
		if !until(func() bool {
			return time.Now().After(deadline) && pUtil.UnreadBytes(c.Conn) >= int64(c.first)
		}) {
			return 0, errors.New("the first part never reached the socket")
		}
		c.mu.Lock()
		c.phase = 2
		c.mu.Unlock()
		return 0, timedOut
	case 3:
		if !until(func() bool { return pUtil.UnreadBytes(c.Conn) >= int64(c.rest) }) {
			return 0, errors.New("the rest never reached the socket")
		}
		c.mu.Lock()
		c.phase = 4
		c.mu.Unlock()
	}
	if phase == 2 && len(p) > c.first/2 {
		p = p[:c.first/2]
	}
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	if c.phase == 2 {
		if c.read += n; c.read >= c.first {
			c.phase = 3
			close(c.gotFirst)
		}
	}
	c.mu.Unlock()
	return n, err
}

// A request the parser finds in the socket only once a round's deadline has
// passed, held before it read any of it, is read in the next round, from its
// first bytes, and answered whole though its rest comes later: the parser
// looks at what the socket holds only past a request's window, and a round
// that read nothing has none, nor does a read that returned bytes.
func TestARequestInTheSocketAtARoundsEndIsReadWhole(t *testing.T) {
	req := varied(40<<10, true)
	for name, store := range bigExchanges(req) {
		t.Run(name, func(t *testing.T) {
			eachSwitch(t, func(t *testing.T) {
				app, proxySide := tcpPair(t)
				conn := &heldAtARoundsEnd{Conn: proxySide, first: 1000, rest: len(req) - 1000, gotFirst: make(chan struct{})}
				done := make(chan error, 1)
				go func() {
					done <- decodeGeneric(context.Background(), zap.NewNop(), []byte("PING"), conn, nil, store, models.OutgoingOptions{})
				}()
				readAnswer(t, app, "PONG")
				if _, err := app.Write(req[:1000]); err != nil {
					t.Fatal(err)
				}
				select {
				case <-conn.gotFirst:
				case <-time.After(10 * time.Second):
					t.Fatal("the parser never read the request's first part")
				}
				if _, err := app.Write(req[1000:]); err != nil {
					t.Fatal(err)
				}
				readAnswer(t, app, "BIGOK")
				_ = app.Close()
				awaitEnd(t, done, "a connection the application closed")
			})
		})
	}
}

// A connection whose read deadline cannot be set as the parser reads on past
// a request's window, to what the socket held when it looked, ends the
// parser, the request unanswered and no miss filed: the deadline it reads
// on with, or only the one, already passed, it reads what the layers above
// the socket hold with once the socket has given what it held.
func TestADeadlineThatCannotBeSetPastTheWindowEndsTheParser(t *testing.T) {
	req := varied(40<<10, true)
	store := bigExchanges(req)["recorded whole"]
	for _, c := range []struct {
		name     string
		pastOnly bool
	}{
		{"any deadline after its look", false},
		{"only the deadline that has passed", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			eachSwitch(t, func(t *testing.T) {
				app, proxySide := tcpPair(t)
				conn := &heldPast{Conn: proxySide, cut: 1000, rest: len(req) - 1000, deadlineErr: errors.New("the deadline cannot be set"), pastOnly: c.pastOnly}
				done := make(chan error, 1)
				go func() {
					done <- decodeGeneric(context.Background(), zap.NewNop(), []byte("PING"), conn, nil, store, models.OutgoingOptions{})
				}()
				readAnswer(t, app, "PONG")
				if _, err := app.Write(req); err != nil {
					t.Fatal(err)
				}
				awaitEnd(t, done, "a connection whose deadline cannot be set")
			})
		})
	}
}
