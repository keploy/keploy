package realdeps

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// ReaderSize is the size of the buffered reader a connection's first bytes
// are read through: the most of a ClientHello or an HTTP/1 request head keploy
// reads a name from.
const ReaderSize = 64 << 10

// Name is the name a connection's client opened it for, read from its first
// bytes.
type Name struct {
	// Host is the name, as the client wrote it.
	Host string
	// Port is a CONNECT target's port; 0 for a name read at the connection's
	// own port.
	Port uint16
	// Via says where the name was read: "sni", "host" or "connect".
	Via string
	// TLS: the client opened with a TLS ClientHello; H2: it offered h2, so
	// the connection may carry other names under one certificate.
	TLS, H2 bool
}

// ErrNoName is ReadName's error for first bytes that carry no name keploy can
// read: a protocol that is neither TLS nor HTTP/1, a ClientHello without an
// SNI, an HTTP/1 request without a Host, or a head larger than ReaderSize.
var ErrNoName = errors.New("the client's first bytes carry no name keploy can read")

// ErrUnreadable marks an ErrNoName for first bytes that are a TLS ClientHello
// or an HTTP/1 request line keploy could not read whole (a hello crypto/tls
// refuses, a head larger than ReaderSize, a head that does not parse): they
// may carry a name keploy cannot check. Bytes of another protocol, a hello
// without an SNI and a request without a Host are ErrNoName alone: they
// carry no name.
var ErrUnreadable = errors.New("TLS or HTTP/1 keploy could not read")

// ErrH2C marks an ErrNoName for the HTTP/2 prior-knowledge preface (h2c):
// its names travel in HPACK, which keploy does not read, so a connection
// made REAL by its name is relayed with its REAL row's h2 note, and one
// whose name must be read from its bytes has none.
var ErrH2C = errors.New("HTTP/2 prior knowledge (h2c), whose names keploy does not read")

// unreadable is ErrNoName, marked ErrUnreadable, with why.
func unreadable(format string, a ...any) error {
	return fmt.Errorf("%w: %w: "+format, append([]any{ErrNoName, ErrUnreadable}, a...)...)
}

// ReadName reads the name a connection's client opened it for from its first
// bytes, without consuming them: br then still holds every byte, for whatever
// handles the connection next. br must be at least ReaderSize.
//
// It waits for the client's bytes with no clock, and only for bytes that are
// still on their way to being a ClientHello or an HTTP/1 request: it decides
// at the first byte that rules both out. A TLS ClientHello is known by its
// record header and handshake type (16 03 00..04 <length> 01) and read whole;
// an HTTP/1 request by its first line (METHOD target HTTP/1.x), and read to
// the end of its head. A line protocol (SMTP's EHLO, Redis's inline
// commands, NATS's CONNECT) and a binary one (a MySQL packet whose length
// starts 0x16) are ErrNoName as soon as their first line or their first
// bytes are in, never a wait for bytes their client will not send. A client
// that sends nothing keeps it waiting until the client closes the connection
// or the connection is closed under it (the run's end), and the error is the
// read's.
func ReadName(br *bufio.Reader) (Name, error) {
	first, err := br.Peek(1)
	if err != nil {
		return Name{}, err
	}
	switch {
	case first[0] == 0x16:
		ok, err := clientHelloPrefix(br)
		if err != nil {
			return Name{}, err
		}
		if !ok {
			return Name{}, fmt.Errorf("%w: it is neither TLS nor HTTP/1", ErrNoName)
		}
		return readClientHello(br)
	case isLetter(first[0]), first[0] == '\r', first[0] == '\n':
		// Either case: a known method in lowercase ("get") is a request line
		// keploy does not read, never bytes that carry no name
		// (looksLikeRequestLine).
		return readRequestHead(br)
	}
	return Name{}, fmt.Errorf("%w: it is neither TLS nor HTTP/1", ErrNoName)
}

// clientHelloPrefix reports whether br starts with a TLS handshake record
// that holds a ClientHello: content type 22, version 3.0 to 3.4 (TLS 1.3
// says 3.1 or 3.3 here), a length a record may have, handshake type 1, and a
// hello length under 64 KiB. It
// reads one more byte only while the bytes so far still match, so a protocol
// whose first byte is 0x16 is told apart at its second or third byte.
func clientHelloPrefix(br *bufio.Reader) (bool, error) {
	for i := 1; i < 7; i++ {
		b, err := br.Peek(i + 1)
		if err != nil {
			return false, err
		}
		switch i {
		case 1:
			if b[1] != 0x03 {
				return false, nil
			}
		case 2:
			if b[2] > 0x04 {
				return false, nil
			}
		case 4:
			if n := int(b[3])<<8 | int(b[4]); n == 0 || n > 1<<14+2048 {
				return false, nil
			}
		case 5:
			if b[5] != 0x01 {
				return false, nil
			}
		case 6:
			// A ClientHello's 24-bit length: its high byte is 0 (a hello is
			// far shorter than 64 KiB).
			if b[6] != 0x00 {
				return false, nil
			}
		}
	}
	return true, nil
}

