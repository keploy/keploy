package pkg

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// serveEachConn listens on loopback and hands each connection it accepts to
// serve, which owns it.
func serveEachConn(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
			go serve(c)
		}
	}()
	return ln.Addr().String()
}

// readTheRequest reads one HTTP request off c, body and all, so what the
// server does next comes after the request is on the connection. Closing a
// socket with unread bytes in it resets the connection, so a close after this
// is a plain one.
func readTheRequest(c net.Conn) {
	req, err := http.ReadRequest(bufio.NewReader(c))
	if err == nil {
		_, _ = io.Copy(io.Discard, req.Body)
	}
}

// post sends a POST to url on a fresh connection and returns its error; it
// fails the test if an answer came back.
func post(t *testing.T, ctx context.Context, timeout time.Duration, url string) error {
	t.Helper()
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: transport, Timeout: timeout}).Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("POST %s got an answer", url)
	}
	return err
}

// nxdomainResolver is a Go resolver whose every query is answered NXDOMAIN by
// a DNS server on loopback, so a lookup through it fails the way a lookup of a
// name that does not exist does, on every OS and with no network.
func nxdomainResolver(t *testing.T) *net.Resolver {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	srv := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) },
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
			m := new(dns.Msg)
			m.SetRcode(r, dns.RcodeNameError)
			_ = w.WriteMsg(m)
		})}
	go func() { _ = srv.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = srv.Shutdown() })
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", pc.LocalAddr().String())
	}}
}

// A failed test request is classified by its error, never by its text, by
// three rules. Each error here is the real one, provoked.
//
// IsAppConnectionError is the one rule for APP_CONNECTION_ERROR: a test
// request that got no answer at all because the connection to the app failed.
// A drop counts in every shape net/http reports it in (the docker-proxy drop at
// a port no app listens behind is two of them), and so does a gRPC call whose
// connection fails, dropped or answered by something that does not speak
// HTTP/2; a timeout, a cancel and a close part-way through an answer do not.
//
// IsAppNoAnswer is the one rule for replay's no-answer mark: no answer came in
// time, the request was cancelled, or the connection closed part-way through
// the answer, for HTTP and gRPC alike; a drop or a refusal does not.
//
// IsTransportConnReset, the reset re-send's class, differs from
// IsAppConnectionError only for a close part-way through an answer.
//
// An error that only says "connection refused", or "context deadline
// exceeded", in its text is none of them.
func TestAFailedTestRequestIsClassifiedByTheErrorNotItsText(t *testing.T) {
	refusedAddr := func() string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		return addr
	}

	for _, tt := range []struct {
		name string
		err  func(t *testing.T) error
		want bool
		// reset is what IsTransportConnReset says; it differs from want only
		// for a close part-way through an answer.
		reset bool
		// noAnswer is what IsAppNoAnswer says.
		noAnswer bool
	}{
		{"refused", func(t *testing.T) error {
			return post(t, context.Background(), 5*time.Second, "http://"+refusedAddr()+"/x")
		}, true, false, false},
		{"dropped before the request was on the connection (errServerClosedIdle)", func(t *testing.T) error {
			return postToAServerThatClosesFirst(t)
		}, true, true, false},
		{"dropped after the request was read (io.EOF)", func(t *testing.T) error {
			addr := serveEachConn(t, func(c net.Conn) { readTheRequest(c); _ = c.Close() })
			err := post(t, context.Background(), 5*time.Second, "http://"+addr+"/x")
			if !errors.Is(err, io.EOF) {
				t.Fatalf("got %v; want the io.EOF this case provokes", err)
			}
			return err
		}, true, true, false},
		{"reset after the request was read", func(t *testing.T) error {
			addr := serveEachConn(t, func(c net.Conn) {
				readTheRequest(c)
				_ = c.(*net.TCPConn).SetLinger(0)
				_ = c.Close()
			})
			return post(t, context.Background(), 5*time.Second, "http://"+addr+"/x")
		}, true, true, false},
		{"host not found", func(t *testing.T) error {
			transport := &http.Transport{DialContext: (&net.Dialer{Resolver: nxdomainResolver(t)}).DialContext}
			t.Cleanup(transport.CloseIdleConnections)
			resp, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Get("http://app.invalid.:8080/x")
			if err == nil {
				_ = resp.Body.Close()
				t.Fatal("a request to a name that does not exist got an answer")
			}
			var dnsErr *net.DNSError
			if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
				t.Fatalf("got %v; want the not-found *net.DNSError this case provokes", err)
			}
			return err
		}, true, false, false},
		{"gRPC call whose connection is dropped", func(t *testing.T) error {
			addr := serveEachConn(t, func(c net.Conn) { _ = c.Close() })
			_, err := SimulateGRPC(context.Background(), grpcTestCase(addr, []models.GrpcLengthPrefixedMessage{{}}),
				"set", zap.NewNop(), SimulationConfig{APITimeout: 5})
			if err == nil {
				t.Fatal("a gRPC call to a server that drops every connection succeeded")
			}
			return err
		}, true, false, false},
		{"gRPC call at a port an HTTP/1 server answers on", func(t *testing.T) error {
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			t.Cleanup(srv.Close)
			_, err := SimulateGRPC(context.Background(), grpcTestCase(srv.Listener.Addr().String(), []models.GrpcLengthPrefixedMessage{{}}),
				"set", zap.NewNop(), SimulationConfig{APITimeout: 5})
			if err == nil {
				t.Fatal("a gRPC call to an HTTP/1 server succeeded")
			}
			return err
		}, true, false, false},
		{"no answer in time", func(t *testing.T) error {
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			addr := serveEachConn(t, func(c net.Conn) { readTheRequest(c); <-release; _ = c.Close() })
			return post(t, context.Background(), 50*time.Millisecond, "http://"+addr+"/x")
		}, false, false, true},
		{"the body did not come in time", func(t *testing.T) error {
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			addr := serveEachConn(t, func(c net.Conn) {
				readTheRequest(c)
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\n{")
				<-release
				_ = c.Close()
			})
			transport := &http.Transport{}
			t.Cleanup(transport.CloseIdleConnections)
			resp, err := (&http.Client{Transport: transport, Timeout: 50 * time.Millisecond}).Get("http://" + addr + "/x")
			if err != nil {
				t.Fatalf("the answer's headers did not come: %v", err)
			}
			defer resp.Body.Close()
			// What SimulateHTTP does with the body.
			_, err = io.ReadAll(resp.Body)
			if err == nil {
				t.Fatal("a body that never came was read whole")
			}
			return err
		}, false, false, true},
		{"cancelled", func(t *testing.T) error {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return post(t, ctx, 5*time.Second, "http://"+refusedAddr()+"/x")
		}, false, false, true},
		{"gRPC call with no answer in time", func(t *testing.T) error {
			err := grpcCallToASilentServer(t, context.Background())
			// grpc-go's status for its call context's deadline, with no cause:
			// only the code says what it is.
			if status.Code(err) != codes.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("got %v; want the DeadlineExceeded status this case provokes", err)
			}
			return err
		}, false, false, true},
		{"gRPC call cancelled", func(t *testing.T) error {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := grpcCallToASilentServer(t, ctx)
			if status.Code(err) != codes.Canceled || errors.Is(err, context.Canceled) {
				t.Fatalf("got %v; want the Canceled status this case provokes", err)
			}
			return err
		}, false, false, true},
		{"closed part-way through the answer", func(t *testing.T) error {
			addr := serveEachConn(t, func(c net.Conn) {
				readTheRequest(c)
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: text/pl")
				_ = c.Close()
			})
			err := post(t, context.Background(), 5*time.Second, "http://"+addr+"/x")
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("got %v; want the io.ErrUnexpectedEOF this case provokes", err)
			}
			return err
		}, false, true, true},
		{"only the text of a refusal", func(*testing.T) error {
			return errors.New(`Post "http://localhost:8080/x": dial tcp [::1]:8080: connect: connection refused`)
		}, false, false, false},
		{"only the text of a timeout", func(*testing.T) error {
			return errors.New(`Post "http://localhost:8080/x": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`)
		}, false, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.err(t)
			if got := IsAppConnectionError(err); got != tt.want {
				t.Errorf("IsAppConnectionError(%v) = %v, want %v", err, got, tt.want)
			}
			if got := IsTransportConnReset(err); got != tt.reset {
				t.Errorf("IsTransportConnReset(%v) = %v, want %v", err, got, tt.reset)
			}
			if got := IsAppNoAnswer(err); got != tt.noAnswer {
				t.Errorf("IsAppNoAnswer(%v) = %v, want %v", err, got, tt.noAnswer)
			}
		})
	}
}

