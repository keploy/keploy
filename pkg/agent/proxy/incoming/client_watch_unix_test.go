//go:build linux || darwin

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
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/config"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
)

// An exchange whose client hung up before the answer's end is not recorded,
// in either loop: the client did not take the answer, and the app's end of it
// is its being told the client went (watchClient). Its capture, were one
// started, is over once captures is waited on, so the check is not a race
// with it. A half-closed client may have read the whole answer: that its
// exchange is not recorded is told at Info, as a test the user expects would
// otherwise be missing with no trace.
func TestAnExchangeItsClientLeftIsNotRecorded(t *testing.T) {
	for _, tc := range []struct {
		synchronous bool
		// halfClose: the client half-closes and reads the whole answer,
		// which the app ends once told; else it closes.
		halfClose bool
	}{{false, false}, {true, false}, {false, true}, {true, true}} {
		synchronous := tc.synchronous
		t.Run(fmt.Sprintf("synchronous %v, half-closed %v", tc.synchronous, tc.halfClose), func(t *testing.T) {
			t.Parallel()
			gone := make(chan struct{})
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprintln(w, `{"status":"RUNNING"}`)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(gone)
				_, _ = fmt.Fprintln(w, `{"status":"CLIENT GONE"}`)
			})}
			appLn, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go func() { _ = srv.Serve(appLn) }()
			t.Cleanup(func() { _ = srv.Close() })

			cfg := &config.Config{}
			cfg.Agent.MockMode = true
			cfg.Agent.RecordRequests = true
			cfg.Agent.Synchronous = synchronous
			core, logs := observer.New(zapcore.InfoLevel)
			pm := New(zap.New(core), nil, cfg)

			client, proxied := tcpPairForWatch(t)
			served := make(chan struct{})
			go func() {
				defer close(served)
				defer func() { _ = proxied.Close() }() // as the caller does once the handler returns
				pm.ServeLive(context.Background(), proxied, appLn.Addr().String(), 8083)
			}()
			if _, err := client.Write([]byte("GET /client/v1/jobs/j/events HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			br := bufio.NewReader(client)
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					t.Fatalf("the answer's first event: %v", err)
				}
				if strings.Contains(line, "RUNNING") {
					break
				}
			}
			if tc.halfClose {
				_ = client.(*net.TCPConn).CloseWrite()
				rest, err := io.ReadAll(br)
				if err != nil || !strings.Contains(string(rest), "CLIENT GONE") {
					t.Fatalf("the half-closed client got %q, %v", rest, err)
				}
			} else {
				_ = client.Close()
			}

			// A hang fails at the test binary's deadline, not at a budget
			// of its own.
			never := make(<-chan time.Time)
			if d, ok := t.Deadline(); ok {
				never = time.After(time.Until(d) - 5*time.Second)
			}
			for _, w := range []struct {
				what string
				c    <-chan struct{}
			}{{"the app saw its client gone", gone}, {"the handler ended", served}} {
				select {
				case <-w.c:
				case <-never:
					t.Fatalf("never: %s", w.what)
				}
			}
			pm.captures.Wait()
			select {
			case tc := <-pm.TCChan():
				t.Fatalf("the exchange its client left was recorded: %s %s", tc.HTTPReq.Method, tc.HTTPReq.URL)
			default:
			}
			if !tc.halfClose {
				return
			}
			told := logs.FilterMessageSnippet("Not recording this request as a test case: its client closed its side").
				FilterField(zap.String("url", "/client/v1/jobs/j/events")).Len()
			if told != 1 {
				t.Fatalf("the half-closed client's exchange not recorded was told %d times at Info, want once", told)
			}
		})
	}
}

// tcpPairForWatch is a connected TCP pair: the client's end, and the proxy's.
func tcpPairForWatch(t *testing.T) (client, proxied net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	proxied = <-accepted
	if proxied == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = client.Close(); _ = proxied.Close() })
	return client, proxied
}

// A client conn with no socket to be found under it (a wrapper that does not
// give the conn it wraps) is not watched, and that is told at Info, once for
// its kind: never a silent no-op, where its app would not be told its
// client went.
func TestAClientThatCannotBeWatchedIsToldOnce(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	logger := zap.New(core)
	_, proxied := tcpPairForWatch(t)
	c := unwatchable{proxied}
	unwatched.Delete(fmt.Sprintf("%T", c)) // a run before this one (-count) told it
	for i := 0; i < 2; i++ {
		stop := watchClient(logger, c, bufio.NewReader(c), func() net.Conn { return nil }, nothingArrived)
		if stop.endedBefore(-1) {
			t.Fatal("an unwatched client reported an end")
		}
	}
	told := logs.FilterMessageSnippet("cannot watch this kind of client connection").
		FilterField(zap.String("connection", fmt.Sprintf("%T", c))).Len()
	if told != 1 {
		t.Fatalf("an unwatchable client was told %d times, want once", told)
	}
}

// unwatchable wraps a conn and does not give it (no NetConn).
type unwatchable struct{ net.Conn }

// nothingArrived is a watch's count of what arrived from an app that sent
// nothing.
func nothingArrived() int64 { return 0 }

// Whether a client went before the answer's end is decided by what had
// arrived from the app as its going was seen, not by when the ingress found
// the end: the count read as the going is seen, before the app is told,
// against where the answer ended in it. A client that went once the answer's
// end had arrived (a client that took the headers and closed, the body
// already with keploy) did not end the exchange; one that went before it
// did, and so did one whose answer's end was never found.
func TestAClientEndsTheExchangeOnlyBeforeTheAnswersEndArrived(t *testing.T) {
	const answerEnd = 120
	for _, tc := range []struct {
		name    string
		arrived int64
		end     int64
		want    bool
	}{
		{"the end had arrived", answerEnd, answerEnd, false},
		{"more than the end had arrived", answerEnd + 40, answerEnd, false},
		{"the end had not arrived", answerEnd - 1, answerEnd, true},
		{"the end was never found", answerEnd, -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, proxied := tcpPairForWatch(t)
			appSide, app := tcpPairForWatch(t)
			var arrived atomic.Int64
			arrived.Store(tc.arrived)
			stop := watchClient(zap.NewNop(), proxied, bufio.NewReader(proxied), func() net.Conn { return appSide }, arrived.Load)
			_ = client.Close()
			// The app is told: its end of the connection reads the end.
			if n, err := app.Read(make([]byte, 1)); n != 0 || err != io.EOF {
				t.Fatalf("the app read %d %v, want the client's going", n, err)
			}
			// What arrives after the going was seen counts for nothing.
			arrived.Add(1000)
			if ended := stop.endedBefore(tc.end); ended != tc.want {
				t.Fatalf("with %d arrived as the client went and the answer's end at %d, stop reported an end %v, want %v", tc.arrived, tc.end, ended, tc.want)
			}
		})
	}
}