// readClientHello reads the SNI and the ALPN offer of the ClientHello at the
// start of br. crypto/tls parses it: a server handshake runs over br's bytes,
// peeked and never consumed, and stops as soon as the hello is read. It
// writes nothing to the client.
func readClientHello(br *bufio.Reader) (Name, error) {
	var hello *tls.ClientHelloInfo
	stop := errors.New("hello read")
	conn := &peekConn{r: &peekReader{br: br}}
	srv := tls.Server(conn, &tls.Config{
		GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			hello = chi
			return nil, stop
		},
	})
	err := srv.Handshake()
	if hello == nil {
		if errors.Is(conn.r.err, bufio.ErrBufferFull) {
			return Name{}, unreadable("its ClientHello is larger than %d bytes", ReaderSize)
		}
		if errors.Is(conn.r.err, io.EOF) {
			// A ClientHello its client ended before its end: it may name
			// a server keploy never saw.
			return Name{}, unreadable("its client ended its ClientHello before the hello's end")
		}
		if conn.r.err != nil {
			return Name{}, conn.r.err
		}
		if err == nil {
			err = errors.New("no ClientHello")
		}
		return Name{}, unreadable("it starts a TLS handshake keploy could not read: %v", err)
	}
	n := Name{Host: hello.ServerName, Via: "sni", TLS: true, H2: slices.Contains(hello.SupportedProtos, "h2")}
	if n.Host == "" {
		return n, fmt.Errorf("%w: its ClientHello has no SNI", ErrNoName)
	}
	return n, nil
}

// readRequestHead reads the Host of the HTTP/1 request head at the start of
// br, or the target of a CONNECT. Empty lines before the request line are
// passed over, as servers do (RFC 9112 §2.2). It decides at the end of the
// request line whether the bytes are HTTP/1 at all: a line that is plainly
// HTTP but not a request line keploy reads (a lowercase method or version,
// trailing spaces, longer than the buffer) is ErrUnreadable, never nameless; the
// HTTP/2 prior-knowledge preface is ErrH2C.
func readRequestHead(br *bufio.Reader) (Name, error) {
	r := &peekReader{br: br}
	var head []byte
	start := -1
	lineDone := false
	for {
		buf, err := r.more()
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				if !lineDone {
					return Name{}, unreadable("its request line is longer than %d bytes", ReaderSize)
				}
				return Name{}, unreadable("its request head is larger than %d bytes", ReaderSize)
			}
			if lineDone && errors.Is(err, io.EOF) {
				// A request whose head its client ended before its end: it
				// may name a Host keploy never saw.
				return Name{}, unreadable("its client ended its HTTP/1 request head before the head's end")
			}
			return Name{}, err
		}
		if start < 0 {
			start = len(buf) - len(bytes.TrimLeft(buf, "\r\n"))
			if start == len(buf) {
				start = -1
				continue
			}
		}
		line := buf[start:]
		if !lineDone {
			i := bytes.IndexByte(line, '\n')
			if i < 0 {
				if !looksLikeRequestLine(line) {
					return Name{}, fmt.Errorf("%w: it is neither TLS nor HTTP/1", ErrNoName)
				}
				continue
			}
			switch first := line[:i+1]; {
			case isRequestLine(first):
			case bytes.HasPrefix(first, []byte("PRI * HTTP/2.0")):
				return Name{H2: true}, fmt.Errorf("%w: %w", ErrNoName, ErrH2C)
			case looseRequestLine.Match(first):
				return Name{}, unreadable("its request line is HTTP keploy does not read: %q", requestLineShape(bytes.TrimRight(first, "\r\n")))
			default:
				return Name{}, fmt.Errorf("%w: it is neither TLS nor HTTP/1", ErrNoName)
			}
			lineDone = true
		}
		if n := headEnd(line); n > 0 {
			head = line[:n]
			break
		}
	}
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(head)))
	if err != nil {
		return Name{}, unreadable("%v", models.HTTPParseError("its HTTP/1 request head", len(head), err))
	}
	if req.Method == http.MethodConnect {
		host, port, ok := splitTarget(req.RequestURI)
		if !ok {
			return Name{}, unreadable("its CONNECT target is not host:port")
		}
		return Name{Host: host, Port: port, Via: "connect"}, nil
	}
	host := hostOnly(req.Host)
	if host == "" {
		return Name{}, fmt.Errorf("%w: its HTTP/1 request has no Host", ErrNoName)
	}
	return Name{Host: host, Via: "host"}, nil
}

