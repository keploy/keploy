package util

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// IsInterimStatus reports whether code is an interim response's (RFC 9110
// 15.2): a 1xx other than 101 (Switching Protocols). An interim response has
// no body, and the final response to the same request comes after it on the
// connection. After a 101 the connection speaks another protocol: it is final.
func IsInterimStatus(code int) bool {
	return code >= 100 && code < 200 && code != http.StatusSwitchingProtocols
}

// ReadFinalResponse reads the response to req from r: the final one, past the
// interim responses an HTTP/1.1 server may send before it (a 100 Continue to
// a request that expects one, a 102 Processing, a 103 Early Hints), which a
// client must take, however many come (RFC 9110 15.2). net/http's
// ReadResponse returns the first of them as if it were the answer, and leaves
// the answer on the connection for whatever reads it next.
//
// onInterim, if not nil, is given each interim response in the order they
// come, before the next is read; an error from it ends the read, and is
// returned as it is. A 101 is returned as the final response. Interim
// responses past maxInterimHeaderBytes end the read with
// errTooManyInterimResponses.
func ReadFinalResponse(r *bufio.Reader, req *http.Request, onInterim func(*http.Response) error) (*http.Response, error) {
	interimBytes := 0
	for {
		resp, err := http.ReadResponse(r, req)
		if err != nil || !IsInterimStatus(resp.StatusCode) {
			return resp, err
		}
		if interimBytes += headerSize(resp); interimBytes > maxInterimHeaderBytes {
			return nil, errTooManyInterimResponses
		}
		if onInterim != nil {
			if err := onInterim(resp); err != nil {
				return nil, err
			}
		}
	}
}

// maxInterimHeaderBytes bounds the interim responses ReadFinalResponse reads
// before the final one, by the size of their status lines and headers: the
// bound Go's client puts on them (Transport.MaxResponseHeaderBytes, 10 MiB by
// default, which counts them with the final response's headers), and so the
// bound of keploy's replay client. Without one, an app that sends interim
// responses without end held the read, and each was forwarded on.
const maxInterimHeaderBytes = 10 << 20

var errTooManyInterimResponses = fmt.Errorf("interim (1xx) responses past %d MiB of headers without a final response", maxInterimHeaderBytes>>20)

// headerSize is the size of resp's status line and header section as they
// came, near enough: the header is parsed, its folding and spacing gone.
func headerSize(resp *http.Response) int {
	n := len(resp.Proto) + 1 + len(resp.Status) + len("\r\n\r\n")
	for name, values := range resp.Header {
		for _, v := range values {
			n += len(name) + len(": ") + len(v) + len("\r\n")
		}
	}
	return n
}

// WriteInterimResponse writes resp, an interim response, to w: its status
// line and its header section, in one write. It has no body. The header is
// net/http's parse of the app's, written back with Header.Write: the same
// fields and values, in canonical case and sorted by name, not the app's bytes
// to the letter. It is not written with net/http's Response.Write, which gives
// the 100 Continue to a POST a Content-Length that no 1xx may carry (RFC 9110
// 8.6).
func WriteInterimResponse(w io.Writer, resp *http.Response) error {
	code := strconv.Itoa(resp.StatusCode)
	reason := strings.TrimPrefix(strings.TrimPrefix(resp.Status, code), " ")
	var b bytes.Buffer
	fmt.Fprintf(&b, "HTTP/%d.%d %s %s\r\n", resp.ProtoMajor, resp.ProtoMinor, code, reason)
	if err := resp.Header.Write(&b); err != nil {
		return err
	}
	b.WriteString("\r\n")
	_, err := w.Write(b.Bytes())
	return err
}
