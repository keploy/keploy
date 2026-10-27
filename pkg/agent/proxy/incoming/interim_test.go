package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// interimModes are the ingress's three ways of recording: the synchronous
// loop, and the request-framed loop of normal (keep-alive) and sampled
// recording.
var interimModes = []string{"sync", "normal", "sampled"}

// interimApp is an app that sends interim responses before its final one.
// /hints sends a 103 Early Hints, /processing a 102 Processing, each before a
// 200. /upload answers with the body it read: net/http sends the 100 Continue
// to a request that expects one when the handler first reads the body. Any
// other path is answered at once. It returns its address and how many
// connections it has taken.
func interimApp(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var conns atomic.Int32
	app := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hints":
			w.Header().Add("Link", "</s.css>; rel=preload")
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("final"))
		case "/processing":
			w.WriteHeader(http.StatusProcessing)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("final"))
		case "/upload":
			b, _ := io.ReadAll(r.Body)
			// The handler goes on with the request a moment, as one that
			// calls out with its context does: a request that ended under
			// it (its connection half-closed) is cancelled.
			select {
			case <-r.Context().Done():
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("got:" + string(b) + ", then the request was cancelled"))
				return
			case <-time.After(100 * time.Millisecond):
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("got:" + string(b)))
		default:
			_, _ = w.Write([]byte("plain"))
		}
	}))
	app.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	app.Start()
	t.Cleanup(func() {
		// An ingress still reading for an answer (a failed test) does not
		// keep Close waiting on its connection.
		app.CloseClientConnections()
		app.Close()
	})
	return strings.TrimPrefix(app.URL, "http://"), &conns
}

// recordedExchange is what the capture hook was given for one exchange.
type recordedExchange struct {
	method, path, reqBody string
	status                int
	respBody              string
}

// interimIngress serves one client connection through a manager in mode to
// upstream, and returns the client end, a channel closed when the handler
// returns, and the exchanges it records.
func interimIngress(t *testing.T, mode, upstream string) (net.Conn, chan struct{}, chan recordedExchange) {
	t.Helper()
	return interimIngressLogged(t, mode, upstream, zap.NewNop())
}

// interimIngressLogged is interimIngress with the manager logging to logger.
func interimIngressLogged(t *testing.T, mode, upstream string, logger *zap.Logger) (net.Conn, chan struct{}, chan recordedExchange) {
	t.Helper()
	pm, ctx, recorded := interimManager(t, mode, logger)
	client, done := ingressPipeOn(t, ctx, pm, upstream, make(chan struct{}, 1), nil)
	// A response that never comes fails the read, not the whole run.
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	return client, done, recorded
}

// interimManager is a manager recording in mode, logging to logger, with the
// context to serve connections in and the exchanges it records.
func interimManager(t *testing.T, mode string, logger *zap.Logger) (*IngressProxyManager, context.Context, chan recordedExchange) {
	t.Helper()
	stubIngressPaused(t, func() bool { return false })
	recorded := make(chan recordedExchange, 8)
	stubCaptureHook(t, func(_ context.Context, _ *zap.Logger, _ chan *models.TestCase,
		req *http.Request, resp *http.Response, _, _ time.Time,
		_ models.IncomingOptions, _ bool, _ bool, _ uint16) {
		reqBody, _ := io.ReadAll(req.Body)
		respBody, _ := io.ReadAll(resp.Body)
		recorded <- recordedExchange{req.Method, req.URL.Path, string(reqBody), resp.StatusCode, string(respBody)}
	})
	pm := &IngressProxyManager{
		logger:      logger,
		tcChan:      make(chan *models.TestCase, 8),
		synchronous: mode == "sync",
		sampling:    mode == "sampled",
		samplingSem: make(chan struct{}, 1),
	}
	mgr := syncMock.New(zap.NewNop())
	mgr.SetFirstRequestSignaled()
	ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 10*time.Second)
	t.Cleanup(cancel)
	return pm, ctx, recorded
}

// readThroughFinal reads responses from r up to the final one, and returns the
// interim ones and the final one with its body read.
func readThroughFinal(t *testing.T, r *bufio.Reader) (interim []*http.Response, final *http.Response, body string) {
	t.Helper()
	for {
		resp, err := http.ReadResponse(r, nil)
		if err != nil {
			t.Fatalf("after %d interim responses, the client read no final response: %v", len(interim), err)
		}
		if resp.StatusCode >= 200 || resp.StatusCode == http.StatusSwitchingProtocols {
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("final %d response's body: %v", resp.StatusCode, err)
			}
			return interim, resp, string(b)
		}
		interim = append(interim, resp)
	}
}

// awaitRecorded returns the exchange recorded for path, failing the test if
// none is within a few seconds.
func awaitRecorded(t *testing.T, recorded chan recordedExchange, path string) recordedExchange {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case r := <-recorded:
			if r.path == path {
				return r
			}
		case <-timeout:
			t.Fatalf("no exchange recorded for %s", path)
		}
	}
}

