//go:build linux

package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
	incoming "go.keploy.io/server/v3/pkg/agent/proxy/incoming"
)

// A test's client that hangs up on a live call while the app's answer still
// streams (a job's event stream the client stops following) is told to the
// app, as it would be without keploy, and the call's connection settles: the
// test's end does not wait on it. Before, the HTTP/1 ingress read only the
// app while it forwarded the answer: an app that sent nothing more (it waits
// for events that never come) held the handler, so the app never learned its
// client had gone and the connection held work for good. Its socket in
// CLOSE_WAIT, it held a request not answered; once the app wrote again and
// the client's kernel reset the socket (TCP_CLOSE), its counts took the
// client's FIN for one byte no read returned, and the settle's watchdog gave
// up on it ("held 1 byte(s) the application had sent that its handler had not
// read"), leaving the test not judged, its window open for the app's
// background calls to fill. The exchange its client left is not recorded: the
// client did not take the answer.
func TestALiveClientsHangUpMidAnswerReachesTheAppAndSettles(t *testing.T) {
	for _, tc := range []struct {
		name string
		// again: the app writes once more after the client hung up.
		again bool
		// sync: the ingress's synchronous loop serves the call.
		sync bool
	}{
		{"the client's FIN, the app quiet", false, false},
		{"the client's FIN, then a line its kernel resets", true, false},
		{"synchronous: the client's FIN, the app quiet", false, true},
		{"synchronous: the client's FIN, then a line its kernel resets", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gone, hungUp := make(chan struct{}), make(chan struct{})
			release := make(chan struct{})
			t.Cleanup(sync.OnceFunc(func() { close(release) }))
			lc := serveLiveCallWith(t, tc.sync, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				fmt.Fprintln(w, `{"status":"RUNNING"}`)
				w.(http.Flusher).Flush()
				if tc.again {
					select {
					case <-hungUp:
					case <-release:
						return
					}
					fmt.Fprintln(w, `{"status":"STILL RUNNING"}`)
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
					close(gone)
				case <-release:
				}
			}))
			if _, err := lc.app.Write([]byte("GET /client/v1/jobs/j/events HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			readUntil(t, bufio.NewReader(lc.app), "RUNNING")
			_ = lc.app.Close()
			close(hungUp)

			select {
			case <-gone:
			case <-lc.never:
				t.Fatalf("the app never saw its client gone: %s", lc.state())
			}
			// The proxy forgets a connection once its handler is done
			// (handleConnection): the handler's end is the settle's.
			select {
			case <-lc.served:
			case <-lc.never:
				t.Fatalf("the live call's handler never ended: %s", lc.state())
			}
			select {
			case got := <-lc.pm.TCChan():
				t.Fatalf("the exchange its client left was recorded: %v", got.HTTPReq.URL)
			default:
			}
		})
	}
}

