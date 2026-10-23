package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// h2Client is the client end of an HTTP/2 connection, written frame by frame.
type h2Client struct {
	t  *testing.T
	c  net.Conn
	fr *http2.Framer
}

func newH2Client(t *testing.T, c net.Conn) *h2Client {
	t.Helper()
	// A test whose server never writes fails at its deadline rather than
	// hanging the binary.
	if dl, ok := t.Deadline(); ok {
		_ = c.SetReadDeadline(dl.Add(-time.Until(dl) / 4))
	}
	return &h2Client{t: t, c: c, fr: http2.NewFramer(c, c)}
}

// preface writes the client preface and the client's SETTINGS.
func (h *h2Client) preface() {
	h.t.Helper()
	if _, err := io.WriteString(h.c, http2.ClientPreface); err != nil {
		h.t.Fatal(err)
	}
	if err := h.fr.WriteSettings(); err != nil {
		h.t.Fatal(err)
	}
}

// request writes a GET on stream 1 with content-type ct, ending the stream.
func (h *h2Client) request(ct string) {
	h.t.Helper()
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range []hpack.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"},
		{Name: ":authority", Value: "deps.example"}, {Name: ":path", Value: "/v1/items"}, {Name: "content-type", Value: ct}} {
		_ = enc.WriteField(f)
	}
	if err := h.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndHeaders: true, EndStream: true}); err != nil {
		h.t.Fatal(err)
	}
}

// read reads the server's next frame.
func (h *h2Client) read() http2.Frame {
	h.t.Helper()
	f, err := h.fr.ReadFrame()
	if err != nil {
		h.t.Fatalf("reading the server's next frame: %v", err)
	}
	return f
}

// serverSide reads what the server end of conn got before it decides: the
// client preface and SETTINGS, as the proxy's first read gets them.
func serverSide(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	buf := make([]byte, len(http2.ClientPreface)+h2FrameHeaderLen)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	return buf
}

// prefaced writes the client's preface on cl and returns what the server end
// read of it (serverSide). The client writes from the test's goroutine: its
// framer is not shared with another.
func prefaced(t *testing.T, cl *h2Client, proxySide net.Conn) []byte {
	t.Helper()
	read := make(chan []byte, 1)
	go func() {
		buf := make([]byte, len(http2.ClientPreface)+h2FrameHeaderLen)
		if _, err := io.ReadFull(proxySide, buf); err != nil {
			read <- nil
			return
		}
		read <- buf
	}()
	cl.preface()
	buf := awaitEvent(t, read, "the server end reading the client preface")
	if buf == nil {
		t.Fatal("the server end could not read the client preface")
	}
	return buf
}

// contentType is the content-type of the first HEADERS frame in buf, which
// starts with the client preface; "" when it has none.
func contentType(buf []byte) string {
	if !util.HasHTTP2HeadersFrame(buf) {
		return ""
	}
	fr := http2.NewFramer(nil, bytes.NewReader(buf[len(http2.ClientPreface):]))
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			return ""
		}
		if hf, ok := f.(*http2.HeadersFrame); ok {
			ct := ""
			dec := hpack.NewDecoder(4096, func(f hpack.HeaderField) {
				if f.Name == "content-type" {
					ct = f.Value
				}
			})
			_, _ = dec.Write(hf.HeaderBlockFragment())
			return ct
		}
	}
}

// h2Frames are the types of the frames in buf after the client preface, a
// SETTINGS ACK as "ACK".
func h2Frames(buf []byte) []string {
	var out []string
	fr := http2.NewFramer(nil, bytes.NewReader(buf[len(http2.ClientPreface):]))
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			return out
		}
		if s, ok := f.(*http2.SettingsFrame); ok && s.IsAck() {
			out = append(out, "ACK")
			continue
		}
		out = append(out, f.Header().Type.String())
	}
}

type h2Read struct {
	buf  []byte
	conn net.Conn
	err  error
}