// The connection the proxy keeps under a watched client (proxyutil.
// HangUpTeller) holds the client's hang-up from the watch's start, and, once
// the hang-up is told on to the app, is told it was: from then on it holds no
// test's work, so a test's end does not wait on an app that goes on with the
// request its client left. A client that stays has the hold released at the
// watch's stop, nothing told.
func TestAWatchedClientsHangUpIsHeldUntilToldOn(t *testing.T) {
	for _, leaves := range []bool{false, true} {
		name := "the client stays"
		if leaves {
			name = "the client leaves"
		}
		t.Run(name, func(t *testing.T) {
			client, proxied := tcpPairForWatch(t)
			appSide, app := tcpPairForWatch(t)
			c := &hangUpTeller{Conn: proxied}
			stop := watchClient(zap.NewNop(), c, bufio.NewReader(c), func() net.Conn { return appSide }, nothingArrived)
			if held, released, told := c.counts(); held != 1 || released != 0 || told != 0 {
				t.Fatalf("as the watch runs: held %d, released %d, told %d; want the hang-up held once", held, released, told)
			}
			if leaves {
				_ = client.Close()
				// The app is told: its end of the connection reads the end.
				if n, err := app.Read(make([]byte, 1)); n != 0 || err != io.EOF {
					t.Fatalf("the app read %d %v, want the client's going", n, err)
				}
			}
			if ended := stop.endedBefore(-1); ended != leaves {
				t.Fatalf("stop reported an end %v, want %v", ended, leaves)
			}
			_ = stop.endedBefore(-1)
			want := 0
			if leaves {
				want = 1
			}
			if held, released, told := c.counts(); held != 1 || released != 1 || told != want {
				t.Fatalf("after the watch: held %d, released %d, told %d; want held and released once, told %d", held, released, told, want)
			}
		})
	}
}

// A kept conn (proxyutil.HangUpTeller) under a client the watch cannot watch
// (no socket under it, as for every client on Windows, whose WatchPeerEnd
// watches nothing) has the hold on its hang-up released at once, nothing
// told. A hold left held keeps the conn busy for good once its client goes,
// so every test's end with such a client would give up on it.
func TestAnUnwatchedKeptConnHoldsNoHangUp(t *testing.T) {
	a, b := net.Pipe() // no socket under it
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	c := &hangUpTeller{Conn: b}
	stop := watchClient(zap.NewNop(), c, bufio.NewReader(c), func() net.Conn { return nil }, nothingArrived)
	if held, released, told := c.counts(); held != 1 || released != 1 || told != 0 {
		t.Fatalf("an unwatched client: held %d, released %d, told %d; want held and released at once, nothing told", held, released, told)
	}
	if stop.endedBefore(-1) {
		t.Fatal("an unwatched client reported an end")
	}
	if held, released, told := c.counts(); held != 1 || released != 1 || told != 0 {
		t.Fatalf("after stop: held %d, released %d, told %d; want held and released once, nothing told", held, released, told)
	}
}

// hangUpTeller is a kept conn (proxyutil.HangUpTeller) that counts its holds
// on the hang-up, their releases, and the tellings.
type hangUpTeller struct {
	net.Conn
	mu                    sync.Mutex
	held, released, toldN int
}

func (c *hangUpTeller) NetConn() net.Conn { return c.Conn }

func (c *hangUpTeller) HoldHangUp() func() {
	c.mu.Lock()
	c.held++
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		c.released++
		c.mu.Unlock()
	}
}

func (c *hangUpTeller) HangUpTold() {
	c.mu.Lock()
	c.toldN++
	c.mu.Unlock()
}

func (c *hangUpTeller) counts() (held, released, told int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.held, c.released, c.toldN
}

// serveWatched serves, in the ingress's synchronous loop or its zero-copy
// one, a live call from a new client to the app on appLn, the client's conn
// handed to the ingress as wrap makes it (nil: as accepted). served is closed
// once the handler ends; logs are its lines at Info and up.
func serveWatched(t *testing.T, synchronous bool, appLn net.Listener, wrap func(net.Conn) net.Conn) (pm *IngressProxyManager, client net.Conn, served <-chan struct{}, logs *observer.ObservedLogs) {
	t.Helper()
	return serveWatchedIn(t, context.Background(), synchronous, appLn, wrap)
}

// serveWatchedIn is serveWatched with the handler's ctx: one that carries a
// mock manager of the test's own counts what the pause costs there.
func serveWatchedIn(t *testing.T, ctx context.Context, synchronous bool, appLn net.Listener, wrap func(net.Conn) net.Conn) (pm *IngressProxyManager, client net.Conn, served <-chan struct{}, logs *observer.ObservedLogs) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Agent.MockMode = true
	cfg.Agent.RecordRequests = true
	cfg.Agent.Synchronous = synchronous
	core, logs := observer.New(zapcore.InfoLevel)
	pm = New(zap.New(core), nil, cfg)
	client, proxied := tcpPairForWatch(t)
	handed := proxied
	if wrap != nil {
		handed = wrap(proxied)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = proxied.Close() }() // as the caller does once the handler returns
		pm.ServeLive(ctx, handed, appLn.Addr().String(), 8083)
	}()
	return pm, client, done, logs
}

