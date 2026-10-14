package realdeps

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// tcpPair is two ends of one loopback TCP connection.
func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	a, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	b := <-accepted
	require.NotNil(t, b)
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return a.(*net.TCPConn), b.(*net.TCPConn)
}

// relayRig is an application, keploy's two ends, and a real server, with a
// Relay running between keploy's ends.
type relayRig struct {
	app, server *net.TCPConn
	// client and dest are keploy's two ends.
	client, dest *net.TCPConn
	sent         atomic.Int64
	h2           atomic.Bool
	mu           sync.Mutex
	refused      []string
	done         chan error
	cancel       context.CancelFunc
}

func newRig(t *testing.T, check *Check) *relayRig {
	t.Helper()
	app, client := tcpPair(t)
	dest, server := tcpPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := &relayRig{app: app, server: server, client: client, dest: dest, done: make(chan error, 1), cancel: cancel}
	relay := &Relay{
		Client: client, In: bufio.NewReaderSize(client, ReaderSize), Dest: dest, Check: check,
		Sent: func(n int) { r.sent.Add(int64(n)) },
		H2:   func() { r.h2.Store(true) },
		Refused: func(why string) {
			r.mu.Lock()
			r.refused = append(r.refused, why)
			r.mu.Unlock()
		},
	}
	go func() { r.done <- relay.Run(ctx) }()
	return r
}

func (r *relayRig) refusals() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.refused...)
}

func checkFor(t *testing.T, port uint16, entries ...string) *Check {
	t.Helper()
	return &Check{list: mustParse(t, entries...), port: port}
}

func TestRelayCarriesEveryByteAndEachHalfClose(t *testing.T) {
	r := newRig(t, nil)
	_, err := r.app.Write([]byte("request"))
	require.NoError(t, err)
	require.NoError(t, r.app.CloseWrite())

	// The server learns the request ended from the half-close keploy passes
	// on, and only then answers.
	got, err := io.ReadAll(r.server)
	require.NoError(t, err)
	require.Equal(t, "request", string(got))
	_, err = r.server.Write([]byte("response"))
	require.NoError(t, err)
	require.NoError(t, r.server.CloseWrite())

	got, err = io.ReadAll(r.app)
	require.NoError(t, err)
	require.Equal(t, "response", string(got))
	require.NoError(t, <-r.done)
	require.Equal(t, int64(len("request")), r.sent.Load())
}

func TestRelayLetsAServerSpeakFirst(t *testing.T) {
	r := newRig(t, checkFor(t, 3306, "db.example.com:3306"))
	greeting := "\x4a\x00\x00\x00\x0agreeting"
	_, err := r.server.Write([]byte(greeting))
	require.NoError(t, err)
	buf := make([]byte, len(greeting))
	_, err = io.ReadFull(r.app, buf)
	require.NoError(t, err)
	require.Equal(t, greeting, string(buf))
	// Bytes that carry no name are relayed as they are.
	_, err = r.app.Write([]byte("\x20\x00\x00\x01login"))
	require.NoError(t, err)
	require.NoError(t, r.app.CloseWrite())
	got, err := io.ReadAll(r.server)
	require.NoError(t, err)
	require.Equal(t, "\x20\x00\x00\x01login", string(got))
	require.NoError(t, r.server.Close())
	<-r.done
	require.Empty(t, r.refusals())
}

func TestRelayChecksTheHostOfEveryRequest(t *testing.T) {
	r := newRig(t, checkFor(t, 8443, "auth.example.com:8443"))
	first := "POST /a HTTP/1.1\r\nHost: Auth.Example.Com:8443\r\nTransfer-Encoding: chunked\r\n\r\n5;ext=1\r\nhello\r\n0\r\nX-Trailer: t\r\n\r\n"
	second := "GET /b HTTP/1.1\r\nHost: auth.example.com\r\nContent-Length: 3\r\n\r\nabc"
	third := "GET /c HTTP/1.1\r\nHost: pay.example.com\r\n\r\n"
	_, err := r.app.Write([]byte(first + second + third))
	require.NoError(t, err)

	// The first two reach the server whole; the third, whose Host is not
	// listed, never does: keploy ends the connection there.
	got, err := io.ReadAll(r.server)
	require.NoError(t, err)
	require.Equal(t, first+second, string(got))
	require.NoError(t, <-r.done)
	refused := r.refusals()
	require.Len(t, refused, 1)
	require.Contains(t, refused[0], "pay.example.com")
	_, err = io.ReadAll(r.app)
	require.NoError(t, err, "the application's connection is closed")
}