// A gRPC client waits for the server's SETTINGS before its HEADERS: keploy
// answers the preface with its own, and reads until the HEADERS, which then
// decide. The client's acknowledgement of keploy's SETTINGS and a PING keploy
// answered never reach the parser.
func TestAnHTTP2ClientThatWaitsForTheServersPrefaceSendsItsHeaders(t *testing.T) {
	app, proxySide := net.Pipe()
	t.Cleanup(func() { _ = app.Close(); _ = proxySide.Close() })
	cl := newH2Client(t, app)
	initial := prefaced(t, cl, proxySide)
	done := make(chan h2Read, 1)
	go func() {
		buf, conn, err := readHTTP2UntilHeaders(context.Background(), proxySide, initial)
		done <- h2Read{buf, conn, err}
	}()
	if s, ok := cl.read().(*http2.SettingsFrame); !ok || s.IsAck() {
		t.Fatal("keploy did not answer the client preface with SETTINGS")
	}
	if err := cl.fr.WriteSettingsAck(); err != nil {
		t.Fatal(err)
	}
	if err := cl.fr.WritePing(false, [8]byte{1, 2, 3, 4, 5, 6, 7, 8}); err != nil {
		t.Fatal(err)
	}
	if p, ok := cl.read().(*http2.PingFrame); !ok || !p.IsAck() || p.Data != [8]byte{1, 2, 3, 4, 5, 6, 7, 8} {
		t.Fatal("keploy did not answer the client's PING")
	}
	cl.request("application/grpc")
	got := awaitEvent(t, done, "the read until HEADERS returning")
	if got.err != nil {
		t.Fatal(got.err)
	}
	if ct := contentType(got.buf); ct != "application/grpc" {
		t.Fatalf("the matchers read content-type %q, want application/grpc", ct)
	}
	if frames := h2Frames(got.buf); len(frames) != 2 || frames[0] != "SETTINGS" || frames[1] != "HEADERS" {
		t.Fatalf("the parser reads frames %v, want the client's SETTINGS and HEADERS only", frames)
	}
	if _, filtered := got.conn.(*ackFilter); filtered {
		t.Fatal("the acknowledgement was read already, yet the rest of the stream is filtered")
	}
}

// A client starved for CPU sends its HEADERS long after its preface: keploy
// waits for them, whatever the delay, and decides on them. Nothing is decided
// from the silence before them.
func TestAStarvedHTTP2ClientIsDecidedOnItsHeaders(t *testing.T) {
	app, proxySide := net.Pipe()
	t.Cleanup(func() { _ = app.Close(); _ = proxySide.Close() })
	cl := newH2Client(t, app)
	initial := prefaced(t, cl, proxySide)
	done := make(chan h2Read, 1)
	go func() {
		buf, conn, err := readHTTP2UntilHeaders(context.Background(), proxySide, initial)
		done <- h2Read{buf, conn, err}
	}()
	cl.read() // keploy's SETTINGS: keploy is now reading
	select {
	case <-done:
		t.Fatal("the connection was decided before its HEADERS")
	default:
	}
	cl.request("application/json")
	got := awaitEvent(t, done, "the read until HEADERS returning")
	if got.err != nil {
		t.Fatal(got.err)
	}
	if ct := contentType(got.buf); ct != "application/json" {
		t.Fatalf("the matchers read content-type %q, want application/json", ct)
	}
}

// The parser keploy hands an h2c connection to serves it with Go's HTTP/2
// server, which hangs up on a SETTINGS ACK it never asked for. The client's
// acknowledgement of keploy's SETTINGS comes after its HEADERS here, so it is
// taken out of the stream after the hand-off: the request is answered.
func TestTheHandOffServerNeverSeesTheAckOfKeploysSettings(t *testing.T) {
	app, proxySide := acceptedConn(t)
	cl := newH2Client(t, app)
	cl.preface()
	initial := serverSide(t, proxySide)
	done := make(chan h2Read, 1)
	go func() {
		buf, conn, err := readHTTP2UntilHeaders(context.Background(), proxySide, initial)
		done <- h2Read{buf, conn, err}
	}()
	cl.read() // keploy's SETTINGS
	cl.request("application/json")
	got := awaitEvent(t, done, "the read until HEADERS returning")
	if got.err != nil {
		t.Fatal(got.err)
	}
	if _, filtered := got.conn.(*ackFilter); !filtered {
		t.Fatal("the acknowledgement is still to come, yet the rest of the stream is not filtered")
	}
	// The acknowledgement comes now, after the hand-off.
	if err := cl.fr.WriteSettingsAck(); err != nil {
		t.Fatal(err)
	}
	served := &util.Conn{Conn: got.conn, Reader: util.NewPrefixReader(got.buf, got.conn), Logger: zap.NewNop()}
	go (&http2.Server{}).ServeConn(served, &http2.ServeConnOpts{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})})
	for {
		switch f := cl.read().(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				if err := cl.fr.WriteSettingsAck(); err != nil {
					t.Fatal(err)
				}
			}
		case *http2.GoAwayFrame:
			t.Fatalf("the server hung up: %v", f.ErrCode)
		case *http2.HeadersFrame:
			if f.StreamID != 1 {
				t.Fatalf("a response on stream %d", f.StreamID)
			}
			return
		}
	}
}

