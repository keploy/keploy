package pkg

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// closingListener accepts each connection and closes it at once, as docker's
// proxy does at a published port behind which the app listens only on
// 127.0.0.1 in its container.
func closingListener(t *testing.T) net.Listener {
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
			_ = c.Close()
		}
	}()
	return ln
}

// ownCloseErr is what net/http's transport returns when the write of a
// request's body is cut short by its own close of the connection, once its
// read of the connection failed: the write's error, net.ErrClosed, with
// nothing of why. client.Do returns it in a *url.Error.
func ownCloseErr() error {
	return &net.OpError{Op: "readfrom", Net: "tcp",
		Err: &net.OpError{Op: "write", Net: "tcp", Err: net.ErrClosed}}
}

// endedBy dials ln through ended and reads the connection until it fails, as
// net/http's read loop does: ended then holds the app's end of it.
func endedBy(t *testing.T, ln net.Listener) *peerEnd {
	t.Helper()
	ended := &peerEnd{}
	c, err := ended.dial(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.Copy(io.Discard, c); err != nil && !isConnDropped(err) {
		t.Fatalf("reading a connection the app closed: %v", err)
	}
	if ended.ended() == nil {
		t.Fatal("the app's end of the connection was not noted")
	}
	return ended
}

// One drop, two reports: net/http returns its read's end of the connection or
// the write its own close of the connection cut short, by a race inside it.
// When it returns the write, the app's end is added to it, and the request
// fails as the drop it is: a reset re-send may follow (IsTransportConnReset),
// the test is APP_CONNECTION_ERROR, and an address the app can never be
// reached at is asked about and named. net/http's own error stays in the
// chain, outermost its *url.Error.
func TestADropNetHTTPReportsAsItsOwnCloseIsADrop(t *testing.T) {
	ln := closingListener(t)
	u := "http://" + ln.Addr().String() + "/echo"

	ended := endedBy(t, ln)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, u, strings.NewReader("{}"))
	rt := &scriptedRT{errs: []error{ownCloseErr()}}
	_, err := doRequestWithConnRefusedRetry(context.Background(), zap.NewNop(), &http.Client{Transport: rt}, req, nil, 0, ended)
	if !IsTransportConnReset(err) || !IsAppConnectionError(err) {
		t.Fatalf("a drop net/http reported as its own close is not a drop: %v", err)
	}
	if !errors.Is(err, net.ErrClosed) {
		t.Errorf("net/http's own error is lost from the chain: %v", err)
	}
	var ue *url.Error
	if !errors.As(err, &ue) || ue != err {
		t.Errorf("the *url.Error is not outermost: %T %v", err, err)
	}
	if want := "use of closed network connection (the app had ended the connection: "; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not say %q", err, want)
	}
	if got := rt.attempts; got != 1 {
		t.Errorf("a drop was sent again: %d attempts", got)
	}

	// At an address the app can never be reached at, the drop is asked
	// about and named, as a reset or io.EOF is.
	rt = &scriptedRT{errs: []error{ownCloseErr()}}
	reach := &answerReach{reason: "the app listens on port 8097 only on 127.0.0.1 inside the container"}
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodPost, u, strings.NewReader("{}"))
	_, err = doRequestWithConnRefusedRetry(context.Background(), zap.NewNop(), &http.Client{Transport: rt}, req, reach, 0, ended)
	if !IsUnreachableAppPort(err) || !IsTransportConnReset(err) {
		t.Fatalf("the drop at an unreachable port is not named: %v", err)
	}
	if len(reach.asked) != 1 || reach.asked[0] != ln.Addr().String() {
		t.Errorf("asked %v, want [%s]", reach.asked, ln.Addr())
	}
}

// net/http closes a connection itself after any failed read, an answer it
// cannot parse among them, and its own close is the app's drop only when the
// app ended the connection first. Without that, or for another error, the
// error is returned as it was.
func TestAnOwnCloseIsADropOnlyAfterTheAppsEnd(t *testing.T) {
	u := "http://127.0.0.1:1/echo"
	for _, tc := range []struct {
		name  string
		ended *peerEnd
		err   error
	}{
		{"no end known", nil, &url.Error{Op: "Post", URL: u, Err: ownCloseErr()}},
		{"the connections read no end", &peerEnd{}, &url.Error{Op: "Post", URL: u, Err: ownCloseErr()}},
		{"not net/http's own close", &peerEnd{err: io.EOF}, &url.Error{Op: "Post", URL: u, Err: errors.New("net/http: HTTP/1.x transport connection broken: malformed HTTP response")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ended.explain(tc.err); got != tc.err {
				t.Fatalf("explain changed %v into %v", tc.err, got)
			}
		})
	}
	if IsAppConnectionError(&url.Error{Op: "Post", URL: u, Err: ownCloseErr()}) {
		t.Error("net/http's own close alone reads as the app's drop")
	}
	// A drop already said as one is not said twice.
	reset := connResetErr(u)
	if got := (&peerEnd{err: io.EOF}).explain(reset); got != reset {
		t.Errorf("a reset was explained again: %v", got)
	}
}

