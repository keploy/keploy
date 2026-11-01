//go:build linux

package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	incoming "go.keploy.io/server/v3/pkg/agent/proxy/incoming"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
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
				_, _ = fmt.Fprintln(w, `{"status":"RUNNING"}`)
				w.(http.Flusher).Flush()
				if tc.again {
					select {
					case <-hungUp:
					case <-release:
						return
					}
					_, _ = fmt.Fprintln(w, `{"status":"STILL RUNNING"}`)
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
			// That the exchange its client left is not recorded is the
			// incoming package's row (it waits on the captures).
			select {
			case <-lc.served:
			case <-lc.never:
				t.Fatalf("the live call's handler never ended: %s", lc.state())
			}
		})
	}
}

// The same hang-up through the proxy's own chain under the proven switch:
// serveTreeListener hands the live handler (incoming.ServeLive) the call's
// tracked conn wrapped as a LIVE call (liveReader) and, for a mock.real
// destination, as one counted to it (treeRealConn). Neither wrapper gave the
// conn it wraps, so the watch found no socket under them and watched
// nothing: in a proven run, the flow that left its test not judged, the app
// never learned its client had gone.
func TestALiveClientsHangUpReachesTheAppThroughTheProxysChain(t *testing.T) {
	for _, tc := range []struct {
		name string
		sync bool
		// real: the destination is mock.real (treeRealConn over
		// liveReader); else only live (liveReader).
		real bool
		want string
	}{
		{"live", false, false, "*proxy.liveReader"},
		{"synchronous: live", true, false, "*proxy.liveReader"},
		{"mock.real", false, true, "*proxy.treeRealConn"},
		{"synchronous: mock.real", true, true, "*proxy.treeRealConn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gone := make(chan struct{})
			release := make(chan struct{})
			t.Cleanup(sync.OnceFunc(func() { close(release) }))
			lc, handed := serveTreeCall(t, tc.sync, tc.real, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprintln(w, `{"status":"RUNNING"}`)
				w.(http.Flusher).Flush()
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
			if got := <-handed; got != tc.want {
				t.Fatalf("precondition: the live handler was handed a %s, want the proxy's %s", got, tc.want)
			}
			_ = lc.app.Close()
			select {
			case <-gone:
			case <-lc.never:
				t.Fatalf("the app never saw its client gone: %s", lc.state())
			}
			select {
			case <-lc.served:
			case <-lc.never:
				t.Fatalf("the live call's handler never ended: %s", lc.state())
			}
		})
	}
}

// An app deaf to its client's going (a long poll quiet until its own
// timeout, a handler that does not read its request's context) holds the
// live call's handler once its client hangs up, but not the test's end: once
// the hang-up is told on to the app, the call's connection holds no test's
// work, in a record as in a replay, for a settle's scope begun while the
// answer streamed as for one begun after. The exchange is not recorded and
// nothing asserts its answer, so waiting on it would prove nothing. Before,
// the handler, forwarding an answer that never came, held the connection for
// good, and the settle's watchdog gave up on it, leaving the round not
// judged.
func TestALiveCallsAppDeafToItsClientsGoingDoesNotHoldTheTestsEnd(t *testing.T) {
	for _, tc := range []struct {
		name string
		sync bool
	}{{"zero-copy", false}, {"synchronous", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deafAppsClientHangsUp(t, func(h http.Handler) *liveCall { return serveLiveCallWith(t, tc.sync, h) })
		})
	}
}

// The same through the proxy's own chain under the proven switch: the hold
// on the hang-up, and its telling, reach the call's tracked conn through the
// wrappers the proxy puts around a live call (liveReader, treeRealConn).
func TestALiveCallsAppDeafToItsClientsGoingDoesNotHoldTheTestsEndThroughTheProxysChain(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sync, real bool
	}{
		{"live", false, false},
		{"synchronous: live", true, false},
		{"mock.real", false, true},
		{"synchronous: mock.real", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deafAppsClientHangsUp(t, func(h http.Handler) *liveCall {
				lc, _ := serveTreeCall(t, tc.sync, tc.real, h)
				return lc
			})
		})
	}
}