func TestRelayChecksACONNECTTarget(t *testing.T) {
	r := newRig(t, checkFor(t, 3128, "egress.example.net:3128", "api.example.com:443"))
	_, err := r.app.Write([]byte("CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n\x16tunnel"))
	require.NoError(t, err)
	require.NoError(t, r.app.CloseWrite())
	got, err := io.ReadAll(r.server)
	require.NoError(t, err)
	require.Equal(t, "CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n\x16tunnel", string(got), "after a CONNECT the tunnel is relayed as it is")
	require.NoError(t, r.server.Close())
	<-r.done

	r = newRig(t, checkFor(t, 3128, "egress.example.net:3128"))
	_, err = r.app.Write([]byte("CONNECT other.example.com:443 HTTP/1.1\r\nHost: other.example.com:443\r\n\r\n"))
	require.NoError(t, err)
	got, err = io.ReadAll(r.server)
	require.NoError(t, err)
	require.Empty(t, got)
	<-r.done
	require.Len(t, r.refusals(), 1)
	require.Contains(t, r.refusals()[0], "other.example.com:443")
}

func TestRelayAfterAnUpgradeRelaysTheRestAsItIs(t *testing.T) {
	r := newRig(t, checkFor(t, 80, "ws.example.com:80"))
	msg := "GET /ws HTTP/1.1\r\nHost: ws.example.com\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\n\r\n\x81\x05hello"
	_, err := r.app.Write([]byte(msg))
	require.NoError(t, err)
	require.NoError(t, r.app.CloseWrite())
	got, err := io.ReadAll(r.server)
	require.NoError(t, err)
	require.Equal(t, msg, string(got))
	require.NoError(t, r.server.Close())
	<-r.done
	require.Empty(t, r.refusals())
}

func TestRelayChecksTheSNI(t *testing.T) {
	// A listed name passes: the hello reaches the server as it was sent.
	r := newRig(t, checkFor(t, 443, "auth.example.com:443"))
	go func() {
		_ = tls.Client(r.app, &tls.Config{ServerName: "auth.example.com", NextProtos: []string{"h2"}, InsecureSkipVerify: true}).Handshake() // nolint:gosec
	}()
	head := make([]byte, 5)
	_, err := io.ReadFull(r.server, head)
	require.NoError(t, err)
	require.Equal(t, byte(0x16), head[0])
	require.True(t, r.h2.Load(), "an h2 offer is noted for the REAL row")
	r.cancel()
	<-r.done

	// Another name ends it before its hello reaches the server.
	r = newRig(t, checkFor(t, 443, "auth.example.com:443"))
	go func() {
		_ = tls.Client(r.app, &tls.Config{ServerName: "pay.example.com", InsecureSkipVerify: true}).Handshake() // nolint:gosec
	}()
	got, err := io.ReadAll(r.server)
	require.NoError(t, err)
	require.Empty(t, got)
	<-r.done
	require.Len(t, r.refusals(), 1)
	require.True(t, strings.Contains(r.refusals()[0], "ClientHello names pay.example.com"), r.refusals()[0])
	require.False(t, r.h2.Load())
}

func TestRelayEndsWithTheRun(t *testing.T) {
	r := newRig(t, nil)
	r.cancel()
	<-r.done
	_, err := r.server.Read(make([]byte, 1))
	require.Error(t, err, "keploy closed its end of the destination")
}

