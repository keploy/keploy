package generic

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// eachSwitch runs f with the proven switch off and on: the generic parser
// reads its connection the same way under both.
func eachSwitch(t *testing.T, f func(t *testing.T)) {
	for _, on := range []string{"", "1"} {
		name := "switch off"
		if on != "" {
			name = "switch on"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, on)
			f(t)
		})
	}
}

// eofStore answers a request with its recorded response, from the generic
// exchanges it holds.
type eofStore struct {
	integrations.MockMemDb
	mocks []*models.Mock
}

func (s eofStore) GetSessionMocks() ([]*models.Mock, error) { return s.mocks, nil }

func (s eofStore) UpdateUnFilteredMock(_, _ *models.Mock) bool { return true }

// recordedExchange is a generic exchange: req answered with resp.
func recordedExchange(name, req, resp string) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.GENERIC}
	m.TestModeInfo.IsFiltered = true
	m.Spec.GenericRequests = []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: req}}}}
	m.Spec.GenericResponses = []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: resp}}}}
	return m
}

// bigStore answers PING with PONG, and a request of n bytes of "A" with
// BIGOK, for each n given.
func bigStore(ns ...int) eofStore {
	mocks := []*models.Mock{recordedExchange("mock-ping", "PING", "PONG")}
	for _, n := range ns {
		mocks = append(mocks, recordedExchange("mock-big", string(bytes.Repeat([]byte("A"), n)), "BIGOK"))
	}
	return eofStore{mocks: mocks}
}

// readAnswer reads want's length from app, failing the test when it does not
// come within 10 s (a request the parser dropped is never answered).
func readAnswer(t *testing.T, app net.Conn, want string) {
	t.Helper()
	if err := app.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(app, got); err != nil || string(got) != want {
		t.Fatalf("answer %q, %v; want %q", got, err, want)
	}
}

// A request exactly as long as the parser's read buffer (1024 bytes), or a
// multiple of it, is answered. The parser reads a full buffer, reads again,
// and that read waits out the 10 ms deadline: the deadline's timeout comes
// back with the bytes already read. The parser took the timeout for "nothing
// more" and dropped them: the request was never answered, nor filed as a
// miss, and the application hung on it.
func TestTheGenericParserAnswersARequestAsLongAsItsReadBuffer(t *testing.T) {
	eachSwitch(t, func(t *testing.T) {
		for _, n := range []int{1024, 2048} {
			app, proxySide := net.Pipe()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- decodeGeneric(ctx, zap.NewNop(), []byte("PING"), proxySide, nil, bigStore(n), models.OutgoingOptions{})
			}()
			readAnswer(t, app, "PONG")
			// A pipe's write waits for the parser to read it: bounded, so a
			// parser that stopped reading fails the test instead of hanging it.
			if err := app.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := app.Write(bytes.Repeat([]byte("A"), n)); err != nil {
				t.Fatal(err)
			}
			readAnswer(t, app, "BIGOK")
			cancel()
			_ = app.Close()
			<-done
		}
	})
}

// tcpPair is a loopback TCP connection: the application's end, and the
// proxy's (a net.Pipe ends a parser's poll on its own: its SetReadDeadline
// fails once the peer closed, which a TCP socket's never does).
func tcpPair(t *testing.T) (app *net.TCPConn, proxySide net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	proxySide = <-accepted
	if proxySide == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = c.Close(); _ = proxySide.Close() })
	return c.(*net.TCPConn), proxySide
}

// awaitEnd fails the test unless decodeGeneric (done) ends, with nil, within
// 10 s.
func awaitEnd(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the parser ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the parser polled %s and never ended", what)
	}
}

// The parser ends when the application closes its connection. It read its
// connection with a 10 ms deadline and retried an empty EOF three times, 50
// ms apart (ReadBytes' handshake retry): the retry came back as the
// deadline's timeout, and an EOF that did come back was taken for "nothing
// more" too, so the parser polled the closed connection for ever, a
// goroutine and a connection that never ended.
func TestTheGenericParserEndsWhenTheApplicationCloses(t *testing.T) {
	eachSwitch(t, func(t *testing.T) {
		app, proxySide := tcpPair(t)
		done := make(chan error, 1)
		go func() {
			done <- decodeGeneric(context.Background(), zap.NewNop(), []byte("PING"), proxySide, nil, bigStore(), models.OutgoingOptions{})
		}()
		readAnswer(t, app, "PONG")
		_ = app.Close()
		awaitEnd(t, done, "a connection the application closed")
	})
}