// deafAppsClientHangsUp serves (serve) a live call to an app that sees its
// client go and goes on with the request, and checks that once its client
// hangs up mid-answer the call's connection holds no test's work while its
// handler still waits on the app.
func deafAppsClientHangsUp(t *testing.T, serve func(http.Handler) *liveCall) {
	t.Helper()
	gone, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(sync.OnceFunc(func() { close(release) }))
	lc := serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, `{"status":"RUNNING"}`)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(gone)
		case <-release:
			return
		}
		<-release // deaf: it goes on with the request its client left
	}))
	if _, err := lc.app.Write([]byte("GET /client/v1/jobs/j/events HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(lc.app)
	readUntil(t, br, "RUNNING")
	// The chunk's end too: a client that closes with bytes unread resets
	// its socket, and a reset closes the app's connection, which ends the
	// handler whatever the app does. This client sends its FIN.
	if line, err := br.ReadString('\n'); err != nil || line != "\r\n" {
		t.Fatalf("precondition: the chunk's end read %q, %v", line, err)
	}
	streaming := lc.tr.scope()
	if n, _ := streaming.State(true); n != 1 {
		t.Fatalf("precondition: a live call whose answer streams to its client held no work of a replay's settle (pending %d): %s", n, lc.state())
	}
	_ = lc.app.Close()
	select {
	case <-gone:
	case <-lc.never:
		t.Fatalf("the app never saw its client gone: %s", lc.state())
	}
	for {
		changed := lc.tr.Changed()
		n, _ := streaming.State(true)
		if n == 0 && lc.tr.Pending(true) == 0 && lc.tr.Pending(false) == 0 {
			break
		}
		select {
		case <-changed:
		case <-lc.never:
			t.Fatalf("the hang-up told on to an app deaf to it, the call's connection still holds work (scope's %d): %s", n, lc.state())
		}
	}
	if left := lc.tr.scope().PendingProgress(true); len(left) != 0 {
		t.Fatalf("a settle begun after the hang-up was told on would wait on %v: %s", left, lc.state())
	}
	select {
	case <-lc.served:
		t.Fatalf("precondition: the handler ended, though the app still holds the request: %s", lc.state())
	default:
	}
}

// Each wrapper the proxy puts around a live call's conn gives the conn it
// wraps, so the socket under it can be watched for the caller's hang-up.
// Red with any of them not giving it: the watch finds no socket.
func TestEachWrapperOfALiveCallGivesItsSocket(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(net.Conn) net.Conn
	}{
		{"trackedConn", func(c net.Conn) net.Conn { return c }},
		{"liveReader", func(c net.Conn) net.Conn { return &liveReader{Conn: c} }},
		{"treeRealConn over liveReader", func(c net.Conn) net.Conn {
			return &treeRealConn{Conn: &liveReader{Conn: c}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, c := acceptedPair(t, &inflight{})
			stop, watched := pUtil.WatchPeerEnd(tc.wrap(c), func(bool) {})
			defer stop()
			if !watched {
				t.Fatalf("no socket found under %T", tc.wrap(c))
			}
		})
	}
}

// A long poll: the app writes nothing until its event comes, and the client
// hangs up before the answer's first byte. The app is told, as it would be
// without keploy.
func TestALiveClientsHangUpBeforeTheAnswerReachesTheApp(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		t.Run(fmt.Sprintf("synchronous %v", synchronous), func(t *testing.T) {
			t.Parallel()
			gone, entered := make(chan struct{}), make(chan struct{})
			release := make(chan struct{})
			t.Cleanup(sync.OnceFunc(func() { close(release) }))
			lc := serveLiveCallWith(t, synchronous, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				select {
				case <-r.Context().Done():
					close(gone)
				case <-release:
				}
			}))
			if _, err := lc.app.Write([]byte("GET /poll HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-lc.never:
				t.Fatalf("the app never got the request: %s", lc.state())
			}
			_ = lc.app.Close()
			select {
			case <-gone:
			case <-lc.never:
				t.Fatalf("the app never saw its client gone: %s", lc.state())
			}
			select {
			case <-lc.served:
			case <-lc.never:
				t.Fatalf("the live call's handler never ended: %s", lc.state())
			}
		})
	}
}

