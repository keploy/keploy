package http

import (
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/logleak"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// encodeHTTP's log, every line of it down to Debug, holds no stretch of a
// recorded call's request body, response body, query values or a path
// segment's value in any encoding: it gives lengths, and names the call by
// its method, host, path shape and query parameter names. It logged the request and the response whole,
// as text, each chunk of a response, and on an upstream error the URL whole,
// query values and all. The log is rendered as production renders it, JSON
// and console, and checked once encodeHTTP and its recorder have returned.
func TestEncodeHTTP_LogsNoBodyAndNoQueryValue(t *testing.T) {
	// The values mix letters and digits, so no run of their hex or decimal
	// forms turns up in a log's timestamps by chance.
	const token, email, account = "sk_live_Q7wZr9KfXp2M", "grace.hopper.vault@example.net", "acct_9Lq2Zx7WbN4"
	reqBody := []byte(`{"card":"4111-WXYZ-1111-QRST","holder":"Grace Hopper","cvv":"7f3"}`)
	respBody := []byte(`{"iban":"DE89-ABCD-3704-0044","owner":"Grace Hopper Vault"}`)
	planted := map[string][]byte{"the token": []byte(token), "the e-mail": []byte(email), "the account in the path": []byte(account), "the request body": reqBody, "the response body": respBody}
	head := "POST /pay/" + account + "?token=" + token + "&email=" + url.QueryEscape(email) + " HTTP/1.1\r\nHost: up\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(reqBody)) + "\r\n\r\n"
	respHead := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(respBody)) + "\r\n\r\n"
	half := len(respBody) / 2

	for _, c := range []struct {
		name string
		// lines must each be logged, so the paths that logged content ran.
		lines []string
		// upstream serves the proxy's one connection; more is closed once
		// the app has the first half of the response body.
		upstream func(up net.Conn, more <-chan struct{})
		// app is what the app reads once it has sent the request body.
		app func(t *testing.T, app net.Conn, more chan<- struct{})
		// recorded is the response body the mock holds, nil for an error.
		recorded []byte
	}{
		{"a call recorded, its response body in two reads",
			[]string{"read the initial request", "read the complete request", "read the initial response", "read a chunk of the response[content-length]", "read the final response", "read the response body"},
			func(up net.Conn, more <-chan struct{}) {
				_ = readUntil(up, string(reqBody), time.Minute)
				_, _ = up.Write([]byte(respHead + string(respBody[:half])))
				<-more
				_, _ = up.Write(respBody[half:])
			},
			func(t *testing.T, app net.Conn, more chan<- struct{}) {
				if got := readUntil(app, string(respBody[:half]), time.Minute); !strings.HasSuffix(got, string(respBody[:half])) {
					t.Fatalf("the app got %q, want the response's head and the first half of its body", got)
				}
				close(more)
				if got := readUntil(app, string(respBody[half:]), time.Minute); !strings.HasSuffix(got, string(respBody[half:])) {
					t.Fatalf("the app got %q, want the rest of the response body", got)
				}
			}, respBody},
		{"an upstream that resets before it answers",
			[]string{"read the complete request", "upstream call errored before any response bytes; synthesized mock persisted so replay stays deterministic"},
			func(up net.Conn, _ <-chan struct{}) {
				_ = readUntil(up, string(reqBody), time.Minute)
				_ = up.(*net.TCPConn).SetLinger(0)
				_ = up.Close()
			},
			func(t *testing.T, app net.Conn, _ chan<- struct{}) {
				if got := readUntil(app, "\r\n\r\n", time.Minute); !strings.HasPrefix(got, "HTTP/1.1 502") {
					t.Fatalf("the app got %q, want the synthesized 502", got)
				}
			}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			obs, logs := observer.New(zapcore.DebugLevel)
			logger, out := logleak.Logger(obs)
			// Registered before the harness, so it runs after the harness
			// has waited for encodeHTTP and its recorder.
			t.Cleanup(func() {
				for _, msg := range c.lines {
					if logs.FilterMessage(msg).Len() == 0 {
						t.Errorf("the line %q was not logged: the path that logged content did not run; got %v", msg, logs.All())
					}
				}
				for _, l := range logs.FilterMessage("upstream call errored before any response bytes; synthesized mock persisted so replay stays deterministic").All() {
					meta, _ := l.ContextMap()["upstream"].(map[string]string)
					if meta["method"] != "POST" || meta["host"] != "up" || meta["path"] != "/pay/{}" || meta["query keys"] != "email,token" {
						t.Errorf("the line names the call by its method, host, path shape and query parameter names; got %v", l.ContextMap())
					}
				}
				log := out.String()
				if all := logs.All(); len(all) == 0 || !strings.Contains(log, all[0].Message) {
					t.Fatalf("the rendered log is not the one checked: %s", log)
				}
				for what, content := range planted {
					if found := logleak.Find(t, log, content); len(found) > 0 {
						t.Errorf("the log holds %s: %v", what, found)
					}
				}
			})
			more := make(chan struct{})
			app, done, mocks := encodeHarnessLogged(t, logger, head, func(up net.Conn) { c.upstream(up, more) })
			if _, err := app.Write(reqBody); err != nil {
				t.Fatal(err)
			}
			c.app(t, app, more)
			var m *models.Mock
			select {
			case m = <-mocks:
			case <-time.After(time.Minute): // a hang guard only
				t.Fatal("no mock recorded")
			}
			if m.Spec.HTTPReq == nil || m.Spec.HTTPReq.Body != string(reqBody) || !strings.Contains(m.Spec.HTTPReq.URL, token) {
				t.Fatalf("recorded %+v, want the planted request: the mock keeps what the log must not", m.Spec.HTTPReq)
			}
			if c.recorded != nil && (m.Spec.HTTPResp == nil || m.Spec.HTTPResp.Body != string(c.recorded)) {
				t.Fatalf("recorded %+v, want the planted response", m.Spec.HTTPResp)
			}
			_ = app.Close()
			<-done
		})
	}
}
