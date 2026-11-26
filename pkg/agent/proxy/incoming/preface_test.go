package proxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// A connection's first bytes say what it carries as soon as they can: a TLS
// record by its header, HTTP/2 by all of its preface (which also begins an
// HTTP/1 request line, of method PRI), an HTTP/1 request by its method and
// space, and anything else by its first byte no method has. At the end (the
// connection's, or all 24 bytes read) the bytes say one kind.
func TestAConnectionsFirstBytesSayWhatItCarries(t *testing.T) {
	sslRequest := []byte{0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f} // PostgreSQL's
	for _, c := range []struct {
		b    string
		end  bool
		want prefaceKind
	}{
		{"", false, prefaceUndecided},
		{"", true, prefaceOther},
		{"G", false, prefaceUndecided},
		{"GET", false, prefaceUndecided},
		{"GET", true, prefaceOther},
		{"GET ", false, prefaceHTTP1},
		{"get /x", false, prefaceHTTP1},
		{"M-SEARCH * HTTP/1.1", false, prefaceHTTP1},
		{" GET /", false, prefaceOther},
		{"GET\r\n", false, prefaceOther},
		{"P", false, prefaceUndecided},
		{"PRI ", false, prefaceUndecided},
		{"PRI * HTTP/2.0\r\n", false, prefaceUndecided},
		{"PRI ", true, prefaceHTTP1},
		{"PRI /x HTTP/1.1", false, prefaceHTTP1},
		{clientPreface, false, prefaceH2},
		{clientPreface, true, prefaceH2},
		{strings.Repeat("A", len(clientPreface)), true, prefaceOther},
		{"\x16", false, prefaceUndecided},
		{"\x16\x03", false, prefaceUndecided},
		{"\x16\x03\x01\x02", false, prefaceUndecided},
		{"\x16\x03\x01\x02\x00", false, prefaceTLS},
		{"\x16\x03\x01", true, prefaceOther},
		{"\x16\x04", false, prefaceOther},
		{"\x16\x03\x04", false, prefaceOther},
		{string(sslRequest[:1]), false, prefaceOther},
		{string(sslRequest), false, prefaceOther},
		{"\x01\x00\x00\x00", false, prefaceOther},
	} {
		if got := kindOf([]byte(c.b), c.end); got != c.want {
			t.Errorf("kindOf(%q, end=%v) = %d, want %d", c.b, c.end, got, c.want)
		}
	}
}

// binaryApp is an app on 127.0.0.1 that speaks a protocol that is not HTTP,
// as Gradle's daemon does to its client: it takes a hello of helloLen bytes
// and answers it at once (none: no answer), then takes what comes up to the
// client's end and answers with its length. got has every byte the app was
// sent.
func binaryApp(t *testing.T, helloLen int) (port int, got chan []byte) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got = make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		hello := make([]byte, helloLen)
		if helloLen > 0 {
			if _, err := io.ReadFull(c, hello); err != nil {
				got <- nil
				return
			}
			if _, err := c.Write([]byte("N")); err != nil {
				got <- nil
				return
			}
		}
		rest, _ := io.ReadAll(c)
		_, _ = fmt.Fprintf(c, "took %d", len(rest))
		got <- append(hello, rest...)
	}()
	return ln.Addr().(*net.TCPAddr).Port, got
}

// A call the live handler takes (one between the run's own processes: a
// process of its tree to a listener of another) that is not HTTP reaches the
// app unread, both ways, at the address the proxy writes a dual-stack
// socket's destination in ([127.0.0.1]:p, a Java client's): its client's
// hello is answered before it sends more, and every byte it sends, those no
// HTTP request has among them, reaches the app as it was sent. Nothing is
// recorded. The hello is a binary protocol's (its first byte is no
// method's: PostgreSQL's 8-byte SSLRequest), or more bytes than HTTP/2's
// preface, all of them a method's characters (they say what they carry
// once 24 have come), or none: the client sends two such bytes and ends its
// side before they say what they carry. Before, the ingress refused that
// address as not an IP address and closed the call; given another, it
// waited for 24 bytes the client never sent before its answer, and read the
// rest as an HTTP/1 request, dropping it: Gradle's client, to the daemon it
// forks, failed with "The first result from the daemon was empty".
func TestACallBetweenTheRunsOwnProcessesThatIsNotHTTPReachesTheAppUnread(t *testing.T) {
	for _, c := range []struct {
		name        string
		hello, body []byte
	}{
		{"a binary hello", []byte{0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f}, []byte("\x00\x01binary\r\n\r\nGET / HTTP/1.1\r\n\r\n\xff")},
		{"a hello of a method's characters longer than the preface", []byte(strings.Repeat("A", len(clientPreface)+6)), []byte("rest")},
		{"bytes that end before they say what they carry", nil, []byte("GE")},
	} {
		for _, sync := range []bool{false, true} {
			t.Run(c.name+"/"+map[bool]string{false: "zero-copy", true: "synchronous"}[sync], func(t *testing.T) {
				logs := testNotHTTPCall(t, sync, c.hello, c.body)
				if n := logs.FilterMessageSnippet("Failed to read client request").Len(); n != 0 {
					t.Fatal("the connection was read as HTTP/1 (synchronous)")
				}
				if n := logs.FilterMessageSnippet("Failed to parse downstream HTTP request").Len(); n != 0 {
					t.Fatal("the connection was read as HTTP/1 (zero-copy)")
				}
			})
		}
	}
}

