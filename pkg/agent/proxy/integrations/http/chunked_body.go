package http

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// errMalformedChunkedBody is returned when a chunked message body does not
// follow RFC 9112 §7.1 framing, so its end cannot be located.
var errMalformedChunkedBody = errors.New("malformed chunked message body")

// maxChunkedLineLen bounds a chunk-size line or trailer field line that has not
// yet seen its line ending. A peer that streams an unterminated line would
// otherwise make every later framing pass rescan an ever-growing buffer.
const maxChunkedLineLen = 64 << 10

// maxChunkSize caps the size of one chunk. HTTP peers send chunks of kilobytes
// to megabytes, so a larger chunk-size is taken as malformed. It fits an int on
// every platform, so a chunk size is exact as an int on a 32-bit build too.
const maxChunkSize = math.MaxInt32

// legacyChunkedTerminator is what the chunked readers used to take for the end
// of a body when it ended the bytes read so far. It is only a fallback now, for
// a RELAY whose peer does not frame its chunked body by the RFC (see
// chunkedBody.completeOrLegacy).
var legacyChunkedTerminator = []byte("0\r\n\r\n")

// chunkedBody locates the end of a chunked message body (RFC 9112 §7.1) as its
// bytes arrive in arbitrary pieces.
//
// A chunked body ends with the last-chunk (a chunk-size of zero), an optional
// trailer section and an empty line. Where that falls cannot be read off the
// bytes that happen to arrive last: the final "0\r\n" and "\r\n" can come in
// separate reads (Java's HttpURLConnection writes them separately, so a
// chunked-streaming upload's terminator arrives split), a trailer puts fields
// between them, and chunk data can itself end in "0\r\n\r\n" (a multipart part
// header "Content-Length: 10\r\n\r\n"). Every chunked reader here used to test
// for that suffix instead, on the last read or on the whole buffer, and so
// either waited forever on a message that had ended or cut one short. Framing
// by the chunk sizes has neither failure.
//
// Framing is incremental: each call resumes at the element after the last one
// it framed, so the cost of a body is proportional to its number of chunks, not
// to the number of reads times its size.
type chunkedBody struct {
	// response frames a response: interim 1xx header blocks before the final
	// response are skipped (they carry no body).
	response bool
	started  bool // the header section has been found; start is set
	start    int  // offset of the body in the message
	off      int  // offset in the body of the next element to frame

	inTrailer bool // the last-chunk has been framed; off is in the trailer section
	done      bool
	end       int // the body's length, once done

	// legacy is set once framing failed on a relay: from then on the end is
	// judged by the legacy terminator suffix (completeOrLegacy).
	legacy bool
}

// complete frames the chunked body of msg — a whole HTTP/1.x message received
// so far: start line, header section, body — and reports whether it has all
// arrived. msg must only grow between calls.
func (c *chunkedBody) complete(msg []byte) (bool, error) {
	if !c.started {
		_, start, _, ok := messageHead(msg, c.response)
		if !ok {
			return false, nil
		}
		c.start, c.started = start, true
	}
	return c.feed(msg[c.start:])
}

// completeOrLegacy is complete for a proxy that RELAYS the message while it
// reads it (record mode): the app's live traffic must keep flowing even when
// the peer does not frame its chunked body by the RFC. The first framing error
// is returned once, for the caller to log, and from then on the end is judged
// as before this framing existed — the bytes read so far ending in
// "0\r\n\r\n" — or the peer closing.
func (c *chunkedBody) completeOrLegacy(msg []byte) (done bool, framingErr error) {
	if c.legacy {
		return bytes.HasSuffix(msg, legacyChunkedTerminator), nil
	}
	done, err := c.complete(msg)
	if err != nil {
		c.legacy = true
		return bytes.HasSuffix(msg, legacyChunkedTerminator), err
	}
	return done, nil
}

// feed reports whether body — every body byte received so far, body[0] being
// the first byte after the header section — holds the complete chunked body.
// Once it does, the body's length is c.end; bytes past it belong to whatever
// follows on the connection. body must only grow between calls.
func (c *chunkedBody) feed(body []byte) (bool, error) {
	for !c.done {
		line, next, ok, err := chunkedLine(body, c.off)
		if err != nil {
			return false, err
		}
		if !ok {
			if len(body)-c.off > maxChunkedLineLen {
				return false, fmt.Errorf("%w: a line longer than %d bytes has no line ending", errMalformedChunkedBody, maxChunkedLineLen)
			}
			return false, nil
		}
		if c.inTrailer {
			// Trailer section: field lines until the empty line that ends the
			// message.
			c.off = next
			if len(line) == 0 {
				c.done, c.end = true, next
			}
			continue
		}
		size, err := parseChunkSize(line)
		if err != nil {
			return false, err
		}
		if size == 0 {
			c.off, c.inTrailer = next, true
			continue
		}
		if size > math.MaxInt-2-next {
			// Reachable only on a 32-bit build, where the chunk's data and
			// its CRLF could take the offset past the largest int.
			return false, fmt.Errorf("%w: chunk size %d is larger than any body this reader can hold", errMalformedChunkedBody, size)
		}
		dataEnd := next + size
		if len(body) < dataEnd+2 {
			return false, nil
		}
		if body[dataEnd] != '\r' || body[dataEnd+1] != '\n' {
			return false, fmt.Errorf("%w: chunk data is not followed by CRLF", errMalformedChunkedBody)
		}
		c.off = dataEnd + 2
	}
	return true, nil
}