// serveApp serves h on ln until the test ends.
func serveApp(t *testing.T, ln net.Listener, h http.HandlerFunc) {
	t.Helper()
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// never fires at the test binary's deadline less 5 s: a hang fails there,
// not at a budget of its own.
func never(t *testing.T) <-chan time.Time {
	if d, ok := t.Deadline(); ok {
		return time.After(time.Until(d) - 5*time.Second)
	}
	return make(<-chan time.Time)
}

// streamUntilGone is an app handler that reads the request's body, streams
// one event, and closes gone once it sees its client go.
func streamUntilGone(gone chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, `{"status":"RUNNING"}`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(gone)
	}
}

// hangUpAfterTheFirstEvent reads the answer's first event, and its chunk's
// end (a client that closes with bytes unread resets its socket), and closes.
func hangUpAfterTheFirstEvent(t *testing.T, client net.Conn, br *bufio.Reader) {
	t.Helper()
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("the answer's first event: %v", err)
		}
		if strings.Contains(line, "RUNNING") {
			break
		}
	}
	if line, err := br.ReadString('\n'); err != nil || line != "\r\n" {
		t.Fatalf("the chunk's end read %q, %v", line, err)
	}
	_ = client.Close()
}

// A request sent with "Expect: 100-continue" has its client watched once its
// body, which waited for the app's 100, is all read: its hang-up mid-answer
// reaches the app, in either loop. Before the body is read the client is not
// watched (it is still sending), so the watch starts only there.
func TestAHundredContinueClientsHangUpReachesTheApp(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		t.Run(fmt.Sprintf("synchronous %v", synchronous), func(t *testing.T) {
			t.Parallel()
			gone := make(chan struct{})
			appLn, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			serveApp(t, appLn, streamUntilGone(gone))
			_, client, served, _ := serveWatched(t, synchronous, appLn, nil)
			if _, err := client.Write([]byte("POST /client/v1/jobs HTTP/1.1\r\nHost: api\r\nExpect: 100-continue\r\nContent-Length: 4\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			br := bufio.NewReader(client)
			if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "HTTP/1.1 100") {
				t.Fatalf("precondition: the app's interim answer read %q, %v", line, err)
			}
			if line, err := br.ReadString('\n'); err != nil || line != "\r\n" {
				t.Fatalf("precondition: the interim answer's end read %q, %v", line, err)
			}
			if _, err := client.Write([]byte("ping")); err != nil {
				t.Fatal(err)
			}
			hangUpAfterTheFirstEvent(t, client, br)
			for _, w := range []struct {
				what string
				c    <-chan struct{}
			}{{"the app saw its client gone", gone}, {"the handler ended", served}} {
				select {
				case <-w.c:
				case <-never(t):
					t.Fatalf("never: %s", w.what)
				}
			}
		})
	}
}

// The zero-copy loop sends an idempotent request again, on a new connection,
// when the app's connection ended with no answer (a stale one): the client
// is watched again from there, its app the new connection, so its hang-up
// mid-answer reaches the app that answers.
func TestAClientsHangUpReachesTheAppAfterARedialAndReplay(t *testing.T) {
	gone := make(chan struct{})
	appLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stale := &dropsFirst{Listener: appLn, dropped: make(chan struct{})}
	serveApp(t, stale, streamUntilGone(gone))
	_, client, served, _ := serveWatched(t, false, appLn, nil)
	if _, err := client.Write([]byte("GET /client/v1/jobs/j/events HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	hangUpAfterTheFirstEvent(t, client, bufio.NewReader(client))
	select {
	case <-stale.dropped:
	default:
		t.Fatal("precondition: the app's first connection answered; no redial and replay was made")
	}
	for _, w := range []struct {
		what string
		c    <-chan struct{}
	}{{"the app saw its client gone", gone}, {"the handler ended", served}} {
		select {
		case <-w.c:
		case <-never(t):
			t.Fatalf("never: %s", w.what)
		}
	}
}

// dropsFirst is a listener whose first connection reads one request's head
// and is closed with no answer, as a stale pooled connection is (dropped is
// closed just before it); the rest are accepted as they come.
type dropsFirst struct {
	net.Listener
	once    sync.Once
	dropped chan struct{}
}

func (l *dropsFirst) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		first := false
		l.once.Do(func() { first = true })
		if !first {
			return c, nil
		}
		go func() {
			br := bufio.NewReader(c)
			for {
				if line, err := br.ReadString('\n'); err != nil || line == "\r\n" {
					break
				}
			}
			// dropped is closed before the conn is: the ingress redials
			// only once it sees this close, so a client that has an
			// answer from the redial finds dropped closed. Closed after,
			// the ingress could redial, replay and answer while this
			// goroutine waited to run.
			close(l.dropped)
			_ = c.Close()
		}()
	}
}

// The watch of a client ends as the answer's end is read from the app,
// before the client can have that end: a client that closes once it has the
// whole answer (as most do) is never taken for one that hung up, and its
// exchange is recorded, in either loop. Its kept conn notes, as the answer's
// last bytes are written to it, whether the watch was over by then.
func TestAClientThatClosesOnceItHasTheAnswerIsRecorded(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		t.Run(fmt.Sprintf("synchronous %v", synchronous), func(t *testing.T) {
			t.Parallel()
			appLn, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			serveApp(t, appLn, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "10")
				_, _ = io.WriteString(w, "ANSWER-END")
			})
			var kept *answerEndConn
			pm, client, served, _ := serveWatched(t, synchronous, appLn, func(c net.Conn) net.Conn {
				kept = &answerEndConn{hangUpTeller: &hangUpTeller{Conn: c}}
				return kept
			})
			if _, err := client.Write([]byte("GET /client/v1/jobs/j HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(client), nil)
			if err != nil {
				t.Fatal(err)
			}
			if body, err := io.ReadAll(resp.Body); err != nil || string(body) != "ANSWER-END" {
				t.Fatalf("the answer read %q, %v", body, err)
			}
			_ = client.Close()
			select {
			case <-served:
			case <-never(t):
				t.Fatal("never: the handler ended")
			}
			if over := kept.watchOverAtEnd(); len(over) != 1 || !over[0] {
				t.Fatalf("as the answer's last bytes were written to the client, the watch was over: %v, want once, true", over)
			}
			pm.captures.Wait()
			select {
			case tc := <-pm.TCChan():
				if !strings.HasSuffix(tc.HTTPReq.URL, "/client/v1/jobs/j") {
					t.Fatalf("recorded %s", tc.HTTPReq.URL)
				}
			default:
				t.Fatal("the exchange of a client that closed once it had the whole answer was not recorded")
			}
		})
	}
}

