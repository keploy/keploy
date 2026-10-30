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
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/config"
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
				fmt.Fprintln(w, `{"status":"RUNNING"}`)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(gone)
				fmt.Fprintln(w, `{"status":"CLIENT GONE"}`)
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
				defer proxied.Close() // as the caller does once the handler returns
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
	defer ln.Close()
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
		stop := watchClient(logger, c, bufio.NewReader(c), func() net.Conn { return nil })
		if stop() {
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
			stop := watchClient(zap.NewNop(), c, bufio.NewReader(c), func() net.Conn { return appSide })
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
			if ended := stop(); ended != leaves {
				t.Fatalf("stop reported an end %v, want %v", ended, leaves)
			}
			_ = stop()
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
		defer proxied.Close() // as the caller does once the handler returns
		pm.ServeLive(context.Background(), handed, appLn.Addr().String(), 8083)
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
		fmt.Fprintln(w, `{"status":"RUNNING"}`)
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
// closed then); the rest are accepted as they come.
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
			defer close(l.dropped)
			br := bufio.NewReader(c)
			for {
				if line, err := br.ReadString('\n'); err != nil || line == "\r\n" {
					break
				}
			}
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