// chunkedLine returns the CRLF-terminated line starting at off without its
// line ending, and the offset just past it; ok is false while the line is
// incomplete. A bare LF is malformed, as net/http has treated it since Go
// 1.23.8 and 1.24.2: accepting what a strict peer rejects would let the two
// disagree on where the message ends.
func chunkedLine(body []byte, off int) (line []byte, next int, ok bool, err error) {
	i := bytes.IndexByte(body[off:], '\n')
	if i < 0 {
		return nil, 0, false, nil
	}
	if i == 0 || body[off+i-1] != '\r' {
		return nil, 0, false, fmt.Errorf("%w: a line ends with a bare LF", errMalformedChunkedBody)
	}
	return body[off : off+i-1], off + i + 1, true, nil
}

// parseChunkSize parses a chunk-size line: hex digits, then optional
// whitespace and chunk extensions (";name=value"), which are ignored. A size
// above maxChunkSize is malformed.
func parseChunkSize(line []byte) (int, error) {
	if i := bytes.IndexByte(line, ';'); i >= 0 {
		line = line[:i]
	}
	line = bytes.TrimRight(line, " \t")
	if len(line) == 0 {
		return 0, fmt.Errorf("%w: empty chunk-size line", errMalformedChunkedBody)
	}
	size, err := strconv.ParseUint(string(line), 16, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: chunk-size %q", errMalformedChunkedBody, line)
	}
	if size > maxChunkSize {
		return 0, fmt.Errorf("%w: chunk size %d is larger than %d", errMalformedChunkedBody, size, maxChunkSize)
	}
	return int(size), nil
}

// messageHead finds the header section that governs msg's body: the request's
// own, or for a response the first one that is not an interim 1xx response (a
// 103 Early Hints or an unsolicited 100 Continue can precede the final response
// in the same read, and has no body). It returns that section (start line and
// header fields), the offset of the body after it, the response status (0 for
// a request), and ok=false while that section has not fully arrived. Empty
// lines before a request line are skipped, as requestMethod skips them (RFC
// 9112 §2.2): an old client sends a CRLF after a POST body, and two of them in
// front of the next request are no empty header section.
func messageHead(msg []byte, response bool) (head []byte, bodyStart int, status int, ok bool) {
	pos := 0
	if !response {
		pos = len(msg) - len(bytes.TrimLeft(msg, "\r\n"))
	}
	for {
		i := bytes.Index(msg[pos:], []byte("\r\n\r\n"))
		if i < 0 {
			return nil, 0, 0, false
		}
		head, bodyStart = msg[pos:pos+i+4], pos+i+4
		if !response {
			return head, bodyStart, 0, true
		}
		status = responseStatus(head)
		if status >= 100 && status < 200 && status != 101 {
			pos = bodyStart // an interim response: the final one follows
			continue
		}
		return head, bodyStart, status, true
	}
}

// finalResponse is resp from its final response on: the interim 1xx
// responses in front of it (a 100 Continue to an "Expect: 100-continue"
// request, a 103 Early Hints) are dropped. The mock records the answer to the
// request, not a 100 with an empty body. resp is returned as is when its final
// header section has not arrived.
func finalResponse(resp []byte) []byte {
	head, bodyStart, _, ok := messageHead(resp, true)
	if !ok {
		return resp
	}
	return resp[bodyStart-len(head):]
}

// responseStatus is the status code of an HTTP/1.x response head, or 0 when
// its status line does not parse.
func responseStatus(head []byte) int {
	line := head
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	fields := bytes.Fields(line)
	if len(fields) < 2 || !bytes.HasPrefix(fields[0], []byte("HTTP/")) {
		return 0
	}
	code, err := strconv.Atoi(string(fields[1]))
	if err != nil {
		return 0
	}
	return code
}

// responseHasNoBody reports whether a response carries no body whatever its
// headers say (RFC 9112 §6.3): the answer to a HEAD, an interim 1xx, 204 or
// 304. Not a 101, nor a 2xx to a CONNECT: their message has no body either,
// but the connection then carries another protocol, which is no next HTTP
// message.
func responseHasNoBody(reqMethod string, status int) bool {
	return reqMethod == "HEAD" || (status >= 100 && status < 200 && status != 101) || status == 204 || status == 304
}

// startsWithInterimResponse reports whether resp begins with an interim 1xx
// response (a 100 Continue, a 103 Early Hints) rather than its final one.
func startsWithInterimResponse(resp []byte) bool {
	status := responseStatus(resp)
	return status >= 100 && status < 200 && status != 101
}

// requestMethod is the method token of an HTTP/1.x request, or "". Empty lines
// before the request line are skipped, as a server does (RFC 9112 §2.2).
func requestMethod(req []byte) string {
	req = bytes.TrimLeft(req, "\r\n")
	if i := bytes.IndexByte(req, ' '); i > 0 {
		return string(req[:i])
	}
	return ""
}

// dropEmptyLinesBeforeRequest removes the empty lines in front of req's request
// line and returns how many bytes that was. They are not part of the request
// (RFC 9112 §2.2: a server ignores at least one), and net/http.ReadRequest,
// which replay and both record paths parse a framed request with, does not skip
// them: it fails on them with `malformed HTTP request ""`. The bytes have
// already been relayed where the proxy relays; this only keeps them out of
// the request the proxy parses and records.
func dropEmptyLinesBeforeRequest(req *[]byte) int {
	n := len(*req) - len(bytes.TrimLeft(*req, "\r\n"))
	*req = (*req)[n:]
	return n
}