// testANotHTTPCallReachesTheAppUnread is one call of a non-HTTP protocol
// through ServeLive: the client sends hello and waits for its answer (none
// for an empty hello), then sends body and ends its side.
func testANotHTTPCallReachesTheAppUnread(t *testing.T, sync bool, hello, body []byte) {
	t.Helper()
	testNotHTTPCall(t, sync, hello, body)
}

func testNotHTTPCall(t *testing.T, sync bool, hello, body []byte) *observer.ObservedLogs {
	t.Helper()
	stubIngressPaused(t, func() bool { return false })
	var captured atomic.Int32
	stubCaptureHook(t, func(context.Context, *zap.Logger, chan *models.TestCase, *http.Request, *http.Response, time.Time, time.Time, models.IncomingOptions, bool, bool, uint16) {
		captured.Add(1)
	})
	core, logs := observer.New(zapcore.DebugLevel)
	cfg := &config.Config{}
	cfg.Agent.RecordRequests = true
	cfg.Agent.Synchronous = sync
	pm := New(zap.New(core), nil, cfg)
	t.Cleanup(pm.captures.Wait)
	port, got := binaryApp(t, len(hello))
	client, proxied := loopbackPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	served := make(chan struct{})
	go func() {
		defer close(served)
		pm.ServeLive(ctx, proxied, fmt.Sprintf("[127.0.0.1]:%d", port), uint16(port))
	}()
	_ = client.SetDeadline(time.Now().Add(20 * time.Second)) // a call that hangs fails, not the run
	if len(hello) > 0 {
		if _, err := client.Write(hello); err != nil {
			t.Fatal(err)
		}
		answer := make([]byte, 1)
		if _, err := io.ReadFull(client, answer); err != nil || string(answer) != "N" {
			t.Fatalf("the hello's answer = %q, %v; want the app's N, before the client sends more", answer, err)
		}
	}
	if _, err := client.Write(body); err != nil {
		t.Fatal(err)
	}
	_ = client.(*net.TCPConn).CloseWrite()
	end, err := io.ReadAll(client)
	if err != nil || string(end) != fmt.Sprintf("took %d", len(body)) {
		t.Fatalf("the app's last answer = %q, %v; want %q", end, err, fmt.Sprintf("took %d", len(body)))
	}
	want := append(append([]byte(nil), hello...), body...)
	if sent := <-got; !bytes.Equal(sent, want) {
		t.Fatalf("the app was sent %q, want the client's %q", sent, want)
	}
	<-served
	pm.captures.Wait()
	if n := captured.Load(); n != 0 {
		t.Fatalf("%d calls captured from a connection that is not HTTP, want none", n)
	}
	return logs
}

