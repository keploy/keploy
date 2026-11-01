package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
)

// An answer the app wrote whole at once, before its client took the headers
// and went, is the app's answer to its request, and is recorded although
// forwarding the rest of it to the gone client fails. Its end may be found
// before the forward fails (an answer of a few reads) or not (one of many):
// then the ingress reads it on, not forwarded, up to what of it the app had
// sent before it could learn its client had gone, and finds it there. Either
// way the exchange is recorded, and not told as one its client ended. The
// client goes once the whole answer has reached the ingress: in its socket
// (the app's side has acknowledged it), and in the synchronous loop read by
// its read-ahead. The ingress is held before its first body read until the
// client has gone and the app has been told, and before its second until the
// client's reset of the first is back, so the forward fails at its second
// write.
//
// Two more ways the answer is with the app's side as the client goes. In one
// the app wrote it whole but much of it waits in the app's own send queue,
// held there by keploy's receive window (the zero-copy loop reads the app
// only as it forwards; the read-ahead stops at its bound, lowered here): no
// count of keploy's sees those bytes, and before the drain the exchange was
// told as ended by its client. The watch drains the app's side before it
// tells the app (drainConn), so the whole answer is read. In the other the
// client resets its connection: the watch closes the app's connection (an
// app is told of a reset by a close), and what its socket held unread would
// be lost with it, but the drain has read it first.
func TestAnAnswerThatArrivedWholeIsRecordedThoughItsForwardFails(t *testing.T) {
	type answerCase struct {
		size int
		// queued: the app's send queue still holds part of the answer as
		// the client goes. reset: the client resets its connection.
		queued, reset bool
	}
	cases := []answerCase{{size: 8 << 10}, {size: 64 << 10}, {size: 512 << 10, queued: true}, {size: 64 << 10, reset: true}, {size: 512 << 10, queued: true, reset: true}}
	for _, synchronous := range []bool{false, true} {
		for _, tc := range cases {
			size := tc.size
			t.Run(fmt.Sprintf("synchronous %v, %d KB, queued %v, reset %v", synchronous, size>>10, tc.queued, tc.reset), func(t *testing.T) {
				if tc.queued {
					saved := aheadMax
					aheadMax = 16 << 10
					t.Cleanup(func() { aheadMax = saved })
				}
				body := strings.Repeat("w", size)
				answer := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
				appLn := listenForApp(t)
				sent, told := make(chan struct{}), make(chan struct{})
				var unsent atomic.Int64
				go func() {
					c, err := appLn.Accept()
					if err != nil {
						return
					}
					defer func() { _ = c.Close() }()
					br := bufio.NewReader(c)
					if _, err := http.ReadRequest(br); err != nil {
						return
					}
					_, _ = io.WriteString(c, answer)
					if tc.queued {
						unsent.Store(int64(unsentBy(t, c)))
					} else {
						waitAcked(t, c)
					}
					close(sent)
					_, _ = io.Copy(io.Discard, br)
					close(told)
				}()
				readAll := make(chan struct{})
				if tc.queued {
					close(readAll)
				} else {
					readAll = readAhead(t, synchronous, len(answer))
				}

				left := make(chan struct{})
				var proxied net.Conn
				holdBodyReads(t, func(nth int) {
					switch nth {
					case 0:
						waitFor(t, "the client did not go, or the app was not told of it", left, told)
					case 1:
						// The first write reached a closed client: its reset
						// comes back, and the next write fails on it.
						for tcpState(t, proxied) != tcpClose {
							select {
							case <-never(t):
								t.Error("the client's reset never came back")
								return
							default:
								runtime.Gosched()
							}
						}
					}
				})
				pm, client, served, logs := serveOneGET(t, synchronous, appLn, &proxied)
				readHeaders(t, bufio.NewReader(client))
				waitFor(t, "the app's answer did not reach the ingress", sent, readAll)
				if tc.queued && unsent.Load() == 0 {
					t.Fatal("the app's send queue held none of its answer as its client went: this case tests nothing")
				}
				if tc.reset {
					if err := client.(*net.TCPConn).SetLinger(0); err != nil {
						t.Fatal(err)
					}
				}
				_ = client.Close()
				close(left)
				tcs, toldLeft := queuedOnceGone(t, pm, served, logs)
				if len(tcs) != 1 || toldLeft != 0 {
					t.Fatalf("%d test cases reached the queue from an exchange whose app answered whole before its client went, and %d were told as ended by their client; want 1 and 0", len(tcs), toldLeft)
				}
				if got := tcs[0].HTTPResp.Body; got != body {
					t.Fatalf("the recorded answer's body is %d bytes, want the app's %d", len(got), len(body))
				}
			})
		}
	}
}

