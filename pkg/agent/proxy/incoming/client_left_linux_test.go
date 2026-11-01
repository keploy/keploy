package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
)

// An answer the app wrote whole at once, before its client took the headers
// and went, is the app's answer to its request, and is recorded although
// forwarding the rest of it to the gone client fails. Its end may be found
// before the forward fails (an answer of a few reads) or not (one of many):
// the ingress reads it on, not forwarded, up to what of it the app had sent
// before it could learn its client had gone. The forward failed after the
// end was found and the exchange went with a Debug line, or failed before it
// and the exchange was told as one the client ended. The ingress is held
// before its first body read until the client has gone and the app has been
// told, and before its second until the client's reset of the first is back,
// so the forward fails at its second write.
func TestAnAnswerThatArrivedWholeIsRecordedThoughItsForwardFails(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		for _, size := range []int{8 << 10, 64 << 10} {
			t.Run(fmt.Sprintf("synchronous %v, %d KB", synchronous, size>>10), func(t *testing.T) {
				body := strings.Repeat("w", size)
				appLn, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = appLn.Close() })
				told := make(chan struct{})
				go func() {
					c, err := appLn.Accept()
					if err != nil {
						return
					}
					defer c.Close()
					br := bufio.NewReader(c)
					if _, err := http.ReadRequest(br); err != nil {
						return
					}
					_, _ = fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
					_, _ = io.Copy(io.Discard, br)
					close(told)
				}()

				client, proxied := tcpPairForWatch(t)
				left := make(chan struct{})
				saved := bodyReading
				bodyReading = func(nth int) {
					switch nth {
					case 0:
						for _, c := range []chan struct{}{left, told} {
							select {
							case <-c:
							case <-never(t):
								t.Error("the client did not go, or the app was not told of it")
								return
							}
						}
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
				}
				t.Cleanup(func() { bodyReading = saved })

				cfg := &config.Config{}
				cfg.Agent.MockMode = true
				cfg.Agent.RecordRequests = true
				cfg.Agent.Synchronous = synchronous
				pm := New(zap.NewNop(), nil, cfg)
				mgr := syncMock.New(zap.NewNop())
				mgr.SetFirstRequestSignaled()
				t.Cleanup(mgr.AddWatermark(quietCapture{}))
				ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), mgr))
				served := make(chan struct{})
				go func() {
					defer close(served)
					defer func() { _ = proxied.Close() }()
					pm.ServeLive(ctx, proxied, appLn.Addr().String(), 8083)
				}()
				t.Cleanup(func() {
					cancel()
					_ = client.Close()
					<-served
				})
				if _, err := client.Write([]byte("GET /whoami HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
					t.Fatal(err)
				}
				br := bufio.NewReader(client)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						t.Fatalf("the answer's headers: %v", err)
					}
					if line == "\r\n" {
						break
					}
				}
				_ = client.Close()
				close(left)
				select {
				case <-served:
				case <-never(t):
					t.Fatal("the exchange did not end once its client went")
				}
				pm.captures.Wait()
				if n := len(pm.tcChan); n != 1 {
					t.Fatalf("%d test cases reached the queue from an exchange whose app answered whole before its client went, want 1", n)
				}
				if tc := <-pm.tcChan; tc.HTTPResp.Body != body {
					t.Fatalf("the recorded answer's body is %d bytes, want the app's %d", len(tc.HTTPResp.Body), len(body))
				}
			})
		}
	}
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