// An HTTP call the live handler takes at the address the proxy writes a
// dual-stack socket's destination in ([127.0.0.1]:p) reaches the app and is
// captured. Before, the ingress refused that address as not an IP address
// and port, and closed the call unanswered.
func TestAnHTTPCallToADualStackSocketsIPv4DestinationIsServedAndCaptured(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	var captured atomic.Int32
	stubCaptureHook(t, func(_ context.Context, _ *zap.Logger, _ chan *models.TestCase, req *http.Request, _ *http.Response, _, _ time.Time, _ models.IncomingOptions, _, _ bool, _ uint16) {
		if req.URL.Path == "/ping" {
			captured.Add(1)
		}
	})
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("pong"))
	}))
	t.Cleanup(app.Close)
	cfg := &config.Config{}
	cfg.Agent.RecordRequests = true
	pm := New(zap.NewNop(), nil, cfg)
	t.Cleanup(pm.captures.Wait)
	port := app.Listener.Addr().(*net.TCPAddr).Port
	client, proxied := loopbackPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	served := make(chan struct{})
	go func() {
		defer close(served)
		pm.ServeLive(ctx, proxied, fmt.Sprintf("[127.0.0.1]:%d", port), uint16(port))
	}()
	_ = client.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := client.Write([]byte("GET /ping HTTP/1.1\r\nHost: app\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("the call was not answered: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "pong" {
		t.Fatalf("the call's answer = %d %q, want the app's 200 pong", resp.StatusCode, b)
	}
	_ = client.Close()
	<-served
	pm.captures.Wait()
	if n := captured.Load(); n != 1 {
		t.Fatalf("%d captures of the call, want 1", n)
	}
}

// readPreface must cap its buffer at exactly len(clientPreface) (24 bytes):
// when HTTP/2's client preface and its initial SETTINGS frame arrive in a
// single write, readPreface must read only the 24-byte preface, classify it
// as prefaceH2, and leave the SETTINGS frame intact on conn.
func TestReadPrefaceClassifiesHTTP2PrefaceAndSettingsInOneWriteAsH2(t *testing.T) {
	settings := "\x00\x00\x00\x04\x00\x00\x00\x00\x00"
	client, server := loopbackPair(t)
	if _, err := client.Write([]byte(clientPreface + settings)); err != nil {
		t.Fatal(err)
	}
	_ = client.(*net.TCPConn).CloseWrite()
	got, kind, err := readPreface(context.Background(), zap.NewNop(), server, nil)
	if err != nil {
		t.Fatalf("readPreface error: %v", err)
	}
	if kind != prefaceH2 {
		t.Fatalf("kind = %d, want prefaceH2 (%d) for preface+SETTINGS in one write (got %q)", kind, prefaceH2, got)
	}
	if string(got) != clientPreface {
		t.Fatalf("preface = %q, want %q", got, clientPreface)
	}
	rest, err := io.ReadAll(server)
	if err != nil || string(rest) != settings {
		t.Fatalf("remaining bytes = %q, %v; want SETTINGS frame %q", rest, err, settings)
	}
}

// isH2SettingsFrame and isH2SettingsPrefix distinguish an HTTP/2 server
// SETTINGS frame (which waits for the client's HTTP/2 preface) from every
// other app-first protocol greeting (SMTP, SSH, MySQL), which relays unread
// at once.
func TestIsH2SettingsFrameAndPrefix(t *testing.T) {
	for _, c := range []struct {
		name       string
		b          []byte
		wantFrame  bool
		wantPrefix bool
	}{
		{"empty settings", []byte{0, 0, 0, 4, 0, 0, 0, 0, 0}, true, false},
		{"one setting + payload", []byte{0, 0, 6, 4, 0, 0, 0, 0, 0, 0, 3, 0, 0, 0, 100}, true, false},
		{"reserved MSB in stream ID ignored", []byte{0, 0, 0, 4, 0, 0x80, 0, 0, 0}, true, false},
		{"prefix 1 byte", []byte{0}, false, true},
		{"prefix 4 bytes", []byte{0, 0, 6, 4}, false, true},
		{"prefix 8 bytes", []byte{0, 0, 6, 4, 0, 0, 0, 0}, false, true},
		{"length not multiple of 6", []byte{0, 0, 5, 4, 0, 0, 0, 0, 0}, false, false},
		{"prefix length not multiple of 6", []byte{0, 0, 5}, false, false},
		{"length over 16384", []byte{0, 0x41, 0, 4, 0, 0, 0, 0, 0}, false, false},
		{"prefix length over 16384", []byte{0, 0x41}, false, false},
		{"wrong frame type", []byte{0, 0, 0, 1, 0, 0, 0, 0, 0}, false, false},
		{"settings ACK flag", []byte{0, 0, 0, 4, 1, 0, 0, 0, 0}, false, false},
		{"non-zero stream ID", []byte{0, 0, 0, 4, 0, 0, 0, 0, 1}, false, false},
		{"SMTP 220 greeting", []byte("220 hi\r\n"), false, false},
		{"SSH banner", []byte("SSH-2.0-OpenSSH_9.0\r\n"), false, false},
		{"MySQL greeting", []byte{0x4a, 0x00, 0x00, 0x00, 0x0a, '8', '.', '0'}, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := isH2SettingsFrame(c.b); got != c.wantFrame {
				t.Errorf("isH2SettingsFrame(%x) = %v, want %v", c.b, got, c.wantFrame)
			}
			if got := isH2SettingsPrefix(c.b); got != c.wantPrefix {
				t.Errorf("isH2SettingsPrefix(%x) = %v, want %v", c.b, got, c.wantPrefix)
			}
		})
	}
}