// A close-delimited answer (Connection: close, no length) ends where the app
// closes its connection, and that close is a part of what arrives from the
// app, one more than its bytes. Its client took what had come, the headers
// and the body's first bytes, and went; the app, a stream that ends only once
// told its client has gone, closed then. The answer ended on the going, not on
// the request: not recorded, and told. Were the close counted as nothing, or
// the end taken a byte early, the end would fall within what had arrived when
// the client went, and the exchange would be recorded with an answer to the
// going.
func TestACloseDelimitedAnswerTheAppEndsOnlyOnceToldIsNotRecorded(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		t.Run(fmt.Sprintf("synchronous %v", synchronous), func(t *testing.T) {
			const first = "the first event of a stream\n"
			appLn := listenForApp(t)
			told := make(chan struct{})
			go func() {
				c, err := appLn.Accept()
				if err != nil {
					return
				}
				defer func() { _ = c.Close() }()
				br := bufio.NewReader(c)
				if _, err := http.ReadRequest(br); err != nil {
					return
				}
				_, _ = fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\n%s", first)
				_, _ = io.Copy(io.Discard, br)
				close(told)
			}()

			pm, client, served, logs := serveOneGET(t, synchronous, appLn, nil)
			resp, err := http.ReadResponse(bufio.NewReader(client), nil)
			if err != nil {
				t.Fatalf("the answer's headers: %v", err)
			}
			if resp.ContentLength >= 0 || len(resp.TransferEncoding) > 0 {
				t.Fatalf("the answer is not close-delimited: length %d, transfer encoding %v", resp.ContentLength, resp.TransferEncoding)
			}
			got := make([]byte, len(first))
			if _, err := io.ReadFull(resp.Body, got); err != nil || string(got) != first {
				t.Fatalf("the body's first bytes: %q, %v", got, err)
			}
			// All it was sent is read: its close is a FIN, and the app is
			// told by a half-close.
			_ = client.Close()
			waitFor(t, "the app was not told its client went", told)
			tcs, toldLeft := queuedOnceGone(t, pm, served, logs)
			if len(tcs) != 0 || toldLeft != 1 {
				t.Fatalf("%d test cases reached the queue from an exchange whose app ended its answer only once told its client had gone, and %d were told as ended by their client; want 0 and 1", len(tcs), toldLeft)
			}
		})
	}
}

