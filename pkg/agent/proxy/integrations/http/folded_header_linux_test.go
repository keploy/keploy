package http

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A server that folds a header line, recorded through the proxy's relay on
// real sockets, with net/http's client reading each response at the app: the
// recording holds each response as that client read it.
func TestRecordThroughTheRelayFramesAFoldedHeaderAsTheClientDoes(t *testing.T) {
	type exchange struct {
		path string
		req  string   // the request as the app writes it ("": a GET of path)
		resp []string // each element written once the app has the one before
	}
	for _, row := range []struct {
		name      string
		exchanges []exchange
		closeDest bool // the server closes after the last response
		// refused: net/http's client reads the response by a framing keploy's
		// would not give it, so the connection is passed through whole and
		// nothing is recorded.
		refused bool
	}{
		{name: "a Content-Length with a space before its colon, the body read to the close",
			exchanges: []exchange{{path: "/a", resp: []string{"HTTP/1.1 200 OK\r\nContent-Length : 5\r\n\r\n", "hello world"}}},
			closeDest: true, refused: true},
		{name: "a chunked HTTP/1.0 response, the body read to the close",
			exchanges: []exchange{{path: "/a", resp: []string{"HTTP/1.0 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n", "5\r\nhello\r\n0\r\n\r\n"}}},
			closeDest: true, refused: true},
		{name: "a folded negative Content-Length, the body read to the close",
			exchanges: []exchange{{path: "/a", resp: []string{"HTTP/1.1 200 OK\r\nX-A: b\r\n Content-Length: -5\r\n\r\n", "hello"}}},
			closeDest: true},
		{name: "a folded larger Content-Length on a keep-alive connection",
			exchanges: []exchange{
				{path: "/a", resp: []string{"HTTP/1.1 200 OK\r\nContent-Length: 5\r\nX-A: b\r\n Content-Length: 10\r\n\r\nhello"}},
				{path: "/b", resp: []string{"HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nworld"}},
			}},
		// Lines that end at a bare LF: net/http's reader ends the section at
		// the first empty line, "\n" alone, and so must keploy's. Looking for
		// "\r\n\r\n", it read the first message on to the close, and lost
		// the exchanges after it, and a client's first one too.
		{name: "a server that ends its lines with a bare LF, on a keep-alive connection",
			exchanges: []exchange{
				{path: "/a", resp: []string{"HTTP/1.1 200 OK\nContent-Length: 5\n\nhello"}},
				{path: "/b", resp: []string{"HTTP/1.1 200 OK\r\nX-A: b\nContent-Length: 5\r\n\nworld"}},
			}},
		{name: "a client that ends its lines with a bare LF, on a keep-alive connection",
			exchanges: []exchange{
				{path: "/a", req: "GET /a HTTP/1.1\nHost: up\n\n", resp: []string{"HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello"}},
				{path: "/b", req: "GET /b HTTP/1.1\r\nHost: up\n\r\n", resp: []string{"HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nworld"}},
			}},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := newRelayedConn(t)
			c.start(t)
			app := bufio.NewReader(c.app)
			var clientRead []string
			var sent int // what the server has written
			for i, ex := range row.exchanges {
				raw := ex.req
				if raw == "" {
					raw = "GET " + ex.path + " HTTP/1.1\r\nHost: up\r\n\r\n"
				}
				req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
				if err != nil {
					t.Fatal(err)
				}
				write(t, c.app, c.src, raw)
				relayed(t, c.dest, raw)
				for j, b := range ex.resp {
					if j > 0 {
						// The relay has handed the app what came before: these
						// bytes reach the proxy, and the capture, after it.
						for limit := time.Now().Add(5 * time.Second); received(t, c.app) < uint64(sent); {
							if time.Now().After(limit) {
								t.Fatalf("the app received %d of %d bytes in 5s", received(t, c.app), sent)
							}
							time.Sleep(50 * time.Microsecond)
						}
					}
					write(t, c.dest, c.dst, b)
					sent += len(b)
				}
				if row.closeDest && i == len(row.exchanges)-1 {
					_ = c.dest.Close()
				}
				resp, err := http.ReadResponse(app, req)
				if err != nil {
					t.Fatalf("net/http's client: %v", err)
				}
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					t.Fatalf("net/http's client, reading %s's body: %v", ex.path, err)
				}
				clientRead = append(clientRead, fmt.Sprintf("GET %s -> %d %q", ex.path, resp.StatusCode, body))
			}
			mocks, err := c.endErr(t)
			if refused := refusedFraming(err); refused != row.refused || err != nil && !refused {
				t.Fatalf("recordV2 = %v, want its framing refused %v", err, row.refused)
			}
			var got []string
			for _, m := range mocks {
				got = append(got, fmt.Sprintf("%s %s -> %d %q", m.Spec.HTTPReq.Method, m.Spec.HTTPReq.URL, m.Spec.HTTPResp.StatusCode, m.Spec.HTTPResp.Body))
			}
			if row.refused {
				// The client read every byte the server sent after the head.
				var sentBody string
				for _, b := range row.exchanges[0].resp[1:] {
					sentBody += b
				}
				if want := fmt.Sprintf("GET /a -> 200 %q", sentBody); len(clientRead) != 1 || clientRead[0] != want {
					t.Fatalf("the client read %q, want %q", clientRead, want)
				}
				if len(got) != 0 {
					t.Fatalf("recorded\n  %s\nwant nothing: the client read\n  %s", strings.Join(got, "\n  "), strings.Join(clientRead, "\n  "))
				}
				return
			}
			if strings.Join(got, "\n") != strings.Join(clientRead, "\n") {
				t.Fatalf("recorded\n  %s\nthe client read\n  %s", strings.Join(got, "\n  "), strings.Join(clientRead, "\n  "))
			}
		})
	}
}