// answerEndConn is a kept conn (hangUpTeller) that notes, at each write that
// ends with the answer's last bytes ("-END"), whether the watch of its
// client was over: its hold on the hang-up taken and released.
type answerEndConn struct {
	*hangUpTeller
	mu   sync.Mutex
	over []bool
}

func (c *answerEndConn) Write(b []byte) (int, error) {
	if strings.HasSuffix(string(b), "-END") {
		held, released, _ := c.counts()
		c.mu.Lock()
		c.over = append(c.over, held > 0 && held == released)
		c.mu.Unlock()
	}
	return c.hangUpTeller.Write(b)
}

func (c *answerEndConn) watchOverAtEnd() []bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]bool(nil), c.over...)
}

// rawApp serves ln as an app that is not a Go server: each connection's
// request head is read, then serve has it. accepted counts the connections.
func rawApp(t *testing.T, ln net.Listener, serve func(n int, c net.Conn, br *bufio.Reader)) (accepted *atomic.Int32) {
	t.Helper()
	accepted = new(atomic.Int32)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n := int(accepted.Add(1))
			go func() {
				defer func() { _ = c.Close() }()
				br := bufio.NewReader(c)
				for {
					if line, err := br.ReadString('\n'); err != nil || line == "\r\n" {
						break
					}
				}
				serve(n, c, br)
			}()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return accepted
}

// untilEnd reads c until its peer's end.
func untilEnd(br *bufio.Reader) {
	_, _ = io.Copy(io.Discard, br)
}

// An app told of its client's going that ends its answer there, closing
// the connection mid-stream (a server that is not Go's sends no last
// chunk), fails the answer's forwarding on the client's going: that is told
// at Info as the exchange not recorded, as any exchange its client left is,
// and is no error of keploy's or the app's, in either loop.
func TestAnAnswerCutShortByItsClientsGoingIsToldAsNotRecorded(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		t.Run(fmt.Sprintf("synchronous %v", synchronous), func(t *testing.T) {
			t.Parallel()
			appLn, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			rawApp(t, appLn, func(_ int, c net.Conn, br *bufio.Reader) {
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nTransfer-Encoding: chunked\r\n\r\n15\r\n{\"status\":\"RUNNING\"}\n\r\n")
				untilEnd(br) // told of its client's going: it ends its answer
			})
			_, client, served, logs := serveWatched(t, synchronous, appLn, nil)
			if _, err := client.Write([]byte("GET /client/v1/jobs/j/events HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			hangUpAfterTheFirstEvent(t, client, bufio.NewReader(client))
			select {
			case <-served:
			case <-never(t):
				t.Fatal("never: the handler ended")
			}
			if n := logs.FilterMessageSnippet("Not recording this request as a test case: its client closed").
				FilterField(zap.String("url", "/client/v1/jobs/j/events")).Len(); n != 1 {
				t.Fatalf("the exchange its client left was told %d times at Info, want once; logs: %v", n, logs.All())
			}
			if n := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); n != 0 {
				t.Fatalf("a client's going was logged as %d error(s): %v", n, logs.FilterLevelExact(zapcore.ErrorLevel).All())
			}
		})
	}
}

// After the zero-copy loop sent a request again on a new connection (the
// app's first ended with no answer), a client that hangs up before the app
// answers has its app told, and the app's end with no answer is the
// client's going: no error, no 502 for an app that crashed, and the request
// is not sent a third time.
func TestAClientsHangUpBeforeTheReplayedAnswerIsNoAppFailure(t *testing.T) {
	appLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	asked := make(chan struct{})
	accepted := rawApp(t, appLn, func(n int, _ net.Conn, br *bufio.Reader) {
		if n == 1 {
			return // a stale connection: closed with no answer
		}
		if n == 2 {
			close(asked)
		}
		untilEnd(br) // told of its client's going: it ends, unanswered
	})
	_, client, served, logs := serveWatched(t, false, appLn, nil)
	if _, err := client.Write([]byte("GET /client/v1/jobs/j/poll HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-asked:
	case <-never(t):
		t.Fatal("never: the request was sent again on a new connection")
	}
	_ = client.Close()
	select {
	case <-served:
	case <-never(t):
		t.Fatal("never: the handler ended")
	}
	if n := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); n != 0 {
		t.Fatalf("a client's going before the replayed answer was logged as %d error(s): %v", n, logs.FilterLevelExact(zapcore.ErrorLevel).All())
	}
	if n := accepted.Load(); n != 2 {
		t.Fatalf("the app had %d connections, want 2: the request was sent again after its client went", n)
	}
}

