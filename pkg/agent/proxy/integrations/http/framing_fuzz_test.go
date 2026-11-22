package http

import (
	"bufio"
	"bytes"
	"net/http"
	"net/textproto"
	"strings"
	"testing"
)

// framing is how a reader frames a message's body: refused (passed through
// whole), or the offset its body starts at and how the body ends.
type framing struct {
	refused   bool
	bodyStart int
	noBody    bool // a response that has no body whatever its fields say
	length    int  // -1: none (a response's body runs to the close)
	chunked   bool
}

// keployRequestFraming is the framing readRequestV2 gives msg, a request from
// its first byte: the header section messageHead finds, framed by
// parseHeaders. A request with no framing field has no body.
func keployRequestFraming(msg []byte) framing {
	head, bodyStart, _, ok := messageHead(msg, false)
	if !ok {
		return framing{refused: true}
	}
	cl, chunked, err := parseHeaders(head, false)
	if err != nil {
		return framing{refused: true, bodyStart: bodyStart}
	}
	if cl < 0 && !chunked {
		cl = 0
	}
	return framing{bodyStart: bodyStart, length: cl, chunked: chunked}
}

// keployResponseFraming is the framing readResponseWrites gives msg, the
// answer to a method request from its first byte: its final header section,
// past the interim 1xx responses messageHead frames by their blank lines, a
// 101's as its head alone (framedHeadOnly).
func keployResponseFraming(msg []byte, method string) framing {
	head, bodyStart, status, ok := messageHead(msg, true)
	if !ok {
		return framing{refused: true}
	}
	cl, chunked, err := parseHeaders(head, true)
	if err != nil {
		return framing{refused: true, bodyStart: bodyStart}
	}
	if framedHeadOnly(method, status) {
		return framing{bodyStart: bodyStart, noBody: true}
	}
	return framing{bodyStart: bodyStart, length: cl, chunked: chunked}
}

// netHTTPRequestFraming is the framing net/http's ReadRequest gives msg,
// after the empty lines keploy drops in front of a request (emptyLinesBefore;
// net/http's server drops them after a POST). keployRefuses is whether msg is
// one keploy refuses though net/http frames it (see parseHeaders): its
// framing is not one every peer shares.
func netHTTPRequestFraming(msg []byte) (f framing, keployRefuses bool) {
	lead := len(msg) - len(bytes.TrimLeft(msg, "\r\n"))
	r := bytes.NewReader(msg[lead:])
	br := bufio.NewReaderSize(r, len(msg)+16)
	req, err := http.ReadRequest(br)
	if err != nil {
		return framing{refused: true}, false
	}
	f.bodyStart = len(msg) - r.Len() - br.Buffered()
	switch {
	case len(req.TransferEncoding) > 0:
		f.length, f.chunked = -1, true
	case req.Body == http.NoBody:
		f.length = 0
	default:
		f.length = int(req.ContentLength)
	}
	return f, sharedFramingFails(msg[lead:f.bodyStart], req.ProtoMajor, req.ProtoMinor)
}

// netHTTPResponseFraming is the framing net/http's client gives msg's final
// response, the answer to a method request. keploy frames an interim 1xx
// response by its blank lines whatever its fields, so that one net/http will
// not read does not cost the final response behind it
// (TestRecordV2_FinalResponseBehindAMalformedInterim): net/http reads the
// final response from where messageHead finds it.
func netHTTPResponseFraming(msg []byte, method string) (f framing, keployRefuses bool) {
	head, bodyStart, _, ok := messageHead(msg, true)
	if !ok {
		return framing{refused: true}, false
	}
	start := bodyStart - len(head)
	r := bytes.NewReader(msg[start:])
	br := bufio.NewReaderSize(r, len(msg)+16)
	resp, err := http.ReadResponse(br, &http.Request{Method: method})
	if err != nil {
		return framing{refused: true}, false
	}
	f.bodyStart = len(msg) - r.Len() - br.Buffered()
	switch {
	case method == http.MethodHead || resp.StatusCode/100 == 1 || resp.StatusCode == 204 || resp.StatusCode == 304:
		f.noBody = true
	case len(resp.TransferEncoding) > 0:
		f.length, f.chunked = -1, true
	default:
		f.length = int(resp.ContentLength)
	}
	return f, sharedFramingFails(msg[start:f.bodyStart], resp.ProtoMajor, resp.ProtoMinor)
}

