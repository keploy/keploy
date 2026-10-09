package util

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// A response is read up to the final one, past every interim (1xx) response
// in front of it, each given to onInterim in order; a 101 is final. The
// reader is left at the final response's body: the next response on a
// keep-alive connection is where it was.
func TestReadFinalResponse(t *testing.T) {
	for _, c := range []struct {
		name, wire string
		interim    []int
		status     int
		body       string
	}{
		{"none", "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok", nil, 200, "ok"},
		{"100 Continue", "HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 201 Created\r\nContent-Length: 2\r\n\r\nok", []int{100}, 201, "ok"},
		{"several", "HTTP/1.1 102 Processing\r\n\r\nHTTP/1.1 103 Early Hints\r\nLink: </a.css>\r\n\r\nHTTP/1.1 103 Early Hints\r\nLink: </b.js>\r\n\r\nHTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nok\r\n0\r\n\r\n", []int{102, 103, 103}, 200, "ok"},
		{"to a body-less final", "HTTP/1.1 103 Early Hints\r\n\r\nHTTP/1.1 204 No Content\r\n\r\n", []int{103}, 204, ""},
		{"101 is final", "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n\r\n\x81\x02ok", nil, 101, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(c.wire + "HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\nnext"))
			req, _ := http.NewRequest(http.MethodGet, "http://a/", nil)
			var interim []int
			resp, err := ReadFinalResponse(r, req, func(i *http.Response) error {
				interim = append(interim, i.StatusCode)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != c.status || string(body) != c.body {
				t.Fatalf("final = %d %q, want %d %q", resp.StatusCode, body, c.status, c.body)
			}
			if !slices.Equal(interim, c.interim) {
				t.Fatalf("interim responses %v, want %v", interim, c.interim)
			}
			if c.status == http.StatusSwitchingProtocols {
				return // what follows is not HTTP
			}
			next, err := http.ReadResponse(r, req)
			if err != nil {
				t.Fatalf("the response after it: %v", err)
			}
			if b, _ := io.ReadAll(next.Body); string(b) != "next" {
				t.Fatalf("the response after it has body %q, want %q", b, "next")
			}
		})
	}
}

// An error from onInterim ends the read, as it is; the final response is not
// read. A connection that ends after an interim response is an error, not an
// answer.
func TestReadFinalResponseErrors(t *testing.T) {
	stop := errors.New("client gone")
	r := bufio.NewReader(strings.NewReader("HTTP/1.1 103 Early Hints\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"))
	if _, err := ReadFinalResponse(r, nil, func(*http.Response) error { return stop }); err != stop {
		t.Fatalf("err = %v, want onInterim's", err)
	}
	if !strings.HasPrefix(peekAll(r), "HTTP/1.1 200 OK") {
		t.Fatal("the final response was read past an error")
	}

	r = bufio.NewReader(strings.NewReader("HTTP/1.1 100 Continue\r\n\r\n"))
	if resp, err := ReadFinalResponse(r, nil, nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("= (%v, %v), want io.ErrUnexpectedEOF for an answer that never came", resp, err)
	}
}

// Interim responses are read up to maxInterimHeaderBytes of them, as Go's
// client reads them, and past that the read ends with an error, not on and on
// with an app that sends them without end. Under it, the final response is
// read.
func TestReadFinalResponseCapsInterimResponses(t *testing.T) {
	interim := "HTTP/1.1 103 Early Hints\r\nX-Pad: " + strings.Repeat("x", 64<<10) + "\r\n\r\n"
	fit := maxInterimHeaderBytes / len(interim) // whole ones under the cap
	final := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
	for _, c := range []struct {
		n       int
		wantErr bool
	}{{fit, false}, {fit + 1, true}} {
		r := bufio.NewReader(strings.NewReader(strings.Repeat(interim, c.n) + final))
		forwarded := 0
		resp, err := ReadFinalResponse(r, nil, func(*http.Response) error { forwarded++; return nil })
		if c.wantErr {
			if !errors.Is(err, errTooManyInterimResponses) || forwarded != fit {
				t.Fatalf("%d interim responses: (%v, %v) after %d forwarded, want the cap's error after %d", c.n, resp, err, forwarded, fit)
			}
			continue
		}
		if err != nil || resp.StatusCode != http.StatusOK || forwarded != c.n {
			t.Fatalf("%d interim responses: (%v, %v) after %d forwarded, want the final 200 after all of them", c.n, resp, err, forwarded)
		}
	}
}

// An interim response is written as it came: its status line, its header
// section (here already in canonical case and order), nothing else. net/http's Response.Write gives the 100 Continue to a
// POST a Content-Length, which no 1xx may carry (RFC 9110 8.6).
func TestWriteInterimResponse(t *testing.T) {
	post, _ := http.NewRequest(http.MethodPost, "http://a/", strings.NewReader("abc"))
	for _, wire := range []string{
		"HTTP/1.1 100 Continue\r\n\r\n",
		"HTTP/1.1 103 Early Hints\r\nLink: </a.css>; rel=preload\r\nLink: </b.js>; rel=preload\r\n\r\n",
		"HTTP/1.1 199 \r\nX-Progress: 50\r\n\r\n", // no reason phrase: the space stays
	} {
		resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(wire)), post)
		if err != nil {
			t.Fatal(err)
		}
		var got bytes.Buffer
		if err := WriteInterimResponse(&got, resp); err != nil {
			t.Fatal(err)
		}
		if got.String() != wire {
			t.Errorf("wrote %q, want %q", got.String(), wire)
		}
	}
}

func TestIsInterimStatus(t *testing.T) {
	for code, want := range map[int]bool{99: false, 100: true, 101: false, 102: true, 103: true, 199: true, 200: false, 204: false} {
		if got := IsInterimStatus(code); got != want {
			t.Errorf("IsInterimStatus(%d) = %v, want %v", code, got, want)
		}
	}
}

func peekAll(r *bufio.Reader) string {
	b, _ := r.Peek(r.Buffered())
	return string(b)
}