// A close-delimited answer the app wrote whole and then closed, before its
// client took the headers and went, is the app's whole answer to the
// request: its close had arrived, a part of what had come from the app when
// the client went, so the client did not end it. Recorded. The synchronous
// loop reads the app ahead of its client, and has read the close by then.
// The zero-copy loop reads the app only as it forwards: the close is in its
// socket, unread, and is counted all the same (TCP_INFO counts the close the
// socket has received; SIOCINQ counted bytes only, and such an answer was
// not recorded, and was told).
func TestACloseDelimitedAnswerWhoseCloseCameBeforeItsClientWentIsRecorded(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		t.Run(fmt.Sprintf("synchronous %v", synchronous), func(t *testing.T) {
			const body = "the whole answer"
			appLn := listenForApp(t)
			acked, told := make(chan struct{}), make(chan struct{})
			go func() {
				c, err := appLn.Accept()
				if err != nil {
					return
				}
				defer func() { _ = c.Close() }()
				br := bufio.NewReader(c)
				if _, err := http.ReadRequest(br); err != nil {
					return
				}
				_, _ = fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\n%s", body)
				_ = c.(*net.TCPConn).CloseWrite() // the answer's end
				waitAcked(t, c)                   // its close is in the ingress's socket
				close(acked)
				_, _ = io.Copy(io.Discard, br)
				close(told)
			}()
			closeRead := make(chan struct{})
			if synchronous {
				var once sync.Once
				seen := func(_ int, err error) {
					if errors.Is(err, io.EOF) {
						once.Do(func() { close(closeRead) })
					}
				}
				aheadReadSeen.Store(&seen)
				t.Cleanup(func() { aheadReadSeen.Store(nil) })
			} else {
				close(closeRead)
			}
			holdBodyReads(t, func(nth int) {
				if nth == 0 {
					waitFor(t, "the client did not go, or the app was not told of it", told)
				}
			})

			pm, client, served, logs := serveOneGET(t, synchronous, appLn, nil)
			readHeaders(t, bufio.NewReader(client))
			waitFor(t, "the app's close never reached the ingress", acked, closeRead)
			_ = client.Close()
			tcs, toldLeft := queuedOnceGone(t, pm, served, logs)
			if len(tcs) != 1 || toldLeft != 0 {
				t.Fatalf("%d test cases reached the queue from an exchange whose close-delimited answer ended before its client went, and %d were told as ended by their client; want 1 and 0", len(tcs), toldLeft)
			}
			if got := tcs[0].HTTPResp.Body; got != body {
				t.Fatalf("the recorded answer's body is %q, want %q", got, body)
			}
		})
	}
}

// Where an answer ends in what arrives from the app is what the ingress has
// read of it less what it read past the end and holds unread: what the app
// sent after its answer does not move the answer's end. The app writes an
// answer of a known length whole, at once: it had arrived when its client
// took the headers and went. Told of the going, the app sends more, and the
// ingress's read of the answer's last bytes (a fill of its reader's buffer)
// brings that too. The answer is the app's to the request: recorded. The
// zero-copy loop has read only the first fill of the answer by the going; the
// rest of it is in the app connection's socket, and has arrived too.
func TestWhatTheAppSentPastItsAnswerDoesNotMoveTheAnswersEnd(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		t.Run(fmt.Sprintf("synchronous %v", synchronous), func(t *testing.T) {
			// The answer is the ingress reader's buffer (bufio's 4096
			// bytes) and 100 more: the fill that brings the headers leaves
			// the last 100 unread, and their read is a fill of its own.
			const head = "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n"
			size := 4096 + 100 - len(fmt.Sprintf(head, 4000))
			if len(fmt.Sprintf(head, size))+size != 4096+100 {
				t.Fatal("the answer is not the reader's buffer and 100 bytes")
			}
			body := strings.Repeat("w", size)
			const more = "what the app sent once told its client had gone"
			answer := fmt.Sprintf(head, size) + body
			appLn := listenForApp(t)
			sent, told := make(chan struct{}), make(chan struct{})
			go func() {
				c, err := appLn.Accept()
				if err != nil {
					return
				}
				defer func() { _ = c.Close() }()
				br := bufio.NewReader(c)
				if _, err := http.ReadRequest(br); err != nil {
					return
				}
				_, _ = io.WriteString(c, answer)
				waitAcked(t, c)
				close(sent)
				_, _ = io.Copy(io.Discard, br)
				_, _ = io.WriteString(c, more)
				waitAcked(t, c)
				close(told)
			}()
			readAll := readAhead(t, synchronous, len(answer))
			moreRead := readAhead(t, synchronous, len(answer)+len(more))
			holdBodyReads(t, func(nth int) {
				if nth == 0 {
					waitFor(t, "the client did not go, or the app was not told of it, or what it sent then did not reach the ingress", told, moreRead)
				}
			})

			pm, client, served, logs := serveOneGET(t, synchronous, appLn, nil)
			readHeaders(t, bufio.NewReader(client))
			waitFor(t, "the app's answer did not reach the ingress", sent, readAll)
			_ = client.Close()
			tcs, toldLeft := queuedOnceGone(t, pm, served, logs)
			if len(tcs) != 1 || toldLeft != 0 {
				t.Fatalf("%d test cases reached the queue from an exchange whose app answered whole before its client went and sent more after, and %d were told as ended by their client; want 1 and 0", len(tcs), toldLeft)
			}
			if got := tcs[0].HTTPResp.Body; got != body {
				t.Fatalf("the recorded answer's body is %d bytes, want the app's %d", len(got), len(body))
			}
		})
	}
}