// An app may send interim responses (1xx) before its final one: a 103 Early
// Hints, a 102 Processing. The ingress forwards each to the client as it
// comes, as the app sent it, then the final response, and records the final
// one. net/http's ReadResponse returns an interim response as if it were the
// answer: the ingress forwarded only it, recorded it (a 103 with no body), and
// left the answer unread. The synchronous and sampled loops then closed the
// connection, and the client got no answer; the normal loop went back to
// reading requests, the client waited for an answer that never came, and the
// app's answer was left for whatever request came next on the connection.
func TestIngressForwardsInterimResponsesAndRecordsTheFinal(t *testing.T) {
	upstream, _ := interimApp(t)
	for _, mode := range interimModes {
		for _, c := range []struct {
			path, interim string
			code          int
		}{
			{"/hints", "HTTP/1.1 103 Early Hints\r\nLink: </s.css>; rel=preload\r\n\r\n", http.StatusEarlyHints},
			{"/processing", "HTTP/1.1 102 Processing\r\n\r\n", http.StatusProcessing},
		} {
			t.Run(mode+c.path, func(t *testing.T) {
				client, done, recorded := interimIngress(t, mode, upstream)
				if _, err := io.WriteString(client, "GET "+c.path+" HTTP/1.1\r\nHost: app.local\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				var wire bytes.Buffer
				r := bufio.NewReader(io.TeeReader(client, &wire))
				interim, final, body := readThroughFinal(t, r)
				if len(interim) != 1 || interim[0].StatusCode != c.code {
					t.Fatalf("the client got interim responses %v, want the app's %d", interim, c.code)
				}
				// As the app sent it: no Connection: close the loop adds to the
				// final response, no Content-Length, which no 1xx may carry.
				if !strings.HasPrefix(wire.String(), c.interim+"HTTP/1.1 200 OK\r\n") {
					t.Fatalf("the client got %q, want the app's interim response %q as it was sent, then the final one", wire.String(), c.interim)
				}
				if final.StatusCode != http.StatusOK || body != "final" {
					t.Fatalf("the client got final %d %q, want the app's 200 %q", final.StatusCode, body, "final")
				}
				if mode == "normal" {
					// Keep-alive: the connection carries the next exchange.
					if _, err := io.WriteString(client, "GET /plain HTTP/1.1\r\nHost: app.local\r\nConnection: close\r\n\r\n"); err != nil {
						t.Fatal(err)
					}
					if _, next, nextBody := readThroughFinal(t, r); next.StatusCode != http.StatusOK || nextBody != "plain" {
						t.Fatalf("the next request on the connection got %d %q, want its own answer", next.StatusCode, nextBody)
					}
				}
				got := awaitRecorded(t, recorded, c.path)
				if got.status != http.StatusOK || got.respBody != "final" {
					t.Fatalf("recorded %d %q, want the final response, 200 %q", got.status, got.respBody, "final")
				}
				_ = client.Close()
				<-done
			})
		}
	}
}

// A client that sends "Expect: 100-continue" sends its body only once the
// app's 100 (Continue) reaches it. The ingress forwards the request's headers,
// forwards the 100 as it comes, and then the body, and records the exchange
// with its body and the app's final response. It forwarded the 100 only once
// the body had gone to the app, which the client was waiting for the 100 to
// send: a client that waits for it (Java's) hung, and one that gives up
// waiting (curl, Go's, after a second) sent the body, and the ingress, reading
// the 100 as the answer, sent it on and closed the connection (in normal
// recording, left the answer for the next request). The normal loop buffered
// a PUT's body before it forwarded anything, for a re-send on a stale
// connection: the same wait.
//
// The app's side stays open once the whole body is through: the end of the
// body is no end of the request, and an app told it ended (its connection
// half-closed) cancels a handler still at work (Go's cancels the request's
// context), and closes a keep-alive connection the next request then needs a
// new one in place of.
func TestIngressForwardsTheContinueBeforeTheBody(t *testing.T) {
	upstream, conns := interimApp(t)
	for _, mode := range interimModes {
		for _, method := range []string{http.MethodPost, http.MethodPut} {
			t.Run(mode+"/"+method, func(t *testing.T) {
				connsBefore := conns.Load()
				client, done, recorded := interimIngress(t, mode, upstream)
				if _, err := io.WriteString(client, method+" /upload HTTP/1.1\r\nHost: app.local\r\nExpect: 100-continue\r\nContent-Length: 3\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				var wire bytes.Buffer
				r := bufio.NewReader(io.TeeReader(client, &wire))
				// Nothing of the body is sent until the 100 is here.
				_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
				cont, err := http.ReadResponse(r, nil)
				if err != nil {
					t.Fatalf("the client, waiting to send its body, got no 100 Continue: %v", err)
				}
				if cont.StatusCode != http.StatusContinue || wire.String() != "HTTP/1.1 100 Continue\r\n\r\n" {
					t.Fatalf("the client got %q, want the app's 100 Continue as it was sent", wire.String())
				}
				if _, err := io.WriteString(client, "abc"); err != nil {
					t.Fatal(err)
				}
				_, final, body := readThroughFinal(t, r)
				if final.StatusCode != http.StatusCreated || body != "got:abc" {
					t.Fatalf("the client got %d %q, want the app's 201 for the body it sent after the 100", final.StatusCode, body)
				}
				if mode == "normal" {
					// Keep-alive: the app's connection carries the next exchange.
					if _, err := io.WriteString(client, "GET /plain HTTP/1.1\r\nHost: app.local\r\nConnection: close\r\n\r\n"); err != nil {
						t.Fatal(err)
					}
					if _, next, nextBody := readThroughFinal(t, r); next.StatusCode != http.StatusOK || nextBody != "plain" {
						t.Fatalf("the next request on the connection got %d %q, want its own answer", next.StatusCode, nextBody)
					}
					if n := conns.Load() - connsBefore; n != 1 {
						t.Fatalf("the app took %d connections for the two requests, want the one it kept", n)
					}
				}
				got := awaitRecorded(t, recorded, "/upload")
				want := recordedExchange{method, "/upload", "abc", http.StatusCreated, "got:abc"}
				if got != want {
					t.Fatalf("recorded %+v, want %+v", got, want)
				}
				_ = client.Close()
				<-done
			})
		}
	}
}

// refusingApp is an app that turns down a request that expects a 100
// (Continue) with a final response, without reading the body, which its
// client then never sends: /refuse at once, /refuse-after-hints after a 103
// Early Hints (no 100). The rest of its answer comes a moment later, and,
// as some apps do, it takes the request ending meanwhile (its connection
// half-closed) for its client going away, and drops the answer. Else it keeps
// the connection. /refuse-whole sends the whole answer at once, framed by its
// length (no stream, which gives the --sync lock back at its headers), and
// keeps the connection.
func refusingApp(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				if req.URL.Path == "/refuse-whole" {
					_, _ = io.WriteString(c, "HTTP/1.1 417 Expectation Failed\r\nContent-Length: 10\r\n\r\nno, thanks")
					_, _ = io.Copy(io.Discard, br)
					return
				}
				if req.URL.Path == "/refuse-after-hints" {
					_, _ = io.WriteString(c, "HTTP/1.1 103 Early Hints\r\nLink: </s.css>; rel=preload\r\n\r\n")
				}
				_, _ = io.WriteString(c, "HTTP/1.1 417 Expectation Failed\r\nTransfer-Encoding: chunked\r\n\r\n4\r\nno, \r\n")
				_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
				if _, err := br.ReadByte(); err == io.EOF {
					return
				}
				_, _ = io.WriteString(c, "6\r\nthanks\r\n0\r\n\r\n")
				_ = c.SetReadDeadline(time.Time{})
				_, _ = io.Copy(io.Discard, br) // and keeps the connection
			}(c)
		}
	}()
	return ln.Addr().String()
}

