package http

import (
	"bufio"
	"io"
	nethttp "net/http"
	"testing"
	"time"
)

// retried is what a client's retry policy sends again: a 5xx, a 408 and a
// 429, the statuses the AWS SDKs, .NET's standard resilience handler and
// Polly's transient-error policy retry.
func retried(status int) bool {
	return status >= 500 || status == nethttp.StatusRequestTimeout || status == nethttp.StatusTooManyRequests
}

// keploy answers an outgoing HTTP call no mock matches itself. It answered
// 502 Bad Gateway, which every client's retry policy sends again: the AWS
// .NET SDK made a refused DynamoDB call up to 11 times, its backoff outlasted
// the test that made it, and the repeat landed in a later test's window,
// failing that test for a call it never made (a Playwright suite whose one app
// process serves every test). The miss is answered 400 Bad Request, with the
// body that names it and the connection closed, so a client that retries what
// such policies retry makes the refused call once.
func TestAMissIsAnsweredWithAStatusNoClientRetries(t *testing.T) {
	const body = "keploy: no matching mock found for this HTTP request\n"
	for _, c := range []struct{ proto, request string }{
		{"HTTP/1.1", "POST / HTTP/1.1\r\nHost: dynamodb.test\r\nX-Amz-Target: DynamoDB_20120810.PutItem\r\nContent-Length: 2\r\n\r\n{}"},
		{"HTTP/1.0", "GET /lead HTTP/1.0\r\nHost: api.test\r\n\r\n"},
	} {
		t.Run(c.proto, func(t *testing.T) {
			attempts := 0
			for attempts < 4 {
				attempts++
				app, proxy := tcpPair(t)
				done := startDecode(t, c.request, proxy)
				_ = app.SetReadDeadline(time.Now().Add(5 * time.Second))
				resp, err := nethttp.ReadResponse(bufio.NewReader(app), nil)
				if err != nil {
					t.Fatalf("attempt %d: the app read no answer: %v", attempts, err)
				}
				got, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					t.Fatalf("attempt %d: the answer's body: %v", attempts, err)
				}
				wantMockMiss(t, done)
				if resp.Status != "400 Bad Request" || resp.Proto != c.proto {
					t.Errorf("attempt %d: the miss was answered %s %q, want %s 400 Bad Request", attempts, resp.Proto, resp.Status, c.proto)
				}
				if string(got) != body || resp.Header.Get("Content-Type") != "text/plain" {
					t.Errorf("attempt %d: the miss's answer is %q (%s), want keploy's %q (text/plain)", attempts, got, resp.Header.Get("Content-Type"), body)
				}
				if !resp.Close {
					t.Errorf("attempt %d: the miss's answer leaves the connection open", attempts)
				}
				if !retried(resp.StatusCode) {
					break
				}
			}
			if attempts != 1 {
				t.Fatalf("a client that retries what retry policies retry made the refused call %d times, want once", attempts)
			}
		})
	}
}