// listenForApp is a listener for a test's app, closed as the test ends.
func listenForApp(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// holdBodyReads runs hold before each read of an answer's body (bodyReading)
// until the test ends.
func holdBodyReads(t *testing.T, hold func(nth int)) {
	saved := bodyReading
	bodyReading = hold
	t.Cleanup(func() { bodyReading = saved })
}

// waitFor waits for each of chans to close, and fails the test with what if
// one does not by the test binary's deadline.
func waitFor(t *testing.T, what string, chans ...chan struct{}) {
	for _, c := range chans {
		select {
		case <-c:
		case <-never(t):
			t.Error(what)
			return
		}
	}
}

// serveOneGET serves a GET from a new client to the app on appLn, as
// serveWatched does, under a recording that waits on no capture, and sends
// the request. proxied, if not nil, is set to the ingress's side of the
// client's connection. The exchange's handler has returned by the test's end.
func serveOneGET(t *testing.T, synchronous bool, appLn net.Listener, proxied *net.Conn) (pm *IngressProxyManager, client net.Conn, served <-chan struct{}, logs *observer.ObservedLogs) {
	t.Helper()
	mgr := syncMock.New(zap.NewNop())
	mgr.SetFirstRequestSignaled()
	t.Cleanup(mgr.AddWatermark(quietCapture{}))
	ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), mgr))
	wrap := func(c net.Conn) net.Conn {
		if proxied != nil {
			*proxied = c
		}
		return c
	}
	pm, client, served, logs = serveWatchedIn(t, ctx, synchronous, appLn, wrap)
	t.Cleanup(func() {
		cancel()
		_ = client.Close()
		<-served
	})
	if _, err := client.Write([]byte("GET /whoami HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	return pm, client, served, logs
}

// readHeaders reads an answer's headers from br, and nothing past them.
func readHeaders(t *testing.T, br *bufio.Reader) {
	t.Helper()
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("the answer's headers: %v", err)
		}
		if line == "\r\n" {
			return
		}
	}
}

// queuedOnceGone waits for the exchange's handler to return, its client
// gone, and its capture to end, and returns the test cases it queued and how
// many exchanges were told at Info as not recorded because their client
// ended them.
func queuedOnceGone(t *testing.T, pm *IngressProxyManager, served <-chan struct{}, logs *observer.ObservedLogs) (tcs []*models.TestCase, toldLeft int) {
	t.Helper()
	select {
	case <-served:
	case <-never(t):
		t.Fatal("the exchange did not end once its client went")
	}
	pm.captures.Wait()
	for len(pm.tcChan) > 0 {
		tcs = append(tcs, <-pm.tcChan)
	}
	return tcs, logs.FilterMessageSnippet("Not recording this request as a test case: its client closed its side").Len()
}

// waitAcked waits until what the app has written on conn is acknowledged by
// the ingress's side (SIOCOUTQ is 0): in its socket, if not read.
func waitAcked(t *testing.T, conn net.Conn) {
	for unsentOrUnacked(t, conn) > 0 {
		select {
		case <-never(t):
			t.Error("what the app wrote was never acknowledged")
			return
		default:
			runtime.Gosched()
		}
	}
}

// readAhead closes once the synchronous loop's read-ahead has read n bytes
// from the app (aheadReadSeen); at once for the zero-copy loop, which reads
// the app only as it forwards. Its hook passes each read on to the one it
// was installed over, and counts from when it was installed: install it
// before the app's first byte.
func readAhead(t *testing.T, synchronous bool, n int) chan struct{} {
	done := make(chan struct{})
	if !synchronous {
		close(done)
		return done
	}
	prev := aheadReadSeen.Load()
	var total atomic.Int64
	var once sync.Once
	seen := func(got int, err error) {
		if prev != nil {
			(*prev)(got, err)
		}
		if total.Add(int64(got)) >= int64(n) {
			once.Do(func() { close(done) })
		}
	}
	aheadReadSeen.Store(&seen)
	t.Cleanup(func() { aheadReadSeen.Store(prev) })
	return done
}