// A server-first protocol (SMTP, MySQL, SSH): the app speaks before the
// client writes anything. Because keploy dials eagerly and watches the app's
// side concurrently, the greeting reaches the client at once and the rest of
// the session relays unread both ways in both zero-copy and synchronous modes.
func TestServerFirstGreetingReachesTheClient(t *testing.T) {
	for _, tc := range []struct {
		name     string
		greeting []byte
		reply    []byte
		ack      []byte
	}{
		{"SMTP", []byte("220 hi\r\n"), []byte("EHLO client\r\n"), []byte("250 ok\r\n")},
		{"MySQL", []byte{0x0e, 0x00, 0x00, 0x00, 0x0a, '8', '.', '0', '.', '3', '6', 0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06}, []byte{0x04, 0x00, 0x00, 0x01, 0x01, 0x02, 0x03, 0x04}, []byte{0x01, 0x00, 0x00, 0x02, 0x00}},
		{"SSH", []byte("SSH-2.0-OpenSSH_9.0\r\n"), []byte("SSH-2.0-Client\r\n"), []byte("OK\r\n")},
	} {
		for _, sync := range []bool{false, true} {
			t.Run(tc.name+"/"+map[bool]string{false: "zero-copy", true: "synchronous"}[sync], func(t *testing.T) {
				stubIngressPaused(t, func() bool { return false })
				var captured atomic.Int32
				stubCaptureHook(t, func(context.Context, *zap.Logger, chan *models.TestCase, *http.Request, *http.Response, time.Time, time.Time, models.IncomingOptions, bool, bool, uint16) {
					captured.Add(1)
				})
				ln, err := net.Listen("tcp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = ln.Close() })
				go func() {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					defer func() { _ = c.Close() }()
					_, _ = c.Write(tc.greeting)
					got := make([]byte, len(tc.reply))
					if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, tc.reply) {
						return
					}
					_, _ = c.Write(tc.ack)
				}()
				cfg := &config.Config{}
				cfg.Agent.RecordRequests = true
				cfg.Agent.Synchronous = sync
				pm := New(zap.NewNop(), nil, cfg)
				t.Cleanup(pm.captures.Wait)
				client, proxied := loopbackPair(t)
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				served := make(chan struct{})
				go func() {
					defer close(served)
					pm.ServeLive(ctx, proxied, ln.Addr().String(), uint16(ln.Addr().(*net.TCPAddr).Port))
				}()
				_ = client.SetDeadline(time.Now().Add(5 * time.Second))
				g := make([]byte, len(tc.greeting))
				if _, err := io.ReadFull(client, g); err != nil || !bytes.Equal(g, tc.greeting) {
					t.Fatalf("greeting = %q, %v; want %q", g, err, tc.greeting)
				}
				if _, err := client.Write(tc.reply); err != nil {
					t.Fatal(err)
				}
				a := make([]byte, len(tc.ack))
				if _, err := io.ReadFull(client, a); err != nil || !bytes.Equal(a, tc.ack) {
					t.Fatalf("ack = %q, %v; want %q", a, err, tc.ack)
				}
				_ = client.Close()
				<-served
				pm.captures.Wait()
				if n := captured.Load(); n != 0 {
					t.Fatalf("%d captures on server-first protocol, want 0", n)
				}
			})
		}
	}
}

// When the app sends an HTTP/2 SETTINGS frame immediately upon accept (as a
// gRPC server does), keploy waits for the client's HTTP/2 preface instead of
// treating it as a non-HTTP server-first greeting, replays the SETTINGS frame
// intact, and records the gRPC call.
func TestHTTP2ServerSettingsFirstStillRecordsGRPC(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	frames := func(write func(*http2.Framer, *hpack.Encoder, *bytes.Buffer) error) []byte {
		var out, block bytes.Buffer
		if err := write(http2.NewFramer(&out, nil), hpack.NewEncoder(&block), &block); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	headers := func(fr *http2.Framer, enc *hpack.Encoder, block *bytes.Buffer, end bool, fields ...string) error {
		block.Reset()
		for i := 0; i < len(fields); i += 2 {
			if err := enc.WriteField(hpack.HeaderField{Name: fields[i], Value: fields[i+1]}); err != nil {
				return err
			}
		}
		return fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: block.Bytes(), EndStream: end, EndHeaders: true})
	}
	serverSettings := frames(func(fr *http2.Framer, _ *hpack.Encoder, _ *bytes.Buffer) error {
		return fr.WriteSettings(http2.Setting{ID: http2.SettingMaxConcurrentStreams, Val: 100})
	})
	msg := []byte{0, 0, 0, 0, 0}
	req := append([]byte(clientPreface), frames(func(fr *http2.Framer, enc *hpack.Encoder, block *bytes.Buffer) error {
		if err := headers(fr, enc, block, false, ":method", "POST", ":scheme", "http", ":path", "/svc.Echo/Ping",
			":authority", "localhost", "content-type", "application/grpc"); err != nil {
			return err
		}
		return fr.WriteData(1, true, msg)
	})...)
	resp := frames(func(fr *http2.Framer, enc *hpack.Encoder, block *bytes.Buffer) error {
		if err := headers(fr, enc, block, false, ":status", "200", "content-type", "application/grpc"); err != nil {
			return err
		}
		if err := fr.WriteData(1, false, msg); err != nil {
			return err
		}
		return headers(fr, enc, block, true, "grpc-status", "0")
	})

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	settingsSent := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = c.Write(serverSettings)
		close(settingsSent)
		if _, err := io.ReadFull(c, make([]byte, len(req))); err == nil {
			_, _ = c.Write(resp)
		}
	}()

	cfg := &config.Config{}
	cfg.Agent.RecordRequests = true
	pm := New(zap.NewNop(), nil, cfg)
	tcs := make(chan *models.TestCase, 4)
	client, proxied := loopbackPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	served := make(chan struct{})
	go func() {
		defer close(served)
		pm.handleConnection(ctx, proxied, ln.Addr().String(), pm.logger, tcs, make(chan struct{}, 1), 50051)
	}()

	// Wait until the app has written its SETTINGS frame and let it reach
	// keploy before the client speaks: keploy must NOT bail to raw relay.
	<-settingsSent
	time.Sleep(50 * time.Millisecond)

	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := client.Write(req); err != nil {
		t.Fatal(err)
	}
	wantResp := append(append([]byte(nil), serverSettings...), resp...)
	got := make([]byte, len(wantResp))
	if _, err := io.ReadFull(client, got); err != nil || !bytes.Equal(got, wantResp) {
		t.Fatalf("client read %x (%v), want SETTINGS+resp %x", got, err, wantResp)
	}
	_ = client.Close()
	<-served

	select {
	case tc := <-tcs:
		if tc == nil || tc.Kind != models.GRPC_EXPORT {
			t.Fatalf("captured test case = %+v, want GRPC_EXPORT", tc)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gRPC call was not recorded after app sent HTTP/2 SETTINGS first")
	}
}