// sharedFramingFails reports whether section, a header section net/http
// reads, is one whose framing net/http takes but not every peer shares, so
// keploy refuses it (parseHeaders): a Content-Length or Transfer-Encoding
// field with a space before its colon (net/http's client reads another field,
// its server refuses the request), or a Transfer-Encoding below HTTP/1.1
// (net/http ignores it; RFC 9112 §6.1 calls the framing faulty). It reads the
// raw fields with net/textproto: net/http drops the Transfer-Encoding it
// ignores.
func sharedFramingFails(section []byte, major, minor int) bool {
	tp := textproto.NewReader(bufio.NewReader(bytes.NewReader(section)))
	if _, err := tp.ReadLine(); err != nil {
		return false
	}
	fields, err := tp.ReadMIMEHeader()
	if err != nil {
		return false
	}
	for name := range fields {
		trimmed := http.CanonicalHeaderKey(strings.TrimRight(name, " "))
		if trimmed != name && (trimmed == "Content-Length" || trimmed == "Transfer-Encoding") {
			return true
		}
	}
	if major == 0 && minor == 0 {
		major, minor = 1, 1 // net/http frames HTTP/0.0 as HTTP/1.1
	}
	_, coded := fields["Transfer-Encoding"]
	return coded && (major < 1 || major == 1 && minor < 1)
}

// checkFraming fails t unless keploy's framing of msg is net/http's: the same
// refusal, and for a message both frame the same body start (both end its
// header section at the same empty line) and the same framing. keploy refuses
// as well a message keployRefuses names.
func checkFraming(t *testing.T, msg []byte, got, want framing, keployRefuses bool) {
	t.Helper()
	if !want.refused && keployRefuses {
		want = framing{refused: true, bodyStart: got.bodyStart}
	}
	if got.refused || want.refused {
		got.bodyStart, want.bodyStart = 0, 0
	}
	if got != want {
		t.Fatalf("framing of %q: keploy %+v, net/http %+v", msg, got, want)
	}
}

// lineReaderSectionEnd is the offset just past the header section msg starts
// with as net/textproto's line reader ends it, or -1 while it has not ended:
// past the start line and the lines after it up to the first one its reader
// reads as empty. It drops only the CR right before a line's LF, so a line
// that starts with a CR and holds more ("\rX-A: b", "\r\r") is none.
func lineReaderSectionEnd(msg []byte) int {
	br := bufio.NewReaderSize(bytes.NewReader(msg), len(msg)+16)
	tp := textproto.NewReader(br)
	if _, err := tp.ReadLine(); err != nil { // the start line
		return -1
	}
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return -1
		}
		if line == "" {
			return len(msg) - br.Buffered()
		}
	}
}

// checkSectionEnd fails t unless headerSectionEnd ends the header section msg
// starts with where net/textproto's line reader ends it (lineReaderSectionEnd),
// for a message either reader refuses too: checkFraming compares no body
// start of a refused message, and keploy frames an interim by its blank lines
// whatever its fields.
func checkSectionEnd(t *testing.T, msg []byte) {
	t.Helper()
	if got, want := headerSectionEnd(msg), lineReaderSectionEnd(msg); got != want {
		t.Fatalf("headerSectionEnd(%q) = %d; net/textproto's line reader ends that section at %d", msg, got, want)
	}
}