// looksLikeRequestLine reports whether b, a line not yet ended, can still
// become an HTTP/1 request line, read or not: METHOD SP TARGET SP HTTP/. The
// decision waits only while it can, so a client that sends anything else and
// waits for its server is decided at once.
//
// The method is an RFC 9110 token of at most 32 characters led by a letter
// (the grammar looseRequestLine reads whole lines by). Before its space, one
// with a lowercase letter is only a known method written in another case, so
// "ruok" (ZooKeeper's four-letter words, sent with no line end) is decided at
// once, while a request line split after "Ge" is still read whole; after its
// space, any token. The target must start as one does: "/", "*", an http or
// https URL, any URI scheme after a known or uppercase method (a forward
// proxy's absolute form), or any authority after CONNECT; and after it,
// "HTTP/" in either case. So "ping me" or "hello world" sent with no line end
// is decided at once too.
//
// What a split can still decide otherwise than the whole line (each relayed
// unchecked on a name-checked connection when split, refused whole): a
// lowercase extension method split before its space ("pur" then "ge / ..."),
// or followed by a non-http absolute target; a method token longer than 32
// characters; a target keploy does not read as one ("[::1]:80", "@h", a
// control byte in it). Waiting on any of these would hang a client that
// sends a word, or a long token, and waits for its answer.
func looksLikeRequestLine(b []byte) bool {
	sp := bytes.IndexByte(b, ' ')
	token := b
	if sp >= 0 {
		token = b[:sp]
	}
	if sp == 0 || len(token) > 32 || !methodPrefix(token, sp >= 0) {
		return false
	}
	if sp < 0 {
		return true
	}
	rest := bytes.TrimLeft(b[sp:], " ")
	if len(rest) == 0 {
		return true
	}
	end := bytes.IndexByte(rest, ' ')
	target := rest
	if end >= 0 {
		target = rest[:end]
	}
	for _, c := range target {
		if c < 0x21 || c == 0x7f {
			return false
		}
	}
	if !targetPrefix(target, token) {
		return false
	}
	if end < 0 {
		return true
	}
	version := bytes.TrimLeft(rest[end:], " ")
	if len(version) > 5 {
		version = version[:5]
	}
	return strings.EqualFold(string(version), "HTTP/"[:len(version)])
}

// methodPrefix reports whether token can start (whole: be) a request line's
// method: an RFC 9110 token (the grammar looseRequestLine reads whole lines
// by) led by a letter. A token not yet ended that has a lowercase letter is
// only a known method's start: a lowercase word a client sends with no space
// after it (ZooKeeper's "ruok") is decided at once. Once a space ends it, any
// token may be a method; the target after it decides (targetPrefix).
func methodPrefix(token []byte, whole bool) bool {
	upper := true
	for i, c := range token {
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
			upper = false
		case i > 0 && isTokenChar(c):
		default:
			return false
		}
	}
	if upper || whole {
		return true
	}
	for _, m := range knownMethods {
		if len(token) <= len(m) && strings.EqualFold(string(token), m[:len(token)]) {
			return true
		}
	}
	return false
}

// isUpper reports whether token has no lowercase letter.
func isUpper(token []byte) bool {
	return !bytes.ContainsFunc(token, func(r rune) bool { return r >= 'a' && r <= 'z' })
}

