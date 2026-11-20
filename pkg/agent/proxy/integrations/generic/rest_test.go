package generic

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// withRestWait sets how long the parser waits for the rest of a request for
// the row, and restores it after.
func withRestWait(t *testing.T, d time.Duration) {
	t.Helper()
	old := restWait.Swap(int64(d))
	t.Cleanup(func() { restWait.Store(old) })
}

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
// another deadline. From there its reads fail with readErr, and the read
// deadlines set after okDeadlines more were with deadlineErr.
type faults struct {
	net.Conn
	mu          sync.Mutex
	left        int
	expire      bool
	readErr     error
	okDeadlines int
	deadlineErr error
	reached     bool
	expired     bool
}

func (c *faults) Read(p []byte) (int, error) {
	c.mu.Lock()
	reached, expired, left := c.reached, c.expired, c.left
	c.mu.Unlock()
	switch {
	case expired:
		return 0, &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	case reached && c.readErr != nil:
		return 0, c.readErr
	case !reached && len(p) > left:
		p = p[:left]
	}
	n, err := c.Conn.Read(p)
	c.mu.Lock()
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
		if c.okDeadlines == 0 {
			c.mu.Unlock()
			return c.deadlineErr
		}
		c.okDeadlines--
	}
	c.expired = false
	c.mu.Unlock()
	return c.Conn.SetReadDeadline(t)
}

// A request longer than one read whose window ends between two of its reads,
// the rest already in the socket (the parser was held past its deadline), is
// answered whole. The parser matched the part it had read: it missed, or
// took the recording's answer and left the rest to miss. The row waits for
// nothing it does not need: the parser waits an hour for a rest that does
// not come, so one that waited for bytes it already had fails the row.
func TestARequestWhoseWindowEndsInsideItIsAnsweredWhole(t *testing.T) {
	withRestWait(t, time.Hour)
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

// timesOutAfterBytes is the proxy's end of a connection that tells, once,
// when a read returned the deadline's timeout after bytes came: the round's
// window ended with what had come.
type timesOutAfterBytes struct {
	net.Conn
	got, told bool
	ended     chan struct{}
}

func (c *timesOutAfterBytes) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	var netErr net.Error
	if c.got && !c.told && errors.As(err, &netErr) && netErr.Timeout() {
		c.told = true
		close(c.ended)
	}
	c.got = c.got || n > 0
	return n, err
}