// A live call whose client stays holds work while its answer streams: the
// watch on the client only peeks at its socket, so the handler is not taken
// for one blocked reading its next request (the settle would call the call
// answered mid-answer), and once the app ends the answer the handler goes back
// to reading, the exchange recorded whole.
func TestALiveCallsAnswerStillHoldsWorkWhileTheClientWaits(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	free := sync.OnceFunc(func() { close(release) })
	t.Cleanup(free)
	lc := serveLiveCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"status":"RUNNING"}`)
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprintln(w, `{"status":"DONE"}`)
	}))
	if _, err := lc.app.Write([]byte("GET /client/v1/jobs/j/events HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(lc.app)
	readUntil(t, br, "RUNNING")
	if n := lc.tr.Pending(true); n != 1 {
		t.Fatalf("a live call mid-answer holds no work: %s", lc.state())
	}
	free()
	readUntil(t, br, "DONE")
	waitSettled(t, lc.tr, func() bool { return lc.tr.Pending(true) == 0 })
	select {
	case got := <-lc.pm.TCChan():
		if got.HTTPReq.URL == "" || !strings.Contains(got.HTTPResp.Body, "DONE") {
			t.Fatalf("the exchange was recorded cut: %+v", got.HTTPResp)
		}
	case <-lc.never:
		t.Fatalf("the whole exchange was not recorded: %s", lc.state())
	}
}

// A client that sends its next request while the answer to the one before
// still streams (pipelining), then half-closes, gets both answers: its next
// bytes end the watch and stay for the next read, and the app is not told of
// an end that came after a request it has not been sent.
func TestALiveCallsPipelinedRequestIsStillServed(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	free := sync.OnceFunc(func() { close(release) })
	t.Cleanup(free)
	lc := serveLiveCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fast" {
			fmt.Fprint(w, "fast")
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"status":"RUNNING"}`)
		w.(http.Flusher).Flush()
		select {
		case <-release:
			fmt.Fprintln(w, `{"status":"DONE"}`)
		case <-r.Context().Done():
			fmt.Fprintln(w, `{"status":"TOLD THE CLIENT WENT"}`)
		}
	}))
	if _, err := lc.app.Write([]byte("GET /slow HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(lc.app)
	slow, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("the first answer: %v (%s)", err, lc.state())
	}
	events := bufio.NewReader(slow.Body)
	readUntil(t, events, "RUNNING")
	if _, err := lc.app.Write([]byte("GET /fast HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := lc.app.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	// The next request's bytes and the client's end reach the proxy's
	// socket before the answer goes on: a watch that took either for the
	// client's going tells the app before the release.
	lc.waitClientFIN(t)
	free()
	rest, err := io.ReadAll(events)
	if err != nil || !strings.Contains(string(rest), "DONE") {
		t.Fatalf("the first answer ended %q (%v): %s", rest, err, lc.state())
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("the pipelined request's answer: %v (%s)", err, lc.state())
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "fast" {
		t.Fatalf("the pipelined request's answer is %q", body)
	}
}

// A client that half-closes once its request is sent, and then reads, gets
// the whole answer of an app that answers on: the app is told the client's
// FIN, as it would be without keploy (a Go server cancels the request's
// context), and its connection stays open to write on.
func TestALiveCallsHalfClosingClientGetsTheWholeAnswer(t *testing.T) {
	t.Parallel()
	lc := serveLiveCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"line":1}`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		fmt.Fprintln(w, `{"line":2}`)
		fmt.Fprintln(w, `{"line":3}`)
	}))
	if _, err := lc.app.Write([]byte("GET /lines HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := lc.app.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(lc.app), nil)
	if err != nil {
		t.Fatalf("the answer: %v (%s)", err, lc.state())
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || !strings.Contains(string(body), `{"line":3}`) {
		t.Fatalf("the half-closed client got %q (%v): %s", body, err, lc.state())
	}
	select {
	case <-lc.served:
	case <-lc.never:
		t.Fatalf("the live call's handler never ended: %s", lc.state())
	}
}

// liveCall is a live call the proxy serves through incoming.ServeLive: the
// client's end (app), the proxy's tracked end of it, the app server behind.
type liveCall struct {
	app    net.Conn
	proxy  *trackedConn
	tr     *inflight
	pm     *incoming.IngressProxyManager
	served chan struct{}
	// never fires at the test binary's deadline less 5 s: a hang that never
	// ends fails there, not at a budget of its own (the tests run in
	// parallel, so each has all of it).
	never <-chan time.Time
}

func serveLiveCall(t *testing.T, h http.Handler) *liveCall {
	t.Helper()
	return serveLiveCallWith(t, false, h)
}

func serveLiveCallWith(t *testing.T, synchronous bool, h http.Handler) *liveCall {
	t.Helper()
	srv := &http.Server{Handler: h}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	cfg := &config.Config{}
	cfg.Agent.MockMode = true
	cfg.Agent.RecordRequests = true
	cfg.Agent.Synchronous = synchronous
	lc := &liveCall{tr: &inflight{}, served: make(chan struct{}), pm: incoming.New(zap.NewNop(), nil, cfg)}
	lc.app, lc.proxy = acceptedPair(t, lc.tr)
	go func() {
		defer close(lc.served)
		lc.pm.ServeLive(context.Background(), lc.proxy, ln.Addr().String(), 8083)
	}()
	never := make(<-chan time.Time)
	if d, ok := t.Deadline(); ok {
		never = time.After(time.Until(d) - 5*time.Second)
	}
	lc.never = never
	return lc
}

func (lc *liveCall) state() string {
	_, st, ok := socketReceived(lc.proxy.raw)
	v := lc.proxy.view()
	return fmt.Sprintf("socket state %d (asked: %v), delivered %d, untaken %d, eof %v, reading %d, pending in a replay %d",
		st, ok, v.delivered, lc.proxy.untaken(v), v.eof, v.reading, lc.tr.Pending(true))
}

// waitClientFIN waits until the proxy's socket has the client's FIN, and so
// all the client sent before it.
func (lc *liveCall) waitClientFIN(t *testing.T) {
	t.Helper()
	for {
		if _, st, ok := socketReceived(lc.proxy.raw); ok && st == unix.BPF_TCP_CLOSE_WAIT {
			return
		}
		select {
		case <-lc.never:
			t.Fatalf("the client's FIN never reached the proxy: %s", lc.state())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// readUntil reads lines from br until one holds want.
func readUntil(t *testing.T, br *bufio.Reader, want string) {
	t.Helper()
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("waiting for %q: %v", want, err)
		}
		if strings.Contains(line, want) {
			return
		}
	}
}
