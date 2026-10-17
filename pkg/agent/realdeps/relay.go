package realdeps

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// maxHead bounds an HTTP/1 request head, and maxLine a chunk-size line, the
// relay reads to check a REAL connection's names.
const (
	maxHead = ReaderSize
	maxLine = 4 << 10
)

// Relay relays a REAL connection: every byte both ways, the bytes keploy
// already read from the client first, with nothing recorded and nothing
// mocked (design v14 §4). No clock ends it: each direction ends on its own
// end. A clean end of one direction is passed on as a half-close, so the peer
// learns of it as it would without keploy; an error in either ends both, as a
// reset would. It ends early when ctx ends (the run is over).
//
// A connection made REAL by its name has its client's names checked as they
// pass (Check): the ClientHello's SNI, each HTTP/1 request's Host, a CONNECT
// target. A name that is not listed ends the connection there, before its
// bytes reach the destination, and Refused says why: capture_incomplete. Only
// those name-carrying bytes are parsed; nothing else is.
type Relay struct {
	// Client is the application's connection, and In what its bytes are
	// read from: a buffered reader over it that may hold bytes already
	// peeked. Dest is the real destination's.
	Client net.Conn
	In     *bufio.Reader
	Dest   net.Conn
	// Check is nil for a connection made REAL by its address or its
	// container: its bytes are not read.
	Check *Check
	// Sent is told of every chunk of the client's bytes before it goes to
	// the destination. It may be nil.
	Sent func(n int)
	// Answered is told of each chunk of bytes the destination sends back: it
	// is reached, whatever the dial was, and again on a pooled connection in
	// every later read of the run's REAL bookkeeping. It may be nil.
	Answered func()
	// H2 is told, after Sent, of every chunk the client sends on a
	// connection whose ClientHello offered h2, or that speaks h2c: a pooled
	// connection carries the requests of many tests, and each of their REAL
	// rows says other names may share it, not only the row of the test open
	// at its handshake. It may be nil.
	H2 func()
	// Refused is told why a name check ended the connection. It may be nil.
	Refused func(why string)
	// AwaitsAnswer is told once the client has ended its side cleanly and
	// every byte of it has gone to the destination: what is left to relay
	// is the destination's answer, the real server's to give, which a
	// test's end does not wait on. What it returns is called once the
	// destination's direction ends. It may be nil.
	AwaitsAnswer func() (release func())

	// h2: the connection offered h2. Set and read by the client's direction
	// only.
	h2 bool
}

// errRefused ends the client's direction when a name check fails.
var errRefused = errors.New("a name check ended the connection")

// Run relays until both directions have ended, closes both connections, and
// returns the first error that ended one: nil when both ended cleanly, or
// when a name check ended the connection (Refused was told).
func (r *Relay) Run(ctx context.Context) error {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = r.Client.Close()
			_ = r.Dest.Close()
		})
	}
	stop := context.AfterFunc(ctx, closeBoth)
	defer stop()

	back := make(chan error, 1)
	go func() {
		var from io.Reader = r.Dest
		if r.Answered != nil {
			from = &answeredReader{r: r.Dest, answered: r.Answered}
		}
		_, err := io.Copy(r.Client, from)
		if err == nil {
			_ = closeWrite(r.Client)
		} else {
			closeBoth()
		}
		back <- err
	}()

	err := r.forward()
	release := func() {}
	switch {
	case err == nil:
		_ = closeWrite(r.Dest)
		// The client's side is over and all of it relayed: the rest is
		// the destination's answer.
		if r.AwaitsAnswer != nil {
			release = r.AwaitsAnswer()
		}
	case errors.Is(err, errRefused):
		closeBoth()
		err = nil
	default:
		closeBoth()
	}
	berr := <-back
	release()
	if err == nil && !errors.Is(berr, net.ErrClosed) {
		err = berr
	}
	// Both directions have ended: the connection is over, and both sockets
	// go now, not when a finalizer finds them.
	closeBoth()
	return err
}