// A request the application sends just before it closes its side of the
// connection (a half-close) is answered, and then the parser ends.
func TestTheGenericParserAnswersTheLastRequestOfAHalfClose(t *testing.T) {
	eachSwitch(t, func(t *testing.T) {
		app, proxySide := tcpPair(t)
		done := make(chan error, 1)
		go func() {
			done <- decodeGeneric(context.Background(), zap.NewNop(), []byte("PING"), proxySide, nil, bigStore(), models.OutgoingOptions{})
		}()
		readAnswer(t, app, "PONG")
		if _, err := io.WriteString(app, "PING"); err != nil {
			t.Fatal(err)
		}
		if err := app.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		readAnswer(t, app, "PONG")
		awaitEnd(t, done, "a half-closed connection")
	})
}

// closesAfterAnswer is the proxy's end of a connection whose application,
// once answered, sends last and closes, and whose read returns last together
// with the EOF: crypto/tls does so when a TLS 1.2 close_notify arrives right
// behind the request, as a read does for a request as long as the read
// buffer that a close follows.
type closesAfterAnswer struct {
	net.Conn
	answered atomic.Bool
	last     []byte
}

func (c *closesAfterAnswer) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.answered.Store(true)
	return n, err
}

func (c *closesAfterAnswer) Read(p []byte) (int, error) {
	if !c.answered.Load() {
		return c.Conn.Read(p)
	}
	n := copy(p, c.last)
	c.last = c.last[n:]
	return n, io.EOF
}

// A request a read returns together with the connection's EOF is answered,
// and then the parser ends. Before, the parser took the EOF for "nothing
// more" and dropped the request: never answered, nor filed as a miss.
func TestTheGenericParserAnswersARequestReturnedWithItsEOF(t *testing.T) {
	eachSwitch(t, func(t *testing.T) {
		app, proxySide := tcpPair(t)
		conn := &closesAfterAnswer{Conn: proxySide, last: []byte("PING")}
		done := make(chan error, 1)
		go func() {
			done <- decodeGeneric(context.Background(), zap.NewNop(), []byte("PING"), conn, nil, bigStore(), models.OutgoingOptions{})
		}()
		awaitEnd(t, done, "a connection whose last request came with its EOF")
		// Ended: what it wrote is all the application gets.
		_ = proxySide.Close()
		readAnswer(t, app, "PONGPONG")
	})
}

// A request longer than one read is answered with its recording, whether
// the old read loop recorded it whole or the relay in its pieces: the
// parser reads it in pieces of at most ReadChunkSize, as they arrive, and
// matched them piece by piece with a recording made in other pieces (none).
func TestTheGenericParserAnswersARequestLongerThanARead(t *testing.T) {
	req := varied(40<<10, true)
	for name, recorded := range map[string]*models.Mock{
		"recorded whole":     recordedAs("mock-big", req),
		"recorded in pieces": recordedAs("mock-big", req[:32<<10], req[32<<10:]),
	} {
		recorded.TestModeInfo.IsFiltered = true
		recorded.Spec.GenericResponses = []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: "BIGOK"}}}}
		store := eofStore{mocks: []*models.Mock{recordedExchange("mock-ping", "PING", "PONG"), recorded}}
		t.Run(name, func(t *testing.T) {
			eachSwitch(t, func(t *testing.T) {
				app, proxySide := tcpPair(t)
				done := make(chan error, 1)
				go func() {
					done <- decodeGeneric(context.Background(), zap.NewNop(), []byte("PING"), proxySide, nil, store, models.OutgoingOptions{})
				}()
				readAnswer(t, app, "PONG")
				// One write, then the half-close: the request's end is its
				// EOF, whatever pieces its reads return.
				if _, err := app.Write(req); err != nil {
					t.Fatal(err)
				}
				if err := app.CloseWrite(); err != nil {
					t.Fatal(err)
				}
				readAnswer(t, app, "BIGOK")
				awaitEnd(t, done, "a half-closed connection")
			})
		})
	}
}