// When neither side speaks yet, no timer runs: waiting longer than
// undecidedPrefaceWait does not cause an idle client-first HTTP connection to
// lose capture or a late server-first greeting to be dropped.
func TestBothSidesSilentWaitsWithoutTimingOut(t *testing.T) {
	quietWait := undecidedPrefaceWait + 150*time.Millisecond

	t.Run("late HTTP client is still captured", func(t *testing.T) {
		stubIngressPaused(t, func() bool { return false })
		var captured atomic.Int32
		stubCaptureHook(t, func(_ context.Context, _ *zap.Logger, _ chan *models.TestCase, req *http.Request, _ *http.Response, _, _ time.Time, _ models.IncomingOptions, _, _ bool, _ uint16) {
			if req.URL.Path == "/after-idle" {
				captured.Add(1)
			}
		})
		app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("awake"))
		}))
		t.Cleanup(app.Close)
		cfg := &config.Config{}
		cfg.Agent.RecordRequests = true
		pm := New(zap.NewNop(), nil, cfg)
		t.Cleanup(pm.captures.Wait)
		client, proxied := loopbackPair(t)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		served := make(chan struct{})
		go func() {
			defer close(served)
			pm.ServeLive(ctx, proxied, app.Listener.Addr().String(), uint16(app.Listener.Addr().(*net.TCPAddr).Port))
		}()
		time.Sleep(quietWait)
		_ = client.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := client.Write([]byte("GET /after-idle HTTP/1.1\r\nHost: app\r\nConnection: close\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(client), nil)
		if err != nil {
			t.Fatalf("late HTTP request not answered: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(b) != "awake" {
			t.Fatalf("answer = %d %q, want 200 awake", resp.StatusCode, b)
		}
		_ = client.Close()
		<-served
		pm.captures.Wait()
		if n := captured.Load(); n != 1 {
			t.Fatalf("captures = %d, want 1 after idle wait", n)
		}
	})

	t.Run("late server greeting reaches client", func(t *testing.T) {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }()
			time.Sleep(quietWait)
			_, _ = c.Write([]byte("220 late\r\n"))
		}()
		cfg := &config.Config{}
		cfg.Agent.RecordRequests = true
		pm := New(zap.NewNop(), nil, cfg)
		client, proxied := loopbackPair(t)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		served := make(chan struct{})
		go func() {
			defer close(served)
			pm.ServeLive(ctx, proxied, ln.Addr().String(), uint16(ln.Addr().(*net.TCPAddr).Port))
		}()
		_ = client.SetDeadline(time.Now().Add(5 * time.Second))
		g := make([]byte, 10)
		if _, err := io.ReadFull(client, g); err != nil || string(g) != "220 late\r\n" {
			t.Fatalf("late greeting = %q, %v; want %q", g, err, "220 late\r\n")
		}
		_ = client.Close()
		<-served
	})
}

