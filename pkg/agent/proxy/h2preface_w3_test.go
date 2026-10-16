package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// h2Client is the client end of an HTTP/2 connection, written frame by frame.
type h2Client struct {
	t  *testing.T
	c  net.Conn
	fr *http2.Framer
}

func newH2Client(t *testing.T, c net.Conn) *h2Client {
	t.Helper()
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
	p.nsswitchData = []byte("preseeded-by-test")
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
