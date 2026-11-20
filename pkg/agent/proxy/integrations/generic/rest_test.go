package generic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// awaitMiss fails the test unless decodeGeneric (done) ends with a miss
// within 10 s.
func awaitMiss(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if !errors.Is(err, models.ErrNoMockMatched) {
			t.Fatalf("the parser ended with %v; want a miss", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the parser never ended the request")
	}
}

// bigExchanges is the store of the long-request rows: PING answered with
// PONG, and req answered with BIGOK, recorded whole or in two pieces cut at
// 32 KiB, as the relay reads.
func bigExchanges(req []byte) map[string]eofStore {
	out := map[string]eofStore{}
	for name, recorded := range map[string]*models.Mock{
		"recorded whole":     recordedAs("mock-big", req),
		"recorded in pieces": recordedAs("mock-big", req[:32<<10], req[32<<10:]),
	} {
		recorded.TestModeInfo.IsFiltered = true
		recorded.Spec.GenericResponses = []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: "BIGOK"}}}}
		out[name] = eofStore{mocks: []*models.Mock{recordedExchange("mock-ping", "PING", "PONG"), recorded}}
	}
	return out
}

// faults is the proxy's end of a connection that breaks once left bytes
// have been read, the reads before returning at most what is left. With
// expire its read deadline passes there: the parser was held past it while
// the rest of the request sat in the socket, and every read times out, as a
// TCP connection's does once its deadline passed, until the parser sets
// another deadline. From there its reads fail with readErr, and its read
// deadlines cannot be set (deadlineErr; with deadlineOnce only the first
// one, the deadline the parser reads on to). With rearmed, the row sends the
// rest of the request only once the parser sets that deadline (rearmed is
// closed then), and the parser's next read waits until the socket holds it
// (rest bytes), so the rest comes in the request's window however long the
// parser takes to read it. It gives the connection beneath (NetConn), as
// the proxy's own wrappers do: the parser looks at what its socket holds.
// It gives the request's first bytes only once the clock has moved past
// the start of the round that reads them, so they come late in the round on
// any clock: on one that moves in steps (Windows' moves about every 2 ms),
// bytes read just as a round begins share its start's reading, and their
// window is the round's.
type faults struct {
	net.Conn
	mu           sync.Mutex
	left         int
	expire       bool
	readErr      error
	deadlineErr  error
	deadlineOnce bool
	rearmed      chan struct{}
	rest         int
	reached      bool
	expired      bool
	waiting      bool
	began        bool
	setAt        time.Time
}

func (c *faults) NetConn() net.Conn { return c.Conn }

func (c *faults) Read(p []byte) (int, error) {
	c.mu.Lock()
	reached, expired, waiting, left, began, setAt := c.reached, c.expired, c.waiting, c.left, c.began, c.setAt
	c.mu.Unlock()
	if !began && !expired && !reached && !until(func() bool { return time.Now().After(setAt) }) {
		return 0, errors.New("the clock never moved past the round's start")
	}
	switch {
	case expired:
		return 0, timedOut
	case reached && c.readErr != nil:
		return 0, c.readErr
	case waiting:
		if !until(func() bool { return pUtil.UnreadBytes(c.Conn) >= int64(c.rest) }) {
			return 0, errors.New("the rest never reached the socket")
		}
		c.mu.Lock()
		c.waiting = false
		c.mu.Unlock()
	case !reached && len(p) > left:
		p = p[:left]
	}
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	c.began = c.began || n > 0
	if !c.reached {
		c.left -= n
		c.reached = c.left == 0
		c.expired = c.reached && c.expire
	}
	c.mu.Unlock()
	return n, err
}

func (c *faults) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	if c.reached && c.deadlineErr != nil {
		err := c.deadlineErr
		if c.deadlineOnce {
			c.deadlineErr = nil
		}
		c.mu.Unlock()
		return err
	}
	if c.expired && c.rearmed != nil {
		c.waiting = true
		close(c.rearmed)
	}
	c.expired = false
	c.setAt = time.Now()
	c.mu.Unlock()
	return c.Conn.SetReadDeadline(t)
}