// unsentOrUnacked is what conn has sent that its peer has not acknowledged
// (SIOCOUTQ).
func unsentOrUnacked(t *testing.T, conn net.Conn) int {
	rc, err := conn.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Error(err)
		return 0
	}
	var n int
	var ierr error
	if err := rc.Control(func(fd uintptr) { n, ierr = unix.IoctlGetInt(int(fd), unix.SIOCOUTQ) }); err != nil || ierr != nil {
		t.Errorf("SIOCOUTQ: %v %v", err, ierr)
		return 0
	}
	return n
}

// unsentBy is what conn holds in its send queue that it has not sent yet
// (SIOCOUTQNSD).
func unsentBy(t *testing.T, conn net.Conn) int {
	rc, err := conn.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Error(err)
		return 0
	}
	var n int
	var ierr error
	if err := rc.Control(func(fd uintptr) { n, ierr = unix.IoctlGetInt(int(fd), unix.SIOCOUTQNSD) }); err != nil || ierr != nil {
		t.Errorf("SIOCOUTQNSD: %v %v", err, ierr)
		return 0
	}
	return n
}

// tcpClose is TCP_CLOSE, the state a socket is in once reset.
const tcpClose = 7

// tcpState is the TCP state of conn's socket (TCP_INFO).
func tcpState(t *testing.T, conn net.Conn) uint8 {
	t.Helper()
	rc, err := conn.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var info *unix.TCPInfo
	var ierr error
	if err := rc.Control(func(fd uintptr) {
		info, ierr = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
	}); err != nil || ierr != nil {
		t.Fatalf("TCP_INFO: %v %v", err, ierr)
	}
	return info.State
}

// toldConn is the app's side of an exchange, as the watch tells it: it
// remembers being told (a half-close or a close), and nothing else.
type toldConn struct {
	net.Conn
	told atomic.Bool
}

func (c *toldConn) CloseWrite() error { c.told.Store(true); return nil }
func (c *toldConn) Close() error      { c.told.Store(true); return c.Conn.Close() }

// The watch reads what has arrived from the app as it sees the client go,
// and only then tells the app: all it counts, the app sent before it could
// learn of the going. Read after the telling, it could count the end the app
// sent on learning its client had gone (a stream it then closed, the rest of
// an answer it then cut short), and the exchange would be recorded with an
// answer to the going. Here the count fails the test if the app has been told
// by the time it is read: a FIN and a reset, each told its own way.
func TestTheWatchReadsWhatArrivedBeforeItTellsTheApp(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprintf("reset %v", reset), func(t *testing.T) {
			client, proxied := tcpPairForWatch(t)
			appEnd, other := net.Pipe()
			t.Cleanup(func() { _ = appEnd.Close(); _ = other.Close() })
			app := &toldConn{Conn: appEnd}
			read := make(chan struct{})
			var once atomic.Bool
			arrived := func() int64 {
				if app.told.Load() {
					t.Error("what had arrived from the app was read after the app was told of its client's going")
				}
				if once.CompareAndSwap(false, true) {
					close(read)
				}
				return 0
			}
			w := watchClient(zap.NewNop(), proxied, bufio.NewReader(proxied), func() net.Conn { return app }, arrived)
			if w == nil {
				t.Fatal("the client is not watched")
			}
			if reset {
				if err := client.(*net.TCPConn).SetLinger(0); err != nil {
					t.Fatal(err)
				}
			}
			_ = client.Close()
			select {
			case <-read:
			case <-never(t):
				t.Fatal("the watch did not see the client go")
			}
			if !w.endedBefore(-1) {
				t.Fatal("the client's going before the answer's end did not end the exchange")
			}
			if !app.told.Load() {
				t.Fatal("the app was not told of its client's going")
			}
		})
	}
}