// JDWP's handshake ("JDWP-Handshake", 14 token characters with no space)
// waits for the server's reply before sending more: once the client bytes
// stay undecided for undecidedPrefaceWait, keploy relays them unread and the
// answer reaches the client in both zero-copy and synchronous modes.
func TestTokenOnlyFirstMessageReachesTheAppUnread(t *testing.T) {
	for _, sync := range []bool{false, true} {
		t.Run(map[bool]string{false: "zero-copy", true: "synchronous"}[sync], func(t *testing.T) {
			testANotHTTPCallReachesTheAppUnread(t, sync, []byte("JDWP-Handshake"), []byte("x"))
		})
	}
}

// A client-first text protocol whose first line is a token and a space
// (memcached "get foo\r\n", Redis inline "SET a b\r\n", IRC "NICK x\r\n", or
// a truncated "get foo" followed by half-close) enters HTTP/1 parsing, fails
// http.ReadRequest before any byte is forwarded, and is relayed to the app
// unread instead of returning a synthetic 400 or closing with nothing — for
// both the first message on a connection and a later message on a keep-alive
// HTTP connection, in both zero-copy and synchronous modes.
func TestAWordAndASpaceThatIsNotHTTPReachesTheAppUnread(t *testing.T) {
	for _, hello := range []string{"get foo\r\n", "SET a b\r\n", "NICK x\r\n"} {
		for _, sync := range []bool{false, true} {
			t.Run(fmt.Sprintf("%q/sync=%v", hello, sync), func(t *testing.T) {
				testANotHTTPCallReachesTheAppUnread(t, sync, []byte(hello), []byte("more\r\n"))
			})
		}
	}
	for _, sync := range []bool{false, true} {
		t.Run(fmt.Sprintf("truncated/sync=%v", sync), func(t *testing.T) {
			testANotHTTPCallReachesTheAppUnread(t, sync, nil, []byte("get foo"))
		})
	}
	for _, sync := range []bool{false, true} {
		t.Run(fmt.Sprintf("later-request-on-keepalive/sync=%v", sync), func(t *testing.T) {
			stubIngressPaused(t, func() bool { return false })
			var captured atomic.Int32
			stubCaptureHook(t, func(_ context.Context, _ *zap.Logger, _ chan *models.TestCase, req *http.Request, _ *http.Response, _, _ time.Time, _ models.IncomingOptions, _, _ bool, _ uint16) {
				if req.URL.Path == "/ok" {
					captured.Add(1)
				}
			})
			ln, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			gotSecond := make(chan string, 1)
			go func() {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				defer func() { _ = c.Close() }()
				br := bufio.NewReader(c)
				if _, err := http.ReadRequest(br); err != nil {
					gotSecond <- "err1:" + err.Error()
					return
				}
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
				line, err := br.ReadString('\n')
				if err != nil {
					gotSecond <- "err2:" + err.Error()
					return
				}
				_, _ = io.WriteString(c, "VALUE foo\r\n")
				gotSecond <- line
			}()
			cfg := &config.Config{}
			cfg.Agent.RecordRequests = true
			cfg.Agent.Synchronous = sync
			pm := New(zap.NewNop(), nil, cfg)
			t.Cleanup(pm.captures.Wait)
			client, proxied := loopbackPair(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			served := make(chan struct{})
			go func() {
				defer close(served)
				pm.ServeLive(ctx, proxied, ln.Addr().String(), uint16(ln.Addr().(*net.TCPAddr).Port))
			}()
			_ = client.SetDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.WriteString(client, "GET /ok HTTP/1.1\r\nHost: app\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			br := bufio.NewReader(client)
			resp, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("first HTTP response: %v", err)
			}
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(b) != "ok" {
				t.Fatalf("first body = %q, want ok", b)
			}
			// In synchronous mode forceCloseMode injects Connection: close on
			// the first response; only zero-copy keeps the connection alive
			// for a second message. When kept alive, a non-HTTP second message
			// must reach the app unread.
			if sync {
				_ = client.Close()
				<-served
				pm.captures.Wait()
				return
			}
			if _, err := io.WriteString(client, "get foo\r\n"); err != nil {
				t.Fatal(err)
			}
			line, err := br.ReadString('\n')
			if err != nil || line != "VALUE foo\r\n" {
				t.Fatalf("second answer = %q, %v; want VALUE foo\\r\\n", line, err)
			}
			_ = client.Close()
			<-served
			pm.captures.Wait()
			if got := <-gotSecond; got != "get foo\r\n" {
				t.Fatalf("app saw second message %q, want %q", got, "get foo\r\n")
			}
			if n := captured.Load(); n != 1 {
				t.Fatalf("captures = %d, want 1 (only the HTTP request)", n)
			}
		})
	}
	t.Run("pipelined-non-http-after-keepalive", func(t *testing.T) {
		stubIngressPaused(t, func() bool { return false })
		var captured atomic.Int32
		stubCaptureHook(t, func(_ context.Context, _ *zap.Logger, _ chan *models.TestCase, req *http.Request, _ *http.Response, _, _ time.Time, _ models.IncomingOptions, _, _ bool, _ uint16) {
			if req.URL.Path == "/ok" {
				captured.Add(1)
			}
		})
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		gotSecond := make(chan string, 1)
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }()
			br := bufio.NewReader(c)
			if _, err := http.ReadRequest(br); err != nil {
				gotSecond <- "err1:" + err.Error()
				return
			}
			_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
			line, err := br.ReadString('\n')
			if err != nil {
				gotSecond <- "err2:" + err.Error()
				return
			}
			_, _ = io.WriteString(c, "VALUE foo\r\n")
			gotSecond <- line
		}()
		cfg := &config.Config{}
		cfg.Agent.RecordRequests = true
		pm := New(zap.NewNop(), nil, cfg)
		t.Cleanup(pm.captures.Wait)
		client, proxied := loopbackPair(t)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		served := make(chan struct{})
		go func() {
			defer close(served)
			pm.ServeLive(ctx, proxied, ln.Addr().String(), uint16(ln.Addr().(*net.TCPAddr).Port))
		}()
		_ = client.SetDeadline(time.Now().Add(10 * time.Second))
		// Write both the HTTP request and the non-HTTP second message in one
		// TCP write so bufio.Reader prefetches the non-HTTP bytes during
		// request 1 and reqTeeConn.startReq must capture clientReader.Buffered().
		if _, err := io.WriteString(client, "GET /ok HTTP/1.1\r\nHost: app\r\n\r\nget foo\r\n"); err != nil {
			t.Fatal(err)
		}
		br := bufio.NewReader(client)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("first HTTP response: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(b) != "ok" {
			t.Fatalf("first body = %q, want ok", b)
		}
		line, err := br.ReadString('\n')
		if err != nil || line != "VALUE foo\r\n" {
			t.Fatalf("second answer = %q, %v; want VALUE foo\\r\\n", line, err)
		}
		_ = client.Close()
		<-served
		pm.captures.Wait()
		if got := <-gotSecond; got != "get foo\r\n" {
			t.Fatalf("app saw pipelined second message %q, want %q", got, "get foo\r\n")
		}
		if n := captured.Load(); n != 1 {
			t.Fatalf("captures = %d, want 1", n)
		}
	})
}