// A client that hangs up before any answer, on an app that then drops its
// connection without answering, is not sent its request again: the ingress
// sends an idempotent request again on a fresh connection when the app's
// ends with no answer (a stale keep-alive), which here would be a second
// side effect of a request its client gave up on.
func TestALiveClientsHangUpBeforeTheAnswerIsNotSentAgain(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		t.Run(fmt.Sprintf("synchronous %v", synchronous), func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			entered := make(chan struct{}, 4)
			lc := serveLiveCallWith(t, synchronous, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				entered <- struct{}{}
				<-r.Context().Done()
				panic(http.ErrAbortHandler) // drop the connection, no answer
			}))
			if _, err := lc.app.Write([]byte("GET /poll HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-lc.never:
				t.Fatalf("the app never got the request: %s", lc.state())
			}
			_ = lc.app.Close()
			select {
			case <-lc.served:
			case <-lc.never:
				t.Fatalf("the live call's handler never ended: %s", lc.state())
			}
			if n := calls.Load(); n != 1 {
				t.Fatalf("the app got the request %d times; its client had gone before the first answer", n)
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
		_, _ = fmt.Fprintln(w, `{"status":"RUNNING"}`)
		w.(http.Flusher).Flush()
		<-release
		_, _ = fmt.Fprintln(w, `{"status":"DONE"}`)
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
	const slowReq, fastReq = "GET /slow HTTP/1.1\r\nHost: api\r\n\r\n", "GET /fast HTTP/1.1\r\nHost: api\r\n\r\n"
	for _, tc := range []struct {
		name string
		// atOnce: both requests and the FIN come in one go, so the
		// ingress has the next request buffered as it forwards the first's
		// answer; else the next is sent once the first answer has begun.
		atOnce bool
	}{
		{"sent once the first answer began", false},
		{"sent at once", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			release, began := make(chan struct{}), make(chan struct{})
			free := sync.OnceFunc(func() { close(release) })
			t.Cleanup(free)
			lc := serveLiveCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// A watch that took a byte of the next request
				// leaves it a request still, with a method cut
				// short ("ET").
				if r.Method != http.MethodGet {
					http.Error(w, "method "+r.Method, http.StatusBadRequest)
					return
				}
				if r.URL.Path == "/fast" {
					_, _ = fmt.Fprint(w, "fast")
					return
				}
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprintln(w, `{"status":"RUNNING"}`)
				w.(http.Flusher).Flush()
				close(began)
				select {
				case <-release:
					_, _ = fmt.Fprintln(w, `{"status":"DONE"}`)
				case <-r.Context().Done():
					_, _ = fmt.Fprintln(w, `{"status":"TOLD THE CLIENT WENT"}`)
				}
			}))
			first := slowReq
			if tc.atOnce {
				first += fastReq
			}
			if _, err := lc.app.Write([]byte(first)); err != nil {
				t.Fatal(err)
			}
			br := bufio.NewReader(lc.app)
			slow, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("the first answer: %v (%s)", err, lc.state())
			}
			events := bufio.NewReader(slow.Body)
			readUntil(t, events, "RUNNING")
			if !tc.atOnce {
				if _, err := lc.app.Write([]byte(fastReq)); err != nil {
					t.Fatal(err)
				}
			}
			if err := lc.app.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			// The next request's bytes and the client's end reach the
			// proxy's socket before the answer goes on: a watch that took
			// either for the client's going tells the app before the
			// release.
			<-began
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
		})
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
		_, _ = fmt.Fprintln(w, `{"line":1}`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		_, _ = fmt.Fprintln(w, `{"line":2}`)
		_, _ = fmt.Fprintln(w, `{"line":3}`)
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

// serveTreeCall serves a live call through the proxy's own chain, under the
// proven switch: serveTreeListener, as handleConnection calls it for a call
// to a listener of the run's tree (the app server here, this process's,
// called by this process's parent, which it descends from), and its live
// handler incoming.ServeLive. real lists the app server as a mock.real
// destination. handed gets the type of the conn the live handler was handed.
// Not parallel: the switch is the environment's.
func serveTreeCall(t *testing.T, synchronous, real bool, h http.Handler) (lc *liveCall, handed <-chan string) {
	t.Helper()
	srv := &http.Server{Handler: h}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return serveTreeCallTo(t, synchronous, real, ln.Addr().(*net.TCPAddr))
}

// serveTreeCallTo is serveTreeCall to a listener the caller started, at
// addr (this process's).
func serveTreeCallTo(t *testing.T, synchronous, real bool, tcpAddr *net.TCPAddr) (lc *liveCall, handed <-chan string) {
	t.Helper()
	p, _ := provenTreeProxy(t)
	addr := tcpAddr.String()
	if real {
		if err := p.SetRealDeps(context.Background(), []string{addr}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{}
	cfg.Agent.MockMode = true
	cfg.Agent.RecordRequests = true
	cfg.Agent.Synchronous = synchronous
	lc = &liveCall{tr: &inflight{}, served: make(chan struct{}), pm: incoming.New(zap.NewNop(), nil, cfg)}
	kinds := make(chan string, 1)
	p.live = func(ctx context.Context, conn net.Conn, upstream string, port uint16) {
		kinds <- fmt.Sprintf("%T", conn)
		lc.pm.ServeLive(ctx, conn, upstream, port)
	}
	lc.app, lc.proxy = acceptedPair(t, lc.tr)
	port := uint32(tcpAddr.Port)
	dest := &agent.NetworkAddress{Version: 4, Port: port, KernelPid: uint32(os.Getppid()), AgentNsPid: uint32(os.Getppid())}
	go func() {
		defer close(lc.served)
		defer func() { _ = lc.proxy.Close() }() // as handleConnection does once it returns
		if served, _, err := p.serveTreeListener(context.Background(), lc.proxy, dest, addr, "1"); !served || err != nil {
			kinds <- fmt.Sprintf("not served (served %v, err %v)", served, err)
		}
	}()
	never := make(<-chan time.Time)
	if d, ok := t.Deadline(); ok {
		never = time.After(time.Until(d) - 5*time.Second)
	}
	lc.never = never
	return lc, kinds
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