func TestRelayEndsBothDirectionsOnAReset(t *testing.T) {
	r := newRig(t, nil)
	require.NoError(t, r.app.SetLinger(0))
	require.NoError(t, r.app.Close())
	<-r.done
	_, err := io.ReadAll(r.server)
	_ = err // EOF or a reset: either way the server's end is over
}

// closed reports whether keploy closed its end c.
func closed(c *net.TCPConn) bool {
	return errors.Is(c.SetReadDeadline(time.Time{}), net.ErrClosed)
}

// A relay that ended cleanly closes both of keploy's ends at once.
func TestRelayClosesBothEndsWhenItEnds(t *testing.T) {
	r := newRig(t, nil)
	require.NoError(t, r.app.CloseWrite())
	_, err := io.ReadAll(r.server)
	require.NoError(t, err)
	require.NoError(t, r.server.CloseWrite())
	require.NoError(t, <-r.done)
	require.True(t, closed(r.dest), "the destination's socket is closed")
	require.True(t, closed(r.client), "the application's socket is closed")
}

// A client whose first line is another protocol's, that then waits for the
// server, is relayed as soon as its line is in: its bytes carry no name, so
// they contradict none.
func TestRelayOfANameCheckedConnectionDoesNotWaitOnALineProtocol(t *testing.T) {
	for _, first := range []string{"EHLO me\r\n", "SET k v\r\n", "\x16\x01\x00\x01\x00\x00\x00\x00"} {
		r := newRig(t, checkFor(t, 25, "mail.example.com:25"))
		_, err := r.app.Write([]byte(first))
		require.NoError(t, err)
		got := make([]byte, len(first))
		_, err = io.ReadFull(r.server, got)
		require.NoError(t, err, "%q", first)
		require.Equal(t, first, string(got))
		_, err = r.server.Write([]byte("250 ok\r\n"))
		require.NoError(t, err)
		reply := make([]byte, 8)
		_, err = io.ReadFull(r.app, reply)
		require.NoError(t, err)
		r.cancel()
		<-r.done
		require.Empty(t, r.refusals())
	}
}

// TLS or HTTP/1 keploy cannot read whole may carry a name it cannot check:
// the connection ends there, before its bytes reach the destination.
func TestRelayRefusesUnreadableTLSOrHTTP(t *testing.T) {
	for _, first := range []string{
		"\x16\x03\x01\x00\x08\x01\x00\x00\x04garbage",
		"GET / HTTP/1.1\r\nX-Big: " + strings.Repeat("a", ReaderSize) + "\r\n\r\n",
	} {
		r := newRig(t, checkFor(t, 443, "auth.example.com:443"))
		go func() { _, _ = r.app.Write([]byte(first)) }()
		got, err := io.ReadAll(r.server)
		require.NoError(t, err)
		require.Empty(t, got)
		require.NoError(t, <-r.done)
		require.Len(t, r.refusals(), 1)
		require.Contains(t, r.refusals()[0], "could not read")
	}
}

// A first request after empty lines is checked like any other, and so is the
// request after it: an unlisted Host never reaches the destination.
func TestRelayChecksARequestAfterLeadingEmptyLines(t *testing.T) {
	r := newRig(t, checkFor(t, 80, "api.example.com:80"))
	first := "\r\nGET /a HTTP/1.1\r\nHost: api.example.com\r\n\r\n"
	_, err := r.app.Write([]byte(first + "GET /b HTTP/1.1\r\nHost: evil.example.com\r\n\r\n"))
	require.NoError(t, err)
	got, err := io.ReadAll(r.server)
	require.NoError(t, err)
	require.Equal(t, first, string(got))
	<-r.done
	require.Len(t, r.refusals(), 1)
	require.Contains(t, r.refusals()[0], "evil.example.com")
}