// Client-first HTTP pays no preface wait delay: 20 sequential fresh
// connections through ServeLive complete with per-request latency far below
// undecidedPrefaceWait (250 ms) and within a small loopback overhead of
// direct connections.
func TestClientFirstHTTPAddsNoPrefaceDelay(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	stubCaptureHook(t, func(context.Context, *zap.Logger, chan *models.TestCase, *http.Request, *http.Response, time.Time, time.Time, models.IncomingOptions, bool, bool, uint16) {
	})
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("pong"))
	}))
	t.Cleanup(app.Close)
	appAddr := app.Listener.Addr().String()
	port := uint16(app.Listener.Addr().(*net.TCPAddr).Port)

	const runs = 20
	directDurs := make([]time.Duration, runs)
	for i := range runs {
		t0 := time.Now()
		c, err := net.Dial("tcp4", appAddr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(c, "GET /lat HTTP/1.1\r\nHost: app\r\nConnection: close\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		_ = c.Close()
		directDurs[i] = time.Since(t0)
	}

	cfg := &config.Config{}
	cfg.Agent.RecordRequests = true
	pm := New(zap.NewNop(), nil, cfg)
	t.Cleanup(pm.captures.Wait)

	proxiedDurs := make([]time.Duration, runs)
	for i := range runs {
		client, proxied := loopbackPair(t)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		served := make(chan struct{})
		t0 := time.Now()
		go func() {
			defer close(served)
			pm.ServeLive(ctx, proxied, appAddr, port)
		}()
		if _, err := io.WriteString(client, "GET /lat HTTP/1.1\r\nHost: app\r\nConnection: close\r\n\r\n"); err != nil {
			cancel()
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(client), nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		proxiedDurs[i] = time.Since(t0)
		_ = client.Close()
		<-served
		cancel()
	}
	pm.captures.Wait()

	slices.Sort(directDurs)
	slices.Sort(proxiedDurs)
	directMed := directDurs[runs/2]
	proxiedMed := proxiedDurs[runs/2]
	t.Logf("HTTP latency over %d fresh connections: direct median=%v max=%v; proxied median=%v max=%v (undecidedPrefaceWait=%v)",
		runs, directMed, directDurs[runs-1], proxiedMed, proxiedDurs[runs-1], undecidedPrefaceWait)
	if proxiedMed >= undecidedPrefaceWait/5 {
		t.Fatalf("proxied HTTP median latency %v is too high (want well below %v; direct median=%v)",
			proxiedMed, undecidedPrefaceWait/5, directMed)
	}
}

// An HTTP/1.0 request with no header, shorter than HTTP/2's 24-byte
// preface, is answered: the ingress no longer waits for 24 bytes its
// client, waiting for the answer, never sends.
func TestAnHTTPRequestShorterThanTheHTTP2PrefaceIsAnswered(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	stubCaptureHook(t, func(context.Context, *zap.Logger, chan *models.TestCase, *http.Request, *http.Response, time.Time, time.Time, models.IncomingOptions, bool, bool, uint16) {
	})
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("short"))
	}))
	t.Cleanup(app.Close)
	cfg := &config.Config{}
	cfg.Agent.RecordRequests = true
	pm := New(zap.NewNop(), nil, cfg)
	t.Cleanup(pm.captures.Wait)
	client, proxied := loopbackPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	served := make(chan struct{})
	go func() {
		defer close(served)
		pm.handleConnection(ctx, proxied, app.Listener.Addr().String(), pm.logger, make(chan *models.TestCase, 1), make(chan struct{}, 1), 8080)
	}()
	_ = client.SetDeadline(time.Now().Add(20 * time.Second))
	req := "GET /s HTTP/1.0\r\n\r\n"
	if len(req) >= len(clientPreface) {
		t.Fatalf("the request is %d bytes, want fewer than the preface's %d", len(req), len(clientPreface))
	}
	if _, err := client.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("the short request was not answered: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "short" {
		t.Fatalf("the answer's body = %q, want the app's", b)
	}
	_ = client.Close()
	<-served
}

