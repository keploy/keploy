package http

import (
	"bufio"
	"context"
	"fmt"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
)

// A header line that begins with a space or a tab continues the field before
// it (obsolete line folding, RFC 9112 §5.2): net/http's reader joins it to that
// field's value, and a user agent must. keploy's framing read it as a field of
// its own, so "X-A: b\r\n Content-Length: -5" framed the message by a
// Content-Length its peer never sees. These rows hold parseHeaders to what
// net/http's reader makes of the same header section, where it reads one.
func TestParseHeadersReadsAFoldedLineAsTheFieldBefore(t *testing.T) {
	for _, row := range []struct {
		name, head string
		cl, te     string
		goRefuses  bool // net/http's reader refuses the section; no oracle
	}{
		{name: "a folded Content-Length is X-A's value",
			head: "HTTP/1.1 200 OK\r\nX-A: b\r\n Content-Length: -5\r\n\r\n"},
		{name: "folded with a tab",
			head: "HTTP/1.1 200 OK\r\nX-A: b\r\n\tContent-Length: 7\r\n\r\n"},
		{name: "a folded Transfer-Encoding does not override the Content-Length",
			head: "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nX-A: b\r\n\tTransfer-Encoding: chunked\r\n\r\n", cl: "5"},
		{name: "a Content-Length whose value is on the folded line",
			head: "HTTP/1.1 200 OK\r\nContent-Length:\r\n 5\r\n\r\n", cl: "5"},
		{name: "a Content-Length whose value goes on past the fold is no length",
			head: "HTTP/1.1 200 OK\r\nContent-Length: 1\r\n 0\r\n\r\n", cl: "1 0"},
		{name: "a Transfer-Encoding whose chunked is on the folded line",
			head: "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nTransfer-Encoding: gzip,\r\n chunked\r\n\r\n", te: "gzip, chunked"},
		{name: "lines that end at a bare LF",
			head: "HTTP/1.1 200 OK\nX-A: b\n Content-Length: 3\nContent-Length:\n\t4\n\n", cl: "4"},
		{name: "a folded line right after the start line continues no field",
			head: "HTTP/1.1 200 OK\r\n Content-Length: 2\r\nX-A: b\r\n\r\n", goRefuses: true},
		{name: "a folded line after a line with no colon continues no field",
			head: "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nbogus\r\n Content-Length: 7\r\n\r\n", cl: "5", goRefuses: true},
		{name: "a line with no colon is no field",
			head: "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nContent-Length\r\nX-A: b\r\n\r\n", cl: "5", goRefuses: true},
		{name: "the last line with no colon is no field",
			head: "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nContent-Length\r\n\r\n", cl: "5", goRefuses: true},
		{name: "the header section ends at its empty line",
			head: "HTTP/1.1 200 OK\r\nX-A: b\r\n\r\nContent-Length: 5\r\n\r\n"},
		{name: "an unfolded section is read as before",
			head: "POST /a HTTP/1.1\r\nHost: up\r\ncontent-length: 3\r\nX-A: b\r\n\r\n", cl: "3"},
	} {
		t.Run(row.name, func(t *testing.T) {
			in := []byte(row.head)
			cl, te := parseHeaders(in)
			if cl != row.cl || te != row.te {
				t.Errorf("parseHeaders(%q) = (%q, %q), want (%q, %q)", row.head, cl, te, row.cl, row.te)
			}
			if string(in) != row.head {
				t.Errorf("parseHeaders wrote into the message it read: %q, was %q", in, row.head)
			}
			// net/http's reader, on the same section: the oracle.
			r := textproto.NewReader(bufio.NewReader(strings.NewReader(row.head)))
			if _, err := r.ReadLine(); err != nil {
				t.Fatal(err)
			}
			h, err := r.ReadMIMEHeader()
			if row.goRefuses {
				if err == nil {
					t.Fatalf("net/http's reader read %q, the row says it refuses it", row.head)
				}
				return
			}
			if err != nil {
				t.Fatalf("net/http's reader: %v", err)
			}
			goCL, goTE := h.Get("Content-Length"), h.Get("Transfer-Encoding")
			if strings.Contains(strings.ToLower(goTE), "chunked") {
				goCL = ""
			}
			if cl != goCL || te != goTE {
				t.Errorf("parseHeaders = (%q, %q), net/http's reader = (%q, %q)", cl, te, goCL, goTE)
			}
		})
	}
}

// "Expect: 100-continue" on a folded line is part of the field before it, not
// an expectation: a server whose reader joins it (net/http's) sends no 100,
// and the record proxy waited for one. One whose value is on the folded line
// is an expectation.
func TestExpectsContinueReadsAFoldedLineAsTheFieldBefore(t *testing.T) {
	for req, want := range map[string]bool{
		"POST /u HTTP/1.1\r\nHost: a\r\nX-A: b\r\n Expect: 100-continue\r\n\r\n":  false,
		"POST /u HTTP/1.1\r\nHost: a\r\nX-A: b\r\n\tExpect: 100-continue\r\n\r\n": false,
		"POST /u HTTP/1.1\r\nHost: a\r\nExpect:\r\n 100-continue\r\n\r\n":         true,
		"POST /u HTTP/1.1\r\nHost: a\r\nExpect:\r\n\t100-continue\r\n\r\n":        true,
		"POST /u HTTP/1.1\r\nHost: a\r\nX-A: 100-continue\r\n\r\n":                false,
		"POST /u HTTP/1.1\r\nHost: a\r\nExpect: 100-later\r\n\r\n":                false,
	} {
		if got := expectsContinue([]byte(req)); got != want {
			t.Errorf("expectsContinue(%q) = %v, want %v", req, got, want)
		}
	}
}

