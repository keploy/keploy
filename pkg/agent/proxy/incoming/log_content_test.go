package proxy

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils/logcontent/logtest"
)

// canary is in a test case's query.
const canary = "kp-canary-ingress-4c7b"

// An exchange the ingress does not record is logged by its method and path,
// not its query's values, unless KEPLOY_LOG_CONTENT is set.
func TestIngress_AnExchangeNotRecordedIsLoggedWithoutItsQuery(t *testing.T) {
	t.Run("answered before its body came", func(t *testing.T) {
		stubContinueDrainTimeout(t, 100*time.Millisecond)
		app := refusingApp(t)
		for _, mode := range interimModes {
			t.Run(mode, func(t *testing.T) {
				logtest.Each(t, func(t *testing.T, on bool) {
					logger, logs := logtest.Observe()
					client, done, _ := interimIngressLogged(t, mode, app, logger)
					if _, err := io.WriteString(client, "POST /refuse?ref="+canary+" HTTP/1.1\r\nHost: app.local\r\nExpect: 100-continue\r\nContent-Length: 3\r\n\r\n"); err != nil {
						t.Fatal(err)
					}
					_, _, _ = readThroughFinal(t, bufio.NewReader(client))
					_ = client.Close()
					<-done
					logtest.Check(t, logs, canary, on, "Not recording this request as a test case: the app answered it before the whole request body reached the app, so the request the app answered is not the one the client sent. The client still got the app's answer. To record it, send the request without the \"Expect: 100-continue\" header (curl: -H 'Expect:').")
				})
			})
		}
	})
	// net/http's error for a header line it cannot read quotes the line
	// whole: the synchronous loop's line and the request-framed loop's, which
	// answer it with a 400, carry it only as logcontent allows.
	t.Run("a header line it cannot read", func(t *testing.T) {
		app, _ := interimApp(t)
		for _, mode := range interimModes {
			t.Run(mode, func(t *testing.T) {
				line := "Failed to parse downstream HTTP request"
				if mode == "sync" {
					line = "Failed to read client request; ignoring this connection. Verify the client is sending valid HTTP if this persists."
				}
				logtest.Each(t, func(t *testing.T, on bool) {
					logger, logs := logtest.Observe()
					client, done, _ := interimIngressLogged(t, mode, app, logger)
					if _, err := io.WriteString(client, "GET /reset HTTP/1.1\r\nHost: app.local\r\nX-Reset-Token "+canary+"\r\n\r\n"); err != nil {
						t.Fatal(err)
					}
					_, _ = io.Copy(io.Discard, client) // what the ingress answers, up to its close
					<-done
					logtest.Check(t, logs, canary, on, line)
				})
			})
		}
	})
	// The sync loop, which takes one exchange at a time, is what checks a
	// capture against its budget.
	for name, tc := range map[string]struct {
		reqBody, respBody int
		want              string
	}{
		"a body over its budget": {0, maxHTTPBodyCaptureBytes + 1, "Skipping HTTP capture because body exceeded capture budget"},
		"an exchange over its budget": {maxHTTPBodyCaptureBytes, maxHTTPBodyCaptureBytes,
			"Skipping HTTP capture because combined request and response exceeded capture budget"},
	} {
		t.Run(name, func(t *testing.T) {
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Length", strconv.Itoa(tc.respBody))
				_, _ = w.Write(make([]byte, tc.respBody))
			}))
			t.Cleanup(app.Close)
			logtest.Each(t, func(t *testing.T, on bool) {
				logger, logs := logtest.Observe()
				client, done, _ := interimIngressLogged(t, "sync", strings.TrimPrefix(app.URL, "http://"), logger)
				head := "POST /big?ref=" + canary + " HTTP/1.1\r\nHost: app.local\r\nConnection: close\r\nContent-Length: " + strconv.Itoa(tc.reqBody) + "\r\n\r\n"
				go func() {
					_, _ = io.WriteString(client, head)
					_, _ = client.Write(make([]byte, tc.reqBody))
				}()
				resp, err := http.ReadResponse(bufio.NewReader(client), nil)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = client.Close()
				<-done
				deadline := time.Now().Add(5 * time.Second)
				for logs.FilterMessage(tc.want).Len() == 0 && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond) // the combined check runs in the capture's goroutine
				}
				logtest.Check(t, logs, canary, on, tc.want)
			})
		})
	}
}