// A recorded connection that ends before its first byte connects to the app
// and closes it cleanly, as without keploy: eager dialing delivers the
// connect and its close (EOF, zero bytes) to the app, and closes the client.
func TestAConnectionThatEndsBeforeItsFirstByteConnectsAndClosesAtTheApp(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	appResult := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			appResult <- fmt.Errorf("accept failed: %w", err)
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		b, rerr := io.ReadAll(c)
		if rerr != nil || len(b) != 0 {
			appResult <- fmt.Errorf("the app read %q, %v; want 0 bytes and EOF", b, rerr)
			return
		}
		appResult <- nil
	}()
	stubIngressPaused(t, func() bool { return false })
	cfg := &config.Config{}
	cfg.Agent.RecordRequests = true
	pm := New(zap.NewNop(), nil, cfg)
	t.Cleanup(pm.captures.Wait)
	client, proxied := loopbackPair(t)
	if err := client.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	served := make(chan struct{})
	go func() {
		defer close(served)
		pm.ServeLive(ctx, proxied, ln.Addr().String(), uint16(ln.Addr().(*net.TCPAddr).Port))
	}()
	if err := <-appResult; err != nil {
		t.Fatal(err)
	}
	<-served
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	if n, err := client.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("the client's read = %d, %v; want its connection ended (EOF)", n, err)
	}
}

// An HTTP request whose first bytes come in pieces is told and captured as
// HTTP: the ingress reads on until the bytes say what the connection
// carries. Its client is a net.Pipe, so the ingress reads each piece by
// itself: decided on its first read, "G" read as not HTTP, and the call was
// relayed unrecorded.
func TestAnHTTPRequestWhoseFirstBytesComeInPiecesIsCaptured(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	var captured atomic.Int32
	stubCaptureHook(t, func(_ context.Context, _ *zap.Logger, _ chan *models.TestCase, req *http.Request, _ *http.Response, _, _ time.Time, _ models.IncomingOptions, _, _ bool, _ uint16) {
		if req.URL.Path == "/ping" {
			captured.Add(1)
		}
	})
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("pong"))
	}))
	t.Cleanup(app.Close)
	cfg := &config.Config{}
	cfg.Agent.RecordRequests = true
	pm := New(zap.NewNop(), nil, cfg)
	t.Cleanup(pm.captures.Wait)
	port := app.Listener.Addr().(*net.TCPAddr).Port
	client, proxied := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	served := make(chan struct{})
	go func() {
		defer close(served)
		pm.ServeLive(ctx, proxied, app.Listener.Addr().String(), uint16(port))
	}()
	_ = client.SetDeadline(time.Now().Add(20 * time.Second))
	// A net.Pipe write returns once the reader has taken all of it.
	for _, piece := range []string{"G", "ET", " /ping HTTP/1.1\r\nHost: app\r\nConnection: close\r\n\r\n"} {
		if _, err := client.Write([]byte(piece)); err != nil {
			t.Fatalf("writing %q: %v", piece, err)
		}
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("the call was not answered: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "pong" {
		t.Fatalf("the call's answer = %d %q, want the app's 200 pong", resp.StatusCode, b)
	}
	_ = client.Close()
	<-served
	pm.captures.Wait()
	if n := captured.Load(); n != 1 {
		t.Fatalf("%d captures of the call, want 1", n)
	}
}