// h2c's names travel in HPACK: its bytes are relayed, and its REAL row says
// h2. A near-miss HTTP request line is refused.
func TestRelayOfH2CAndOfHTTPItCannotRead(t *testing.T) {
	r := newRig(t, checkFor(t, 80, "api.example.com:80"))
	preface := "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
	_, err := r.app.Write([]byte(preface))
	require.NoError(t, err)
	got := make([]byte, len(preface))
	_, err = io.ReadFull(r.server, got)
	require.NoError(t, err)
	require.True(t, r.h2.Load())
	r.cancel()
	<-r.done

	for _, parts := range [][]string{
		{"GET / http/1.1\r\nHost: evil.example.com\r\n\r\n"},
		{"get / HTTP/1.1\r\nHost: evil.example.com\r\n\r\n"},
		{"Ge", "t / HTTP/1.1\r\nHost: evil.example.com\r\n\r\n"},
	} {
		r = newRig(t, checkFor(t, 80, "api.example.com:80"))
		for _, part := range parts {
			_, err = r.app.Write([]byte(part))
			require.NoError(t, err)
		}
		b, err := io.ReadAll(r.server)
		require.NoError(t, err)
		require.Empty(t, b, "%q reached the destination", parts)
		<-r.done
		require.Len(t, r.refusals(), 1, "%q", parts)
	}
}

// writerFunc is an io.Writer from a function.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// A use is told before its bytes go: the destination may answer, and the
// test end, before the write returns.
func TestRelayTellsAUseBeforeItsBytesGo(t *testing.T) {
	told := 0
	w := &sentWriter{
		w: writerFunc(func(p []byte) (int, error) {
			require.Equal(t, 1, told, "the bytes went before their use was told")
			return len(p), nil
		}),
		sent: func(n int) { told++; require.Equal(t, 5, n) },
	}
	n, err := w.Write([]byte("hello"))
	require.NoError(t, err)
	require.Equal(t, 5, n)
	n, err = w.Write(nil)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Equal(t, 1, told, "an empty write tells nothing")
}

// A name-checked REAL connection of a word protocol (ZooKeeper's four-letter
// words) reaches its destination: a word that waits for its answer is relayed
// at once, and one its client half-closes after is relayed with the end, so
// the answer comes back. A request head its client cut short is refused.
func TestRelayOfAWordAndOfAHalfClosedStart(t *testing.T) {
	for _, tc := range []struct {
		send      string
		halfClose bool
	}{{"ruok", false}, {"ruok", true}, {"STAT", true}} {
		r := newRig(t, checkFor(t, 2181, "zk.example.com:2181"))
		_, err := r.app.Write([]byte(tc.send))
		require.NoError(t, err)
		if tc.halfClose {
			require.NoError(t, r.app.CloseWrite())
		}
		got := make([]byte, len(tc.send))
		_, err = io.ReadFull(r.server, got)
		require.NoError(t, err, "%+v", tc)
		require.Equal(t, tc.send, string(got))
		if tc.halfClose {
			_, err = r.server.Read(make([]byte, 1))
			require.ErrorIs(t, err, io.EOF, "the half-close reached the destination")
			_, err = r.server.Write([]byte("imok"))
			require.NoError(t, err)
			require.NoError(t, r.server.CloseWrite())
			back, err := io.ReadAll(r.app)
			require.NoError(t, err)
			require.Equal(t, "imok", string(back))
		}
		r.cancel()
		<-r.done
		require.Empty(t, r.refusals(), "%+v", tc)
		require.NotZero(t, r.sent.Load(), "%+v", tc)
	}

	hello := clientHelloBytes(t, "evil.example.com")
	for _, cut := range []string{"GET / HTTP/1.1\r\nHost: evil.example.com\r\n", string(hello[:len(hello)-10])} {
		r := newRig(t, checkFor(t, 443, "api.example.com:443"))
		_, err := r.app.Write([]byte(cut))
		require.NoError(t, err)
		require.NoError(t, r.app.CloseWrite())
		b, err := io.ReadAll(r.server)
		require.NoError(t, err)
		require.Empty(t, b, "a cut-short start reached the destination")
		<-r.done
		require.Len(t, r.refusals(), 1)
	}
}