// fuzzSeeds are messages built from every row of the oracles: each
// netHTTPFramingRows row as a response and as a request, and the folded-line
// rows.
func fuzzSeeds(response bool) [][]byte {
	const body = "5\r\nhello\r\n0\r\n\r\n"
	var seeds [][]byte
	for _, row := range netHTTPFramingRows {
		if response {
			seeds = append(seeds, []byte("HTTP/"+row.version+" 200 OK\r\n"+row.fields+"\r\n"+body))
		} else {
			seeds = append(seeds, []byte("POST /a HTTP/"+row.version+"\r\n"+row.fields+"Host: up\r\n\r\n"+body))
		}
	}
	for _, s := range []string{
		"X-A: b\r\n Content-Length: -5\r\n",
		"X-A: b\r\n\tContent-Length: 7\r\n",
		"Content-Length: 5\r\nX-A: b\r\n\tTransfer-Encoding: chunked\r\n",
		"Content-Length:\r\n 5\r\n",
		"Content-Length: 1\r\n 0\r\n",
		"Transfer-Encoding: gzip,\r\n\tchunked\r\n",
		"X-A: b\n Content-Length: 3\nContent-Length:\n\t4\n",
		"Content-Length: 5\r\nbogus\r\n Content-Length: 7\r\n",
		"Expect: 100-continue\r\nContent-Length: 5\r\n",
		"Content-Length: 5\r\nTransfer-Encoding: chunked\r\nTrailer: X-A\r\n",
		"X-A: b\nContent-Length: 5\n",
		"X-A: b\n\nContent-Length: 5\r\n",
		"Content-Length: 5\r\n\r\r\n",
		"\rContent-Length: 5\r\n",
		"Transfer-Encoding: chunked\r\n \n",
	} {
		if response {
			seeds = append(seeds, []byte("HTTP/1.1 200 OK\r\n"+s+"\r\n"+body))
		} else {
			seeds = append(seeds, []byte("POST /a HTTP/1.1\r\nHost: up\r\n"+s+"\r\n"+body))
		}
	}
	if response {
		for _, s := range []string{
			"HTTP/1.1 304 Not Modified\r\nContent-Length: 5\r\nContent-Length: 6\r\n\r\n",
			"HTTP/1.1 204 No Content\r\nTransfer-Encoding: chunked\r\n\r\n",
			"HTTP/1.1 103 Early Hints\r\nLink: </a>\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello",
			"HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n" + body,
			"HTTP/1.1 103 Early Hints\r\nbogus\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello",
			"HTTP/1.1 103 Early Hints\r\n\rLink: </a>\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello",
			"HTTP/1.0 200 OK\r\n\r\nhello",
			"HTTP/1.1 200\r\nContent-Length: 5\r\n\r\nhello",
			"HTTP/1.1 2000 OK\r\nContent-Length: 5\r\n\r\nhello",
			"\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello",
			"HTTP/1.1 100 Continue\r\n\r\n\r\n\r\n",
			"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n\x81\x05hello",
			"HTTP/1.1 101 Switching Protocols\r\nContent-Length: 5\r\nUpgrade: echo\r\n\r\nhello",
			"HTTP/1.1 101 Switching Protocols\nTransfer-Encoding: chunked\nUpgrade: echo\n\n" + body,
			"HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 101 Switching Protocols\r\nUpgrade: echo\r\n\r\n",
			"HTTP/1.1 200 OK\nContent-Length: 5\n\nhello",
			"HTTP/1.1 100 Continue\n\nHTTP/1.1 200 OK\nTransfer-Encoding: chunked\n\n" + body,
			"HTTP/1.0 200 OK\n\nhello",
			"\n\nHTTP/1.1 200 OK\nContent-Length: 5\n\nhello",
		} {
			seeds = append(seeds, []byte(s))
		}
	} else {
		for _, s := range []string{
			"\r\nGET / HTTP/1.1\r\nHost: a\r\n\r\n",
			"GET http://a/b?c HTTP/1.1\r\nHost: a\r\nHost: b\r\n\r\n",
			"CONNECT a:443 HTTP/1.1\r\nContent-Length: 5\r\n\r\nhello",
			"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n",
			"GET / HTTP/1.1\nHost: a\n\n",
			"\n\nPOST /a HTTP/1.1\nHost: a\nContent-Length: 5\n\nhello",
			"POST /a HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\n\n" + body,
			"GET /ws HTTP/1.1\r\nHost: a\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n\x81\x05hello",
		} {
			seeds = append(seeds, []byte(s))
		}
	}
	return seeds
}

// FuzzRequestFraming holds keploy's framing of a request to net/http's
// ReadRequest's, and the end of its first header section to net/textproto's,
// over any bytes: the seeds are every oracle row.
func FuzzRequestFraming(f *testing.F) {
	for _, s := range fuzzSeeds(false) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, msg []byte) {
		checkSectionEnd(t, msg)
		want, keployRefuses := netHTTPRequestFraming(msg)
		checkFraming(t, msg, keployRequestFraming(msg), want, keployRefuses)
	})
}

// FuzzResponseFraming holds keploy's framing of a response to net/http's
// client's, as the answer to a GET or to a HEAD, and the end of its first
// header section to net/textproto's, over any bytes.
func FuzzResponseFraming(f *testing.F) {
	for _, s := range fuzzSeeds(true) {
		f.Add(s, false)
		f.Add(s, true)
	}
	f.Fuzz(func(t *testing.T, msg []byte, head bool) {
		method := http.MethodGet
		if head {
			method = http.MethodHead
		}
		checkSectionEnd(t, msg)
		want, keployRefuses := netHTTPResponseFraming(msg, method)
		checkFraming(t, msg, keployResponseFraming(msg, method), want, keployRefuses)
	})
}