// Only the app's end of a connection is noted: not a read that returned
// bytes, nor one past a deadline, nor one of a connection keploy (or
// net/http) closed itself; and only the first end.
func TestPeerEndNotesOnlyTheAppsEnd(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	served := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			served <- c
		}
	}()
	ended := &peerEnd{}
	c, err := ended.dial(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	app := <-served
	if _, err := app.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	if got := ended.ended(); got != nil {
		t.Fatalf("a read that returned bytes was noted: %v", got)
	}
	if err := c.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(buf); !isTimedOut(err) {
		t.Fatalf("read past its deadline: %v", err)
	}
	if got := ended.ended(); got != nil {
		t.Fatalf("a read past its deadline was noted as the app's end: %v", got)
	}
	_ = c.Close()
	if _, err := c.Read(buf); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("read of a closed connection: %v", err)
	}
	if got := ended.ended(); got != nil {
		t.Fatalf("keploy's own close was noted as the app's end: %v", got)
	}
	_ = app.Close()

	ended.note(io.EOF)
	ended.note(io.ErrUnexpectedEOF)
	if got := ended.ended(); got != io.EOF {
		t.Errorf("the first end is %v, want io.EOF", got)
	}
}

// End to end: a request with a body to a port that drops every connection
// fails as a drop every time, however net/http reports it. Under the proven
// switch (its answer watch counts the body, so net/http sends the body in a
// write of its own) a two-byte body was reported as net/http's own close in
// about 1 run in 25, and with the switch off a body too big for one write in
// most runs: replay's TestADroppedConnectionAtAnUnreachableAppPortSaysSo
// failed 2 in 30 once the proven model was Linux's default.
func TestARequestToAPortThatDropsItFailsAsADropEveryTime(t *testing.T) {
	for _, tc := range []struct {
		name, proven string
		body         string
		cfg          SimulationConfig
	}{
		{"proven switch, a small body", "1", "{}", SimulationConfig{APITimeout: 5, Unbounded: true}},
		{"switch off, a body of many writes", "0", strings.Repeat("x", 1<<20), SimulationConfig{APITimeout: 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, tc.proven)
			ln := closingListener(t)
			_, port, _ := net.SplitHostPort(ln.Addr().String())
			n, _ := strconv.Atoi(port)
			const runs = 300
			for i := 0; i < runs; i++ {
				test := &models.TestCase{Name: "post-echo", Kind: models.HTTP, AppPort: uint16(n),
					HTTPReq: models.HTTPReq{Method: "POST", URL: "http://" + ln.Addr().String() + "/echo", Header: map[string]string{}, Body: tc.body}}
				for _, simulate := range []struct {
					name string
					send func() error
				}{
					{"SimulateHTTP", func() error {
						_, err := SimulateHTTP(context.Background(), test, "set", zap.NewNop(), tc.cfg)
						return err
					}},
					{"SimulateHTTPStreaming", func() error {
						_, err := SimulateHTTPStreaming(context.Background(), test, "set", zap.NewNop(), tc.cfg)
						return err
					}},
				} {
					err := simulate.send()
					if err == nil {
						t.Fatalf("%s: a request to a port that drops every connection succeeded", simulate.name)
					}
					if !IsAppConnectionError(err) || !IsTransportConnReset(err) {
						t.Fatalf("%s, run %d of %d: the drop is not a drop: %v", simulate.name, i+1, runs, fmt.Sprint(err))
					}
				}
			}
		})
	}
}

// Every transport a test request is sent on dials through the request's
// peerEnd, and tries HTTP/2 exactly when a Transport without a DialContext of
// its own would: Go tries it by itself only for a Transport with no
// TLSClientConfig and no dialer of its own, unless ForceAttemptHTTP2 says to.
// So setting the dialer changes no request's protocol.
func TestEveryTestRequestsTransportNotesTheAppsEnd(t *testing.T) {
	for _, conn := range []string{"", "keep-alive", "close"} {
		for _, tlsCfg := range []*tls.Config{nil, {InsecureSkipVerify: true}} {
			name := fmt.Sprintf("Connection %q, keploy's TLS config %v", conn, tlsCfg != nil)
			header := map[string]string{}
			if conn != "" {
				header["Connection"] = conn
			}
			test := &models.TestCase{Name: "get", Kind: models.HTTP,
				HTTPReq: models.HTTPReq{Method: "GET", URL: "https://127.0.0.1:1/", Header: header}}
			prepared, err := prepareHTTPRequest(context.Background(), test, "set", zap.NewNop(), SimulationConfig{APITimeout: 1, TLSConfig: tlsCfg})
			if err != nil {
				t.Fatal(err)
			}
			tr, ok := prepared.Client.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("%s: transport %T", name, prepared.Client.Transport)
			}
			if tr.DialContext == nil || prepared.ended == nil {
				t.Errorf("%s: the transport does not note the app's end", name)
			}
			if tr.ForceAttemptHTTP2 != (tlsCfg == nil) {
				t.Errorf("%s: ForceAttemptHTTP2 %v, want %v", name, tr.ForceAttemptHTTP2, tlsCfg == nil)
			}
		}
	}
}

// A request's connection writes a body as the connection under it does: its
// own ReadFrom (a *net.TCPConn's), not a copy loop of net/http's.
func TestPeerEndConnWritesThroughItsConnectionsReadFrom(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b, _ := io.ReadAll(c)
		got <- string(b)
	}()
	c, err := (&peerEnd{}).dial(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	rf, ok := c.(io.ReaderFrom)
	if !ok {
		t.Fatalf("%T has no ReadFrom: net/http would copy a body through a loop of its own", c)
	}
	if n, err := rf.ReadFrom(strings.NewReader("hello")); err != nil || n != 5 {
		t.Fatalf("ReadFrom: %d, %v", n, err)
	}
	_ = c.Close()
	if s := <-got; s != "hello" {
		t.Fatalf("the app read %q", s)
	}
}