// h2Probe is a parser that claims every HTTP/2 connection and reports what
// its matcher was handed.
type h2Probe struct{ saw chan []byte }

func (p h2Probe) MatchType(_ context.Context, buf []byte) bool {
	if util.IsHTTP2Preface(buf) {
		p.saw <- append([]byte(nil), buf...)
		return true
	}
	return false
}
func (h2Probe) RecordOutgoing(context.Context, *integrations.RecordSession) error { return nil }
func (h2Probe) MockOutgoing(context.Context, net.Conn, *models.ConditionalDstCfg, integrations.MockMemDb, models.OutgoingOptions) error {
	return nil
}

// replayH2Through hands an h2c connection whose client waits for the server's
// SETTINGS before its HEADERS to handleConnection in replay, and returns what
// the HTTP/2 matcher was handed.
func replayH2Through(t *testing.T, proven bool) []byte {
	t.Helper()
	if proven {
		t.Setenv(provenswitch.Env, "1")
	} else {
		t.Setenv(provenswitch.Env, "")
	}
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_TEST
	p := New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, "127.0.0.1:50051")}, cfg)
	probe := h2Probe{saw: make(chan []byte, 1)}
	p.Integrations = map[integrations.IntegrationType]integrations.Integrations{integrations.HTTP2: probe}
	p.integrationsPriority = []ParserPriority{{Priority: 1, ParserType: integrations.HTTP2}}
	if err := p.Mock(context.Background(), models.OutgoingOptions{DisableMysqlAutoDetect: true, Mocking: true}); err != nil {
		t.Fatal(err)
	}
	app, src := acceptedConn(t)
	go func() { _ = p.handleConnection(context.Background(), src) }()
	cl := newH2Client(t, app)
	cl.preface()
	if proven {
		if s, ok := cl.read().(*http2.SettingsFrame); !ok || s.IsAck() {
			t.Fatal("keploy did not answer the client preface with SETTINGS")
		}
		cl.request("application/json")
		if err := cl.fr.WriteSettingsAck(); err != nil {
			t.Fatal(err)
		}
	}
	return awaitEvent(t, probe.saw, "the HTTP/2 matcher being asked")
}

// Under the proven switch an h2c replay is decided on its HEADERS, however
// long its client takes to send them; without it, as before, the matcher is
// handed the preface alone once 150 ms passed in silence (and an h2c client
// that waits for the server's preface is then taken for gRPC).
func TestAnH2cReplayIsDecidedOnItsHeadersUnderTheSwitch(t *testing.T) {
	if ct := contentType(replayH2Through(t, true)); ct != "application/json" {
		t.Fatalf("under the switch the HTTP/2 matcher read content-type %q, want application/json", ct)
	}
	if buf := replayH2Through(t, false); util.HasHTTP2HeadersFrame(buf) {
		t.Fatal("without the switch the matcher was handed HEADERS its client never sent")
	}
}

// A connection whose client never sends its HEADERS waits for them until the
// connection's context ends, and no longer.
func TestTheReadUntilHeadersEndsWithItsContext(t *testing.T) {
	app, proxySide := acceptedConn(t)
	cl := newH2Client(t, app)
	cl.preface()
	initial := serverSide(t, proxySide)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan h2Read, 1)
	go func() {
		buf, conn, err := readHTTP2UntilHeaders(ctx, proxySide, initial)
		done <- h2Read{buf, conn, err}
	}()
	cl.read() // keploy's SETTINGS: keploy is now reading
	cancel()
	if got := awaitEvent(t, done, "the read until HEADERS returning"); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("the read ended with %v, want the context's end", got.err)
	}
}

// deadlineConn records the read deadlines set on the server end of a
// connection, and tells when a Read begins.
type deadlineConn struct {
	net.Conn
	mu      sync.Mutex
	set     []time.Time
	reading chan struct{}
}

func newDeadlineConn(c net.Conn) *deadlineConn {
	return &deadlineConn{Conn: c, reading: make(chan struct{}, 64)}
}

func (d *deadlineConn) SetReadDeadline(t time.Time) error {
	d.mu.Lock()
	d.set = append(d.set, t)
	d.mu.Unlock()
	return d.Conn.SetReadDeadline(t)
}

func (d *deadlineConn) Read(p []byte) (int, error) {
	select {
	case d.reading <- struct{}{}:
	default:
	}
	return d.Conn.Read(p)
}