// An app that dies mid-answer while its client stays fails the answer's
// forwarding too, but its client did not go: that is never told as an
// exchange its client left, in either loop.
func TestAnAppThatDiesMidAnswerIsNoClientsGoing(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		t.Run(fmt.Sprintf("synchronous %v", synchronous), func(t *testing.T) {
			t.Parallel()
			appLn, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			rawApp(t, appLn, func(_ int, c net.Conn, _ *bufio.Reader) {
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nTransfer-Encoding: chunked\r\n\r\n15\r\n{\"status\":\"RUNNING\"}\n\r\n")
				// it dies here: no last chunk, and its client is still there
			})
			_, client, served, logs := serveWatched(t, synchronous, appLn, nil)
			if _, err := client.Write([]byte("GET /client/v1/jobs/j/events HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, client) // the client stays to the end
			select {
			case <-served:
			case <-never(t):
				t.Fatal("never: the handler ended")
			}
			if n := logs.FilterMessageSnippet("its client closed").Len(); n != 0 {
				t.Fatalf("an app's death mid-answer was told %d time(s) as its client's going: %v", n, logs.All())
			}
		})
	}
}

// hangUps are two ways a client goes mid-answer, each with an app that ends
// its answer once told. A client that closes, with an app that then closes
// the connection mid-stream: the answer's forwarding fails. A client that
// half-closes and reads on, with Go's server, which sends one more event and
// its last chunk: the forwarding ends whole.
var hangUps = []struct {
	name   string
	serve  func(t *testing.T, ln net.Listener, told chan struct{})
	hangUp func(t *testing.T, client net.Conn)
}{
	{"closes, the answer cut short",
		func(t *testing.T, ln net.Listener, told chan struct{}) {
			rawApp(t, ln, func(_ int, c net.Conn, br *bufio.Reader) {
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nTransfer-Encoding: chunked\r\n\r\n15\r\n{\"status\":\"RUNNING\"}\n\r\n")
				untilEnd(br)
				close(told)
			})
		},
		func(t *testing.T, client net.Conn) { hangUpAfterTheFirstEvent(t, client, bufio.NewReader(client)) },
	},
	{"half-closes, the answer ended whole",
		func(t *testing.T, ln net.Listener, told chan struct{}) {
			serveApp(t, ln, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprintln(w, `{"status":"RUNNING"}`)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(told)
				_, _ = fmt.Fprintln(w, `{"status":"CLIENT GONE"}`)
			})
		},
		func(t *testing.T, client net.Conn) {
			br := bufio.NewReader(client)
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					t.Fatalf("the answer's first event: %v", err)
				}
				if strings.Contains(line, "RUNNING") {
					break
				}
			}
			_ = client.(interface{ CloseWrite() error }).CloseWrite()
			if rest, err := io.ReadAll(br); err != nil || !strings.Contains(string(rest), "CLIENT GONE") {
				t.Fatalf("the half-closed client got %q, %v", rest, err)
			}
		},
	},
}

// A sampling-bypass connection (every sampling slot is taken) is never
// captured: when its client hangs up mid-answer the app is told, but the
// exchange was never one to record, so "Not recording this request as a
// test case: its client closed" is not its reason, and is not told; nor is
// the client's going an error.
func TestABypassedExchangeIsNotToldAsNotRecorded(t *testing.T) {
	for _, h := range hangUps {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			appLn, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			told := make(chan struct{})
			h.serve(t, appLn, told)
			cfg := &config.Config{}
			cfg.Agent.MockMode = true
			cfg.Agent.RecordRequests = true
			cfg.Agent.EnableSampling = 1
			core, logs := observer.New(zapcore.InfoLevel)
			pm := New(zap.New(core), nil, cfg)
			pm.samplingSem <- struct{}{} // every sampling slot is taken: this conn bypasses
			client, proxied := tcpPairForWatch(t)
			served := make(chan struct{})
			go func() {
				defer close(served)
				defer func() { _ = proxied.Close() }()
				pm.ServeLive(context.Background(), proxied, appLn.Addr().String(), 8083)
			}()
			if _, err := client.Write([]byte("GET /client/v1/jobs/j/events HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			h.hangUp(t, client)
			select {
			case <-told:
			case <-never(t):
				t.Fatal("never: the bypassed connection's app was not told its client went")
			}
			select {
			case <-served:
			case <-never(t):
				t.Fatal("never: the handler ended")
			}
			if n := logs.FilterMessageSnippet("Not recording this request as a test case").Len(); n != 0 {
				t.Fatalf("a bypassed (never captured) exchange was told %d time(s) as not recorded for its client's going: %v", n, logs.All())
			}
			if n := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); n != 0 {
				t.Fatalf("a client's going was logged as %d error(s): %v", n, logs.FilterLevelExact(zapcore.ErrorLevel).All())
			}
		})
	}
}

// An exchange the memory guard's pause keeps from being recorded is counted
// as lost to the pause, once, not told as not recorded for its client's
// going, in either loop; its app is still told, and the client's going is no
// error. Counted only where the exchange's end is reached, the synchronous
// loop's was neither told nor counted: missing with no trace.
func TestAPausedExchangeIsNotToldAsNotRecorded(t *testing.T) {
	stubIngressPaused(t, func() bool { return true })
	for _, synchronous := range []bool{false, true} {
		for _, h := range hangUps {
			t.Run(fmt.Sprintf("synchronous %v, %s", synchronous, h.name), func(t *testing.T) {
				appLn, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				told := make(chan struct{})
				h.serve(t, appLn, told)
				mgr := syncMock.New(zap.NewNop())
				_, client, served, logs := serveWatchedIn(t, syncMock.NewContext(context.Background(), mgr), synchronous, appLn, nil)
				if _, err := client.Write([]byte("GET /client/v1/jobs/j/events HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
					t.Fatal(err)
				}
				h.hangUp(t, client)
				select {
				case <-told:
				case <-never(t):
					t.Fatal("never: the paused exchange's app was not told its client went")
				}
				select {
				case <-served:
				case <-never(t):
					t.Fatal("never: the handler ended")
				}
				if n := logs.FilterMessageSnippet("Not recording this request as a test case: its client closed").Len(); n != 0 {
					t.Fatalf("an exchange lost to the pause was told %d time(s) as not recorded for its client's going: %v", n, logs.All())
				}
				if n := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); n != 0 {
					t.Fatalf("a client's going was logged as %d error(s): %v", n, logs.FilterLevelExact(zapcore.ErrorLevel).All())
				}
				if got := mgr.NotRecordedUnderPressure(); got.Total != 1 {
					t.Fatalf("an exchange lost to the pause was counted %d time(s) as not recorded under pressure, want 1", got.Total)
				}
			})
		}
	}
}