// awaitReadOn waits until the parser sets the deadline it reads on to
// (rearmed), and fails the test if it ends first: it ended the request at
// the deadline that passed, before the rest came.
func awaitReadOn(t *testing.T, rearmed <-chan struct{}, done <-chan error) {
	t.Helper()
	select {
	case <-rearmed:
	case err := <-done:
		t.Fatalf("the parser ended with %v before it read on to the request's window", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the parser never read on to the request's window")
	}
}

// A request longer than one read whose round's deadline passes between two
// of its reads, the rest already in the socket (the parser was held past
// it), is answered whole: the parser reads on to the end of the request's
// window, one window from its first bytes. It matched the part it had read:
// it missed, or took the recording's answer and left the rest to miss.
func TestARequestWhoseWindowEndsInsideItIsAnsweredWhole(t *testing.T) {
	req := varied(40<<10, true)
	for name, store := range bigExchanges(req) {
		for _, cut := range []int{1000, 32 << 10, 40<<10 - 1} {
			t.Run(fmt.Sprintf("%s, cut at %d", name, cut), func(t *testing.T) {
				eachSwitch(t, func(t *testing.T) {
					app, proxySide := tcpPair(t)
					conn := &faults{Conn: proxySide, left: cut, expire: true}
					done := make(chan error, 1)
					go func() {
						done <- decodeGeneric(context.Background(), zap.NewNop(), []byte("PING"), conn, nil, store, models.OutgoingOptions{})
					}()
					readAnswer(t, app, "PONG")
					if _, err := app.Write(req); err != nil {
						t.Fatal(err)
					}
					readAnswer(t, app, "BIGOK")
					_ = app.Close()
					awaitEnd(t, done, "a connection the application closed")
				})
			})
		}
	}
}

// A request whose rest comes after the round's deadline has passed, inside
// the request's window (one window from its first bytes), is answered whole:
// the parser reads on to the window's end and waits for the rest there. It
// ended the request at the round's deadline, and the part it had read
// missed. The rest is sent once the parser reads on, and its read waits for
// it, so the row does not race the window: past it, the parser reads what
// the socket holds.
func TestARequestWhoseRestComesAfterTheRoundsDeadlineIsAnsweredWhole(t *testing.T) {
	req := varied(40<<10, true)
	for name, store := range bigExchanges(req) {
		for _, cut := range []int{1000, 32 << 10} {
			t.Run(fmt.Sprintf("%s, cut at %d", name, cut), func(t *testing.T) {
				eachSwitch(t, func(t *testing.T) {
					app, proxySide := tcpPair(t)
					conn := &faults{Conn: proxySide, left: cut, expire: true, rearmed: make(chan struct{}), rest: len(req) - cut}
					done := make(chan error, 1)
					go func() {
						done <- decodeGeneric(context.Background(), zap.NewNop(), []byte("PING"), conn, nil, store, models.OutgoingOptions{})
					}()
					readAnswer(t, app, "PONG")
					if _, err := app.Write(req[:cut]); err != nil {
						t.Fatal(err)
					}
					awaitReadOn(t, conn.rearmed, done)
					if _, err := app.Write(req[cut:]); err != nil {
						t.Fatal(err)
					}
					readAnswer(t, app, "BIGOK")
					_ = app.Close()
					awaitEnd(t, done, "a connection the application closed")
				})
			})
		}
	}
}

// A long request that changed from its recording is read whole however the
// parser is held, and, without exact serving, answered by its similarity
// with the recording, whatever pieces its reads cut it in: the bytes are
// compared, not the pieces. Cut 0 is the control: no hold.
func TestAChangedLongRequestIsAnsweredWhateverItsReads(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	recorded := varied(40<<10, true)
	for _, changedAt := range []int{5, 35000} {
		live := append([]byte(nil), recorded...)
		live[changedAt] ^= 0x01
		for name, store := range bigExchanges(recorded) {
			for _, cut := range []int{0, 1000, 32 << 10, 40<<10 - 1} {
				t.Run(fmt.Sprintf("changed at %d, %s, cut at %d", changedAt, name, cut), func(t *testing.T) {
					app, proxySide := tcpPair(t)
					conn := net.Conn(proxySide)
					if cut > 0 {
						conn = &faults{Conn: proxySide, left: cut, expire: true}
					}
					done := make(chan error, 1)
					go func() {
						done <- decodeGeneric(context.Background(), zap.NewNop(), []byte("PING"), conn, nil, store, models.OutgoingOptions{})
					}()
					readAnswer(t, app, "PONG")
					if _, err := app.Write(live); err != nil {
						t.Fatal(err)
					}
					readAnswer(t, app, "BIGOK")
					_ = app.Close()
					awaitEnd(t, done, "a connection the application closed")
				})
			}
		}
	}
}

// A request on a connection whose first read brought no bytes has its
// window from its first bytes too: its rest, sent after the round's
// deadline has passed, is read in it.
func TestARequestAfterAnEmptyFirstReadIsReadWhole(t *testing.T) {
	req := varied(40<<10, true)
	for name, store := range bigExchanges(req) {
		t.Run(name, func(t *testing.T) {
			eachSwitch(t, func(t *testing.T) {
				app, proxySide := tcpPair(t)
				conn := &faults{Conn: proxySide, left: 1000, expire: true, rearmed: make(chan struct{}), rest: len(req) - 1000}
				done := make(chan error, 1)
				go func() {
					done <- decodeGeneric(context.Background(), zap.NewNop(), nil, conn, nil, store, models.OutgoingOptions{})
				}()
				if _, err := app.Write(req[:1000]); err != nil {
					t.Fatal(err)
				}
				awaitReadOn(t, conn.rearmed, done)
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

// A whole request is answered, or missed, at its window's end: nothing more
// is waited for, whether or not it begins a longer recording.
func TestAWholeRequestIsNotWaitedOn(t *testing.T) {
	req := varied(40<<10, true)
	prefix := eofStore{mocks: []*models.Mock{
		recordedExchange("mock-ping", "PING", "PONG"),
		recordedExchange("mock-two", "GET a\r\nGET b\r\n", "TWO"),
	}}
	for _, c := range []struct {
		name   string
		store  eofStore
		send   []byte
		answer string
	}{
		{"a long request, answered", bigExchanges(req)["recorded whole"], req, "BIGOK"},
		{"the start of a longer recording, missed", prefix, []byte("GET a\r\n"), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			eachSwitch(t, func(t *testing.T) {
				app, proxySide := tcpPair(t)
				done := make(chan error, 1)
				go func() {
					done <- decodeGeneric(context.Background(), zap.NewNop(), []byte("PING"), proxySide, nil, c.store, models.OutgoingOptions{})
				}()
				readAnswer(t, app, "PONG")
				sent := time.Now()
				if _, err := app.Write(c.send); err != nil {
					t.Fatal(err)
				}
				if c.answer != "" {
					readAnswer(t, app, c.answer)
				} else {
					awaitMiss(t, done)
				}
				// 50 windows: a parser that waited past the request's window
				// for more bytes (a second, as one did) is not answered so.
				if took := time.Since(sent); took > 50*quietWindow {
					t.Fatalf("the request was answered %v after it was sent; its window is %v", took, quietWindow)
				}
			})
		})
	}
}

// endsWith is the proxy's end of a connection the application closed right
// after the first part of a request: after quiet reads that time out, its
// next read returns that part with the connection's EOF, and from then on,
// as net.Pipe's do once its peer closed it, its read deadlines cannot be
// set.
type endsWith struct {
	net.Conn
	mu    sync.Mutex
	quiet int
	part  []byte
	ended bool
}

func (c *endsWith) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.ended:
		return 0, io.EOF
	case c.quiet > 0:
		c.quiet--
		return 0, &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	}
	c.ended = true
	return copy(p, c.part), io.EOF
}

func (c *endsWith) SetReadDeadline(time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended {
		return io.ErrClosedPipe
	}
	return nil
}

// A request the connection's end cuts short is matched as it is, at once:
// no rest comes after the end, and the parser reads no more of a connection
// whose end it read, in the round that begins with the request or a later
// one. It misses.
func TestARequestTheConnectionsEndCutsShortMissesAtOnce(t *testing.T) {
	for _, c := range []struct {
		name   string
		reqBuf string
		quiet  int
	}{
		{"the round begins with it", "PI", 0},
		{"it comes in a later round", "PING", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			eachSwitch(t, func(t *testing.T) {
				app, proxySide := net.Pipe()
				t.Cleanup(func() { _ = app.Close(); _ = proxySide.Close() })
				conn := &endsWith{Conn: proxySide, quiet: c.quiet, part: []byte("N")}
				done := make(chan error, 1)
				go func() {
					done <- decodeGeneric(context.Background(), zap.NewNop(), []byte(c.reqBuf), conn, nil, bigStore(), models.OutgoingOptions{})
				}()
				if c.quiet > 0 {
					readAnswer(t, app, "PONG")
				}
				awaitMiss(t, done)
			})
		})
	}
}

// endless is the proxy's end of a connection whose bytes never pause: after
// quiet reads that time out at once with no bytes, each read returns one
// more byte at once, and a deadline passes once perWindow reads have come
// since it was set (the bytes are that much faster than the window). It
// counts the bytes it gave. Past limit bytes, or closed, its reads fail: a
// parser that never ended its read stops there. What the parser writes is
// dropped. It gives the first byte only once the clock has moved past the
// start of the round that reads it, so the bytes come late in that round on
// any clock (faults says why).
type endless struct {
	net.Conn
	mu        sync.Mutex
	quiet     int
	perWindow int
	limit     int
	reads     int
	total     int
	closed    bool
	setAt     time.Time
}

func (c *endless) Read(p []byte) (int, error) {
	c.mu.Lock()
	first, setAt := c.total == 0 && c.quiet == 0 && !c.closed, c.setAt
	c.mu.Unlock()
	if first && !until(func() bool { return time.Now().After(setAt) }) {
		return 0, errors.New("the clock never moved past the round's start")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.closed || c.total >= c.limit:
		return 0, net.ErrClosed
	case c.quiet > 0:
		c.quiet--
		return 0, &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	case c.reads >= c.perWindow:
		return 0, &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	}
	c.reads++
	c.total++
	p[0] = 'Z'
	return 1, nil
}

func (c *endless) Write(p []byte) (int, error) { return len(p), nil }

func (c *endless) SetReadDeadline(time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads = 0
	c.setAt = time.Now()
	return nil
}

func (c *endless) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

// A request whose bytes never pause still ends at its window: the parser
// reads on past its round's deadline once, to the end of the request's
// window, and not when the round begins with the request (its window is
// the round's). It never holds a request that grows without bound.
func TestARequestWhoseBytesNeverPauseEndsAtItsWindow(t *testing.T) {
	const perWindow = 64
	for _, c := range []struct {
		name   string
		reqBuf string
		quiet  int
		want   int
	}{
		{"the round begins with it", "ZZZZ", 0, perWindow},
		{"it comes in a later round", "PING", 1, 2 * perWindow},
	} {
		t.Run(c.name, func(t *testing.T) {
			eachSwitch(t, func(t *testing.T) {
				app, proxySide := net.Pipe()
				t.Cleanup(func() { _ = app.Close(); _ = proxySide.Close() })
				conn := &endless{Conn: proxySide, quiet: c.quiet, perWindow: perWindow, limit: 4096}
				t.Cleanup(func() { _ = conn.Close() })
				done := make(chan error, 1)
				go func() {
					done <- decodeGeneric(context.Background(), zap.NewNop(), []byte(c.reqBuf), conn, nil, bigStore(), models.OutgoingOptions{})
				}()
				awaitMiss(t, done)
				conn.mu.Lock()
				total := conn.total
				conn.mu.Unlock()
				if total != c.want {
					t.Fatalf("the parser read %d bytes of the request after its first; want %d", total, c.want)
				}
			})
		})
	}
}

// A connection that breaks while the parser reads a request ends the
// parser, the request unmatched: no miss is filed for a request the
// connection never finished sending. A deadline the parser cannot set as
// it reads on ends it there, though a later one could be set.
func TestAConnectionThatBreaksInARequestEndsTheParser(t *testing.T) {
	reset := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	refused := errors.New("the deadline cannot be set")
	req := varied(40<<10, true)
	store := bigExchanges(req)["recorded whole"]
	for _, c := range []struct {
		name string
		send []byte
		conn func(net.Conn) net.Conn
	}{
		{"a read fails in its window", []byte("NOPE"), func(c net.Conn) net.Conn {
			return &faults{Conn: c, left: 4, readErr: reset}
		}},
		{"a read fails as the parser reads on", req, func(c net.Conn) net.Conn {
			return &faults{Conn: c, left: 1000, expire: true, readErr: reset}
		}},
		{"the deadline to read on to cannot be set", req, func(c net.Conn) net.Conn {
			return &faults{Conn: c, left: 1000, expire: true, deadlineErr: refused}
		}},
		{"only the deadline to read on to cannot be set", req, func(c net.Conn) net.Conn {
			return &faults{Conn: c, left: 1000, expire: true, deadlineErr: refused, deadlineOnce: true}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			eachSwitch(t, func(t *testing.T) {
				app, proxySide := tcpPair(t)
				conn := c.conn(proxySide)
				done := make(chan error, 1)
				go func() {
					done <- decodeGeneric(context.Background(), zap.NewNop(), []byte("PING"), conn, nil, store, models.OutgoingOptions{})
				}()
				readAnswer(t, app, "PONG")
				if _, err := app.Write(c.send); err != nil {
					t.Fatal(err)
				}
				awaitEnd(t, done, "a connection that broke")
			})
		})
	}
}