// budgets are the deadlines set that lie ahead: a clock on the read. A
// deadline already past (cutting a read short when its context ends) or
// cleared (zero) is none.
func (d *deadlineConn) budgets() []time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []time.Time
	for _, t := range d.set {
		if !t.IsZero() && t.After(time.Unix(1, 0)) {
			out = append(out, t)
		}
	}
	return out
}

// No clock decides while keploy waits for a client's HEADERS: keploy sets no
// read deadline on the connection, and the client sends its HEADERS only
// when the test lets it, while keploy is blocked reading.
func TestNoClockDecidesTheReadUntilHeaders(t *testing.T) {
	app, raw := net.Pipe()
	t.Cleanup(func() { _ = app.Close(); _ = raw.Close() })
	cl := newH2Client(t, app)
	initial := prefaced(t, cl, raw)
	proxySide := newDeadlineConn(raw)
	done := make(chan h2Read, 1)
	go func() {
		buf, conn, err := readHTTP2UntilHeaders(context.Background(), proxySide, initial)
		done <- h2Read{buf, conn, err}
	}()
	cl.read() // keploy's SETTINGS
	awaitEvent(t, proxySide.reading, "keploy reading for the HEADERS")
	if b := proxySide.budgets(); len(b) != 0 {
		t.Fatalf("keploy put a clock on the read: deadlines %v", b)
	}
	select {
	case <-done:
		t.Fatal("the connection was decided before its HEADERS")
	default:
	}
	cl.request("application/json")
	got := awaitEvent(t, done, "the read until HEADERS returning")
	if got.err != nil || contentType(got.buf) != "application/json" {
		t.Fatalf("read %q err %v", contentType(got.buf), got.err)
	}
	if b := proxySide.budgets(); len(b) != 0 {
		t.Fatalf("keploy put a clock on the read: deadlines %v", b)
	}
}

// A first read that holds only part of the client preface is read on to
// the whole preface, however late the rest comes, and with no clock: what
// is decided on it never depends on how the preface was split.
func TestAPartialClientPrefaceIsReadToTheWholePreface(t *testing.T) {
	app, raw := net.Pipe()
	t.Cleanup(func() { _ = app.Close(); _ = raw.Close() })
	first := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 15)
		n, _ := io.ReadFull(raw, buf)
		first <- buf[:n]
	}()
	if _, err := io.WriteString(app, http2.ClientPreface[:15]); err != nil {
		t.Fatal(err)
	}
	initial := awaitEvent(t, first, "the first read of the preface")
	proxySide := newDeadlineConn(raw)
	done := make(chan []byte, 1)
	go func() {
		buf, err := readHTTP2Preface(context.Background(), proxySide, initial)
		if err != nil {
			buf = nil
		}
		done <- buf
	}()
	awaitEvent(t, proxySide.reading, "keploy reading for the rest of the preface")
	if b := proxySide.budgets(); len(b) != 0 {
		t.Fatalf("keploy put a clock on the read: deadlines %v", b)
	}
	if _, err := io.WriteString(app, http2.ClientPreface[15:]); err != nil {
		t.Fatal(err)
	}
	if got := awaitEvent(t, done, "the read of the preface returning"); !util.IsHTTP2Preface(got) {
		t.Fatalf("read %q, want the whole client preface", got)
	}
	// Bytes that leave the preface end the read at once.
	if buf, err := readHTTP2Preface(context.Background(), proxySide, []byte("POST")); err != nil || string(buf) != "POST" {
		t.Fatalf("an HTTP/1 request was read on: %q %v", buf, err)
	}
}