// What the memory guard's pause costs an exchange is counted once, however
// the exchange ends, in either loop: its end is not the only way out. A pause
// as its request comes, with its client gone mid-answer (each hangUps shape)
// or before the app answered; a pause that begins mid-answer, once the first
// event has reached the client, and cuts the capture of what the app sends
// after (Go's server, to a half-closed client, the forwarding ended whole; or
// an app that sends one more event to a client that closed, then ends
// mid-stream, the forwarding failed): that exchange is counted, and told as
// not recorded for its client's going too, both true. The control: a
// pause that begins mid-answer while the app sends nothing more cuts no
// capture, and costs nothing (told, not counted). The synchronous loop
// counted none of the first three, the zero-copy loop not the cut ones.
func TestAnExchangeAPauseCostIsCountedOnceHoweverItEnds(t *testing.T) {
	var paused atomic.Bool
	stubIngressPaused(t, paused.Load)
	beforeAnswer := hangUps[0]
	beforeAnswer.name = "closes before the app answers"
	beforeAnswer.serve = func(t *testing.T, ln net.Listener, told chan struct{}) {
		rawApp(t, ln, func(_ int, c net.Conn, br *bufio.Reader) {
			untilEnd(br)
			close(told)
		})
	}
	beforeAnswer.hangUp = func(t *testing.T, client net.Conn) { _ = client.Close() }
	// The client closes; the app, told, sends one more event, which a pause
	// begun mid-answer cuts from the capture, then ends the connection
	// mid-stream, with no last chunk: the forwarding fails, and the told
	// return of a failed forward counts the cut, as that of a whole one does.
	cutThenDropped := hangUps[0]
	cutThenDropped.name = "closes, one more event sent and the answer cut short"
	cutThenDropped.serve = func(t *testing.T, ln net.Listener, told chan struct{}) {
		rawApp(t, ln, func(_ int, c net.Conn, br *bufio.Reader) {
			_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nTransfer-Encoding: chunked\r\n\r\n15\r\n{\"status\":\"RUNNING\"}\n\r\n")
			untilEnd(br)
			close(told)
			_, _ = io.WriteString(c, "19\r\n{\"status\":\"CLIENT GONE\"}\n\r\n")
		})
	}
	type end struct {
		name      string
		midAnswer bool // the pause begins once the first event reached the client
		hangUp    int  // into ways
		counted   uint64
		told      int // "Not recording ... its client closed"
	}
	ways := append(append(hangUps[:0:0], hangUps...), beforeAnswer, cutThenDropped)
	ends := []end{
		{"paused as its request comes, " + ways[0].name, false, 0, 1, 0},
		{"paused as its request comes, " + ways[1].name, false, 1, 1, 0},
		{"paused as its request comes, " + ways[2].name, false, 2, 1, 0},
		{"paused mid-answer, the rest's capture cut, " + ways[1].name, true, 1, 1, 1},
		{"paused mid-answer, the rest's capture cut, " + ways[3].name, true, 3, 1, 1},
		{"paused mid-answer, nothing more sent, " + ways[0].name, true, 0, 0, 1},
	}
	for _, synchronous := range []bool{false, true} {
		for _, e := range ends {
			t.Run(fmt.Sprintf("synchronous %v, %s", synchronous, e.name), func(t *testing.T) {
				paused.Store(!e.midAnswer)
				w := ways[e.hangUp]
				appLn, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				told := make(chan struct{})
				w.serve(t, appLn, told)
				mgr := syncMock.New(zap.NewNop())
				_, client, served, logs := serveWatchedIn(t, syncMock.NewContext(context.Background(), mgr), synchronous, appLn, nil)
				if _, err := client.Write([]byte("GET /client/v1/jobs/j/events HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
					t.Fatal(err)
				}
				hangUp := w.hangUp
				if e.midAnswer {
					// The first event is read whole before the pause begins: its
					// capture is done, and the pause cuts only what comes after.
					hangUp = func(t *testing.T, client net.Conn) {
						w.hangUp(t, &pauseAtFirstEvent{Conn: client, paused: &paused})
					}
				}
				hangUp(t, client)
				select {
				case <-told:
				case <-never(t):
					t.Fatal("never: the app was not told its client went")
				}
				select {
				case <-served:
				case <-never(t):
					t.Fatal("never: the handler ended")
				}
				if got := mgr.NotRecordedUnderPressure(); got.Total != e.counted {
					t.Fatalf("counted %d time(s) as not recorded under pressure, want %d: %v", got.Total, e.counted, logs.All())
				}
				if n := logs.FilterMessageSnippet("Not recording this request as a test case: its client closed").Len(); n != e.told {
					t.Fatalf("told %d time(s) as not recorded for its client's going, want %d: %v", n, e.told, logs.All())
				}
				if n := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); n != 0 {
					t.Fatalf("a client's going was logged as %d error(s): %v", n, logs.FilterLevelExact(zapcore.ErrorLevel).All())
				}
			})
		}
	}
}

// pauseAtFirstEvent is a client conn that begins the pause once it has read
// the answer's first event, before it reads on or hangs up.
type pauseAtFirstEvent struct {
	net.Conn
	paused *atomic.Bool
	seen   []byte
}

func (c *pauseAtFirstEvent) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if !c.paused.Load() {
		c.seen = append(c.seen, p[:n]...)
		if strings.Contains(string(c.seen), "RUNNING") {
			c.paused.Store(true)
		}
	}
	return n, err
}

// CloseWrite half-closes the client conn, as the hangUps shape that reads on
// does.
func (c *pauseAtFirstEvent) CloseWrite() error { return c.Conn.(*net.TCPConn).CloseWrite() }