// recordV2 frames a response or a request whose header folds as net/http's
// reader frames it, so each exchange on the connection is recorded whole and
// the next one starts where it does.
func TestRecordV2FramesAFoldedHeaderAsItsPeerDoes(t *testing.T) {
	type exchange struct {
		req, resp []string // each element one chunk
		want      string   // "METHOD URL -> STATUS BODY"
	}
	for _, row := range []struct {
		name      string
		exchanges []exchange
	}{
		{name: "a folded negative Content-Length cut the body", exchanges: []exchange{{
			req:  []string{"GET /a HTTP/1.1\r\nHost: up\r\n\r\n"},
			resp: []string{"HTTP/1.1 200 OK\r\nX-A: b\r\n Content-Length: -5\r\n\r\n", "hel", "lo"},
			want: `GET /a -> 200 "hello"`,
		}}},
		{name: "a folded Transfer-Encoding failed the chunk read", exchanges: []exchange{{
			req:  []string{"GET /a HTTP/1.1\r\nHost: up\r\n\r\n"},
			resp: []string{"HTTP/1.1 200 OK\r\nContent-Length: 5\r\nX-A: b\r\n\tTransfer-Encoding: chunked\r\n\r\n", "hello"},
			want: `GET /a -> 200 "hello"`,
		}, {
			req:  []string{"GET /b HTTP/1.1\r\nHost: up\r\n\r\n"},
			resp: []string{"HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nworld"},
			want: `GET /b -> 200 "world"`,
		}}},
		{name: "a folded larger Content-Length took the next response", exchanges: []exchange{{
			req:  []string{"GET /a HTTP/1.1\r\nHost: up\r\n\r\n"},
			resp: []string{"HTTP/1.1 200 OK\r\nContent-Length: 5\r\nX-A: b\r\n Content-Length: 10\r\n\r\n", "hello"},
			want: `GET /a -> 200 "hello"`,
		}, {
			req:  []string{"GET /b HTTP/1.1\r\nHost: up\r\n\r\n"},
			resp: []string{"HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nworld"},
			want: `GET /b -> 200 "world"`,
		}}},
		{name: "a Content-Length on the folded line framed no body", exchanges: []exchange{{
			req:  []string{"GET /a HTTP/1.1\r\nHost: up\r\n\r\n"},
			resp: []string{"HTTP/1.1 200 OK\r\nContent-Length:\r\n 5\r\n\r\n", "hello"},
			want: `GET /a -> 200 "hello"`,
		}, {
			req:  []string{"GET /b HTTP/1.1\r\nHost: up\r\n\r\n"},
			resp: []string{"HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nworld"},
			want: `GET /b -> 200 "world"`,
		}}},
		{name: "a request's folded Content-Length took the next request", exchanges: []exchange{{
			req:  []string{"POST /a HTTP/1.1\r\nHost: up\r\nX-A: b\r\n Content-Length: 3\r\n\r\n"},
			resp: []string{"HTTP/1.1 204 No Content\r\n\r\n"},
			want: `POST /a -> 204 ""`,
		}, {
			req:  []string{"GET /b HTTP/1.1\r\nHost: up\r\n\r\n"},
			resp: []string{"HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nworld"},
			want: `GET /b -> 200 "world"`,
		}}},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := &HTTP{Logger: zaptest.NewLogger(t)}
			sess, sendReq, closeReq, sendResp, closeResp, mocks := newTestSession(t)
			now := time.Now()
			var want []string
			for _, ex := range row.exchanges {
				for _, b := range ex.req {
					sendReq([]byte(b), now, now)
				}
				for _, b := range ex.resp {
					sendResp([]byte(b), now, now)
				}
				want = append(want, ex.want)
			}
			closeReq()
			closeResp()

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := h.recordV2(ctx, sess); err != nil {
				t.Fatalf("recordV2: %v", err)
			}
			close(mocks)
			var got []string
			for m := range mocks {
				got = append(got, fmt.Sprintf("%s %s -> %d %q", m.Spec.HTTPReq.Method, m.Spec.HTTPReq.URL, m.Spec.HTTPResp.StatusCode, m.Spec.HTTPResp.Body))
			}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("recorded\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
		})
	}
}

// The other readers of a message's framing read a folded header the same way:
// the replay proxy's read of the app's request, and the legacy record path's
// read of the destination's response.
func TestChunkedReadersFrameAFoldedHeaderAsItsPeerDoes(t *testing.T) {
	t.Run("replay request", func(t *testing.T) {
		const req = "POST /a HTTP/1.1\r\nHost: up\r\nX-A: b\r\n Content-Length: 3\r\n\r\n"
		got, err := runChunkedRequest(t, req, newScriptedConn())
		if err != nil || got != req {
			t.Fatalf("HandleChunkedRequests = (%q, %v), want (%q, nil)", got, err, req)
		}
	})
	t.Run("record response", func(t *testing.T) {
		head := "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nX-A: b\r\n\tTransfer-Encoding: chunked\r\n\r\nhel"
		dest := newScriptedConn(readStep{data: []byte("lo")})
		client := newScriptedConn()
		t.Cleanup(func() { _ = dest.Close(); _ = client.Close() })
		h := &HTTP{Logger: zaptest.NewLogger(t)}
		finalResp := []byte(head)
		done := make(chan error, 1)
		go func() {
			done <- h.handleChunkedResponses(context.Background(), &finalResp, client, dest, []byte(head), "GET")
		}()
		select {
		case err := <-done:
			if err != nil || string(finalResp) != head+"lo" {
				t.Fatalf("handleChunkedResponses = (%q, %v), want (%q, nil)", finalResp, err, head+"lo")
			}
		case <-time.After(time.Second):
			t.Fatal("handleChunkedResponses did not return")
		}
	})
}