// An app may turn down a request that expects a 100 (Continue) with a final
// response, without reading the body, and without a 100 (an interim response
// of another kind does not ask for it), and here its client never sends the
// body. The ingress waits for the body continueDrainTimeout, as a client that
// stops waiting for the 100 sends it then, and then gives the client the
// answer, all of it: the app is not told the request ended (it did not, the
// client only never sent the rest). The connection ends with the answer,
// though the app (this one) would keep it: the ingress no longer knows where
// the client's next request would start. The exchange is not recorded, and the
// user is told why: the request the client meant was never sent, and a replay
// could not send what was. The ingress waited for the body before it read any
// response, without end, and the client, waiting for a 100, got nothing.
func TestIngressAnswersAnUploadTheAppTurnsDownWithoutItsBody(t *testing.T) {
	// Short of the app's own wait for the end of the request (300 ms), so a
	// half-close at the cut-off would drop the rest of its answer.
	stubContinueDrainTimeout(t, 100*time.Millisecond)
	app := refusingApp(t)
	for _, mode := range interimModes {
		for _, path := range []string{"/refuse", "/refuse-after-hints"} {
			t.Run(mode+path, func(t *testing.T) {
				core, logs := observer.New(zap.WarnLevel)
				client, done, recorded := interimIngressLogged(t, mode, app, zap.New(core))
				if _, err := io.WriteString(client, "POST "+path+" HTTP/1.1\r\nHost: app.local\r\nExpect: 100-continue\r\nContent-Length: 3\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				r := bufio.NewReader(client)
				interim, final, body := readThroughFinal(t, r)
				wantInterims := 0
				if path == "/refuse-after-hints" {
					wantInterims = 1
				}
				if len(interim) != wantInterims || final.StatusCode != http.StatusExpectationFailed || body != "no, thanks" {
					t.Fatalf("the client got %d interim responses then %d %q, want %d then the app's whole 417", len(interim), final.StatusCode, body, wantInterims)
				}
				if !final.Close {
					t.Fatal("the answer does not say the connection ends with it")
				}
				if _, err := r.ReadByte(); err != io.EOF {
					t.Fatalf("after the answer the connection gave %v, want its end", err)
				}
				_ = client.Close() // as a client does at the end of the connection
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("the ingress is still waiting for a body the client will not send")
				}
				select {
				case got := <-recorded:
					t.Fatalf("recorded %+v, a request whose body was never sent", got)
				case <-time.After(100 * time.Millisecond):
				}
				warned := logs.FilterMessageSnippet("Not recording this request as a test case").FilterField(zap.Int("status_code", http.StatusExpectationFailed))
				if warned.Len() != 1 {
					t.Fatalf("logged %v, want one warning that the exchange is not recorded and why", logs.All())
				}
			})
		}
	}
}