// When the last read before the hand-off ends inside the header of the
// client's acknowledgement of keploy's SETTINGS, that acknowledgement is
// still the one taken out: the parser gets the client's next one.
func TestAnAckSplitAcrossTheHandOffIsStillTakenOut(t *testing.T) {
	app, proxySide := acceptedConn(t)
	cl := newH2Client(t, app)
	cl.preface()
	initial := serverSide(t, proxySide)
	done := make(chan h2Read, 1)
	go func() {
		buf, conn, err := readHTTP2UntilHeaders(context.Background(), proxySide, initial)
		done <- h2Read{buf, conn, err}
	}()
	cl.read() // keploy's SETTINGS
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	_ = enc.WriteField(hpack.HeaderField{Name: ":method", Value: "GET"})
	hdrs := []byte{0, 0, byte(hb.Len()), h2FrameHeaders, 0x5, 0, 0, 0, 1}
	ack := []byte{0, 0, 0, h2FrameSettings, h2FlagAck, 0, 0, 0, 0}
	// HEADERS and the first 4 bytes of the ACK in one write.
	if _, err := app.Write(append(append(hdrs, hb.Bytes()...), ack[:4]...)); err != nil {
		t.Fatal(err)
	}
	got := awaitEvent(t, done, "the read until HEADERS returning")
	if got.err != nil {
		t.Fatal(got.err)
	}
	if frames := h2Frames(got.buf); len(frames) != 2 || frames[1] != "HEADERS" {
		t.Fatalf("the parser reads frames %v first", frames)
	}
	// The rest of keploy's ACK, then the client's ACK of the replay
	// server's SETTINGS.
	if _, err := app.Write(append(ack[4:], ack...)); err != nil {
		t.Fatal(err)
	}
	read := make(chan []byte, 1)
	go func() {
		rest := make([]byte, len(ack))
		n, _ := io.ReadFull(got.conn, rest)
		read <- rest[:n]
	}()
	if rest := awaitEvent(t, read, "the parser reading on"); !bytes.Equal(rest, ack) {
		t.Fatalf("the parser reads %x after the HEADERS, want one ACK", rest)
	}
}

// oneConn is a listener that hands out one connection, then blocks.
type oneConn struct {
	c    chan net.Conn
	done chan struct{}
}

func (l *oneConn) Accept() (net.Conn, error) {
	select {
	case c := <-l.c:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *oneConn) Close() error   { return nil }
func (l *oneConn) Addr() net.Addr { return &net.TCPAddr{} }

// throughKeploy accepts one client connection on a listener, reads its
// preface, answers it as keploy does on replay (readHTTP2UntilHeaders), and
// returns the connection a replay server reads.
func throughKeploy(ln net.Listener) (net.Conn, error) {
	c, err := ln.Accept()
	if err != nil {
		return nil, err
	}
	initial := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(c, initial); err != nil {
		return nil, err
	}
	buf, conn, err := readHTTP2UntilHeaders(context.Background(), c, initial)
	if err != nil {
		return nil, err
	}
	return &util.Conn{Conn: conn, Reader: util.NewPrefixReader(buf, conn), Logger: zap.NewNop()}, nil
}

// A real grpc-go client, through keploy's answer to its preface, to a
// grpc-go server, the gRPC replay server's: every call is answered.
func TestARealGRPCClientIsServedThroughKeploysPreface(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	served := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		served <- c
	}()
	cc, err := grpc.NewClient("passthrough:///deps.example", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp4", ln.Addr().String())
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	hc := healthpb.NewHealthClient(cc)
	answered := make(chan error, 1)
	go func() {
		_, err := hc.Check(context.Background(), &healthpb.HealthCheckRequest{})
		answered <- err
	}()
	c := awaitEvent(t, served, "the client connecting")
	initial := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(c, initial); err != nil {
		t.Fatal(err)
	}
	buf, conn, err := readHTTP2UntilHeaders(context.Background(), c, initial)
	if err != nil {
		t.Fatal(err)
	}
	if ct := contentType(buf); ct != "application/grpc" {
		t.Fatalf("the matchers read content-type %q", ct)
	}
	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, health.NewServer())
	l := &oneConn{c: make(chan net.Conn, 1), done: make(chan struct{})}
	l.c <- &util.Conn{Conn: conn, Reader: util.NewPrefixReader(buf, conn), Logger: zap.NewNop()}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { close(l.done); srv.Stop() })
	if err := awaitEvent(t, answered, "the first call being answered"); err != nil {
		t.Fatalf("the first call: %v", err)
	}
	for i := 0; i < 20; i++ {
		if _, err := hc.Check(context.Background(), &healthpb.HealthCheckRequest{}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

// A real Go h2c client, through keploy's answer to its preface, to Go's
// HTTP/2 server, the h2c replay server's: every request is answered.
func TestARealH2cClientIsServedThroughKeploysPreface(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := throughKeploy(ln)
		if err != nil {
			return
		}
		defer c.Close()
		(&http2.Server{}).ServeConn(c, &http2.ServeConnOpts{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		})})
	}()
	tr := &http2.Transport{AllowHTTP: true, DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp4", ln.Addr().String())
	}}
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr}
	for i := 0; i < 20; i++ {
		resp, err := client.Get("http://deps.example/v1/items")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusTeapot {
			t.Fatalf("request %d: %d", i, resp.StatusCode)
		}
	}
}
