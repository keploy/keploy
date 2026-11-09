package http

import (
	"bufio"
	"bytes"
	"net/http"
	"testing"
)

// net/textproto's reader ends a line at an LF, the one CR before it dropped,
// and a header section at its first empty line: "\r\n" or a bare "\n". keploy
// looked for "\r\n\r\n" alone, so it read a section whose lines end at a bare
// LF on past its end, into the body and the messages after it. These rows hold
// messageHead's end of a section to where net/http's reader ends it.
func TestMessageHeadEndsASectionWhereNetHTTPDoes(t *testing.T) {
	for _, row := range []struct {
		name, msg string
		response  bool
	}{
		{name: "a response whose lines end at a bare LF", response: true,
			msg: "HTTP/1.1 200 OK\nContent-Length: 5\n\nhello"},
		{name: "a response that mixes the two line ends", response: true,
			msg: "HTTP/1.1 200 OK\r\nX-A: b\nContent-Length: 5\r\n\nhello"},
		{name: "a response whose empty line is a bare LF after CRLF lines", response: true,
			msg: "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\nhello"},
		{name: "a response with no field, its start line ended by a bare LF", response: true,
			msg: "HTTP/1.1 204 No Content\n\n"},
		{name: "a bare-LF interim before the final response", response: true,
			msg: "HTTP/1.1 100 Continue\n\nHTTP/1.1 200 OK\nContent-Length: 5\n\nhello"},
		{name: "a folded empty line is no empty line", response: true,
			msg: "HTTP/1.1 200 OK\r\nX-A: b\r\n \nContent-Length: 5\r\n\r\nhello"},
		{name: "a request whose lines end at a bare LF",
			msg: "POST /a HTTP/1.1\nHost: up\nContent-Length: 5\n\nhello"},
		{name: "a request whose empty line is a bare LF after CRLF lines",
			msg: "GET /a HTTP/1.1\r\nHost: up\r\n\nGET /b HTTP/1.1\r\nHost: up\r\n\r\n"},
	} {
		t.Run(row.name, func(t *testing.T) {
			msg := []byte(row.msg)
			// net/http's reader, on the same bytes: where its body starts.
			br := bufio.NewReaderSize(bytes.NewReader(msg), len(msg)+16)
			var err error
			if row.response {
				var resp *http.Response
				for {
					resp, err = http.ReadResponse(br, &http.Request{Method: http.MethodGet})
					if err != nil || resp.StatusCode/100 != 1 || resp.StatusCode == http.StatusSwitchingProtocols {
						break
					}
				}
			} else {
				_, err = http.ReadRequest(br)
			}
			if err != nil {
				t.Fatalf("net/http's reader: %v", err)
			}
			start := len(msg) - br.Buffered()
			head, bodyStart, _, ok := messageHead(msg, row.response)
			if !ok || bodyStart != start || !bytes.HasSuffix(msg[:bodyStart], head) {
				t.Fatalf("messageHead(%q) = (%q, %d, ok %v); net/http's body starts at %d", msg, head, bodyStart, ok, start)
			}
			// A section cut anywhere before its end has not arrived.
			for n := range start {
				if _, _, _, ok := messageHead(msg[:n], row.response); ok {
					t.Fatalf("messageHead(%q) found a section net/http's reader ends at %d", msg[:n], start)
				}
			}
		})
	}
}

// net/textproto's reader drops only the CR right before a line's LF, so a line
// that starts with a CR and holds more ("\rX-A: b", the "\r" of "\r\r\n") is
// no empty line: the section goes on past it to its empty line, net/http
// refuses the line, and keploy refuses the section's framing.
func TestMessageHeadTakesNoLineThatStartsWithACRForTheEmptyOne(t *testing.T) {
	for _, row := range []struct {
		name, msg string
		response  bool
	}{
		{name: "a response's field line that starts with a CR", response: true,
			msg: "HTTP/1.1 200 OK\r\n\rX-A: b\r\nContent-Length: 5\r\n\r\nhello"},
		{name: "a response's line of two CRs", response: true,
			msg: "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\r\n\r\nhello"},
		{name: "a request's field line that starts with a CR",
			msg: "POST /a HTTP/1.1\r\nHost: up\r\n\rContent-Length: 5\r\n\r\nhello"},
	} {
		t.Run(row.name, func(t *testing.T) {
			msg := []byte(row.msg)
			end := len(msg) - len("hello")
			if got := lineReaderSectionEnd(msg); got != end {
				t.Fatalf("net/textproto's line reader ends %q at %d, want %d", msg, got, end)
			}
			br := bufio.NewReader(bytes.NewReader(msg))
			var err error
			if row.response {
				_, err = http.ReadResponse(br, nil)
			} else {
				_, err = http.ReadRequest(br)
			}
			if err == nil {
				t.Fatalf("net/http's reader read %q", msg)
			}
			head, bodyStart, _, ok := messageHead(msg, row.response)
			if !ok || bodyStart != end {
				t.Fatalf("messageHead(%q) = (%q, %d, ok %v); net/textproto ends the section at %d", msg, head, bodyStart, ok, end)
			}
			if _, _, err := parseHeaders(head, row.response); !refusedFraming(err) {
				t.Fatalf("parseHeaders(%q) = %v, want it refused", head, err)
			}
		})
	}
}

// A response that starts with an empty line has an empty status line, which
// net/http's client refuses: its section is that line and the empty one after
// it, and keploy refuses its framing, rather than wait for a section that has
// already ended.
func TestMessageHeadEndsASectionAtAnEmptyStartLine(t *testing.T) {
	for _, msg := range []string{"\n\nHTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello", "\r\n\nHTTP/1.1 200 OK\n\n"} {
		if _, err := http.ReadResponse(bufio.NewReader(bytes.NewReader([]byte(msg))), nil); err == nil {
			t.Fatalf("net/http's client read %q", msg)
		}
		head, _, _, ok := messageHead([]byte(msg), true)
		if !ok || len(head) != len(msg)-len(bytes.TrimLeft([]byte(msg), "\r\n")) {
			t.Fatalf("messageHead(%q) = (%q, ok %v), want its leading empty lines", msg, head, ok)
		}
		if _, _, err := parseHeaders(head, true); !refusedFraming(err) {
			t.Fatalf("parseHeaders(%q) = %v, want it refused", head, err)
		}
	}
}

// A response its client read only in part (responseAsRead) is closed after its
// last whole line, a bare-LF line too, and past its interims, bare-LF ones
// too: net/http's client reads it as far as the client had it.
func TestResponseAsReadClosesABareLFSection(t *testing.T) {
	for in, want := range map[string]string{
		"HTTP/1.1 200 OK\nX-A: b\nX-B: c":                       "HTTP/1.1 200 OK\nX-A: b\n\r\n",
		"HTTP/1.1 100 Continue\n\nHTTP/1.1 200 OK\nX-A: b\nX-B": "HTTP/1.1 200 OK\nX-A: b\n\r\n",
		"HTTP/1.1 200 OK\r\nX-A: b\r\nX-B: c":                   "HTTP/1.1 200 OK\r\nX-A: b\r\n\r\n",
		"HTTP/1.1 200 OK\nContent-Length: 5\n\nhel":             "HTTP/1.1 200 OK\nContent-Length: 5\n\nhel",
	} {
		got := responseAsRead([]byte(in))
		if string(got) != want {
			t.Errorf("responseAsRead(%q) = %q, want %q", in, got, want)
		}
		if _, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(got)), nil); err != nil {
			t.Errorf("net/http's client on responseAsRead(%q) = %q: %v", in, got, err)
		}
	}
}