// closeWrite half-closes c when it can.
func closeWrite(c net.Conn) error {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// forward copies the client's bytes to the destination, checking each name
// they carry when the connection was made REAL by its name. nil is a clean
// end of the client's bytes.
func (r *Relay) forward() error {
	w := &sentWriter{w: r.Dest, sent: r.sent}
	if r.Check == nil {
		return copyClean(w, r.In)
	}
	n, err := ReadName(r.In)
	switch {
	case errors.Is(err, ErrUnreadable):
		// TLS or HTTP/1 keploy cannot read whole may carry a name it cannot
		// check: relayed, it could reach a destination mock.real does not
		// list.
		return r.refuse(err.Error())
	case errors.Is(err, ErrNoName):
		// Bytes that carry no name contradict none: the connection keeps
		// the name it was made REAL by. h2c's names are not read: its row
		// says so.
		if errors.Is(err, ErrH2C) {
			r.h2 = true
		}
		return copyClean(w, r.In)
	case errors.Is(err, io.EOF):
		// The client ended its side before its bytes decided anything: they
		// are the start of no whole request line, and of no ClientHello
		// (ReadName refuses a request head or a hello its client ended), so
		// they name nothing, and they go on as they are, with the end. A
		// client that sends a word and half-closes (ZooKeeper's "STAT")
		// gets its answer.
		if n := r.In.Buffered(); n > 0 {
			b, _ := r.In.Peek(n)
			if _, werr := w.Write(b); werr != nil {
				return werr
			}
			_, _ = r.In.Discard(n)
		}
		return nil
	case err != nil:
		return err
	}
	if !n.TLS {
		return r.forwardHTTP(w)
	}
	if !r.allowed(n.Host, 0, "its ClientHello names") {
		return errRefused
	}
	r.h2 = n.H2
	return copyClean(w, r.In)
}

// allowed checks one name the client carries; what says where it was read.
// A name that is not a DNS name (an IP literal) names nothing to check.
func (r *Relay) allowed(host string, port uint16, what string) bool {
	h := CanonicalName(host)
	if h == "" || r.Check.Allows(h, port) {
		return true
	}
	if r.Refused != nil {
		target := h
		if port != 0 {
			target = net.JoinHostPort(h, strconv.Itoa(int(port)))
		}
		r.Refused(fmt.Sprintf("a mock.real connection to %s carried another name: %s %s, which mock.real does not list; keploy ended it (one connection carries one real name: list that name too, or let the client open a connection per name)",
			r.Check.dest(), what, target))
	}
	return false
}

// forwardHTTP relays HTTP/1 requests one by one, checking each head's Host
// (or CONNECT target) before the head reaches the destination. After a
// CONNECT or an upgrade the connection is no longer HTTP/1, and the rest is
// relayed as it is.
func (r *Relay) forwardHTTP(w io.Writer) error {
	for {
		head, err := readHead(r.In)
		if err != nil {
			if errors.Is(err, io.EOF) && len(head) == 0 {
				return nil
			}
			if errors.Is(err, errNotHTTP) {
				return r.refuse(err.Error())
			}
			return err
		}
		// Parsed without the empty lines before it, which servers pass over
		// (RFC 9112 §2.2); forwarded with them, byte for byte.
		req, perr := http.ReadRequest(bufio.NewReader(bytes.NewReader(bytes.TrimLeft(head, "\r\n"))))
		if perr != nil {
			return r.refuse("its bytes stopped being HTTP/1 request heads keploy can check: " + perr.Error())
		}
		raw := false
		if req.Method == http.MethodConnect {
			host, port, ok := splitTarget(req.RequestURI)
			if !ok {
				return r.refuse("its CONNECT target is not host:port")
			}
			if !r.allowed(host, port, "a CONNECT to") {
				return errRefused
			}
			raw = true
		} else {
			if !r.allowed(hostOnly(req.Host), 0, "a request with Host") {
				return errRefused
			}
			raw = upgrades(req)
		}
		if _, err := w.Write(head); err != nil {
			return err
		}
		if raw {
			return copyClean(w, r.In)
		}
		if err := forwardBody(w, r.In, req); err != nil {
			return err
		}
	}
}

// refuse ends the connection for bytes whose names keploy can no longer
// check.
func (r *Relay) refuse(why string) error {
	if r.Refused != nil {
		r.Refused("keploy ended a mock.real connection to " + r.Check.dest() + ": " + why)
	}
	return errRefused
}

// dest names the connection a check is for, for messages.
func (c *Check) dest() string {
	hosts := c.list.hostsOn(c.port)
	return strings.Join(hosts, "|") + ":" + strconv.Itoa(int(c.port))
}

// upgrades reports whether req asks to leave HTTP/1 (Connection: upgrade),
// after which keploy cannot frame the client's bytes.
func upgrades(req *http.Request) bool {
	for _, v := range req.Header.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}

var errNotHTTP = errors.New("its bytes stopped being HTTP/1 request heads keploy can check")

// readHead reads one request head from br, with every empty line before it:
// the bytes up to and including the empty line that ends it. A clean end
// before any byte of it is io.EOF with no bytes.
func readHead(br *bufio.Reader) ([]byte, error) {
	var head []byte
	started := false
	for {
		line, err := readLine(br, maxHead-len(head))
		head = append(head, line...)
		if err != nil {
			if errors.Is(err, io.EOF) && !started && len(bytes.TrimSpace(head)) == 0 {
				return nil, io.EOF
			}
			if errors.Is(err, errLineTooLong) {
				return head, fmt.Errorf("%w (a head of more than %d bytes)", errNotHTTP, maxHead)
			}
			if errors.Is(err, io.EOF) {
				return head, io.ErrUnexpectedEOF
			}
			return head, err
		}
		empty := len(bytes.TrimRight(line, "\r\n")) == 0
		switch {
		case empty && started:
			return head, nil
		case !empty:
			if !started && !isRequestLine(line) {
				return head, errNotHTTP
			}
			started = true
		}
	}
}

var errLineTooLong = errors.New("line too long")

// readLine reads one line, its '\n' included, of at most limit bytes.
func readLine(br *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		frag, err := br.ReadSlice('\n')
		line = append(line, frag...)
		if len(line) > limit {
			return line, errLineTooLong
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}

// forwardBody copies a request's body, framed as its head says: chunked, or
// Content-Length bytes, or none.
func forwardBody(w io.Writer, br *bufio.Reader, req *http.Request) error {
	for _, te := range req.TransferEncoding {
		if strings.EqualFold(te, "chunked") {
			return forwardChunked(w, br)
		}
	}
	if req.ContentLength > 0 {
		_, err := io.CopyN(w, br, req.ContentLength)
		return unexpected(err)
	}
	return nil
}

// forwardChunked copies a chunked body, its trailer included.
func forwardChunked(w io.Writer, br *bufio.Reader) error {
	for {
		line, err := readLine(br, maxLine)
		if err != nil {
			return unexpected(err)
		}
		if _, err := w.Write(line); err != nil {
			return err
		}
		sizeText := strings.TrimSpace(string(bytes.TrimRight(line, "\r\n")))
		if i := strings.IndexByte(sizeText, ';'); i >= 0 {
			sizeText = strings.TrimSpace(sizeText[:i])
		}
		size, err := strconv.ParseInt(sizeText, 16, 64)
		if err != nil || size < 0 {
			return fmt.Errorf("%w: a chunk size %q", errNotHTTP, sizeText)
		}
		if size == 0 {
			for {
				t, err := readLine(br, maxLine)
				if err != nil {
					return unexpected(err)
				}
				if _, err := w.Write(t); err != nil {
					return err
				}
				if len(bytes.TrimRight(t, "\r\n")) == 0 {
					return nil
				}
			}
		}
		if _, err := io.CopyN(w, br, size+2); err != nil {
			return unexpected(err)
		}
	}
}

// unexpected is err, with a clean end in the middle of a body made the error
// it is.
func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// copyClean copies the rest of the client's bytes; a clean end is nil.
func copyClean(w io.Writer, r io.Reader) error {
	_, err := io.Copy(w, r)
	return err
}

// sent tells Sent of n bytes the client sent, and H2 when its connection
// offered h2.
func (r *Relay) sent(n int) {
	if r.Sent != nil {
		r.Sent(n)
	}
	if r.h2 && r.H2 != nil {
		r.H2()
	}
}

// sentWriter writes the client's bytes to the destination and tells of each
// chunk before it goes: once the bytes are written, the destination can
// answer, the app answer its test, and the test end before a write returns,
// so a use told after the write could land in the next test's row.
type sentWriter struct {
	w    io.Writer
	sent func(int)
}

func (s *sentWriter) Write(p []byte) (int, error) {
	if len(p) > 0 && s.sent != nil {
		s.sent(len(p))
	}
	return s.w.Write(p)
}

// answeredReader tells answered of each chunk of bytes it reads.
type answeredReader struct {
	r        io.Reader
	answered func()
}

func (a *answeredReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.answered()
	}
	return n, err
}