// A client that goes away after the app's 100 (Continue), without its body,
// leaves the app waiting for a body that will not come, and the ingress
// reading for an answer the app will not give. The app is told the body ended
// there, as the client's going would have told it, and the exchange ends:
// unrecorded, and in sync recording with the lock given back.
func TestIngressEndsAnUploadItsClientLeftAfterTheContinue(t *testing.T) {
	upstream, _ := interimApp(t)
	for _, mode := range interimModes {
		t.Run(mode, func(t *testing.T) {
			client, done, recorded := interimIngress(t, mode, upstream)
			if _, err := io.WriteString(client, "POST /upload HTTP/1.1\r\nHost: app.local\r\nExpect: 100-continue\r\nContent-Length: 3\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			cont, err := http.ReadResponse(bufio.NewReader(client), nil)
			if err != nil || cont.StatusCode != http.StatusContinue {
				t.Fatalf("the client got (%v, %v), want the app's 100 Continue", cont, err)
			}
			_ = client.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the ingress is still waiting on an exchange whose client left")
			}
			select {
			case got := <-recorded:
				t.Fatalf("recorded %+v, a request whose body was never sent", got)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

// An HTTP/1.0 client gets no interim response (RFC 9110 15.2): it does not
// know them, and only the ingress, forwarding its request as HTTP/1.1, made
// the app think it would.
func TestIngressSendsNoInterimResponseToAnHTTP10Client(t *testing.T) {
	upstream, _ := interimApp(t)
	for _, mode := range interimModes {
		t.Run(mode, func(t *testing.T) {
			client, done, recorded := interimIngress(t, mode, upstream)
			if _, err := io.WriteString(client, "GET /hints HTTP/1.0\r\nHost: app.local\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			var wire bytes.Buffer
			resp, err := http.ReadResponse(bufio.NewReader(io.TeeReader(client, &wire)), nil)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK || string(body) != "final" {
				t.Fatalf("the HTTP/1.0 client got %q first, want the final response", wire.String())
			}
			if got := awaitRecorded(t, recorded, "/hints"); got.status != http.StatusOK {
				t.Fatalf("recorded %d, want the final 200", got.status)
			}
			_ = client.Close()
			<-done
		})
	}
}

// A request the app has begun to answer (an interim response came) is not
// sent to it again when its connection then drops: the re-send is for a
// pooled connection found closed before the request reached the app, and this
// one reached it. Sent again, the app ran it twice, and its calls out were
// recorded twice.
func TestIngressDoesNotResendARequestTheAppBeganToAnswer(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var requests atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if _, err := http.ReadRequest(bufio.NewReader(c)); err != nil {
					return
				}
				requests.Add(1)
				_, _ = io.WriteString(c, "HTTP/1.1 103 Early Hints\r\nLink: </s.css>; rel=preload\r\n\r\n")
			}(c)
		}
	}()
	client, done, _ := interimIngress(t, "normal", ln.Addr().String())
	if _, err := io.WriteString(client, "GET /hints HTTP/1.1\r\nHost: app.local\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	interim, final, _ := readThroughFinal(t, bufio.NewReader(client))
	if len(interim) != 1 || final.StatusCode != http.StatusBadGateway {
		t.Fatalf("the client got %v then %d, want the app's 103, then a 502 for the answer that never came", interim, final.StatusCode)
	}
	<-done
	if n := requests.Load(); n != 1 {
		t.Fatalf("the app got the request %d times, want once", n)
	}
}

// stubContinueDrainTimeout sets continueDrainTimeout for one test.
func stubContinueDrainTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := continueDrainTimeout
	continueDrainTimeout = d
	t.Cleanup(func() { continueDrainTimeout = prev })
}

// stubLingerTimeout sets lingerTimeout for one test.
func stubLingerTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := lingerTimeout
	lingerTimeout = d
	t.Cleanup(func() { lingerTimeout = prev })
}

// assertLingers checks that the handler of a connection the client has just
// read the end of (done) is still there: the ingress ended what it sends and
// reads on (lingerClose), and closed nothing yet, which would reset the
// connection under a client still sending.
func assertLingers(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatal("the connection closed with the client's bytes unread, without lingering for them")
	case <-time.After(100 * time.Millisecond):
	}
}

// eagerContinueApp is an app that sends the 100 (Continue) to a request that
// expects one as soon as it has the request's headers, before its handler
// runs, as Node's and Python's servers do; whose handler answers 401 at once,
// without reading the body (an auth check that fails); and that then reads the
// body and drops it, as Node's server does with a body its handler left, and
// serves the next request on the connection. It returns its address and how
// many connections it has taken.
func eagerContinueApp(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var conns atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					req, err := http.ReadRequest(br)
					if err != nil {
						return
					}
					if req.Header.Get("Expect") == "100-continue" {
						_, _ = io.WriteString(c, "HTTP/1.1 100 Continue\r\n\r\n")
					}
					_, _ = io.WriteString(c, "HTTP/1.1 401 Unauthorized\r\nContent-Length: 4\r\n\r\ndeny")
					if _, err := io.Copy(io.Discard, req.Body); err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String(), &conns
}

// uploadAfterContinue sends a POST /up of size bytes that expects a 100
// (Continue) on client, sends the body once the 100 is here, and returns the
// final response, its body read, and the reader the connection goes on in.
func uploadAfterContinue(t *testing.T, client net.Conn, size int) (*http.Response, string, *bufio.Reader) {
	t.Helper()
	if _, err := fmt.Fprintf(client, "POST /up HTTP/1.1\r\nHost: app.local\r\nExpect: 100-continue\r\nContent-Length: %d\r\n\r\n", size); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(client)
	if cont, err := http.ReadResponse(r, nil); err != nil || cont.StatusCode != http.StatusContinue {
		t.Fatalf("the client, waiting to send its body, got (%v, %v), want the app's 100 Continue", cont, err)
	}
	if _, err := io.WriteString(client, strings.Repeat("x", size)); err != nil {
		t.Fatal(err)
	}
	_, final, body := readThroughFinal(t, r)
	return final, body, r
}

// An app that sent the 100 (Continue) and then answered at once, without the
// body (Node's and Python's servers send the 100 before the handler runs, and
// an auth check turns the request down), still gets the body, which the client
// a 100 reached sends whatever the answer (RFC 9110 10.1.1). The ingress
// forwards it, within bounds, before the answer, and records the exchange
// whole; the app reads the body (and drops it), and in normal recording the
// keep-alive connection carries the next request, as it does without keploy.
// The ingress cut the body off the moment the answer came: whether the
// exchange was recorded, and whether the connection lived on, turned on which
// of the two raced ahead (recorded in 0 or 1 of 30 runs).
func TestIngressRecordsAnUploadTheAppAnsweredAfterItsContinue(t *testing.T) {
	const runs = 20
	for _, mode := range interimModes {
		for _, size := range []int{3, 64 << 10} {
			for i := 0; i < runs; i++ {
				ok := t.Run(fmt.Sprintf("%s/%dB/%d", mode, size, i), func(t *testing.T) {
					app, conns := eagerContinueApp(t)
					client, done, recorded := interimIngress(t, mode, app)
					final, body, r := uploadAfterContinue(t, client, size)
					if final.StatusCode != http.StatusUnauthorized || body != "deny" {
						t.Fatalf("the client got %d %q, want the app's 401", final.StatusCode, body)
					}
					if mode == "normal" {
						if final.Close {
							t.Fatal("the connection ends with the answer, which the app keeps")
						}
						if _, err := io.WriteString(client, "GET /next HTTP/1.1\r\nHost: app.local\r\nConnection: close\r\n\r\n"); err != nil {
							t.Fatal(err)
						}
						if _, next, nextBody := readThroughFinal(t, r); next.StatusCode != http.StatusUnauthorized || nextBody != "deny" {
							t.Fatalf("the next request on the connection got %d %q, want its own answer", next.StatusCode, nextBody)
						}
						if n := conns.Load(); n != 1 {
							t.Fatalf("the app took %d connections for the two requests, want the one it kept", n)
						}
					}
					got := awaitRecorded(t, recorded, "/up")
					if got.method != http.MethodPost || got.reqBody != strings.Repeat("x", size) || got.status != http.StatusUnauthorized || got.respBody != "deny" {
						t.Fatalf("recorded %s with a %d-byte body, answered %d %q; want the POST with its %d-byte body, answered 401 %q",
							got.method, len(got.reqBody), got.status, got.respBody, size, "deny")
					}
					_ = client.Close()
					<-done
				})
				if !ok {
					return // one run that is not recorded is enough
				}
			}
		}
	}
}

// The body after an answer is forwarded under a deadline (on reading the
// client and writing to the app), which is lifted once the body is through:
// the keep-alive connection carries a request that comes after the deadline
// would have passed.
func TestIngressKeepsTheConnectionPastTheBodysDeadline(t *testing.T) {
	app, conns := eagerContinueApp(t)
	client, done, recorded := interimIngress(t, "normal", app)
	_ = client.SetReadDeadline(time.Now().Add(continueDrainTimeout + 5*time.Second))
	final, body, r := uploadAfterContinue(t, client, 3)
	if final.StatusCode != http.StatusUnauthorized || body != "deny" || final.Close {
		t.Fatalf("the client got %d %q (close: %v), want the app's 401 on a connection it keeps", final.StatusCode, body, final.Close)
	}
	if got := awaitRecorded(t, recorded, "/up"); got.reqBody != "xxx" {
		t.Fatalf("recorded the body %q, want the whole of it", got.reqBody)
	}
	time.Sleep(continueDrainTimeout + 500*time.Millisecond)
	if _, err := io.WriteString(client, "GET /next HTTP/1.1\r\nHost: app.local\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if _, next, nextBody := readThroughFinal(t, r); next.StatusCode != http.StatusUnauthorized || nextBody != "deny" {
		t.Fatalf("the next request on the connection got %d %q, want its own answer", next.StatusCode, nextBody)
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("the app took %d connections for the two requests, want the one it kept", n)
	}
	_ = client.Close()
	<-done
}

// A client the app's 100 (Continue) reached, that then sends nothing more and
// stays, does not hold the answer, nor (in --sync) the lock, past
// continueDrainTimeout: the body is cut off there, the client gets the answer
// on a connection that ends with it, and the exchange is not recorded.
func TestIngressCutsOffABodyItsClientDoesNotSendAfterTheContinue(t *testing.T) {
	stubContinueDrainTimeout(t, 300*time.Millisecond)
	app, _ := eagerContinueApp(t)
	for _, mode := range interimModes {
		t.Run(mode, func(t *testing.T) {
			client, done, recorded := interimIngress(t, mode, app)
			if _, err := io.WriteString(client, "POST /up HTTP/1.1\r\nHost: app.local\r\nExpect: 100-continue\r\nContent-Length: 3\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			r := bufio.NewReader(client)
			interim, final, body := readThroughFinal(t, r)
			if len(interim) != 1 || interim[0].StatusCode != http.StatusContinue || final.StatusCode != http.StatusUnauthorized || body != "deny" {
				t.Fatalf("the client got %d interim responses then %d %q, want the 100, then the app's 401", len(interim), final.StatusCode, body)
			}
			if !final.Close {
				t.Fatal("the answer does not say the connection ends with it")
			}
			if _, err := r.ReadByte(); err != io.EOF {
				t.Fatalf("after the answer the connection gave %v, want its end", err)
			}
			_ = client.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the ingress is still waiting for the body")
			}
			select {
			case got := <-recorded:
				t.Fatalf("recorded %+v, a request whose body never came", got)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

// An app that sent the 100 (Continue), read a little of a large upload, and
// answered (413) without reading more, keeping the connection, gets no more of
// the body than its connection takes before the answer: more than
// maxContinueDrainBytes of it is left, so the rest is cut off at once, without
// waiting out continueDrainTimeout, and the write the app no longer reads is
// ended. The client gets the 413 on a connection that ends with it, and the
// ingress reads what the client still sends a moment before it closes, so the
// close resets no answer the client has not read yet. Not recorded.
func TestIngressCutsOffALargeUploadTheAppStoppedReading(t *testing.T) {
	// Only the bound on bytes cuts the body off before the client's read gives up.
	stubContinueDrainTimeout(t, time.Minute)
	stubLingerTimeout(t, time.Minute) // the client's close ends it
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				if _, err := http.ReadRequest(br); err != nil {
					return
				}
				_, _ = io.WriteString(c, "HTTP/1.1 100 Continue\r\n\r\n")
				_, _ = io.ReadFull(br, make([]byte, 10))
				// Meanwhile the ingress fills the connection, and its write
				// of the body waits for room.
				time.Sleep(700 * time.Millisecond)
				_, _ = io.WriteString(c, "HTTP/1.1 413 Content Too Large\r\nContent-Length: 3\r\n\r\nbig")
				<-release // keeps the connection, and reads no more
			}(c)
		}
	}()
	const size = 32 << 20
	for _, mode := range interimModes {
		t.Run(mode, func(t *testing.T) {
			client, done, recorded := interimIngress(t, mode, ln.Addr().String())
			_ = client.SetReadDeadline(time.Now().Add(8 * time.Second))
			if _, err := fmt.Fprintf(client, "POST /up HTTP/1.1\r\nHost: app.local\r\nExpect: 100-continue\r\nContent-Length: %d\r\n\r\n", size); err != nil {
				t.Fatal(err)
			}
			r := bufio.NewReader(client)
			if cont, err := http.ReadResponse(r, nil); err != nil || cont.StatusCode != http.StatusContinue {
				t.Fatalf("the client got (%v, %v), want the app's 100 Continue", cont, err)
			}
			go func() { _, _ = client.Write(make([]byte, size)) }()
			_, final, body := readThroughFinal(t, r)
			if final.StatusCode != http.StatusRequestEntityTooLarge || body != "big" {
				t.Fatalf("the client got %d %q, want the app's 413", final.StatusCode, body)
			}
			if !final.Close {
				t.Fatal("the answer does not say the connection ends with it")
			}
			if _, err := r.ReadByte(); err != io.EOF {
				t.Fatalf("after the answer the connection gave %v, want its end (not a reset)", err)
			}
			assertLingers(t, done)
			_ = client.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the ingress is still writing a body the app does not read")
			}
			select {
			case got := <-recorded:
				t.Fatalf("recorded %+v, a request the app did not get whole", got)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

// A chunked body, whose length nothing tells, is forwarded after an answer up
// to maxContinueDrainBytes, and cut off past them, not waited out: here the
// client sends one with no end. The client gets the answer on a connection
// that ends with it, without a reset, and the exchange is not recorded.
func TestIngressCutsOffAChunkedUploadPastTheBound(t *testing.T) {
	// Only the bound on bytes cuts the body off before the client's read gives up.
	stubContinueDrainTimeout(t, time.Minute)
	stubLingerTimeout(t, time.Minute) // the client's close ends it
	app, _ := eagerContinueApp(t)
	chunk := fmt.Sprintf("%x\r\n%s\r\n", 32<<10, strings.Repeat("x", 32<<10))
	for _, mode := range interimModes {
		t.Run(mode, func(t *testing.T) {
			client, done, recorded := interimIngress(t, mode, app)
			_ = client.SetReadDeadline(time.Now().Add(8 * time.Second))
			if _, err := io.WriteString(client, "POST /up HTTP/1.1\r\nHost: app.local\r\nExpect: 100-continue\r\nTransfer-Encoding: chunked\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			r := bufio.NewReader(client)
			if cont, err := http.ReadResponse(r, nil); err != nil || cont.StatusCode != http.StatusContinue {
				t.Fatalf("the client got (%v, %v), want the app's 100 Continue", cont, err)
			}
			go func() {
				for {
					if _, err := io.WriteString(client, chunk); err != nil {
						return
					}
				}
			}()
			_, final, body := readThroughFinal(t, r)
			if final.StatusCode != http.StatusUnauthorized || body != "deny" {
				t.Fatalf("the client got %d %q, want the app's 401", final.StatusCode, body)
			}
			if !final.Close {
				t.Fatal("the answer does not say the connection ends with it")
			}
			if _, err := r.ReadByte(); err != io.EOF {
				t.Fatalf("after the answer the connection gave %v, want its end (not a reset)", err)
			}
			assertLingers(t, done)
			_ = client.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the ingress is still forwarding a body with no end")
			}
			select {
			case got := <-recorded:
				t.Fatalf("recorded %+v, a request the app did not get whole", got)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

// A request re-sent on a new connection, the one it was sent on having closed
// before any answer (a pooled connection the app dropped), has its answer read
// past interim responses too: the client gets the 103, then the 200, which is
// recorded.
func TestIngressForwardsInterimResponsesAfterARedial(t *testing.T) {
	for _, mode := range []string{"normal", "sampled"} { // the sync loop re-sends nothing
		t.Run(mode, func(t *testing.T) {
			ln, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			var conns atomic.Int32
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					first := conns.Add(1) == 1
					go func(c net.Conn) {
						defer c.Close()
						br := bufio.NewReader(c)
						if _, err := http.ReadRequest(br); err != nil || first {
							return // the first connection closes unanswered
						}
						_, _ = io.WriteString(c, "HTTP/1.1 103 Early Hints\r\nLink: </s.css>; rel=preload\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nfinal")
						_, _ = io.Copy(io.Discard, br)
					}(c)
				}
			}()
			client, done, recorded := interimIngress(t, mode, ln.Addr().String())
			if _, err := io.WriteString(client, "GET /hints HTTP/1.1\r\nHost: app.local\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			interim, final, body := readThroughFinal(t, bufio.NewReader(client))
			if len(interim) != 1 || interim[0].StatusCode != http.StatusEarlyHints || final.StatusCode != http.StatusOK || body != "final" {
				t.Fatalf("the client got %d interim responses then %d %q, want the app's 103, then its 200", len(interim), final.StatusCode, body)
			}
			if got := awaitRecorded(t, recorded, "/hints"); got.status != http.StatusOK || got.respBody != "final" {
				t.Fatalf("recorded %d %q, want the final response, 200 %q", got.status, got.respBody, "final")
			}
			if n := conns.Load(); n != 2 {
				t.Fatalf("the app took %d connections, want the one dropped and the one the request was re-sent on", n)
			}
			_ = client.Close()
			<-done
		})
	}
}

// The connection of an exchange the ingress cut off lingers a moment before
// it closes (lingerClose), for a client that may still be sending; the slot
// it held is given back first: in --sync the lock, which the next exchange
// waits for, and in sampled recording the sampling slot, without which the
// next exchange is passed through unrecorded.
func TestIngressGivesItsSlotBackBeforeItLingers(t *testing.T) {
	stubLingerTimeout(t, time.Minute) // outlasts the next exchange
	stubContinueDrainTimeout(t, 100*time.Millisecond)
	refusing := refusingApp(t)
	plain, _ := interimApp(t)
	for _, mode := range []string{"sync", "sampled"} {
		t.Run(mode, func(t *testing.T) {
			pm, ctx, recorded := interimManager(t, mode, zap.NewNop())
			sem := make(chan struct{}, 1)
			first, firstDone := ingressPipeOn(t, ctx, pm, refusing, sem, nil)
			_ = first.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.WriteString(first, "POST /refuse-whole HTTP/1.1\r\nHost: app.local\r\nExpect: 100-continue\r\nContent-Length: 3\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			r := bufio.NewReader(first)
			if _, final, _ := readThroughFinal(t, r); final.StatusCode != http.StatusExpectationFailed {
				t.Fatalf("the first client got %d, want the app's 417", final.StatusCode)
			}
			if _, err := r.ReadByte(); err != io.EOF {
				t.Fatalf("after the answer the connection gave %v, want its end", err)
			}
			// The first client stays: its connection lingers.
			second, secondDone := ingressPipeOn(t, ctx, pm, plain, sem, nil)
			_ = second.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.WriteString(second, "GET /hints HTTP/1.1\r\nHost: app.local\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			if _, final, body := readThroughFinal(t, bufio.NewReader(second)); final.StatusCode != http.StatusOK || body != "final" {
				t.Fatalf("the next exchange got %d %q, want its 200", final.StatusCode, body)
			}
			if got := awaitRecorded(t, recorded, "/hints"); got.status != http.StatusOK {
				t.Fatalf("recorded %d for the next exchange, want its 200", got.status)
			}
			select {
			case <-firstDone:
				t.Fatal("the cut-off connection did not linger")
			default:
			}
			_ = first.Close() // ends the linger
			_ = second.Close()
			<-firstDone
			<-secondDone
		})
	}
}

// ingressServe serves every connection to a fresh listener through pm, as the
// agent does, until the test ends, and returns the listener's URL.
func ingressServe(t *testing.T, ctx context.Context, pm *IngressProxyManager, upstream string) string {
	t.Helper()
	t.Cleanup(pm.captures.Wait)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	sem := make(chan struct{}, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				pm.handleHttp1Connection(ctx, c, upstream, pm.logger, pm.tcChan, sem, 8080)
			}()
		}
	}()
	return "http://" + ln.Addr().String()
}

// A Go handler that answers a request expecting a 100 (Continue) without
// reading the body (an auth check, a handler with no use for it) gets no 100
// sent for it by net/http, and answers before the body. Its client, curl or
// Go's, sends the body once it stops waiting for the 100 (a second, by
// default); the ingress forwards it, then the answer, and records the exchange
// with its body, as before the interim-response fix, which wrote the whole
// request before it read any response. Cut off when the answer came, the
// exchange was not recorded.
func TestIngressRecordsAnUploadAGoHandlerAnswersWithoutReading(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("no"))
	}))
	t.Cleanup(app.Close)
	for _, mode := range interimModes {
		t.Run(mode, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			pm, ctx, recorded := interimManager(t, mode, zap.New(core))
			base := ingressServe(t, ctx, pm, strings.TrimPrefix(app.URL, "http://"))
			transport := &http.Transport{ExpectContinueTimeout: 100 * time.Millisecond}
			t.Cleanup(transport.CloseIdleConnections)
			req, err := http.NewRequest(http.MethodPost, base+"/reject", strings.NewReader("abc"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Expect", "100-continue")
			resp, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized || string(body) != "no" {
				t.Fatalf("the client got %d %q, want the app's 401", resp.StatusCode, body)
			}
			got := awaitRecorded(t, recorded, "/reject")
			if want := (recordedExchange{http.MethodPost, "/reject", "abc", http.StatusUnauthorized, "no"}); got != want {
				t.Fatalf("recorded %+v, want %+v", got, want)
			}
			if warned := logs.FilterMessageSnippet("Not recording").Len(); warned != 0 {
				t.Fatalf("logged %v, want no warning for an exchange that was recorded", logs.All())
			}
		})
	}
}

// An app that drops the connection of a request that expects a 100 (Continue)
// without an answer gives the ingress no answer to forward the rest of the
// body with: the body is cut off at once, not waited for, and the client is
// told at once (a 502 from the request-framed loop; the end of the connection
// from the synchronous one). Nothing is recorded.
func TestIngressEndsAnUploadWhoseAppLeftWithoutAnAnswer(t *testing.T) {
	stubContinueDrainTimeout(t, time.Minute) // a body waited for outlasts the client's read
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = http.ReadRequest(bufio.NewReader(c)) // and leaves
			}(c)
		}
	}()
	for _, mode := range interimModes {
		t.Run(mode, func(t *testing.T) {
			client, done, recorded := interimIngress(t, mode, ln.Addr().String())
			if _, err := io.WriteString(client, "POST /up HTTP/1.1\r\nHost: app.local\r\nExpect: 100-continue\r\nContent-Length: 3\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(client), nil)
			switch {
			case mode == "sync" && !errors.Is(err, io.ErrUnexpectedEOF) && err != io.EOF:
				t.Fatalf("the client got (%v, %v), want the end of the connection", resp, err)
			case mode != "sync" && (err != nil || resp.StatusCode != http.StatusBadGateway):
				t.Fatalf("the client got (%v, %v), want a 502", resp, err)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the ingress is still waiting on the body of a request the app left")
			}
			select {
			case got := <-recorded:
				t.Fatalf("recorded %+v, a request the app never answered", got)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}