// grpcCallToASilentServer replays a gRPC call through SimulateGRPC to a server
// that accepts the connection and sends nothing, not even HTTP/2's preface, and
// returns the call's error. The connection never becomes ready, so the call
// waits in NewStream until its context ends: at once for a ctx already
// cancelled (a non-blocking grpc.DialContext does not read the context, so
// NewStream is where the cancel is found), else at the call's own deadline
// (APITimeout, 1 s here). Either way grpc-go returns its status for the end of
// the context, with no cause. A stream that did open would end on its context
// in the response's grpc-status, not in an error: grpc-go's Header does not
// return one.
func grpcCallToASilentServer(t *testing.T, ctx context.Context) error {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	addr := serveEachConn(t, func(c net.Conn) { <-release; _ = c.Close() })
	_, err := SimulateGRPC(ctx, grpcTestCase(addr, []models.GrpcLengthPrefixedMessage{{}}),
		"set", zap.NewNop(), SimulationConfig{APITimeout: 1})
	if err == nil {
		t.Fatal("a gRPC call to a server that sends nothing got an answer")
	}
	return err
}

// A drop is classified wherever it sits in the error tree, as errors.Is finds
// io.EOF: behind errors.Join and a fmt.Errorf with two %w, as well as behind
// one Unwrap.
func TestADropIsClassifiedAnywhereInTheErrorTree(t *testing.T) {
	closedIdle := postToAServerThatClosesFirst(t)
	other := errors.New("the app's port 8097 cannot be reached from the host")
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"errors.Join", errors.Join(other, closedIdle)},
		{"two %w", fmt.Errorf("%w: %w", other, closedIdle)},
		{"%w inside errors.Join", errors.Join(other, fmt.Errorf("replay: %w", closedIdle))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if !IsTransportConnReset(tt.err) {
				t.Errorf("IsTransportConnReset(%v) = false", tt.err)
			}
			if !IsAppConnectionError(tt.err) {
				t.Errorf("IsAppConnectionError(%v) = false", tt.err)
			}
		})
	}
	if joined := errors.Join(other, errors.New("timeout")); IsTransportConnReset(joined) || IsAppConnectionError(joined) {
		t.Errorf("a tree with no drop in it is classified as one: %v", joined)
	}
}