// isTokenChar reports whether c may be in an RFC 9110 token.
func isTokenChar(c byte) bool {
	return c >= '0' && c <= '9' || isLetter(c) || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

// targetPrefix reports whether target can start a request target of method:
// origin form ("/"), asterisk form ("*"), absolute form (an http or https
// URL; for a known or uppercase method, any URI scheme, as a forward proxy
// reads), or, for CONNECT, authority form (anything). A lowercase unknown
// method's target must start as http's do, so "hello world" sent with no
// line end is decided at once.
func targetPrefix(target, method []byte) bool {
	if strings.EqualFold(string(method), http.MethodConnect) || len(target) == 0 || target[0] == '/' || target[0] == '*' {
		return true
	}
	for _, scheme := range []string{"http://", "https://"} {
		n := min(len(target), len(scheme))
		if strings.EqualFold(string(target[:n]), scheme[:n]) {
			return true
		}
	}
	if !isUpper(method) && !slices.ContainsFunc(knownMethods, func(m string) bool { return strings.EqualFold(string(method), m) }) {
		return false
	}
	// scheme ":" ..., scheme = ALPHA *( ALPHA / DIGIT / "+" / "-" / "." )
	for i, c := range target {
		switch {
		case c == ':' && i > 0:
			return true
		case isLetter(c), i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return true
}

// knownMethods are the HTTP methods a request line written in another case
// is read as: HTTP's own (RFC 9110), PATCH, and WebDAV's.
var knownMethods = []string{
	"GET", "HEAD", "POST", "PUT", "DELETE", "CONNECT", "OPTIONS", "TRACE", "PATCH",
	"PROPFIND", "PROPPATCH", "MKCOL", "COPY", "MOVE", "LOCK", "UNLOCK", "SEARCH", "REPORT", "QUERY",
}

// isLetter reports whether c is an ASCII letter.
func isLetter(c byte) bool { return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' }

// requestLine is an HTTP/1 request line, its line ending included (CRLF, or
// a bare LF, which servers accept). looseRequestLine is any line that is
// plainly an HTTP request line, read or not.
var (
	requestLine      = regexp.MustCompile(`^[A-Z]{1,16} [^ \r\n]+ HTTP/1\.[01]\r?\n$`)
	looseRequestLine = regexp.MustCompile("(?i)^[!#$%&'*+.^_`|~0-9a-z-]+ +[^ \\r\\n]+ +http/")
)

// requestLineShape is a request line keploy does not read, as its error
// names it: the method kept when it is letters and '-' only, the target in
// its shape (models.TargetShape: no path value, no query value), the version
// kept when it reads as one, and the spaces between them as sent, so what
// made the line unreadable (a lowercase method or version, a doubled or
// trailing space, HTTP/3.0) still shows. Anything else in the line is {}.
func requestLineShape(line []byte) string {
	m := requestLineParts.FindSubmatch(line)
	if m == nil {
		return "{}"
	}
	method, version := string(m[1]), string(m[5])
	if !methodWord.MatchString(method) {
		method = "{}"
	}
	if !versionWord.MatchString(version) {
		version = "{}"
	}
	return method + string(m[2]) + models.TargetShape(string(m[3])) + string(m[4]) + version
}

// requestLineParts splits a line looseRequestLine matched into its method,
// the spaces after it, its target, the spaces after that, and the rest.
var (
	requestLineParts = regexp.MustCompile(`^([^ ]+)( +)([^ ]+)( +)(.*)$`)
	methodWord       = regexp.MustCompile(`^[A-Za-z-]{1,16}$`)
	versionWord      = regexp.MustCompile(`(?i)^http/[0-9]{1,2}(\.[0-9]{1,2})? *$`)
)

// isRequestLine reports whether line, a whole line, is an HTTP/1 request
// line.
func isRequestLine(line []byte) bool { return requestLine.Match(line) }

// headEnd is the length of the request head at the start of b, its empty
// line included, or 0 while the head is not whole: the head ends at the
// first line that is empty, CRLF or bare LF.
func headEnd(b []byte) int {
	for i := 0; i < len(b); {
		j := bytes.IndexByte(b[i:], '\n')
		if j < 0 {
			return 0
		}
		line := b[i : i+j+1]
		if i > 0 && len(bytes.TrimRight(line, "\r\n")) == 0 {
			return i + j + 1
		}
		i += j + 1
	}
	return 0
}

// splitTarget reads a CONNECT target, host:port.
func splitTarget(target string) (string, uint16, bool) {
	host, portText, err := net.SplitHostPort(target)
	if err != nil || host == "" {
		return "", 0, false
	}
	p, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || p == 0 {
		return "", 0, false
	}
	return host, uint16(p), true
}

// hostOnly is a Host header's name, without a port.
func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

// peekReader reads br's bytes without consuming them: each Read returns the
// next bytes br holds, waiting for the client only when it has handed out all
// of them.
type peekReader struct {
	br  *bufio.Reader
	off int
	// err is the first error a read of the client returned: crypto/tls
	// wraps it, and ReadName returns it as it is.
	err error
}

// more waits until br holds bytes past what was handed out, and returns all
// of them, from the start.
func (r *peekReader) more() ([]byte, error) {
	n := r.br.Buffered()
	if n <= r.off {
		if _, err := r.br.Peek(r.off + 1); err != nil {
			if r.err == nil {
				r.err = err
			}
			return nil, err
		}
		n = r.br.Buffered()
	}
	buf, err := r.br.Peek(n)
	if err != nil {
		return nil, err
	}
	r.off = n
	return buf, nil
}

func (r *peekReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	start := r.off
	buf, err := r.more()
	if err != nil {
		return 0, err
	}
	n := copy(p, buf[start:])
	r.off = start + n
	return n, nil
}

// peekConn is the connection crypto/tls reads a ClientHello from: reads come
// from the client's peeked bytes, and writes (an alert, when the handshake is
// stopped) go nowhere.
type peekConn struct{ r *peekReader }

func (c *peekConn) Read(p []byte) (int, error)       { return c.r.Read(p) }
func (c *peekConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *peekConn) Close() error                     { return nil }
func (c *peekConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *peekConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *peekConn) SetDeadline(time.Time) error      { return nil }
func (c *peekConn) SetReadDeadline(time.Time) error  { return nil }
func (c *peekConn) SetWriteDeadline(time.Time) error { return nil }

var _ io.Reader = (*peekReader)(nil)