// An exchange the memory guard's pause keeps from being recorded as its
// request comes is counted while it is still in flight, in either loop: the
// recording's end reads the count (routes/record.go, the stream's end
// summary) without waiting for an exchange that opened no window, so a long
// poll or a stream still running then must already be in it. Both loops count
// it before the request reaches the app, so the app having it orders the
// read, with no clock. The synchronous loop counted it only as its handler
// returned.
func TestAPausedInFlightExchangeIsCountedBeforeItEnds(t *testing.T) {
	var paused atomic.Bool
	paused.Store(true)
	stubIngressPaused(t, paused.Load)
	for _, synchronous := range []bool{false, true} {
		t.Run(fmt.Sprintf("synchronous %v", synchronous), func(t *testing.T) {
			appLn, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			got := make(chan struct{})
			release := make(chan struct{})
			serveApp(t, appLn, func(w http.ResponseWriter, r *http.Request) {
				close(got)
				select {
				case <-release:
				case <-r.Context().Done():
				}
				_, _ = io.WriteString(w, "done")
			})
			mgr := syncMock.New(zap.NewNop())
			_, client, served, _ := serveWatchedIn(t, syncMock.NewContext(context.Background(), mgr), synchronous, appLn, nil)
			if _, err := client.Write([]byte("GET /poll HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			select {
			case <-got:
			case <-never(t):
				t.Fatal("never: the app got the request")
			}
			inFlight := mgr.NotRecordedUnderPressure().Total
			close(release)
			resp, err := http.ReadResponse(bufio.NewReader(client), nil)
			if err != nil {
				t.Fatalf("the answer: %v", err)
			}
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			_ = client.Close()
			select {
			case <-served:
			case <-never(t):
				t.Fatal("never: the handler ended")
			}
			if after := mgr.NotRecordedUnderPressure().Total; after != 1 {
				t.Fatalf("counted %d time(s) once the handler ended, want 1", after)
			}
			if inFlight != 1 {
				t.Fatalf("counted %d time(s) while the exchange was in flight, want 1: a summary read then misses it", inFlight)
			}
		})
	}
}

// A pause read as the request comes is counted before the request reaches
// the app, in either loop, with no clock: the request's body is larger than
// the sockets between the proxy and the app hold, so the proxy's forward of
// it cannot end until the app reads it, and the app reads the count before
// it reads the body. A count run once the forward has ended reads 0 here.
func TestAPausedRequestIsCountedBeforeItsBodyReachesTheApp(t *testing.T) {
	var paused atomic.Bool
	paused.Store(true)
	stubIngressPaused(t, paused.Load)
	const bodySize = 64 << 20
	for _, synchronous := range []bool{false, true} {
		t.Run(fmt.Sprintf("synchronous %v", synchronous), func(t *testing.T) {
			appLn, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			mgr := syncMock.New(zap.NewNop())
			var atHeaders atomic.Int64
			atHeaders.Store(-1)
			serveApp(t, appLn, func(w http.ResponseWriter, r *http.Request) {
				atHeaders.Store(int64(mgr.NotRecordedUnderPressure().Total))
				n, _ := io.Copy(io.Discard, r.Body)
				_, _ = fmt.Fprintf(w, "read %d", n)
			})
			_, client, served, _ := serveWatchedIn(t, syncMock.NewContext(context.Background(), mgr), synchronous, appLn, nil)
			sent := make(chan error, 1)
			go func() {
				head := fmt.Sprintf("POST /upload HTTP/1.1\r\nHost: api\r\nContent-Length: %d\r\n\r\n", bodySize)
				if _, err := io.WriteString(client, head); err != nil {
					sent <- err
					return
				}
				_, err := io.Copy(client, io.LimitReader(zeros{}, bodySize))
				sent <- err
			}()
			resp, err := http.ReadResponse(bufio.NewReader(client), nil)
			if err != nil {
				t.Fatalf("the answer: %v", err)
			}
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err := <-sent; err != nil {
				t.Fatalf("the request: %v", err)
			}
			_ = client.Close()
			<-served
			if want := fmt.Sprintf("read %d", bodySize); !strings.Contains(string(b), want) {
				t.Fatalf("the answer %q, want %q", b, want)
			}
			if got := atHeaders.Load(); got != 1 {
				t.Fatalf("counted %d time(s) when the app had the request's headers, want 1", got)
			}
			if after := mgr.NotRecordedUnderPressure().Total; after != 1 {
				t.Fatalf("counted %d time(s) once the handler ended, want 1", after)
			}
		})
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// A pause that begins mid-answer and cuts the capture of the rest is counted
// as its exchange ends, not as its connection does: on a kept-alive
// connection the next exchange starts a count of its own, and a count left
// to the connection's end would be lost, or come after the recording's
// summary was read.
func TestAPauseCutIsCountedAsItsExchangeEndsOnAKeptAliveConnection(t *testing.T) {
	var paused atomic.Bool
	stubIngressPaused(t, paused.Load)
	appLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	step := make(chan struct{})
	serveApp(t, appLn, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/next" {
			_, _ = fmt.Fprint(w, "next")
			return
		}
		_, _ = fmt.Fprintln(w, `{"status":"RUNNING"}`)
		w.(http.Flusher).Flush()
		select {
		case <-step:
		case <-r.Context().Done():
			return
		}
		_, _ = fmt.Fprintln(w, `{"status":"DONE"}`)
	})
	mgr := syncMock.New(zap.NewNop())
	pm, client, served, _ := serveWatchedIn(t, syncMock.NewContext(context.Background(), mgr), false, appLn, nil)
	br := bufio.NewReader(client)
	if _, err := client.Write([]byte("GET /events HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := bufio.NewReader(resp.Body)
	if line, err := body.ReadString('\n'); err != nil || !strings.Contains(line, "RUNNING") {
		t.Fatalf("the answer's first event: %q, %v", line, err)
	}
	paused.Store(true) // the first event is captured; the pause cuts the rest
	close(step)
	if rest, err := io.ReadAll(body); err != nil || !strings.Contains(string(rest), "DONE") {
		t.Fatalf("the rest of the answer: %q, %v", rest, err)
	}
	_ = resp.Body.Close()
	paused.Store(false)
	// The next exchange is read only once the first has ended: its answer
	// here means the first's count is in.
	if _, err := client.Write([]byte("GET /next HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	next, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("the kept-alive connection's next exchange: %v", err)
	}
	if b, err := io.ReadAll(next.Body); err != nil || string(b) != "next" {
		t.Fatalf("the next answer: %q, %v", b, err)
	}
	if got := mgr.NotRecordedUnderPressure(); got.Total != 1 {
		t.Fatalf("the exchange the pause cut was counted %d time(s) by its next exchange's end, want 1", got.Total)
	}
	// The next exchange is captured: its handler, and its capture, end
	// before the test does, so neither reads a hook or a pause the next
	// test swaps.
	_ = client.Close()
	select {
	case <-served:
	case <-never(t):
		t.Fatal("never: the handler ended")
	}
	pm.captures.Wait()
}

// A pause cut on a later exchange of a kept-alive connection is counted too:
// the once-only count is the exchange's, not the connection's. A whole,
// unpaused exchange comes first, then the one the pause cuts, then the next.
func TestAPauseCutOnALaterExchangeOfAKeptAliveConnectionIsCounted(t *testing.T) {
	var paused atomic.Bool
	stubIngressPaused(t, paused.Load)
	appLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	step := make(chan struct{})
	serveApp(t, appLn, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/first", "/next":
			_, _ = fmt.Fprint(w, strings.TrimPrefix(r.URL.Path, "/"))
			return
		}
		_, _ = fmt.Fprintln(w, `{"status":"RUNNING"}`)
		w.(http.Flusher).Flush()
		select {
		case <-step:
		case <-r.Context().Done():
			return
		}
		_, _ = fmt.Fprintln(w, `{"status":"DONE"}`)
	})
	mgr := syncMock.New(zap.NewNop())
	pm, client, served, _ := serveWatchedIn(t, syncMock.NewContext(context.Background(), mgr), false, appLn, nil)
	br := bufio.NewReader(client)
	get := func(path, want string) {
		t.Helper()
		if _, err := client.Write([]byte("GET " + path + " HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if b, err := io.ReadAll(resp.Body); err != nil || string(b) != want {
			t.Fatalf("%s: %q, %v", path, b, err)
		}
	}
	get("/first", "first")
	if _, err := client.Write([]byte("GET /events HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := bufio.NewReader(resp.Body)
	if line, err := body.ReadString('\n'); err != nil || !strings.Contains(line, "RUNNING") {
		t.Fatalf("the answer's first event: %q, %v", line, err)
	}
	paused.Store(true) // the first event is captured; the pause cuts the rest
	close(step)
	if rest, err := io.ReadAll(body); err != nil || !strings.Contains(string(rest), "DONE") {
		t.Fatalf("the rest of the answer: %q, %v", rest, err)
	}
	_ = resp.Body.Close()
	paused.Store(false)
	// The next exchange is read only once the cut one has ended: its answer
	// here means the cut one's count is in.
	get("/next", "next")
	if got := mgr.NotRecordedUnderPressure(); got.Total != 1 {
		t.Fatalf("the later exchange the pause cut was counted %d time(s), want 1", got.Total)
	}
	_ = client.Close()
	select {
	case <-served:
	case <-never(t):
		t.Fatal("never: the handler ended")
	}
	pm.captures.Wait()
}

// A pause cut whose client then hangs up is counted before its exchange's
// window closes, in the zero-copy loop: that close is what a stop's drain
// waits on (Window.Close ends a request the stop found in flight), and the
// recording's summary reads the count once the drain is done. Counted after
// the close, the cut could miss the summary. No clock orders the two: the
// upstream conn's close, which comes after the window's as the handler
// returns, is held until the count has been read where the window's close is
// seen (TestSpanAt's Changed). The synchronous loop dials its own upstream,
// so this row is the zero-copy loop's; its countPause is deferred after win's
// the same way.
func TestAPauseCutIsCountedBeforeItsExchangesWindowCloses(t *testing.T) {
	var paused atomic.Bool
	stubIngressPaused(t, paused.Load)
	appLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	told := make(chan struct{})
	hangUps[1].serve(t, appLn, told) // the app sends on to a half-closed client
	up, err := net.Dial("tcp", appLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseClose := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseClose) // a failed step never leaves the handler held
	upConn := &closeHeldOnceClosed{TCPConn: up.(*net.TCPConn), release: release}
	mgr := syncMock.New(zap.NewNop())
	ctx := syncMock.NewContext(context.Background(), mgr)
	cfg := &config.Config{}
	cfg.Agent.MockMode = true
	cfg.Agent.RecordRequests = true
	core, logs := observer.New(zapcore.InfoLevel)
	pm := New(zap.New(core), nil, cfg)
	client, proxied := tcpPairForWatch(t)
	served := make(chan struct{})
	go func() {
		defer close(served)
		defer func() { _ = proxied.Close() }()
		pm.handleHttp1ZeroCopy(ctx, proxied, upConn, pm.logger, pm.tcChan, 8083, false, nil)
	}()
	if _, err := client.Write([]byte("GET /client/v1/jobs/j/events HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(client)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("the answer's first event: %v", err)
		}
		if strings.Contains(line, "RUNNING") {
			break
		}
	}
	paused.Store(true) // the first event is captured; the pause cuts the rest
	span, ok := mgr.TestSpanAt(time.Now())
	if !ok || span.Final() {
		t.Fatalf("the exchange's window is not open as its answer streams: %+v, %v", span, ok)
	}
	upConn.closed.Store(&span)
	_ = client.(*net.TCPConn).CloseWrite()
	select {
	case <-span.Changed():
	case <-never(t):
		t.Fatal("never: the exchange's window closed")
	}
	counted := mgr.NotRecordedUnderPressure().Total
	releaseClose()
	if rest, err := io.ReadAll(br); err != nil || !strings.Contains(string(rest), "CLIENT GONE") {
		t.Fatalf("the half-closed client got %q, %v", rest, err)
	}
	select {
	case <-told:
	case <-never(t):
		t.Fatal("never: the app was not told its client went")
	}
	select {
	case <-served:
	case <-never(t):
		t.Fatal("never: the handler ended")
	}
	pm.captures.Wait()
	if counted != 1 {
		t.Fatalf("the cut was counted %d time(s) as its exchange's window closed, want 1: a stop's drain could end before it", counted)
	}
	if got := mgr.NotRecordedUnderPressure(); got.Total != 1 {
		t.Fatalf("counted %d time(s) once the handler ended, want 1", got.Total)
	}
	if n := logs.FilterMessageSnippet("Not recording this request as a test case: its client closed").Len(); n != 1 {
		t.Fatalf("told %d time(s) as not recorded for its client's going, want 1: %v", n, logs.All())
	}
}

// closeHeldOnceClosed is an upstream conn whose Close, once the window in
// closed has closed, waits for release first: what the handler does after
// the window's close waits until the test has looked.
type closeHeldOnceClosed struct {
	*net.TCPConn
	closed  atomic.Pointer[syncMock.TestSpan]
	release <-chan struct{}
}

func (c *closeHeldOnceClosed) Close() error {
	if span := c.closed.Load(); span != nil {
		select {
		case <-span.Changed():
			<-c.release
		default:
		}
	}
	return c.TCPConn.Close()
}