// The rest of a request that comes after its window ended (a client that
// writes it in parts) is waited for, and the request answered whole.
func TestTheRestOfARequestThatComesAfterItsWindowIsWaitedFor(t *testing.T) {
	withRestWait(t, time.Hour)
	req := varied(40<<10, true)
	for name, store := range bigExchanges(req) {
		t.Run(name, func(t *testing.T) {
			eachSwitch(t, func(t *testing.T) {
				app, proxySide := tcpPair(t)
				conn := &timesOutAfterBytes{Conn: proxySide, ended: make(chan struct{})}
				done := make(chan error, 1)
				go func() {
					done <- decodeGeneric(context.Background(), zap.NewNop(), []byte("PING"), conn, nil, store, models.OutgoingOptions{})
				}()
				readAnswer(t, app, "PONG")
				if _, err := app.Write(req[:1000]); err != nil {
					t.Fatal(err)
				}
				select {
				case <-conn.ended:
				case <-time.After(10 * time.Second):
					t.Fatal("the parser's window never ended")
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

// A request that begins a recorded one and whose rest never comes is
// matched as it is once restWait has passed: it misses.
func TestARequestWhoseRestNeverComesMisses(t *testing.T) {
	withRestWait(t, 50*time.Millisecond)
	req := varied(40<<10, true)
	for name, store := range bigExchanges(req) {
		t.Run(name, func(t *testing.T) {
			eachSwitch(t, func(t *testing.T) {
				app, proxySide := tcpPair(t)
				done := make(chan error, 1)
				go func() {
					done <- decodeGeneric(context.Background(), zap.NewNop(), []byte("PING"), proxySide, nil, store, models.OutgoingOptions{})
				}()
				readAnswer(t, app, "PONG")
				if _, err := app.Write(req[:1000]); err != nil {
					t.Fatal(err)
				}
				awaitMiss(t, done)
			})
		})
	}
}

// A request that is a recorded request, and begins a longer one, is answered
// at its window's end: no rest is waited for.
func TestARequestThatIsARecordingIsNotWaitedOn(t *testing.T) {
	withRestWait(t, time.Hour)
	store := eofStore{mocks: []*models.Mock{
		recordedExchange("mock-ping", "PING", "PONG"),
		recordedExchange("mock-pingping", "PINGPING", "TWO"),
	}}
	eachSwitch(t, func(t *testing.T) {
		app, proxySide := tcpPair(t)
		done := make(chan error, 1)
		go func() {
			done <- decodeGeneric(context.Background(), zap.NewNop(), []byte("PING"), proxySide, nil, store, models.OutgoingOptions{})
		}()
		readAnswer(t, app, "PONG")
		_ = app.Close()
		awaitEnd(t, done, "a connection the application closed")
	})
}

// A request that begins no recorded request, a changed one, ends at its
// window's end and misses: nothing tells its length.
func TestARequestThatBeginsNoRecordingIsNotWaitedOn(t *testing.T) {
	withRestWait(t, time.Hour)
	eachSwitch(t, func(t *testing.T) {
		app, proxySide := tcpPair(t)
		done := make(chan error, 1)
		go func() {
			done <- decodeGeneric(context.Background(), zap.NewNop(), []byte("PING"), proxySide, nil, bigStore(), models.OutgoingOptions{})
		}()
		readAnswer(t, app, "PONG")
		if _, err := io.WriteString(app, "NOPE"); err != nil {
			t.Fatal(err)
		}
		awaitMiss(t, done)
	})
}

// endless is the proxy's end of a connection whose bytes never pause: each
// read returns one more byte at once, and a deadline passes once perWindow
// reads have come since it was set (the bytes are that much faster than the
// window). Past limit reads, or closed, its reads fail: a parser that never
// ended the request stops there, unmatched.
type endless struct {
	net.Conn
	mu        sync.Mutex
	perWindow int
	limit     int
	reads     int
	total     int
	closed    bool
}

func (c *endless) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.closed || c.total >= c.limit:
		return 0, net.ErrClosed
	case c.reads >= c.perWindow:
		return 0, &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	}
	c.reads++
	c.total++
	p[0] = 'Z'
	return 1, nil
}

func (c *endless) SetReadDeadline(time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads = 0
	return nil
}

func (c *endless) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

// A request whose bytes never pause still ends at its round's window: the
// window is the round's, from its start, so the parser neither waits for
// ever nor holds a request that grows without bound.
func TestARequestWhoseBytesNeverPauseEndsAtItsWindow(t *testing.T) {
	withRestWait(t, time.Hour)
	eachSwitch(t, func(t *testing.T) {
		app, proxySide := net.Pipe()
		t.Cleanup(func() { _ = app.Close(); _ = proxySide.Close() })
		conn := &endless{Conn: proxySide, perWindow: 64, limit: 4096}
		t.Cleanup(func() { _ = conn.Close() })
		done := make(chan error, 1)
		go func() {
			done <- decodeGeneric(context.Background(), zap.NewNop(), []byte("ZZZZ"), conn, nil, bigStore(), models.OutgoingOptions{})
		}()
		awaitMiss(t, done)
	})
}

// A connection that breaks while the parser reads a request ends the
// parser, the request unmatched: no miss is filed for a request the
// connection never finished sending.
func TestAConnectionThatBreaksInARequestEndsTheParser(t *testing.T) {
	withRestWait(t, time.Hour)
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
		{"a read fails while its rest is awaited", req, func(c net.Conn) net.Conn {
			return &faults{Conn: c, left: 1000, expire: true, readErr: reset}
		}},
		{"the deadline for its rest cannot be set", req, func(c net.Conn) net.Conn {
			return &faults{Conn: c, left: 1000, expire: true, deadlineErr: refused}
		}},
		{"the deadline after part of its rest cannot be set", req, func(c net.Conn) net.Conn {
			return &faults{Conn: c, left: 1000, expire: true, okDeadlines: 1, deadlineErr: refused}
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

func textPieces(s ...string) []models.Payload {
	var out []models.Payload
	for _, p := range s {
		out = append(out, models.Payload{Message: []models.OutputBinary{{Type: models.String, Data: p}}})
	}
	return out
}

func binaryPiece(data string) models.Payload {
	return models.Payload{Message: []models.OutputBinary{{Type: binaryPayload, Data: data}}}
}

func requestOf(s ...string) [][]byte {
	var out [][]byte
	for _, p := range s {
		out = append(out, []byte(p))
	}
	return out
}

// A request begins a recorded request when its bytes are the recording's
// first bytes, fewer than them, in whatever pieces either was cut, each
// recorded piece read as the exact match reads it.
func TestARequestBeginsARecordingWithItsFirstBytes(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString
	for _, c := range []struct {
		name     string
		recorded []models.Payload
		req      [][]byte
		want     bool
	}{
		{"its start, in one piece", textPieces("PINGPONG"), requestOf("PING"), true},
		{"its start, in two", textPieces("PINGPONG"), requestOf("PI", "NG"), true},
		{"ending inside a later piece", textPieces("PING", "PONG"), requestOf("PINGPO"), true},
		{"ending where a piece ends", textPieces("PING", "PONG"), requestOf("PING"), true},
		{"before an empty piece only", textPieces("PING", ""), requestOf("PING"), false},
		{"after an empty piece", textPieces("", "PINGPONG"), requestOf("PING"), true},
		{"the recording whole", textPieces("PING", "PONG"), requestOf("PINGPONG"), false},
		{"longer than the recording", textPieces("PINGPONG"), requestOf("PINGPONGX"), false},
		{"other bytes", textPieces("PINGPONG"), requestOf("PONG"), false},
		{"other bytes in a whole earlier piece", textPieces("PING", "PONG"), requestOf("PANGPO"), false},
		{"other bytes in a later piece", textPieces("PING", "PONG"), requestOf("PINGPA"), false},
		{"a binary piece's start", []models.Payload{binaryPiece(b64([]byte("PINGPONG")))}, requestOf("PIN"), true},
		{"a binary piece whose base64 breaks later", []models.Payload{binaryPiece("UElO!!!!")}, requestOf("PIN"), false},
		{"a binary piece of no whole length", []models.Payload{binaryPiece("UElOR1BPTkc")}, requestOf("PIN"), false},
		{"a piece of two messages", []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: "PINGPONG"}, {Type: models.String, Data: "X"}}}}, requestOf("PING"), false},
		{"a piece of no message", []models.Payload{{}}, requestOf("PING"), false},
		{"a piece of another type", []models.Payload{{Message: []models.OutputBinary{{Type: "other", Data: "PINGPONG"}}}}, requestOf("PING"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := newSentRequest(c.req).begins(c.recorded); got != c.want {
				t.Fatalf("begins = %v; want %v", got, c.want)
			}
		})
	}
}

// The parser waits for the rest of a request only when it begins a generic
// recording's request and is no generic recording's request.
func TestARestIsToComeOnlyForARequestThatBeginsARecording(t *testing.T) {
	generic := func(reqs ...string) eofStore {
		var s eofStore
		for i, r := range reqs {
			m := &models.Mock{Name: fmt.Sprintf("mock-%d", i), Kind: models.GENERIC}
			m.Spec.GenericRequests = textPieces(r)
			s.mocks = append(s.mocks, m)
		}
		return s
	}
	other := &models.Mock{Name: "mock-http", Kind: models.HTTP}
	other.Spec.GenericRequests = textPieces("PINGPONG")
	for _, c := range []struct {
		name  string
		store eofStore
		want  bool
	}{
		{"one it begins", generic("PINGPONG"), true},
		{"one it begins after one it does not", generic("PONG", "PINGPONG"), true},
		{"one it begins before one it does not", generic("PINGPONG", "PONG"), true},
		{"one it begins, and one it is", generic("PINGPONG", "PING"), false},
		{"none it begins", generic("PONG"), false},
		{"one of another kind", eofStore{mocks: []*models.Mock{other}}, false},
		{"none at all", eofStore{}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := restToCome(c.store, requestOf("PING")); got != c.want {
				t.Fatalf("restToCome = %v; want %v", got, c.want)
			}
		})
	}
}
