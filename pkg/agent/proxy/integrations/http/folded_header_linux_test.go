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
		resp []string // each element written once the app has the one before
	}
	for _, row := range []struct {
		name      string
		exchanges []exchange
		closeDest bool // the server closes after the last response
	}{
		{name: "a folded negative Content-Length, the body read to the close",
			exchanges: []exchange{{path: "/a", resp: []string{"HTTP/1.1 200 OK\r\nX-A: b\r\n Content-Length: -5\r\n\r\n", "hello"}}},
			closeDest: true},
		{name: "a folded larger Content-Length on a keep-alive connection",
			exchanges: []exchange{
				{path: "/a", resp: []string{"HTTP/1.1 200 OK\r\nContent-Length: 5\r\nX-A: b\r\n Content-Length: 10\r\n\r\nhello"}},
				{path: "/b", resp: []string{"HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nworld"}},
			}},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := newRelayedConn(t)
			c.start(t)
			app := bufio.NewReader(c.app)
			var clientRead []string
			var sent int // what the server has written
			for i, ex := range row.exchanges {
				raw := "GET " + ex.path + " HTTP/1.1\r\nHost: up\r\n\r\n"
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
			var got []string
			for _, m := range c.end(t) {
				got = append(got, fmt.Sprintf("%s %s -> %d %q", m.Spec.HTTPReq.Method, m.Spec.HTTPReq.URL, m.Spec.HTTPResp.StatusCode, m.Spec.HTTPResp.Body))
			}
			if strings.Join(got, "\n") != strings.Join(clientRead, "\n") {
				t.Fatalf("recorded\n  %s\nthe client read\n  %s", strings.Join(got, "\n  "), strings.Join(clientRead, "\n  "))
			}
		})
	}
}
