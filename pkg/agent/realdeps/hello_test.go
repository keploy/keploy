package realdeps

import (
	"bufio"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// clientHello starts a TLS client on one end of a pipe and returns the other
// end, buffered as the proxy reads it. The client waits for a ServerHello
// that never comes until the pipe closes.
func clientHello(t *testing.T, serverName string, protos []string) *bufio.Reader {
	t.Helper()
	c, s := net.Pipe()
	t.Cleanup(func() { _ = c.Close(); _ = s.Close() })
	go func() {
		_ = tls.Client(c, &tls.Config{ServerName: serverName, NextProtos: protos, InsecureSkipVerify: true}).Handshake() // nolint:gosec
	}()
	return bufio.NewReaderSize(s, ReaderSize)
}

// writes sends parts on one end of a pipe, in order, then closes it, and
// returns the other end, buffered.
func writes(t *testing.T, parts ...string) *bufio.Reader {
	t.Helper()
	c, s := net.Pipe()
	t.Cleanup(func() { _ = c.Close(); _ = s.Close() })
	go func() {
		for _, p := range parts {
			if _, err := c.Write([]byte(p)); err != nil {
				return
			}
		}
		_ = c.Close()
	}()
	return bufio.NewReaderSize(s, ReaderSize)
}

func TestReadNameReadsTheSNIWithoutConsumingTheHello(t *testing.T) {
	br := clientHello(t, "auth.example.com", []string{"h2", "http/1.1"})
	n, err := ReadName(br)
	require.NoError(t, err)
	require.Equal(t, Name{Host: "auth.example.com", Via: "sni", TLS: true, H2: true}, n)

	// Every byte of the hello is still there for whatever handles the
	// connection next.
	head, err := br.Peek(5)
	require.NoError(t, err)
	require.Equal(t, byte(0x16), head[0])
	size := 5 + int(head[3])<<8 | int(head[4])
	require.GreaterOrEqual(t, br.Buffered(), size)
}

func TestReadNameWithoutSNIIsNoName(t *testing.T) {
	br := clientHello(t, "", []string{"http/1.1"})
	n, err := ReadName(br)
	require.ErrorIs(t, err, ErrNoName)
	require.True(t, n.TLS)
	require.False(t, n.H2)
}

func TestReadNameWaitsForTheWholeRequestHead(t *testing.T) {
	// The head arrives in pieces: the read waits for each, with no clock.
	br := writes(t, "GE", "T /x HTTP/1.1\r\nHo", "st: Auth.Example.Com:8443\r\nAccept: */*\r\n", "\r\nbody")
	n, err := ReadName(br)
	require.NoError(t, err)
	require.Equal(t, Name{Host: "Auth.Example.Com", Via: "host"}, n)
	rest, err := io.ReadAll(br)
	require.NoError(t, err)
	require.Equal(t, "GET /x HTTP/1.1\r\nHost: Auth.Example.Com:8443\r\nAccept: */*\r\n\r\nbody", string(rest), "nothing consumed")
}

func TestReadNameReadsACONNECTTarget(t *testing.T) {
	br := writes(t, "CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n")
	n, err := ReadName(br)
	require.NoError(t, err)
	require.Equal(t, Name{Host: "api.example.com", Port: 443, Via: "connect"}, n)
}

func TestReadNameOfAnotherProtocolIsNoNameAtOnce(t *testing.T) {
	for _, first := range []string{"\x00\x00\x00\x08\x04\xd2\x16\x2f", "get / HTTP/1.1\r\n\r\n", "*1\r\n$4\r\nPING\r\n", "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"} {
		_, err := ReadName(writes(t, first))
		require.ErrorIs(t, err, ErrNoName, "%q", first)
	}
	_, err := ReadName(writes(t, "GET / HTTP/1.0\r\n\r\n"))
	require.ErrorIs(t, err, ErrNoName, "a request without a Host")
}

func TestReadNameOfAClientThatSendsNothingIsItsEnd(t *testing.T) {
	_, err := ReadName(writes(t))
	require.ErrorIs(t, err, io.EOF)
	require.NotErrorIs(t, err, ErrNoName)
}

func TestReadNameOfAHeadLargerThanTheBufferIsNoName(t *testing.T) {
	big := "GET / HTTP/1.1\r\nX-Big: " + strings.Repeat("a", ReaderSize) + "\r\n\r\n"
	_, err := ReadName(writes(t, big))
	require.ErrorIs(t, err, ErrNoName)
}

// holds sends parts on one end of a pipe, in order, and keeps it open: a
// client that waits for the server's answer. A read that waited for more of
// its bytes would never return.
func holds(t *testing.T, parts ...string) *bufio.Reader {
	t.Helper()
	c, s := net.Pipe()
	t.Cleanup(func() { _ = c.Close(); _ = s.Close() })
	go func() {
		for _, p := range parts {
			if _, err := c.Write([]byte(p)); err != nil {
				return
			}
		}
	}()
	return bufio.NewReaderSize(s, ReaderSize)
}

// A client of another protocol that waits for the server after its first
// line, or its first bytes, is told apart from HTTP/1 and TLS then: never a
// wait for bytes it will not send.
func TestReadNameDecidesAtTheFirstLineOrBytes(t *testing.T) {
	for _, first := range []string{
		"EHLO me\r\n",                          // SMTP
		"SET k v\r\n",                          // Redis inline
		"CONNECT {\"verbose\":false}\r\n",      // NATS
		"USER anonymous\r\n",                   // FTP, POP3
		"GET\r\n",                              // HTTP/0.9: no name
		"set k v\r\n",                          // Redis inline, lowercase
		"get key\r\n",                          // memcached text
		"\x16\x01\x00\x01\x00\x00\x00\x00\x00", // a MySQL packet whose length starts 0x16
		"\x16\x03\x05\x00\x10",                 // no TLS version past 1.3
		"\x16\x03\x01\x00\x00",                 // an empty record
		"\x16\x03\x01\x00\x30\x02",             // a handshake that is not a ClientHello
	} {
		_, err := ReadName(holds(t, first))
		require.ErrorIs(t, err, ErrNoName, "%q", first)
		require.NotErrorIs(t, err, ErrUnreadable, "%q", first)
	}
}

// Bare LF ends lines as well as CRLF.
func TestReadNameReadsABareLFHead(t *testing.T) {
	n, err := ReadName(holds(t, "GET / HTTP/1.1\nHost: api.example.com\n\n"))
	require.NoError(t, err)
	require.Equal(t, Name{Host: "api.example.com", Via: "host"}, n)
}

// TLS or HTTP/1 keploy cannot read whole is ErrUnreadable: it may carry a
// name keploy cannot check. A request or a hello that carries no name is
// not.
func TestReadNameTellsUnreadableFromNameless(t *testing.T) {
	_, err := ReadName(holds(t, "\x16\x03\x01\x00\x08\x01\x00\x00\x04garbage"))
	require.ErrorIs(t, err, ErrUnreadable, "a hello crypto/tls refuses")
	_, err = ReadName(writes(t, "GET / HTTP/1.1\r\nX-Big: "+strings.Repeat("a", ReaderSize)+"\r\n\r\n"))
	require.ErrorIs(t, err, ErrUnreadable, "a head larger than the buffer")
	_, err = ReadName(writes(t, "CONNECT nowhere HTTP/1.1\r\n\r\n"))
	require.ErrorIs(t, err, ErrUnreadable, "a CONNECT target that is not host:port")
	_, err = ReadName(writes(t, "GET / HTTP/1.0\r\n\r\n"))
	require.ErrorIs(t, err, ErrNoName)
	require.NotErrorIs(t, err, ErrUnreadable, "a request without a Host carries no name")
	_, err = ReadName(clientHello(t, "", nil))
	require.NotErrorIs(t, err, ErrUnreadable, "a hello without an SNI carries no name")
}

// Empty lines before a request line are passed over, as servers do; a line
// that is plainly HTTP but not a request line keploy reads is ErrUnreadable,
// never nameless; the h2c preface is ErrH2C.
func TestReadNameOfHTTPItCannotReadWholeIsUnreadable(t *testing.T) {
	n, err := ReadName(holds(t, "\r\n\r\nGET / HTTP/1.1\r\nHost: api.example.com\r\n\r\n"))
	require.NoError(t, err)
	require.Equal(t, "api.example.com", n.Host)

	for _, first := range []string{
		"GET / http/1.1\r\nHost: evil.example.com\r\n\r\n",  // a lowercase version
		"GET / HTTP/1.1 \r\nHost: evil.example.com\r\n\r\n", // trailing space
		"GET /  HTTP/1.1\r\nHost: evil.example.com\r\n\r\n", // two spaces
		"GET /x HTTP/3.0\r\n\r\n",                           // a version it does not read
		"get / HTTP/1.1\r\nHost: evil.example.com\r\n\r\n",  // a lowercase method
		"Get / HTTP/1.1\r\nHost: evil.example.com\r\n\r\n",  // a mixed-case method
	} {
		_, err := ReadName(holds(t, first))
		require.ErrorIs(t, err, ErrUnreadable, "%q", first)
	}
	// However the client's segments split it: the decision waits for the end
	// of the request line, never taking a method's lowercase letter for
	// another protocol.
	for _, parts := range [][]string{
		{"Ge", "t / HTTP/1.1\r\nHost: evil.example.com\r\n\r\n"},
		{"g", "et / HTTP/1.1\r\nHost: evil.example.com\r\n\r\n"},
	} {
		_, err := ReadName(holds(t, parts...))
		require.ErrorIs(t, err, ErrUnreadable, "%q", parts)
	}
	_, err = ReadName(writes(t, "GET /"+strings.Repeat("a", ReaderSize)+" HTTP/1.1\r\n\r\n"))
	require.ErrorIs(t, err, ErrUnreadable, "a request line longer than the buffer")

	n, err = ReadName(holds(t, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"))
	require.ErrorIs(t, err, ErrH2C)
	require.ErrorIs(t, err, ErrNoName)
	require.True(t, n.H2)
}

// A lowercase word is waited on only while it may still be a known method
// in another case: ZooKeeper's four-letter words, sent with no line end while
// the client waits for the answer, are decided at once. An uppercase token
// may be any method.
func TestReadNameWaitsOnALowercaseWordOnlyWhileItMayBeAMethod(t *testing.T) {
	for _, word := range []string{"ruok", "mntr", "srvr", "cons", "conf", "isro"} {
		_, err := ReadName(holds(t, word))
		require.ErrorIs(t, err, ErrNoName, word)
		require.NotErrorIs(t, err, ErrUnreadable, word)
	}
	for _, prefix := range []string{"G", "ge", "Pos", "delet", "OPTIONS", "propfind", "STAT", "PROPFIND", "FOO", "VERSION-CONTROL", "FOO2", "FOO.BAR", strings.Repeat("G", 32), "quer"} {
		require.True(t, looksLikeRequestLine([]byte(prefix)), prefix)
	}
	for _, line := range []string{"ruok", "stat", "posts", "Gets", "get1", " GET", "-GET", "GE\x00", strings.Repeat("G", 33)} {
		require.False(t, looksLikeRequestLine([]byte(line)), line)
	}
	// Past its space, a line is waited on only while it can still be
	// METHOD TARGET HTTP/: "purge " split from the rest is still read whole,
	// while words sent with no line end are decided at once.
	for _, wait := range []string{"get ", "FOO ", "purge ", "GET /", "get /x ", "GET /x HT", "OPTIONS * ", "GET http://h", "get HTTPS:", "CONNECT db:5432 ", "GET  /x  http/1", "GET ftp://h", "get ws:", "GET urn:x", "GET f", "GET host:80 ", "PURGE ftp://h", "BAN ws:", "HELLO WORLD"} {
		require.True(t, looksLikeRequestLine([]byte(wait)), wait)
	}
	for _, decide := range []string{"hello world", "ping me", "auth \x00\x01\x02", "GET /x 1.1", "GET /x HTTX", "purge ftp://h", "get /\x00", "GET 1x:", "SET k v", "HELLO WORLD AGAIN"} {
		require.False(t, looksLikeRequestLine([]byte(decide)), "%q", decide)
		_, err := ReadName(holds(t, decide))
		require.ErrorIs(t, err, ErrNoName, "%q", decide)
	}
	_, err := ReadName(holds(t, "purge ", "/ HTTP/1.1\r\nHost: evil.example.com\r\n\r\n"))
	require.ErrorIs(t, err, ErrUnreadable)
	// A request with any method token is plainly HTTP: one keploy does not
	// read is unreadable, never relayed unchecked.
	for _, line := range []string{"VERSION-CONTROL / HTTP/1.1\r\nHost: evil.example.com\r\n\r\n", "UPDATEREDIRECTREF / HTTP/1.1\r\nHost: evil.example.com\r\n\r\n"} {
		_, err := ReadName(holds(t, line))
		require.ErrorIs(t, err, ErrUnreadable, "%q", line)
	}
}

// A request whose client ends its head before the head's end may name a Host
// keploy never saw: unreadable. Bytes that ended before a whole request line
// are the read's end, for the relay to pass on.
func TestReadNameOfARequestHeadCutShort(t *testing.T) {
	_, err := ReadName(writes(t, "GET / HTTP/1.1\r\nHost: evil.example.com\r\n"))
	require.ErrorIs(t, err, ErrUnreadable)
	_, err = ReadName(writes(t, "STAT"))
	require.ErrorIs(t, err, io.EOF)
	require.NotErrorIs(t, err, ErrNoName)

	// A ClientHello its client ended before its end may name a server
	// keploy never saw.
	hello := clientHelloBytes(t, "evil.example.com")
	_, err = ReadName(writes(t, string(hello[:len(hello)/2])))
	require.ErrorIs(t, err, ErrUnreadable)
}

// clientHelloBytes is a whole ClientHello record for serverName.
func clientHelloBytes(t *testing.T, serverName string) []byte {
	t.Helper()
	c, s := net.Pipe()
	t.Cleanup(func() { _ = c.Close(); _ = s.Close() })
	go func() {
		_ = tls.Client(c, &tls.Config{ServerName: serverName, InsecureSkipVerify: true}).Handshake() // nolint:gosec
	}()
	head := make([]byte, 5)
	_, err := io.ReadFull(s, head)
	require.NoError(t, err)
	body := make([]byte, int(head[3])<<8|int(head[4]))
	_, err = io.ReadFull(s, body)
	require.NoError(t, err)
	return append(head, body...)
}

// The run closing a connection mid-head is not its client ending the head:
// not unreadable.
func TestReadNameOfAHeadTheRunClosed(t *testing.T) {
	c, srv := net.Pipe()
	t.Cleanup(func() { _ = c.Close() })
	wrote := make(chan struct{})
	go func() {
		_, _ = c.Write([]byte("GET / HTTP/1.1\r\nHost: api.example.com\r\n"))
		close(wrote)
	}()
	done := make(chan error, 1)
	go func() {
		_, err := ReadName(bufio.NewReaderSize(srv, ReaderSize))
		done <- err
	}()
	<-wrote // the head's bytes are in ReadName's buffer
	_ = srv.Close()
	err := <-done
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrUnreadable)
}

// However a request line's bytes arrive, whole or a byte at a time, it is
// decided as it is whole: never "neither" in pieces when it is checked
// whole. (looksLikeRequestLine documents what a split may still decide
// otherwise: a lowercase extension method, and targets keploy does not read
// as one; waiting on those would hang a client that sends a word.)
func TestReadNameDecidesARequestLineAsWholeHoweverItArrives(t *testing.T) {
	lines := []string{
		"GET / HTTP/1.1\r\nHost: evil.example.com\r\n\r\n",
		"get /x HTTP/1.1\r\nHost: evil.example.com\r\n\r\n",
		"GET ftp://evil.example.com/f HTTP/1.1\r\nHost: evil.example.com\r\n\r\n",
		"GET ws://evil.example.com/ HTTP/1.1\r\nHost: evil.example.com\r\n\r\n",
		"GET http://evil.example.com/ HTTP/1.1\r\nHost: evil.example.com\r\n\r\n",
		"OPTIONS * HTTP/1.1\r\nHost: evil.example.com\r\n\r\n",
		"CONNECT evil.example.com:443 HTTP/1.1\r\n\r\n",
		"VERSION-CONTROL / HTTP/1.1\r\nHost: evil.example.com\r\n\r\n",
		"PURGE ws://h/ HTTP/1.1\r\nHost: evil.example.com\r\n\r\n",
		"M-SEARCH * HTTP/1.1\r\nHost: evil.example.com\r\n\r\n",
		"QUERY /q HTTP/1.1\r\nHost: evil.example.com\r\n\r\n",
		"\r\nPOST /x HTTP/1.0\r\nHost: evil.example.com\r\nContent-Length: 0\r\n\r\n",
	}
	outcome := func(n Name, err error) string {
		switch {
		case err == nil:
			return n.Via + " " + n.Host
		case errors.Is(err, ErrUnreadable):
			return "unreadable"
		case errors.Is(err, ErrNoName):
			return "noname"
		}
		return err.Error()
	}
	for _, line := range lines {
		whole := outcome(ReadName(holds(t, line)))
		require.NotEqual(t, "noname", whole, "%q", line)
		var bytes []string
		for i := range len(line) {
			bytes = append(bytes, line[i:i+1])
		}
		require.Equal(t, whole, outcome(ReadName(holds(t, bytes...))), "%q a byte at a time", line)
	}
}

// A request line with a method token longer than 32 characters, sent whole,
// is plainly HTTP keploy does not read: refused, never relayed unchecked.
func TestReadNameOfALongMethodIsUnreadable(t *testing.T) {
	_, err := ReadName(holds(t, strings.Repeat("G", 40)+" / HTTP/1.1\r\nHost: evil.example.com\r\n\r\n"))
	require.ErrorIs(t, err, ErrUnreadable)
}
